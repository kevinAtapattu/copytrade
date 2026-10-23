// Command migrate applies SQL migrations and seeds wallets from WALLETS_FILE.
package main

import (
	"context"
	"log/slog"
	"os"

	"copytrade/internal/config"
	"copytrade/internal/seed"
	"copytrade/internal/store"
)

func main() {
	cfg := config.Load()
	ctx := context.Background()
	pg, err := store.OpenPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("connect", "err", err)
		os.Exit(1)
	}
	defer pg.Close()
	applied, err := pg.Migrate(ctx)
	if err != nil {
		slog.Error("migrate", "err", err)
		os.Exit(1)
	}
	slog.Info("migrations applied", "new", applied)
	n, err := seed.Wallets(ctx, pg, cfg.WalletsFile)
	if err != nil {
		slog.Error("seed wallets", "err", err)
		os.Exit(1)
	}
	slog.Info("wallets seeded", "added", n, "file", cfg.WalletsFile)
}
