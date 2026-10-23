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
}

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
