package alert

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"copytrade/internal/model"
)

// Alerts come in three tiers so 20+ tracked wallets never turn into a ping per trade:
//   - Now: rare events that need a decision (executor closes, real-money fills,
//     live promotions/demotions, spending and safety stops, errors).
//   - Roll-up: every closed paper copy, batched into one message per interval
//     (built from the database by the alerter, not from events).
//   - Digest: one daily summary (also built from the database).
// Everything else (sim fills, target trades, bench drops, discovery adds, paper
// buys, rejections) only lands in the roll-up/digest or the dashboard.

// Decision says whether an event alerts immediately and how it is deduped.
type Decision struct {
	Now bool
	// Key dedupes repeats within Window (empty = never deduped).
	Key    string
	Window time.Duration
}

var digits = regexp.MustCompile(`[0-9.]+`)

// Classify decides an event's tier. Pure, so it is unit-tested.
func Classify(ev model.Event) Decision {
	switch ev.Kind {
	case "exec_closed":
		return Decision{Now: true}
	case "live_fill":
		// Paper executor buys are noise; real-money fills always alert.
		if dataString(ev, "mode") == "live" {
			return Decision{Now: true}
		}
	case "error":
		// Same error from the same service at most every 3h (digits stripped so
		// counters and amounts don't make each repeat look new).
		return Decision{Now: true, Key: "err|" + ev.Service + "|" + digits.ReplaceAllString(ev.Message, "#"), Window: 3 * time.Hour}
	case "rotation":
		// Promotions to / demotions from live matter; bench drops go to the digest.
		// Ingest's spam-guard drop has no Data, so it falls through to the digest.
		if dataString(ev, "From") == model.StatusLive || dataString(ev, "To") == model.StatusLive {
			return Decision{Now: true}
		}
	case "info":
		m := strings.ToLower(ev.Message)
		switch {
		case strings.Contains(m, "kill switch"):
			return Decision{Now: true, Key: "kill", Window: 24 * time.Hour}
		case strings.Contains(m, "daily loss limit"):
			return Decision{Now: true, Key: "dayloss", Window: 24 * time.Hour}
		case strings.Contains(m, "message budget used"):
			return Decision{Now: true, Key: "budget80", Window: 24 * time.Hour}
		}
	}
	return Decision{}
}

func dataString(ev model.Event, k string) string {
	m, ok := ev.Data.(map[string]any)
	if !ok {
		b, err := json.Marshal(ev.Data)
		if err != nil || json.Unmarshal(b, &m) != nil {
			return ""
		}
	}
	s, _ := m[k].(string)
	return s
}

// Limiter applies Decision dedupe plus an hourly cap on immediate alerts.
type Limiter struct {
	MaxPerHour int
	seen       map[string]time.Time
	sent       []time.Time
	Suppressed int // dropped by the cap since last reset (reported in the next roll-up)
}

// Allow reports whether an immediate alert may go out now.
func (l *Limiter) Allow(d Decision, now time.Time) bool {
	if l.seen == nil {
		l.seen = map[string]time.Time{}
	}
	if d.Key != "" {
		if t, ok := l.seen[d.Key]; ok && now.Sub(t) < d.Window {
			return false
		}
	}
	cut := now.Add(-time.Hour)
	i := 0
	for i < len(l.sent) && l.sent[i].Before(cut) {
		i++
	}
	l.sent = l.sent[i:]
	if l.MaxPerHour > 0 && len(l.sent) >= l.MaxPerHour {
		l.Suppressed++
		return false
	}
	if d.Key != "" {
		l.seen[d.Key] = now
	}
	l.sent = append(l.sent, now)
	return true
}

// CopyLine is one closed copy in a roll-up.
type CopyLine struct {
	Wallet   string  // label or short address
	Mint     string  // short mint
	PnlUSD   float64 // our result
	PnlPct   float64
	TheirPct float64 // NaN when unknown (they bought before we tracked them)
	HoldS    float64
	Migrated bool
}

// MaxMsg keeps messages under Discord's 2000-character limit with room to spare.
const MaxMsg = 1900

// Rollup is the periodic high-level summary: net result, win rate, and the best
// and worst copy for the period, plus today's running totals and the best and
// worst wallet so far. lines are the period's closed copies, day is today's.
// Returns "" when nothing closed in the period (and nothing was held back).
func Rollup(label string, lines, day []CopyLine, suppressed int) string {
	if len(lines) == 0 && suppressed == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", label, totals(lines))
	if len(lines) > 0 {
		best, worst := lines[0], lines[0]
		for _, l := range lines {
			if l.PnlUSD > best.PnlUSD {
				best = l
			}
			if l.PnlUSD < worst.PnlUSD {
				worst = l
			}
		}
		fmt.Fprintf(&b, "Best: %s %s (%s)", best.Wallet, USD(best.PnlUSD), Pct(best.PnlPct))
		if len(lines) > 1 {
			fmt.Fprintf(&b, " · Worst: %s %s (%s)", worst.Wallet, USD(worst.PnlUSD), Pct(worst.PnlPct))
		}
		b.WriteString("\n")
	}
	if len(day) > 0 {
		fmt.Fprintf(&b, "Today so far: %s\n", totals(day))
		by := map[string]float64{}
		for _, l := range day {
			by[l.Wallet] += l.PnlUSD
		}
		var names []string
		for w := range by {
			names = append(names, w)
		}
		sort.Slice(names, func(i, j int) bool {
			return by[names[i]] > by[names[j]] || (by[names[i]] == by[names[j]] && names[i] < names[j])
		})
		fmt.Fprintf(&b, "Top wallet today: %s %s", names[0], USD(by[names[0]]))
		if len(names) > 1 {
			last := names[len(names)-1]
			fmt.Fprintf(&b, " · Bottom: %s %s", last, USD(by[last]))
		}
		b.WriteString("\n")
	}
	if suppressed > 0 {
		fmt.Fprintf(&b, "(%d other alerts held back by the hourly cap)\n", suppressed)
	}
	return strings.TrimRight(b.String(), "\n")
}

// totals is "+$8.40 net · 12 closed · 7 won (58%)".
func totals(ls []CopyLine) string {
	var net float64
	wins := 0
	for _, l := range ls {
		net += l.PnlUSD
		if l.PnlUSD > 0 {
			wins++
		}
	}
	if len(ls) == 0 {
		return "nothing closed"
	}
	return fmt.Sprintf("%s net · %d closed · %d won (%.0f%%)", USD(net), len(ls), wins, float64(wins)/float64(len(ls))*100)
}

// FormatCopy renders one closed copy: who, what, our result vs theirs, hold.
func FormatCopy(l CopyLine) string {
	s := fmt.Sprintf("%s · %s: %s (%s)", l.Wallet, l.Mint, USD(l.PnlUSD), Pct(l.PnlPct))
	if !math.IsNaN(l.TheirPct) {
		s += " · they " + Pct(l.TheirPct)
	}
	s += " · held " + Dur(l.HoldS)
	if l.Migrated {
		s += " · migrated"
	}
	return s
}

func USD(v float64) string {
	sign := "+"
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s$%.2f", sign, v)
}

func Pct(f float64) string {
	return fmt.Sprintf("%+.0f%%", f*100)
}

func Dur(s float64) string {
	switch {
	case s < 90:
		return fmt.Sprintf("%.0fs", s)
	case s < 90*60:
		return fmt.Sprintf("%.0fm", s/60)
	default:
		return fmt.Sprintf("%.1fh", s/3600)
	}
}
