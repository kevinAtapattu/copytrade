// Command alerter sends one Discord/Telegram message an hour about the copies
// the executor made of our live wallets: P&L this hour, P&L today, then a note
// per trade (see internal/alert/policy.go). Quiet hours send nothing. The only
// instant alerts are failed sells and safety stops (kill switch, daily loss limit).
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

const keyRollupLast = "alert_rollup_last" // RFC3339 end of the last hourly window

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
			lines, mode, err := alert.ExecLines(a.Ctx, a.Store, last, now)
			if err != nil {
				slog.Warn("hourly", "err", err)
				return
			}
			local := now.In(loc)
			midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
			day, _, err := alert.ExecLines(a.Ctx, a.Store, midnight, now)
			if err != nil {
				slog.Warn("hourly (today)", "err", err)
				return
			}
			fail, fmode, err := alert.ExecFailures(a.Ctx, a.Store, last, now)
			if err != nil {
				slog.Warn("hourly (failures)", "err", err)
				return
			}
			if mode == "" {
				mode = fmode
			}
			send(alert.Hourly(mode, lines, day, fail)) // "" when nothing closed or failed: no message
			setSetting(a.Ctx, a, keyRollupLast, now.Format(time.RFC3339Nano))
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
			return nil // stale after downtime
		}
		mu.Lock()
		ok := lim.Allow(d, time.Now())
		mu.Unlock()
		if !ok {
			held.Add(1)
			return nil
		}
		if ev.Kind == "error" {
			var f model.LiveFill
			if b, err := json.Marshal(ev.Data); err == nil && json.Unmarshal(b, &f) == nil && f.Mint != "" {
				send(alert.SellFailed(f, alert.WalletName(ctx, a.Store, f.Wallet)))
				return nil
			}
		}
		send(alert.Format(ev))
		return nil
	})
	if err != nil && a.Ctx.Err() == nil {
		slog.Error("subscribe", "err", err)
	}
}

// window names a roll-up period: "hour", "4 hours", "30 min".
func window(d time.Duration) string {
	switch h := d.Round(time.Hour).Hours(); {
	case d < 50*time.Minute:
		return fmt.Sprintf("%.0f min", d.Minutes())
	case h <= 1:
		return "hour"
	default:
		return fmt.Sprintf("%.0f hours", h)
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
