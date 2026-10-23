"use client";
import Link from "next/link";
import { useState } from "react";
import { Col, Err, Seg, Table } from "@/components/ui";
import { Score, Wallet, apiSend, useApi } from "@/lib/api";
import { ago, dur, num, pct, pnlClass, short, usd } from "@/lib/format";

type Row = Wallet & { scores: Record<string, Record<string, Score>>; open_positions: number };

export default function WalletsPage() {
  const [delay, setDelay] = useState(2);
  const [win, setWin] = useState(14);
  const [showDropped, setShowDropped] = useState(false);
  const { data, error, reload } = useApi<Row[]>(`wallets${showDropped ? "?dropped=1" : ""}`);
  const [addr, setAddr] = useState("");
  const [label, setLabel] = useState("");
  const [msg, setMsg] = useState<string | null>(null);

  const sc = (r: Row) => r.scores?.[String(delay)]?.[String(win)];
  const act = async (fn: () => Promise<unknown>, ok: string) => {
    try { await fn(); setMsg(ok); reload(); } catch (e) { setMsg(String((e as Error).message)); }
  };
  const cols: Col<Row>[] = [
    { key: "status", label: "Status", sort: (r) => (r.status === "live" ? 0 : r.status === "bench" ? 1 : 2),
      render: (r) => <span className={`badge ${r.status}`}>{r.status}</span> },
    { key: "wallet", label: "Wallet", render: (r) => (
      <Link href={`/wallets/${r.address}`}><span className="mono">{short(r.address)}</span> {r.label && <span className="muted">{r.label}</span>}</Link>
    ) },
    { key: "score", label: "Score", num: true, sort: (r) => sc(r)?.score ?? -1e9, render: (r) => <b className={pnlClass(sc(r)?.score)}>{num(sc(r)?.score)}</b> },
    { key: "pnl", label: "Sim PnL", num: true, sort: (r) => sc(r)?.pnl_usd ?? -1e9, render: (r) => <span className={pnlClass(sc(r)?.pnl_usd)}>{usd(sc(r)?.pnl_usd, true)}</span> },
    { key: "exbest", label: "Ex best trade", num: true, sort: (r) => sc(r)?.pnl_ex_best_usd ?? -1e9,
      render: (r) => <span className={pnlClass(sc(r)?.pnl_ex_best_usd)}>{usd(sc(r)?.pnl_ex_best_usd, true)}{sc(r)?.lucky_trade ? " (lucky)" : ""}</span> },
    { key: "win", label: "Win rate", num: true, sort: (r) => sc(r)?.win_rate ?? 0, render: (r) => pct(sc(r)?.win_rate, 0) },
    { key: "dd", label: "Max DD", num: true, sort: (r) => sc(r)?.max_drawdown ?? 0, render: (r) => pct(sc(r)?.max_drawdown) },
    { key: "hold", label: "Avg hold", num: true, sort: (r) => sc(r)?.avg_hold_s ?? 0, render: (r) => dur(sc(r)?.avg_hold_s) },
    { key: "tpd", label: "Trades/day", num: true, sort: (r) => sc(r)?.trades_per_day ?? 0, render: (r) => num(sc(r)?.trades_per_day, 1) },
    { key: "n", label: "Positions", num: true, sort: (r) => sc(r)?.positions ?? 0, render: (r) => sc(r)?.positions ?? 0 },
    { key: "days", label: "History", num: true, sort: (r) => sc(r)?.active_days ?? 0, render: (r) => `${num(sc(r)?.active_days, 1)}d` },
    { key: "last", label: "Last trade", sort: (r) => r.last_trade_at ?? "", render: (r) => ago(r.last_trade_at) },
    { key: "src", label: "Source", render: (r) => <span className="muted" title={r.reason}>{r.source}</span> },
    { key: "act", label: "", render: (r) => (
      <div className="row">
        {r.status === "bench" && <button className="btn small primary" onClick={() => act(() => apiSend(`wallets/${r.address}/status`, "POST", { status: "live" }), "promoted")}>Promote</button>}
        {r.status === "live" && <button className="btn small" onClick={() => act(() => apiSend(`wallets/${r.address}/status`, "POST", { status: "bench" }), "benched")}>Bench</button>}
        {r.status !== "dropped"
          ? <button className="btn small danger" onClick={() => confirm(`Remove ${r.address}?`) && act(() => apiSend(`wallets/${r.address}`, "DELETE"), "removed")}>Remove</button>
          : <button className="btn small" onClick={() => act(() => apiSend(`wallets`, "POST", { address: r.address }), "re-added")}>Re-add</button>}
      </div>
    ) },
  ];
  const live = (data ?? []).filter((r) => r.status === "live");
  const rest = (data ?? []).filter((r) => r.status !== "live");
  return (
    <>
      <div className="page-head">
        <div>
          <h2>Wallet leaderboard</h2>
          <div className="sub">Copyability = what we would have made copying them, at the selected delay. Click a column to sort.</div>
        </div>
        <div className="row">
          <Seg value={delay} onChange={setDelay} options={[2, 10, 60].map((d) => ({ v: d, label: `+${d}s` }))} />
          <Seg value={win} onChange={setWin} options={[{ v: 14, label: "14d" }, { v: 30, label: "30d" }]} />
          <label className="sub"><input type="checkbox" checked={showDropped} onChange={(e) => setShowDropped(e.target.checked)} /> show dropped</label>
        </div>
      </div>
      <Err error={error} />
      <div className="card" style={{ marginBottom: 14 }}>
        <h3>Add wallet to bench</h3>
        <form className="row" onSubmit={(e) => {
          e.preventDefault();
          act(() => apiSend("wallets", "POST", { address: addr, label }), "added").then(() => { setAddr(""); setLabel(""); });
        }}>
          <input placeholder="Solana address" value={addr} onChange={(e) => setAddr(e.target.value)} style={{ minWidth: 380 }} />
          <input placeholder="label (optional)" value={label} onChange={(e) => setLabel(e.target.value)} />
          <button className="btn primary" type="submit">Add</button>
          {msg && <span className="sub">{msg}</span>}
        </form>
      </div>
      <div className="card" style={{ marginBottom: 14 }}>
        <h3>Live ({live.length})</h3>
        <Table rows={live} cols={cols} rowKey={(r) => r.address} initialSort={{ key: "score", desc: true }} empty="No live wallets yet. Bench wallets get promoted after 14+ days positive." />
      </div>
      <div className="card">
        <h3>Bench ({rest.length})</h3>
        <Table rows={rest} cols={cols} rowKey={(r) => r.address} initialSort={{ key: "score", desc: true }} empty="No wallets. Add some above or put them in wallets.txt." />
      </div>
    </>
  );
}
