package alert

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"copytrade/internal/model"
)

// Alerts are one hourly message about the copies the executor made of our live
// wallets (paper or real money): P&L this hour, P&L today, failed trades, then a
// note per trade. The only instant messages are failed sells (we may be stuck
// holding a token) and safety stops, which mean trading has stopped.

// Decision says whether an event alerts immediately and how it is deduped.
type Decision struct {
	Now bool
	// Key dedupes repeats within Window (empty = never deduped).
	Key    string
	Window time.Duration
}

// Classify decides whether an event alerts on its own. Only failed sells, low
// SOL and safety stops do; trades and failed buys (a missed trade) go in the hourly
// message. Pure, so it is unit-tested.
func Classify(ev model.Event) Decision {
	if ev.Kind == "error" && ev.Service == "executor" && dataString(ev, "side") == model.SideSell && dataString(ev, "status") == "failed" {
		return Decision{Now: true, Key: "sellfail:" + dataString(ev, "mint"), Window: 15 * time.Minute}
	}
	if ev.Kind != "info" {
		return Decision{}
	}
	m := strings.ToLower(ev.Message)
	switch {
	case strings.Contains(m, "kill switch"):
		return Decision{Now: true, Key: "kill", Window: 24 * time.Hour}
	case strings.Contains(m, "daily loss limit hit"):
		// One per trading day: a 24h window could swallow the next day's stop.
		return Decision{Now: true, Key: "dayloss:" + dataString(ev, "day"), Window: 24 * time.Hour}
	case strings.Contains(m, "low sol balance"):
		return Decision{Now: true, Key: "lowsol", Window: 6 * time.Hour}
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

// CopyLine is one closed copy.
type CopyLine struct {
	Wallet   string  // label or short address
	Mint     string  // short mint
	PnlUSD   float64 // our result
	PnlPct   float64
	TheirPct float64 // NaN when unknown (they bought before we tracked them)
	HoldS    float64
	Migrated bool
}

// Failures counts the executor's trades that failed after all retries.
type Failures struct{ Buys, Sells int }

// SellFailed is the instant alert for a sell that failed after all retries.
func SellFailed(f model.LiveFill, wallet string) string {
	return fmt.Sprintf("[%s] SELL FAILED: %s · %s after %d tr%s: %s\n"+
		"Still holding it. The bot tries again on their next sell or at the stop loss; sell by hand in your wallet if it keeps failing.",
		strings.ToUpper(f.Mode), wallet, short(f.Mint), f.Attempts, map[bool]string{true: "y", false: "ies"}[f.Attempts == 1], f.Error)
}

// MaxMsg keeps messages under Discord's 2000-character limit with room to spare.
const MaxMsg = 1900

// Hourly is the alert: P&L this hour, P&L today, then one note per trade that
// closed this hour, newest last. mode is "paper" or "live". Returns "" when
// nothing closed or failed this hour.
func Hourly(mode string, hour, day []CopyLine, fail Failures) string {
	if len(hour) == 0 && fail == (Failures{}) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] P&L this hour: %s\n", strings.ToUpper(mode), totals(hour))
	fmt.Fprintf(&b, "P&L today: %s\n", totals(day))
	if fail != (Failures{}) {
		fmt.Fprintf(&b, "Failed this hour: %s\n", failText(fail))
	}
	if len(hour) > 0 {
		b.WriteString("\nTrade notes:\n")
	}
	for i, l := range hour {
		line := "• " + FormatCopy(l) + "\n"
		if b.Len()+len(line) > MaxMsg-40 {
			fmt.Fprintf(&b, "...and %d more\n", len(hour)-i)
			break
		}
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// failText is "2 buys (missed), 1 sell (still holding)".
func failText(f Failures) string {
	var parts []string
	if f.Buys > 0 {
		parts = append(parts, fmt.Sprintf("%d buy%s (missed)", f.Buys, map[bool]string{true: "", false: "s"}[f.Buys == 1]))
	}
	if f.Sells > 0 {
		parts = append(parts, fmt.Sprintf("%d sell%s (still holding)", f.Sells, map[bool]string{true: "", false: "s"}[f.Sells == 1]))
	}
	return strings.Join(parts, ", ")
}

// totals is "+$8.40 (3 trades, 2 won)".
func totals(ls []CopyLine) string {
	var net float64
	wins := 0
	for _, l := range ls {
		net += l.PnlUSD
		if l.PnlUSD > 0 {
			wins++
		}
	}
	return fmt.Sprintf("%s (%d trade%s, %d won)", USD(net), len(ls), map[bool]string{true: "", false: "s"}[len(ls) == 1], wins)
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
