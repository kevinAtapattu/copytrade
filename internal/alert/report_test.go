package alert

import (
	"context"
	"testing"
	"time"

	"copytrade/internal/model"
	"copytrade/internal/store"
)

// The day we go live, paper copies that closed earlier that day stay out of the
// live P&L and failure counts.
func TestReportKeepsModesApart(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	closed := now.Add(-10 * time.Minute)
	for _, m := range []string{"paper", "live"} {
		st.UpsertLivePosition(ctx, model.LivePosition{ID: m, Mode: m, Wallet: "W", Mint: "M" + m, OpenedAt: now.Add(-time.Hour),
			ClosedAt: &closed, Status: model.PosClosed, CostSol: 0.1, ProceedsSol: 0.2, SolUSD: 100})
		st.InsertLiveFill(ctx, model.LiveFill{ID: m, Mode: m, Side: model.SideSell, Status: "failed", SentAt: closed})
	}
	lines, err := ExecLines(ctx, st, "live", now.Add(-time.Hour), now)
	if err != nil || len(lines) != 1 || lines[0].Mint != short("Mlive") {
		t.Fatalf("live lines %+v %v", lines, err)
	}
	f, err := ExecFailures(ctx, st, "live", now.Add(-time.Hour), now)
	if err != nil || f != (Failures{Sells: 1}) {
		t.Fatalf("live failures %+v %v", f, err)
	}
}
