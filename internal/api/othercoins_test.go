package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"copytrade/internal/config"
	"copytrade/internal/model"
	"copytrade/internal/store"
)

// Other coins: per-coin percent at the wallet's own prices, skipping coins whose
// trades carry no quote amount and rounds that started before tracking.
func TestOtherCoins(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	t0 := time.Date(2026, 11, 19, 16, 0, 0, 0, time.UTC)
	st.AddWallet(ctx, model.Wallet{Address: "NAD", Status: model.StatusLive})
	n := 0
	add := func(mint, side string, q, tok, bal, vsol float64, at time.Duration) {
		n++
		st.InsertTargetTrade(ctx, model.Trade{Signature: string(rune('a' + n)), Trader: "NAD", Mint: mint, Side: side,
			SolAmount: q, TokenAmount: tok, NewTokenBalance: bal, VSol: vsol, VTokens: 1e8, Pool: "nonsol", ReceivedAt: t0.Add(at)})
	}
	// A: the 4Wdn6G round, one buy one sell, -21.7%.
	add("A", "buy", 0.195139843, 2042169.698018, 2042169.698018, 16, 0)
	add("A", "sell", 0.152721391, 2042169.698018, 0, 14, 37*time.Second)
	// B: two buys, a partial sell, then the rest: paid 3, got 4.5 -> +50%. Open after.
	add("B", "buy", 1, 100, 100, 20, time.Minute)
	add("B", "buy", 2, 100, 200, 20, 2*time.Minute)
	add("B", "sell", 1.5, 100, 100, 20, 3*time.Minute)
	add("B", "sell", 3, 100, 0, 20, 4*time.Minute)
	add("B", "buy", 1, 50, 50, 20, 5*time.Minute)
	// C: held before tracking (balance above the buy), excluded. Then a sell-only coin D.
	add("C", "buy", 1, 100, 500, 20, time.Minute)
	add("C", "sell", 9, 500, 0, 20, 2*time.Minute)
	add("D", "sell", 1, 100, 0, 20, time.Minute)
	// E: curve trades with no quote amount; F: mixed.
	add("E", "buy", 0, 100, 100, 0, time.Minute)
	add("E", "sell", 0, 100, 0, 0, 2*time.Minute)
	add("F", "buy", 0, 100, 100, 0, time.Minute)
	add("F", "sell", 5, 100, 0, 20, 2*time.Minute)
	// G: curve buy, AMM sell after graduation at 4x the price: same units, counted (+300%).
	// H: AMM prices 1000x the curve's: a different scale, flagged mixed.
	add("G", "buy", 1, 100, 100, 0, time.Minute)
	add("G", "sell", 4, 100, 0, 20, 2*time.Minute)
	add("H", "buy", 1, 100, 100, 0, time.Minute)
	add("H", "sell", 1000, 100, 0, 20, 2*time.Minute)
	// A SOL coin never shows up here.
	st.InsertTargetTrade(ctx, model.Trade{Signature: "sol", Trader: "NAD", Mint: "S", Side: "buy", SolAmount: 1, TokenAmount: 1, Pool: "pump", ReceivedAt: t0})

	s := New(st, nil, nil, config.Config{AlertTZ: "UTC"})
	w := httptest.NewRecorder()
	s.otherCoins(w, httptest.NewRequest("GET", "/api/other-coins", nil))
	var got struct {
		Summary struct {
			Rounds     int     `json:"rounds"`
			Won        int     `json:"won"`
			Open       int     `json:"open"`
			Total      float64 `json:"total_per10"`
			WithoutBst float64 `json:"without_best_per10"`
		} `json:"summary"`
		Rounds  []otherRound `json:"rounds"`
		Skipped []otherSkip  `json:"skipped"`
		Days    []otherDay   `json:"days"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if got.Summary.Rounds != 3 || got.Summary.Won != 2 || got.Summary.Open != 1 {
		t.Fatalf("summary %+v", got.Summary)
	}
	pct := map[string]float64{}
	for _, r := range got.Rounds {
		pct[r.Mint] = r.Pct
	}
	if math.Abs(pct["A"]+21.74) > 0.01 || pct["B"] != 50 || pct["G"] != 300 {
		t.Fatalf("rounds %+v", got.Rounds)
	}
	if got.Summary.Total != 32.83 || got.Summary.WithoutBst != 2.83 {
		t.Fatalf("totals %+v", got.Summary)
	}
	reasons := map[string]string{}
	for _, x := range got.Skipped {
		reasons[x.Mint] = x.Reason
	}
	want := map[string]string{"C": "before_tracking", "D": "sell_only", "E": "no_price", "F": "mixed", "H": "mixed"}
	for m, r := range want {
		if reasons[m] != r {
			t.Fatalf("skipped %+v", got.Skipped)
		}
	}
	if len(got.Skipped) != 5 || len(got.Days) != 1 || got.Days[0].Rounds != 3 {
		t.Fatalf("skipped %+v days %+v", got.Skipped, got.Days)
	}
}
