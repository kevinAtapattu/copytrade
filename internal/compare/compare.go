// Package compare pairs a wallet's own result on each token with our paper copy's,
// so the dashboard and alerts report the same "them vs us" numbers.
package compare

import (
	"math"
	"sort"
	"strconv"
	"time"

	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/score"
)

// CopyResult is our paper copy of one token at one delay.
type CopyResult struct {
	CostSol float64 `json:"cost_sol"`
	PnlUSD  float64 `json:"pnl_usd"`
	PnlPct  float64 `json:"pnl_pct"`
	Open    bool    `json:"open"`
}

// Row is one token a wallet traded: their return next to ours.
type Row struct {
	Mint       string                 `json:"mint"`
	FirstBuyAt time.Time              `json:"first_buy_at"`
	LastAt     time.Time              `json:"last_at"`
	TheirSolIn float64                `json:"their_sol_in"`
	TheirPct   float64                `json:"their_pct"`  // (sold + held marked) / spent - 1
	TheirOpen  bool                   `json:"their_open"` // still holding >5% of what they bought
	Ours       map[string]*CopyResult `json:"ours"`       // by delay; missing = we did not copy (skipped)
}

// Summary is an equal-weight average over the tokens both sides traded,
// which matches our fixed-size copies: "if every one of their buys were $25".
type Summary struct {
	Tokens   int                `json:"tokens"`
	TheirAvg float64            `json:"their_avg_pct"`
	OurAvg   map[string]float64 `json:"our_avg_pct"` // by delay, over the same tokens
	Copied   map[string]int     `json:"copied"`      // tokens we have a copy for, by delay
	// TheirSame is their average over exactly the tokens we copied at that delay,
	// so TheirSame[d] vs OurAvg[d] is the cost of copying (delay + fees + slippage).
	TheirSame map[string]float64 `json:"their_same_pct"`
	OurPnlUSD map[string]float64 `json:"our_pnl_usd"` // total paper PnL of our copies, by delay
	// HoldMedianS is their median buy-to-last-sell time on tokens they fully exited.
	HoldMedianS float64 `json:"hold_median_s"`
}

// Build pairs a wallet's own trades with our simulated positions per token.
// Their held tokens are marked by selling into the token's last curve state with no
// fees (their real cost is unknown); ours use score.Value, fees included.
func Build(trades []model.Trade, ps []model.SimPosition, toks func(string) *model.Token, fees curve.Fees, now time.Time) ([]Row, Summary) {
	type agg struct {
		solIn, solOut, tokIn, tokOut float64
		first, last                  time.Time
	}
	by := map[string]*agg{}
	for _, t := range trades {
		if t.Pool != "" && t.Pool != "pump" {
			continue // we only copy bonding-curve trades
		}
		a := by[t.Mint]
		if a == nil {
			a = &agg{}
			by[t.Mint] = a
		}
		if t.Side == model.SideBuy {
			if a.first.IsZero() || t.ReceivedAt.Before(a.first) {
				a.first = t.ReceivedAt
			}
			a.solIn += t.SolAmount
			a.tokIn += t.TokenAmount
		} else {
			a.solOut += t.SolAmount
			a.tokOut += t.TokenAmount
		}
		if t.ReceivedAt.After(a.last) {
			a.last = t.ReceivedAt
		}
	}
	ours := map[string]map[string]*CopyResult{}
	for _, p := range ps {
		o := score.Value(p, toks(p.Mint), fees, now)
		if ours[p.Mint] == nil {
			ours[p.Mint] = map[string]*CopyResult{}
		}
		d := strconv.Itoa(p.DelayS)
		c := ours[p.Mint][d]
		if c == nil {
			c = &CopyResult{}
			ours[p.Mint][d] = c
		}
		c.CostSol += p.CostSol
		c.PnlUSD += o.PnlUSD
		if p.CostSol > 0 && p.SolUSD > 0 {
			// accumulate SOL PnL to turn into a percent below
			c.PnlPct += o.PnlUSD / p.SolUSD
		}
		c.Open = c.Open || !o.Closed
	}
	var rows []Row
	for mint, a := range by {
		if a.solIn <= 0 || a.tokIn <= 0 {
			continue // bought before we tracked them
		}
		held := math.Max(a.tokIn-a.tokOut, 0)
		mark := 0.0
		if tok := toks(mint); tok != nil && held > 0 {
			if f, err := curve.Sell(curve.State{VSol: tok.VSol, VTokens: tok.VTokens, AMM: tok.Pool == model.PoolPumpSwap}, held, curve.Fees{}); err == nil {
				mark = math.Max(f.SolAmount, 0)
			}
		}
		r := Row{Mint: mint, FirstBuyAt: a.first, LastAt: a.last, TheirSolIn: a.solIn,
			TheirPct: (a.solOut+mark)/a.solIn - 1, TheirOpen: held > a.tokIn*0.05, Ours: map[string]*CopyResult{}}
		for d, c := range ours[mint] {
			if c.CostSol > 0 {
				c.PnlPct /= c.CostSol
			}
			r.Ours[d] = c
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].FirstBuyAt.After(rows[j].FirstBuyAt) })

	sum := Summary{OurAvg: map[string]float64{}, Copied: map[string]int{}, TheirSame: map[string]float64{}, OurPnlUSD: map[string]float64{}}
	var holds []float64
	for _, r := range rows {
		sum.Tokens++
		if !r.TheirOpen && r.LastAt.After(r.FirstBuyAt) {
			holds = append(holds, r.LastAt.Sub(r.FirstBuyAt).Seconds())
		}
		sum.TheirAvg += r.TheirPct
		for _, d := range model.Delays {
			k := strconv.Itoa(d)
			if c := r.Ours[k]; c != nil {
				sum.OurAvg[k] += c.PnlPct
				sum.OurPnlUSD[k] += c.PnlUSD
				sum.TheirSame[k] += r.TheirPct
				sum.Copied[k]++
			}
		}
	}
	if sum.Tokens > 0 {
		sum.TheirAvg /= float64(sum.Tokens)
	}
	if len(holds) > 0 {
		sort.Float64s(holds)
		sum.HoldMedianS = holds[len(holds)/2]
	}
	for k, n := range sum.Copied {
		if n > 0 {
			sum.OurAvg[k] /= float64(n)
			sum.TheirSame[k] /= float64(n)
		}
	}
	return rows, sum
}
