package snapshot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/store"
)

type fakeReader struct {
	accts map[string]curve.Account
	calls int
}

func (f *fakeReader) Read(_ context.Context, keys []string, _ time.Time) ([]Result, error) {
	f.calls++
	out := make([]Result, len(keys))
	for i, k := range keys {
		if a, ok := f.accts[k]; ok {
			out[i] = Result{Found: true, Account: a}
		}
	}
	return out, nil
}

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func TestClassifyFlagsNonSOLOncePerMint(t *testing.T) {
	r := &fakeReader{accts: map[string]curve.Account{
		"C1": {State: curve.State{VSol: 31, VTokens: 1.04e9}},
		"C2": {State: curve.State{VSol: 5539, VTokens: 1.07e9}, QuoteMint: "WXMRyRZhsa19ety5erZhHg4N3xj3EVN92u94422teJp"},
	}}
	s := New(r, bus.NewMemory(), store.NewMemory(), model.Delays, time.Minute)
	ctx := context.Background()
	sol := s.Classify(ctx, model.Trade{Mint: "M1", BondingCurveKey: "C1", IsTarget: true, Pool: "pump", ReceivedAt: t0})
	odd := s.Classify(ctx, model.Trade{Mint: "M2", BondingCurveKey: "C2", IsTarget: true, Pool: "pump", ReceivedAt: t0})
	again := s.Classify(ctx, model.Trade{Mint: "M2", BondingCurveKey: "C2", IsTarget: true, Pool: "pump", ReceivedAt: t0})
	if sol.Pool != "pump" || odd.Pool != PoolNonSOL || again.Pool != PoolNonSOL {
		t.Fatalf("pools %s %s %s", sol.Pool, odd.Pool, again.Pool)
	}
	if r.calls != 2 {
		t.Fatalf("expected one read per mint, got %d", r.calls)
	}
	if s.Classify(ctx, model.Trade{Mint: "M3", BondingCurveKey: "C3", IsTarget: false, Pool: "pump"}); r.calls != 2 {
		t.Fatal("untracked trades must not cost reads")
	}
}

func TestTickPublishesFillAndMarkReads(t *testing.T) {
	r := &fakeReader{accts: map[string]curve.Account{"C1": {State: curve.State{VSol: 40, VTokens: 8e8}}}}
	b := bus.NewMemory()
	var got []model.Trade
	b.Register(bus.StreamTrades, func(_ context.Context, p []byte) error {
		var tr model.Trade
		jsonUnmarshal(p, &tr)
		got = append(got, tr)
		return nil
	})
	s := New(r, b, store.NewMemory(), []int{2, 10}, 30*time.Second)
	ctx := context.Background()
	s.OnTrade(model.Trade{Mint: "M1", BondingCurveKey: "C1", IsTarget: true, Pool: "pump", ReceivedAt: t0})
	s.SubscribeTokens([]string{"M1"})
	s.Tick(ctx, t0.Add(time.Second)) // mark only (first mark fires immediately)
	s.Tick(ctx, t0.Add(2500*time.Millisecond))
	s.Tick(ctx, t0.Add(11*time.Second))
	if len(got) != 3 {
		t.Fatalf("published %d snapshots, want mark + 2 fills", len(got))
	}
	if !got[1].ReceivedAt.Equal(t0.Add(2*time.Second)) || !got[2].ReceivedAt.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("fill snapshots must be stamped at their due time: %v %v", got[1].ReceivedAt, got[2].ReceivedAt)
	}
	if got[0].Side != SideSnapshot || got[0].IsTarget || got[0].VSol != 40 {
		t.Fatalf("snapshot shape %+v", got[0])
	}
	if st := s.Stats(); st.Pending != 0 || st.Watched != 1 {
		t.Fatalf("stats %+v", st)
	}
	s.UnsubscribeTokens([]string{"M1"})
	s.Tick(ctx, t0.Add(5*time.Minute))
	if len(got) != 3 {
		t.Fatal("unwatched token must not be marked")
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

type fakeAccounts map[string][]byte

func (f fakeAccounts) GetMultipleAccounts(_ context.Context, addrs []string) ([][]byte, error) {
	out := make([][]byte, len(addrs))
	for i, a := range addrs {
		out[i] = f[a]
	}
	return out, nil
}

func vault(amount uint64) []byte {
	b := make([]byte, 165)
	for i := 0; i < 8; i++ {
		b[64+i] = byte(amount >> (8 * i))
	}
	return b
}

// A graduated token's curve is frozen; the snapshot must switch to its PumpSwap
// pool (vaults + virtual SOL reserve) and publish pool "pumpswap".
func TestGraduatedTokenReadFromPumpSwapPool(t *testing.T) {
	raw, err := os.ReadFile("../pumpswap/testdata/mainnet.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		PoolB64 string `json:"pool_b64"`
	}
	_ = json.Unmarshal(raw, &fx)
	poolData, _ := base64.StdEncoding.DecodeString(fx.PoolB64)
	const mint = "Hiog5ybjwPKa2bENQTyK9yNMEjUdUX9SStdRVRHLpump"
	accts := fakeAccounts{
		"4xMCDt7EuE1HDA6h4AF2F9xQWSBk1BXduj3y7eukkVby": poolData,
		"2TGBhPbrxYP5XFhDYnRdPSxdBAtnqWJNxbjvUnP6WT5N": vault(500_000_000_000_000), // 500M tokens
		"2nDPiv8FZfGr9z8ryUshaMK623SyJQRbdoiRarCcE9oa": vault(12_000_000_000),      // 12 SOL
	}
	r := &fakeReader{accts: map[string]curve.Account{"CURVE": {State: curve.State{VSol: 115, VTokens: 279.9e6}, Complete: true}}}
	b := bus.NewMemory()
	var pub []model.Trade
	b.Register(bus.StreamTrades, func(_ context.Context, p []byte) error {
		var tr model.Trade
		_ = jsonUnmarshal(p, &tr)
		pub = append(pub, tr)
		return nil
	})
	st := store.NewMemory()
	s := New(r, b, st, model.Delays, time.Minute)
	s.Accounts = accts
	s.OnTrade(model.Trade{Mint: mint, BondingCurveKey: "CURVE", IsTarget: true, Pool: "pump", ReceivedAt: t0})
	ctx := context.Background()
	s.Tick(ctx, t0.Add(2*time.Second))
	if len(pub) != 1 || pub[0].Pool != model.PoolPumpSwap || pub[0].VTokens != 500e6 || pub[0].VSol < 29.5 || pub[0].VSol > 29.7 {
		t.Fatalf("published %+v", pub)
	}
	calls := r.calls
	s.Tick(ctx, t0.Add(10*time.Second))
	if r.calls != calls || len(pub) != 2 || pub[1].Pool != model.PoolPumpSwap {
		t.Fatalf("after graduating, reads must go to the pool only: curve calls %d->%d, %+v", calls, r.calls, pub)
	}
	if tok, ok, _ := st.GetToken(ctx, mint); !ok || tok.Pool != model.PoolPumpSwap || !tok.Migrated || tok.VTokens != 500e6 {
		t.Fatalf("token %+v", tok)
	}
}
