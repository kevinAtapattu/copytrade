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
- On startup, ingest re-subscribes tokens that tracked wallets still held at their last seen trade (within `TOKEN_MAX_WATCH`), so a restart does not orphan open positions.
- Without `PUMPPORTAL_API_KEY`, ingest stays idle instead of connecting (the metered streams need a key) and health shows why.
- Tracked wallets = every wallet that is not `dropped` (live + bench).
- A token is subscribed when a tracked wallet buys it (or partially sells one we were not watching). It is unsubscribed once every tracked holder's `newTokenBalance` drops under 1 token, plus a cooldown (default 5 min, never below 90s so the +60s sim can fill), or after `TOKEN_MAX_WATCH` (24h) to cap metered usage.
- Trades are deduped by `signature|trader`, since a tracked wallet's trade arrives on both its account and its token subscription.
- The PumpPortal SOL usage estimate is metered messages / 10,000 * 0.01 SOL.

## Simulator
- Event time, not sleeps. A fill due at T+d uses the curve state after the last trade at or before T+d. It is finalized when a later trade on that mint arrives, or by a 1s ticker once T+d+3s has passed (3s of grace for late frames). The ticker's clock is stream time: the newest processed `received_at` plus wall time since it was processed. During a backlog (restart, Redis lag) it stays at stream time, so fills are never finalized before the trades that should price them have been read. When idle, it advances with the wall clock. Replay finalizes from event time, then flushes at the end.
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

## Discovery
- A token qualifies when `max_price / first_price >= 5` (`DISCOVERY_MULT`), measured from the first trade we observed. It is scanned once, then marked `discovered`.
- We only see trades after a tracked wallet entered, since that is when we subscribe. "Early" therefore means relative to our first observation: the wallet's first buy was at or below 1.5x our first seen price (`DISCOVERY_EARLY_MULT`). Helius history could widen this later.
- "Exited well" means an average exit of at least 2x the average entry (`DISCOVERY_EXIT_MULT`), with at least 50% of the bought tokens sold.
- Wallets with hits on 2+ distinct 5x tokens (`DISCOVERY_MIN_HITS`) join the bench with `source=discovery` and a reason listing the tokens and the average exit multiple.
- Wallets already known, including dropped ones, are not re-added automatically. You can re-add them from the dashboard.
- Auto-drop runs after each discovery pass and applies to bench wallets only:
  - No trade for `DROP_INACTIVE_DAYS` (7).
  - 30d sim PnL <= `DROP_BAD_PNL_USD` (-$50) over >= 15 trades.
  - When the bench exceeds `BENCH_MAX` (50), the lowest 14d scores are dropped.

## API
- Plain `net/http` with Go 1.22+ route patterns, no framework. CORS is open (`*`) because it binds to 127.0.0.1 and the dashboard proxies to it anyway.
- Live updates use SSE (`/api/stream`), not websockets: one-way, works through the Next.js proxy, auto-reconnects in the browser. One hub goroutine tails the Redis `events` stream and fans out to clients. Slow clients drop messages instead of blocking.
- The overview defaults to live wallets at +2s. While no wallet is live yet, it shows all bench wallets so the page is not empty.
- PumpPortal SOL usage = token trades stored / 10,000 * 0.01 SOL. Every metered message we keep is one stored trade (a tracked wallet's trade can arrive twice, on its account and token subs, so this slightly undercounts). The monthly figure extrapolates the last 24h.

## Dashboard
- Next.js 16 app router, every page a client component. A catch-all route handler (`app/api/[...path]`) proxies to the Go API at runtime (`API_URL`), so the browser talks to a single origin. One build works on localhost and on a VPS, and SSE streams through.
- Package versions are pinned exactly to releases at least two weeks old (next 16.3.8, react 19.3.0, recharts 3.10.1, TypeScript 5.9.3).
- There are no data-fetching libraries. A small `useApi` hook refetches every 15s, and on live SSE events (throttled to once per 2s).
- Dark only. Chart series use the first three slots of the validated reference palette, dark steps (blue/orange/aqua, all checks pass on `#1a1a19`). PnL numbers carry a +/- sign as well as color.
- The add/remove/promote/bench buttons call the API directly. Removing a wallet sets it to `dropped` (history is kept, and you can re-add it).

## Executor
- PAPER is the default. Only `LIVE_TRADING=true` plus a valid `TRADER_PRIVATE_KEY` builds the live trader. With LIVE_TRADING=true and no key, the executor refuses to start.
- Paper fills hit the curve state at the moment the target trade arrives. They pay the same fees a live trade would: pump.fun 1.25% + PumpPortal local API 0.5% + base fee + priority fee (+ Jito tip if set).
- Live trades go through PumpPortal `trade-local`. The tx is signed locally with stdlib ed25519 (a small hand-rolled wire-format parser for legacy and v0 messages, no big Solana SDK) and sent through `SOLANA_RPC_URL` with `skipPreflight` and `maxRetries: 0`.
- With `JITO_TIP_SOL > 0`, the request uses the bundle form (an array body, where the first tx's priorityFee is the tip) and goes to the Jito block engine via `sendBundle`.
- Retries rebuild the tx from PumpPortal, which brings a fresh blockhash. A retry is sent only when one of these holds:
  - the node returned a definite JSON-RPC rejection,
  - the previous tx failed on-chain,
  - or its blockhash stopped being valid without the tx landing.
  On a transport error (timeout, dropped connection), the tx may have been forwarded anyway, so we wait for it to land or expire before rebuilding. A tx that landed but can't be read is not retried.
- Real fills are read from `getTransaction` balance deltas: SOL change (including the network fee) and the token balance change for our wallet.
- Sells use PumpPortal's percentage form (`"37.50%"`), so we mirror the target's percentage of our actual on-chain balance.
- Risk controls:
  - Fixed `EXEC_SIZE_USD`, one position per (wallet, token).
  - `MAX_OPEN_POSITIONS` counts in-flight buys too.
  - Stop-loss: when the curve value of the remaining tokens drops below cost basis by `STOP_LOSS_PCT`, sell 100%.
  - Daily loss limit: UTC-day realized PnL at or below `-DAILY_LOSS_LIMIT_USD` blocks new buys.
  - Kill switch (dashboard, stored in `settings`, polled every 5s) blocks new buys. Exits and stop-losses still run, so you are never stuck in a position.
- Blocked buys are recorded as `rejected` fills with the reason, so the dashboard shows what was skipped and why.
- Target BUYS older than 30s when consumed (backlog after a restart) are not copied. Stale SELLS still go through, so we exit positions the target exited while we were down.
- Live requests use `pool: "auto"`, so sells still work after a token migrates off the bonding curve. These are exactly the winners. Stop-loss can't price a migrated token (no curve reserves in the stream), but copy sells still exit it.
- Each (wallet, token) gets its own ordered worker goroutine. A slow live tx never blocks the stream, and a sell can't overtake its buy.
- The live-vs-sim gap: the API joins each executor fill to the simulator's fill for the same target trade at +2s. The dashboard shows the price gap per fill and on average.

## Alerts
- A separate `alerter` process consumes the `events` stream (its own consumer group), so alerting can never slow down trading.
- It alerts on executor fills, failures, rejections and kill-switch changes, plus rotations, discovery adds and service errors. Simulated fills and raw target trades are excluded because they would flood the chat.
- Messages are paced at 1/sec with a 100-message queue. Overflow is dropped and counted in health.
- Without `TELEGRAM_BOT_TOKEN`/`TELEGRAM_CHAT_ID`, alerts only go to the log.

## Testing + replay
- `internal/pipeline` wires ingest, simulator, executor (paper, synchronous), scorer and discovery on an in-memory bus and store, driven by a replay source in event time. `cmd/replay` and the end-to-end test share it, so what is tested is what runs.
- The replay source honors subscriptions exactly like PumpPortal. It only delivers trades by subscribed wallets or on subscribed tokens, so auto-subscribe and the entry race are exercised (about 1/3 of fixture messages are filtered out).
- `fixtures/replay.jsonl` comes from `internal/fixture` (seed 42, 21 days, about 6k trades) with scripted wallets. The e2e test regenerates the same data in memory and asserts:
  - Good is promoted only after 14 days.
  - Lucky is blocked by the one-trade rule.
  - Fast (flips within 3s) loses at +2s.
  - Draw (starts live) gets demoted on drawdown.
  - Idle is auto-dropped.
  - Two early buyers on 5x tokens are discovered.
  - The executor copies only while a wallet is live.
  - sim fills = 3 x target trades.
- `RECORD_FILE=path` on ingest appends every live trade in the same JSONL format, so real sessions can be replayed later with `go run ./cmd/replay -fixture path -wallets wallets.txt`.
- The live trader is tested against fake PumpPortal and RPC HTTP servers: request shape, local signature verifies, confirm, balance-delta parsing, and 3 fresh rebuilds on on-chain failure. The PumpPortal websocket client is tested against a fake websocket server: one connection at a time, resubscribe after reconnect, handler can subscribe without deadlock.
