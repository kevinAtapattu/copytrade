-- Paper copies on non-SOL coins, priced with Jupiter quotes (cmd/nonsolsim).
-- Separate from sim_positions so no SOL stat, score or alert sees them.
CREATE TABLE IF NOT EXISTS nonsol_copies (
    id            TEXT PRIMARY KEY,
    wallet        TEXT NOT NULL,
    mint          TEXT NOT NULL,
    their_sig     TEXT NOT NULL DEFAULT '',
    their_at      TIMESTAMPTZ NOT NULL,
    opened_at     TIMESTAMPTZ NOT NULL,
    closed_at     TIMESTAMPTZ,
    status        TEXT NOT NULL,
    sol_usd       DOUBLE PRECISION NOT NULL DEFAULT 0,
    sol_in        DOUBLE PRECISION NOT NULL DEFAULT 0,
    fees_sol      DOUBLE PRECISION NOT NULL DEFAULT 0,
    tokens_bought BIGINT NOT NULL DEFAULT 0,
    tokens_held   BIGINT NOT NULL DEFAULT 0,
    pending_sell  BIGINT NOT NULL DEFAULT 0,
    sol_out       DOUBLE PRECISION NOT NULL DEFAULT 0,
    sells         INT NOT NULL DEFAULT 0,
    quote_route   TEXT NOT NULL DEFAULT '',
    skip_reason   TEXT NOT NULL DEFAULT '',
    last_error    TEXT NOT NULL DEFAULT '',
    mark_sol      DOUBLE PRECISION NOT NULL DEFAULT 0,
    marked_at     TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS nonsol_copies_wallet_idx ON nonsol_copies (wallet, their_at);
CREATE UNIQUE INDEX IF NOT EXISTS nonsol_copies_open_idx ON nonsol_copies (wallet, mint) WHERE status = 'open';
