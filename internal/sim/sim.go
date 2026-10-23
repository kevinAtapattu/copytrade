// Package sim simulates a fixed-size copy of every target trade at several
// delays, using the token's own trade stream as the price source.
//
// Time is event time: each trade carries received_at. A fill due at T+d uses the
// curve state after the last trade at or before T+d. It is finalized when a
// later trade on that mint arrives, or when Tick(now) passes T+d+Grace.
package sim

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"copytrade/internal/config"
	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/price"
	"copytrade/internal/store"
)

// Dust: below this our simulated token balance counts as fully sold.
const Dust = 1e-6

type Params struct {
	SizeUSD float64
	Delays  []int
	Fees    curve.Fees
	Grace   time.Duration // wait this long past due for late trades before finalizing on Tick
}

// Sink is notified of every simulated fill (used to publish events).
type Sink func(s model.SimTrade, p model.SimPosition)

type Engine struct {
	P     Params
	Store store.Store
	Price price.Source
	Sink  Sink

	mu        sync.Mutex
	mints     map[string]*mintState
	positions map[posKey]*model.SimPosition
	pending   map[string][]pending // by mint, sorted by due
}

type mintState struct {
	st       curve.State
	at       time.Time
	migrated bool
}

type posKey struct {
	wallet, mint string
	delay        int
}

type pending struct {
	due    time.Time
	delay  int
	target model.Trade
}

func New(p Params, st store.Store, ps price.Source) *Engine {
	if len(p.Delays) == 0 {
		p.Delays = model.Delays
	}
	return &Engine{
		P: p, Store: st, Price: ps,
		mints: map[string]*mintState{}, positions: map[posKey]*model.SimPosition{}, pending: map[string][]pending{},
	}
}

// LoadOpen restores open positions after a restart.
func (e *Engine) LoadOpen(ctx context.Context) error {
	ps, err := e.Store.ListSimPositions(ctx, store.SimFilter{OpenOnly: true})
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range ps {
		p := ps[i]
		e.positions[posKey{p.Wallet, p.Mint, p.DelayS}] = &p
	}
	return nil
}

// HandleTrade processes one trade from the stream, in event-time order.
func (e *Engine) HandleTrade(ctx context.Context, t model.Trade) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 1. Fills due strictly before this trade see the state before it.
	e.finalizeMint(ctx, t.Mint, func(p pending) bool { return p.due.Before(t.ReceivedAt) })

	// 2. Advance curve state.
	ms := e.mints[t.Mint]
	if ms == nil {
		ms = &mintState{}
		e.mints[t.Mint] = ms
	}
	if t.Pool != "" && t.Pool != "pump" {
		ms.migrated = true
	} else if t.VSol > 0 && t.VTokens > 0 {
		ms.st = curve.State{VSol: t.VSol, VTokens: t.VTokens}
		ms.at = t.ReceivedAt
	}

	// 3. Schedule copies of target trades.
	if t.IsTarget {
		q := e.pending[t.Mint]
		for _, d := range e.P.Delays {
			q = append(q, pending{due: t.ReceivedAt.Add(time.Duration(d) * time.Second), delay: d, target: t})
		}
		sort.SliceStable(q, func(i, j int) bool { return q[i].due.Before(q[j].due) })
		e.pending[t.Mint] = q
	}
}

// Tick finalizes fills whose due time + grace has passed with no newer trade.
func (e *Engine) Tick(ctx context.Context, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cut := now.Add(-e.P.Grace)
	for mint := range e.pending {
		e.finalizeMint(ctx, mint, func(p pending) bool { return !p.due.After(cut) })
	}
}

// Flush finalizes everything pending (end of replay).
func (e *Engine) Flush(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for mint := range e.pending {
		e.finalizeMint(ctx, mint, func(pending) bool { return true })
	}
}

// PendingCount is the number of scheduled, unfinalized fills.
func (e *Engine) PendingCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, q := range e.pending {
		n += len(q)
	}
	return n
}

// OpenCount is the number of open simulated positions.
func (e *Engine) OpenCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.positions)
}

func (e *Engine) finalizeMint(ctx context.Context, mint string, ready func(pending) bool) {
	q := e.pending[mint]
	i := 0
	for i < len(q) && ready(q[i]) {
		e.fill(ctx, q[i])
		i++
	}
	if i == len(q) {
		delete(e.pending, mint)
	} else if i > 0 {
		e.pending[mint] = q[i:]
	}
}

func (e *Engine) fill(ctx context.Context, p pending) {
	t := p.target
	s := model.SimTrade{
		ID: ID(t.Signature, t.Trader, fmt.Sprint(p.delay)), TargetSig: t.Signature, Wallet: t.Trader, Mint: t.Mint,
		DelayS: p.delay, Side: t.Side, TargetAt: t.ReceivedAt, FillAt: p.due, TargetPrice: t.Price(),
	}
	ms := e.mints[t.Mint]
	key := posKey{t.Trader, t.Mint, p.delay}
	pos := e.positions[key]

	switch {
	case ms == nil || !ms.st.Valid():
		s.Skipped, s.SkipReason = true, "no curve state"
	case t.Side == model.SideBuy:
		e.fillBuy(&s, ms, pos, key)
	default:
		e.fillSell(&s, ms, pos, key, t)
	}

	if err := e.Store.InsertSimTrade(ctx, s); err != nil {
		slog.Warn("insert sim trade", "err", err)
	}
	var snap model.SimPosition
	if pp := e.positionFor(s, key); pp != nil {
		snap = *pp
		if err := e.Store.UpsertSimPosition(ctx, snap); err != nil {
			slog.Warn("upsert sim position", "err", err)
		}
		if snap.Status != model.PosOpen {
			delete(e.positions, key)
		}
	}
	if e.Sink != nil {
		e.Sink(s, snap)
	}
}

// positionFor returns the position a non-skipped fill touched.
func (e *Engine) positionFor(s model.SimTrade, key posKey) *model.SimPosition {
	if s.Skipped || s.PositionID == "" {
		return nil
	}
	return e.positions[key]
}

func (e *Engine) fillBuy(s *model.SimTrade, ms *mintState, pos *model.SimPosition, key posKey) {
	if ms.migrated {
		s.Skipped, s.SkipReason = true, "migrated off bonding curve"
		return
	}
	if pos != nil {
		// Fixed size per position: extra target buys while we hold are not copied.
		s.Skipped, s.SkipReason, s.PositionID = true, "already in position", pos.ID
		return
	}
	solUSD := e.Price.SOLUSD()
	s.SolUSD = solUSD
	f, err := curve.Buy(ms.st, e.P.SizeUSD/solUSD, e.P.Fees)
	if err != nil {
		s.Skipped, s.SkipReason = true, err.Error()
		return
	}
	applyFill(s, f)
	np := &model.SimPosition{
		ID:     ID("pos", key.wallet, key.mint, fmt.Sprint(key.delay), s.FillAt.Format(time.RFC3339Nano)),
		Wallet: key.wallet, Mint: key.mint, DelayS: key.delay, OpenedAt: s.FillAt, Status: model.PosOpen,
		CostSol: f.SolAmount, TokensHeld: f.Tokens, TokensBought: f.Tokens, SolUSD: solUSD,
	}
	s.PositionID = np.ID
	e.positions[key] = np
}

func (e *Engine) fillSell(s *model.SimTrade, ms *mintState, pos *model.SimPosition, key posKey, t model.Trade) {
	if pos == nil {
		s.Skipped, s.SkipReason = true, "no open position"
		return
	}
	s.SolUSD = pos.SolUSD
	frac := SellFraction(t)
	s.SellFraction = frac
	tokens := pos.TokensHeld * frac
	if frac >= 0.999 || pos.TokensHeld-tokens < Dust {
		tokens = pos.TokensHeld
	}
	f, err := curve.Sell(ms.st, tokens, e.P.Fees)
	if err != nil {
		s.Skipped, s.SkipReason, s.PositionID = true, err.Error(), pos.ID
		return
	}
	applyFill(s, f)
	if ms.migrated {
		s.SkipReason = "migrated: priced at last curve state"
	}
	s.PositionID = pos.ID
	pos.ProceedsSol += f.SolAmount
	pos.TokensHeld -= tokens
	if pos.TokensHeld < Dust {
		pos.TokensHeld = 0
		at := s.FillAt
		pos.ClosedAt = &at
		pos.Status = model.PosClosed
		if ms.migrated {
			pos.Status = model.PosMigrated
		}
	}
}

// SellFraction is the share of their position the target sold in this trade.
func SellFraction(t model.Trade) float64 {
	if t.NewTokenBalance <= 1 { // ingest.Dust: treat as full exit
		return 1
	}
	before := t.NewTokenBalance + t.TokenAmount
	if before <= 0 {
		return 1
	}
	return t.TokenAmount / before
}

func applyFill(s *model.SimTrade, f curve.Fill) {
	s.SolAmount, s.TokenAmount = f.SolAmount, f.Tokens
	s.SpotPrice, s.FillPrice, s.SlippagePct = f.SpotPrice, f.FillPrice, f.SlippagePct
	s.PumpFeeSol, s.BotFeeSol, s.NetworkFeeSol = f.PumpFeeSol, f.BotFeeSol, f.NetworkSol
}

// ID makes a stable short id from parts, so re-processing is idempotent.
func ID(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// FeesFrom builds the fee model from config.
func FeesFrom(c config.Config) curve.Fees {
	f := curve.Fees{PumpPct: c.PumpFeePct, NetworkSol: c.BaseFeeSol + c.PriorityFeeSol}
	if c.BotFeeEnabled {
		f.BotPct = c.BotFeePct
	}
	return f
}
