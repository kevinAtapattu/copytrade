// Command executor copies live wallets. PAPER mode unless LIVE_TRADING=true.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"copytrade/internal/app"
	"copytrade/internal/bus"
	"copytrade/internal/curve"
	"copytrade/internal/exec"
	"copytrade/internal/model"
	"copytrade/internal/price"
	"copytrade/internal/solana"
)

func main() {
	a := app.Start("executor")
	defer a.Close()
	c := a.Cfg

	trader := buildTrader(c.LiveTrading, a)
	ex := exec.New(trader, a.Store, a.Bus, price.New(a.Ctx, c.SolUSDFixed, c.SolUSDFallback), exec.Risk{
		SizeUSD: c.ExecSizeUSD, MaxOpenPositions: c.MaxOpenPositions, StopLossPct: c.StopLossPct, DailyLossUSD: c.DailyLossLimitUSD,
	})
	if err := ex.Load(a.Ctx, time.Now()); err != nil {
		app.Fatal("load executor state", err)
	}
	slog.Info("executor ready", "mode", trader.Mode(), "open", ex.OpenCount())
	app.Emit(a.Ctx, a.Bus, "executor", "info", "executor started in "+trader.Mode()+" mode", nil)

	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-a.Ctx.Done():
				return
			case <-t.C:
				if err := ex.Refresh(a.Ctx); err != nil {
					slog.Warn("refresh", "err", err)
				}
			}
		}
	}()
	a.Heartbeat(10*time.Second, func() any {
		lag, _ := a.Redis.Lag(context.Background(), bus.StreamTrades, "executor")
		return map[string]any{"mode": trader.Mode(), "open_positions": ex.OpenCount(), "stream_lag": lag}
	})

	err := a.Bus.Subscribe(a.Ctx, bus.StreamTrades, "executor", "exec-1", func(ctx context.Context, b []byte) error {
		var t model.Trade
		if err := json.Unmarshal(b, &t); err != nil {
			return err
		}
		// Never act on stale trades (e.g. backlog after a restart).
		if time.Since(t.ReceivedAt) > 30*time.Second && t.IsTarget {
			t.IsTarget = false
		}
		ex.HandleTrade(ctx, t)
		return nil
	})
	if err != nil && a.Ctx.Err() == nil {
		slog.Error("subscribe", "err", err)
	}
}

func buildTrader(live bool, a *app.App) exec.Trader {
	c := a.Cfg
	if !live {
		return exec.Paper{Fees: curve.Fees{PumpPct: c.PumpFeePct, BotPct: c.PumpPortalLocalFeePct,
			NetworkSol: c.BaseFeeSol + c.ExecPriorityFee + c.JitoTipSol}}
	}
	if c.TraderPrivateKey == "" {
		app.Fatal("LIVE_TRADING=true but TRADER_PRIVATE_KEY is empty", nil)
	}
	kp, err := solana.ParseKeypair(c.TraderPrivateKey)
	if err != nil {
		app.Fatal("TRADER_PRIVATE_KEY", err)
	}
	slog.Warn("LIVE TRADING ENABLED", "wallet", kp.PublicBase58(), "size_usd", c.ExecSizeUSD)
	return &exec.Live{Key: kp, RPC: solana.NewRPC(c.SolanaRPCURL), TradeURL: c.PumpPortalTradeURL,
		SlippagePct: c.ExecSlippagePct, PriorityFee: c.ExecPriorityFee, JitoTipSol: c.JitoTipSol, JitoURL: c.JitoURL,
		MaxAttempts: c.ExecMaxAttempts, HTTP: &http.Client{Timeout: 15 * time.Second}, ConfirmWait: 30 * time.Second}
}
