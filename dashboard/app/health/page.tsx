"use client";
import { Err, Stat, Table } from "@/components/ui";
import { useApi } from "@/lib/api";
import { ago, num } from "@/lib/format";

type Svc = { data: Record<string, unknown>; updated_at: string; stale: boolean };
type Health = {
  services: Record<string, Svc>; stream_lag: Record<string, number>; msgs_per_sec_1m: number;
  messages_today: number; messages_24h: number; est_sol_today: number; est_sol_24h: number; est_sol_month: number;
  failed_tx_24h: number; live_fills_24h: number; last_trade_at: string;
};

const EXPECTED = ["ingest", "simulator", "scorer", "discovery", "api", "executor", "alerter"];

export default function HealthPage() {
  const { data, error } = useApi<Health>("health", 5000);
  const ing = data?.services?.ingest?.data as Record<string, unknown> | undefined;
  const rows = EXPECTED.map((name) => ({ name, svc: data?.services?.[name] }));
  return (
    <>
      <div className="page-head"><div><h2>Health</h2><div className="sub">Refreshes every 5s</div></div></div>
      <Err error={error} />
      <div className="grid stats">
        <Stat label="PumpPortal" value={ing ? (ing.connected ? "connected" : "disconnected") : "no data"}
          hint={ing ? (ing.has_api_key ? `${ing.accounts} wallets, ${ing.tokens} tokens subscribed` : "PUMPPORTAL_API_KEY missing") : "ingest not running"} />
        <Stat label="Messages/sec (1m)" value={num(data?.msgs_per_sec_1m, 2)} hint={`last trade ${ago(data?.last_trade_at)}`} />
        <Stat label="Stream lag (msgs)" value={`${data?.stream_lag?.simulator ?? 0} / ${data?.stream_lag?.executor ?? 0}`} hint="simulator / executor" />
        <Stat label="Failed tx (24h)" value={data?.failed_tx_24h ?? 0} hint={`${data?.live_fills_24h ?? 0} executor fills`} />
        <Stat label="PumpPortal SOL used today" value={`${num(data?.est_sol_today, 4)} SOL`}
          hint={`${num(data?.messages_today, 0)} msgs · ~${num(data?.est_sol_month, 3)} SOL/month at 24h rate`} />
      </div>
      <div className="card" style={{ marginTop: 14 }}>
        <h3>Services</h3>
        <Table rows={rows} rowKey={(r) => r.name} cols={[
          { key: "n", label: "Service", render: (r) => r.name },
          { key: "s", label: "State", render: (r) => !r.svc ? <span className="badge warn">never seen</span>
            : r.svc.stale ? <span className="badge critical">stale</span> : <span className="badge live">ok</span> },
          { key: "u", label: "Heartbeat", render: (r) => ago(r.svc?.updated_at) },
          { key: "d", label: "Details", render: (r) => <span className="mono muted" style={{ whiteSpace: "normal" }}>{r.svc ? fmt(r.svc.data) : ""}</span> },
        ]} />
      </div>
    </>
  );
}

function fmt(d: Record<string, unknown>) {
  return Object.entries(d || {})
    .map(([k, v]) => `${k}=${typeof v === "number" ? num(v, 3) : typeof v === "string" && v.match(/^\d{4}-/) ? ago(v) : String(v)}`)
    .join("  ");
}
