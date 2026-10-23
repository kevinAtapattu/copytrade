# Plan

## Architecture
```
PumpPortal / Helius stream
        |
   ingest (Go)  ->  Redis Streams
        |                 |
   simulator          executor (Phase 4)
        |                 |
        +---> Postgres <--+
                 |
           api (Go) -> dashboard (Next.js)
```

## Phase 1: Collector
- [x] Go module, docker-compose (Postgres, Redis)
- [x] Postgres schema: wallets, target_trades, token_trades, sim_trades
- [x] ingest: single PumpPortal ws, subscribe to wallets, auto-subscribe to their tokens
- [x] simulator: $25 entries/exits at +2s/+10s/+60s with fees + slippage
- [x] wallets loaded from wallets.txt
- [ ] run 24/7 on VPS

## Phase 2: Scoring + discovery
- [x] rolling 14/30-day copyability score per wallet
- [x] one-lucky-trade check, drawdown, hold time, trades/day
- [x] discovery job for early buyers of 5x tokens
- [x] auto-drop inactive/bad wallets

## Phase 3: Dashboard
- [ ] overview (equity, PnL, open positions)
- [ ] wallet leaderboard (live vs bench)
- [ ] wallet detail (their trades vs our sim fills by delay)
- [ ] trade log, discovery queue, health (stream lag, failed tx)

## Go / no-go
2-3 wallets positive for 2+ weeks at +2s delay after costs.

## Phase 4: Live ($500)
- [ ] executor on event bus, live wallets only
- [ ] fixed $25 size, max open positions, stop-loss, daily loss limit, kill switch
- [ ] live vs simulated comparison per trade

## Phase 5: Scale
- capital in steps, gRPC + Jito + VPS near validators, maybe Rust executor
- sharded ingest, multiple executors with separate wallets, strategy variants
