package source

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// PumpPortal holds exactly ONE websocket. A single goroutine (Run) owns the
// connection and does all reads and writes. Subscriptions are kept as desired
// sets and re-sent after every reconnect.
type PumpPortal struct {
	URL    string
	APIKey string

	// Backoff tuning. Conservative on purpose: PumpPortal bans clients that reconnect in a loop.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	BatchSize  int

	mu       sync.Mutex
	accounts map[string]bool
	tokens   map[string]bool
	pending  []ppCommand
	dirty    chan struct{}
	paused   bool
	pauseCh  chan struct{} // wakes the session so it can close
	resumeCh chan struct{} // wakes Run so it can reconnect
	stats    Stats
}

type ppCommand struct {
	Method string   `json:"method"`
	Keys   []string `json:"keys,omitempty"`
}

func NewPumpPortal(wsURL, apiKey string) *PumpPortal {
	return &PumpPortal{
		URL: wsURL, APIKey: apiKey,
		MinBackoff: 5 * time.Second, MaxBackoff: 10 * time.Minute, BatchSize: 100,
		accounts: map[string]bool{}, tokens: map[string]bool{}, dirty: make(chan struct{}, 1),
		pauseCh: make(chan struct{}, 1), resumeCh: make(chan struct{}, 1),
	}
}

func (p *PumpPortal) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.Accounts, s.Tokens = len(p.accounts), len(p.tokens)
	return s
}

func (p *PumpPortal) SubscribeAccounts(keys []string) {
	p.change(p.accounts, keys, true, "subscribeAccountTrade")
}
func (p *PumpPortal) UnsubscribeAccounts(keys []string) {
	p.change(p.accounts, keys, false, "unsubscribeAccountTrade")
}
func (p *PumpPortal) SubscribeTokens(keys []string) {
	p.change(p.tokens, keys, true, "subscribeTokenTrade")
}
func (p *PumpPortal) UnsubscribeTokens(keys []string) {
	p.change(p.tokens, keys, false, "unsubscribeTokenTrade")
}

func (p *PumpPortal) change(set map[string]bool, keys []string, add bool, method string) {
	p.mu.Lock()
	var changed []string
	for _, k := range keys {
		if set[k] != add {
			changed = append(changed, k)
			if add {
				set[k] = true
			} else {
				delete(set, k)
			}
		}
	}
	if len(changed) > 0 {
		p.pending = append(p.pending, ppCommand{Method: method, Keys: changed})
	}
	p.mu.Unlock()
	if len(changed) > 0 {
		select {
		case p.dirty <- struct{}{}:
		default:
		}
	}
}

// Run connects and reads until ctx ends, reconnecting with backoff. Never opens
// a second connection while one is alive.
var errPaused = errors.New("paused")

// Pause closes the connection and keeps it closed until Resume. This is how the
// spending guard stops billing: no connection, no messages, regardless of how
// PumpPortal treats unsubscribe requests.
func (p *PumpPortal) Pause() {
	p.mu.Lock()
	p.paused = true
	p.mu.Unlock()
	signal(p.pauseCh)
}

// Resume reconnects (one connection) and re-sends every subscription.
func (p *PumpPortal) Resume() {
	p.mu.Lock()
	p.paused = false
	p.mu.Unlock()
	signal(p.resumeCh)
}

func (p *PumpPortal) isPaused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (p *PumpPortal) Run(ctx context.Context, h Handler) error {
	if p.APIKey == "" {
		p.setErr("PUMPPORTAL_API_KEY not set; ingest idle")
		slog.Error("PUMPPORTAL_API_KEY not set; ingest will not connect")
		<-ctx.Done()
		return ctx.Err()
	}
	backoff := p.MinBackoff
	for ctx.Err() == nil {
		for p.isPaused() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-p.resumeCh:
			}
		}
		started := time.Now()
		err := p.session(ctx, h)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errPaused) {
			slog.Warn("pumpportal connection closed: spending guard paused ingest")
			backoff = p.MinBackoff
			continue
		}
		p.setErr(errString(err))
		// A session that stayed up a while resets backoff; quick failures grow it.
		if time.Since(started) > 2*time.Minute {
			backoff = p.MinBackoff
		}
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "ban") {
			backoff = time.Hour // bans expire hourly
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
		slog.Warn("pumpportal disconnected, backing off", "err", err, "wait", wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff = min(backoff*2, p.MaxBackoff)
	}
	return ctx.Err()
}

func (p *PumpPortal) session(ctx context.Context, h Handler) error {
	u, err := url.Parse(p.URL)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("api-key", p.APIKey)
	u.RawQuery = q.Encode()

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if resp != nil {
			return errors.New("dial: " + resp.Status + ": " + err.Error())
		}
		return err
	}
	defer conn.Close()

	p.mu.Lock()
	p.stats.Connected = true
	p.stats.Reconnects++
	p.pending = nil // full resubscribe below covers everything
	accounts := keysOf(p.accounts)
	tokens := keysOf(p.tokens)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.stats.Connected = false
		p.mu.Unlock()
	}()
	slog.Info("pumpportal connected", "accounts", len(accounts), "tokens", len(tokens))

	if err := p.sendBatched(conn, "subscribeAccountTrade", accounts); err != nil {
		return err
	}
	if err := p.sendBatched(conn, "subscribeTokenTrade", tokens); err != nil {
		return err
	}

	type frame struct {
		data []byte
		at   time.Time
	}
	frames := make(chan frame, 1024)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(90 * time.Second)) })
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			select {
			case frames <- frame{data, time.Now()}:
			case <-done:
				return
			}
		}
	}()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return ctx.Err()
		case err := <-readErr:
			return err
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return err
			}
		case <-p.pauseCh:
			if p.isPaused() {
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return errPaused
			}
		case <-p.dirty:
			if err := p.flush(conn); err != nil {
				return err
			}
		case f := <-frames:
			t, ok := ParsePumpPortal(f.data, f.at)
			if !ok {
				p.handleControl(f.data)
				continue
			}
			p.mu.Lock()
			p.stats.Messages++
			p.stats.LastMessageAt = f.at
			p.mu.Unlock()
			h(t)
			// Handler may have queued subscriptions; flush promptly.
			if err := p.flush(conn); err != nil {
				return err
			}
		}
	}
}

func (p *PumpPortal) flush(conn *websocket.Conn) error {
	p.mu.Lock()
	cmds := p.pending
	p.pending = nil
	p.mu.Unlock()
	for _, c := range cmds {
		if err := p.sendBatched(conn, c.Method, c.Keys); err != nil {
			return err
		}
	}
	return nil
}

func (p *PumpPortal) sendBatched(conn *websocket.Conn, method string, keys []string) error {
	for i := 0; i < len(keys); i += p.BatchSize {
		j := min(i+p.BatchSize, len(keys))
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteJSON(ppCommand{Method: method, Keys: keys[i:j]}); err != nil {
			return err
		}
	}
	return nil
}

func (p *PumpPortal) handleControl(data []byte) {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return
	}
	if e, ok := m["errors"]; ok {
		slog.Warn("pumpportal error message", "errors", e)
		p.setErr("pumpportal: " + string(data))
		return
	}
	if msg, ok := m["message"]; ok {
		slog.Info("pumpportal", "message", msg)
	}
}

func (p *PumpPortal) setErr(s string) {
	p.mu.Lock()
	p.stats.LastError = s
	p.mu.Unlock()
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
