# copytrade

Solana / pump.fun copy-trading research platform. Paper-tracks wallets, scores how
copyable they are at realistic delays, and (later) executes live copies on the best ones.

## Stack
- **Go**: ingest, simulator, scorer, API, executor
- **Postgres** storage, **Redis Streams** event bus
- **Next.js** dashboard
- **Docker Compose** for local + VPS

See `docs/PLAN.md` for phases and `CLAUDE.md` for project context.
