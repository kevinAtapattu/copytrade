"use client";
import Link from "next/link";
import { useState } from "react";
import LineChart, { mergeSeries } from "@/components/LineChart";
import { Err, Seg, Stat, Table } from "@/components/ui";
import { LivePosition, Point, SimPosition, useApi, useEvents } from "@/lib/api";
import { ago, num, pnlClass, short, usd } from "@/lib/format";

type Overview = {
  delay: number; scope: string; wallet_counts: Record<string, number>; bankroll_usd: number;
  pnl: { day: number; week: number; month: number; all: number };
  equity: Point[]; exec_equity: Point[]; open_positions: SimPosition[]; exec_open: LivePosition[]; positions: number;
};

export default function OverviewPage() {
  const [delay, setDelay] = useState(2);
  const [scope, setScope] = useState<"" | "all">("");
  const { data, error } = useApi<Overview>(`overview?delay=${delay}&scope=${scope}`);
  const { events } = useEvents();
  const rows = mergeSeries({ sim: data?.equity, exec: data?.exec_equity });
  const hasExec = (data?.exec_equity?.length ?? 0) > 0;
  return (
    <>
      <div className="page-head">
        <div>
          <h2>Overview</h2>
          <div className="sub">
            Simulated $25 copies of {data?.scope === "all" ? "all tracked wallets" : "live wallets"} at +{delay}s, after fees and slippage
          </div>
        </div>
        <div className="row">
          <Seg value={scope} onChange={setScope} options={[{ v: "", label: "Live wallets" }, { v: "all", label: "All tracked" }]} />
          <Seg value={delay} onChange={setDelay} options={[2, 10, 60].map((d) => ({ v: d, label: `+${d}s` }))} />
        </div>
      </div>
      <Err error={error} />
      <div className="grid stats">
        <Stat label="PnL today" value={usd(data?.pnl.day, true)} tone={data?.pnl.day} />
        <Stat label="PnL 7 days" value={usd(data?.pnl.week, true)} tone={data?.pnl.week} />
        <Stat label="PnL 30 days" value={usd(data?.pnl.month, true)} tone={data?.pnl.month} />
        <Stat label="Equity" value={usd((data?.bankroll_usd ?? 0) + (data?.pnl.all ?? 0))} hint={`from ${usd(data?.bankroll_usd)} bankroll`} />
        <Stat label="Wallets" value={`${data?.wallet_counts?.live ?? 0} live`} hint={`${data?.wallet_counts?.bench ?? 0} on bench`} />
      </div>
      <div className="grid" style={{ marginTop: 14 }}>
        <div className="card">
          <h3>Cumulative PnL (USD){hasExec ? ", simulated vs executor" : ""}</h3>
          <LineChart rows={rows} series={[
            { key: "sim", label: `Simulated +${delay}s`, color: "var(--series-1)" },
            ...(hasExec ? [{ key: "exec", label: "Executor fills", color: "var(--series-2)" }] : []),
          ]} />
        </div>
      </div>
      <div className="grid two" style={{ marginTop: 14 }}>
        <div className="card">
          <h3>Open simulated positions ({data?.open_positions?.length ?? 0})</h3>
          <Table rows={data?.open_positions ?? []} rowKey={(p) => p.id} empty="No open positions"
            initialSort={{ key: "opened", desc: true }}
            cols={[
              { key: "wallet", label: "Wallet", render: (p) => <Link href={`/wallets/${p.wallet}`} className="mono">{short(p.wallet)}</Link> },
              { key: "mint", label: "Token", render: (p) => <a className="mono" href={`https://pump.fun/coin/${p.mint}`} target="_blank">{short(p.mint)}</a> },
              { key: "opened", label: "Opened", sort: (p) => p.opened_at, render: (p) => ago(p.opened_at) },
              { key: "cost", label: "Cost", num: true, render: (p) => usd(p.cost_sol * p.sol_usd) },
              { key: "mark", label: "Mark PnL", num: true, sort: (p) => p.mark_pnl_usd ?? 0,
                render: (p) => <span className={pnlClass(p.mark_pnl_usd)}>{usd(p.mark_pnl_usd, true)}</span> },
            ]} />
          {(data?.exec_open?.length ?? 0) > 0 && (
            <>
              <h3 style={{ marginTop: 16 }}>Executor open positions ({data!.exec_open.length})</h3>
              <Table rows={data!.exec_open} rowKey={(p) => p.id} cols={[
                { key: "mode", label: "Mode", render: (p) => <span className="badge">{p.mode}</span> },
                { key: "wallet", label: "Wallet", render: (p) => <span className="mono">{short(p.wallet)}</span> },
                { key: "mint", label: "Token", render: (p) => <span className="mono">{short(p.mint)}</span> },
                { key: "cost", label: "Cost", num: true, render: (p) => usd(p.cost_sol * p.sol_usd) },
                { key: "held", label: "Tokens", num: true, render: (p) => num(p.tokens_held, 0) },
              ]} />
            </>
          )}
        </div>
        <div className="card">
          <h3>Live events</h3>
          <div className="events">
            {events.length === 0 && <div className="muted">Waiting for events...</div>}
            {events.map((e, i) => (
              <div key={i}>
                <span className="muted">{new Date(e.at).toLocaleTimeString()}</span>{" "}
                <span className={`badge ${e.kind === "error" ? "critical" : ""}`}>{e.kind}</span>{" "}
                {e.message || describe(e)}
              </div>
            ))}
          </div>
        </div>
      </div>
    </>
  );
}

function describe(e: { kind: string; data?: unknown }) {
  const d = e.data as Record<string, unknown> | undefined;
  if (!d) return "";
  if (e.kind === "sim_fill") return `${d.side} ${short(String(d.mint))} by ${short(String(d.wallet))} at +${d.delay_s}s`;
  return "";
}
