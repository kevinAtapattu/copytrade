package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"copytrade/internal/config"
	"copytrade/internal/model"
	"copytrade/internal/store"
)

// In live mode the overview carries the real-money numbers: only live positions,
// the copied wallets, and the trading wallet's balance.
func TestOverviewRealMoney(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Date(2026, 11, 18, 17, 0, 0, 0, time.UTC)
	st.AddWallet(ctx, model.Wallet{Address: "NAD", Status: model.StatusLive})
	st.AddWallet(ctx, model.Wallet{Address: "OTHER", Status: model.StatusBench})
	closed := now.Add(-time.Hour)
	st.UpsertLivePosition(ctx, model.LivePosition{ID: "l1", Mode: "live", Wallet: "NAD", Mint: "A", OpenedAt: now.Add(-2 * time.Hour),
		ClosedAt: &closed, Status: model.PosClosed, CostSol: 0.1, ProceedsSol: 0.15, SolUSD: 100})
	st.UpsertLivePosition(ctx, model.LivePosition{ID: "p1", Mode: "paper", Wallet: "NAD", Mint: "B", OpenedAt: now.Add(-2 * time.Hour),
		ClosedAt: &closed, Status: model.PosClosed, CostSol: 0.1, ProceedsSol: 0.5, SolUSD: 100})
	s := New(st, nil, nil, config.Config{LiveTrading: true, AlertTZ: "UTC", RotationDelayS: 2})
	s.Now = func() time.Time { return now }
	calls := 0
	s.Wallet = func(context.Context) (string, float64, error) { calls++; return "ME", 0.8, nil }
	s.SolUSD = func() float64 { return 72 }

	var got struct {
		ExecToday struct {
			ClosedUSD float64 `json:"closed_usd"`
			Closed    int     `json:"closed"`
			Won       int     `json:"won"`
		} `json:"exec_today"`
		LiveWallets []string `json:"live_wallets"`
		Wallet      struct {
			Address    string  `json:"address"`
			BalanceSol float64 `json:"balance_sol"`
			SolUSD     float64 `json:"sol_usd"`
		} `json:"wallet"`
	}
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		s.overview(w, httptest.NewRequest("GET", "/api/overview", nil))
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err, w.Body.String())
		}
	}
	if got.ExecToday.Closed != 1 || got.ExecToday.Won != 1 || got.ExecToday.ClosedUSD < 4.99 || got.ExecToday.ClosedUSD > 5.01 {
		t.Fatalf("today counts paper or is wrong: %+v", got.ExecToday)
	}
	if len(got.LiveWallets) != 1 || got.LiveWallets[0] != "NAD" {
		t.Fatalf("live wallets %v", got.LiveWallets)
	}
	if got.Wallet.Address != "ME" || got.Wallet.BalanceSol != 0.8 || got.Wallet.SolUSD != 72 || calls != 1 {
		t.Fatalf("wallet %+v, %d balance reads (want 1, cached)", got.Wallet, calls)
	}
}

// The real-money calendar counts only live copies, by the day they closed.
func TestCalendarRealMoney(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)
	closed := now.Add(-time.Hour)
	st.UpsertLivePosition(ctx, model.LivePosition{ID: "l1", Mode: "live", Wallet: "NAD", Mint: "A", OpenedAt: now.Add(-2 * time.Hour),
		ClosedAt: &closed, Status: model.PosClosed, CostSol: 0.1, ProceedsSol: 0.12, SolUSD: 100})
	st.UpsertLivePosition(ctx, model.LivePosition{ID: "p1", Mode: "paper", Wallet: "NAD", Mint: "B", OpenedAt: now.Add(-2 * time.Hour),
		ClosedAt: &closed, Status: model.PosClosed, CostSol: 0.1, ProceedsSol: 0.5, SolUSD: 100})
	st.UpsertSimPosition(ctx, model.SimPosition{ID: "s1", Wallet: "NAD", Mint: "C", DelayS: 2, OpenedAt: now.Add(-2 * time.Hour),
		ClosedAt: &closed, Status: model.PosClosed, CostSol: 0.25, ProceedsSol: 1, SolUSD: 100})
	s := New(st, nil, nil, config.Config{LiveTrading: true, AlertTZ: "UTC", RotationDelayS: 2})
	s.Now = func() time.Time { return now }
	w := httptest.NewRecorder()
	s.calendar(w, httptest.NewRequest("GET", "/api/calendar?mode=live", nil))
	var got struct {
		Real bool `json:"real"`
		Days []struct {
			Date   string  `json:"date"`
			UsUSD  float64 `json:"us_usd"`
			Closed int     `json:"closed"`
		} `json:"days"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if !got.Real || len(got.Days) != 1 || got.Days[0].Closed != 1 || got.Days[0].UsUSD != 2 {
		t.Fatalf("want one day with the live copy's +$2 only: %+v", got)
	}
}
