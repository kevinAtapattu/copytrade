package ingest

import (
	"context"
	"log/slog"

	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/source"
	"copytrade/internal/store"
)

// SeedMigration records a token that completed its bonding curve as a discovery
// candidate. Its first and max prices are the curve's launch and completion
// prices, so "early" is judged against the real launch.
func SeedMigration(ctx context.Context, st store.Store, m source.Migration) error {
	key, err := curve.DeriveBondingCurve(m.Mint)
	if err != nil {
		return err
	}
	pool := m.Pool
	if pool == "" {
		pool = "pump-amm"
	}
	added, err := st.SeedMigratedToken(ctx, model.Token{
		Mint: m.Mint, FirstSeen: m.At, FirstPrice: curve.LaunchPrice, MaxPrice: curve.CompletePrice,
		LastPrice: curve.CompletePrice, Pool: pool, LastTradeAt: m.At, CurveKey: key,
	})
	if err == nil && added {
		slog.Info("migrated token queued for discovery", "mint", m.Mint)
	}
	return err
}
