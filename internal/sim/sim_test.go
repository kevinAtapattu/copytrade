package sim

import (
	"context"
	"testing"
	"time"

	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/price"
	"copytrade/internal/store"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// stream builds trades on one curve, applying each to the reserves like pump.fun.
type stream struct {
	st  curve.State
	out []model.Trade
	n   int
}

func newStream() *stream { return &stream{st: curve.State{VSol: 32, VTokens: 1_005_937_500}} }

func (s *stream) trade(trader, side string, amt float64, at time.Duration, target bool, newBal float64) model.Trade {
	s.n++
	t := model.Trade{Signature: "sig" + string(rune('a'+s.n)), Mint: "MINT", Trader: trader, Side: side,
		Pool: "pump", ReceivedAt: t0.Add(at), IsTarget: target, NewTokenBalance: newBal}
	if side == model.SideBuy {
		out, ns, _ := s.st.BuyOut(amt)
		s.st, t.SolAmount, t.TokenAmount = ns, amt, out
	} else {
		sol, ns, _ := s.st.SellOut(amt)
		s.st, t.SolAmount, t.TokenAmount = ns, sol, amt
	}
	t.VSol, t.VTokens = s.st.VSol, s.st.VTokens
	s.out = append(s.out, t)
	return t
}

func newEngine() (*Engine, *store.Memory) {
	st := store.NewMemory()
	e := New(Params{SizeUSD: 25, Delays: []int{2, 10, 60}, Fees: curve.Fees{PumpPct: 1.25, NetworkSol: 0.0005}}, st, price.Fixed(150))
	return e, st
}

func TestFillUsesStateAtDelay(t *testing.T) {
	e, st := newEngine()
	ctx := context.Background()
	s := newStream()
	buy := s.trade("W", model.SideBuy, 1, 0, true, 30e6)
	afterBuy := s.st
	s.trade("X", model.SideBuy, 2, 1*time.Second, false, 0) // before +2s
	at1 := s.st
	s.trade("Y", model.SideBuy, 3, 5*time.Second, false, 0) // between +2s and +10s
	at5 := s.st
	s.trade("Z", model.SideBuy, 4, 30*time.Second, false, 0) // between +10s and +60s
	at30 := s.st
	for _, tr := range s.out {
		e.HandleTrade(ctx, tr)
	}
	e.Tick(ctx, t0.Add(2*time.Minute))
	_ = afterBuy

	sims, _ := st.SimTradesForTarget(ctx, buy.Signature)
	if len(sims) != 3 {
		t.Fatalf("want 3 sim fills, got %d", len(sims))
	}
	want := map[int]curve.State{2: at1, 10: at5, 60: at30}
	for _, x := range sims {
		if x.Skipped {
			t.Fatalf("delay %d skipped: %s", x.DelayS, x.SkipReason)
		}
		if got := want[x.DelayS].Price(); !near(x.SpotPrice, got) {
			t.Fatalf("delay %d spot %v want %v", x.DelayS, x.SpotPrice, got)
		}
		if !x.FillAt.Equal(t0.Add(time.Duration(x.DelayS) * time.Second)) {
			t.Fatalf("fill time %v", x.FillAt)
		}
		if !near(x.SolAmount, 25.0/150) {
			t.Fatalf("spent %v, want $25 = %v SOL", x.SolAmount, 25.0/150)
		}
	}
}

func TestTargetStateSeedsWhenNoLaterTrades(t *testing.T) {
	e, st := newEngine()
	ctx := context.Background()
	s := newStream()
	buy := s.trade("W", model.SideBuy, 1, 0, true, 30e6)
	e.HandleTrade(ctx, buy)
	if e.PendingCount() != 3 {
		t.Fatalf("pending %d", e.PendingCount())
	}
	e.Tick(ctx, t0.Add(5*time.Second)) // only +2s is due (grace 0)
	if e.PendingCount() != 2 {
		t.Fatalf("after tick pending %d, want 2", e.PendingCount())
	}
	e.Flush(ctx)
	sims, _ := st.SimTradesForTarget(ctx, buy.Signature)
	for _, x := range sims {
		if !near(x.SpotPrice, buy.Price()) {
			t.Fatalf("expected fill at target's post-trade price")
		}
	}
}

func TestMirrorSellPercentageAndClose(t *testing.T) {
	e, st := newEngine()
	ctx := context.Background()
	s := newStream()
	bal := 30e6
	s.trade("W", model.SideBuy, 1, 0, true, bal)
	s.trade("X", model.SideBuy, 3, 30*time.Second, false, 0)
	half := s.trade("W", model.SideSell, bal*0.5, 2*time.Minute, true, bal*0.5)
	s.trade("X", model.SideBuy, 0.1, 3*time.Minute, false, 0)
	all := s.trade("W", model.SideSell, bal*0.5, 4*time.Minute, true, 0)
	for _, tr := range s.out {
		e.HandleTrade(ctx, tr)
	}
	e.Flush(ctx)

	if SellFraction(half) != 0.5 || SellFraction(all) != 1 {
		t.Fatalf("sell fractions %v %v", SellFraction(half), SellFraction(all))
	}
	ps, _ := st.ListSimPositions(ctx, store.SimFilter{DelayS: 2})
	if len(ps) != 1 {
		t.Fatalf("positions %d", len(ps))
	}
	p := ps[0]
	if p.Status != model.PosClosed || p.TokensHeld != 0 || p.ClosedAt == nil {
		t.Fatalf("position not closed: %+v", p)
	}
	halfSims, _ := st.SimTradesForTarget(ctx, half.Signature)
	for _, x := range halfSims {
		if x.DelayS == 2 && !near(x.TokenAmount, p.TokensBought*0.5) {
			t.Fatalf("half sell sold %v of %v", x.TokenAmount, p.TokensBought)
		}
	}
	if p.ProceedsSol <= p.CostSol {
		t.Fatalf("price rose 3x+, position should profit: %+v", p)
	}
	if e.OpenCount() != 0 {
		t.Fatal("closed positions must leave memory")
	}
}

func TestSkips(t *testing.T) {
	e, st := newEngine()
	ctx := context.Background()
	s := newStream()
	b1 := s.trade("W", model.SideBuy, 1, 0, true, 30e6)
	b2 := s.trade("W", model.SideBuy, 1, 20*time.Second, true, 60e6)
	orphan := model.Trade{Signature: "orphan", Mint: "OTHER", Trader: "W", Side: model.SideSell, TokenAmount: 5,
		VSol: 40, VTokens: 8e8, Pool: "pump", ReceivedAt: t0.Add(30 * time.Second), IsTarget: true}
	for _, tr := range []model.Trade{b1, b2, orphan} {
		e.HandleTrade(ctx, tr)
	}
	e.Flush(ctx)
	for _, x := range must(st.SimTradesForTarget(ctx, b2.Signature)) {
		if !x.Skipped || x.SkipReason != "already in position" {
			t.Fatalf("second buy should be skipped: %+v", x)
		}
	}
	for _, x := range must(st.SimTradesForTarget(ctx, orphan.Signature)) {
		if !x.Skipped || x.SkipReason != "no open position" {
			t.Fatalf("orphan sell should be skipped: %+v", x)
		}
	}
	// migrated tokens are not bought
	mig := model.Trade{Signature: "mig", Mint: "AMM", Trader: "W", Side: model.SideBuy, Pool: "pump-amm",
		ReceivedAt: t0, IsTarget: true}
	e.HandleTrade(ctx, model.Trade{Signature: "pre", Mint: "AMM", Trader: "Q", Side: model.SideBuy, VSol: 40, VTokens: 8e8, Pool: "pump", ReceivedAt: t0.Add(-time.Second)})
	e.HandleTrade(ctx, mig)
	e.Flush(ctx)
	for _, x := range must(st.SimTradesForTarget(ctx, "mig")) {
		if !x.Skipped || x.SkipReason != "not on a SOL bonding curve (migrated or non-SOL quote)" {
			t.Fatalf("migrated buy should be skipped: %+v", x)
		}
	}
}

func TestIdempotentIDs(t *testing.T) {
	if ID("a", "b") == ID("ab") || ID("a", "b") != ID("a", "b") {
		t.Fatal("ID must separate parts and be stable")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func near(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	m := b
	if m < 0 {
		m = -m
	}
	return d <= 1e-9*max(1, m)
}

func TestTickWallUsesStreamTimeDuringBacklog(t *testing.T) {
	e, st := newEngine()
	ctx := context.Background()
	wall := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) // wall clock far ahead of the data
	e.Wall = func() time.Time { return wall }
	s := newStream()
	buy := s.trade("W", model.SideBuy, 1, 0, true, 30e6)
	s.trade("X", model.SideBuy, 2, 5*time.Second, false, 0)
	at5 := s.st
	s.trade("Y", model.SideBuy, 3, 20*time.Second, false, 0)

	e.HandleTrade(ctx, s.out[0])
	e.TickWall(ctx) // backlog: wall time has not moved since the last trade was read
	if e.PendingCount() != 3 {
		t.Fatalf("finalized early during backlog: pending %d", e.PendingCount())
	}
	e.HandleTrade(ctx, s.out[1])
	e.HandleTrade(ctx, s.out[2])
	wall = wall.Add(2 * time.Minute) // stream goes idle
	e.TickWall(ctx)
	if e.PendingCount() != 0 {
		t.Fatalf("idle stream should finalize: pending %d", e.PendingCount())
	}
	for _, x := range must(st.SimTradesForTarget(ctx, buy.Signature)) {
		if x.DelayS == 10 && !near(x.SpotPrice, at5.Price()) {
			t.Fatal("+10s fill should see the trade at +5s")
		}
	}
}

// After graduation the wallet trades on PumpSwap: we copy the buy at the pool
// price with the AMM fee, and their sell closes the copy (not "migrated").
func TestCopiesOnPumpSwap(t *testing.T) {
	st := store.NewMemory()
	e := New(Params{SizeUSD: 25, Delays: []int{2}, Fees: curve.Fees{PumpPct: 1.25, AMMPct: 1.0, NetworkSol: 0.0005}}, st, price.Fixed(150))
	ctx := context.Background()
	pool := curve.State{VSol: 80, VTokens: 300e6, AMM: true}
	tr := func(sig, side string, at time.Duration, target bool, bal float64) model.Trade {
		return model.Trade{Signature: sig, Mint: "M", Trader: "W", Side: side, Pool: model.PoolPumpSwap, IsTarget: target,
			VSol: pool.VSol, VTokens: pool.VTokens, ReceivedAt: t0.Add(at), NewTokenBalance: bal, TokenAmount: 1e6}
	}
	// A frozen "curve complete" read arrives late; it must not undo the pool price.
	e.HandleTrade(ctx, tr("b", model.SideBuy, 0, true, 1e6))
	e.HandleTrade(ctx, model.Trade{Signature: "late", Mint: "M", Side: "snapshot", Pool: "migrated", ReceivedAt: t0.Add(time.Second)})
	e.Tick(ctx, t0.Add(time.Minute))
	ps, _ := st.ListSimPositions(ctx, store.SimFilter{})
	if len(ps) != 1 || ps[0].Status != model.PosOpen {
		t.Fatalf("buy on PumpSwap not copied: %+v", ps)
	}
	want, _ := curve.Buy(pool, 25.0/150, curve.Fees{PumpPct: 1.25, AMMPct: 1.0, NetworkSol: 0.0005})
	if d := ps[0].TokensBought - want.Tokens; d > 1 || d < -1 {
		t.Fatalf("tokens %.0f, want %.0f at the pool price with the AMM fee", ps[0].TokensBought, want.Tokens)
	}
	e.HandleTrade(ctx, tr("s", model.SideSell, 2*time.Minute, true, 0))
	e.Tick(ctx, t0.Add(5*time.Minute))
	ps, _ = st.ListSimPositions(ctx, store.SimFilter{})
	if ps[0].Status != model.PosClosed || ps[0].ProceedsSol <= 0 {
		t.Fatalf("sell on PumpSwap should close the copy: %+v", ps[0])
	}
}
