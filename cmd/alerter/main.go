// Command alerter forwards trade, rotation, discovery and error events to Discord and/or Telegram.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"copytrade/internal/alert"
	"copytrade/internal/app"
	"copytrade/internal/bus"
	"copytrade/internal/model"
)

func main() {
	a := app.Start("alerter")
	defer a.Close()
	var targets []alert.Notifier
	var names []string
	for _, n := range []alert.Notifier{
		alert.NewDiscord(a.Cfg.DiscordWebhookURL),
		alert.NewTelegram(a.Cfg.TelegramBotToken, a.Cfg.TelegramChatID),
	} {
		if n.Enabled() {
			targets = append(targets, n)
			names = append(names, n.Name())
		}
	}
	if len(targets) == 0 {
		slog.Warn("no DISCORD_WEBHOOK_URL or TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID set; alerts are logged only")
	}
	var sent, dropped atomic.Int64
	a.Heartbeat(30*time.Second, func() any {
		return map[string]any{"targets": names, "sent": sent.Load(), "dropped": dropped.Load()}
	})

	// Queue + 1 msg/sec pacing keeps us under Discord/Telegram rate limits.
	q := make(chan string, 100)
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

	err := a.Bus.Subscribe(a.Ctx, bus.StreamEvents, "alerter", "alert-1", func(_ context.Context, b []byte) error {
		var ev model.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			return err
		}
		text := alert.Format(ev)
		if text == "" {
			return nil
		}
		slog.Info("alert", "text", text)
		if len(targets) == 0 {
			return nil
		}
		select {
		case q <- text:
		default:
			dropped.Add(1)
		}
		return nil
	})
	if err != nil && a.Ctx.Err() == nil {
		slog.Error("subscribe", "err", err)
	}
}
