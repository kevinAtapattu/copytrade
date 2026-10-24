// Command discovery periodically finds early buyers of 5x+ tokens and adds
// repeat names to the bench, then prunes inactive or bad bench wallets.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"copytrade/internal/app"
	"copytrade/internal/history"
	"copytrade/internal/jobs"
	"copytrade/internal/model"
	"copytrade/internal/solana"
)

// capped stops fetching token history once the job's daily RPC allowance is used.
type capped struct {
	f   history.Fetcher
	rpc *solana.RPC
	max int64
}

func (c capped) TokenTrades(ctx context.Context, tok model.Token) ([]model.Trade, error) {
	if today, _ := c.rpc.Calls(); c.max > 0 && today >= c.max {
		return nil, fmt.Errorf("%w (%d calls)", jobs.ErrBudget, c.max)
	}
	return c.f.TokenTrades(ctx, tok)
}

func main() {
	a := app.Start("discovery")
	defer a.Close()

	var hist jobs.History // stream mode: trades we stored
	var rpc *solana.RPC
	if a.Cfg.DataMode == "snapshot" {
		rpc = solana.NewRPC(a.Cfg.SolanaRPCURL)
		rpc.RPS = max(a.Cfg.RPCRPS/4, 1) // background job: leave headroom for ingest's price reads
		hist = capped{history.Fetcher{RPC: rpc, MaxPages: a.Cfg.DiscoveryMaxPages, MaxTx: a.Cfg.DiscoveryMaxTx}, rpc, a.Cfg.DiscoveryRPCDailyMax}
	}

	var mu sync.Mutex
	var last jobs.DiscoveryResult
	var lastRun time.Time
	a.Heartbeat(30*time.Second, func() any {
		mu.Lock()
		defer mu.Unlock()
		m := map[string]any{"last_run": lastRun, "tokens_scanned": last.TokensScanned, "new_hits": last.NewHits,
			"added": len(last.Added), "dropped": len(last.Dropped)}
		if rpc != nil {
			m["rpc_calls_today"], _ = rpc.Calls()
		}
		return m
	})

	t := time.NewTicker(a.Cfg.DiscoveryInterval)
	defer t.Stop()
	for {
		now := time.Now()
		res, err := jobs.Discover(a.Ctx, a.Store, a.Bus, a.Cfg, now, hist)
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
