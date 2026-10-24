// Command simulator consumes the trades stream and paper-copies every target
// trade at +2s/+10s/+60s.
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
	"copytrade/internal/price"
	"copytrade/internal/sim"
)

func main() {
	a := app.Start("simulator")
	defer a.Close()
	c := a.Cfg

	eng := sim.New(sim.Params{
		SizeUSD: c.CopySizeUSD,
		Delays:  model.Delays,
		Fees:    sim.FeesFrom(c),
		Grace:   3 * time.Second,
	}, a.Store, price.New(a.Ctx, c.SolUSDFixed, c.SolUSDFallback))
	eng.Sink = func(s model.SimTrade, p model.SimPosition) {
		if !s.Skipped {
			app.Emit(a.Ctx, a.Bus, "simulator", "sim_fill", "", s)
		}
		if p.ID != "" && p.Status != model.PosOpen {
			// A paper copy finished (closed or migrated); the alerter rolls these up.
			app.Emit(a.Ctx, a.Bus, "simulator", "copy_closed", "", p)
		}
	}
	if err := eng.LoadOpen(a.Ctx); err != nil {
		app.Fatal("load open positions", err)
	}

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-a.Ctx.Done():
				return
			case <-t.C:
				eng.TickWall(a.Ctx)
			}
		}
	}()

	var processed atomic.Int64
	var lastAt atomic.Value
	lastAt.Store(time.Time{})
	a.Heartbeat(10*time.Second, func() any {
		lag, _ := a.Redis.Lag(context.Background(), bus.StreamTrades, "simulator")
		return map[string]any{"processed": processed.Load(), "pending_fills": eng.PendingCount(),
			"open_positions": eng.OpenCount(), "stream_lag": lag, "last_trade_at": lastAt.Load()}
	})

	err := a.Bus.Subscribe(a.Ctx, bus.StreamTrades, "simulator", "sim-1", func(ctx context.Context, b []byte) error {
		var t model.Trade
		if err := json.Unmarshal(b, &t); err != nil {
			return err
		}
		eng.HandleTrade(ctx, t)
		processed.Add(1)
		lastAt.Store(t.ReceivedAt)
		return nil
	})
	if err != nil && a.Ctx.Err() == nil {
		slog.Error("subscribe", "err", err)
	}
}
