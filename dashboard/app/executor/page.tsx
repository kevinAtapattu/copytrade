"use client";
import { useState } from "react";
import { Err, Stat, Table } from "@/components/ui";
import { LiveFill, LivePosition, apiSend, useApi } from "@/lib/api";
import { num, pnlClass, price, short, sol, time, usd } from "@/lib/format";

type Exec = {
  mode: string; kill_switch: boolean; fills: LiveFill[] | null; positions: LivePosition[] | null;
  day_realized_usd: number; daily_loss_limit_usd: number; max_open_positions: number; stop_loss_pct: number;
  size_usd: number; avg_gap_vs_sim_pct: number; gap_samples: number;
};

export default function ExecutorPage() {
  const { data, error, reload } = useApi<Exec>("executor", 5000);
  const [busy, setBusy] = useState(false);
  const open = (data?.positions ?? []).filter((p) => p.status === "open");
  const toggle = async () => {
    if (!data) return;
    if (!data.kill_switch && !confirm("Stop all new copy buys? Exits still run.")) return;
    setBusy(true);
    try { await apiSend("executor/kill", "POST", { on: !data.kill_switch }); await reload(); } finally { setBusy(false); }
  };
  return (
    <>
      <div className="page-head">
        <div>
          <h2>Executor</h2>
          <div className="sub">Copies live wallets only. Mode is set by LIVE_TRADING in .env; PAPER fills use the live curve with no transaction sent.</div>
        </div>
        <button className={`btn kill ${data?.kill_switch ? "on" : "danger"}`} disabled={busy || !data} onClick={toggle}>
          {data?.kill_switch ? "KILL SWITCH ON (click to resume)" : "KILL SWITCH"}
        </button>
      </div>
      <Err error={error} />
      <div className="grid stats">
        <Stat label="Mode" value={data?.mode?.toUpperCase() ?? "-"} hint={`$${data?.size_usd ?? "-"} per trade`} />
        <Stat label="Realized today" value={usd(data?.day_realized_usd, true)} tone={data?.day_realized_usd} hint={`limit -${usd(data?.daily_loss_limit_usd)}`} />
        <Stat label="Open positions" value={`${open.length} / ${data?.max_open_positions ?? "-"}`} hint={`stop-loss ${data?.stop_loss_pct ?? "-"}%`} />
        <Stat label="Avg gap vs sim" value={`${num(data?.avg_gap_vs_sim_pct, 2)}%`} hint={`${data?.gap_samples ?? 0} fills, positive = worse than simulated`} />
      </div>
      <div className="card" style={{ marginTop: 14 }}>
        <h3>Positions</h3>
        <Table rows={data?.positions ?? []} rowKey={(p) => p.id} initialSort={{ key: "o", desc: true }} empty="No positions" cols={[
          { key: "o", label: "Opened", sort: (p) => p.opened_at, render: (p) => time(p.opened_at) },
          { key: "m", label: "Mode", render: (p) => <span className="badge">{p.mode}</span> },
          { key: "w", label: "Wallet", render: (p) => <span className="mono">{short(p.wallet)}</span> },
          { key: "t", label: "Token", render: (p) => <span className="mono">{short(p.mint)}</span> },
          { key: "s", label: "Status", render: (p) => p.status },
          { key: "c", label: "Cost", num: true, render: (p) => sol(p.cost_sol) },
          { key: "r", label: "Realized", num: true, render: (p) => p.status === "open" ? "-" :
            <span className={pnlClass(p.proceeds_sol - p.cost_sol)}>{usd((p.proceeds_sol - p.cost_sol) * p.sol_usd, true)}</span> },
        ]} />
      </div>
      <div className="card" style={{ marginTop: 14 }}>
        <h3>Fills vs simulation</h3>
        <Table rows={data?.fills ?? []} rowKey={(f) => f.id} empty="No fills yet" cols={[
          { key: "t", label: "Sent", render: (f) => time(f.sent_at) },
          { key: "st", label: "Status", render: (f) => <span className={`badge ${f.status === "failed" ? "failed" : ""}`} title={f.error}>{f.status}</span> },
          { key: "side", label: "Side", render: (f) => `${f.side}${f.reason !== "copy" ? ` (${f.reason})` : ""}` },
          { key: "w", label: "Wallet", render: (f) => <span className="mono">{short(f.wallet)}</span> },
          { key: "px", label: "Our price", num: true, render: (f) => <span className="mono">{price(f.fill_price)}</span> },
          { key: "sim", label: "Sim +2s", num: true, render: (f) => <span className="mono">{price(f.sim_fill_price)}</span> },
          { key: "lat", label: "Latency", num: true, render: (f) => f.filled_at ? `${((new Date(f.filled_at).getTime() - new Date(f.target_at).getTime()) / 1000).toFixed(1)}s` : "-" },
          { key: "fee", label: "Fees", num: true, render: (f) => sol(f.fees_sol) },
          { key: "err", label: "Error", render: (f) => <span className="muted">{f.error}</span> },
        ]} />
      </div>
    </>
  );
}
