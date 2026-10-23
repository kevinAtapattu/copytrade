package source

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"copytrade/internal/model"
)

// FixtureLine is one line of a replay file: a raw PumpPortal message and when it arrived.
type FixtureLine struct {
	At  time.Time       `json:"at"`
	Msg json.RawMessage `json:"msg"`
}

// Replay feeds recorded messages in order. Like the real stream, it only delivers
// trades by subscribed accounts or on subscribed tokens, so the auto-subscribe
// logic (and its race with early trades) is exercised.
type Replay struct {
	lines []FixtureLine
	// AfterEach runs after every delivered or skipped message with its event time,
	// letting the driver advance event-time clocks in other components.
	AfterEach func(at time.Time)

	mu       sync.Mutex
	paused   bool
	accounts map[string]bool
	tokens   map[string]bool
	stats    Stats
}

func NewReplay(r io.Reader) (*Replay, error) {
	rp := &Replay{accounts: map[string]bool{}, tokens: map[string]bool{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var l FixtureLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, err
		}
		rp.lines = append(rp.lines, l)
	}
	return rp, sc.Err()
}

func (r *Replay) Len() int { return len(r.lines) }

// Trades returns every trade in the file (ignoring subscriptions), for building
// a history-backed curve reader.
func (r *Replay) Trades() []model.Trade {
	out := make([]model.Trade, 0, len(r.lines))
	for _, l := range r.lines {
		if t, ok := ParsePumpPortal(l.Msg, l.At); ok {
			out = append(out, t)
		}
	}
	return out
}

func (r *Replay) Run(ctx context.Context, h Handler) error {
	r.mu.Lock()
	r.stats.Connected = true
	r.mu.Unlock()
	for _, l := range r.lines {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t, ok := ParsePumpPortal(l.Msg, l.At)
		if ok {
			r.mu.Lock()
			deliver := !r.paused && (r.accounts[t.Trader] || r.tokens[t.Mint])
			if deliver {
				r.stats.Messages++
				r.stats.LastMessageAt = l.At
			}
			r.mu.Unlock()
			if deliver {
				h(t)
			}
		}
		if r.AfterEach != nil {
			r.AfterEach(l.At)
		}
	}
	return nil
}

func (r *Replay) set(m map[string]bool, keys []string, v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range keys {
		if v {
			m[k] = true
		} else {
			delete(m, k)
		}
	}
}

func (r *Replay) SubscribeAccounts(k []string)   { r.set(r.accounts, k, true) }
func (r *Replay) UnsubscribeAccounts(k []string) { r.set(r.accounts, k, false) }
func (r *Replay) SubscribeTokens(k []string)     { r.set(r.tokens, k, true) }
func (r *Replay) UnsubscribeTokens(k []string)   { r.set(r.tokens, k, false) }

// Pause stops delivery, like a closed connection.
func (r *Replay) Pause()  { r.mu.Lock(); r.paused = true; r.mu.Unlock() }
func (r *Replay) Resume() { r.mu.Lock(); r.paused = false; r.mu.Unlock() }

func (r *Replay) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	s.Accounts, s.Tokens = len(r.accounts), len(r.tokens)
	return s
}
