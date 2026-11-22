// Package price provides SOL/USD.
package price

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"sync/atomic"
	"time"
)

type Source interface {
	SOLUSD() float64
}

type Fixed float64

func (f Fixed) SOLUSD() float64 { return float64(f) }

// Live polls CoinGecko and falls back to the last good value or a configured default.
type Live struct {
	fallback float64
	cur      atomic.Uint64 // math.Float64bits
	URL      string
}

func NewLive(fallback float64) *Live {
	l := &Live{fallback: fallback, URL: "https://api.coingecko.com/api/v3/simple/price?ids=solana&vs_currencies=usd"}
	l.set(fallback)
	return l
}

func (l *Live) SOLUSD() float64 { return f64(l.cur.Load()) }

func (l *Live) set(v float64) { l.cur.Store(bits(v)) }

// Run refreshes every interval until ctx ends.
func (l *Live) Run(ctx context.Context, every time.Duration) {
	for {
		if v, err := l.fetch(ctx); err == nil && v > 0 {
			l.set(v)
		} else if err != nil {
			slog.Debug("sol price fetch", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (l *Live) fetch(ctx context.Context) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, l.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var body struct {
		Solana struct {
			USD float64 `json:"usd"`
		} `json:"solana"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	return body.Solana.USD, nil
}

// New returns a Fixed source when fixed > 0, else a Live one started in the background.
func New(ctx context.Context, fixed, fallback float64) Source {
	if fixed > 0 {
		return Fixed(fixed)
	}
	l := NewLive(fallback)
	// Fetch once before returning: a service that starts and trades straight away
	// must not size by the fallback ($150 vs a real $70 sized a buy at half).
	for i := 0; i < 3; i++ {
		if v, err := l.fetch(ctx); err == nil && v > 0 {
			l.set(v)
			break
		} else if err != nil {
			slog.Warn("sol price: first fetch failed, using fallback until it works", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	go l.Run(ctx, time.Minute)
	return l
}

func bits(f float64) uint64 { return math.Float64bits(f) }
func f64(b uint64) float64  { return math.Float64frombits(b) }
