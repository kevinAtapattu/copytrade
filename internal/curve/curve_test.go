package curve

import (
	"math"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol*math.Max(1, math.Abs(b)) }

var launch = State{VSol: InitialVirtualSol, VTokens: InitialVirtualTokens}

func TestBuyOutConstantProduct(t *testing.T) {
	out, ns, err := launch.BuyOut(1)
	if err != nil {
		t.Fatal(err)
	}
	k0 := launch.VSol * launch.VTokens
	if !near(ns.VSol*ns.VTokens, k0, 1e-12) {
		t.Fatalf("k changed: %v vs %v", ns.VSol*ns.VTokens, k0)
	}
	// 1 SOL at launch: 1.073e9 - 32.19e9/31 = 34,612,903.2 tokens
	if !near(out, 34_612_903.2258, 1e-9) {
		t.Fatalf("tokens out = %v", out)
	}
	if ns.Price() <= launch.Price() {
		t.Fatal("buy should raise price")
	}
}

func TestSellReversesBuy(t *testing.T) {
	out, ns, _ := launch.BuyOut(2)
	sol, back, err := ns.SellOut(out)
	if err != nil {
		t.Fatal(err)
	}
	if !near(sol, 2, 1e-9) || !near(back.VSol, launch.VSol, 1e-9) {
		t.Fatalf("round trip: sol=%v state=%+v", sol, back)
	}
}

func TestCurveComplete(t *testing.T) {
	if _, _, err := launch.BuyOut(100); err != ErrComplete {
		t.Fatalf("expected ErrComplete, got %v", err)
	}
	if _, _, err := launch.BuyOut(80); err != nil {
		t.Fatalf("80 SOL should still fit: %v", err)
	}
}

func TestInvalid(t *testing.T) {
	if _, _, err := (State{}).BuyOut(1); err != ErrInvalid {
		t.Fatal("zero state should be invalid")
	}
	if _, _, err := launch.SellOut(0); err != ErrInvalid {
		t.Fatal("zero sell should be invalid")
	}
}

func TestBuyFees(t *testing.T) {
	f := Fees{PumpPct: 1.25, BotPct: 1, NetworkSol: 0.000505}
	fill, err := Buy(launch, 0.2, f)
	if err != nil {
		t.Fatal(err)
	}
	wantCurve := (0.2 - 0.000505) / 1.0225
	if !near(fill.CurveSol, wantCurve, 1e-12) {
		t.Fatalf("curve sol %v want %v", fill.CurveSol, wantCurve)
	}
	total := fill.CurveSol + fill.PumpFeeSol + fill.BotFeeSol + fill.NetworkSol
	if !near(total, 0.2, 1e-12) {
		t.Fatalf("fees + curve = %v, want budget 0.2", total)
	}
	if !near(fill.PumpFeeSol, wantCurve*0.0125, 1e-12) || !near(fill.BotFeeSol, wantCurve*0.01, 1e-12) {
		t.Fatalf("fee split wrong: %+v", fill)
	}
	if fill.SlippagePct <= 0 {
		t.Fatal("buy slippage should be positive")
	}
	if fill.FillPrice <= fill.CurvePrice || fill.CurvePrice <= fill.SpotPrice {
		t.Fatalf("expected all-in > curve > spot: %+v", fill)
	}
}

func TestSellFees(t *testing.T) {
	f := Fees{PumpPct: 1.25, NetworkSol: 0.0005}
	_, ns, _ := launch.BuyOut(5)
	fill, err := Sell(ns, 10_000_000, f)
	if err != nil {
		t.Fatal(err)
	}
	if !near(fill.SolAmount, fill.CurveSol*(1-0.0125)-0.0005, 1e-12) {
		t.Fatalf("net %v from gross %v", fill.SolAmount, fill.CurveSol)
	}
	if fill.SlippagePct <= 0 {
		t.Fatal("sell slippage should be positive (we got less than spot)")
	}
}

func TestRoundTripCostsAboutFees(t *testing.T) {
	f := Fees{PumpPct: 1.25, NetworkSol: 0.0005}
	b, _ := Buy(launch, 0.2, f)
	_, ns, _ := launch.BuyOut(b.CurveSol)
	s, _ := Sell(ns, b.Tokens, f)
	loss := (0.2 - s.SolAmount) / 0.2
	// ~1.25% in + ~1.25% out + 2 x 0.0005 SOL network on a 0.2 SOL trade (0.5%)
	if loss < 0.029 || loss > 0.031 {
		t.Fatalf("round trip loss %.4f outside expected ~3%%", loss)
	}
}

func TestAMMStateHasNoCompletionLimitAndUsesAMMFee(t *testing.T) {
	// A pool with fewer tokens than a completed curve: the curve would refuse the buy.
	s := State{VSol: 90, VTokens: 200e6, AMM: true}
	f := Fees{PumpPct: 1.25, AMMPct: 0.5}
	b, err := Buy(s, 1, f)
	if err != nil || b.Tokens <= 0 {
		t.Fatalf("buy %+v %v", b, err)
	}
	if want := b.CurveSol * 0.005; b.PumpFeeSol < want*0.999 || b.PumpFeeSol > want*1.001 {
		t.Fatalf("fee %.6f, want AMM 0.5%% = %.6f", b.PumpFeeSol, want)
	}
	if _, err := Buy(State{VSol: 90, VTokens: 200e6}, 1, f); err == nil {
		t.Fatal("a bonding curve past completion must still refuse")
	}
}
