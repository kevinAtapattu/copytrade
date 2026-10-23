// Package snapshot is the cheap price source ("snapshot mode"). Instead of paying
// for every trade on every token, it reads the token's bonding curve account over
// RPC at exactly +2s/+10s/+60s after each tracked trade, and re-reads the curves of
// tokens tracked wallets hold every MarkEvery (for marks and stop-loss).
//
// Each read is published on the trades stream as a "snapshot" trade carrying the
// curve's reserves, so the simulator and executor need no changes.
package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/solana"
	"copytrade/internal/store"
)

// SideSnapshot marks a synthetic trade that only carries curve state.
const SideSnapshot = "snapshot"

type Result struct {
	Found   bool
	Account curve.Account
}

// Reader reads bonding curve accounts. at is the moment we want the state for;
// the RPC reader ignores it (it reads "now"), the replay reader uses it.
type Reader interface {
	Read(ctx context.Context, curves []string, at time.Time) ([]Result, error)
}

// RPCReader reads curves via getMultipleAccounts, 100 per request.
type RPCReader struct{ RPC *solana.RPC }

func (r RPCReader) Read(ctx context.Context, curves []string, _ time.Time) ([]Result, error) {
	out := make([]Result, len(curves))
	for i := 0; i < len(curves); i += 100 {
		j := min(i+100, len(curves))
		datas, err := r.RPC.GetMultipleAccounts(ctx, curves[i:j])
		if err != nil {
			return nil, err
		}
		for k, d := range datas {
			if d == nil {
				continue
			}
			if a, err := curve.ParseAccount(d); err == nil {
				out[i+k] = Result{Found: true, Account: a}
			}
		}
	}
	return out, nil
}

type req struct {
	due  time.Time
	mint string
}

type Snapshotter struct {
	Reader    Reader
	Bus       bus.Bus
	Store     store.Store
	Delays    []int
	MarkEvery time.Duration
	// SkipMarks, if set and returning true, skips periodic marks (e.g. RPC daily cap hit).
	// Fill reads at +2/+10/+60s are never skipped.
	SkipMarks func() bool

	mu       sync.Mutex
	keys     map[string]string // mint -> bonding curve address
	watched  map[string]bool
	pending  []req
	lastMark time.Time
	reads    int64
	errors   int64
	lastErr  string
}

func New(r Reader, b bus.Bus, st store.Store, delays []int, markEvery time.Duration) *Snapshotter {
	return &Snapshotter{Reader: r, Bus: b, Store: st, Delays: delays, MarkEvery: markEvery,
		keys: map[string]string{}, watched: map[string]bool{}}
}

// OnTrade schedules fill-time reads for each tracked trade on the bonding curve.
func (s *Snapshotter) OnTrade(t model.Trade) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.BondingCurveKey != "" {
		s.keys[t.Mint] = t.BondingCurveKey
	}
	if !t.IsTarget || (t.Pool != "" && t.Pool != "pump") {
		return
	}
	for _, d := range s.Delays {
		s.pending = append(s.pending, req{due: t.ReceivedAt.Add(time.Duration(d) * time.Second), mint: t.Mint})
	}
}

// SubscribeTokens / UnsubscribeTokens implement ingest.TokenWatcher: watched
// tokens get marked every MarkEvery.
func (s *Snapshotter) SubscribeTokens(mints []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range mints {
		s.watched[m] = true
	}
}

func (s *Snapshotter) UnsubscribeTokens(mints []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range mints {
		delete(s.watched, m)
	}
}

type Stats struct {
	Watched int    `json:"watched_tokens"`
	Pending int    `json:"pending_reads"`
	Reads   int64  `json:"curve_reads"`
	Errors  int64  `json:"read_errors"`
	LastErr string `json:"last_error,omitempty"`
}

func (s *Snapshotter) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Watched: len(s.watched), Pending: len(s.pending), Reads: s.reads, Errors: s.errors, LastErr: s.lastErr}
}

type job struct {
	stamp time.Time
	mint  string
}

// Tick performs every read that is due at now: fill reads whose time has come,
// plus a mark of all watched tokens if MarkEvery has passed. One batched RPC call.
func (s *Snapshotter) Tick(ctx context.Context, now time.Time) {
	s.mu.Lock()
	var jobs []job
	keep := s.pending[:0]
	for _, r := range s.pending {
		if !r.due.After(now) {
			jobs = append(jobs, job{r.due, r.mint})
		} else {
			keep = append(keep, r)
		}
	}
	s.pending = keep
	if s.MarkEvery > 0 && now.Sub(s.lastMark) >= s.MarkEvery && len(s.watched) > 0 &&
		(s.SkipMarks == nil || !s.SkipMarks()) {
		s.lastMark = now
		for m := range s.watched {
			jobs = append(jobs, job{now, m})
		}
	}
	if len(jobs) == 0 {
		s.mu.Unlock()
		return
	}
	var mints []string
	idx := map[string]int{}
	for _, j := range jobs {
		if _, ok := idx[j.mint]; !ok {
			idx[j.mint] = len(mints)
			mints = append(mints, j.mint)
		}
	}
	curves := make([]string, len(mints))
	for i, m := range mints {
		k := s.keys[m]
		if k == "" {
			var err error
			if k, err = curve.DeriveBondingCurve(m); err != nil {
				slog.Warn("no bonding curve address", "mint", m, "err", err)
			}
			s.keys[m] = k
		}
		curves[i] = k
	}
	s.mu.Unlock()

	// One batched read per distinct moment. Live ticks every 250ms, so a tick
	// rarely holds more than one or two moments; replay needs each exact moment.
	byStamp := map[time.Time][]job{}
	for _, j := range jobs {
		byStamp[j.stamp] = append(byStamp[j.stamp], j)
	}
	stamps := make([]time.Time, 0, len(byStamp))
	for st := range byStamp {
		stamps = append(stamps, st)
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })

	for _, st := range stamps {
		group := byStamp[st]
		var want []string
		seen := map[string]bool{}
		for _, j := range group {
			if !seen[j.mint] {
				seen[j.mint] = true
				want = append(want, j.mint)
			}
		}
		keys := make([]string, len(want))
		for i, m := range want {
			keys[i] = curves[idx[m]]
		}
		res, err := s.Reader.Read(ctx, keys, st)
		s.mu.Lock()
		s.reads++
		if err != nil {
			s.errors++
			s.lastErr = err.Error()
			// Retry fill reads on the next tick while they are still fresh.
			for _, j := range group {
				if j.stamp.After(now.Add(-5*time.Second)) && !j.stamp.Equal(now) {
					s.pending = append(s.pending, req{j.stamp, j.mint})
				}
			}
		}
		s.mu.Unlock()
		if err != nil {
			slog.Warn("curve read", "err", err)
			continue
		}
		for i, m := range want {
			if res[i].Found {
				s.publish(ctx, m, keys[i], res[i].Account, st)
			}
		}
	}
}

func (s *Snapshotter) publish(ctx context.Context, mint, key string, a curve.Account, at time.Time) {
	pool := "pump"
	if a.Complete {
		pool = "migrated"
	}
	t := model.Trade{
		Signature: fmt.Sprintf("snap:%s:%d", mint, at.UnixNano()), Mint: mint, Side: SideSnapshot,
		VSol: a.State.VSol, VTokens: a.State.VTokens, MarketCapSol: a.State.Price() * 1e9,
		Pool: pool, BondingCurveKey: key, ReceivedAt: at,
	}
	if err := s.Store.UpsertTokenFromTrade(ctx, t); err != nil {
		slog.Warn("upsert token from snapshot", "err", err)
	}
	if err := bus.PublishJSON(ctx, s.Bus, bus.StreamTrades, t); err != nil {
		slog.Warn("publish snapshot", "err", err)
	}
}
