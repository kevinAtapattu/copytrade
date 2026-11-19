"use client";
import Link from "next/link";
import { useState } from "react";
import LineChart, { mergeSeries } from "@/components/LineChart";
import { Err, Seg, Stat, Table, WalletName } from "@/components/ui";
import { LiveFill, LivePosition, Point, SimPosition, useApi, useEvents, useWalletNames } from "@/lib/api";
import { ago, num, pnlClass, short, sol, usd } from "@/lib/format";
import { help } from "@/lib/glossary";
import { useView } from "@/lib/view";

type Overview = {
  delay: number; scope: string; wallet_counts: Record<string, number>; bankroll_usd: number;
  pnl: { day: number; week: number; month: number; all: number };
  today: { closed_usd: number; closed: number; won: number; open_usd: number; open_cost_usd: number; open: number; tz: string };
  equity: Point[]; exec_equity: Point[]; open_positions: SimPosition[]; exec_open: LivePosition[] | null; exec_mode: string; positions: number;
  run_mode: string; live_wallets: string[] | null;
  exec_today: { closed_usd: number; closed: number; won: number; open_usd: number; open_cost_usd: number; open: number };
  exec_pnl: { week: number; all: number; since: string | null };
  wallet: { address: string; balance_sol: number; sol_usd?: number } | null;
};

export default function OverviewPage() {
  const { view, ready } = useView();
  const { data } = useApi<Overview>(ready && view === "real" ? "overview?mode=live" : null);
  if (!ready || (view === "real" && !data)) return <div className="empty">Loading...</div>;
  return view === "real" && data ? <RealOverview data={data} /> : <PaperOverview />;
}

// RealOverview is the live wallet: its balance, what the auto-trader made, and
// its open trades and orders. Only the wallets being copied appear.
function RealOverview({ data }: { data: Overview }) {
  const { names } = useWalletNames();
  const { data: ex } = useApi<{ fills: LiveFill[] | null }>("executor?mode=live&limit=15", 10000);
  const t = data.exec_today;
  const w = data.wallet;
  const rows = mergeSeries({ exec: data.exec_equity });
  const who = (data.live_wallets ?? []).map((a) => names.get(a) ?? short(a));
  return (
    <>
      <div className="page-head">
        <div>
          <h2>Overview</h2>
          <div className="sub">
            Real money: your wallet{w ? <> <a className="mono link" href={`https://solscan.io/account/${w.address}`} target="_blank" rel="noreferrer">{short(w.address)}</a></> : ""}{" "}
            copying {who.length ? who.join(", ") : "no one yet"}. Results are after all fees.
          </div>
        </div>
      </div>
      <div className="grid stats">
        <Stat label="Wallet balance" value={w ? (w.sol_usd ? usd(w.balance_sol * w.sol_usd) : sol(w.balance_sol)) : "-"}
          hint={w ? `${sol(w.balance_sol)} free to trade, plus open trades` : "no trading wallet set"} />
        <Stat label="Today, closed" help={help("closed")} value={usd(t.closed_usd, true)} tone={t.closed_usd}
          hint={`${t.won} of ${t.closed} trades won · since midnight Eastern`} />
        <Stat label="Open right now" help={help("open")} value={usd(t.open_usd, true)} tone={t.open ? t.open_usd : undefined}
          hint={t.open ? `${t.open} trade${t.open > 1 ? "s" : ""}, ${usd(t.open_cost_usd)} in, not final` : "nothing open"} />
        <Stat label="Since going live" value={usd(data.exec_pnl.all, true)} tone={data.exec_pnl.all}
          hint={data.exec_pnl.since ? `closed + open, since ${new Date(data.exec_pnl.since).toLocaleDateString([], { month: "short", day: "numeric" })}` : "no trades yet"} />
        <Stat label="Copying" value={who.length ? who.join(", ") : "-"} hint={<Link href="/executor" className="link">limits and controls</Link>} />
      </div>
      <div className="grid two" style={{ marginTop: 14 }}>
        <div className="card">
          <h3>Open trades ({data.exec_open?.length ?? 0})</h3>
          <Table rows={data.exec_open ?? []} rowKey={(p) => p.id} initialSort={{ key: "opened", desc: true }}
            empty="Nothing open. A trade opens here when a wallet you copy buys."
            cols={[
              { key: "wallet", label: "Copying", render: (p) => <WalletName addr={p.wallet} names={names} /> },
              { key: "mint", label: "Token", render: (p) => <a className="mono" href={`https://pump.fun/coin/${p.mint}`} target="_blank" rel="noreferrer">{short(p.mint)}</a> },
              { key: "opened", label: "Opened", sort: (p) => p.opened_at, render: (p) => ago(p.opened_at) },
              { key: "cost", label: "In", num: true, hideSm: true, render: (p) => usd(p.cost_sol * p.sol_usd) },
              { key: "mark", label: "Now", help: help("open"), num: true, sort: (p) => p.mark_pnl_usd ?? 0,
                render: (p) => <span className={pnlClass(p.mark_pnl_usd)}>{usd(p.mark_pnl_usd, true)}</span> },
            ]} />
        </div>
        <div className="card">
          <h3>Recent orders</h3>
          <div className="sub" style={{ marginBottom: 8 }}>Skipped buys of coins priced in something other than SOL are left out; the Auto-trader page lists them.</div>
          <Table rows={(ex?.fills ?? []).filter((f) => !f.error?.startsWith("not on a SOL bonding curve"))} rowKey={(f) => f.id} empty="No real orders yet. The first one goes out on the next buy by a wallet you copy."
            cols={[
              { key: "t", label: "Sent", render: (f) => new Date(f.sent_at).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }) },
              { key: "side", label: "Order", render: (f) => `${f.side}${f.reason !== "copy" ? ` (${f.reason.replace(/_/g, " ")})` : ""}` },
              { key: "mint", label: "Token", render: (f) => <span className="mono">{short(f.mint)}</span> },
              { key: "amt", label: "SOL", num: true, render: (f) => f.status === "filled" ? num(f.sol_amount, 4) : "-" },
              { key: "lat", label: "Delay", help: "Time from their trade to our order landing.", num: true,
                render: (f) => f.filled_at ? `${((new Date(f.filled_at).getTime() - new Date(f.target_at).getTime()) / 1000).toFixed(1)}s` : "-" },
              { key: "st", label: "Status", render: (f) => f.signature ? <a href={`https://solscan.io/tx/${f.signature}`} target="_blank" rel="noreferrer"
                className={`badge ${f.status === "failed" ? "failed" : ""}`} title={f.error}>{f.status}</a>
                : <span className={`badge ${f.status === "failed" ? "failed" : ""}`} title={f.error}>{f.status}</span> },
            ]} />
        </div>
      </div>
      <div className="card" style={{ marginTop: 14 }}>
        <h3>Real P&amp;L by day</h3>
        {rows.length < 2 ? <div className="empty">This fills in after the first days of live trading.</div>
          : <LineChart rows={rows} series={[{ key: "exec", label: "Real money", color: "var(--series-2)" }]} />}
      </div>
    </>
  );
}

function PaperOverview() {
  const [delay, setDelay] = useState(2);
  const [scope, setScope] = useState<"" | "all">("");
  const [execView, setExecView] = useState<"" | "paper" | "live">("");
  const { data, error, loading } = useApi<Overview>(`overview?delay=${delay}&scope=${scope}${execView ? `&mode=${execView}` : ""}`);
  const { names } = useWalletNames();
  const shownExec = (execView || data?.exec_mode || "paper") as "paper" | "live";
  const { events } = useEvents();
  const rows = mergeSeries({ sim: data?.equity, exec: data?.exec_equity });
  const hasExec = (data?.exec_equity?.length ?? 0) > 0;
  const counts = data?.wallet_counts ?? {};
  const live = counts.live ?? 0;
  const watching = (counts.bench ?? 0) + (counts.paused ?? 0);
  // With no live wallets the API shows everyone we watch; reflect that in the toggle.
  const effScope = (data?.scope === "all" ? "all" : "") as "" | "all";
  const t = data?.today;
  return (
    <>
      <div className="page-head">
        <div>
          <h2>Overview</h2>
          <div className="sub">
            Paper results: a $25 copy of every trade by {effScope === "all" ? "every wallet we watch" : "live wallets"}, copied {delay}s late, after fees and slippage.
            {live === 0 && scope === "" && data && <> No wallets are live yet, so this shows everyone we watch.</>}
          </div>
        </div>
        <div className="row">
          <Seg value={effScope} onChange={setScope} options={[
            { v: "", label: "Live wallets", title: help("liveWallet") }, { v: "all", label: "All watched", title: help("watching") }]} />
          <Seg value={delay} onChange={setDelay} options={[2, 10, 60].map((d) => ({ v: d, label: `+${d}s`, title: help("delay") }))} />
        </div>
      </div>
      <Err error={error} />
      <div className="grid stats">
        <Stat label="Today, closed" help={help("closed")} value={usd(t?.closed_usd, true)} tone={t?.closed_usd}
          hint={t ? `${t.won} of ${t.closed} copies won · since midnight ${tzName(t.tz)}` : " "} />
        <Stat label="Open right now" help={help("open")} value={usd(t?.open_usd, true)} tone={t?.open_usd}
          hint={t ? `${t.open} copies, ${usd(t.open_cost_usd)} in, not final` : " "} />
        <Stat label="Last 7 days" value={usd(data?.pnl.week, true)} tone={data?.pnl.week} hint="closed + open" />
        <Stat label="Paper balance" value={usd((data?.bankroll_usd ?? 0) + (data?.pnl.all ?? 0))}
          hint={`started at ${usd(data?.bankroll_usd)}`} />
        <Stat label="Wallets" value={<>{watching} <span className="unit">watching</span></>} hint={live ? `${live} live` : <Link href="/wallets" className="link">none live yet, see ranking</Link>} />
      </div>
      <div className="grid" style={{ marginTop: 14 }}>
        <div className="card">
          <h3>Paper balance change, by day{hasExec ? `, simulated vs auto-trader (${shownExec})` : ""}</h3>
          {rows.length < 3 && !loading ? (
            <div className="empty">
              {rows.length === 0 ? "No results yet. This fills in once copies start closing." :
                <>Only {rows.length} day{rows.length > 1 ? "s" : ""} of results so far ({rows.map((r) => `${new Date(`${r.x}T12:00:00Z`).toLocaleDateString([], { month: "short", day: "numeric" })}: ${usd(Number(r.sim), true)}`).join(", ")}). The chart appears after 3 days, when a trend starts to mean something.</>}
            </div>
          ) : (
            <LineChart rows={rows} series={[
              { key: "sim", label: `Paper copies +${delay}s`, color: "var(--series-1)" },
              ...(hasExec ? [{ key: "exec", label: `Auto-trader ${shownExec}`, color: "var(--series-2)" }] : []),
            ]} />
          )}
          {hasExec && (
            <div className="row" style={{ marginTop: 8 }}>
              <span className="sub">Auto-trader line:</span>
              <Seg value={shownExec} onChange={setExecView} options={[{ v: "paper", label: "Paper" }, { v: "live", label: "Live" }]} />
            </div>
          )}
        </div>
      </div>
      <div className="grid two" style={{ marginTop: 14 }}>
        <div className="card">
          <h3>Open paper copies ({data?.open_positions?.length ?? 0})</h3>
          <Table rows={data?.open_positions ?? []} rowKey={(p) => p.id} loading={loading}
            empty="Nothing open. A copy opens here when a watched wallet buys."
            initialSort={{ key: "opened", desc: true }}
            cols={[
              { key: "wallet", label: "Copying", render: (p) => <WalletName addr={p.wallet} names={names} /> },
              { key: "mint", label: "Token", hideSm: true, render: (p) => <a className="mono" href={`https://pump.fun/coin/${p.mint}`} target="_blank" rel="noreferrer">{short(p.mint)}</a> },
              { key: "opened", label: "Opened", sort: (p) => p.opened_at, render: (p) => ago(p.opened_at) },
              { key: "mark", label: "Now", help: help("open"), num: true, sort: (p) => p.mark_pnl_usd ?? 0,
                render: (p) => <span className={pnlClass(p.mark_pnl_usd)}>{usd(p.mark_pnl_usd, true)}</span> },
            ]} />
          {(data?.exec_open?.length ?? 0) > 0 && (
            <>
              <h3 style={{ marginTop: 16 }}>Auto-trader open positions, {shownExec} ({data!.exec_open!.length})</h3>
              <Table rows={data!.exec_open ?? []} rowKey={(p) => p.id} cols={[
                { key: "mode", label: "Mode", render: (p) => <span className="badge">{p.mode}</span> },
                { key: "wallet", label: "Copying", render: (p) => <WalletName addr={p.wallet} names={names} /> },
                { key: "mint", label: "Token", render: (p) => <span className="mono">{short(p.mint)}</span> },
                { key: "cost", label: "Cost", num: true, render: (p) => usd(p.cost_sol * p.sol_usd) },
                { key: "held", label: "Tokens", num: true, render: (p) => num(p.tokens_held, 0) },
              ]} />
            </>
          )}
        </div>
        <div className="card">
          <h3>Activity</h3>
          <div className="events" aria-live="polite">
            {events.length === 0 && <div className="muted">Quiet so far. Copies, closes and alerts show up here as they happen.</div>}
            {events.map((e, i) => (
              <div key={i}>
                <span className="muted">{new Date(e.at).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}</span>{" "}
                <span className={`badge ${e.kind === "error" ? "critical" : ""}`}>{KIND[e.kind] ?? e.kind.replace(/_/g, " ")}</span>{" "}
                {describe(e, names) || e.message}
              </div>
            ))}
          </div>
        </div>
      </div>
    </>
  );
}

const KIND: Record<string, string> = {
  sim_fill: "paper copy", copy_closed: "copy closed", exec_closed: "auto-trader closed", live_fill: "auto-trader",
  rotation: "wallet", info: "note", error: "problem", discovery: "discovery",
};

function tzName(tz: string) {
  return tz === "America/Toronto" || tz === "America/New_York" ? "Eastern" : tz === "UTC" ? "UTC" : tz.split("/").pop()?.replace(/_/g, " ");
}

function describe(e: { kind: string; data?: unknown }, names: Map<string, string>) {
  const d = e.data as Record<string, unknown> | undefined;
  if (!d) return "";
  const who = (a: unknown) => names.get(String(a)) ?? short(String(a));
  if (e.kind === "sim_fill") return `${d.side === "buy" ? "Bought" : "Sold"} ${short(String(d.mint))} copying ${who(d.wallet)} (+${d.delay_s}s)`;
  return "";
}
