// Command discovery periodically finds early buyers of 5x+ tokens and adds
// repeat names to the bench, then prunes inactive or bad bench wallets.
package main

import (
	"log/slog"
	"sync"
	"time"

	"copytrade/internal/app"
	"copytrade/internal/jobs"
)

func main() {
	a := app.Start("discovery")
	defer a.Close()

	var mu sync.Mutex
	var last jobs.DiscoveryResult
	var lastRun time.Time
	a.Heartbeat(30*time.Second, func() any {
		mu.Lock()
		defer mu.Unlock()
		return map[string]any{"last_run": lastRun, "tokens_scanned": last.TokensScanned, "new_hits": last.NewHits,
			"added": len(last.Added), "dropped": len(last.Dropped)}
	})

	t := time.NewTicker(a.Cfg.DiscoveryInterval)
	defer t.Stop()
	for {
		now := time.Now()
		res, err := jobs.Discover(a.Ctx, a.Store, a.Bus, a.Cfg, now)
		if err != nil {
			slog.Error("discovery run", "err", err)
			jobs.EmitError(a.Ctx, a.Bus, "discovery", err)
		} else {
			mu.Lock()
			last, lastRun = res, now
			mu.Unlock()
			slog.Info("discovery", "tokens", res.TokensScanned, "hits", res.NewHits, "added", len(res.Added), "dropped", len(res.Dropped))
		}
		select {
		case <-a.Ctx.Done():
			return
		case <-t.C:
		}
	}
}
