# Decisions

Choices made while building, so they can be revisited. Newest at the bottom of each section.

## Infra
- One Go module, one binary per service under `cmd/`, shared code in `internal/`. One Docker image holds every binary; compose picks the command. Faster builds, same code everywhere.
- Every service runs migrations on start (idempotent, tracked in `schema_migrations`), plus a one-shot `migrate` container that also seeds `wallets.txt`. Any service can come up first.
- `Store`, `Bus` and `Source` are interfaces with in-memory implementations, so tests and replay run the full pipeline without Docker or keys.
- Money and token amounts are stored as float64 (`DOUBLE PRECISION`). Fine for analytics at $25 sizes. The executor works in lamports and raw token units when it talks to the chain.
- `wallets.txt` is gitignored (your alpha list). `wallets.example.txt` documents the format. A missing file just skips seeding.
- Postgres and Redis ports bind to 127.0.0.1 only, so a VPS does not expose them.

## Ingest
- PumpPortal trade messages have no timestamp, so ingest stamps `received_at` when the frame arrives. Everything downstream uses that as event time. If PumpPortal ever adds a `timestamp`, the parser already reads it.
- Numbers are parsed leniently (number or numeric string) and non-trade frames (acks, errors) are logged and skipped.
- One goroutine owns the websocket and does every write. Subscribe/unsubscribe calls only update the desired sets and queue a command, so callers never block and never touch the socket.
- After any reconnect, all account and token subscriptions are re-sent in batches of 100.
- Backoff starts at 5s, doubles to 10 min, with jitter. A session that lasted 2+ min resets it. An error mentioning "ban" waits 1 hour, since bans expire hourly. Compose pins `replicas: 1`.
- Without `PUMPPORTAL_API_KEY`, ingest stays idle instead of connecting (the metered streams need a key) and health shows why.
- Tracked wallets = every wallet that is not `dropped` (live + bench).
- A token is subscribed when a tracked wallet buys it (or partially sells one we were not watching). It is unsubscribed once every tracked holder's `newTokenBalance` drops under 1 token, plus a cooldown (default 5 min, never below 90s so the +60s sim can fill), or after `TOKEN_MAX_WATCH` (24h) to cap metered usage.
- Trades are deduped by `signature|trader`, since a tracked wallet's trade arrives on both its account and its token subscription.
- The PumpPortal SOL usage estimate is metered messages / 10,000 * 0.01 SOL.
