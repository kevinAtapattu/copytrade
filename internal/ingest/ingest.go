// Package ingest turns the raw trade stream into persisted trades and bus events,
// and manages which accounts and tokens we are subscribed to.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/model"
	"copytrade/internal/source"
	"copytrade/internal/store"
)

// Dust is the token balance below which a wallet counts as fully exited.
const Dust = 1.0

// MinCooldown keeps a token subscribed long enough for the +60s simulated exit.
const MinCooldown = 90 * time.Second

// TokenWatcher follows token prices for tokens tracked wallets hold. In stream
// mode it is the source itself (subscribeTokenTrade); in snapshot mode it polls
// the bonding curve over RPC instead.
type TokenWatcher interface {
	SubscribeTokens(mints []string)
	UnsubscribeTokens(mints []string)
}

type Ingestor struct {
	Src      source.Source
	Tokens   TokenWatcher
	Guard    Guard
	Store    store.Store
	Bus      bus.Bus
	Cooldown time.Duration
	MaxWatch time.Duration
	// Recorder, if set, receives every trade as a replay fixture line (JSONL).
	Recorder io.Writer
	// Observer, if set, sees every new trade after IsTarget is known (snapshot mode
	// uses it to schedule curve reads).
	Observer func(model.Trade)
	// Classify, if set, may adjust a tracked trade before it is stored or published
	// (snapshot mode flags tokens our SOL math cannot price).
	Classify func(context.Context, model.Trade) model.Trade

	mu      sync.Mutex
	tracked map[string]bool
	watch   map[string]*watch
	seen    *dedupe
	rate    rateWindow
	g       guardState
}

type watch struct {
	since   time.Time
	holders map[string]float64
	unsubAt time.Time // zero while a tracked wallet still holds
}

func New(src source.Source, st store.Store, b bus.Bus, cooldown, maxWatch time.Duration) *Ingestor {
	return &Ingestor{
		Src: src, Tokens: src, Store: st, Bus: b, Cooldown: max(cooldown, MinCooldown), MaxWatch: maxWatch,
		tracked: map[string]bool{}, watch: map[string]*watch{}, seen: newDedupe(50000),
	}
}

// ReloadWallets diffs the DB wallet list against current subscriptions. Every
// non-dropped wallet (live + bench) is tracked.
func (in *Ingestor) ReloadWallets(ctx context.Context) error {
	ws, err := in.Store.ListWallets(ctx)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, w := range ws {
		if w.Status != model.StatusDropped {
			want[w.Address] = true
		}
	}
	in.mu.Lock()
	var add, remove []string
	for a := range want {
		if !in.tracked[a] {
			add = append(add, a)
		}
	}
	for a := range in.tracked {
		if !want[a] {
			remove = append(remove, a)
		}
	}
	in.tracked = want
	paused := in.g.paused
	in.mu.Unlock()
	if len(add) > 0 && !paused {
		in.Src.SubscribeAccounts(add)
		slog.Info("subscribed accounts", "n", len(add))
	}
	if len(remove) > 0 && !paused {
		in.Src.UnsubscribeAccounts(remove)
		slog.Info("unsubscribed accounts", "n", len(remove))
	}
	return nil
}

// RestoreWatches re-subscribes tokens that tracked wallets still held when we last
// saw them (within MaxWatch), so a restart does not orphan open positions.
func (in *Ingestor) RestoreWatches(ctx context.Context, now time.Time) error {
	in.mu.Lock()
	wallets := make([]string, 0, len(in.tracked))
	for w := range in.tracked {
		wallets = append(wallets, w)
	}
	in.mu.Unlock()
	var mints []string
	for _, w := range wallets {
		trades, err := in.Store.ListTargetTrades(ctx, store.TradeFilter{Wallet: w, Since: now.Add(-in.MaxWatch)})
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, t := range trades { // newest first: the first row per mint is the latest balance
			if seen[t.Mint] {
				continue
			}
			seen[t.Mint] = true
			if t.NewTokenBalance <= Dust {
				continue
			}
			in.mu.Lock()
			wt, ok := in.watch[t.Mint]
			if !ok {
				wt = &watch{since: t.ReceivedAt, holders: map[string]float64{}}
				in.watch[t.Mint] = wt
				mints = append(mints, t.Mint)
			}
			wt.holders[w] = t.NewTokenBalance
			in.mu.Unlock()
		}
	}
	in.mu.Lock()
	paused := in.g.paused
	in.mu.Unlock()
	if len(mints) > 0 && !paused {
		in.Tokens.SubscribeTokens(mints)
		slog.Info("restored token subscriptions", "n", len(mints))
	}
	return nil
}

// HandleTrade is the source callback.
func (in *Ingestor) HandleTrade(ctx context.Context, t model.Trade) {
	in.mu.Lock()
	// Meter before dedupe: PumpPortal bills every message, duplicates included.
	pause, warn, spam := in.countMessage(t)
	if spam {
		in.mu.Unlock()
		in.dropSpammer(ctx, t.Trader, t.ReceivedAt)
		in.mu.Lock()
	}
	if in.seen.seen(t.Signature + "|" + t.Trader) {
		in.mu.Unlock()
		in.afterCount(ctx, pause, warn)
		return
	}
	in.rate.add(t.ReceivedAt)
	if in.Recorder != nil {
		line, _ := json.Marshal(source.FixtureLine{At: t.ReceivedAt, Msg: source.EncodePumpPortal(t)})
		_, _ = in.Recorder.Write(append(line, '\n'))
	}
	t.IsTarget = in.tracked[t.Trader]
	var subscribe bool
	if t.IsTarget {
		subscribe = in.trackHolding(t) && !in.g.paused
	}
	in.mu.Unlock()

	if in.Classify != nil && t.IsTarget {
		t = in.Classify(ctx, t)
	}
	if in.Observer != nil {
		in.Observer(t)
	}
	if subscribe {
		in.Tokens.SubscribeTokens([]string{t.Mint})
	}
	if _, err := in.Store.InsertTokenTrade(ctx, t); err != nil {
		slog.Warn("insert token trade", "err", err)
	}
	if t.IsTarget {
		if _, err := in.Store.InsertTargetTrade(ctx, t); err != nil {
			slog.Warn("insert target trade", "err", err)
		}
	}
	if err := in.Store.UpsertTokenFromTrade(ctx, t); err != nil {
		slog.Warn("upsert token", "err", err)
	}
	if err := bus.PublishJSON(ctx, in.Bus, bus.StreamTrades, t); err != nil {
		slog.Warn("publish trade", "err", err)
	}
	in.afterCount(ctx, pause, warn)
}

func (in *Ingestor) afterCount(ctx context.Context, pause, warn bool) {
	if warn {
		st := in.GuardStats()
		in.emit(ctx, "info", fmt.Sprintf("80%% of today's message budget used (%d/%d)", st.MessagesToday, st.DailyBudget))
	}
	if pause {
		in.pauseAll(ctx)
	}
}

// trackHolding updates who holds the token. Returns true if we must subscribe now.
// Caller holds in.mu.
func (in *Ingestor) trackHolding(t model.Trade) bool {
	w, ok := in.watch[t.Mint]
	holds := t.NewTokenBalance > Dust
	if !ok {
		if !holds {
			return false // a sell-out of a token we never watched; nothing to follow
		}
		w = &watch{since: t.ReceivedAt, holders: map[string]float64{}}
		in.watch[t.Mint] = w
	}
	if holds {
		w.holders[t.Trader] = t.NewTokenBalance
		w.unsubAt = time.Time{}
	} else {
		delete(w.holders, t.Trader)
		if len(w.holders) == 0 {
			w.unsubAt = t.ReceivedAt.Add(in.Cooldown)
		}
	}
	return !ok
}

// Tick unsubscribes tokens whose tracked holders have exited (after cooldown) or
// that we have watched longer than MaxWatch.
func (in *Ingestor) Tick(now time.Time) {
	in.resumeIfNewDay(context.Background(), now)
	in.mu.Lock()
	var drop []string
	for mint, w := range in.watch {
		expired := !w.unsubAt.IsZero() && !now.Before(w.unsubAt)
		tooOld := in.MaxWatch > 0 && now.Sub(w.since) > in.MaxWatch
		if expired || tooOld {
			drop = append(drop, mint)
			delete(in.watch, mint)
		}
	}
	in.mu.Unlock()
	if len(drop) > 0 {
		in.Tokens.UnsubscribeTokens(drop)
	}
}

// Watched returns the number of tokens currently subscribed by the ingestor.
func (in *Ingestor) Watched() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.watch)
}

func (in *Ingestor) IsWatching(mint string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	_, ok := in.watch[mint]
	return ok
}

// MsgsPerSec over the last minute (event time).
func (in *Ingestor) MsgsPerSec(now time.Time) float64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.rate.perSec(now)
}

// Run starts the source plus periodic wallet reloads and ticks. Blocks.
func (in *Ingestor) Run(ctx context.Context, reload time.Duration) error {
	if err := in.InitBudget(ctx, time.Now()); err != nil {
		slog.Warn("init budget", "err", err)
	}
	if err := in.ReloadWallets(ctx); err != nil {
		slog.Warn("initial wallet load", "err", err)
	}
	if err := in.RestoreWatches(ctx, time.Now()); err != nil {
		slog.Warn("restore token watches", "err", err)
	}
	go func() {
		rt := time.NewTicker(reload)
		tt := time.NewTicker(5 * time.Second)
		defer rt.Stop()
		defer tt.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-rt.C:
				if err := in.ReloadWallets(ctx); err != nil {
					slog.Warn("reload wallets", "err", err)
				}
			case now := <-tt.C:
				in.Tick(now)
			}
		}
	}()
	return in.Src.Run(ctx, func(t model.Trade) { in.HandleTrade(ctx, t) })
}

// dedupe is a fixed-size ring + set of recent keys.
type dedupe struct {
	ring []string
	pos  int
	set  map[string]struct{}
}

func newDedupe(n int) *dedupe {
	return &dedupe{ring: make([]string, n), set: make(map[string]struct{}, n)}
}

func (d *dedupe) seen(k string) bool {
	if _, ok := d.set[k]; ok {
		return true
	}
	if old := d.ring[d.pos]; old != "" {
		delete(d.set, old)
	}
	d.ring[d.pos] = k
	d.set[k] = struct{}{}
	d.pos = (d.pos + 1) % len(d.ring)
	return false
}

// rateWindow counts messages in 1s buckets for the last 60s.
type rateWindow struct {
	buckets [60]int
	secs    [60]int64
}

func (r *rateWindow) add(t time.Time) {
	s := t.Unix()
	i := s % 60
	if r.secs[i] != s {
		r.secs[i], r.buckets[i] = s, 0
	}
	r.buckets[i]++
}

func (r *rateWindow) perSec(now time.Time) float64 {
	n := 0
	cut := now.Unix() - 60
	for i := range r.buckets {
		if r.secs[i] > cut {
			n += r.buckets[i]
		}
	}
	return float64(n) / 60
}
