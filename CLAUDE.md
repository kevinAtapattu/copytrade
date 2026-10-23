# CLAUDE.md

Context for Claude Code. Read this and docs/PLAN.md before working.

## What this is
Kevin's side project: a Solana pump.fun copy-trading system. Goal is NOT to blindly copy
famous wallets. We paper-track many wallets and measure **copyability**: the PnL we would
realistically get copying them at our delay, after fees and slippage. Only wallets that
score positive for 2+ weeks get copied live, starting with $500 of play money and fixed
~$25 positions. Also meant as a resume project (Go, event-driven, real-time).

## Decisions so far
- Backend in **Go** (goroutines fit long-running websocket streams). Dashboard in **Next.js**.
- Postgres + Redis Streams, Docker Compose. Each component is its own process so it can be
  scaled horizontally later (sharded ingest workers, multiple executors w/ separate wallets).
- Data source Phase 1: **PumpPortal websocket** `wss://pumpportal.fun/api/data?api-key=...`
  - `subscribeAccountTrade` (target wallets), `subscribeTokenTrade` (all trades on tokens
    a target touched, gives us price after their entry)
  - Both are metered: 0.01 SOL per 10k messages, needs API key + wallet with >= 0.02 SOL
  - ONE websocket connection only; send all subscribes on it (repeated connects = 1h ban)
  - Later for speed: Helius gRPC / LaserStream, Jito bundles, VPS near validators
- Simulate entries/exits at **+2s, +10s, +60s** delays, fixed $25 size, include pump.fun fee,
  priority fee, optional 1% bot fee toggle.
- Sizing when live: fixed amount per trade, mirror sells by percentage.
- Wallet rotation: live set (2-3), bench (20-50) always paper-tracked, rolling 14/30-day score.
  Drop live wallet on negative rolling score or -25% drawdown; promote bench wallet only
  after 2+ weeks positive and not driven by one trade.
- Discovery: when a token 5x+, flag early buyers who exited well; repeat names join bench.

## Conventions
- Never commit secrets. Trading key only in .env, small balance hot wallet.
- Commit after each meaningful step.
- Kevin prefers short, concrete explanations, no em dashes.
