package alert

import (
	"math"
	"strings"
	"testing"
	"time"

	"copytrade/internal/model"
	"copytrade/internal/rotation"
)

func TestClassify(t *testing.T) {
	now := []model.Event{
		{Kind: "info", Message: "[paper] buy X rejected: daily loss limit hit (-80.00 USD)"},
		{Kind: "info", Message: "kill switch on"},
	}
	for _, ev := range now {
		if !Classify(ev).Now {
			t.Errorf("should alert now: %+v", ev)
		}
	}
	// Trades go in the hourly message; everything else stays on the dashboard.
	quiet := []model.Event{
		{Kind: "exec_closed"}, {Kind: "sim_fill"}, {Kind: "copy_closed"}, {Kind: "discovery"},
		{Kind: "live_fill", Data: map[string]any{"mode": "live"}},
		{Kind: "error", Service: "ingest", Message: "daily message budget hit (1000/1000)"},
		{Kind: "rotation", Data: rotation.Change{Wallet: "W", From: "bench", To: "live"}},
		{Kind: "info", Message: "80% of today's message budget used (800/1000)"},
		{Kind: "info", Message: "[paper] buy X rejected: max open positions"},
	}
	for _, ev := range quiet {
		if Classify(ev).Now {
			t.Errorf("should not alert now: %+v", ev)
		}
	}
}

func TestLimiterDedupesAndCaps(t *testing.T) {
	l := &Limiter{MaxPerHour: 3}
	t0 := time.Unix(1700000000, 0)
	e1 := Classify(model.Event{Kind: "info", Message: "[paper] buy X rejected: daily loss limit hit (-80.00 USD)"})
	e2 := Classify(model.Event{Kind: "info", Message: "[paper] buy Y rejected: daily loss limit hit (-91.00 USD)"})
	if !l.Allow(e1, t0) || l.Allow(e2, t0.Add(time.Minute)) {
		t.Fatal("the daily loss limit should alert once a day")
	}
	if !l.Allow(e2, t0.Add(24*time.Hour+time.Second)) {
		t.Fatal("dedupe window should expire after 24h")
	}
	now := Decision{Now: true}
	t1 := t0.Add(30 * time.Hour)
	for i := 0; i < 3; i++ {
		if !l.Allow(now, t1) {
			t.Fatalf("alert %d should pass", i)
		}
	}
	if l.Allow(now, t1) || l.Suppressed != 1 {
		t.Fatalf("4th in an hour should be held back, suppressed=%d", l.Suppressed)
	}
	if !l.Allow(now, t1.Add(61*time.Minute)) {
		t.Fatal("cap should reset after an hour")
	}
}

func TestHourly(t *testing.T) {
	nan := math.NaN()
	hour := []CopyLine{
		{Wallet: "nad", Mint: "3FVR..pump", PnlUSD: -1.88, PnlPct: -0.075, TheirPct: 0.012, HoldS: 1035},
		{Wallet: "Dior", Mint: "MsGD..pump", PnlUSD: 9.10, PnlPct: 0.364, TheirPct: nan, HoldS: 30},
	}
	day := append([]CopyLine{{Wallet: "Zyaf", PnlUSD: -5.37}}, hour...)
	s := Hourly("paper", hour, day, Failures{})
	want := "[PAPER] P&L this hour: +$7.22 (2 trades, 1 won)\n" +
		"P&L today: +$1.85 (3 trades, 1 won)\n\n" +
		"Trade notes:\n" +
		"• nad · 3FVR..pump: -$1.88 (-8%) · they +1% · held 17m\n" +
		"• Dior · MsGD..pump: +$9.10 (+36%) · held 30s"
	if s != want {
		t.Fatalf("got:\n%s\nwant:\n%s", s, want)
	}
	if Hourly("paper", nil, day, Failures{}) != "" {
		t.Fatal("a quiet hour should send nothing")
	}
	var many []CopyLine
	for i := 0; i < 500; i++ {
		many = append(many, CopyLine{Wallet: "w", Mint: "m", PnlUSD: 1, TheirPct: nan})
	}
	if s := Hourly("live", many, many, Failures{}); len(s) > MaxMsg || !strings.Contains(s, "more") {
		t.Fatalf("long hour: %d chars", len(s))
	}
}

// A failed sell alerts at once (deduped per token); a failed buy waits for the
// hourly message, which also goes out when only failures happened.
func TestFailures(t *testing.T) {
	ev := func(side string) model.Event {
		return model.Event{Kind: "error", Service: "executor", Message: "[live] " + side + " MINT failed",
			Data: model.LiveFill{Mode: "live", Side: side, Mint: "MINTaaaaaaaaaaaaaaapump", Status: "failed"}}
	}
	if d := Classify(ev("sell")); !d.Now || d.Key != "sellfail:MINTaaaaaaaaaaaaaaapump" {
		t.Fatalf("failed sell should alert now: %+v", d)
	}
	if Classify(ev("buy")).Now {
		t.Fatal("failed buy should wait for the hourly message")
	}
	s := SellFailed(model.LiveFill{Mode: "live", Mint: "MINTaaaaaaaaaaaaaaapump", Attempts: 3, Error: "slippage exceeded"}, "nad")
	if !strings.HasPrefix(s, "[LIVE] SELL FAILED: nad · MINT..pump after 3 tries: slippage exceeded") {
		t.Fatalf("sell failed text: %q", s)
	}
	h := Hourly("live", nil, nil, Failures{Buys: 2, Sells: 1})
	want := "[LIVE] P&L this hour: +$0.00 (0 trades, 0 won)\nP&L today: +$0.00 (0 trades, 0 won)\nFailed this hour: 2 buys (missed), 1 sell (still holding)"
	if h != want {
		t.Fatalf("got:\n%s\nwant:\n%s", h, want)
	}
}
