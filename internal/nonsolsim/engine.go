// Package nonsolsim paper-copies tracked wallets on pump.fun coins quoted in a
// token other than SOL (pool "nonsol"), which the live trader and the SOL sim
// skip. Prices come from Jupiter quotes taken when our copy would land: a SOL
// buy of a fixed dollar size 3s after their buy, and a sell of the same share of
// our tokens ~1.5s after each of their sells. Quotes include pool fees; we add
// the network + priority fee per swap. No stop loss in v1.
package nonsolsim

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"copytrade/internal/model"
	"copytrade/internal/sim"
)

// Store is what the engine needs (store.Postgres plus ListWallets).
type Store interface {
	SaveNonsolCopy(ctx context.Context, c model.NonsolCopy) error
	OpenNonsolCopies(ctx context.Context) ([]model.NonsolCopy, error)
	ListWallets(ctx context.Context) ([]model.Wallet, error)
}

const (
	PoolNonSOL = "nonsol"
	// closeShare: a sell that would leave less than this share of what we
	// bought sells everything instead.
	closeShare = 0.01
)

type Engine struct {
	Q        Quoter
	St       Store
	SolUSD   func() float64
	SizeUSD  float64       // dollars per copy
	FeeSol   float64       // network + priority fee per swap
	BuyDelay time.Duration // after their buy (our live buys land ~2.5-3s after)
	// SellDelay after their sell (our live sells land ~1.5s after).
	SellDelay time.Duration
	// MaxBuyAge: a buy we see later than this after it happened (stream lag,
	// restart) is not copied, its price would mean nothing.
	MaxBuyAge time.Duration
	Keep      map[string]bool // KEEP_WALLETS: copied whatever their status
	Now       func() time.Time
	Sleep     func(ctx context.Context, d time.Duration)

	mu      sync.Mutex
	live    map[string]bool
	open    map[key]*model.NonsolCopy
	skipped map[key]bool // a buy we couldn't copy; ignore more buys until they exit
	queues  map[key]chan job
	wg      sync.WaitGroup
}

type key struct{ wallet, mint string }

type job struct {
	t    model.Trade
	mark bool // re-quote what we hold (and retry an owed sell)
}

func New(q Quoter, st Store, solUSD func() float64) *Engine {
	return &Engine{Q: q, St: st, SolUSD: solUSD, SizeUSD: 10, FeeSol: 0.000505, BuyDelay: 3 * time.Second,
		SellDelay: 1500 * time.Millisecond, MaxBuyAge: 20 * time.Second, Keep: map[string]bool{}, Now: time.Now,
		Sleep: sleepCtx, live: map[string]bool{}, open: map[key]*model.NonsolCopy{}, skipped: map[key]bool{},
		queues: map[key]chan job{}}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// LoadOpen restores open copies after a restart.
func (e *Engine) LoadOpen(ctx context.Context) error {
	cs, err := e.St.OpenNonsolCopies(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range cs {
		c := cs[i]
		e.open[key{c.Wallet, c.Mint}] = &c
	}
	return nil
}

// RefreshWallets reloads which wallets are live.
func (e *Engine) RefreshWallets(ctx context.Context) error {
	ws, err := e.St.ListWallets(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, w := range ws {
		if w.Status == model.StatusLive {
			live[w.Address] = true
		}
	}
	e.mu.Lock()
	e.live = live
	e.mu.Unlock()
	return nil
}

func (e *Engine) OpenCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.open)
}

// Wait blocks until every queued job is done (tests).
func (e *Engine) Wait() { e.wg.Wait() }

// HandleTrade queues a trade for its wallet+coin and returns at once; each
// wallet+coin is worked in order on its own goroutine.
func (e *Engine) HandleTrade(ctx context.Context, t model.Trade) {
	k := key{t.Trader, t.Mint}
	e.mu.Lock()
	_, isOpen := e.open[k]
	tracked := e.live[t.Trader] || e.Keep[t.Trader]
	wanted := false
	switch {
	case isOpen && t.Side == model.SideSell:
		wanted = true // follow sells of a copy we hold, whatever the wallet's status now
	case !t.IsTarget || t.Pool != PoolNonSOL || !tracked:
	case t.Side == model.SideBuy || t.Side == model.SideSell:
		// A sell may follow a buy still waiting in the queue; the worker
		// ignores it if no copy opened.
		wanted = true
	}
	e.mu.Unlock()
	if wanted {
		e.enqueue(ctx, k, job{t: t})
	}
}

// MarkOpen re-quotes every open copy (and retries sells that failed).
func (e *Engine) MarkOpen(ctx context.Context) {
	e.mu.Lock()
	var ks []key
	for k := range e.open {
		ks = append(ks, k)
	}
	e.mu.Unlock()
	for _, k := range ks {
		e.enqueue(ctx, k, job{mark: true})
	}
}

func (e *Engine) enqueue(ctx context.Context, k key, j job) {
	e.wg.Add(1)
	e.mu.Lock()
	ch, ok := e.queues[k]
	if !ok {
		ch = make(chan job, 256)
		e.queues[k] = ch
		go e.worker(ctx, k, ch)
	}
	e.mu.Unlock()
	select {
	case ch <- j:
	default:
		e.wg.Done()
		slog.Warn("nonsol queue full, dropping", "wallet", k.wallet, "mint", k.mint)
	}
}

func (e *Engine) worker(ctx context.Context, k key, ch chan job) {
	idle := time.NewTimer(10 * time.Minute)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-ch:
			e.run(ctx, k, j)
			e.wg.Done()
			idle.Reset(10 * time.Minute)
		case <-idle.C:
			e.mu.Lock()
			if len(ch) == 0 {
				delete(e.queues, k)
				e.mu.Unlock()
				return
			}
			e.mu.Unlock()
			idle.Reset(10 * time.Minute)
		}
	}
}

func (e *Engine) run(ctx context.Context, k key, j job) {
	switch {
	case j.mark:
		e.mark(ctx, k)
	case j.t.Side == model.SideBuy:
		e.buy(ctx, k, j.t)
	case j.t.Side == model.SideSell:
		e.sell(ctx, k, j.t)
	}
}

func (e *Engine) get(k key) *model.NonsolCopy {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.open[k]
}

func (e *Engine) save(ctx context.Context, c model.NonsolCopy) {
	if err := e.St.SaveNonsolCopy(ctx, c); err != nil {
		slog.Error("save nonsol copy", "err", err, "id", c.ID)
	}
}

func (e *Engine) buy(ctx context.Context, k key, t model.Trade) {
	e.mu.Lock()
	_, isOpen := e.open[k]
	skip := e.skipped[k]
	e.mu.Unlock()
	if isOpen || skip {
		return // one copy per wallet+coin; their later buys are ignored
	}
	if age := e.Now().Sub(t.ReceivedAt); age > e.MaxBuyAge {
		slog.Info("nonsol buy too old, not copied", "wallet", k.wallet, "mint", k.mint, "age", age.Round(time.Second))
		return
	}
	e.Sleep(ctx, t.ReceivedAt.Add(e.BuyDelay).Sub(e.Now()))
	if ctx.Err() != nil {
		return
	}
	px := e.SolUSD()
	now := e.Now()
	c := model.NonsolCopy{ID: sim.ID("nonsol", t.Trader, t.Mint, t.Signature), Wallet: t.Trader, Mint: t.Mint,
		TheirSig: t.Signature, TheirAt: t.ReceivedAt, OpenedAt: now, SolUSD: px}
	if px <= 0 {
		c.Status, c.SkipReason = model.NonsolSkipped, "NO_SOL_PRICE"
	} else {
		lamports := uint64(math.Round(e.SizeUSD / px * 1e9))
		q, err := e.Q.Quote(ctx, WSOL, t.Mint, lamports)
		if err != nil {
			c.Status, c.SkipReason, c.LastError = model.NonsolSkipped, ReasonOf(err), trunc(err.Error())
		} else {
			c.Status, c.Route = model.NonsolOpen, q.Route
			c.SolIn, c.FeesSol = float64(lamports)/1e9, e.FeeSol
			c.TokensBought, c.TokensHeld = int64(q.OutAmount), int64(q.OutAmount)
		}
	}
	e.mu.Lock()
	if c.Status == model.NonsolOpen {
		e.open[k] = &c
	} else {
		e.skipped[k] = true
	}
	e.mu.Unlock()
	slog.Info("nonsol buy", "wallet", k.wallet, "mint", k.mint, "status", c.Status, "reason", c.SkipReason,
		"sol_in", c.SolIn, "tokens", c.TokensBought, "route", c.Route)
	e.save(ctx, c)
}

func (e *Engine) sell(ctx context.Context, k key, t model.Trade) {
	frac := sim.SellFraction(t)
	c := e.get(k)
	if c == nil {
		if frac >= 1 {
			e.mu.Lock()
			delete(e.skipped, k) // their round is over; the next buy is a new copy
			e.mu.Unlock()
		}
		return
	}
	e.Sleep(ctx, t.ReceivedAt.Add(e.SellDelay).Sub(e.Now()))
	if ctx.Err() != nil {
		return
	}
	amt := c.PendingSell + int64(math.Floor(float64(c.TokensHeld-c.PendingSell)*frac))
	e.sellAmount(ctx, k, c, amt)
}

// sellAmount quotes a sell of amt raw tokens (all of them when what would be
// left is dust) and records it. A failed quote keeps the tokens and owes the
// sell, retried on the next sell or mark.
func (e *Engine) sellAmount(ctx context.Context, k key, c *model.NonsolCopy, amt int64) {
	if amt > c.TokensHeld || float64(c.TokensHeld-amt) < closeShare*float64(c.TokensBought) {
		amt = c.TokensHeld
	}
	if amt <= 0 {
		return
	}
	q, err := e.Q.Quote(ctx, c.Mint, WSOL, uint64(amt))
	if err != nil {
		c.PendingSell, c.LastError = amt, trunc("sell: "+err.Error())
		slog.Warn("nonsol sell quote failed", "wallet", k.wallet, "mint", k.mint, "err", err)
		e.save(ctx, *c)
		return
	}
	c.SolOut += float64(q.OutAmount) / 1e9
	c.FeesSol += e.FeeSol
	c.TokensHeld -= amt
	c.PendingSell, c.LastError = 0, ""
	c.Sells++
	if c.TokensHeld <= 0 {
		now := e.Now()
		c.TokensHeld, c.MarkSol, c.ClosedAt, c.Status = 0, 0, &now, model.NonsolClosed
		e.mu.Lock()
		delete(e.open, k)
		e.mu.Unlock()
	}
	slog.Info("nonsol sell", "wallet", k.wallet, "mint", k.mint, "tokens", amt, "sol_out", float64(q.OutAmount)/1e9,
		"status", c.Status)
	e.save(ctx, *c)
}

func (e *Engine) mark(ctx context.Context, k key) {
	c := e.get(k)
	if c == nil {
		return
	}
	if c.PendingSell > 0 {
		e.sellAmount(ctx, k, c, c.PendingSell)
		return
	}
	q, err := e.Q.Quote(ctx, c.Mint, WSOL, uint64(c.TokensHeld))
	now := e.Now()
	if err != nil {
		c.LastError = trunc("mark: " + err.Error())
	} else {
		c.MarkSol, c.MarkedAt, c.LastError = float64(q.OutAmount)/1e9, &now, ""
	}
	e.save(ctx, *c)
}

func trunc(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
