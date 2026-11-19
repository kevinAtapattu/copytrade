package api

import (
	"math"
	"net/http"
	"sort"
	"time"

	"copytrade/internal/model"
	"copytrade/internal/store"
)

// Other coins: how the copied wallet does on pump.fun coins priced in a token
// other than SOL (pool "nonsol"), which we never copy. There is almost no other
// trader data on these coins, so this is the wallet's own fill-to-fill result,
// not a delayed copy. Read-only, and kept out of every other total.
//
// sol_amount on these trades is an amount of the quote token, not SOL, and its
// scale is unknown. So each round is a percentage in that coin's own quote
// units, and money is "$ per $10 copy" (pct * 10). Raw amounts are never added
// across coins.
//
// Bonding-curve trades on these coins carry no quote amount (the curve event
// reports 0), so a coin with any unpriced trade can't be matched: all unpriced
// is skipped as "no_price", a mix of priced and unpriced is flagged "mixed".

const (
	otherDust = 0.01 // a round closes when holdings drop under 1% of their peak
	otherSlop = 1.05 // 5% slack before a balance counts as "held before tracking"
)

type otherRound struct {
	Wallet   string     `json:"wallet"`
	Mint     string     `json:"mint"`
	OpenedAt time.Time  `json:"opened_at"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	HoldS    float64    `json:"hold_s"`
	Buys     int        `json:"buys"`
	Sells    int        `json:"sells"`
	Pct      float64    `json:"pct"`   // percent return, nad's prices
	Per10    float64    `json:"per10"` // $ per $10 copy
	pct      float64
}

type otherSkip struct {
	Wallet string `json:"wallet"`
	Mint   string `json:"mint"`
	Reason string `json:"reason"` // no_price | mixed | before_tracking | sell_only
	Trades int    `json:"trades"`
	Note   string `json:"note,omitempty"`
}

type otherDay struct {
	Date   string  `json:"date"`
	Rounds int     `json:"rounds"`
	Won    int     `json:"won"`
	Per10  float64 `json:"per10"`
}

type otherResult struct {
	Rounds  []otherRound
	Open    []otherRound
	Skipped []otherSkip
}

// curvePath: trade parsed from a bonding-curve event (no reserves in SOL terms).
func curvePath(t model.Trade) bool { return t.VSol <= 0 }

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// checkUnits says whether a coin's trades can be matched in one unit.
func checkUnits(ts []model.Trade) (reason, note string) {
	priced := 0
	px := map[bool][]float64{}
	for _, t := range ts {
		if t.SolAmount > 0 && t.TokenAmount > 0 {
			priced++
			px[curvePath(t)] = append(px[curvePath(t)], t.SolAmount/t.TokenAmount)
		}
	}
	switch {
	case priced == 0:
		return "no_price", "curve trades record no quote amount"
	case priced < len(ts):
		return "mixed", "some trades record no quote amount, units can't be checked"
	case len(px[true]) > 0 && len(px[false]) > 0:
		// Both parse paths priced: their prices should be in the same ballpark.
		r := median(px[true]) / median(px[false])
		if r > 3 || r < 1.0/3 {
			return "mixed", "curve and AMM prices differ in scale"
		}
	}
	return "", ""
}

// buildOtherCoins matches each wallet's buys and sells per coin. A round closes
// when holdings (by token amount) go to about zero; its return is everything the
// sells got over everything the buys paid, which is average-cost matching once
// the whole position is out.
func buildOtherCoins(trades []model.Trade) otherResult {
	res := otherResult{Rounds: []otherRound{}, Open: []otherRound{}, Skipped: []otherSkip{}}
	type key struct{ wallet, mint string }
	by := map[key][]model.Trade{}
	var keys []key
	for _, t := range trades {
		if t.Pool != "nonsol" {
			continue
		}
		k := key{t.Trader, t.Mint}
		if by[k] == nil {
			keys = append(keys, k)
		}
		by[k] = append(by[k], t)
	}
	for _, k := range keys {
		ts := by[k]
		sort.SliceStable(ts, func(i, j int) bool {
			if !ts[i].ReceivedAt.Equal(ts[j].ReceivedAt) {
				return ts[i].ReceivedAt.Before(ts[j].ReceivedAt)
			}
			return ts[i].Signature < ts[j].Signature
		})
		if reason, note := checkUnits(ts); reason != "" {
			res.Skipped = append(res.Skipped, otherSkip{k.wallet, k.mint, reason, len(ts), note})
			continue
		}
		var (
			r                  *otherRound
			hold, peak, paid   float64
			got                float64
			tainted            bool
			taintedN, sellOnly int
		)
		for _, t := range ts {
			if t.Side == model.SideBuy {
				if r == nil {
					r = &otherRound{Wallet: k.wallet, Mint: k.mint, OpenedAt: t.ReceivedAt}
					hold, peak, paid, got, tainted = 0, 0, 0, 0, false
					// Already held more than this buy: bought before we tracked them.
					if t.NewTokenBalance > t.TokenAmount*otherSlop {
						tainted = true
					}
				}
				r.Buys++
				hold += t.TokenAmount
				paid += t.SolAmount
				peak = math.Max(peak, hold)
				continue
			}
			if r == nil {
				sellOnly++ // no recorded buy
				continue
			}
			r.Sells++
			if t.TokenAmount > hold*otherSlop {
				tainted = true // sold more than we saw them buy
			}
			got += t.SolAmount
			hold -= math.Min(t.TokenAmount, hold)
			if hold > peak*otherDust {
				continue
			}
			at := t.ReceivedAt
			if tainted {
				taintedN++
			} else if paid > 0 {
				r.ClosedAt = &at
				r.HoldS = at.Sub(r.OpenedAt).Seconds()
				r.pct = got/paid - 1
				r.Pct = round2(r.pct * 100)
				r.Per10 = round2(r.pct * 10)
				res.Rounds = append(res.Rounds, *r)
			}
			r = nil
		}
		if r != nil {
			if tainted {
				taintedN++
			} else {
				res.Open = append(res.Open, *r)
			}
		}
		if taintedN > 0 {
			res.Skipped = append(res.Skipped, otherSkip{k.wallet, k.mint, "before_tracking", taintedN, "held tokens bought before we tracked them"})
		}
		if sellOnly > 0 {
			res.Skipped = append(res.Skipped, otherSkip{k.wallet, k.mint, "sell_only", sellOnly, "sells with no recorded buy"})
		}
	}
	sort.Slice(res.Rounds, func(i, j int) bool { return res.Rounds[i].ClosedAt.After(*res.Rounds[j].ClosedAt) })
	sort.Slice(res.Open, func(i, j int) bool { return res.Open[i].OpenedAt.After(res.Open[j].OpenedAt) })
	return res
}

// otherCoins: GET /api/other-coins?wallet=ADDR (default: the live wallets).
func (s *Server) otherCoins(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var wallets []string
	if wl := r.URL.Query().Get("wallet"); wl != "" {
		wallets = []string{wl}
	} else {
		ws, err := s.Store.ListWallets(ctx)
		if err != nil {
			fail(w, 500, err)
			return
		}
		for _, x := range ws {
			if x.Status == model.StatusLive {
				wallets = append(wallets, x.Address)
			}
		}
	}
	loc, err := time.LoadLocation(s.Cfg.AlertTZ)
	if err != nil {
		loc = time.UTC
	}
	var trades []model.Trade
	for _, wl := range wallets {
		ts, err := s.Store.ListTargetTrades(ctx, store.TradeFilter{Wallet: wl})
		if err != nil {
			fail(w, 500, err)
			return
		}
		trades = append(trades, ts...)
	}
	res := buildOtherCoins(trades)

	days := map[string]*otherDay{}
	total, best, won := 0.0, math.Inf(-1), 0
	for _, rd := range res.Rounds {
		p := rd.pct * 10
		total += p
		best = math.Max(best, p)
		k := dayOf(*rd.ClosedAt, loc)
		d := days[k]
		if d == nil {
			d = &otherDay{Date: k}
			days[k] = d
		}
		d.Rounds++
		d.Per10 += p
		if rd.pct > 0 {
			won++
			d.Won++
		}
	}
	dayList := []otherDay{}
	for _, d := range days {
		d.Per10 = round2(d.Per10)
		dayList = append(dayList, *d)
	}
	sort.Slice(dayList, func(i, j int) bool { return dayList[i].Date > dayList[j].Date })
	summary := map[string]any{"rounds": len(res.Rounds), "won": won, "open": len(res.Open),
		"total_per10": round2(total), "skipped": len(res.Skipped)}
	if n := len(res.Rounds); n > 0 {
		summary["win_rate"] = round2(float64(won) / float64(n) * 100)
		summary["best_per10"] = round2(best)
		summary["without_best_per10"] = round2(total - best)
	}
	writeJSON(w, 200, map[string]any{"tz": loc.String(), "wallets": wallets, "summary": summary,
		"days": dayList, "rounds": res.Rounds, "open": res.Open, "skipped": res.Skipped})
}
