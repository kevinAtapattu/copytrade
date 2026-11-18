# copytrade

Solana / pump.fun copy-trading research platform. It paper-tracks many wallets and measures **copyability**: the PnL we would really get copying each wallet at +2s, +10s and +60s, after pump.fun fees, priority fees and curve slippage. Only wallets that stay positive for 2+ weeks get copied, and live trading stays off until you turn it on.

- **Go**: ingest, simulator, scorer, discovery, API, executor, alerter (one process each)
- **Postgres** for storage, **Redis Streams** as the event bus
- **Next.js + Recharts** dashboard
- **Docker Compose** for local and VPS

See `docs/PLAN.md` for phases, `docs/DECISIONS.md` for every judgment call, `CLAUDE.md` for project context.

## Architecture

```
   PumpPortal websocket (ONE connection)          Solana RPC (Helius free tier)
   subscribeAccountTrade: tracked wallets         bonding curve reads at +2/+10/+60s
   (paid per message, spending guard)             and every 30s for held tokens
                              |                         |
                        +-----v------+  <---------------+
                        |   ingest   |----> RECORD_FILE (optional JSONL, replayable)
                        +-----+------+
             persist raw      |      publish
        +---------------------+----------------------+
        |                                            |
        v                                  Redis Stream "trades"
  +-----------+                         /             |            \
  | Postgres  |<---------------- simulator        executor        (more consumers
  |           |   sim_trades     +2/+10/+60s       paper | live     later: shards,
  |  wallets  |   sim_positions  $25 copies        PumpPortal       strategies)
  |  trades   |                                    trade-local
  |  scores   |<---- scorer (5 min): 14/30d scores, live/bench rotation
  |  ...      |<---- discovery (10 min): 5x tokens -> early buyers -> bench, auto-drop
  +-----+-----+
        |                    Redis Stream "events" (fills, rotations, errors)
        |                         |                       |
   +----v----+   SSE + REST  +----v----+            +-----v----+
   |   api   |<--------------| (hub)   |            | alerter  |--> Discord / Telegram
   +----+----+               +---------+            +----------+
        |
   +----v------+
   | dashboard |  http://localhost:3000
   +-----------+
```

Every service is its own process with its own consumer group, so each can be scaled or swapped on its own. `internal/source.Source` is the seam for a future Helius gRPC / LaserStream source.

**Where wallet trades come from.** By default (`WALLET_SOURCE=helius`) each tracked wallet is watched through Helius `logsSubscribe` on your free Helius key. It sees trades from every trading terminal, with about 1 second of lag, at no cost. PumpPortal's own wallet stream missed trades from popular terminals, so it now only carries the free graduated-token feed. See DECISIONS.md. Some wallets are mentioned in thousands of other bots' txs a minute; a wallet over `HELIUS_WALLET_DAILY_MB` of data in a day moves to a low-data feed (its own balance changes only), so it stays tracked without using up the daily cap.

**Data modes.** The default, `DATA_MODE=snapshot`, pays PumpPortal only for your wallets' own trades (about 1 message per trade). It reads token prices straight from each token's bonding curve over RPC, for free. `DATA_MODE=stream` also subscribes to every trade on every token your wallets hold. That costs 10-50x more, so only use it if you need the full tape.

**Self-discovery.** You don't need a wallet list to start. Ingest listens to PumpPortal's free migration stream (every token that graduated pump.fun, about 15x from launch), and discovery reads each one's earliest trades from Helius. Wallets that bought early and sold well on 2+ of them join the bench and start getting paper-tracked. Expect the first ones after hours to a day. `DISCOVERY_SEED_MIGRATIONS=false` turns it off.

**Alerts (Discord).** Put a webhook in `DISCORD_WEBHOOK_URL`, then `docker compose up -d alerter`. You get executor closes and safety stops instantly, an hourly net summary of the paper copies (net result, win rate, best and worst copy, today's running total, top and bottom wallet), and a daily summary at 9am. Paper buys, sim fills and bench changes never ping you. Tune with the `ALERT_*` settings in `.env.example`.

**Spending guard.** Ingest meters every PumpPortal message. When `MSG_DAILY_BUDGET` (default 1000/day, at most about 0.001 SOL/day) is hit, all subscriptions pause until midnight UTC. A wallet with more than `WALLET_MAX_TRADES_PER_DAY` (default 1000) trades in a day is paused until the next day (midnight UTC): it stays on the Wallets page but isn't tracked, then resumes on its own. The Health page shows both.

## What it costs to run (snapshot mode)

| Item | Cost |
|---|---|
| PumpPortal: ~1 message per tracked-wallet trade (0.01 SOL per 10k) | about 0.01 SOL/month for 15 wallets (~$1-2 CAD) |
| PumpPortal: balance it requires in its linked wallet | 0.02 SOL (~$3 CAD), held, not spent |
| Helius free tier (prices, discovery) | $0 (a few thousand of the 1M monthly credits) |
| Hard ceiling from the default budget | 1000 msgs/day = 0.03 SOL/month (~$4 CAD) |

## Try it now, no keys and no Docker

Everything runs in-process on 21 days of scripted fixture data:

```bash
export PATH="$HOME/sdk/go/bin:$PATH"     # Go was installed here (see Prereqs)
go test ./...                             # unit + end-to-end tests
go run ./cmd/replay                       # replay fixtures, print the leaderboard + rotations
go run ./cmd/replay -serve :8080          # ...then keep serving the API on that data
cd dashboard && npm install && API_URL=http://localhost:8080 npm run dev   # http://localhost:3000
```

The fixture has a good wallet (promoted after 14 days), a lucky one-trade wallet (blocked), a fast flipper (loses at +2s), a live wallet that hits -25% drawdown (demoted), an idle wallet (dropped), and two early buyers on 5x tokens (discovered). The executor paper-trades whatever is live.

## Prereqs (macOS)

1. **Accept the Xcode license** (needs your password). Homebrew and `make` are blocked until you do:
   `sudo xcodebuild -license accept`
2. **Go** is installed at `~/sdk/go` (official 1.27.3 tarball, since brew was blocked). The PATH line was added to `~/.zprofile`; open a new terminal or `source ~/.zprofile`.
3. **Docker Desktop** was copied to `/Applications/Docker.app`. Open it once and approve its setup prompts (privileged helper, license). Then `docker compose version` should work.
4. Node 23 is already installed (dashboard).

## Run locally with Docker

```bash
cp .env.example .env              # then fill keys, see KEYS NEEDED below
cp wallets.example.txt wallets.txt  # then put real wallets in it, one per line
make up                           # or: docker compose up -d --build
make logs                         # or: docker compose logs -f --tail=100
open http://localhost:3000
make down
```

Other targets: `make test`, `make migrate` (migrate + seed wallets.txt against `DATABASE_URL`), `make replay`, `make replay-db` (replay into Postgres so the dashboard shows it; **wipes the DB**), `make fixtures`, `make dev-deps` (only Postgres + Redis in Docker, run Go services with `go run ./cmd/<name>`), `make dashboard-dev`.

Services: `migrate` (one-shot), `ingest`, `simulator`, `scorer`, `discovery`, `executor`, `alerter`, `api` (:8080), `dashboard` (:3000), `postgres` (:5432), `redis` (:6379). All ports bind to 127.0.0.1.

To add or remove wallets later, use the dashboard (Wallets page) or edit `wallets.txt` and run `docker compose run --rm migrate`. Seeding only adds wallets; it never changes the status of existing ones.

To record real data for later replays, set `RECORD_FILE=/app/fixtures/recorded.jsonl` for `ingest` and mount `./fixtures`. Then run `go run ./cmd/replay -fixture fixtures/recorded.jsonl -wallets wallets.txt`.

## Run on a VPS

Any Ubuntu 22.04/24.04 box with 2 vCPU / 4 GB works. For copy speed later, pick a region near Solana validators (Frankfurt, Amsterdam, NY, or Tokyo DCs).

```bash
# on the VPS
curl -fsSL https://get.docker.com | sh
git clone <your repo> copytrade && cd copytrade
cp .env.example .env && nano .env          # keys
nano wallets.txt                           # wallets
docker compose up -d --build
docker compose ps
```

The dashboard and API listen on 127.0.0.1 only. Reach them from your laptop over SSH:

```bash
ssh -N -L 3000:127.0.0.1:3000 user@your-vps   # then open http://localhost:3000
```

If you want it public, put Caddy or nginx in front with HTTPS and basic auth. The dashboard has a kill switch and wallet controls, so never expose it without auth. `restart: unless-stopped` keeps everything up 24/7 across reboots (Docker starts on boot).

Never run two ingest instances against the same API key. PumpPortal bans clients that open parallel or repeated connections.

## Tested vs untested without keys

**Tested** (`go test ./...`, all passing with `-race`):
- Curve math, fee math, simulator fills at each delay, mirror sells, skips, migrations
- Scorer metrics, one-lucky-trade check, drawdown
- Rotation and auto-drop rules
- Ingest subscribe/cooldown/unsubscribe and dedupe
- PumpPortal websocket client against a fake server (one connection, resubscribe on reconnect)
- Redis Streams consumer groups (miniredis)
- Solana tx parse and sign
- Live trader against fake PumpPortal + RPC (build, sign, send, confirm, fill parsing, retries)
- Executor risk controls (kill switch, max open, stop-loss, daily loss limit)
- Full end-to-end replay, plus the API on top of it

**Verified against mainnet** (`go run ./cmd/livecheck`: one short free PumpPortal connection plus public RPC; no key, no cost):
- PumpPortal's real message format
- Bonding curve address derivation (6/6)
- Curve account parsing
- pump.fun TradeEvent decoding (10/10 consistent)
- It also found two things now handled: pump.fun tokens priced in non-SOL quote tokens (skipped), and Solana v1 transactions (read OK, refused for signing)

**Also verified by hand:**
- Replay into a real Postgres 16 gives identical results to the in-memory store
- Every API endpoint and the dashboard (all pages, SSE through the Next.js proxy) render on that data
- `docker compose config` validates

**Untested until keys/Docker exist:**
- The paid `subscribeAccountTrade` stream (same message format as the verified free stream)
- Real `trade-local` responses and on-chain fills
- Jito bundle submission
- Discord / Telegram delivery
- The Docker image build and `compose up` (Docker Desktop needs its first-launch approval)
- CoinGecko price fetch (falls back to `SOL_USD_FALLBACK`)

## Going live, step by step

The calendar is a simulation, not your wallet. Paper and sim agree within about $1 for the same wallet and hours (same curve math and fees), but neither models real landing delay (paper fills ~0.1-0.4s after detection; a real tx lands 1s+ later), failed or expired txs, sandwich bots, token account rent (~0.002 SOL per new token, not reclaimed), or SOL/USD moving your balance. The default "Best traders" view also picks wallets with hindsight. A small live pilot is the only way to measure the real gap.

1. Do Prereqs, fill `PUMPPORTAL_API_KEY` and `SOLANA_RPC_URL` (Helius), add wallets, then `go run ./cmd/livecheck` to confirm both work. Then `make up`. Check Health: PumpPortal "connected", data mode "snapshot", budget counting up, RPC calls counting up.
2. Let it paper-track. The executor paper-trades live wallets (`LIVE_TRADING=false`); the Executor page shows paper P&L and "gap vs sim" (paper fill price vs the +2s sim fill for the same trade).
3. Go/no-go for full size (from PLAN.md): 2-3 wallets positive for 2+ weeks at +2s after costs, and gap vs sim small. A pilot can start earlier, sized as money spent to learn the gap.
4. Pilot settings in `.env`: one manually picked wallet, `LIVE_MAX=0` (rotation can't auto-promote anything onto real money; manual picks stay), `EXEC_SIZE_USD=10`, `MAX_OPEN_POSITIONS=2`, `DAILY_LOSS_LIMIT_USD=50`, `JITO_TIP_SOL=0.0001`. Restart scorer and executor.
5. Create a fresh hot wallet (Phantom or `solana-keygen new`), never your main one. Fund it with about $80 of SOL (a day's $50 loss limit, 2 positions x $10, and fees/rent headroom). If the balance can't cover one more buy plus every exit, buys pause and you get an alert; sells continue. Paste its secret key into `TRADER_PRIVATE_KEY` in `.env` yourself and use a paid or Helius `SOLANA_RPC_URL`. Run `go run ./cmd/tradecheck` (dry run, sends nothing).
6. Set `LIVE_TRADING=true`, then `docker compose up -d --no-deps executor alerter api` (the alerter reports only the current mode, so the day you switch, earlier paper copies stay out of live P&L). The executor logs `LIVE TRADING ENABLED` with the wallet address, and the dashboard and alerts show LIVE.
7. Watch the first fills on the Executor page and check Solscan: the wallet's SOL change should match the position P&L. The kill switch stops new buys instantly; exits keep working.
8. After ~30 live round trips, compare: live gap vs sim (target within ~2%), failed tx rate (target under 5%), wallet SOL change vs dashboard P&L. If it holds, raise `EXEC_SIZE_USD` to 25 and add a second wallet. If not, set `LIVE_TRADING=false`.

## KEYS NEEDED

Fill these in `.env` (copy from `.env.example`). Everything else has working defaults.

| Variable | Needed for | Where to get it |
|---|---|---|
| `PUMPPORTAL_API_KEY` | **Required** for any live data | pumpportal.fun, "Create API Key" (Data API docs). It generates a key plus a linked wallet; fund that wallet with **at least 0.02 SOL**; 0.05 SOL covers months in snapshot mode (0.01 SOL per 10k messages; the Health page shows usage). |
| `SOLANA_RPC_URL` | **Required** in snapshot mode (prices + discovery), and for live trading | helius.dev: sign up (no card), copy the mainnet RPC URL (`https://mainnet.helius-rpc.com/?api-key=...`). The free tier (1M credits/month, 10 req/s) is plenty. The public RPC works for prices but throttles discovery. |
| `TRADER_PRIVATE_KEY` | Live trading only | Your **new, small** hot wallet's secret key: base58 export from Phantom/Solflare, or the JSON array from `solana-keygen new -o key.json`. Never your main wallet. |
| `LIVE_TRADING` | Live trading only | Set to `true` only when you are ready (step 5 above). |
| `DISCORD_WEBHOOK_URL` | Optional alerts | In Discord: a channel's settings (gear) > Integrations > Webhooks > New Webhook > Copy Webhook URL. Treat the URL like a password. |
| `TELEGRAM_BOT_TOKEN` | Optional alerts | Message @BotFather on Telegram, `/newbot`. |
| `TELEGRAM_CHAT_ID` | Optional alerts | Send your bot any message, then open `https://api.telegram.org/bot<TOKEN>/getUpdates` and copy `chat.id`. |
| `JITO_TIP_SOL` | Optional, faster landing | No key needed. Set e.g. `0.0001` to send through Jito's block engine with that tip. |

Also put your wallets in `wallets.txt` (not a key, but nothing gets tracked without it).
