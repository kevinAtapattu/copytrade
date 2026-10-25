"use client";
import { useState } from "react";
import { Err, Seg, Stat, Table, WalletName } from "@/components/ui";
import { LiveFill, LivePosition, apiSend, useApi, useWalletNames } from "@/lib/api";
import { help } from "@/lib/glossary";
import { num, pnlClass, price, short, sol, time, usd } from "@/lib/format";

type Exec = {
  mode: string; view: string; kill_switch: boolean; fills: LiveFill[] | null; positions: LivePosition[] | null;
  day_realized_usd: number; daily_loss_limit_usd: number; max_open_positions: number; stop_loss_pct: number;
  size_usd: number; avg_gap_vs_sim_pct: number; gap_samples: number;
};

export default function ExecutorPage() {
  // "" = let the server pick (the mode the executor runs in); then the toggle overrides.
  const [view, setView] = useState<"" | "paper" | "live">("");
  const { data, error, reload } = useApi<Exec>(`executor${view ? `?mode=${view}` : ""}`, 5000);
  const shown = (view || data?.view || "paper") as "paper" | "live";
  const [busy, setBusy] = useState(false);
  const { names } = useWalletNames();
  const open = (data?.positions ?? []).filter((p) => p.status === "open");
  const toggle = async () => {
    if (!data) return;
    if (!data.kill_switch && !confirm("Stop all new buys? Sells of open positions still go through.")) return;
    setBusy(true);
    try { await apiSend("executor/kill", "POST", { on: !data.kill_switch }); await reload(); } finally { setBusy(false); }
  };
  return (
    <>
      <div className="page-head">
        <div>
          <h2>Auto-trader</h2>
          <div className="sub">
            Copies only the wallets you make live, $25 per trade, with a daily loss limit and a stop-loss.{" "}
            {data?.mode === "live" ? "It is trading real money." : "It is in paper mode: it prices each trade at the real market but sends nothing. Real trading needs LIVE_TRADING=true in .env."}
          </div>
        </div>
        <div className="row">
          <Seg value={shown} onChange={setView} options={[{ v: "paper", label: "Paper results", title: help("paper") }, { v: "live", label: "Real results", title: help("live") }]} />
          <button className={`btn kill ${data?.kill_switch ? "on" : "danger"}`} disabled={busy || !data} onClick={toggle} title={help("killSwitch")}>
            {busy ? "Working..." : data?.kill_switch ? "Buying stopped: click to resume" : "Stop all buying"}
          </button>
        </div>
      </div>
      <Err error={error} />
      <div className="grid stats">
        <Stat label="Running in" help={help(data?.mode === "live" ? "live" : "paper")} value={data?.mode === "live" ? "Real money" : data ? "Paper" : "-"}
          hint={`$${data?.size_usd ?? "-"} per trade${data?.kill_switch ? " · buying stopped" : ""}`} />
        <Stat label={`Closed today (${shown})`} value={usd(data?.day_realized_usd, true)} tone={data?.day_realized_usd}
          hint={`stops buying for the day at -${usd(data?.daily_loss_limit_usd)}`} />
        <Stat label="Open positions" value={`${open.length} of ${data?.max_open_positions ?? "-"} max`} hint={`each sold if it falls ${data?.stop_loss_pct ?? "-"}%`} />
        <Stat label="Real vs paper price" help="How much worse (+) or better (-) the auto-trader's prices were than the paper copies'. Near 0% means the paper results are realistic."
          value={data?.gap_samples ? `${num(data?.avg_gap_vs_sim_pct, 2)}%` : "-"} hint={data?.gap_samples ? `over ${data.gap_samples} orders` : "no orders yet"} />
      </div>
      <div className="card" style={{ marginTop: 14 }}>
        <h3>Positions</h3>
        <Table rows={data?.positions ?? []} rowKey={(p) => p.id} initialSort={{ key: "o", desc: true }}
          empty={`No ${shown} positions yet. The auto-trader only copies live wallets; make one live on the Wallets page.`} cols={[
          { key: "o", label: "Opened", sort: (p) => p.opened_at, render: (p) => time(p.opened_at) },
          { key: "m", label: "Mode", render: (p) => <span className="badge">{p.mode}</span> },
          { key: "w", label: "Copying", render: (p) => <WalletName addr={p.wallet} names={names} /> },
          { key: "t", label: "Token", render: (p) => <span className="mono">{short(p.mint)}</span> },
          { key: "s", label: "Status", render: (p) => p.status },
          { key: "c", label: "Cost", num: true, render: (p) => sol(p.cost_sol) },
          { key: "r", label: "Realized", num: true, render: (p) => p.status === "open" ? "-" :
            <span className={pnlClass(p.proceeds_sol - p.cost_sol)}>{usd((p.proceeds_sol - p.cost_sol) * p.sol_usd, true)}</span> },
        ]} />
      </div>
      <div className="card" style={{ marginTop: 14 }}>
        <h3>Orders vs paper copies</h3>
        <Table rows={data?.fills ?? []} rowKey={(f) => f.id} empty={`No ${shown} orders yet`} cols={[
          { key: "t", label: "Sent", render: (f) => time(f.sent_at) },
          { key: "st", label: "Status", render: (f) => <span className={`badge ${f.status === "failed" ? "failed" : ""}`} title={f.error}>{f.status}</span> },
          { key: "side", label: "Side", render: (f) => `${f.side}${f.reason !== "copy" ? ` (${f.reason})` : ""}` },
          { key: "w", label: "Copying", render: (f) => <WalletName addr={f.wallet} names={names} /> },
          { key: "px", label: "Our price", num: true, render: (f) => <span className="mono">{price(f.fill_price)}</span> },
          { key: "sim", label: "Paper +2s", num: true, render: (f) => <span className="mono">{price(f.sim_fill_price)}</span> },
          { key: "lat", label: "Delay", help: "Time from their trade to our order landing.", num: true, render: (f) => f.filled_at ? `${((new Date(f.filled_at).getTime() - new Date(f.target_at).getTime()) / 1000).toFixed(1)}s` : "-" },
          { key: "fee", label: "Fees", num: true, render: (f) => sol(f.fees_sol) },
          { key: "err", label: "Error", render: (f) => <span className="muted">{f.error}</span> },
        ]} />
      </div>
    </>
  );
}
