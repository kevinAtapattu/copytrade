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
//   - WalletMaxPerDay: a tracked wallet that trades more than this in one UTC day is
//     paused (status "paused", reason shown) and unsubscribed. It stays on the
//     dashboard and returns to the bench at the next UTC midnight (or via Resume).
type Guard struct {
	DailyBudget     int64
	WalletMaxPerDay int
}

type guardState struct {
	day       time.Time // UTC midnight of the current budget day
	today     int64     // metered messages received today
	paused    bool
	warned    bool
	walletDay map[string]int // trades per tracked wallet this UTC day
}

// GuardStats is reported on the health page.
type GuardStats struct {
	MessagesToday int64     `json:"messages_today"`
	DailyBudget   int64     `json:"daily_budget"`
	Paused        bool      `json:"paused"`
	ResumesAt     time.Time `json:"resumes_at,omitempty"`
	WalletMaxDay  int       `json:"wallet_max_per_day"`
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
		WalletMaxDay: in.Guard.WalletMaxPerDay}
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
		in.g.walletDay = nil
	}
	in.g.today++
	if b := in.Guard.DailyBudget; b > 0 && !in.g.paused {
		if in.g.today >= b {
			in.g.paused, pause = true, true
		} else if !in.g.warned && in.g.today >= b*8/10 {
			in.g.warned, warn = true, true
		}
	}
	if max := in.Guard.WalletMaxPerDay; max > 0 && in.tracked[t.Trader] {
		if in.g.walletDay == nil {
			in.g.walletDay = map[string]int{}
		}
		in.g.walletDay[t.Trader]++
		if in.g.walletDay[t.Trader] > max {
			spam = true
			delete(in.tracked, t.Trader)
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
	reason := fmt.Sprintf("paused: over %d trades today (daily limit), resumes %s UTC",
		in.Guard.WalletMaxPerDay, utcDay(at).Add(24*time.Hour).Format("Jan 2 15:04"))
	if _, err := in.Store.SetWalletStatus(ctx, wallet, model.StatusPaused, reason, at); err != nil {
		slog.Warn("pause spammer", "err", err)
	}
	slog.Warn("spam guard", "wallet", wallet, "reason", reason)
	in.emit(ctx, "rotation", wallet+" -> "+reason)
}

// PauseNoisy pauses a wallet whose address shows up in too much third-party
// traffic (the source has already unsubscribed it). Same status and next-day
// resume as the daily trade limit.
func (in *Ingestor) PauseNoisy(ctx context.Context, wallet string, mb, limit float64, at time.Time) {
	in.mu.Lock()
	delete(in.tracked, wallet)
	in.mu.Unlock()
	in.Src.UnsubscribeAccounts([]string{wallet})
	reason := fmt.Sprintf("paused: other bots mention this wallet in too many txs (%.0f MB of data today, limit %.0f MB), resumes %s UTC",
		mb, limit, utcDay(at).Add(24*time.Hour).Format("Jan 2 15:04"))
	if _, err := in.Store.SetWalletStatus(ctx, wallet, model.StatusPaused, reason, at); err != nil {
		slog.Warn("pause noisy wallet", "err", err)
	}
	slog.Warn("noise guard", "wallet", wallet, "reason", reason)
	in.emit(ctx, "rotation", wallet+" -> "+reason)
}

// resumePaused returns wallets paused by the daily limit to the bench once a new
// UTC day starts. Returns the wallets resumed so the caller can track them again.
func (in *Ingestor) resumePaused(ctx context.Context, ws []model.Wallet, now time.Time) map[string]bool {
	out := map[string]bool{}
	for _, w := range ws {
		if w.Status != model.StatusPaused || !utcDay(now).After(utcDay(w.StatusAt)) {
			continue
		}
		ok, err := in.Store.SetWalletStatus(ctx, w.Address, model.StatusBench, "resumed: new day", now)
		if err != nil {
			slog.Warn("resume paused wallet", "err", err)
			continue
		}
		if ok {
			out[w.Address] = true
			in.emit(ctx, "rotation", w.Address+" -> bench: resumed for the new day")
		}
	}
	return out
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
