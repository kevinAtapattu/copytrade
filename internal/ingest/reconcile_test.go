package ingest

import (
	"context"
	"testing"
	"time"

	"copytrade/internal/model"
)

type fakeBalances struct {
	held  map[string]uint64 // ATA address -> amount
	owner map[string]float64
	calls int
}

func (f *fakeBalances) GetMultipleAccounts(_ context.Context, addrs []string) ([][]byte, error) {
	f.calls++
	out := make([][]byte, len(addrs))
	for i, a := range addrs {
		if v, ok := f.held[a]; ok {
			b := make([]byte, 165)
			for k := 0; k < 8; k++ {
				b[64+k] = byte(v >> (8 * k))
			}
			out[i] = b
		}
	}
	return out, nil
}

func (f *fakeBalances) OwnerTokenBalance(_ context.Context, owner, mint string) (float64, bool, error) {
	v, ok := f.owner[owner+mint]
	return v, ok, nil
}

const (
	walletA = "FzVQSzj8JJr6WMGqbUHzx2XH1KkrfxRrRPv6WcbbZmND"
	walletB = "36A6mEN5rYJdVTb6fMqVvG6ez8g2mTYdr1omWcQ1kDKG"
	mintX   = "Hiog5ybjwPKa2bENQTyK9yNMEjUdUX9SStdRVRHLpump"
)

// A wallet that no longer holds the token (moved it out, or sold somewhere we
// can't decode) gets a full-exit sell; one that still holds it is left alone.
func TestExitCheck(t *testing.T) {
	in, _, st, pub := setup(t)
	ctx := context.Background()
	st.AddWallet(ctx, model.Wallet{Address: walletA, Status: model.StatusBench})
	st.AddWallet(ctx, model.Wallet{Address: walletB, Status: model.StatusBench})
	in.ReloadWallets(ctx)
	now := t0.Add(time.Hour)
	for _, w := range []string{walletA, walletB} {
		st.UpsertSimPosition(ctx, model.SimPosition{ID: w, Wallet: w, Mint: mintX, DelayS: 2, OpenedAt: t0, Status: model.PosOpen, TokensHeld: 100})
	}
	// Recent copies are skipped (the buy may not show on chain yet).
	st.UpsertSimPosition(ctx, model.SimPosition{ID: "new", Wallet: walletA, Mint: "So11111111111111111111111111111111111111112", DelayS: 2, OpenedAt: now.Add(-30 * time.Second), Status: model.PosOpen})
	st.UpsertTokenFromTrade(ctx, model.Trade{Mint: mintX, VSol: 80, VTokens: 300e6, Pool: model.PoolPumpSwap, ReceivedAt: t0})
	heldATA, _ := ata(walletB, mintX, tokenPrograms[1])
	f := &fakeBalances{held: map[string]uint64{heldATA: 5_000_000}}
	r := &Reconciler{In: in, Store: st, Balances: f, MinAge: 2 * time.Minute, Now: func() time.Time { return now }}
	if err := r.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*pub) != 1 {
		t.Fatalf("published %+v", *pub)
	}
	ex := (*pub)[0]
	if ex.Trader != walletA || ex.Side != model.SideSell || ex.NewTokenBalance != 0 || !ex.IsTarget || ex.Pool != model.PoolPumpSwap || ex.VTokens != 300e6 {
		t.Fatalf("exit trade %+v", ex)
	}
	// Reported once, not every pass.
	if err := r.Check(ctx); err != nil || len(*pub) != 1 {
		t.Fatalf("repeated exit: %d", len(*pub))
	}
	// Tokens held in a non-standard account still count as held.
	in2, _, st2, pub2 := setup(t)
	st2.AddWallet(ctx, model.Wallet{Address: walletA, Status: model.StatusBench})
	in2.ReloadWallets(ctx)
	st2.UpsertSimPosition(ctx, model.SimPosition{ID: "a", Wallet: walletA, Mint: mintX, DelayS: 2, OpenedAt: t0, Status: model.PosOpen})
	r2 := &Reconciler{In: in2, Store: st2, Balances: &fakeBalances{owner: map[string]float64{walletA + mintX: 1e6}}, Now: func() time.Time { return now }}
	if err := r2.Check(ctx); err != nil || len(*pub2) != 0 {
		t.Fatalf("false exit: %+v %v", *pub2, err)
	}
}
