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

## Simulator
- Event time, not sleeps. A fill due at T+d uses the curve state after the last trade at or before T+d. It is finalized when a later trade on that mint arrives, or by a 1s ticker once T+d+3s has passed (3s of grace for late frames). Replay finalizes from event time, then flushes at the end.
- The target's own trade carries post-trade reserves, so it seeds the curve state. That covers the gap before our token subscription is live: if nobody trades before T+d, we fill at the target's post-trade price.
- Curve math is constant product on PumpPortal's virtual reserves. Buys that would push virtual tokens below 279.9M (curve complete) are skipped.
- Fees, all configurable:
  - pump.fun curve fee: 1.25% by default (0.95% protocol + 0.30% creator, per 2026 sources). Charged on top of the SOL that enters the curve on buys, and taken from the SOL that leaves it on sells.
  - Bot fee: 1% when `BOT_FEE_ENABLED=true`, on the same SOL side.
  - Network: base fee 0.000005 plus `PRIORITY_FEE_SOL` (0.0005) per tx.
- $25 converts to SOL at the live CoinGecko SOL/USD (refreshed every minute, `SOL_USD_FALLBACK` if it fails). Replay and tests use a fixed price.
- Slippage is reported as pure curve price impact vs spot. Fees are reported separately. The all-in `fill_price` includes everything.
- There is one position per (wallet, mint, delay). Extra target buys while we hold are recorded as skipped ("already in position"), so size stays fixed at $25. This keeps one sim_trades row per target trade per delay.
- Sells mirror the percentage: `tokenAmount / (newTokenBalance + tokenAmount)` of our remaining tokens. A balance under 1 token counts as a full exit.
- Our simulated buys are not applied to the curve state. Each delay is an independent what-if, so the three delays do not distort each other.
- Tokens that leave the bonding curve (`pool` other than `pump`) are not simulated for new buys. An open position on a migrated token is sold at the last known curve state and marked `migrated`.
- Open positions are restored from the DB on restart. Fills pending at the moment of a restart (60s max) are lost. That is acceptable for research data.

## Scorer + rotation
- Scores are computed per wallet, per delay (2/10/60s), per window (14/30 days, by position open time), every `SCORE_INTERVAL` (5 min). Each run is stored as a snapshot, so history is kept.
- Closed positions count realized PnL. Open positions are marked by selling the remaining tokens into the token's last known curve state, fees included. A wallet that never sells its losers therefore still shows the loss.
- PnL is in USD at the SOL/USD rate when the position opened, so SOL price moves do not leak into wallet skill.
- Max drawdown is the peak-to-trough drop of `BANKROLL_USD` ($500) + cumulative PnL, ordered by close time (open positions at now).
- One lucky trade: `pnl_ex_best = pnl - best trade`. `lucky_trade` is true when the total is positive only because of the best trade.
- `score` = ROI% on deployed capital with the best trade removed, scaled down by `(1 - drawdown)` when positive. Fewer than 3 positions scores 0.
- Rules judge the +2s delay (`ROTATION_DELAY_S`):
  - Demote live to bench when the 14d PnL is < 0, the 14d score is < 0, or drawdown >= 25%.
  - Promote bench to live only if all of these hold:
    - 14+ days of sim history
    - >= `PROMOTE_MIN_TRADES` (10) positions in 14d
    - 14d and 30d PnL > 0
    - PnL still positive ex-best-trade
    - score > 0
    - drawdown < 25%
  - The best scores fill free slots, up to `LIVE_MAX` (3).
- "Positive for 2+ weeks" is read as positive over both the rolling 14d and 30d windows, with at least 14 days of history. That is less brittle than requiring every daily snapshot to be positive.
- Every status change goes to the `rotations` table and the events stream, which feeds the dashboard and Telegram.
