package ingest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/model"
	"copytrade/internal/source"
	"copytrade/internal/store"
)

type fakeSrc struct {
	source.Replay
	subT, unsubT, subA, unsubA [][]string
	pauses, resumes            int
}

func (f *fakeSrc) Pause()  { f.pauses++ }
func (f *fakeSrc) Resume() { f.resumes++ }

func (f *fakeSrc) SubscribeTokens(k []string)     { f.subT = append(f.subT, k) }
func (f *fakeSrc) UnsubscribeTokens(k []string)   { f.unsubT = append(f.unsubT, k) }
func (f *fakeSrc) SubscribeAccounts(k []string)   { f.subA = append(f.subA, k) }
func (f *fakeSrc) UnsubscribeAccounts(k []string) { f.unsubA = append(f.unsubA, k) }

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*Ingestor, *fakeSrc, *store.Memory, *[]model.Trade) {
	st := store.NewMemory()
	ctx := context.Background()
	st.AddWallet(ctx, model.Wallet{Address: "W1", Status: model.StatusBench})
	st.AddWallet(ctx, model.Wallet{Address: "W2", Status: model.StatusLive})
	st.AddWallet(ctx, model.Wallet{Address: "W3", Status: model.StatusDropped})
	b := bus.NewMemory()
	var pub []model.Trade
	b.Register(bus.StreamTrades, func(_ context.Context, p []byte) error {
		var tr model.Trade
		_ = jsonUnmarshal(p, &tr)
		pub = append(pub, tr)
		return nil
	})
	src := &fakeSrc{}
	in := New(src, st, b, time.Minute, 24*time.Hour)
	if err := in.ReloadWallets(ctx); err != nil {
		t.Fatal(err)
	}
	return in, src, st, &pub
}

func tr(sig, trader, side string, bal float64, at time.Duration) model.Trade {
	return model.Trade{Signature: sig, Mint: "M", Trader: trader, Side: side, NewTokenBalance: bal, TokenAmount: 10,
		VSol: 31, VTokens: 1e9, Pool: "pump", ReceivedAt: t0.Add(at)}
}

func TestTracksNonDroppedWallets(t *testing.T) {
	_, src, _, _ := setup(t)
	if len(src.subA) != 1 || len(src.subA[0]) != 2 {
		t.Fatalf("subscribed accounts %v", src.subA)
	}
}

func TestTokenSubscribeAndCooldownUnsub(t *testing.T) {
	in, src, st, pub := setup(t)
	ctx := context.Background()
	in.HandleTrade(ctx, tr("s1", "W1", "buy", 100, 0))
	if len(src.subT) != 1 || src.subT[0][0] != "M" {
		t.Fatalf("should subscribe token on tracked buy: %v", src.subT)
	}
	in.HandleTrade(ctx, tr("s2", "W2", "buy", 50, time.Second))
	if len(src.subT) != 1 {
		t.Fatal("second holder must not resubscribe")
	}
	in.HandleTrade(ctx, tr("s1", "W1", "buy", 100, 0)) // duplicate frame
	in.HandleTrade(ctx, tr("s3", "OTHER", "buy", 5, 2*time.Second))
	in.HandleTrade(ctx, tr("s4", "W1", "sell", 0, time.Minute))
	in.Tick(t0.Add(10 * time.Minute))
	if len(src.unsubT) != 0 {
		t.Fatal("W2 still holds; must stay subscribed")
	}
	in.HandleTrade(ctx, tr("s5", "W2", "sell", 0.5, 2*time.Minute)) // dust = exited
	in.Tick(t0.Add(2*time.Minute + 30*time.Second))
	if len(src.unsubT) != 0 {
		t.Fatal("unsubscribed before cooldown (min 90s)")
	}
	in.Tick(t0.Add(2*time.Minute + 91*time.Second))
	if len(src.unsubT) != 1 || in.IsWatching("M") {
		t.Fatalf("expected unsubscribe after cooldown: %v", src.unsubT)
	}
	if len(*pub) != 5 {
		t.Fatalf("published %d trades, want 5 (dupe dropped)", len(*pub))
	}
	targets, _ := st.ListTargetTrades(ctx, store.TradeFilter{})
	all, _ := st.ListTokenTrades(ctx, store.TradeFilter{})
	if len(targets) != 4 || len(all) != 5 {
		t.Fatalf("stored %d target / %d token trades", len(targets), len(all))
	}
	for _, p := range *pub {
		if p.Trader == "OTHER" && p.IsTarget || p.Trader == "W1" && !p.IsTarget {
			t.Fatalf("IsTarget wrong for %s", p.Trader)
		}
	}
}

func TestMaxWatch(t *testing.T) {
	in, src, _, _ := setup(t)
	in.MaxWatch = time.Hour
	in.HandleTrade(context.Background(), tr("s1", "W1", "buy", 100, 0))
	in.Tick(t0.Add(61 * time.Minute))
	if len(src.unsubT) != 1 {
		t.Fatal("token watched past MaxWatch should be dropped")
	}
}

func TestDroppedWalletUnsubscribed(t *testing.T) {
	in, src, st, _ := setup(t)
	ctx := context.Background()
	st.SetWalletStatus(ctx, "W1", model.StatusDropped, "x", t0)
	in.ReloadWallets(ctx)
	if len(src.unsubA) != 1 || src.unsubA[0][0] != "W1" {
		t.Fatalf("unsub accounts %v", src.unsubA)
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func TestRestoreWatchesAfterRestart(t *testing.T) {
	in, _, st, _ := setup(t)
	ctx := context.Background()
	in.HandleTrade(ctx, tr("a", "W1", "buy", 100, 0))
	in.HandleTrade(ctx, model.Trade{Signature: "b", Mint: "M2", Trader: "W1", Side: "buy", NewTokenBalance: 50, ReceivedAt: t0})
	in.HandleTrade(ctx, model.Trade{Signature: "c", Mint: "M2", Trader: "W1", Side: "sell", NewTokenBalance: 0, ReceivedAt: t0.Add(time.Minute)})

	// new process, same DB
	src2 := &fakeSrc{}
	in2 := New(src2, st, bus.NewMemory(), time.Minute, 24*time.Hour)
	in2.ReloadWallets(ctx)
	if err := in2.RestoreWatches(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(src2.subT) != 1 || len(src2.subT[0]) != 1 || src2.subT[0][0] != "M" || !in2.IsWatching("M") || in2.IsWatching("M2") {
		t.Fatalf("restored %v", src2.subT)
	}
}

func TestDailyBudgetPausesAndResumes(t *testing.T) {
	in, src, _, _ := setup(t)
	ctx := context.Background()
	in.Guard = Guard{DailyBudget: 3}
	in.InitBudget(ctx, t0)
	in.HandleTrade(ctx, tr("a", "W1", "buy", 100, 0))
	in.HandleTrade(ctx, tr("a", "W1", "buy", 100, 0)) // duplicate still costs money
	if in.GuardStats().Paused {
		t.Fatal("paused early")
	}
	in.HandleTrade(ctx, tr("b", "W2", "buy", 100, time.Second))
	st := in.GuardStats()
	if !st.Paused || st.MessagesToday != 3 {
		t.Fatalf("expected pause at budget: %+v", st)
	}
	if src.pauses != 1 || len(src.unsubT) != 1 {
		t.Fatalf("pause must close the connection and stop token reads: pauses %d tokens %v", src.pauses, src.unsubT)
	}
	// while paused, reloads must not resubscribe
	before := len(src.subA)
	in.ReloadWallets(ctx)
	if len(src.subA) != before {
		t.Fatal("resubscribed while paused")
	}
	in.Tick(t0.Add(12 * time.Hour)) // same UTC day
	if !in.GuardStats().Paused {
		t.Fatal("resumed before midnight")
	}
	in.Tick(t0.Add(25 * time.Hour))
	st = in.GuardStats()
	if st.Paused || st.MessagesToday != 0 {
		t.Fatalf("should resume next day: %+v", st)
	}
	last := src.subA[len(src.subA)-1]
	if src.resumes != 1 || len(last) != 2 || len(src.subT) < 2 {
		t.Fatalf("resume must reconnect (%d) and restore accounts %v and tokens %v", src.resumes, last, src.subT)
	}
}

func TestInitBudgetFromDB(t *testing.T) {
	in, src, st, _ := setup(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		st.InsertTokenTrade(ctx, tr(string(rune('a'+i)), "X", "buy", 1, time.Duration(i)*time.Minute))
	}
	in.Guard = Guard{DailyBudget: 5}
	in.InitBudget(ctx, t0.Add(time.Hour))
	if s := in.GuardStats(); !s.Paused || s.MessagesToday != 5 || src.pauses != 1 {
		t.Fatalf("restart over budget should start paused: %+v", s)
	}
}

func TestSpamGuardPausesWalletThenResumes(t *testing.T) {
	in, src, st, _ := setup(t)
	ctx := context.Background()
	in.Guard = Guard{WalletMaxPerDay: 3}
	for i := 0; i < 3; i++ {
		in.HandleTrade(ctx, tr(string(rune('a'+i)), "W1", "buy", 100, time.Duration(i)*10*time.Minute))
	}
	// 3 today is fine; the count starts over on the next UTC day
	for i := 0; i < 3; i++ {
		in.HandleTrade(ctx, tr(string(rune('d'+i)), "W1", "buy", 100, 25*time.Hour+time.Duration(i)*time.Minute))
	}
	if w, _, _ := st.GetWallet(ctx, "W1"); w.Status == model.StatusPaused {
		t.Fatal("paused though only 3 trades in each day")
	}
	in.HandleTrade(ctx, tr("z", "W1", "buy", 100, 26*time.Hour))
	w, _, _ := st.GetWallet(ctx, "W1")
	if w.Status != model.StatusPaused || len(src.unsubA) != 1 || src.unsubA[0][0] != "W1" {
		t.Fatalf("over-limit wallet not paused: status %s unsub %v", w.Status, src.unsubA)
	}
	resubscribed := func() bool {
		for _, sub := range src.subA[1:] {
			for _, a := range sub {
				if a == "W1" {
					return true
				}
			}
		}
		return false
	}
	// Later the same day: a reload keeps it untracked.
	in.Now = func() time.Time { return w.StatusAt.Add(time.Hour) }
	in.ReloadWallets(ctx)
	if resubscribed() {
		t.Fatal("paused spammer was resubscribed")
	}
	// After the pause it is back on the bench and tracked again.
	in.Now = func() time.Time { return w.StatusAt.UTC().Truncate(24 * time.Hour).Add(24*time.Hour + time.Minute) }
	in.ReloadWallets(ctx)
	if w, _, _ := st.GetWallet(ctx, "W1"); w.Status != model.StatusBench {
		t.Fatalf("status after resume %s", w.Status)
	}
	if !resubscribed() {
		t.Fatal("resumed wallet not resubscribed")
	}
}
