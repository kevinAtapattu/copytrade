package nonsolsim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WSOL is wrapped SOL, Jupiter's mint for SOL.
const WSOL = "So11111111111111111111111111111111111111112"

// Quote is what a swap of InAmount would give.
type Quote struct {
	InAmount  uint64
	OutAmount uint64
	Route     string // e.g. "Raydium CLMM > Pump.fun"
}

// Quoter prices a swap. Amounts are raw units (lamports for SOL).
type Quoter interface {
	Quote(ctx context.Context, inMint, outMint string, amount uint64) (Quote, error)
}

// NoRouteError is a swap Jupiter refuses (TOKEN_NOT_TRADABLE, NO_ROUTES_FOUND...).
type NoRouteError struct {
	Code string
	Msg  string
}

func (e *NoRouteError) Error() string { return e.Code + ": " + e.Msg }

// Jupiter calls the free quote API, at most one request per Gap across all
// callers, with one retry on 429.
type Jupiter struct {
	Base        string // e.g. https://lite-api.jup.ag/swap/v1
	SlippageBps int
	Gap         time.Duration
	HTTP        *http.Client

	mu   sync.Mutex
	next time.Time
}

func NewJupiter(base string) *Jupiter {
	return &Jupiter{Base: strings.TrimRight(base, "/"), SlippageBps: 1000, Gap: 400 * time.Millisecond,
		HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// wait blocks until this caller's turn under the shared rate limit.
func (j *Jupiter) wait(ctx context.Context) error {
	j.mu.Lock()
	now := time.Now()
	at := j.next
	if at.Before(now) {
		at = now
	}
	j.next = at.Add(j.Gap)
	j.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Until(at)):
		return nil
	}
}

func (j *Jupiter) Quote(ctx context.Context, inMint, outMint string, amount uint64) (Quote, error) {
	q := url.Values{"inputMint": {inMint}, "outputMint": {outMint}, "amount": {strconv.FormatUint(amount, 10)},
		"slippageBps": {strconv.Itoa(j.SlippageBps)}}
	u := j.Base + "/quote?" + q.Encode()
	for try := 0; ; try++ {
		if err := j.wait(ctx); err != nil {
			return Quote{}, err
		}
		qt, retry, err := j.get(ctx, u)
		if !retry || try > 0 {
			return qt, err
		}
		select {
		case <-ctx.Done():
			return Quote{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (j *Jupiter) get(ctx context.Context, u string) (Quote, bool, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := j.HTTP.Do(req)
	if err != nil {
		return Quote{}, true, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return Quote{}, true, fmt.Errorf("jupiter: %s", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error     string `json:"error"`
			ErrorCode string `json:"errorCode"`
		}
		if json.Unmarshal(data, &e) == nil && e.ErrorCode != "" {
			return Quote{}, false, &NoRouteError{Code: e.ErrorCode, Msg: e.Error}
		}
		return Quote{}, false, fmt.Errorf("jupiter: %s: %.200s", resp.Status, data)
	}
	return parseQuote(data)
}

func parseQuote(data []byte) (Quote, bool, error) {
	var r struct {
		InAmount  string `json:"inAmount"`
		OutAmount string `json:"outAmount"`
		RoutePlan []struct {
			SwapInfo struct {
				Label string `json:"label"`
			} `json:"swapInfo"`
		} `json:"routePlan"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Quote{}, false, fmt.Errorf("jupiter: bad quote: %w", err)
	}
	in, _ := strconv.ParseUint(r.InAmount, 10, 64)
	out, err := strconv.ParseUint(r.OutAmount, 10, 64)
	if err != nil || out == 0 {
		return Quote{}, false, &NoRouteError{Code: "ZERO_OUT", Msg: "quote has no output amount"}
	}
	var labels []string
	for _, p := range r.RoutePlan {
		labels = append(labels, p.SwapInfo.Label)
	}
	return Quote{InAmount: in, OutAmount: out, Route: strings.Join(labels, " > ")}, false, nil
}

// ReasonOf is the skip reason for a failed quote.
func ReasonOf(err error) string {
	var nr *NoRouteError
	if errors.As(err, &nr) {
		return nr.Code
	}
	return "QUOTE_FAILED"
}
