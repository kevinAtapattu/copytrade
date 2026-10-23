package score

import (
	"math"
	"testing"
	"time"

	"copytrade/internal/curve"
	"copytrade/internal/model"
)

var now = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

func closed(openedDaysAgo float64, holdS float64, costSol, proceedsSol float64) model.SimPosition {
	o := now.Add(-time.Duration(openedDaysAgo * float64(24*time.Hour)))
	c := o.Add(time.Duration(holdS) * time.Second)
	return model.SimPosition{ID: o.String(), OpenedAt: o, ClosedAt: &c, Status: model.PosClosed, CostSol: costSol,
		ProceedsSol: proceedsSol, SolUSD: 100}
}

func run(ps []model.SimPosition) model.Score {
	var outs []Outcome
	for _, p := range ps {
		outs = append(outs, Value(p, nil, curve.Fees{}, now))
	}
	return Compute("W", 2, 14, now, ps, outs, Params{SizeUSD: 25, BankrollUSD: 500})
}

func TestMetrics(t *testing.T) {
	ps := []model.SimPosition{
		closed(10, 60, 0.25, 0.30), // +5
		closed(9, 120, 0.25, 0.20), // -5
		closed(8, 180, 0.25, 0.35), // +10
		closed(7, 240, 0.25, 0.25), // 0
	}
	s := run(ps)
	if s.Positions != 4 || s.ClosedCount != 4 {
		t.Fatalf("counts %+v", s)
	}
	if math.Abs(s.PnlUSD-10) > 1e-9 || math.Abs(s.BestTradeUSD-10) > 1e-9 || math.Abs(s.PnlExBestUSD-0) > 1e-9 {
		t.Fatalf("pnl %+v", s)
	}
	if s.WinRate != 0.5 {
		t.Fatalf("win rate %v", s.WinRate)
	}
	if s.AvgHoldS != 150 {
		t.Fatalf("avg hold %v", s.AvgHoldS)
	}
	if !s.LuckyTrade {
		t.Fatal("pnl ex best is 0, so the total depends on one trade")
	}
	if math.Abs(s.ActiveDays-10) > 1e-9 || math.Abs(s.TradesPerDay-4.0/14) > 1e-9 {
		t.Fatalf("activity %+v", s)
	}
	// equity 500 -> 505 -> 500: dd = 5/505
	if math.Abs(s.MaxDrawdown-5.0/505) > 1e-9 {
		t.Fatalf("dd %v", s.MaxDrawdown)
	}
}

func TestLuckyTradeDetected(t *testing.T) {
	ps := []model.SimPosition{closed(5, 60, 0.25, 2.5), closed(4, 60, 0.25, 0.2), closed(3, 60, 0.25, 0.2), closed(2, 60, 0.25, 0.2)}
	s := run(ps)
	if s.PnlUSD <= 0 || s.PnlExBestUSD >= 0 || !s.LuckyTrade {
		t.Fatalf("expected lucky: %+v", s)
	}
	if s.Score >= 0 {
		t.Fatalf("score should use ex-best pnl and be negative: %v", s.Score)
	}
}

func TestMaxDrawdown(t *testing.T) {
	outs := []Outcome{{PnlUSD: 100, At: now}, {PnlUSD: -150, At: now.Add(time.Hour)}, {PnlUSD: 20, At: now.Add(2 * time.Hour)}}
	// 500 -> 600 -> 450: dd = 150/600
	if dd := MaxDrawdown(outs, 500); math.Abs(dd-0.25) > 1e-12 {
		t.Fatalf("dd %v", dd)
	}
	// order is by time, not slice order
	outs[0].At, outs[1].At = outs[1].At, outs[0].At
	if dd := MaxDrawdown(outs, 500); math.Abs(dd-150.0/500) > 1e-12 {
		t.Fatalf("dd reordered %v", dd)
	}
}

func TestOpenPositionMarkedToCurve(t *testing.T) {
	p := model.SimPosition{OpenedAt: now.Add(-time.Hour), Status: model.PosOpen, CostSol: 0.2, TokensHeld: 1e6, SolUSD: 100}
	tok := &model.Token{VSol: 30, VTokens: 1.073e9}
	o := Value(p, tok, curve.Fees{PumpPct: 1.25}, now)
	want := (curve.State{VSol: 30, VTokens: 1.073e9}.Price()*1e6*0.9875 - 0.2) * 100
	if o.Closed || math.Abs(o.PnlUSD-want) > 0.01 {
		t.Fatalf("mark %v want ~%v", o.PnlUSD, want)
	}
	if nilTok := Value(p, nil, curve.Fees{}, now); math.Abs(nilTok.PnlUSD+20) > 1e-9 {
		t.Fatalf("unknown token should mark to zero: %v", nilTok.PnlUSD)
	}
}

func TestFewTradesScoreZero(t *testing.T) {
	if s := run([]model.SimPosition{closed(1, 1, 0.25, 1)}); s.Score != 0 {
		t.Fatalf("score with 1 trade = %v", s.Score)
	}
}
