package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/model"
)

// Guard caps what the metered stream can cost.
//   - DailyBudget: max metered messages per UTC day. When hit, the PumpPortal
//     connection is closed until midnight UTC, then reopened automatically.
//   - WalletMaxPerHour: a tracked wallet that trades more than this in a rolling hour is
//     dropped (status "dropped", reason logged) and unsubscribed. Re-add it from the
//     dashboard if you still want it.
type Guard struct {
	DailyBudget      int64
	WalletMaxPerHour int
}

type guardState struct {
	day       time.Time // UTC midnight of the current budget day
	today     int64     // metered messages received today
	paused    bool
	warned    bool
	walletHit map[string][]time.Time
}

// GuardStats is reported on the health page.
type GuardStats struct {
	MessagesToday int64     `json:"messages_today"`
	DailyBudget   int64     `json:"daily_budget"`
	Paused        bool      `json:"paused"`
	ResumesAt     time.Time `json:"resumes_at,omitempty"`
	WalletMaxHour int       `json:"wallet_max_per_hour"`
}

func utcDay(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// InitBudget restores today's message count from the database after a restart.
// If the budget is already spent, ingest starts paused.
func (in *Ingestor) InitBudget(ctx context.Context, now time.Time) error {
	day := utcDay(now)
	n, err := in.Store.CountTokenTradesSince(ctx, day)
	if err != nil {
		return err
	}
	in.mu.Lock()
	in.g.day, in.g.today = day, n
	over := in.Guard.DailyBudget > 0 && n >= in.Guard.DailyBudget
	if over {
		in.g.paused = true
	}
	in.mu.Unlock()
	if over {
		in.Src.Pause() // never connect until midnight UTC
		slog.Warn("daily message budget already spent; staying disconnected until midnight UTC", "today", n, "budget", in.Guard.DailyBudget)
	}
	return nil
}

// GuardStats returns the current budget state.
func (in *Ingestor) GuardStats() GuardStats {
	in.mu.Lock()
	defer in.mu.Unlock()
	s := GuardStats{MessagesToday: in.g.today, DailyBudget: in.Guard.DailyBudget, Paused: in.g.paused,
		WalletMaxHour: in.Guard.WalletMaxPerHour}
	if in.g.paused {
		s.ResumesAt = in.g.day.Add(24 * time.Hour)
	}
	return s
}

// countMessage meters one received message. Caller holds in.mu.
// Returns pause=true when this message used up the budget, and spam=true when
// the trader is a tracked wallet over its hourly limit.
func (in *Ingestor) countMessage(t model.Trade) (pause, warn, spam bool) {
	if d := utcDay(t.ReceivedAt); d.After(in.g.day) {
		in.g.day, in.g.today, in.g.warned = d, 0, false
	}
	in.g.today++
	if b := in.Guard.DailyBudget; b > 0 && !in.g.paused {
		if in.g.today >= b {
			in.g.paused, pause = true, true
		} else if !in.g.warned && in.g.today >= b*8/10 {
			in.g.warned, warn = true, true
		}
	}
	if max := in.Guard.WalletMaxPerHour; max > 0 && in.tracked[t.Trader] {
		if in.g.walletHit == nil {
			in.g.walletHit = map[string][]time.Time{}
		}
		cut := t.ReceivedAt.Add(-time.Hour)
		hits := in.g.walletHit[t.Trader]
		i := 0
		for i < len(hits) && hits[i].Before(cut) {
			i++
		}
		hits = append(hits[i:], t.ReceivedAt)
		in.g.walletHit[t.Trader] = hits
		if len(hits) > max {
			spam = true
			delete(in.tracked, t.Trader)
			delete(in.g.walletHit, t.Trader)
		}
	}
	return
}

// pauseAll stops all spending (budget hit) by closing the source connection, so the
// cap holds no matter how PumpPortal treats unsubscribe requests. Token price reads
// stop too. Tracked/watch state is kept so resume can restore it.
func (in *Ingestor) pauseAll(ctx context.Context) {
	in.mu.Lock()
	tokens := keys(in.watch)
	resume := in.g.day.Add(24 * time.Hour)
	used, budget := in.g.today, in.Guard.DailyBudget
	in.mu.Unlock()
	in.Src.Pause()
	if len(tokens) > 0 {
		in.Tokens.UnsubscribeTokens(tokens)
	}
	msg := fmt.Sprintf("daily message budget hit (%d/%d, ~%.4f SOL). All subscriptions paused until %s UTC.",
		used, budget, float64(used)/10000*0.01, resume.Format("Jan 2 15:04"))
	slog.Warn(msg)
	in.emit(ctx, "error", msg)
}

// resumeIfNewDay restores subscriptions after midnight UTC. Called from Tick.
func (in *Ingestor) resumeIfNewDay(ctx context.Context, now time.Time) {
	in.mu.Lock()
	d := utcDay(now)
	if !d.After(in.g.day) {
		in.mu.Unlock()
		return
	}
	in.g.day, in.g.today, in.g.warned = d, 0, false
	wasPaused := in.g.paused
	in.g.paused = false
	accounts := keys(in.tracked)
	tokens := keys(in.watch)
	in.mu.Unlock()
	if !wasPaused {
		return
	}
	if len(accounts) > 0 {
		in.Src.SubscribeAccounts(accounts) // no-op for ones still in the desired set
	}
	if len(tokens) > 0 {
		in.Tokens.SubscribeTokens(tokens)
	}
	in.Src.Resume() // one fresh connection, every subscription re-sent
	in.emit(ctx, "info", "new UTC day: message budget reset, subscriptions resumed")
}

func (in *Ingestor) dropSpammer(ctx context.Context, wallet string, at time.Time) {
	in.Src.UnsubscribeAccounts([]string{wallet})
	reason := fmt.Sprintf("dropped: spam guard, over %d trades in 1 hour", in.Guard.WalletMaxPerHour)
	if _, err := in.Store.SetWalletStatus(ctx, wallet, model.StatusDropped, reason, at); err != nil {
		slog.Warn("drop spammer", "err", err)
	}
	slog.Warn("spam guard", "wallet", wallet, "reason", reason)
	in.emit(ctx, "rotation", wallet+" -> dropped: "+reason)
}

func (in *Ingestor) emit(ctx context.Context, kind, msg string) {
	_ = bus.PublishJSON(ctx, in.Bus, bus.StreamEvents, model.Event{Kind: kind, At: time.Now(), Service: "ingest", Message: msg})
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
