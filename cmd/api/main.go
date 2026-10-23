// Command api serves REST + SSE for the dashboard.
package main

import (
	"log/slog"
	"time"

	"copytrade/internal/api"
	"copytrade/internal/app"
)

func main() {
	a := app.Start("api")
	defer a.Close()
	srv := api.New(a.Store, a.Bus, a.Redis, a.Cfg)
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
