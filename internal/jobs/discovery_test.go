package jobs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/config"
	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/store"
)

type fakeHistory struct {
	byMint map[string][]model.Trade
	budget int // tokens served before ErrBudget; <0 = unlimited
	served int
}

func (f *fakeHistory) TokenTrades(_ context.Context, tok model.Token) ([]model.Trade, error) {
	if f.budget >= 0 && f.served >= f.budget {
		return nil, ErrBudget
	}
	f.served++
	return f.byMint[tok.Mint], nil
}

// trade on the curve at a given virtual SOL level, so price follows the constant product.
func curveTrade(mint, who, side string, vSol, sol float64, at time.Time) model.Trade {
	vTok := curve.InitialVirtualSol * curve.InitialVirtualTokens / vSol
	return model.Trade{Signature: fmt.Sprintf("%s-%s-%s-%v", mint, who, side, vSol), Mint: mint, Trader: who, Side: side,
		SolAmount: sol, TokenAmount: sol / (vSol / vTok), VSol: vSol, VTokens: vTok, Pool: "pump", ReceivedAt: at}
}

// sellAll makes t a sell of the whole bag at t's spot price.
func sellAll(t model.Trade, tokens float64) model.Trade {
	t.TokenAmount, t.SolAmount = tokens, tokens*t.Price()
	return t
}

func TestDiscoverFindsRepeatEarlyBuyerOnMigratedTokens(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	c := config.Load()
	now := time.Now().UTC()
	hist := &fakeHistory{byMint: map[string][]model.Trade{}, budget: -1}
	for i, mint := range []string{"MINT_A", "MINT_B"} {
		mig := now.Add(-time.Duration(i+1) * time.Hour)
		if _, err := st.SeedMigratedToken(ctx, model.Token{Mint: mint, FirstSeen: mig, FirstPrice: curve.LaunchPrice,
			MaxPrice: curve.CompletePrice, Pool: "pump-amm", LastTradeAt: mig}); err != nil {
			t.Fatal(err)
		}
		start := mig.Add(-30 * time.Minute)
		buy := curveTrade(mint, "SMART", model.SideBuy, 31, 0.5, start)                  // ~1.07x launch: early
		late := curveTrade(mint, "LATE", model.SideBuy, 50, 0.5, start.Add(time.Minute)) // ~2.8x launch: not early
		hist.byMint[mint] = []model.Trade{buy, late,
			sellAll(curveTrade(mint, "SMART", model.SideSell, 55, 0, start.Add(2*time.Minute)), buy.TokenAmount), // ~3x exit
			sellAll(curveTrade(mint, "LATE", model.SideSell, 60, 0, start.Add(3*time.Minute)), late.TokenAmount),
		}
	}
	res, err := Discover(ctx, st, bus.NewMemory(), c, now, hist)
	if err != nil {
		t.Fatal(err)
	}
	if res.TokensScanned != 2 || len(res.Added) != 1 || res.Added[0] != "SMART" {
		t.Fatalf("want SMART added from 2 tokens, got %+v", res)
	}
	ws, _ := st.ListWallets(ctx)
	if len(ws) != 1 || ws[0].Status != model.StatusBench || ws[0].Source != model.SourceDiscovery {
		t.Fatalf("bad wallets %+v", ws)
	}
	if left, _ := st.ListTokens(ctx, c.DiscoveryMult, true); len(left) != 0 {
		t.Fatalf("scanned tokens should be marked discovered, %d left", len(left))
	}
}

func TestDiscoverStopsAtBudgetAndScansNewestFirst(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	c := config.Load()
	now := time.Now().UTC()
	for i, mint := range []string{"NEW", "MID", "OLD"} {
		at := now.Add(-time.Duration(i) * time.Hour)
		st.SeedMigratedToken(ctx, model.Token{Mint: mint, FirstSeen: at, FirstPrice: curve.LaunchPrice, MaxPrice: curve.CompletePrice})
	}
	res, err := Discover(ctx, st, bus.NewMemory(), c, now, &fakeHistory{budget: 1})
	if err != nil {
		t.Fatal(err)
	}
	left, _ := st.ListTokens(ctx, c.DiscoveryMult, true)
	if res.TokensScanned != 1 || len(left) != 2 {
		t.Fatalf("scanned %d, %d left", res.TokensScanned, len(left))
	}
	for _, tok := range left {
		if tok.Mint == "NEW" {
			t.Fatal("newest token should be scanned first")
		}
	}
}
