"use client";
import { Err, Stat, Table } from "@/components/ui";
import { useApi, useWalletNames } from "@/lib/api";
import { dur, pnlClass, short, usd } from "@/lib/format";

// Other coins: how the wallet we copy does on pump.fun coins priced in a token
// other than SOL, which we skip. Their own result, kept apart from every SOL number.
type Round = {
  wallet: string; mint: string; opened_at: string; closed_at?: string; hold_s: number;
  buys: number; sells: number; pct: number; per10: number;
};
type Skip = { wallet: string; mint: string; reason: string; trades: number; note?: string };
type Day = { date: string; rounds: number; won: number; per10: number };
type Resp = {
  tz: string; wallets: string[];
  summary: { rounds: number; won: number; open: number; total_per10: number; skipped: number;
    win_rate?: number; best_per10?: number; without_best_per10?: number };
  days: Day[]; rounds: Round[]; open: Round[]; skipped: Skip[];
};

type Copy = {
  id: string; wallet: string; mint: string; their_at: string; opened_at: string; closed_at?: string; status: string;
  sol_usd: number; sol_in: number; fees_sol: number; sells: number; route: string; skip_reason?: string;
  last_error?: string; marked_at?: string; pnl_usd: number; pct: number;
};
type CopySummary = {
  wallet?: string; copies: number; won: number; win_rate?: number; total_usd: number; best_usd?: number;
  without_best_usd?: number; open: number; open_usd: number; skipped: number; skipped_by_reason: Record<string, number>;
};
type CopiesResp = { wallets: string[]; summary: CopySummary; by_wallet: CopySummary[]; copies: Copy[] };

const copySkips: Record<string, string> = {
  TOKEN_NOT_TRADABLE: "Jupiter can't trade it",
  NO_ROUTES_FOUND: "No route",
  ZERO_OUT: "No route",
  QUOTE_FAILED: "Quote failed",
  NO_SOL_PRICE: "No SOL price",
};
const skipText = (r?: string) => (r ? copySkips[r] ?? r : "");

const reasons: Record<string, string> = {
  no_price: "No price recorded",
  mixed: "Mixed units",
  before_tracking: "Bought before tracking",
  sell_only: "Sell with no buy",
};

const coin = (m: string) => <a className="mono" href={`https://pump.fun/coin/${m}`} target="_blank" rel="noreferrer">{short(m)}</a>;
const when = (iso?: string) => iso ? new Date(iso).toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" }) : "-";
const fmtDay = (d: string) => new Date(`${d}T12:00:00`).toLocaleDateString([], { weekday: "short", month: "short", day: "numeric" });

function OurCopies({ names }: { names: Map<string, string> }) {
  const { data, error } = useApi<CopiesResp>("other-coins/copies", 30000);
  const s = data?.summary;
  const who = (data?.wallets ?? []).map((a) => names.get(a) ?? short(a)).join(", ") || "the live wallet";
  const skips = Object.entries(s?.skipped_by_reason ?? {}).sort((a, b) => b[1] - a[1])
    .map(([r, n]) => `${n} ${skipText(r).toLowerCase()}`).join(", ");
  const others = (data?.by_wallet ?? []).filter((w) => w.wallet && !data?.wallets.includes(w.wallet));
  const result = (c: Copy) => {
    if (c.status === "skipped") return <span className="sub" title={c.last_error}>{skipText(c.skip_reason)}</span>;
    if (c.status === "open" && !c.marked_at) return <span className="sub">open</span>;
    return <span className={pnlClass(c.pnl_usd)} title={`${c.pct > 0 ? "+" : ""}${c.pct.toFixed(1)}%`}>
      {usd(c.pnl_usd, true)}{c.status === "open" ? " open" : ""}</span>;
  };
  return (
    <>
      <h3 style={{ margin: "4px 0 6px" }}>Our paper copies (Jupiter quotes, 3s after them, $10 each)</h3>
      <div className="sub" style={{ marginBottom: 10 }}>
        What we would get: 3 seconds after {who} buys, we ask Jupiter what $10 of SOL buys, and when they sell we ask what
        the same share of our coins sells for. Pool fees are in the quote, and we take off 0.0005 SOL per swap. One copy per
        coin until it is sold. No stop loss. Paper only, not in any other page.
      </div>
      <Err error={error} />
      {!data ? !error && <div className="empty">Loading...</div> : (
        <>
          <div className="grid stats">
            <Stat label="Copies" value={s?.copies ?? 0} hint={`${s?.open ?? 0} still open`} />
            <Stat label="Win rate" value={s?.win_rate != null ? `${Math.round(s.win_rate)}%` : "-"}
              hint={`${s?.won ?? 0} of ${s?.copies ?? 0} made money`} />
            <Stat label="Total" value={usd(s?.total_usd, true)} tone={s?.total_usd} hint="closed copies, after fees" />
            <Stat label="Without best copy" value={s?.without_best_usd != null ? usd(s.without_best_usd, true) : "-"}
              tone={s?.without_best_usd} hint={s?.best_usd != null ? `best copy was ${usd(s.best_usd, true)}` : " "} />
            <Stat label="Open now" value={usd(s?.open_usd, true)} tone={s?.open_usd} hint="at the last sell quote" />
            <Stat label="Skipped" value={s?.skipped ?? 0} hint={skips || "buys we couldn't copy"} />
          </div>
          {others.length > 0 && (
            <div className="sub" style={{ marginTop: 8 }}>
              Also watched: {others.map((w) => `${names.get(w.wallet!) ?? short(w.wallet)} ${w.copies} closed, ${usd(w.total_usd, true)}`).join("; ")}.
            </div>
          )}
          <div className="card" style={{ marginTop: 14 }}>
            <h3>Copies ({data.copies.length})</h3>
            <Table rows={data.copies} rowKey={(c) => c.id} initialSort={{ key: "t", desc: true }}
              empty="No copies yet. They start with the next buy on one of these coins."
              cols={[
                { key: "t", label: "Bought", sort: (c) => c.their_at, render: (c) => when(c.their_at) },
                { key: "who", label: "Wallet", hideSm: true, render: (c) => names.get(c.wallet) ?? short(c.wallet) },
                { key: "mint", label: "Coin", render: (c) => coin(c.mint) },
                { key: "held", label: "Held", num: true, hideSm: true,
                  sort: (c) => c.closed_at ? Date.parse(c.closed_at) - Date.parse(c.opened_at) : 0,
                  render: (c) => c.closed_at ? dur((Date.parse(c.closed_at) - Date.parse(c.opened_at)) / 1000) : "-" },
                { key: "sells", label: "Sells", num: true, hideSm: true, render: (c) => c.status === "skipped" ? "-" : c.sells },
                { key: "res", label: "Result", num: true, sort: (c) => c.status === "skipped" ? -1e9 : c.pnl_usd, render: result },
                { key: "route", label: "Route", hideSm: true, render: (c) => <span className="sub">{c.route || "-"}</span> },
              ]} />
          </div>
        </>
      )}
    </>
  );
}

export default function OtherCoinsPage() {
  const { data, error } = useApi<Resp>("other-coins", 60000);
  const { names } = useWalletNames();
  const who = (data?.wallets ?? []).map((a) => names.get(a) ?? short(a));
  const whoName = who.length ? who.join(", ") : "the wallet we copy";
  const s = data?.summary;
  const tz = data?.tz === "America/Toronto" || data?.tz === "America/New_York" ? "Eastern time" : data?.tz ?? "";
  return (
    <>
      <div className="page-head">
        <div>
          <h2>Other coins</h2>
          <div className="sub">
            How {whoName} does on pump.fun coins priced in a token other than SOL. We skip these coins, so none of this is in
            any other page. Each round is one coin from first buy until they hold none, as a percent of what they put in,
            shown as dollars per $10 copy.
          </div>
        </div>
      </div>
      <OurCopies names={names} />
      <h3 style={{ margin: "26px 0 6px" }}>{whoName}&apos;s own result</h3>
      <div className="card" style={{ marginBottom: 14 }}>
        <b>{whoName}&apos;s own result at their prices, no copy delay, fees or slippage. Not what we would get.</b>
        <div className="sub" style={{ marginTop: 4 }}>
          On SOL coins our copies normally do a bit worse than {whoName} does. For what a delayed copy would get, see our
          paper copies above.
        </div>
      </div>
      <Err error={error} />
      {!data ? !error && <div className="empty">Loading...</div> : (
        <>
          <div className="grid stats">
            <Stat label="Rounds" value={s?.rounds ?? 0} hint={`${s?.open ?? 0} still open`} />
            <Stat label="Win rate" value={s?.win_rate != null ? `${Math.round(s.win_rate)}%` : "-"}
              hint={`${s?.won ?? 0} of ${s?.rounds ?? 0} made money`} />
            <Stat label="Total per $10 copy" value={usd(s?.total_per10, true)} tone={s?.total_per10}
              hint="each round $10 in, added up" />
            <Stat label="Without best round" value={usd(s?.without_best_per10, true)} tone={s?.without_best_per10}
              hint={s?.best_per10 != null ? `best round was ${usd(s.best_per10, true)}` : " "} />
            <Stat label="Not counted" value={s?.skipped ?? 0} hint="coins or sells we can't price, see below" />
          </div>
          <div className="grid" style={{ marginTop: 14 }}>
            <div className="card">
              <h3>By day</h3>
              <div className="sub" style={{ marginBottom: 8 }}>Counted on the day each round closed ({tz}).</div>
              <Table rows={data.days} rowKey={(d) => d.date} initialSort={{ key: "date", desc: true }} empty="No closed rounds yet."
                cols={[
                  { key: "date", label: "Day", sort: (d) => d.date, render: (d) => fmtDay(d.date) },
                  { key: "n", label: "Rounds", num: true, sort: (d) => d.rounds, render: (d) => d.rounds },
                  { key: "won", label: "Won", num: true, hideSm: true, render: (d) => `${d.won} of ${d.rounds}` },
                  { key: "per10", label: "Per $10", num: true, sort: (d) => d.per10,
                    render: (d) => <span className={pnlClass(d.per10)}>{usd(d.per10, true)}</span> },
                ]} />
            </div>
          </div>
          <div className="card" style={{ marginTop: 14 }}>
            <h3>Rounds ({data.rounds.length})</h3>
            <Table rows={data.rounds} rowKey={(r) => `${r.wallet}${r.mint}${r.opened_at}`} initialSort={{ key: "t", desc: true }}
              empty="No closed rounds yet."
              cols={[
                { key: "t", label: "Closed", sort: (r) => r.closed_at ?? "", render: (r) => when(r.closed_at) },
                { key: "mint", label: "Coin", render: (r) => coin(r.mint) },
                { key: "hold", label: "Held", num: true, hideSm: true, sort: (r) => r.hold_s, render: (r) => dur(r.hold_s) },
                { key: "trades", label: "Buys / sells", num: true, hideSm: true, render: (r) => `${r.buys} / ${r.sells}` },
                { key: "pct", label: "Result", num: true, hideSm: true, sort: (r) => r.pct,
                  render: (r) => <span className={pnlClass(r.pct)}>{r.pct > 0 ? "+" : ""}{r.pct.toFixed(1)}%</span> },
                { key: "per10", label: "Per $10", num: true, sort: (r) => r.per10,
                  render: (r) => <span className={pnlClass(r.per10)}>{usd(r.per10, true)}</span> },
              ]} />
          </div>
          <div className="card" style={{ marginTop: 14 }}>
            <h3>Not counted ({data.skipped.length})</h3>
            <div className="sub" style={{ marginBottom: 8 }}>
              Sells with no recorded buy, tokens held from before we tracked the wallet, or a coin whose prices can&apos;t be put in one unit.
            </div>
            <Table rows={data.skipped} rowKey={(x) => `${x.wallet}${x.mint}${x.reason}`} empty="Nothing left out."
              cols={[
                { key: "mint", label: "Coin", render: (x) => coin(x.mint) },
                { key: "why", label: "Why", sort: (x) => x.reason, render: (x) => <span title={x.note}>{reasons[x.reason] ?? x.reason}</span> },
                { key: "n", label: "Trades", num: true, hideSm: true, render: (x) => x.trades },
              ]} />
          </div>
          {data.open.length > 0 && (
            <div className="card" style={{ marginTop: 14 }}>
              <h3>Still open ({data.open.length})</h3>
              <Table rows={data.open} rowKey={(r) => `${r.wallet}${r.mint}${r.opened_at}`}
                cols={[
                  { key: "t", label: "First buy", render: (r) => when(r.opened_at) },
                  { key: "mint", label: "Coin", render: (r) => coin(r.mint) },
                  { key: "trades", label: "Buys / sells", num: true, render: (r) => `${r.buys} / ${r.sells}` },
                ]} />
            </div>
          )}
        </>
      )}
    </>
  );
}
