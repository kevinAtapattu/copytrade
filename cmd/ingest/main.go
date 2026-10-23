// Command ingest holds the single PumpPortal websocket, persists trades and
// publishes them to the trades stream.
package main

import (
	"time"

	"copytrade/internal/app"
	"copytrade/internal/ingest"
	"copytrade/internal/source"
)

func main() {
	a := app.Start("ingest")
	defer a.Close()

	src := source.NewPumpPortal(a.Cfg.PumpPortalWSURL, a.Cfg.PumpPortalAPIKey)
	in := ingest.New(src, a.Store, a.Bus, a.Cfg.TokenUnsubCooldown, a.Cfg.TokenMaxWatch)

	a.Heartbeat(10*time.Second, func() any {
		s := src.Stats()
		return map[string]any{
			"connected":       s.Connected,
			"reconnects":      s.Reconnects,
			"last_message_at": s.LastMessageAt,
			"last_error":      s.LastError,
			"accounts":        s.Accounts,
			"tokens":          s.Tokens,
			"messages":        s.Messages,
			"msgs_per_sec":    in.MsgsPerSec(time.Now()),
			"est_sol_used":    float64(s.Messages) / 10000 * 0.01,
			"has_api_key":     a.Cfg.PumpPortalAPIKey != "",
		}
	})

	if err := in.Run(a.Ctx, a.Cfg.WalletReload); err != nil && a.Ctx.Err() == nil {
		app.Fatal("ingest", err)
	}
}
