// Package alert sends notifications to Discord and/or Telegram.
package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"copytrade/internal/model"
)

// Notifier is one alert destination.
type Notifier interface {
	Name() string
	Enabled() bool
	Send(ctx context.Context, text string) error
}

// Discord posts to a channel webhook.
type Discord struct {
	WebhookURL string
	HTTP       *http.Client
}

func NewDiscord(url string) *Discord {
	return &Discord{WebhookURL: url, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (d *Discord) Name() string  { return "discord" }
func (d *Discord) Enabled() bool { return d.WebhookURL != "" }

func (d *Discord) Send(ctx context.Context, text string) error {
	if len(text) > 1900 { // Discord caps content at 2000 chars
		text = text[:1900] + "..."
	}
	body, _ := json.Marshal(map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.WebhookURL, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("discord %s: %s", resp.Status, b)
	}
	return nil
}

type Telegram struct {
	Token  string
	ChatID string
	HTTP   *http.Client
	Base   string
}

func NewTelegram(token, chat string) *Telegram {
	return &Telegram{Token: token, ChatID: chat, HTTP: &http.Client{Timeout: 10 * time.Second}, Base: "https://api.telegram.org"}
}

func (t *Telegram) Name() string { return "telegram" }

func (t *Telegram) Enabled() bool { return t.Token != "" && t.ChatID != "" }

func (t *Telegram) Send(ctx context.Context, text string) error {
	form := url.Values{"chat_id": {t.ChatID}, "text": {text}, "disable_web_page_preview": {"true"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/bot%s/sendMessage", t.Base, t.Token),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("telegram %s: %s", resp.Status, b)
	}
	return nil
}

// Format turns an event into an alert line, or "" if it should not alert.
// Simulated fills and target trades are too frequent to alert on.
func Format(ev model.Event) string {
	icon := map[string]string{"live_fill": "TRADE", "rotation": "ROTATION", "error": "ERROR", "discovery": "DISCOVERY", "info": "INFO"}[ev.Kind]
	if icon == "" {
		return ""
	}
	msg := ev.Message
	if msg == "" {
		b, _ := json.Marshal(ev.Data)
		msg = string(b)
	}
	return fmt.Sprintf("[%s] %s (%s)", icon, msg, ev.Service)
}
