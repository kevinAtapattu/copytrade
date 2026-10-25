// Package curve implements pump.fun bonding-curve math (constant product on
// virtual reserves) and our all-in fee model.
package curve

import "errors"

// pump.fun launch constants (UI token units, 6 decimals; SOL).
const (
	InitialVirtualSol    = 30.0
	InitialVirtualTokens = 1_073_000_000.0
	RealTokenReserves    = 793_100_000.0
	// Virtual tokens left in the curve when it completes and migrates.
	CompleteVirtualTokens = InitialVirtualTokens - RealTokenReserves
)

// LaunchPrice and CompletePrice are the spot prices (SOL/token) at launch and at
// curve completion. A token that migrated ran CompletePrice/LaunchPrice (~14.7x).
const (
	LaunchPrice   = InitialVirtualSol / InitialVirtualTokens
	CompletePrice = InitialVirtualSol * InitialVirtualTokens / (CompleteVirtualTokens * CompleteVirtualTokens)
)

var (
	ErrComplete = errors.New("bonding curve complete")
	ErrInvalid  = errors.New("invalid curve state or amount")
)

// State is the virtual reserve pair. AMM marks a PumpSwap pool (a token that
// graduated): the same constant-product math, but no completion limit and the
// AMM's fee instead of the curve's.
type State struct {
	VSol    float64
	VTokens float64
	AMM     bool
}

func (s State) Valid() bool { return s.VSol > 0 && s.VTokens > 0 }

// Price is SOL per token.
func (s State) Price() float64 {
	if s.VTokens <= 0 {
		return 0
	}
	return s.VSol / s.VTokens
}

// BuyOut returns tokens received for solIn SOL entering the curve (after fees).
func (s State) BuyOut(solIn float64) (float64, State, error) {
	if !s.Valid() || solIn <= 0 {
		return 0, s, ErrInvalid
	}
	k := s.VSol * s.VTokens
	ns := State{VSol: s.VSol + solIn, AMM: s.AMM}
	ns.VTokens = k / ns.VSol
	if !s.AMM && ns.VTokens < CompleteVirtualTokens {
		return 0, s, ErrComplete
	}
	return s.VTokens - ns.VTokens, ns, nil
}

// SellOut returns gross SOL leaving the curve for tokensIn (before fees).
func (s State) SellOut(tokensIn float64) (float64, State, error) {
	if !s.Valid() || tokensIn <= 0 {
		return 0, s, ErrInvalid
	}
	k := s.VSol * s.VTokens
	ns := State{VTokens: s.VTokens + tokensIn, AMM: s.AMM}
	ns.VSol = k / ns.VTokens
	return s.VSol - ns.VSol, ns, nil
}

// Fees is the cost model for one copy trade.
type Fees struct {
	PumpPct    float64 // pump.fun curve fee, percent of SOL side
	AMMPct     float64 // PumpSwap fee (LP + protocol + creator), percent of SOL side, for AMM states
	BotPct     float64 // trading bot fee, percent of SOL side (0 if disabled)
	NetworkSol float64 // base fee + priority fee per transaction
}

// For returns the fees that apply on s: on a PumpSwap pool the venue fee is
// AMMPct (reported in the PumpFeeSol fields).
func (f Fees) For(s State) Fees {
	if s.AMM {
		f.PumpPct = f.AMMPct
	}
	return f
}

// Fill is the result of a simulated buy or sell.
type Fill struct {
	SolAmount   float64 // buy: total SOL spent incl. all fees; sell: net SOL received after all fees
	CurveSol    float64 // SOL that actually entered/left the curve
	Tokens      float64
	SpotPrice   float64 // price before our trade
	CurvePrice  float64 // CurveSol / Tokens (curve only, no fees)
	FillPrice   float64 // all-in: SolAmount / Tokens
	SlippagePct float64 // curve price impact vs spot, percent (buy positive = paid more, sell positive = got less)
	PumpFeeSol  float64
	BotFeeSol   float64
	NetworkSol  float64
}

// Buy spends budgetSol in total. Network fee comes off the top; pump and bot fees
// are charged on top of the SOL that enters the curve, like pump.fun does.
func Buy(s State, budgetSol float64, f Fees) (Fill, error) {
	f = f.For(s)
	curveSol := (budgetSol - f.NetworkSol) / (1 + f.PumpPct/100 + f.BotPct/100)
	if curveSol <= 0 {
		return Fill{}, ErrInvalid
	}
	tokens, _, err := s.BuyOut(curveSol)
	if err != nil {
		return Fill{}, err
	}
	spot := s.Price()
	cp := curveSol / tokens
	return Fill{
		SolAmount: budgetSol, CurveSol: curveSol, Tokens: tokens, SpotPrice: spot, CurvePrice: cp,
		FillPrice: budgetSol / tokens, SlippagePct: (cp/spot - 1) * 100,
		PumpFeeSol: curveSol * f.PumpPct / 100, BotFeeSol: curveSol * f.BotPct / 100, NetworkSol: f.NetworkSol,
	}, nil
}

// Sell sells tokens. Fees are taken from the SOL that leaves the curve.
func Sell(s State, tokens float64, f Fees) (Fill, error) {
	f = f.For(s)
	gross, _, err := s.SellOut(tokens)
	if err != nil {
		return Fill{}, err
	}
	pump := gross * f.PumpPct / 100
	bot := gross * f.BotPct / 100
	net := gross - pump - bot - f.NetworkSol
	spot := s.Price()
	cp := gross / tokens
	return Fill{
		SolAmount: net, CurveSol: gross, Tokens: tokens, SpotPrice: spot, CurvePrice: cp,
		FillPrice: net / tokens, SlippagePct: (1 - cp/spot) * 100,
		PumpFeeSol: pump, BotFeeSol: bot, NetworkSol: f.NetworkSol,
	}, nil
}
