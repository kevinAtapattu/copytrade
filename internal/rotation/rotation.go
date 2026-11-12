// Package rotation applies the live/bench rules from CLAUDE.md.
package rotation

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"copytrade/internal/config"
	"copytrade/internal/model"
	"copytrade/internal/store"
)

type Rules struct {
	DelayS           int     // delay the rules judge on (default +2s)
	LiveMax          int     // live slots
	BenchMax         int     // bench cap
	MaxDrawdown      float64 // demote at or beyond this (0.25)
	PromoteMinDays   float64 // must have 14+ days of sim history
	PromoteMinTrades int     // in the 14d window
	DropInactiveDays float64
	DropBadPnlUSD    float64 // 30d sim PnL at or below this drops a bench wallet...
	DropBadMinTrades int     // ...once it has this many positions
	// ManualHold keeps a wallet set live by hand live this long, unless its
	// drawdown hits MaxDrawdown (Dior was demoted 3.5h after "Make live").
	ManualHold time.Duration
	// Cooldown stops a demoted wallet being promoted again this soon (Swan's score
	// hovered at 0 and it flipped live/bench 9 times in a day).
	Cooldown time.Duration
}

// Change is one decided status change.
type Change struct {
	Wallet string
	From   string
	To     string
	Reason string
}

// ScoreIndex looks up the latest score for wallet/delay/window.
type ScoreIndex map[string]model.Score

func key(w string, d, win int) string { return fmt.Sprintf("%s|%d|%d", w, d, win) }

func Index(ss []model.Score) ScoreIndex {
	ix := ScoreIndex{}
	for _, s := range ss {
		ix[key(s.Wallet, s.DelayS, s.WindowDays)] = s
	}
	return ix
}

func (ix ScoreIndex) Get(w string, d, win int) (model.Score, bool) {
	s, ok := ix[key(w, d, win)]
	return s, ok
}

// DemoteReason returns why a live wallet should go back to the bench, or "".
func (r Rules) DemoteReason(s14 model.Score) string {
	switch {
	case s14.Positions == 0:
		return ""
	case s14.PnlUSD < 0:
		return fmt.Sprintf("14d sim PnL %+.2f USD at +%ds", s14.PnlUSD, r.DelayS)
	case s14.Score < 0:
		return fmt.Sprintf("14d score %.2f negative at +%ds", s14.Score, r.DelayS)
	case s14.MaxDrawdown >= r.MaxDrawdown:
		return fmt.Sprintf("drawdown %.0f%% >= %.0f%%", s14.MaxDrawdown*100, r.MaxDrawdown*100)
	}
	return ""
}

// PromoteBlock returns why a bench wallet may NOT be promoted, or "" if eligible.
func (r Rules) PromoteBlock(s14, s30 model.Score) string {
	switch {
	case s30.ActiveDays < r.PromoteMinDays:
		return fmt.Sprintf("only %.1f days of history", s30.ActiveDays)
	case s14.Positions < r.PromoteMinTrades:
		return fmt.Sprintf("only %d trades in 14d", s14.Positions)
	case s14.PnlUSD <= 0:
		return "14d PnL not positive"
	case s30.PnlUSD <= 0:
		return "30d PnL not positive"
	case s14.LuckyTrade || s14.PnlExBestUSD <= 0:
		return "PnL driven by one trade"
	case s14.Score <= 0:
		return "score not positive"
	case s14.MaxDrawdown >= r.MaxDrawdown:
		return "drawdown too deep"
	}
	return ""
}

// Decide computes promotions and demotions. Demotions first, then fill free
// live slots with the best eligible bench wallets.
func (r Rules) Decide(wallets []model.Wallet, ix ScoreIndex, now time.Time) []Change {
	var changes []Change
	live := 0
	type cand struct {
		w     model.Wallet
		score float64
		s14   model.Score
	}
	var cands []cand
	for _, w := range wallets {
		s14, _ := ix.Get(w.Address, r.DelayS, 14)
		s30, _ := ix.Get(w.Address, r.DelayS, 30)
		switch w.Status {
		case model.StatusLive:
			why := r.DemoteReason(s14)
			if held := strings.HasPrefix(w.Reason, "manual:") && now.Sub(w.StatusAt) < r.ManualHold; held && s14.MaxDrawdown < r.MaxDrawdown {
				why = ""
			}
			if why != "" {
				changes = append(changes, Change{w.Address, model.StatusLive, model.StatusBench, "demoted: " + why})
			} else {
				live++
			}
		case model.StatusBench:
			if strings.HasPrefix(w.Reason, "demoted:") && now.Sub(w.StatusAt) < r.Cooldown {
				continue
			}
			if r.PromoteBlock(s14, s30) == "" {
				cands = append(cands, cand{w, s14.Score, s14})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	for _, c := range cands {
		if live >= r.LiveMax {
			break
		}
		live++
		changes = append(changes, Change{c.w.Address, model.StatusBench, model.StatusLive,
			fmt.Sprintf("promoted: 14d %+.2f USD (%+.2f ex-best), score %.2f, %d trades at +%ds",
				c.s14.PnlUSD, c.s14.PnlExBestUSD, c.s14.Score, c.s14.Positions, r.DelayS)})
	}
	return changes
}

// Prune decides which bench wallets to drop: inactive, clearly bad, or over the bench cap.
func (r Rules) Prune(wallets []model.Wallet, ix ScoreIndex, now time.Time) []Change {
	var changes []Change
	type kept struct {
		w     model.Wallet
		score float64
	}
	var bench []kept
	for _, w := range wallets {
		if w.Status != model.StatusBench {
			continue
		}
		last := w.AddedAt
		if w.StatusAt.After(last) {
			last = w.StatusAt // re-added to the bench: a fresh week to trade
		}
		if w.LastTradeAt != nil && w.LastTradeAt.After(last) {
			last = *w.LastTradeAt
		}
		if idle := now.Sub(last).Hours() / 24; idle >= r.DropInactiveDays {
			changes = append(changes, Change{w.Address, model.StatusBench, model.StatusDropped,
				fmt.Sprintf("dropped: inactive %.0f days", idle)})
			continue
		}
		s30, _ := ix.Get(w.Address, r.DelayS, 30)
		if s30.Positions >= r.DropBadMinTrades && s30.PnlUSD <= r.DropBadPnlUSD {
			changes = append(changes, Change{w.Address, model.StatusBench, model.StatusDropped,
				fmt.Sprintf("dropped: 30d sim PnL %+.2f USD over %d trades", s30.PnlUSD, s30.Positions)})
			continue
		}
		s14, _ := ix.Get(w.Address, r.DelayS, 14)
		bench = append(bench, kept{w, s14.Score})
	}
	if r.BenchMax > 0 && len(bench) > r.BenchMax {
		sort.Slice(bench, func(i, j int) bool { return bench[i].score > bench[j].score })
		for _, k := range bench[r.BenchMax:] {
			changes = append(changes, Change{k.w.Address, model.StatusBench, model.StatusDropped,
				fmt.Sprintf("dropped: bench over cap %d, score %.2f", r.BenchMax, k.score)})
		}
	}
	return changes
}

// Apply writes changes (each logged in rotations). Returns those that took effect.
func Apply(ctx context.Context, st store.Store, cs []Change, now time.Time) ([]Change, error) {
	var done []Change
	for _, c := range cs {
		ok, err := st.SetWalletStatus(ctx, c.Wallet, c.To, c.Reason, now)
		if err != nil {
			return done, err
		}
		if ok {
			done = append(done, c)
		}
	}
	return done, nil
}

// FromConfig builds rules from config.
func FromConfig(c config.Config) Rules {
	return Rules{
		DelayS: c.RotationDelayS, LiveMax: c.LiveMax, BenchMax: c.BenchMax, MaxDrawdown: c.MaxDrawdown,
		PromoteMinDays: c.PromoteMinDays, PromoteMinTrades: c.PromoteMinTrades,
		DropInactiveDays: c.DropInactiveDays, DropBadPnlUSD: c.DropBadPnlUSD, DropBadMinTrades: c.DropBadMinTrades,
		ManualHold: 24 * time.Hour, Cooldown: 24 * time.Hour,
	}
}
