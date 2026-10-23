package exec

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/price"
	"copytrade/internal/sim"
	"copytrade/internal/store"
)

type Risk struct {
	SizeUSD          float64
	MaxOpenPositions int
	StopLossPct      float64
	DailyLossUSD     float64
}

type Executor struct {
	Trader Trader
	Store  store.Store
	Bus    bus.Bus
	Price  price.Source
	Risk   Risk
	// Sync runs orders inline (replay/tests). Otherwise each (wallet, mint) gets its
	// own ordered worker so a slow live tx never blocks the stream.
	Sync bool

	mu        sync.Mutex
	curves    map[string]curve.State
	migrated  map[string]bool
	live      map[string]bool
	positions map[string]*model.LivePosition // by wallet|mint
	inflight  map[string]bool                // buys reserved but not finished
	workers   map[string]chan func()
	killed    bool
	stopping  map[string]bool
	dayStart  time.Time
	dayPnl    float64
}

func New(t Trader, st store.Store, b bus.Bus, p price.Source, r Risk) *Executor {
	return &Executor{Trader: t, Store: st, Bus: b, Price: p, Risk: r,
		curves: map[string]curve.State{}, migrated: map[string]bool{}, live: map[string]bool{},
		positions: map[string]*model.LivePosition{}, inflight: map[string]bool{}, workers: map[string]chan func(){},
		stopping: map[string]bool{}}
}

func pkey(wallet, mint string) string { return wallet + "|" + mint }

// Load restores open positions and today's realized PnL for this mode.
func (e *Executor) Load(ctx context.Context, now time.Time) error {
	ps, err := e.Store.ListLivePositions(ctx, store.LiveFilter{Mode: e.Trader.Mode(), Since: now.AddDate(0, 0, -30)})
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollDay(now)
	for i := range ps {
		p := ps[i]
		if p.Status == model.PosOpen {
			e.positions[pkey(p.Wallet, p.Mint)] = &p
		} else if p.ClosedAt != nil && !p.ClosedAt.Before(e.dayStart) {
			e.dayPnl += (p.ProceedsSol - p.CostSol) * p.SolUSD
		}
	}
	return e.refreshLocked(ctx)
}

// Refresh reloads live wallets and the kill switch.
func (e *Executor) Refresh(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refreshLocked(ctx)
}

func (e *Executor) refreshLocked(ctx context.Context) error {
	ws, err := e.Store.ListWallets(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, w := range ws {
		if w.Status == model.StatusLive {
			live[w.Address] = true
		}
	}
	e.live = live
	v, _, err := e.Store.GetSetting(ctx, store.SettingKillSwitch)
	e.killed = v == "on"
	return err
}

func (e *Executor) rollDay(now time.Time) {
	d := now.UTC().Truncate(24 * time.Hour)
	if !d.Equal(e.dayStart) {
		e.dayStart, e.dayPnl = d, 0
	}
}

// OpenCount returns open positions.
func (e *Executor) OpenCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.positions)
}

// HandleTrade processes one trade from the stream.
func (e *Executor) HandleTrade(ctx context.Context, t model.Trade) {
	e.mu.Lock()
	e.rollDay(t.ReceivedAt)
	if t.Pool != "" && t.Pool != "pump" {
		e.migrated[t.Mint] = true
	} else if t.VSol > 0 && t.VTokens > 0 {
		e.curves[t.Mint] = curve.State{VSol: t.VSol, VTokens: t.VTokens}
	}
	var jobs []func()

	// Stop-loss on every price update for mints we hold.
	if st, ok := e.curves[t.Mint]; ok && e.Risk.StopLossPct > 0 {
		for k, p := range e.positions {
			if p.Mint != t.Mint || e.stopping[k] || p.TokensHeld <= 0 {
				continue
			}
			value := st.Price() * p.TokensHeld
			basis := p.CostSol - p.ProceedsSol
			if basis > 0 && value <= basis*(1-e.Risk.StopLossPct/100) {
				e.stopping[k] = true
				pos := p
				jobs = append(jobs, func() { e.sell(ctx, pos, 100, "stop_loss", t) })
			}
		}
	}

	if t.IsTarget && e.live[t.Trader] {
		k := pkey(t.Trader, t.Mint)
		if t.Side == model.SideBuy {
			if reason := e.buyBlockLocked(k, t); reason == "" {
				e.inflight[k] = true
				jobs = append(jobs, func() { e.buy(ctx, t) })
			} else if reason != "skip" {
				jobs = append(jobs, func() { e.reject(ctx, t, reason) })
			}
		} else if p, ok := e.positions[k]; ok && !e.stopping[k] {
			pct := sim.SellFraction(t) * 100
			pos := p
			jobs = append(jobs, func() { e.sell(ctx, pos, pct, "copy", t) })
		}
	}
	e.mu.Unlock()

	for _, j := range jobs {
		e.run(pkey(t.Trader, t.Mint), j)
	}
}

// buyBlockLocked returns "" if a buy may go ahead, "skip" to ignore silently, or a rejection reason.
func (e *Executor) buyBlockLocked(k string, t model.Trade) string {
	switch {
	case e.positions[k] != nil || e.inflight[k]:
		return "skip" // fixed size: one position per wallet+token
	case e.killed:
		return "kill switch on"
	case e.migrated[t.Mint]:
		return "not on a SOL bonding curve (migrated or non-SOL quote)"
	case e.Risk.MaxOpenPositions > 0 && len(e.positions)+len(e.inflight) >= e.Risk.MaxOpenPositions:
		return fmt.Sprintf("max open positions (%d)", e.Risk.MaxOpenPositions)
	case e.Risk.DailyLossUSD > 0 && e.dayPnl <= -e.Risk.DailyLossUSD:
		return fmt.Sprintf("daily loss limit hit (%.2f USD)", e.dayPnl)
	}
	if _, ok := e.curves[t.Mint]; !ok {
		return "no curve state"
	}
	return ""
}

func (e *Executor) run(key string, job func()) {
	if e.Sync {
		job()
		return
	}
	e.mu.Lock()
	ch, ok := e.workers[key]
	if !ok {
		ch = make(chan func(), 16)
		e.workers[key] = ch
		go e.worker(key, ch)
	}
	e.mu.Unlock()
	ch <- job
}

// worker runs jobs for one wallet+token in order. Workers live for the process;
// there is one per traded wallet+token, which stays small.
func (e *Executor) worker(_ string, ch chan func()) {
	for j := range ch {
		j()
	}
}

func (e *Executor) buy(ctx context.Context, t model.Trade) {
	k := pkey(t.Trader, t.Mint)
	defer func() {
		e.mu.Lock()
		delete(e.inflight, k)
		e.mu.Unlock()
	}()
	solUSD := e.Price.SOLUSD()
	e.mu.Lock()
	st := e.curves[t.Mint]
	e.mu.Unlock()
	sent := time.Now()
	if e.Sync {
		sent = t.ReceivedAt
	}
	res, err := e.Trader.Execute(ctx, Order{Mint: t.Mint, Side: model.SideBuy, SolIn: e.Risk.SizeUSD / solUSD, State: st, At: t.ReceivedAt})
	f := model.LiveFill{ID: sim.ID(e.Trader.Mode(), t.Signature, "buy"), Mode: e.Trader.Mode(), TargetSig: t.Signature,
		Wallet: t.Trader, Mint: t.Mint, Side: model.SideBuy, Reason: "copy", TargetAt: t.ReceivedAt, SentAt: sent,
		Signature: res.Signature, Attempts: res.Attempts, SolUSD: solUSD}
	if err != nil || res.Tokens <= 0 {
		if err == nil {
			err = fmt.Errorf("no tokens received")
		}
		f.Status, f.Error = "failed", err.Error()
		e.record(ctx, f, nil)
		return
	}
	at := res.FilledAt
	f.Status, f.FilledAt, f.SolAmount, f.TokenAmount, f.FeesSol = "filled", &at, res.SolAmount, res.Tokens, res.FeesSol
	f.FillPrice = res.SolAmount / res.Tokens
	p := &model.LivePosition{ID: sim.ID("lpos", e.Trader.Mode(), t.Signature), Mode: e.Trader.Mode(), Wallet: t.Trader,
		Mint: t.Mint, OpenedAt: at, Status: model.PosOpen, CostSol: res.SolAmount, TokensHeld: res.Tokens,
		EntryPrice: f.FillPrice, SolUSD: solUSD}
	f.PositionID = p.ID
	e.mu.Lock()
	e.positions[k] = p
	e.mu.Unlock()
	e.record(ctx, f, p)
}

func (e *Executor) sell(ctx context.Context, p *model.LivePosition, pct float64, reason string, t model.Trade) {
	k := pkey(p.Wallet, p.Mint)
	e.mu.Lock()
	st := e.curves[p.Mint]
	tokens := p.TokensHeld * pct / 100
	if pct >= 99.9 {
		tokens = p.TokensHeld
	}
	e.mu.Unlock()
	if tokens <= 0 {
		return
	}
	sent := time.Now()
	if e.Sync {
		sent = t.ReceivedAt
	}
	res, err := e.Trader.Execute(ctx, Order{Mint: p.Mint, Side: model.SideSell, Tokens: tokens, SellPct: pct, State: st, At: t.ReceivedAt})
	f := model.LiveFill{ID: sim.ID(e.Trader.Mode(), t.Signature, p.ID, reason), PositionID: p.ID, Mode: e.Trader.Mode(),
		TargetSig: t.Signature, Wallet: p.Wallet, Mint: p.Mint, Side: model.SideSell, Reason: reason, TargetAt: t.ReceivedAt,
		SentAt: sent, Signature: res.Signature, Attempts: res.Attempts, SolUSD: p.SolUSD}
	e.mu.Lock()
	defer func() {
		delete(e.stopping, k)
		e.mu.Unlock()
	}()
	if err != nil {
		f.Status, f.Error = "failed", err.Error()
		e.recordLocked(ctx, f, nil)
		return
	}
	at := res.FilledAt
	f.Status, f.FilledAt, f.SolAmount, f.TokenAmount, f.FeesSol = "filled", &at, res.SolAmount, res.Tokens, res.FeesSol
	if res.Tokens > 0 {
		f.FillPrice = res.SolAmount / res.Tokens
	}
	p.ProceedsSol += res.SolAmount
	p.TokensHeld -= res.Tokens
	if p.TokensHeld < 1e-6 || pct >= 99.9 {
		p.TokensHeld = 0
		p.Status = model.PosClosed
		p.ClosedAt = &at
		delete(e.positions, k)
		e.rollDay(at)
		e.dayPnl += (p.ProceedsSol - p.CostSol) * p.SolUSD
	}
	snap := *p
	e.recordLocked(ctx, f, &snap)
}

func (e *Executor) reject(ctx context.Context, t model.Trade, reason string) {
	f := model.LiveFill{ID: sim.ID(e.Trader.Mode(), t.Signature, "buy"), Mode: e.Trader.Mode(), TargetSig: t.Signature,
		Wallet: t.Trader, Mint: t.Mint, Side: model.SideBuy, Reason: "copy", TargetAt: t.ReceivedAt, SentAt: t.ReceivedAt,
		Status: "rejected", Error: reason}
	e.record(ctx, f, nil)
}

func (e *Executor) record(ctx context.Context, f model.LiveFill, p *model.LivePosition) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var snap *model.LivePosition
	if p != nil {
		s := *p
		snap = &s
	}
	e.recordLocked(ctx, f, snap)
}

// recordLocked persists and announces a fill. Caller holds e.mu (store calls are quick).
func (e *Executor) recordLocked(ctx context.Context, f model.LiveFill, p *model.LivePosition) {
	if err := e.Store.InsertLiveFill(ctx, f); err != nil {
		slog.Warn("insert live fill", "err", err)
	}
	if p != nil {
		if err := e.Store.UpsertLivePosition(ctx, *p); err != nil {
			slog.Warn("upsert live position", "err", err)
		}
	}
	kind := "live_fill"
	msg := fmt.Sprintf("[%s] %s %s %s: %s", f.Mode, f.Side, short(f.Mint), f.Status, fmtFill(f))
	if f.Status == "failed" {
		kind = "error"
	}
	if f.Status == "rejected" {
		kind = "info"
	}
	_ = bus.PublishJSON(ctx, e.Bus, bus.StreamEvents, model.Event{Kind: kind, At: f.SentAt, Service: "executor", Message: msg, Data: f})
}

func fmtFill(f model.LiveFill) string {
	if f.Status != "filled" {
		return f.Error
	}
	return fmt.Sprintf("%.4f SOL for %.0f tokens (%s, copying %s)", f.SolAmount, f.TokenAmount, f.Reason, short(f.Wallet))
}

func short(a string) string {
	if len(a) <= 10 {
		return a
	}
	return a[:4] + ".." + a[len(a)-4:]
}
