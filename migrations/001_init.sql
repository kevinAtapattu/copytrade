-- Core schema.

CREATE TABLE IF NOT EXISTS wallets (
    address       TEXT PRIMARY KEY,
    label         TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'bench',
    source        TEXT NOT NULL DEFAULT 'seed',
    reason        TEXT NOT NULL DEFAULT '',
    added_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    status_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_trade_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS wallets_status_idx ON wallets (status);

-- Every trade we receive (target wallets and everyone else on watched tokens).
CREATE TABLE IF NOT EXISTS token_trades (
    signature         TEXT NOT NULL,
    trader            TEXT NOT NULL,
    mint              TEXT NOT NULL,
    side              TEXT NOT NULL,
    sol_amount        DOUBLE PRECISION NOT NULL,
    token_amount      DOUBLE PRECISION NOT NULL,
    new_token_balance DOUBLE PRECISION NOT NULL,
    v_sol             DOUBLE PRECISION NOT NULL,
    v_tokens          DOUBLE PRECISION NOT NULL,
    market_cap_sol    DOUBLE PRECISION NOT NULL,
    pool              TEXT NOT NULL DEFAULT 'pump',
    bonding_curve_key TEXT NOT NULL DEFAULT '',
    received_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (signature, trader)
);
CREATE INDEX IF NOT EXISTS token_trades_mint_idx ON token_trades (mint, received_at);
CREATE INDEX IF NOT EXISTS token_trades_received_idx ON token_trades (received_at);

-- Trades made by tracked wallets.
CREATE TABLE IF NOT EXISTS target_trades (
    signature         TEXT NOT NULL,
    wallet            TEXT NOT NULL,
    mint              TEXT NOT NULL,
    side              TEXT NOT NULL,
    sol_amount        DOUBLE PRECISION NOT NULL,
    token_amount      DOUBLE PRECISION NOT NULL,
    new_token_balance DOUBLE PRECISION NOT NULL,
    price             DOUBLE PRECISION NOT NULL,
    v_sol             DOUBLE PRECISION NOT NULL,
    v_tokens          DOUBLE PRECISION NOT NULL,
    pool              TEXT NOT NULL DEFAULT 'pump',
    received_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (signature, wallet)
);
CREATE INDEX IF NOT EXISTS target_trades_wallet_idx ON target_trades (wallet, received_at);
CREATE INDEX IF NOT EXISTS target_trades_received_idx ON target_trades (received_at);

CREATE TABLE IF NOT EXISTS tokens (
    mint          TEXT PRIMARY KEY,
    first_seen    TIMESTAMPTZ NOT NULL,
    first_price   DOUBLE PRECISION NOT NULL,
    max_price     DOUBLE PRECISION NOT NULL,
    last_price    DOUBLE PRECISION NOT NULL,
    v_sol         DOUBLE PRECISION NOT NULL,
    v_tokens      DOUBLE PRECISION NOT NULL,
    pool          TEXT NOT NULL DEFAULT 'pump',
    last_trade_at TIMESTAMPTZ NOT NULL,
    migrated      BOOLEAN NOT NULL DEFAULT false,
    discovered    BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS sim_positions (
    id            TEXT PRIMARY KEY,
    wallet        TEXT NOT NULL,
    mint          TEXT NOT NULL,
    delay_s       INT NOT NULL,
    opened_at     TIMESTAMPTZ NOT NULL,
    closed_at     TIMESTAMPTZ,
    status        TEXT NOT NULL,
    cost_sol      DOUBLE PRECISION NOT NULL,
    proceeds_sol  DOUBLE PRECISION NOT NULL,
    tokens_held   DOUBLE PRECISION NOT NULL,
    tokens_bought DOUBLE PRECISION NOT NULL,
    sol_usd       DOUBLE PRECISION NOT NULL
);
CREATE INDEX IF NOT EXISTS sim_positions_wallet_idx ON sim_positions (wallet, delay_s, opened_at);
CREATE INDEX IF NOT EXISTS sim_positions_status_idx ON sim_positions (status);

CREATE TABLE IF NOT EXISTS sim_trades (
    id              TEXT PRIMARY KEY,
    position_id     TEXT NOT NULL DEFAULT '',
    target_sig      TEXT NOT NULL,
    wallet          TEXT NOT NULL,
    mint            TEXT NOT NULL,
    delay_s         INT NOT NULL,
    side            TEXT NOT NULL,
    target_at       TIMESTAMPTZ NOT NULL,
    fill_at         TIMESTAMPTZ NOT NULL,
    sol_amount      DOUBLE PRECISION NOT NULL,
    token_amount    DOUBLE PRECISION NOT NULL,
    spot_price      DOUBLE PRECISION NOT NULL,
    fill_price      DOUBLE PRECISION NOT NULL,
    target_price    DOUBLE PRECISION NOT NULL,
    slippage_pct    DOUBLE PRECISION NOT NULL,
    pump_fee_sol    DOUBLE PRECISION NOT NULL,
    bot_fee_sol     DOUBLE PRECISION NOT NULL,
    network_fee_sol DOUBLE PRECISION NOT NULL,
    sol_usd         DOUBLE PRECISION NOT NULL,
    sell_fraction   DOUBLE PRECISION NOT NULL DEFAULT 0,
    skipped         BOOLEAN NOT NULL DEFAULT false,
    skip_reason     TEXT NOT NULL DEFAULT '',
    UNIQUE (target_sig, wallet, delay_s)
);
CREATE INDEX IF NOT EXISTS sim_trades_wallet_idx ON sim_trades (wallet, delay_s, fill_at);
CREATE INDEX IF NOT EXISTS sim_trades_fill_idx ON sim_trades (fill_at);

CREATE TABLE IF NOT EXISTS scores (
    wallet          TEXT NOT NULL,
    delay_s         INT NOT NULL,
    window_days     INT NOT NULL,
    computed_at     TIMESTAMPTZ NOT NULL,
    positions       INT NOT NULL,
    closed          INT NOT NULL,
    pnl_usd         DOUBLE PRECISION NOT NULL,
    pnl_ex_best_usd DOUBLE PRECISION NOT NULL,
    best_trade_usd  DOUBLE PRECISION NOT NULL,
    win_rate        DOUBLE PRECISION NOT NULL,
    max_drawdown    DOUBLE PRECISION NOT NULL,
    avg_hold_s      DOUBLE PRECISION NOT NULL,
    trades_per_day  DOUBLE PRECISION NOT NULL,
    active_days     DOUBLE PRECISION NOT NULL,
    lucky_trade     BOOLEAN NOT NULL,
    score           DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (wallet, delay_s, window_days, computed_at)
);
CREATE INDEX IF NOT EXISTS scores_computed_idx ON scores (computed_at);

CREATE TABLE IF NOT EXISTS rotations (
    id          BIGSERIAL PRIMARY KEY,
    wallet      TEXT NOT NULL,
    from_status TEXT NOT NULL,
    to_status   TEXT NOT NULL,
    reason      TEXT NOT NULL,
    at          TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS discovery_hits (
    id          BIGSERIAL PRIMARY KEY,
    wallet      TEXT NOT NULL,
    mint        TEXT NOT NULL,
    token_mult  DOUBLE PRECISION NOT NULL,
    entry_price DOUBLE PRECISION NOT NULL,
    exit_price  DOUBLE PRECISION NOT NULL,
    exit_mult   DOUBLE PRECISION NOT NULL,
    profit_sol  DOUBLE PRECISION NOT NULL,
    found_at    TIMESTAMPTZ NOT NULL,
    UNIQUE (wallet, mint)
);

CREATE TABLE IF NOT EXISTS live_positions (
    id           TEXT PRIMARY KEY,
    mode         TEXT NOT NULL,
    wallet       TEXT NOT NULL,
    mint         TEXT NOT NULL,
    opened_at    TIMESTAMPTZ NOT NULL,
    closed_at    TIMESTAMPTZ,
    status       TEXT NOT NULL,
    cost_sol     DOUBLE PRECISION NOT NULL,
    proceeds_sol DOUBLE PRECISION NOT NULL,
    tokens_held  DOUBLE PRECISION NOT NULL,
    entry_price  DOUBLE PRECISION NOT NULL,
    sol_usd      DOUBLE PRECISION NOT NULL
);

CREATE TABLE IF NOT EXISTS live_fills (
    id             TEXT PRIMARY KEY,
    position_id    TEXT NOT NULL DEFAULT '',
    mode           TEXT NOT NULL,
    target_sig     TEXT NOT NULL,
    wallet         TEXT NOT NULL,
    mint           TEXT NOT NULL,
    side           TEXT NOT NULL,
    reason         TEXT NOT NULL,
    target_at      TIMESTAMPTZ NOT NULL,
    sent_at        TIMESTAMPTZ NOT NULL,
    filled_at      TIMESTAMPTZ,
    signature      TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL,
    error          TEXT NOT NULL DEFAULT '',
    attempts       INT NOT NULL DEFAULT 0,
    sol_amount     DOUBLE PRECISION NOT NULL,
    token_amount   DOUBLE PRECISION NOT NULL,
    fill_price     DOUBLE PRECISION NOT NULL,
    fees_sol       DOUBLE PRECISION NOT NULL,
    sol_usd        DOUBLE PRECISION NOT NULL,
    sim_fill_price DOUBLE PRECISION NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS live_fills_sent_idx ON live_fills (sent_at);

CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS service_health (
    service    TEXT PRIMARY KEY,
    data       JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
