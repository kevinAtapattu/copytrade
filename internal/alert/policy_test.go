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
		{Kind: "exec_closed"},
		{Kind: "live_fill", Data: map[string]any{"mode": "live"}},
		{Kind: "error", Service: "ingest", Message: "daily message budget hit (1000/1000)"},
		{Kind: "rotation", Data: rotation.Change{Wallet: "W", From: "bench", To: "live"}},
		{Kind: "rotation", Data: map[string]any{"From": "live", "To": "bench"}},
		{Kind: "info", Message: "[paper] buy X rejected: daily loss limit hit (-80.00 USD)"},
		{Kind: "info", Message: "80% of today's message budget used (800/1000)"},
	}
	for _, ev := range now {
		if !Classify(ev).Now {
			t.Errorf("should alert now: %+v", ev)
		}
	}
	quiet := []model.Event{
		{Kind: "sim_fill"}, {Kind: "copy_closed"}, {Kind: "discovery"},
		{Kind: "live_fill", Data: map[string]any{"mode": "paper"}},
		{Kind: "rotation", Message: "W -> dropped: spam guard"}, // ingest spam drop: nil Data
		{Kind: "rotation", Data: rotation.Change{From: "bench", To: "dropped"}},
		{Kind: "info", Message: "[paper] buy X rejected: max open positions"},
		{Kind: "info", Message: "new UTC day: message budget reset, subscriptions resumed"},
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
	e1 := Classify(model.Event{Kind: "error", Service: "discovery", Message: "rpc failed after 3 tries (429)"})
	e2 := Classify(model.Event{Kind: "error", Service: "discovery", Message: "rpc failed after 4 tries (429)"})
	if !l.Allow(e1, t0) || l.Allow(e2, t0.Add(time.Minute)) {
		t.Fatal("same error with different numbers should be deduped")
	}
	if !l.Allow(e2, t0.Add(3*time.Hour+time.Second)) {
		t.Fatal("dedupe window should expire after 3h")
	}
	now := Decision{Now: true}
	t1 := t0.Add(5 * time.Hour)
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

func TestRollupSummary(t *testing.T) {
	hour := []CopyLine{
		{Wallet: "Johnson", PnlUSD: 9.10, PnlPct: 0.36},
		{Wallet: "dash", PnlUSD: -6.20, PnlPct: -0.25},
		{Wallet: "bust", PnlUSD: 5.50, PnlPct: 0.22},
	}
	day := append([]CopyLine{{Wallet: "dash", PnlUSD: -3.60}, {Wallet: "Johnson", PnlUSD: 6.10}}, hour...)
	s := Rollup("Paper copies (+2s), last hour", hour, day, 0)
	want := "Paper copies (+2s), last hour: +$8.40 net · 3 closed · 2 won (67%)\n" +
		"Best: Johnson +$9.10 (+36%) · Worst: dash -$6.20 (-25%)\n" +
		"Today so far: +$10.90 net · 5 closed · 3 won (60%)\n" +
		"Top wallet today: Johnson +$15.20 · Bottom: dash -$9.80"
	if s != want {
		t.Fatalf("got:\n%s\nwant:\n%s", s, want)
	}
	if Rollup("x", nil, day, 0) != "" {
		t.Fatal("nothing closed this period should send nothing")
	}
	if s := Rollup("x", nil, nil, 3); !strings.Contains(s, "3 other alerts held back") {
		t.Fatalf("held-back count missing: %q", s)
	}
	// Many copies still produce a short message.
	var many []CopyLine
	for i := 0; i < 500; i++ {
		many = append(many, CopyLine{Wallet: "w", PnlUSD: 1, TheirPct: math.NaN()})
	}
	if s := Rollup("x", many, many, 0); len(s) > 400 {
		t.Fatalf("summary too long: %d chars", len(s))
	}
}
