// Package score computes rolling copyability scores from simulated positions.
package score

import (
	"context"
	"math"
	"sort"
	"time"

	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/store"
)

var Windows = []int{14, 30}

type Params struct {
	SizeUSD     float64
	BankrollUSD float64
	Fees        curve.Fees
}

// Outcome is one position valued at a point in time.
type Outcome struct {
	PnlUSD float64
	At     time.Time // close time, or now for open positions
	Closed bool
	HoldS  float64
}

// Value values a position: realized if closed, else marked by selling the rest
// into the token's last known curve state (fees included).
func Value(p model.SimPosition, tok *model.Token, fees curve.Fees, now time.Time) Outcome {
	o := Outcome{At: now}
	if p.Status != model.PosOpen && p.ClosedAt != nil {
		o.Closed = true
		o.At = *p.ClosedAt
		o.HoldS = p.ClosedAt.Sub(p.OpenedAt).Seconds()
		o.PnlUSD = p.RealizedSol() * p.SolUSD
		return o
	}
	mark := 0.0
	if tok != nil && p.TokensHeld > 0 {
		if f, err := curve.Sell(curve.State{VSol: tok.VSol, VTokens: tok.VTokens, AMM: tok.Pool == model.PoolPumpSwap}, p.TokensHeld, fees); err == nil {
			mark = math.Max(f.SolAmount, 0)
		}
	}
	o.PnlUSD = (p.ProceedsSol + mark - p.CostSol) * p.SolUSD
	return o
}

// Compute builds a Score from position outcomes in one window.
func Compute(wallet string, delay, window int, now time.Time, ps []model.SimPosition, outs []Outcome, prm Params) model.Score {
	s := model.Score{Wallet: wallet, DelayS: delay, WindowDays: window, ComputedAt: now, Positions: len(outs)}
	if len(outs) == 0 {
		return s
	}
	best := math.Inf(-1)
	wins := 0
	var hold float64
	for _, o := range outs {
		s.PnlUSD += o.PnlUSD
		if o.PnlUSD > best {
			best = o.PnlUSD
		}
		if o.Closed {
			s.ClosedCount++
			hold += o.HoldS
			if o.PnlUSD > 0 {
				wins++
			}
		}
	}
	s.BestTradeUSD = best
	s.PnlExBestUSD = s.PnlUSD
	if best > 0 {
		s.PnlExBestUSD = s.PnlUSD - best
	}
	s.LuckyTrade = s.PnlUSD > 0 && s.PnlExBestUSD <= 0
	if s.ClosedCount > 0 {
		s.WinRate = float64(wins) / float64(s.ClosedCount)
		s.AvgHoldS = hold / float64(s.ClosedCount)
	}
	s.MaxDrawdown = MaxDrawdown(outs, prm.BankrollUSD)
	s.TradesPerDay = float64(len(outs)) / float64(window)
	first := ps[0].OpenedAt
	for _, p := range ps {
		if p.OpenedAt.Before(first) {
			first = p.OpenedAt
		}
	}
	s.ActiveDays = now.Sub(first).Hours() / 24
	s.Score = ScoreOf(s, prm.SizeUSD)
	return s
}

// ScoreOf: ROI% on capital deployed with the best trade removed, scaled down by drawdown.
// Fewer than 3 positions scores 0 (not enough evidence either way).
func ScoreOf(s model.Score, sizeUSD float64) float64 {
	if s.Positions < 3 || sizeUSD <= 0 {
		return 0
	}
	roi := 100 * s.PnlExBestUSD / (float64(s.Positions) * sizeUSD)
	if roi > 0 {
		roi *= 1 - math.Min(s.MaxDrawdown, 1)
	}
	return math.Round(roi*100) / 100
}

// MaxDrawdown walks equity (bankroll + cumulative PnL in time order) and returns
// the largest peak-to-trough drop as a fraction of the peak.
func MaxDrawdown(outs []Outcome, bankroll float64) float64 {
	if bankroll <= 0 {
		bankroll = 1
	}
	sorted := append([]Outcome(nil), outs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	eq, peak, dd := bankroll, bankroll, 0.0
	for _, o := range sorted {
		eq += o.PnlUSD
		if eq > peak {
			peak = eq
		}
		if peak > 0 {
			dd = math.Max(dd, (peak-eq)/peak)
		}
	}
	return dd
}

// Scorer computes all scores for all non-dropped wallets.
type Scorer struct {
	Store store.Store
	P     Params
}

// Run computes and stores a score snapshot at now. Returns the new scores.
func (sc *Scorer) Run(ctx context.Context, now time.Time) ([]model.Score, error) {
	wallets, err := sc.Store.ListWallets(ctx)
	if err != nil {
		return nil, err
	}
	maxWin := Windows[len(Windows)-1]
	tokens := map[string]*model.Token{}
	tokenOf := func(mint string) *model.Token {
		if t, ok := tokens[mint]; ok {
			return t
		}
		t, ok, _ := sc.Store.GetToken(ctx, mint)
		if !ok {
			tokens[mint] = nil
			return nil
		}
		tokens[mint] = &t
		return &t
	}
	var out []model.Score
	for _, w := range wallets {
		if w.Status == model.StatusDropped {
			continue
		}
		all, err := sc.Store.ListSimPositions(ctx, store.SimFilter{Wallet: w.Address, Since: now.AddDate(0, 0, -maxWin)})
		if err != nil {
			return nil, err
		}
		for _, d := range model.Delays {
			for _, win := range Windows {
				since := now.AddDate(0, 0, -win)
				var ps []model.SimPosition
				var outs []Outcome
				for _, p := range all {
					if p.DelayS != d || p.OpenedAt.Before(since) || p.OpenedAt.After(now) {
						continue
					}
					ps = append(ps, p)
					outs = append(outs, Value(p, tokenOf(p.Mint), sc.P.Fees, now))
				}
				out = append(out, Compute(w.Address, d, win, now, ps, outs, sc.P))
			}
		}
	}
	return out, sc.Store.InsertScores(ctx, out)
}
