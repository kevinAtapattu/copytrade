package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"copytrade/internal/model"
	"copytrade/migrations"
)

type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects, retrying for a while so services can start before the DB is ready.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	var lastErr error
	for i := 0; i < 30; i++ {
		pool, err := pgxpool.New(ctx, url)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return &Postgres{pool: pool}, nil
			}
			pool.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("connect postgres: %w", lastErr)
}

func (p *Postgres) Close() { p.pool.Close() }

// Migrate applies embedded SQL files in name order, once each.
func (p *Postgres) Migrate(ctx context.Context) ([]string, error) {
	if _, err := p.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var applied []string
	for _, n := range names {
		var exists bool
		if err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, n).Scan(&exists); err != nil {
			return applied, err
		}
		if exists {
			continue
		}
		sqlBytes, err := migrations.FS.ReadFile(n)
		if err != nil {
			return applied, err
		}
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return applied, err
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("%s: %w", n, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, n); err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, err
		}
		applied = append(applied, n)
	}
	return applied, nil
}

// Reset truncates all data tables. Used by replay --db.
func (p *Postgres) Reset(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, `TRUNCATE wallets, token_trades, target_trades, tokens, sim_positions, sim_trades,
		scores, rotations, discovery_hits, live_positions, live_fills, settings, service_health RESTART IDENTITY`)
	return err
}

// ---- wallets

const walletCols = `address, label, status, source, reason, added_at, status_at, last_trade_at`

func scanWallet(r pgx.Row) (model.Wallet, error) {
	var w model.Wallet
	err := r.Scan(&w.Address, &w.Label, &w.Status, &w.Source, &w.Reason, &w.AddedAt, &w.StatusAt, &w.LastTradeAt)
	return w, err
}

func (p *Postgres) AddWallet(ctx context.Context, w model.Wallet) (bool, error) {
	if w.AddedAt.IsZero() {
		w.AddedAt = time.Now()
	}
	if w.StatusAt.IsZero() {
		w.StatusAt = w.AddedAt
	}
	tag, err := p.pool.Exec(ctx, `INSERT INTO wallets (address, label, status, source, reason, added_at, status_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (address) DO NOTHING`,
		w.Address, w.Label, w.Status, w.Source, w.Reason, w.AddedAt, w.StatusAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) ListWallets(ctx context.Context) ([]model.Wallet, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+walletCols+` FROM wallets ORDER BY address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Wallet
	for rows.Next() {
		w, err := scanWallet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (p *Postgres) GetWallet(ctx context.Context, addr string) (model.Wallet, bool, error) {
	w, err := scanWallet(p.pool.QueryRow(ctx, `SELECT `+walletCols+` FROM wallets WHERE address=$1`, addr))
	if errors.Is(err, pgx.ErrNoRows) {
		return w, false, nil
	}
	return w, err == nil, err
}

func (p *Postgres) SetWalletStatus(ctx context.Context, addr, status, reason string, at time.Time) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var from string
	err = tx.QueryRow(ctx, `SELECT status FROM wallets WHERE address=$1 FOR UPDATE`, addr).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if from == status {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE wallets SET status=$2, reason=$3, status_at=$4 WHERE address=$1`, addr, status, reason, at); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rotations (wallet, from_status, to_status, reason, at) VALUES ($1,$2,$3,$4,$5)`,
		addr, from, status, reason, at); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (p *Postgres) ListRotations(ctx context.Context, limit int) ([]model.Rotation, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, wallet, from_status, to_status, reason, at FROM rotations ORDER BY id DESC LIMIT $1`, lim(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Rotation
	for rows.Next() {
		var r model.Rotation
		if err := rows.Scan(&r.ID, &r.Wallet, &r.FromStatus, &r.ToStatus, &r.Reason, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- trades

func (p *Postgres) InsertTokenTrade(ctx context.Context, t model.Trade) (bool, error) {
	tag, err := p.pool.Exec(ctx, `INSERT INTO token_trades (signature, trader, mint, side, sol_amount, token_amount,
		new_token_balance, v_sol, v_tokens, market_cap_sol, pool, bonding_curve_key, received_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING`,
		t.Signature, t.Trader, t.Mint, t.Side, t.SolAmount, t.TokenAmount, t.NewTokenBalance, t.VSol, t.VTokens,
		t.MarketCapSol, t.Pool, t.BondingCurveKey, t.ReceivedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) InsertTargetTrade(ctx context.Context, t model.Trade) (bool, error) {
	tag, err := p.pool.Exec(ctx, `INSERT INTO target_trades (signature, wallet, mint, side, sol_amount, token_amount,
		new_token_balance, price, v_sol, v_tokens, pool, received_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT DO NOTHING`,
		t.Signature, t.Trader, t.Mint, t.Side, t.SolAmount, t.TokenAmount, t.NewTokenBalance, t.Price(), t.VSol, t.VTokens,
		t.Pool, t.ReceivedAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	_, err = p.pool.Exec(ctx, `UPDATE wallets SET last_trade_at=$2 WHERE address=$1 AND (last_trade_at IS NULL OR last_trade_at < $2)`,
		t.Trader, t.ReceivedAt)
	return true, err
}

func tradeWhere(f TradeFilter, walletCol string) (string, []any) {
	var conds []string
	var args []any
	add := func(c string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(c, len(args)))
	}
	if f.Wallet != "" {
		add(walletCol+"=$%d", f.Wallet)
	}
	if f.Mint != "" {
		add("mint=$%d", f.Mint)
	}
	if !f.Since.IsZero() {
		add("received_at>=$%d", f.Since)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, lim(f.Limit))
	return where + fmt.Sprintf(" ORDER BY received_at DESC LIMIT $%d", len(args)), args
}

func (p *Postgres) ListTargetTrades(ctx context.Context, f TradeFilter) ([]model.Trade, error) {
	where, args := tradeWhere(f, "wallet")
	rows, err := p.pool.Query(ctx, `SELECT signature, wallet, mint, side, sol_amount, token_amount, new_token_balance,
		v_sol, v_tokens, pool, received_at FROM target_trades`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Trade
	for rows.Next() {
		var t model.Trade
		if err := rows.Scan(&t.Signature, &t.Trader, &t.Mint, &t.Side, &t.SolAmount, &t.TokenAmount, &t.NewTokenBalance,
			&t.VSol, &t.VTokens, &t.Pool, &t.ReceivedAt); err != nil {
			return nil, err
		}
		t.IsTarget = true
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *Postgres) ListTokenTrades(ctx context.Context, f TradeFilter) ([]model.Trade, error) {
	where, args := tradeWhere(f, "trader")
	rows, err := p.pool.Query(ctx, `SELECT signature, trader, mint, side, sol_amount, token_amount, new_token_balance,
		v_sol, v_tokens, market_cap_sol, pool, bonding_curve_key, received_at FROM token_trades`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Trade
	for rows.Next() {
		var t model.Trade
		if err := rows.Scan(&t.Signature, &t.Trader, &t.Mint, &t.Side, &t.SolAmount, &t.TokenAmount, &t.NewTokenBalance,
			&t.VSol, &t.VTokens, &t.MarketCapSol, &t.Pool, &t.BondingCurveKey, &t.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *Postgres) CountTokenTradesSince(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM token_trades WHERE received_at >= $1`, since).Scan(&n)
	return n, err
}

// ---- tokens

const tokenCols = `mint, first_seen, first_price, max_price, last_price, v_sol, v_tokens, pool, last_trade_at, migrated, discovered`

func scanToken(r pgx.Row) (model.Token, error) {
	var t model.Token
	err := r.Scan(&t.Mint, &t.FirstSeen, &t.FirstPrice, &t.MaxPrice, &t.LastPrice, &t.VSol, &t.VTokens, &t.Pool,
		&t.LastTradeAt, &t.Migrated, &t.Discovered)
	return t, err
}

func (p *Postgres) UpsertTokenFromTrade(ctx context.Context, t model.Trade) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	cur, err := scanToken(tx.QueryRow(ctx, `SELECT `+tokenCols+` FROM tokens WHERE mint=$1 FOR UPDATE`, t.Mint))
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	n := ApplyTradeToToken(cur, exists, t)
	_, err = tx.Exec(ctx, `INSERT INTO tokens (`+tokenCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (mint) DO UPDATE SET first_price=EXCLUDED.first_price, max_price=EXCLUDED.max_price,
		last_price=EXCLUDED.last_price, v_sol=EXCLUDED.v_sol, v_tokens=EXCLUDED.v_tokens, pool=EXCLUDED.pool,
		last_trade_at=EXCLUDED.last_trade_at, migrated=EXCLUDED.migrated`,
		n.Mint, n.FirstSeen, n.FirstPrice, n.MaxPrice, n.LastPrice, n.VSol, n.VTokens, n.Pool, n.LastTradeAt, n.Migrated, n.Discovered)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) GetToken(ctx context.Context, mint string) (model.Token, bool, error) {
	t, err := scanToken(p.pool.QueryRow(ctx, `SELECT `+tokenCols+` FROM tokens WHERE mint=$1`, mint))
	if errors.Is(err, pgx.ErrNoRows) {
		return t, false, nil
	}
	return t, err == nil, err
}

func (p *Postgres) ListTokens(ctx context.Context, minMult float64, undiscovered bool) ([]model.Token, error) {
	q := `SELECT ` + tokenCols + ` FROM tokens WHERE ($1 <= 0 OR (first_price > 0 AND max_price/first_price >= $1))
		AND (NOT $2 OR NOT discovered) ORDER BY first_seen`
	rows, err := p.pool.Query(ctx, q, minMult, undiscovered)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *Postgres) MarkTokenDiscovered(ctx context.Context, mint string) error {
	_, err := p.pool.Exec(ctx, `UPDATE tokens SET discovered=true WHERE mint=$1`, mint)
	return err
}

// ---- simulation

const simTradeCols = `id, position_id, target_sig, wallet, mint, delay_s, side, target_at, fill_at, sol_amount,
	token_amount, spot_price, fill_price, target_price, slippage_pct, pump_fee_sol, bot_fee_sol, network_fee_sol,
	sol_usd, sell_fraction, skipped, skip_reason`

func (p *Postgres) InsertSimTrade(ctx context.Context, s model.SimTrade) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO sim_trades (`+simTradeCols+`) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22) ON CONFLICT DO NOTHING`,
		s.ID, s.PositionID, s.TargetSig, s.Wallet, s.Mint, s.DelayS, s.Side, s.TargetAt, s.FillAt, s.SolAmount,
		s.TokenAmount, s.SpotPrice, s.FillPrice, s.TargetPrice, s.SlippagePct, s.PumpFeeSol, s.BotFeeSol, s.NetworkFeeSol,
		s.SolUSD, s.SellFraction, s.Skipped, s.SkipReason)
	return err
}

func scanSimTrade(r pgx.Row) (model.SimTrade, error) {
	var s model.SimTrade
	err := r.Scan(&s.ID, &s.PositionID, &s.TargetSig, &s.Wallet, &s.Mint, &s.DelayS, &s.Side, &s.TargetAt, &s.FillAt,
		&s.SolAmount, &s.TokenAmount, &s.SpotPrice, &s.FillPrice, &s.TargetPrice, &s.SlippagePct, &s.PumpFeeSol,
		&s.BotFeeSol, &s.NetworkFeeSol, &s.SolUSD, &s.SellFraction, &s.Skipped, &s.SkipReason)
	return s, err
}

const simPosCols = `id, wallet, mint, delay_s, opened_at, closed_at, status, cost_sol, proceeds_sol, tokens_held, tokens_bought, sol_usd`

func (p *Postgres) UpsertSimPosition(ctx context.Context, x model.SimPosition) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO sim_positions (`+simPosCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (id) DO UPDATE SET closed_at=EXCLUDED.closed_at, status=EXCLUDED.status, cost_sol=EXCLUDED.cost_sol,
		proceeds_sol=EXCLUDED.proceeds_sol, tokens_held=EXCLUDED.tokens_held, tokens_bought=EXCLUDED.tokens_bought`,
		x.ID, x.Wallet, x.Mint, x.DelayS, x.OpenedAt, x.ClosedAt, x.Status, x.CostSol, x.ProceedsSol, x.TokensHeld,
		x.TokensBought, x.SolUSD)
	return err
}

func simWhere(f SimFilter, timeCol string) (string, []any) {
	var conds []string
	var args []any
	add := func(c string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(c, len(args)))
	}
	if f.Wallet != "" {
		add("wallet=$%d", f.Wallet)
	}
	if f.Mint != "" {
		add("mint=$%d", f.Mint)
	}
	if f.DelayS != 0 {
		add("delay_s=$%d", f.DelayS)
	}
	if !f.Since.IsZero() {
		add(timeCol+">=$%d", f.Since)
	}
	if f.OpenOnly {
		conds = append(conds, "status='open'")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	return where, args
}

func (p *Postgres) ListSimPositions(ctx context.Context, f SimFilter) ([]model.SimPosition, error) {
	where, args := simWhere(f, "opened_at")
	args = append(args, lim(f.Limit))
	rows, err := p.pool.Query(ctx, `SELECT `+simPosCols+` FROM sim_positions`+where+
		fmt.Sprintf(" ORDER BY opened_at, id LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SimPosition
	for rows.Next() {
		var x model.SimPosition
		if err := rows.Scan(&x.ID, &x.Wallet, &x.Mint, &x.DelayS, &x.OpenedAt, &x.ClosedAt, &x.Status, &x.CostSol,
			&x.ProceedsSol, &x.TokensHeld, &x.TokensBought, &x.SolUSD); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (p *Postgres) ListSimTrades(ctx context.Context, f SimFilter) ([]model.SimTrade, error) {
	where, args := simWhere(SimFilter{Wallet: f.Wallet, Mint: f.Mint, DelayS: f.DelayS, Since: f.Since}, "fill_at")
	args = append(args, lim(f.Limit))
	rows, err := p.pool.Query(ctx, `SELECT `+simTradeCols+` FROM sim_trades`+where+
		fmt.Sprintf(" ORDER BY fill_at DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SimTrade
	for rows.Next() {
		s, err := scanSimTrade(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *Postgres) SimTradesForTarget(ctx context.Context, sig string) ([]model.SimTrade, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+simTradeCols+` FROM sim_trades WHERE target_sig=$1 ORDER BY delay_s`, sig)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SimTrade
	for rows.Next() {
		s, err := scanSimTrade(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- scores

const scoreCols = `wallet, delay_s, window_days, computed_at, positions, closed, pnl_usd, pnl_ex_best_usd, best_trade_usd,
	win_rate, max_drawdown, avg_hold_s, trades_per_day, active_days, lucky_trade, score`

func (p *Postgres) InsertScores(ctx context.Context, ss []model.Score) error {
	if len(ss) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, s := range ss {
		b.Queue(`INSERT INTO scores (`+scoreCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT DO NOTHING`,
			s.Wallet, s.DelayS, s.WindowDays, s.ComputedAt, s.Positions, s.ClosedCount, s.PnlUSD, s.PnlExBestUSD,
			s.BestTradeUSD, s.WinRate, s.MaxDrawdown, s.AvgHoldS, s.TradesPerDay, s.ActiveDays, s.LuckyTrade, s.Score)
	}
	return p.pool.SendBatch(ctx, b).Close()
}

func scanScores(rows pgx.Rows) ([]model.Score, error) {
	defer rows.Close()
	var out []model.Score
	for rows.Next() {
		var s model.Score
		if err := rows.Scan(&s.Wallet, &s.DelayS, &s.WindowDays, &s.ComputedAt, &s.Positions, &s.ClosedCount, &s.PnlUSD,
			&s.PnlExBestUSD, &s.BestTradeUSD, &s.WinRate, &s.MaxDrawdown, &s.AvgHoldS, &s.TradesPerDay, &s.ActiveDays,
			&s.LuckyTrade, &s.Score); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *Postgres) LatestScores(ctx context.Context) ([]model.Score, error) {
	rows, err := p.pool.Query(ctx, `SELECT DISTINCT ON (wallet, delay_s, window_days) `+scoreCols+`
		FROM scores ORDER BY wallet, delay_s, window_days, computed_at DESC`)
	if err != nil {
		return nil, err
	}
	return scanScores(rows)
}

func (p *Postgres) ScoreHistory(ctx context.Context, wallet string, delayS, window int, since time.Time) ([]model.Score, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+scoreCols+` FROM scores WHERE wallet=$1 AND delay_s=$2 AND window_days=$3
		AND computed_at >= $4 ORDER BY computed_at`, wallet, delayS, window, since)
	if err != nil {
		return nil, err
	}
	return scanScores(rows)
}

// ---- discovery

func (p *Postgres) InsertDiscoveryHit(ctx context.Context, h model.DiscoveryHit) (bool, error) {
	tag, err := p.pool.Exec(ctx, `INSERT INTO discovery_hits (wallet, mint, token_mult, entry_price, exit_price, exit_mult,
		profit_sol, found_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`,
		h.Wallet, h.Mint, h.TokenMult, h.EntryPrice, h.ExitPrice, h.ExitMult, h.ProfitSol, h.FoundAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) ListDiscoveryHits(ctx context.Context, limit int) ([]model.DiscoveryHit, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, wallet, mint, token_mult, entry_price, exit_price, exit_mult, profit_sol, found_at
		FROM discovery_hits ORDER BY id DESC LIMIT $1`, lim(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.DiscoveryHit
	for rows.Next() {
		var h model.DiscoveryHit
		if err := rows.Scan(&h.ID, &h.Wallet, &h.Mint, &h.TokenMult, &h.EntryPrice, &h.ExitPrice, &h.ExitMult,
			&h.ProfitSol, &h.FoundAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---- executor

const liveFillCols = `id, position_id, mode, target_sig, wallet, mint, side, reason, target_at, sent_at, filled_at, signature,
	status, error, attempts, sol_amount, token_amount, fill_price, fees_sol, sol_usd, sim_fill_price`

func (p *Postgres) InsertLiveFill(ctx context.Context, f model.LiveFill) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO live_fills (`+liveFillCols+`) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) ON CONFLICT DO NOTHING`,
		f.ID, f.PositionID, f.Mode, f.TargetSig, f.Wallet, f.Mint, f.Side, f.Reason, f.TargetAt, f.SentAt, f.FilledAt,
		f.Signature, f.Status, f.Error, f.Attempts, f.SolAmount, f.TokenAmount, f.FillPrice, f.FeesSol, f.SolUSD, f.SimFillPrice)
	return err
}

func liveWhere(f LiveFilter, timeCol string) (string, []any) {
	var conds []string
	var args []any
	add := func(c string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(c, len(args)))
	}
	if f.Mode != "" {
		add("mode=$%d", f.Mode)
	}
	if f.Wallet != "" {
		add("wallet=$%d", f.Wallet)
	}
	if !f.Since.IsZero() {
		add(timeCol+">=$%d", f.Since)
	}
	if f.OpenOnly {
		conds = append(conds, "status='open'")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	return where, args
}

func (p *Postgres) ListLiveFills(ctx context.Context, f LiveFilter) ([]model.LiveFill, error) {
	where, args := liveWhere(LiveFilter{Mode: f.Mode, Wallet: f.Wallet, Since: f.Since}, "sent_at")
	args = append(args, lim(f.Limit))
	rows, err := p.pool.Query(ctx, `SELECT `+liveFillCols+` FROM live_fills`+where+
		fmt.Sprintf(" ORDER BY sent_at DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LiveFill
	for rows.Next() {
		var x model.LiveFill
		if err := rows.Scan(&x.ID, &x.PositionID, &x.Mode, &x.TargetSig, &x.Wallet, &x.Mint, &x.Side, &x.Reason, &x.TargetAt,
			&x.SentAt, &x.FilledAt, &x.Signature, &x.Status, &x.Error, &x.Attempts, &x.SolAmount, &x.TokenAmount,
			&x.FillPrice, &x.FeesSol, &x.SolUSD, &x.SimFillPrice); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

const livePosCols = `id, mode, wallet, mint, opened_at, closed_at, status, cost_sol, proceeds_sol, tokens_held, entry_price, sol_usd`

func (p *Postgres) UpsertLivePosition(ctx context.Context, x model.LivePosition) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO live_positions (`+livePosCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (id) DO UPDATE SET closed_at=EXCLUDED.closed_at, status=EXCLUDED.status, cost_sol=EXCLUDED.cost_sol,
		proceeds_sol=EXCLUDED.proceeds_sol, tokens_held=EXCLUDED.tokens_held`,
		x.ID, x.Mode, x.Wallet, x.Mint, x.OpenedAt, x.ClosedAt, x.Status, x.CostSol, x.ProceedsSol, x.TokensHeld,
		x.EntryPrice, x.SolUSD)
	return err
}

func (p *Postgres) ListLivePositions(ctx context.Context, f LiveFilter) ([]model.LivePosition, error) {
	where, args := liveWhere(f, "opened_at")
	args = append(args, lim(f.Limit))
	rows, err := p.pool.Query(ctx, `SELECT `+livePosCols+` FROM live_positions`+where+
		fmt.Sprintf(" ORDER BY opened_at LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LivePosition
	for rows.Next() {
		var x model.LivePosition
		if err := rows.Scan(&x.ID, &x.Mode, &x.Wallet, &x.Mint, &x.OpenedAt, &x.ClosedAt, &x.Status, &x.CostSol,
			&x.ProceedsSol, &x.TokensHeld, &x.EntryPrice, &x.SolUSD); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---- settings + health

func (p *Postgres) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := p.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (p *Postgres) SetSetting(ctx context.Context, key, value string) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ($1,$2,now())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, key, value)
	return err
}

func (p *Postgres) PutHealth(ctx context.Context, service string, data json.RawMessage, at time.Time) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO service_health (service, data, updated_at) VALUES ($1,$2,$3)
		ON CONFLICT (service) DO UPDATE SET data=EXCLUDED.data, updated_at=EXCLUDED.updated_at`, service, []byte(data), at)
	return err
}

func (p *Postgres) ListHealth(ctx context.Context) (map[string]Health, error) {
	rows, err := p.pool.Query(ctx, `SELECT service, data, updated_at FROM service_health`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Health{}
	for rows.Next() {
		var s string
		var d []byte
		var at time.Time
		if err := rows.Scan(&s, &d, &at); err != nil {
			return nil, err
		}
		out[s] = Health{Data: d, UpdatedAt: at}
	}
	return out, rows.Err()
}

func lim(n int) int {
	if n <= 0 || n > 100000 {
		return 100000
	}
	return n
}
