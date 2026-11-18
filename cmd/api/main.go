// Command api serves REST + SSE for the dashboard.
package main

import (
	"context"
	"log/slog"
	"time"
	_ "time/tzdata" // ALERT_TZ for the overview's "today" in a slim container

	"copytrade/internal/api"
	"copytrade/internal/app"
	"copytrade/internal/price"
	"copytrade/internal/solana"
)

func main() {
	a := app.Start("api")
	defer a.Close()
	srv := api.New(a.Store, a.Bus, a.Redis, a.Cfg)
	// The overview's real-money view shows the trading wallet's balance. Only the
	// public address is kept; the secret key is dropped here.
	if kp, err := solana.ParseKeypair(a.Cfg.TraderPrivateKey); err == nil && a.Cfg.TraderPrivateKey != "" {
		addr, rpc := kp.PublicBase58(), solana.NewRPC(a.Cfg.SolanaRPCURL)
		srv.Wallet = func(ctx context.Context) (string, float64, error) {
			bal, err := rpc.GetBalance(ctx, addr)
			return addr, bal, err
		}
		px := price.New(a.Ctx, a.Cfg.SolUSDFixed, a.Cfg.SolUSDFallback)
		srv.SolUSD = px.SOLUSD
	}
	go func() {
		if err := srv.RunHub(a.Ctx); err != nil && a.Ctx.Err() == nil {
			slog.Error("event hub", "err", err)
		}
	}()
	a.Heartbeat(30*time.Second, func() any { return map[string]any{"addr": a.Cfg.APIAddr} })
	if err := srv.Serve(a.Ctx, a.Cfg.APIAddr); err != nil {
		app.Fatal("serve", err)
	}
}
