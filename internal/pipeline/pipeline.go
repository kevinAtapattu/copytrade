// Package pipeline runs every component in one process on an in-memory bus, driven
// by a replay source in event time. Used by `cmd/replay` and the end-to-end test.
package pipeline

import (
	"context"
	"encoding/json"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/config"
	"copytrade/internal/curve"
	"copytrade/internal/exec"
	"copytrade/internal/ingest"
	"copytrade/internal/jobs"
	"copytrade/internal/model"
	"copytrade/internal/price"
	"copytrade/internal/sim"
	"copytrade/internal/snapshot"
	"copytrade/internal/source"
	"copytrade/internal/store"
)

type Options struct {
	// Mode "stream" (default) replays the full trade stream; "snapshot" delivers only
	// tracked wallets' trades and prices them from the recorded history via the
	// snapshotter, exactly as production snapshot mode reads the chain.
	Mode     string
	JobEvery time.Duration // event-time interval between scorer/discovery runs
	SolUSD   float64
}

type Report struct {
	Messages     int
	Delivered    int64
	TargetTrades int
	SimTrades    int
	Events       map[string]int
	JobRuns      int
	Start, End   time.Time
}

// Run replays src through ingest -> bus -> simulator + executor, running the
// periodic jobs on event time. The store must already hold the tracked wallets.
func Run(ctx context.Context, c config.Config, st store.Store, src *source.Replay, opt Options) (Report, error) {
	if opt.JobEvery == 0 {
		opt.JobEvery = 6 * time.Hour
	}
	if opt.SolUSD == 0 {
		opt.SolUSD = 150
	}
	rep := Report{Events: map[string]int{}, Messages: src.Len()}
	b := bus.NewMemory()
	px := price.Fixed(opt.SolUSD)

	in := ingest.New(src, st, b, c.TokenUnsubCooldown, c.TokenMaxWatch)
	in.Guard = ingest.Guard{DailyBudget: c.MsgDailyBudget, WalletMaxPerHour: c.WalletMaxPerHour}
	var snap *snapshot.Snapshotter
	if opt.Mode == "snapshot" {
		snap = snapshot.New(snapshot.NewHistoryReader(src.Trades()), b, st, model.Delays, c.MarkInterval)
		in.Tokens = snap
		in.Observer = snap.OnTrade
	}
	eng := sim.New(sim.Params{SizeUSD: c.CopySizeUSD, Delays: model.Delays, Fees: sim.FeesFrom(c)}, st, px)
	ex := exec.New(exec.Paper{Fees: curve.Fees{PumpPct: c.PumpFeePct, BotPct: c.PumpPortalLocalFeePct,
		NetworkSol: c.BaseFeeSol + c.ExecPriorityFee}}, st, b, px,
		exec.Risk{SizeUSD: c.ExecSizeUSD, MaxOpenPositions: c.MaxOpenPositions, StopLossPct: c.StopLossPct, DailyLossUSD: c.DailyLossLimitUSD})
	ex.Sync = true

	b.Register(bus.StreamTrades, func(ctx context.Context, p []byte) error {
		var t model.Trade
		if err := json.Unmarshal(p, &t); err != nil {
			return err
		}
		eng.HandleTrade(ctx, t)
		ex.HandleTrade(ctx, t)
		return nil
	})
	b.Register(bus.StreamEvents, func(_ context.Context, p []byte) error {
		var ev model.Event
		if json.Unmarshal(p, &ev) == nil {
			rep.Events[ev.Kind]++
		}
		return nil
	})

	if err := in.ReloadWallets(ctx); err != nil {
		return rep, err
	}
	if err := ex.Load(ctx, time.Time{}); err != nil {
		return rep, err
	}

	var nextJob time.Time
	var runErr error
	runJobs := func(now time.Time) {
		if _, _, err := jobs.ScoreAndRotate(ctx, st, b, c, now); err != nil && runErr == nil {
			runErr = err
		}
		if _, err := jobs.Discover(ctx, st, b, c, now); err != nil && runErr == nil {
			runErr = err
		}
		if err := in.ReloadWallets(ctx); err != nil && runErr == nil {
			runErr = err
		}
		if err := ex.Refresh(ctx); err != nil && runErr == nil {
			runErr = err
		}
		rep.JobRuns++
	}
	src.AfterEach = func(at time.Time) {
		if rep.Start.IsZero() {
			rep.Start = at
			nextJob = at.Add(opt.JobEvery)
		}
		rep.End = at
		in.Tick(at)
		if snap != nil {
			snap.Tick(ctx, at)
		}
		eng.Tick(ctx, at)
		for !at.Before(nextJob) {
			runJobs(nextJob)
			nextJob = nextJob.Add(opt.JobEvery)
		}
	}
	if err := src.Run(ctx, func(t model.Trade) { in.HandleTrade(ctx, t) }); err != nil {
		return rep, err
	}
	eng.Flush(ctx)
	runJobs(rep.End.Add(time.Minute))
	rep.Delivered = src.Stats().Messages

	tt, err := st.ListTargetTrades(ctx, store.TradeFilter{})
	if err != nil {
		return rep, err
	}
	rep.TargetTrades = len(tt)
	stt, err := st.ListSimTrades(ctx, store.SimFilter{})
	if err != nil {
		return rep, err
	}
	rep.SimTrades = len(stt)
	return rep, runErr
}
