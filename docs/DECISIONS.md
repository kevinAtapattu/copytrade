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
- Discord was added (`DISCORD_WEBHOOK_URL`, a plain channel webhook, no bot). Every configured destination gets each alert. @mentions are disabled in messages.

## Spending guard
- PumpPortal bills per message received, duplicates included, so ingest meters every message before dedupe.
- `MSG_DAILY_BUDGET` (default 1000/day, about 0.001 SOL/day or roughly $4 CAD/month at most): when hit, ingest **closes the PumpPortal websocket** and keeps it closed until midnight UTC, then opens one fresh connection and re-sends every subscription. Closing the connection, rather than relying on unsubscribe messages, means the cap holds even if PumpPortal ignores or mishandles keyed `unsubscribeAccountTrade` (its docs don't show that form). A restart that finds today's budget already spent starts disconnected. 80% usage sends a warning alert. After a restart, today's count is restored from the DB, so restarting can't reset the budget.
- `WALLET_MAX_TRADES_PER_HOUR` (default 30): a tracked wallet over this in a rolling hour is set to `dropped` with reason "spam guard", unsubscribed, and alerted. It stays dropped until you re-add it from the dashboard.
- PumpPortal may deliver a few messages after an unsubscribe; those are still counted. The spam guard uses keyed unsubscribe for the one wallet. If that is ignored, the daily cap is still the backstop.
- The non-SOL check (`Classify`) runs on the stream's reader goroutine, after the price reads are scheduled, with a 1.5s timeout. It fails open, so a slow RPC can't stall ingest or delay the +2s read.

## Snapshot mode (default, `DATA_MODE=snapshot`)
- The stream design paid for every trade on every token a wallet touched, roughly $85+ CAD/month. Snapshot mode subscribes only to tracked wallets (`subscribeAccountTrade`) and reads prices from the chain for free.
- For each tracked trade on the bonding curve, the snapshotter reads the token's bonding curve account (`getMultipleAccounts`, `processed` commitment) at +2s, +10s and +60s. Each read is published on the trades stream as a `snapshot` trade carrying the reserves, stamped with the intended moment. The simulator and executor needed no changes.
- Tokens tracked wallets hold are re-read every `MARK_INTERVAL` (30s) in one batched call (up to 100 accounts each), for open-position marks and the executor's stop-loss. Ingest's existing watch/unwatch logic drives this through a `TokenWatcher` interface, so stream and snapshot share it.
- The curve address comes from PumpPortal's `bondingCurveKey`. If missing, it is derived as the PDA `["bonding-curve", mint]` of the pump program.
- A completed (migrated) curve is published with `pool=migrated`, so the simulator stops new buys and prices exits at the last curve state, as before.
- RPC is rate-limited (`RPC_RPS`, 8/s) and counted per UTC day. Over `RPC_DAILY_MAX` (30k), periodic marks pause; fill reads never do. Expected use is a few thousand calls a day, well within Helius free (1M/month).
- Accuracy: a read reflects the chain about 0.25s plus RPC latency after the intended moment. Stream mode instead used the last trade before the moment. Both are within the simulator's 3s grace window.
- Replay supports both modes. `-mode snapshot` delivers only tracked wallets' trades and answers curve reads from the recorded history. On the fixture it reaches the same promotions and demotions with 329 metered messages instead of 4,255.
- Discovery in snapshot mode: when a watched token reaches 5x (from our curve reads), the discovery job fetches the token's earliest trades from its bonding curve address. It walks back with `getSignaturesForAddress` (up to `DISCOVERY_MAX_PAGES` x 1000), decodes the oldest `DISCOVERY_MAX_TX` (400) transactions, and reads pump.fun's own `TradeEvent` from them. Events come from both "Program data:" logs and Anchor `emit_cpi` inner instructions, deduped. This is better than stream mode's view: "early" is now measured from launch, not from when we started watching.
- Discovery uses its own RPC client at a quarter of `RPC_RPS` and a separate daily cap (`DISCOVERY_RPC_DAILY_MAX`, 5000 calls), so it can never starve ingest's price reads. A token whose fetch fails is retried on the next run.
- Tokens remember their bonding curve address (migration `002`).
- Unverified until it runs against mainnet: the `TradeEvent` byte layout (taken from pump.fun's IDL: mint, sol_amount, token_amount, is_buy, user, timestamp, virtual reserves) and the PDA derivation fallback. Both are only fallbacks or best-effort; a decode mismatch means discovery finds nothing, not wrong trades.

## Verified on mainnet (`go run ./cmd/livecheck`, October 2026)
- PumpPortal messages have the fields we parse (`signature, mint, traderPublicKey, txType, bondingCurveKey, vSolInBondingCurve, vTokensInBondingCurve, marketCapSol, pool`). Reserves are in SOL and UI tokens, and sit on the constant-product curve k = 30 x 1.073e9.
- Bonding curve PDA derivation matched PumpPortal's `bondingCurveKey` on 6/6 tokens.
- Curve account parsing matches the stream on SOL curves.
- `TradeEvent` decoding works. The event appears both as "Program data:" in the logs and as an emit_cpi inner instruction (tag e445a52e51cb9a1d); the two are deduped.
- **Non-SOL curves exist.** pump.fun now launches tokens priced in another token. Their curve account has a non-zero quote mint at bytes [83:115], and "vSol" is not SOL. On the first tracked buy of a mint, ingest does one curve read (`Classify`, cached per mint) and tags these trades `pool=nonsol`. The simulator and executor skip them like migrated tokens. PumpPortal still reported vSol=30 for such a token, so the message alone cannot tell.
- **Transaction v1 exists.** RPC calls now request `maxSupportedTransactionVersion: 1`; with 0, those transactions error out. v1 has a different wire layout (version byte first), and our signer only handles legacy/v0. It refuses v1 with an error rather than mis-signing. If PumpPortal ever returns v1 transactions for trading, live sends fail safely and the signer needs v1 support.
- The public RPC rate-limits `getTransaction` (429s). Discovery needs a real RPC key such as Helius free.

## Paper / Live view
- Executor results are always tagged `paper` or `live`. The Executor page and the Overview's executor line show one mode at a time, through a toggle (`?mode=paper|live` on `/api/executor` and `/api/overview`). The default is the mode the executor is running in, so real-money totals (today's PnL, gap vs sim, equity) never include paper history. The trade log still lists every fill with its mode badge.

## Self-discovery from graduated tokens
- Discovery used to scan only tokens our tracked wallets touched, so with no wallets it found nothing. Ingest now also subscribes to PumpPortal's **free** `subscribeMigration` stream on its one existing connection (no second socket). It is free per the PumpPortal docs, and the spending guard does not count it.
- Every token that completes its bonding curve has run about 14.7x from launch (vSol 30 to ~115). Each migration is stored as a token with `first_price` = launch price and `max_price` = completion price, so "early" is judged against the real launch, not our first sighting.
- Discovery fetches the token's earliest curve transactions from Helius and runs the existing early-buyer / exited-well logic. A wallet that does this on `DISCOVERY_MIN_HITS` (2) tokens joins the bench.
- **Cost control.** Migrations arrive every minute or so, far more than the RPC budget can scan. Discovery scans newest first and stops at `DISCOVERY_RPC_DAILY_MAX`; the backlog is just skipped. A graduated token whose history is longer than `DISCOVERY_MAX_PAGES` (raised to 15 x 1000 signatures) is skipped without decoding, because its "earliest" trades would really be mid-life ones.
- **This can start PumpPortal charges on its own.** Discovered wallets go to the bench, and bench wallets are paper-tracked through metered `subscribeAccountTrade`. The daily message budget and per-wallet spam limit still cap it. Set `DISCOVERY_SEED_MIGRATIONS=false` to stop it.
- Unrecognized PumpPortal messages that carry a mint are logged once, so a changed migration format is visible.

## Alerts: three tiers instead of a ping per trade
- With 20+ wallets, per-trade alerts would be hundreds a day. Alerts are now tiered (`internal/alert/policy.go`):
  - **Instant:** executor position closed (paper or live, with our result, their result on the token and the wallet's 14d record), real-money fills, promotions to / demotions from live, budget hit, kill switch / daily loss limit (once a day), errors (same error at most every 3h, digits ignored). Capped at `ALERT_MAX_PER_HOUR`; anything held back is counted in the next roll-up.
  - **Roll-up:** every closed paper copy at `ALERT_SIM_DELAY` in one message per `ALERT_ROLLUP_EVERY` (default 1h), biggest result first, each line with wallet, token, our $ and %, their % and hold time. Nothing closed means no message. Built to fit Discord's 2000-character limit.
  - **Daily digest** at `ALERT_DIGEST_HOUR` in `ALERT_TZ`: 24h net, best/worst wallets, executor results, discoveries, drops, PumpPortal spend.
- No longer instant: paper executor buys, executor rejections, bench drops (spam guard, inactivity), discovery adds, the new-day notice.
- Roll-ups and the digest are built from the database for a time window, with the last window stored in `settings`, so container restarts neither lose nor repeat them. The first run starts at "now", so turning alerts on never dumps history. Stream events older than 1h (backlog after downtime) are not sent instantly.
- "Their return" comes from one shared function (`internal/compare`) used by both the dashboard and alerts.

## Spam guard pauses instead of dropping
- A wallet over `WALLET_MAX_TRADES_PER_HOUR` used to be dropped, which hid it from the Wallets page and lost track of it for good. It is now **paused**: status `paused` with the reason and resume time, still listed (orange "paused (spam)" badge), not subscribed so it costs nothing, and back on the bench after `WALLET_SPAM_PAUSE` (default 6h). The Resume button brings it back sooner. A live wallet that spams resumes to the bench, not live.
- Ingest only tracks `live` and `bench`; the pause expiry is checked on each wallet reload (every 30s).

## Wallet trades come from Helius, not PumpPortal (verified October 24 2026)
- **Problem found on mainnet:** PumpPortal's `subscribeAccountTrade` delivered trades placed through GMGN (program `GMgnVF…`), but **nothing** from trades placed through the `term9Y…` trading terminal, which most tracked wallets use. Over ~2 hours only one wallet (theo, GMGN) produced any data while the chain showed others trading.
- **Side-by-side probe (12 min, same wallets):** Helius `logsSubscribe` (`mentions: [wallet]`, commitment confirmed) delivered 14 trades for Johnson and dash with ~1s lag; PumpPortal delivered 0. Data volume was 0.34 MB (~7 credits), so a day is ~1k of the free plan's 1M credits/month. A spam-flooded wallet produced 42 notifications and no trades.
- **Now:** `WALLET_SOURCE=helius` (default) subscribes each tracked wallet over one Helius websocket. Trades are decoded from the pump program's own log events (failed txs and non-pump txs skipped; truncated logs fall back to `getTransaction`). Each trade's post-trade token balance is read with one `getTransaction` so partial sells copy correctly (logs don't carry it; without it every sell would look like a full exit). `HELIUS_WS_DAILY_MB` caps data per UTC day.
- PumpPortal keeps its single connection for the free migration (graduation) stream only, so it bills nothing; the message budget is disabled in this mode and the Health page shows Helius usage instead. `WALLET_SOURCE=pumpportal` restores the old behavior.
- **Decoder bug fixed alongside:** trading terminals emit their own Anchor `TradeEvent` with the same discriminator as pump's. Events are now accepted only from the pump program's log frame or its own emit_cpi inner instructions; before, the terminal's event decoded as a zero-SOL trade.
- Data before this change only contains GMGN-routed trades, so early scores and rankings were biased toward those wallets.
