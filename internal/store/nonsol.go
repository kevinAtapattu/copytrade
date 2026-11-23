package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"copytrade/internal/model"
)

// Paper copies on non-SOL coins (cmd/nonsolsim). Only on Postgres: they are
// not part of the Store interface, so nothing else reads or writes them.

const nonsolCols = `id, wallet, mint, their_sig, their_at, opened_at, closed_at, status, sol_usd, sol_in, fees_sol,
	tokens_bought, tokens_held, pending_sell, sol_out, sells, quote_route, skip_reason, last_error, mark_sol, marked_at, updated_at`

// SaveNonsolCopy inserts or replaces a copy by id.
func (p *Postgres) SaveNonsolCopy(ctx context.Context, c model.NonsolCopy) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO nonsol_copies (`+nonsolCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,now())
		ON CONFLICT (id) DO UPDATE SET closed_at=EXCLUDED.closed_at, status=EXCLUDED.status, sol_usd=EXCLUDED.sol_usd,
			sol_in=EXCLUDED.sol_in, fees_sol=EXCLUDED.fees_sol, tokens_bought=EXCLUDED.tokens_bought,
			tokens_held=EXCLUDED.tokens_held, pending_sell=EXCLUDED.pending_sell, sol_out=EXCLUDED.sol_out,
			sells=EXCLUDED.sells, quote_route=EXCLUDED.quote_route, skip_reason=EXCLUDED.skip_reason,
			last_error=EXCLUDED.last_error, mark_sol=EXCLUDED.mark_sol, marked_at=EXCLUDED.marked_at, updated_at=now()`,
		c.ID, c.Wallet, c.Mint, c.TheirSig, c.TheirAt, c.OpenedAt, c.ClosedAt, c.Status, c.SolUSD, c.SolIn, c.FeesSol,
		c.TokensBought, c.TokensHeld, c.PendingSell, c.SolOut, c.Sells, c.Route, c.SkipReason, c.LastError, c.MarkSol, c.MarkedAt)
	return err
}

// OpenNonsolCopies returns copies still holding tokens.
func (p *Postgres) OpenNonsolCopies(ctx context.Context) ([]model.NonsolCopy, error) {
	return p.queryNonsol(ctx, `SELECT `+nonsolCols+` FROM nonsol_copies WHERE status='open'`)
}

// ListNonsolCopies returns the newest copies first (all wallets, all statuses).
func (p *Postgres) ListNonsolCopies(ctx context.Context, limit int) ([]model.NonsolCopy, error) {
	return p.queryNonsol(ctx, `SELECT `+nonsolCols+` FROM nonsol_copies ORDER BY their_at DESC LIMIT $1`, limit)
}

func (p *Postgres) queryNonsol(ctx context.Context, q string, args ...any) ([]model.NonsolCopy, error) {
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (model.NonsolCopy, error) {
		var c model.NonsolCopy
		err := r.Scan(&c.ID, &c.Wallet, &c.Mint, &c.TheirSig, &c.TheirAt, &c.OpenedAt, &c.ClosedAt, &c.Status, &c.SolUSD,
			&c.SolIn, &c.FeesSol, &c.TokensBought, &c.TokensHeld, &c.PendingSell, &c.SolOut, &c.Sells, &c.Route,
			&c.SkipReason, &c.LastError, &c.MarkSol, &c.MarkedAt, &c.UpdatedAt)
		return c, err
	})
}
