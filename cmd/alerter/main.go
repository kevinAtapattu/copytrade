// Command alerter sends Discord/Telegram alerts in three tiers so a large wallet
// list never means a ping per trade (see internal/alert/policy.go):
//   - immediate: executor closes, real-money fills, live promotions/demotions,
//     spending and safety stops, errors (deduped, capped per hour)
//   - roll-up: every closed paper copy, batched once per ALERT_ROLLUP_EVERY
//   - digest: a daily summary at ALERT_DIGEST_HOUR in ALERT_TZ
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // ALERT_TZ must work in a slim container

	"copytrade/internal/alert"
	"copytrade/internal/app"
	"copytrade/internal/bus"
	"copytrade/internal/model"
)

const (
	keyRollupLast = "alert_rollup_last" // RFC3339 end of the last roll-up window
	keyDigestLast = "alert_digest_last" // local date of the last digest
)

func main() {
	a := app.Start("alerter")
	defer a.Close()
	c := a.Cfg
	var targets []alert.Notifier
	var names []string
	for _, n := range []alert.Notifier{
		alert.NewDiscord(c.DiscordWebhookURL),
		alert.NewTelegram(c.TelegramBotToken, c.TelegramChatID),
	} {
		if n.Enabled() {
			targets = append(targets, n)
			names = append(names, n.Name())
		}
	}
	if len(targets) == 0 {
		slog.Warn("no DISCORD_WEBHOOK_URL or TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID set; alerts are logged only")
	}
	loc, err := time.LoadLocation(c.AlertTZ)
	if err != nil {
		slog.Warn("bad ALERT_TZ, using UTC", "tz", c.AlertTZ, "err", err)
		loc = time.UTC
	}
	var sent, dropped, held atomic.Int64
	a.Heartbeat(30*time.Second, func() any {
		return map[string]any{"targets": names, "sent": sent.Load(), "dropped": dropped.Load(), "held_back": held.Load()}
	})

	// Queue + 1 msg/sec pacing keeps us under Discord/Telegram rate limits.
	q := make(chan string, 100)
	send := func(text string) {
		slog.Info("alert", "text", text)
		if len(targets) == 0 || text == "" {
			return
		}
		select {
		case q <- text:
		default:
			dropped.Add(1)
		}
	}
	go func() {
		for {
			select {
			case <-a.Ctx.Done():
				return
			case text := <-q:
				for _, n := range targets {
					if err := n.Send(a.Ctx, text); err != nil {
						slog.Warn("alert send", "to", n.Name(), "err", err)
					} else {
						sent.Add(1)
					}
				}
				time.Sleep(time.Second)
			}
		}
	}()

	policy := fmt.Sprintf("copytrade alerts on. Instant: executor closes, real-money trades, live promotions/demotions, spending and safety stops, errors (max %d/hour). "+
		"Paper copies at +%ds: one roll-up every %s. Daily summary at %d:00 %s.", c.AlertMaxPerHour, c.AlertSimDelay, c.AlertRollupEvery, c.AlertDigestHour, c.AlertTZ)
	if c.AlertRollupEvery <= 0 {
		policy += " (roll-ups off)"
	}
	send(policy)

	var mu sync.Mutex
	lim := &alert.Limiter{MaxPerHour: c.AlertMaxPerHour}

	if c.AlertRollupEvery > 0 {
		go loop(a.Ctx, time.Minute, func(now time.Time) {
			last := settingTime(a.Ctx, a, keyRollupLast)
			if last.IsZero() {
				// First run: start from now so enabling alerts never dumps history.
				setSetting(a.Ctx, a, keyRollupLast, now.Format(time.RFC3339Nano))
				return
			}
			if now.Sub(last) < c.AlertRollupEvery {
				return
			}
			lines, err := alert.CopyLines(a.Ctx, a.Store, c.AlertSimDelay, last, now)
			if err != nil {
				slog.Warn("roll-up", "err", err)
				return
			}
			mu.Lock()
			sup := lim.Suppressed
			lim.Suppressed = 0
			mu.Unlock()
			title := fmt.Sprintf("Paper copies closed since %s (+%ds):", last.In(loc).Format("3:04pm"), c.AlertSimDelay)
			send(alert.Rollup(title, lines, sup)) // "" when nothing happened: no message
			setSetting(a.Ctx, a, keyRollupLast, now.Format(time.RFC3339Nano))
		})
	}
	if c.AlertDigestHour >= 0 {
		go loop(a.Ctx, time.Minute, func(now time.Time) {
			local := now.In(loc)
			today := local.Format("2006-01-02")
			if local.Hour() != c.AlertDigestHour {
				return
			}
			if v, _, _ := a.Store.GetSetting(a.Ctx, keyDigestLast); v == today {
				return
			}
			text, err := alert.Digest(a.Ctx, a.Store, c.AlertSimDelay, now)
			if err != nil {
				slog.Warn("digest", "err", err)
				return
			}
			send(text)
			setSetting(a.Ctx, a, keyDigestLast, today)
		})
	}

	err = a.Bus.Subscribe(a.Ctx, bus.StreamEvents, "alerter", "alert-1", func(ctx context.Context, b []byte) error {
		var ev model.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			return err
		}
		d := alert.Classify(ev)
		if !d.Now {
			return nil
		}
		if time.Since(ev.At) > time.Hour {
			return nil // backlog after downtime: the roll-up/digest cover it
		}
		mu.Lock()
		ok := lim.Allow(d, time.Now())
		mu.Unlock()
		if !ok {
			held.Add(1)
			return nil
		}
		text := alert.Format(ev)
		if ev.Kind == "exec_closed" {
			var p model.LivePosition
			if raw, err := json.Marshal(ev.Data); err == nil && json.Unmarshal(raw, &p) == nil {
				text = alert.ExecClosedText(ctx, a.Store, p, c.AlertSimDelay, time.Now())
			}
		}
		send(text)
		return nil
	})
	if err != nil && a.Ctx.Err() == nil {
		slog.Error("subscribe", "err", err)
	}
}

func loop(ctx context.Context, every time.Duration, f func(time.Time)) {
	t := time.NewTicker(every)
	defer t.Stop()
	f(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			f(now)
		}
	}
}

func settingTime(ctx context.Context, a *app.App, key string) time.Time {
	v, ok, err := a.Store.GetSetting(ctx, key)
	if err != nil || !ok {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

func setSetting(ctx context.Context, a *app.App, key, v string) {
	if err := a.Store.SetSetting(ctx, key, v); err != nil {
		slog.Warn("save setting", "key", key, "err", err)
	}
}
