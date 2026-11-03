package api

import (
	"testing"
	"time"

	"copytrade/internal/model"
	"copytrade/internal/score"
)

// A wallet is copied on a day only if it ranked top N on results known that
// morning, and a copy counts on the day it closed, with their result scaled to ours.
func TestCalendarCopiesOnlyThatMorningsPicks(t *testing.T) {
	loc := time.UTC
	day := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, loc) }
	var ps []model.SimPosition
	var outs []score.Outcome
	add := func(w, mint string, open, close time.Time, pnl float64) {
		c := close
		ps = append(ps, model.SimPosition{Wallet: w, Mint: mint, OpenedAt: open, ClosedAt: &c, CostSol: 0.25, SolUSD: 100})
		outs = append(outs, score.Outcome{Closed: true, At: close, PnlUSD: pnl})
	}
	// Before Nov 10: "good" made +$5 on each of 6 copies, "bad" lost $5 on each of 6,
	// "few" won big but on only 2 copies (not enough to be picked).
	for i := 0; i < 6; i++ {
		add("good", "g", day(2+i, 10), day(2+i, 11), 5)
		add("bad", "b", day(2+i, 10), day(2+i, 11), -5)
	}
	add("few", "f", day(3, 10), day(3, 11), 50)
	add("few", "f2", day(4, 10), day(4, 11), 50)
	// Nov 10: all three trade; only "good" was picked that morning (n=1).
	add("good", "g10", day(10, 9), day(11, 9), 20) // closes the next day
	add("bad", "b10", day(10, 9), day(10, 10), 100)
	add("few", "f10", day(10, 9), day(10, 10), 100)

	their := func(w, mint string) (float64, bool) { return 1.0, mint == "g10" } // they doubled on g10
	res := buildCalendar(calInput{ps: ps, outs: outs, their: their, loc: loc, now: day(12, 0), pick: "best", n: 1})

	var nov11 *calDay
	for i := range res.Days {
		switch res.Days[i].Date {
		case "2026-11-10":
			t.Fatalf("copies of unpicked wallets counted: %+v", res.Days[i])
		case "2026-11-11":
			nov11 = &res.Days[i]
		}
	}
	if nov11 == nil || nov11.UsUSD != 20 || nov11.ThemUSD != 25 || nov11.Closed != 1 || nov11.Won != 1 {
		t.Fatalf("nov 11 %+v", nov11)
	}
	if got := picksFor("2026-11-10", calInput{ps: ps, outs: outs, loc: loc, n: 3}); len(got) != 2 || got[0] != "good" || got[1] != "bad" {
		t.Fatalf("picks %v", got)
	}
	if all := buildCalendar(calInput{ps: ps, outs: outs, their: their, loc: loc, pick: "all", n: 1}); len(all.Days) < 3 {
		t.Fatalf("all mode days %d", len(all.Days))
	}
}

// Strict picks skip wallets whose copies lost money, and snipers (copies held
// under a minute) are never picked.
func TestPicksSkipLosersAndSnipers(t *testing.T) {
	loc := time.UTC
	at := func(d, h, m, s int) time.Time { return time.Date(2026, 11, d, h, m, s, 0, loc) }
	var ps []model.SimPosition
	var outs []score.Outcome
	add := func(w string, open, close time.Time, pnl float64) {
		c := close
		ps = append(ps, model.SimPosition{Wallet: w, OpenedAt: open, ClosedAt: &c})
		outs = append(outs, score.Outcome{Closed: true, At: close, PnlUSD: pnl})
	}
	for i := 0; i < 6; i++ {
		add("steady", at(2+i, 10, 0, 0), at(2+i, 10, 30, 0), 5) // +$5, held 30 min
		add("sniper", at(2+i, 10, 0, 0), at(2+i, 10, 0, 8), 50) // +$50, held 8s
		add("loser", at(2+i, 10, 0, 0), at(2+i, 11, 0, 0), -2)  // -$2, held 1h
	}
	in := calInput{ps: ps, outs: outs, loc: loc, n: 3, positiveOnly: true}
	if got := picksFor("2026-11-10", in); len(got) != 1 || got[0] != "steady" {
		t.Fatalf("strict picks %v", got)
	}
	in.positiveOnly = false
	if got := picksFor("2026-11-10", in); len(got) != 2 || got[0] != "steady" || got[1] != "loser" {
		t.Fatalf("least-bad picks %v", got)
	}
}
