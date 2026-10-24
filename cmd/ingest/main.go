// Command ingest holds the single PumpPortal websocket, persists trades and
// publishes them to the trades stream. In snapshot mode (default) it subscribes
// to tracked wallets only and reads token prices from the bonding curve over RPC.
package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"copytrade/internal/app"
	"copytrade/internal/ingest"
	"copytrade/internal/model"
	"copytrade/internal/snapshot"
	"copytrade/internal/solana"
	"copytrade/internal/source"
)

func main() {
	a := app.Start("ingest")
	defer a.Close()

	// PumpPortal: one connection. Wallet trades come from it only with
	// WALLET_SOURCE=pumpportal; otherwise it carries just the free migration stream.
	pp := source.NewPumpPortal(a.Cfg.PumpPortalWSURL, a.Cfg.PumpPortalAPIKey)
	var src source.Source = pp
	helius := a.Cfg.WalletSource == "helius"
	if helius {
		ws := a.Cfg.HeliusWSURL
		if ws == "" {
			ws = strings.Replace(a.Cfg.SolanaRPCURL, "https://", "wss://", 1)
		}
		lookup := solana.NewRPC(a.Cfg.SolanaRPCURL)
		lookup.RPS = 3 // post-trade balance reads, one per trade
		src = source.NewHelius(ws, lookup, a.Cfg.HeliusWSDailyMB)
	}
	in := ingest.New(src, a.Store, a.Bus, a.Cfg.TokenUnsubCooldown, a.Cfg.TokenMaxWatch)
	if a.Cfg.DiscoverySeedMigrations {
		pp.OnMigration = func(m source.Migration) {
			// Off the reader goroutine; migrations arrive a few per minute at most.
			go func() {
				ctx, cancel := context.WithTimeout(a.Ctx, 10*time.Second)
				defer cancel()
				if err := ingest.SeedMigration(ctx, a.Store, m); err != nil {
					slog.Warn("seed migrated token", "mint", m.Mint, "err", err)
				}
			}()
		}
	}
	in.Guard = ingest.Guard{DailyBudget: a.Cfg.MsgDailyBudget, WalletMaxPerHour: a.Cfg.WalletMaxPerHour, SpamPause: a.Cfg.WalletSpamPause}
	if helius {
		// Helius bills by data volume (capped by HELIUS_WS_DAILY_MB), not per message,
		// and PumpPortal carries no paid streams, so the message budget is off.
		in.Guard.DailyBudget = 0
		go func() {
			if err := pp.Run(a.Ctx, func(model.Trade) {}); err != nil && a.Ctx.Err() == nil {
				slog.Warn("pumpportal (migrations) stopped", "err", err)
			}
		}()
		slog.Info("wallet trades from Helius logsSubscribe; PumpPortal for migrations only")
	}
	var snap *snapshot.Snapshotter
	var rpc *solana.RPC
	switch a.Cfg.DataMode {
	case "snapshot":
		rpc = solana.NewRPC(a.Cfg.SolanaRPCURL)
		rpc.RPS = a.Cfg.RPCRPS
		snap = snapshot.New(snapshot.RPCReader{RPC: rpc}, a.Bus, a.Store, model.Delays, a.Cfg.MarkInterval)
		snap.SkipMarks = func() bool {
			today, _ := rpc.Calls()
			return a.Cfg.RPCDailyMax > 0 && today >= a.Cfg.RPCDailyMax
		}
		in.Tokens = snap
		in.Observer = snap.OnTrade
		in.Classify = snap.Classify
		go func() {
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-a.Ctx.Done():
					return
				case now := <-t.C:
					snap.Tick(a.Ctx, now)
				}
			}
		}()
		slog.Info("snapshot mode: prices from RPC curve reads", "rpc_rps", rpc.RPS, "mark_every", a.Cfg.MarkInterval)
	case "stream":
		slog.Warn("stream mode: subscribing to every trade on held tokens; this costs much more", "budget", a.Cfg.MsgDailyBudget)
	default:
		app.Fatal("DATA_MODE must be snapshot or stream", nil)
	}
	if path := os.Getenv("RECORD_FILE"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			app.Fatal("open RECORD_FILE", err)
		}
		defer f.Close()
		in.Recorder = f
		slog.Info("recording trades for replay", "file", path)
	}

	a.Heartbeat(10*time.Second, func() any {
		s := src.Stats()
		ps := pp.Stats()
		m := map[string]any{
			"wallet_source":        a.Cfg.WalletSource,
			"pumpportal_connected": ps.Connected,
			"pumpportal_messages":  ps.Messages,
			"bytes_today":          s.BytesToday,
			"helius_daily_mb":      a.Cfg.HeliusWSDailyMB,
			"connected":            s.Connected,
			"reconnects":           s.Reconnects,
			"last_message_at":      s.LastMessageAt,
			"last_error":           s.LastError,
			"accounts":             s.Accounts,
			"tokens":               s.Tokens,
			"messages":             s.Messages,
			"migrations":           ps.Migrations,
			"msgs_per_sec":         in.MsgsPerSec(time.Now()),
			"est_sol_used":         float64(ps.Messages) / 10000 * 0.01, // PumpPortal's paid streams only
			"has_api_key":          a.Cfg.PumpPortalAPIKey != "",
			"guard":                in.GuardStats(),
			"mode":                 a.Cfg.DataMode,
		}
		if snap != nil {
			today, _ := rpc.Calls()
			m["snapshot"] = snap.Stats()
			m["rpc_calls_today"] = today
			m["rpc_daily_max"] = a.Cfg.RPCDailyMax
		}
		return m
	})

	if err := in.Run(a.Ctx, a.Cfg.WalletReload); err != nil && a.Ctx.Err() == nil {
		app.Fatal("ingest", err)
	}
}
