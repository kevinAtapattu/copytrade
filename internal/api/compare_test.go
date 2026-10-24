package api

import (
	"math"
	"testing"
	"time"

	"copytrade/internal/curve"
	"copytrade/internal/model"
)

func TestBuildCompareTheirReturnVsOurs(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	closed := now.Add(-time.Minute)
	trades := []model.Trade{
		{Mint: "A", Side: model.SideBuy, SolAmount: 1, TokenAmount: 1000, Pool: "pump", ReceivedAt: now.Add(-10 * time.Minute)},
		{Mint: "A", Side: model.SideSell, SolAmount: 2, TokenAmount: 1000, Pool: "pump", ReceivedAt: now.Add(-2 * time.Minute)},
		{Mint: "B", Side: model.SideBuy, SolAmount: 1, TokenAmount: 1000, Pool: "pump", ReceivedAt: now.Add(-5 * time.Minute)},
		{Mint: "B", Side: model.SideSell, SolAmount: 0.5, TokenAmount: 1000, Pool: "pump", ReceivedAt: now.Add(-4 * time.Minute)},
		{Mint: "C", Side: model.SideBuy, SolAmount: 1, TokenAmount: 1000, Pool: "pump-amm", ReceivedAt: now}, // not copyable, ignored
		{Mint: "D", Side: model.SideSell, SolAmount: 1, TokenAmount: 1000, Pool: "pump", ReceivedAt: now},     // bought before tracking
	}
	ps := []model.SimPosition{
		{Mint: "A", DelayS: 2, Status: model.PosClosed, ClosedAt: &closed, CostSol: 0.2, ProceedsSol: 0.3, SolUSD: 125},
	}
	rows, sum := buildCompare(trades, ps, func(string) *model.Token { return nil }, curve.Fees{}, now)
	if len(rows) != 2 || rows[0].Mint != "B" || rows[1].Mint != "A" {
		t.Fatalf("want rows B, A (newest first), got %+v", rows)
	}
	a := rows[1]
	if math.Abs(a.TheirPct-1) > 1e-9 || a.Ours["2"] == nil || math.Abs(a.Ours["2"].PnlPct-0.5) > 1e-9 || a.Ours["10"] != nil {
		t.Fatalf("bad row A: their %.3f ours %+v", a.TheirPct, a.Ours)
	}
	if sum.Tokens != 2 || math.Abs(sum.TheirAvg-0.25) > 1e-9 || sum.Copied["2"] != 1 ||
		math.Abs(sum.OurAvg["2"]-0.5) > 1e-9 || math.Abs(sum.TheirSame["2"]-1) > 1e-9 {
		t.Fatalf("bad summary %+v", sum)
	}
	// A: bought -10m, sold -2m = 480s; B: -5m to -4m = 60s. Median of [60, 480] picks index 1.
	if sum.HoldMedianS != 480 || math.Abs(sum.OurPnlUSD["2"]-12.5) > 1e-9 {
		t.Fatalf("hold %.0f pnl %.2f", sum.HoldMedianS, sum.OurPnlUSD["2"])
	}
}
