// Command nonsolsim paper-copies the live wallets, KEEP_WALLETS and
// NONSOL_WALLETS (default nad) on coins quoted in a token other than SOL,
// priced with Jupiter quotes. Writes only the nonsol_copies table; emits no events.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"copytrade/internal/app"
	"copytrade/internal/bus"
	"copytrade/internal/model"
	"copytrade/internal/nonsolsim"
	"copytrade/internal/price"
)

const group = "nonsolsim" // its own consumer group: sees every trade, takes none from others

func main() {
	a := app.Start("nonsolsim")
	defer a.Close()
	c := a.Cfg

	px := price.New(a.Ctx, c.SolUSDFixed, c.SolUSDFallback)
	eng := nonsolsim.New(nonsolsim.NewJupiter(c.JupiterURL), a.PG, px.SOLUSD)
	eng.FeeSol = c.BaseFeeSol + c.PriorityFeeSol
	for _, w := range append(append([]string{}, c.KeepWallets...), c.NonsolWallets...) {
		eng.Keep[w] = true
	}
	if err := eng.LoadOpen(a.Ctx); err != nil {
		app.Fatal("load open copies", err)
	}
	if err := eng.RefreshWallets(a.Ctx); err != nil {
		app.Fatal("load wallets", err)
	}

	go every(a.Ctx, time.Minute, func() {
		if err := eng.RefreshWallets(a.Ctx); err != nil && a.Ctx.Err() == nil {
			slog.Warn("refresh wallets", "err", err)
		}
	})
	go every(a.Ctx, 5*time.Minute, func() { eng.MarkOpen(a.Ctx) })

	var processed, nonsol atomic.Int64
	a.Heartbeat(30*time.Second, func() any {
		lag, _ := a.Redis.Lag(context.Background(), bus.StreamTrades, group)
		return map[string]any{"processed": processed.Load(), "nonsol_target_trades": nonsol.Load(),
			"open_copies": eng.OpenCount(), "stream_lag": lag}
	})

	err := a.Bus.Subscribe(a.Ctx, bus.StreamTrades, group, group+"-1", func(ctx context.Context, b []byte) error {
		var t model.Trade
		if err := json.Unmarshal(b, &t); err != nil {
			return err
		}
		processed.Add(1)
		if t.IsTarget && t.Pool == nonsolsim.PoolNonSOL {
			nonsol.Add(1)
		}
		eng.HandleTrade(a.Ctx, t) // queues and returns; quotes run on per-coin workers
		return nil
	})
	if err != nil && a.Ctx.Err() == nil {
		slog.Error("subscribe", "err", err)
	}
}

func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}
