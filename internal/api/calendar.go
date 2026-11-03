package api

import (
	"net/http"
	"sort"
	"time"

	"copytrade/internal/compare"
	"copytrade/internal/model"
	"copytrade/internal/score"
	"copytrade/internal/sim"
	"copytrade/internal/store"
)

// The calendar answers "what would copying only our best wallets have made each
// day?". Each morning it picks the top N wallets from results known by then, and
// takes only the copies opened that day by a picked wallet. A copy counts on the
// day it closed. Next to ours it shows what the same trades made the wallet
// itself, scaled to our size: their return on the token times our cost.

const (
	pickLookback  = 14 * 24 * time.Hour
	pickMinClosed = 5
	// Snipers flip within seconds; 2s late we buy after their pump (5KhMUK: our
	// buys averaged 41% above theirs, median hold 6s). Not copyable, never picked.
	pickMinHold = time.Minute
)

type calCopy struct {
	Mint     string     `json:"mint"`
	OpenedAt time.Time  `json:"opened_at"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	CostUSD  float64    `json:"cost_usd"`
	UsUSD    float64    `json:"us_usd"`
	ThemUSD  *float64   `json:"them_usd"` // nil: we don't have their whole trade (bought before we tracked them)
}

type calWallet struct {
	Address string    `json:"address"`
	UsUSD   float64   `json:"us_usd"`
	ThemUSD float64   `json:"them_usd"`
	Closed  int       `json:"closed"`
	Won     int       `json:"won"`
	Copies  []calCopy `json:"copies,omitempty"`
}

type calDay struct {
	Date    string      `json:"date"`
	UsUSD   float64     `json:"us_usd"`
	ThemUSD float64     `json:"them_usd"`
	Closed  int         `json:"closed"`
	Won     int         `json:"won"`
	Picks   []string    `json:"picks,omitempty"`   // wallets picked that morning (best mode)
	Wallets []calWallet `json:"wallets,omitempty"` // only with ?date=
}

// pickRank is one wallet's record over the lookback before a morning.
type pickRank struct {
	wallet          string
	sum, best, rank float64
	n               int
	holds           []float64
}

// calInput is everything buildCalendar needs, so it can be tested without a store.
type calInput struct {
	ps    []model.SimPosition
	outs  []score.Outcome // same order as ps
	their func(wallet, mint string) (float64, bool)
	loc   *time.Location
	now   time.Time
	pick  string // "best" | "all"
	n     int
	// positiveOnly picks only wallets whose copies made money (best trade aside);
	// on a day nobody qualifies, we copy nobody.
	positiveOnly bool
}

type calResult struct {
	Days    []calDay
	OpenUSD float64
	Open    int
	byDate  map[string]map[string]*calWallet
}

func dayOf(t time.Time, loc *time.Location) string { return t.In(loc).Format("2006-01-02") }

func midnight(day string, loc *time.Location) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", day, loc)
	return t
}

// picksFor ranks wallets by what copying them made over the lookback before the
// morning of day, using only copies that had closed by then. The single best
// trade is left out, like the score, so one lucky token can't carry a wallet.
func picksFor(day string, in calInput) []string {
	t0 := midnight(day, in.loc)
	by := map[string]*pickRank{}
	for i, p := range in.ps {
		o := in.outs[i]
		if !o.Closed || !o.At.Before(t0) || o.At.Before(t0.Add(-pickLookback)) {
			continue
		}
		r := by[p.Wallet]
		if r == nil {
			r = &pickRank{wallet: p.Wallet, best: o.PnlUSD}
			by[p.Wallet] = r
		}
		r.sum += o.PnlUSD
		r.n++
		r.holds = append(r.holds, o.At.Sub(p.OpenedAt).Seconds())
		if o.PnlUSD > r.best {
			r.best = o.PnlUSD
		}
	}
	var rs []pickRank
	for _, r := range by {
		sort.Float64s(r.holds)
		if r.n >= pickMinClosed && r.holds[len(r.holds)/2] >= pickMinHold.Seconds() && (r.sum-r.best > 0 || !in.positiveOnly) {
			r.rank = r.sum - r.best
			rs = append(rs, *r)
		}
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].rank != rs[j].rank {
			return rs[i].rank > rs[j].rank
		}
		return rs[i].wallet < rs[j].wallet
	})
	var out []string
	for _, r := range rs[:min(in.n, len(rs))] {
		out = append(out, r.wallet)
	}
	return out
}

func buildCalendar(in calInput) calResult {
	res := calResult{Days: []calDay{}, byDate: map[string]map[string]*calWallet{}}
	picks := map[string]map[string]bool{} // open day -> picked wallets
	pickList := map[string][]string{}
	taken := func(p model.SimPosition) bool {
		if in.pick != "best" {
			return true
		}
		d := dayOf(p.OpenedAt, in.loc)
		if picks[d] == nil {
			pickList[d] = picksFor(d, in)
			picks[d] = map[string]bool{}
			for _, w := range pickList[d] {
				picks[d][w] = true
			}
		}
		return picks[d][p.Wallet]
	}
	days := map[string]*calDay{}
	for i, p := range in.ps {
		if !taken(p) {
			continue
		}
		o := in.outs[i]
		if !o.Closed {
			res.OpenUSD += o.PnlUSD
			res.Open++
			continue
		}
		k := dayOf(o.At, in.loc)
		d := days[k]
		if d == nil {
			d = &calDay{Date: k}
			days[k] = d
			res.byDate[k] = map[string]*calWallet{}
		}
		cw := res.byDate[k][p.Wallet]
		if cw == nil {
			cw = &calWallet{Address: p.Wallet}
			res.byDate[k][p.Wallet] = cw
		}
		cost := p.CostSol * p.SolUSD
		c := calCopy{Mint: p.Mint, OpenedAt: p.OpenedAt, ClosedAt: p.ClosedAt, CostUSD: round2(cost), UsUSD: round2(o.PnlUSD)}
		d.UsUSD += o.PnlUSD
		cw.UsUSD += o.PnlUSD
		if pct, ok := in.their(p.Wallet, p.Mint); ok {
			them := pct * cost
			c.ThemUSD = &them
			d.ThemUSD += them
			cw.ThemUSD += them
		}
		d.Closed++
		cw.Closed++
		if o.PnlUSD > 0 {
			d.Won++
			cw.Won++
		}
		cw.Copies = append(cw.Copies, c)
	}
	for k, d := range days {
		d.UsUSD, d.ThemUSD = round2(d.UsUSD), round2(d.ThemUSD)
		if in.pick == "best" {
			d.Picks = pickList[k]
		}
		res.Days = append(res.Days, *d)
	}
	sort.Slice(res.Days, func(i, j int) bool { return res.Days[i].Date < res.Days[j].Date })
	res.OpenUSD = round2(res.OpenUSD)
	return res
}

// calendar: ?delay=2&pick=best|all&n=3&wallet=ADDR&date=YYYY-MM-DD (date adds that
// day's per-wallet, per-token breakdown and the morning's picks).
func (s *Server) calendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	delay := qInt(r, "delay", s.Cfg.RotationDelayS)
	wallet := q.Get("wallet")
	pick := "best"
	if q.Get("pick") == "all" || wallet != "" {
		pick = "all" // one wallet: all its copies
	}
	n := min(max(qInt(r, "n", 3), 1), 20)
	strict := q.Get("strict") != "0" // only wallets whose copies made money
	loc, err := time.LoadLocation(s.Cfg.AlertTZ)
	if err != nil {
		loc = time.UTC
	}
	now := s.Now()
	since := now.AddDate(0, 0, -120)
	ps, err := s.Store.ListSimPositions(ctx, store.SimFilter{Wallet: wallet, DelayS: delay, Since: since})
	if err != nil {
		fail(w, 500, err)
		return
	}
	fees := sim.FeesFrom(s.Cfg)
	toks := s.tokenCache(ctx)
	outs := make([]score.Outcome, len(ps))
	for i, p := range ps {
		outs[i] = score.Value(p, toks(p.Mint), fees, now)
	}
	// Their return per token, from their own trades (compare does the same for the wallet page).
	tts, err := s.Store.ListTargetTrades(ctx, store.TradeFilter{Wallet: wallet, Since: since.AddDate(0, 0, -1)})
	if err != nil {
		fail(w, 500, err)
		return
	}
	tradesBy := map[string][]model.Trade{}
	for _, t := range tts {
		tradesBy[t.Trader] = append(tradesBy[t.Trader], t)
	}
	posBy := map[string][]model.SimPosition{}
	for _, p := range ps {
		posBy[p.Wallet] = append(posBy[p.Wallet], p)
	}
	theirs := map[string]map[string]float64{}
	their := func(wl, mint string) (float64, bool) {
		m, ok := theirs[wl]
		if !ok {
			m = map[string]float64{}
			rows, _ := compare.Build(tradesBy[wl], posBy[wl], toks, fees, now)
			for _, row := range rows {
				m[row.Mint] = row.TheirPct
			}
			theirs[wl] = m
		}
		v, ok := m[mint]
		return v, ok
	}
	in := calInput{ps: ps, outs: outs, their: their, loc: loc, now: now, pick: pick, n: n, positiveOnly: strict}
	res := buildCalendar(in)

	out := map[string]any{"tz": loc.String(), "delay": delay, "pick": pick, "n": n, "today": dayOf(now, loc),
		"days": res.Days, "open_usd": res.OpenUSD, "open": res.Open,
		"strict": strict, "picks_today": picksFor(dayOf(now, loc), in)}
	if pick == "best" && strict {
		// What copying the top N anyway (least bad, even if losing) would have made.
		loose := in
		loose.positiveOnly = false
		alt := 0.0
		for _, d := range buildCalendar(loose).Days {
			alt += d.UsUSD
		}
		out["loose_us_usd"] = round2(alt)
	}
	if date := q.Get("date"); date != "" {
		var ws []calWallet
		for _, cw := range res.byDate[date] {
			cw.UsUSD, cw.ThemUSD = round2(cw.UsUSD), round2(cw.ThemUSD)
			sort.Slice(cw.Copies, func(i, j int) bool { return cw.Copies[i].UsUSD > cw.Copies[j].UsUSD })
			ws = append(ws, *cw)
		}
		sort.Slice(ws, func(i, j int) bool { return ws[i].UsUSD > ws[j].UsUSD })
		day := calDay{Date: date, Wallets: ws}
		if pick == "best" {
			day.Picks = picksFor(date, in)
		}
		out["day"] = day
	}
	if wallet == "" {
		// Wallet list for the picker: everyone with copies in the period, best first.
		tot := map[string]float64{}
		for i, p := range ps {
			tot[p.Wallet] += outs[i].PnlUSD
		}
		type wt struct {
			Address string  `json:"address"`
			PnlUSD  float64 `json:"pnl_usd"`
		}
		var list []wt
		for a, v := range tot {
			list = append(list, wt{a, round2(v)})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].PnlUSD > list[j].PnlUSD })
		out["wallets"] = list
	}
	writeJSON(w, 200, out)
}
