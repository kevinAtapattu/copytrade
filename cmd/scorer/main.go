// Command scorer periodically computes copyability scores and rotates wallets
// between live and bench.
package main

import (
	"log/slog"
	"sync"
	"time"

	"copytrade/internal/app"
	"copytrade/internal/jobs"
)

func main() {
	a := app.Start("scorer")
	defer a.Close()

	var mu sync.Mutex
	var lastRun time.Time
	var lastScores, lastChanges int
	a.Heartbeat(30*time.Second, func() any {
		mu.Lock()
		defer mu.Unlock()
		return map[string]any{"last_run": lastRun, "scores": lastScores, "rotations": lastChanges}
	})

	t := time.NewTicker(a.Cfg.ScoreInterval)
	defer t.Stop()
	for {
		now := time.Now()
		scores, changes, err := jobs.ScoreAndRotate(a.Ctx, a.Store, a.Bus, a.Cfg, now)
		if err != nil {
			slog.Error("score run", "err", err)
			jobs.EmitError(a.Ctx, a.Bus, "scorer", err)
		} else {
			mu.Lock()
			lastRun, lastScores, lastChanges = now, len(scores), len(changes)
			mu.Unlock()
			slog.Info("scored", "scores", len(scores), "rotations", len(changes), "took", time.Since(now).Round(time.Millisecond))
		}
		select {
		case <-a.Ctx.Done():
			return
		case <-t.C:
		}
	}
}
