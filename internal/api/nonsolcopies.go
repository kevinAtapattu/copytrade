package api

import (
	"math"
	"net/http"
	"sort"

	"copytrade/internal/model"
)

// Our paper copies on other coins (cmd/nonsolsim): Jupiter quotes 3s after
// their buy, a fixed $10 each. Read-only, kept out of every other total.

type copyRow struct {
	model.NonsolCopy
	PnlSol float64 `json:"pnl_sol"` // closed: final; open: marked at the last sell quote
	PnlUSD float64 `json:"pnl_usd"`
	Pct    float64 `json:"pct"`
}

type copySummary struct {
	Wallet          string         `json:"wallet,omitempty"`
	Copies          int            `json:"copies"` // closed
	Won             int            `json:"won"`
	WinRate         *float64       `json:"win_rate,omitempty"`
	TotalUSD        float64        `json:"total_usd"`
	BestUSD         *float64       `json:"best_usd,omitempty"`
	WithoutBestUSD  *float64       `json:"without_best_usd,omitempty"`
	Open            int            `json:"open"`
	OpenUSD         float64        `json:"open_usd"` // open copies at their last mark
	Skipped         int            `json:"skipped"`
	SkippedByReason map[string]int `json:"skipped_by_reason"`
}

func toCopyRow(c model.NonsolCopy) copyRow {
	r := copyRow{NonsolCopy: c}
	cost := c.SolIn + c.FeesSol
	switch c.Status {
	case model.NonsolClosed:
		r.PnlSol = c.SolOut - cost
	case model.NonsolOpen:
		if c.MarkedAt == nil {
			return r // no mark yet
		}
		r.PnlSol = c.SolOut + c.MarkSol - cost
	default:
		return r
	}
	r.PnlUSD = round2(r.PnlSol * c.SolUSD)
	if c.SolIn > 0 {
		r.Pct = round2(r.PnlSol / c.SolIn * 100)
	}
	return r
}

func summarizeCopies(rows []copyRow) copySummary {
	s := copySummary{SkippedByReason: map[string]int{}}
	best := math.Inf(-1)
	for _, r := range rows {
		switch r.Status {
		case model.NonsolClosed:
			s.Copies++
			s.TotalUSD += r.PnlUSD
			best = math.Max(best, r.PnlUSD)
			if r.PnlSol > 0 {
				s.Won++
			}
		case model.NonsolOpen:
			s.Open++
			s.OpenUSD += r.PnlUSD
		case model.NonsolSkipped:
			s.Skipped++
			s.SkippedByReason[r.SkipReason]++
		}
	}
	s.TotalUSD, s.OpenUSD = round2(s.TotalUSD), round2(s.OpenUSD)
	if s.Copies > 0 {
		wr, b, wb := round2(float64(s.Won)/float64(s.Copies)*100), round2(best), round2(s.TotalUSD-best)
		s.WinRate, s.BestUSD, s.WithoutBestUSD = &wr, &b, &wb
	}
	return s
}

// nonsolCopies: GET /api/other-coins/copies. Summary for the live wallets and
// NONSOL_WALLETS (or ?wallet=), one per wallet, and every copy row.
func (s *Server) nonsolCopies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.NonsolCopies == nil {
		writeJSON(w, 200, map[string]any{"wallets": []string{}, "summary": summarizeCopies(nil), "by_wallet": []copySummary{},
			"copies": []copyRow{}})
		return
	}
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
		// The wallets nonsolsim always copies (default nad), live or not.
		for _, wl := range s.Cfg.NonsolWallets {
			if !contains(wallets, wl) {
				wallets = append(wallets, wl)
			}
		}
	}
	cs, err := s.NonsolCopies(ctx, 5000)
	if err != nil {
		fail(w, 500, err)
		return
	}
	want := map[string]bool{}
	for _, wl := range wallets {
		want[wl] = true
	}
	rows := []copyRow{}
	var mine []copyRow
	per := map[string][]copyRow{}
	for _, c := range cs {
		row := toCopyRow(c)
		rows = append(rows, row)
		per[c.Wallet] = append(per[c.Wallet], row)
		if want[c.Wallet] {
			mine = append(mine, row)
		}
	}
	byWallet := []copySummary{}
	for wl, rs := range per {
		sm := summarizeCopies(rs)
		sm.Wallet = wl
		byWallet = append(byWallet, sm)
	}
	sort.Slice(byWallet, func(i, j int) bool { return byWallet[i].Wallet < byWallet[j].Wallet })
	if wallets == nil {
		wallets = []string{}
	}
	writeJSON(w, 200, map[string]any{"wallets": wallets, "summary": summarizeCopies(mine), "by_wallet": byWallet,
		"copies": rows, "size_usd": 10, "delay_s": 3})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
