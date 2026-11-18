// Command executor copies live wallets. PAPER mode unless LIVE_TRADING=true.
package main

import (
	"context"
	"encoding/json"
	"fmt"
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
		BuyMaxLag: c.ExecBuyMaxLag, DayLoc: dayLoc(c.AlertTZ),
	})
	if err := ex.Load(a.Ctx, time.Now()); err != nil {
		app.Fatal("load executor state", err)
	}
	slog.Info("executor ready", "mode", trader.Mode(), "open", ex.OpenCount())
	app.Emit(a.Ctx, a.Bus, "executor", "info", "executor started in "+trader.Mode()+" mode", nil)

	if l, ok := trader.(*exec.Live); ok {
		go watchBalance(a, ex, l)
	}
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
	// Manual sells from the dashboard. A request older than a minute (say, replayed
	// after a restart) is ignored: the price it was asked at is gone.
	go func() {
		err := a.Bus.Subscribe(a.Ctx, bus.StreamCommands, "executor", "exec-1", func(ctx context.Context, b []byte) error {
			var c model.Command
			if err := json.Unmarshal(b, &c); err != nil || c.Kind != model.CmdSellPosition {
				return nil
			}
			if time.Since(c.At) > time.Minute {
				slog.Warn("ignoring stale sell request", "position", c.PositionID, "asked_at", c.At)
				return nil
			}
			if err := ex.ManualSell(ctx, c.PositionID, c.Pct, time.Now()); err != nil {
				app.Emit(ctx, a.Bus, "executor", "error", "sell now failed: "+err.Error(), nil)
			}
			return nil
		})
		if err != nil && a.Ctx.Err() == nil {
			slog.Error("subscribe commands", "err", err)
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
		// Never open positions on stale buys (e.g. backlog after a restart). Stale
		// sells still go through so we never stay stuck in a position the target exited.
		if t.IsTarget && t.Side == model.SideBuy && time.Since(t.ReceivedAt) > 30*time.Second {
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
		return exec.Paper{Fees: curve.Fees{PumpPct: c.PumpFeePct, AMMPct: c.PumpSwapFeePct, BotPct: c.PumpPortalLocalFeePct,
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
		SlippagePct: c.ExecSlippagePct, PriorityFee: c.ExecPriorityFee, JitoTipSol: c.JitoTipSol, JitoURL: c.JitoURL, JupiterURL: c.JupiterURL,
		MaxAttempts: c.ExecMaxAttempts, HTTP: &http.Client{Timeout: 15 * time.Second}, ConfirmWait: 30 * time.Second}
}

// dayLoc is the timezone the trading day resets in (the alerts' ALERT_TZ).
func dayLoc(tz string) *time.Location {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		slog.Warn("bad ALERT_TZ, trading day resets at UTC midnight", "tz", tz, "err", err)
		return time.UTC
	}
	return loc
}

// watchBalance pauses buys while the wallet can't pay for one more buy plus the
// exits of everything open (a copy we can't afford to sell is stuck), and says so.
func watchBalance(a *app.App, ex *exec.Executor, l *exec.Live) {
	const (
		perTx = 0.001  // base + priority fee + Jito tip, with room
		rent  = 0.0025 // a new token account
	)
	c := a.Cfg
	px := price.New(a.Ctx, c.SolUSDFixed, c.SolUSDFallback)
	low := false
	check := func() {
		bal, err := l.RPC.GetBalance(a.Ctx, l.Key.PublicBase58())
		if err != nil {
			slog.Warn("wallet balance", "err", err)
			return
		}
		need := c.ExecSizeUSD/px.SOLUSD()*1.02 + rent + float64(ex.OpenCount()+1)*perTx + 2*perTx
		if bal >= need {
			if low {
				ex.SetLowBalance("")
				app.Emit(a.Ctx, a.Bus, "executor", "info", fmt.Sprintf("SOL balance back to %.4f SOL: buys resumed", bal), nil)
			}
			low = false
			return
		}
		why := fmt.Sprintf("low SOL balance: %.4f SOL, need %.4f to buy and still pay every exit", bal, need)
		ex.SetLowBalance(why)
		if !low {
			app.Emit(a.Ctx, a.Bus, "executor", "info", "[LIVE] "+why+". New buys paused; sells continue. Add SOL to "+l.Key.PublicBase58(), nil)
		}
		low = true
	}
	check()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-a.Ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
