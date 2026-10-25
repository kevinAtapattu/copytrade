"use client";
import Link from "next/link";
import { useState } from "react";
import { Col, Err, Seg, Table } from "@/components/ui";
import { CompareSummary, Score, Wallet, apiSend, useApi } from "@/lib/api";
import { ago, dur, num, pct, pnlClass, short, spct, usd } from "@/lib/format";
import { STATUS_HELP, STATUS_LABEL, help } from "@/lib/glossary";

type Row = Wallet & { scores: Record<string, Record<string, Score>>; open_positions: number; compare?: CompareSummary; trades_24h: number; quiet?: boolean };

export default function WalletsPage() {
  const [delay, setDelay] = useState(2);
  const [rankBy, setRankBy] = useState<"total" | "best" | "active">("total");
  const [win, setWin] = useState(14);
  const [showDropped, setShowDropped] = useState(false);
  const [more, setMore] = useState(false);
  const { data, error, reload, loading } = useApi<Row[]>(`wallets${showDropped ? "?dropped=1" : ""}`);
  const [addr, setAddr] = useState("");
  const [label, setLabel] = useState("");
  const [msg, setMsg] = useState<string | null>(null);

  const sc = (r: Row) => r.scores?.[String(delay)]?.[String(win)];
  const act = async (fn: () => Promise<unknown>, ok: string) => {
    try { await fn(); setMsg(ok); reload(); } catch (e) { setMsg(String((e as Error).message)); }
  };
  // Ranking: "best" = our avg return per copied token at the selected delay (quality),
  // "active" = their trades in the last 24h, "total" = our total paper PnL (quality x activity).
  const rankVal = (r: Row): number => {
    const d = String(delay), c = r.compare;
    if (rankBy === "active") return r.trades_24h ?? 0;
    if (!c?.copied?.[d]) return -1e9; // no copies yet: bottom
    return rankBy === "best" ? c.our_avg_pct[d] : c.our_pnl_usd[d];
  };
  const ranked = (rs: Row[]) => [...rs].sort((a, b) => rankVal(b) - rankVal(a) || (b.trades_24h ?? 0) - (a.trades_24h ?? 0));
  const cols: Col<Row>[] = [
    { key: "rank", label: "#", num: true, render: (r) => <span className="muted">{rankOf.get(r.address) ?? ""}</span> },
    { key: "wallet", label: "Wallet", render: (r) => (<>
      <Link href={`/wallets/${r.address}`} title={r.address}>
        {r.label ? <><b>{r.label}</b> <span className="mono muted hide-sm">{short(r.address)}</span></> : <span className="mono">{short(r.address)}</span>}
      </Link>
      {r.quiet && <> <span className="badge" title={help("lowData")}>low-data</span></>}
    </>) },
    { key: "tot", label: `Our total +${delay}s`, help: "What all our $25 paper copies of this wallet made in the last 14 days, closed plus open.", num: true, sort: (r) => r.compare?.our_pnl_usd?.[String(delay)] ?? -1e9,
      render: (r) => { const v = r.compare?.our_pnl_usd?.[String(delay)]; return v == null ? <span className="muted">-</span> : <span className={pnlClass(v)}>{usd(v, true)}</span>; } },
    { key: "score", label: "Score", help: help("score"), num: true, sort: (r) => sc(r)?.score ?? -1e9, render: (r) => sc(r)?.positions ? <b className={pnlClass(sc(r)?.score)}>{num(sc(r)?.score)}</b> : <span className="muted">-</span> },
    { key: "pnl", label: `Paper PnL ${win}d`, help: help("simPnl"), num: true, sort: (r) => sc(r)?.pnl_usd ?? -1e9, render: (r) => <span className={pnlClass(sc(r)?.pnl_usd)}>{usd(sc(r)?.pnl_usd, true)}</span> },
    { key: "exbest", label: "Without best trade", help: help("exBest"), num: true, sort: (r) => sc(r)?.pnl_ex_best_usd ?? -1e9,
      render: (r) => <span className={pnlClass(sc(r)?.pnl_ex_best_usd)}>{usd(sc(r)?.pnl_ex_best_usd, true)}{sc(r)?.lucky_trade ? " (lucky)" : ""}</span> },
    { key: "vs", label: "Them vs us (14d)", help: help("themVsUs"), num: true, sort: (r) => r.compare?.our_avg_pct?.[String(delay)] ?? -1e9,
      render: (r) => {
        const c = r.compare, d = String(delay), n = c?.copied?.[d] ?? 0;
        if (!c || !n) return <span className="muted">{c?.tokens ? `${c.tokens} tokens, no copies yet` : "no trades yet"}</span>;
        return (
          <span title={`Average return per token over the ${n} tokens we copied at +${delay}s. Them = their own trades; us = our $25 paper copy after delay, fees and slippage.`}>
            <span className={pnlClass(c.their_same_pct[d])}>{spct(c.their_same_pct[d])}</span>
            <span className="muted"> vs </span>
            <b className={pnlClass(c.our_avg_pct[d])}>{spct(c.our_avg_pct[d])}</b>
            <span className="muted"> · {n}</span>
          </span>
        );
      } },
    { key: "t24", label: "Trades 24h", help: "How many trades they made in the last 24 hours.", num: true, sort: (r) => r.trades_24h ?? 0, render: (r) => r.trades_24h ?? 0 },
    { key: "hold", label: "Their typical hold", help: help("hold"), num: true, sort: (r) => r.compare?.hold_median_s ?? -1,
      render: (r) => (r.compare?.hold_median_s ? dur(r.compare.hold_median_s) : <span className="muted">-</span>) },
    { key: "win", label: "Won (closed)", help: `${help("winRate")} Open copies don't count until they're sold.`, num: true, sort: (r) => sc(r)?.win_rate ?? 0,
      render: (r) => sc(r)?.closed ? <span title={`${sc(r)!.closed} closed copies`}>{pct(sc(r)?.win_rate, 0)}</span> : <span className="muted">-</span> },
    { key: "status", label: "Status", sort: (r) => (r.status === "live" ? 0 : r.status === "dropped" ? 2 : 1),
      render: (r) => <span className="row nowrap">
        <span className={`badge ${r.status === "paused" ? "bench" : r.status}`} title={`${STATUS_HELP[r.status] ?? ""}${r.reason ? `\n\nLast change: ${r.reason}` : ""}`}>{STATUS_LABEL[r.status] ?? r.status}</span>
        {r.quiet && <span className="badge" title={help("lowData")}>low-data feed</span>}
      </span> },
    { key: "dd", label: "Max drawdown", help: help("drawdown"), num: true, sort: (r) => sc(r)?.max_drawdown ?? 0, render: (r) => pct(sc(r)?.max_drawdown) },
    { key: "ahold", label: "Our avg hold", help: help("hold"), num: true, sort: (r) => sc(r)?.avg_hold_s ?? 0, render: (r) => dur(sc(r)?.avg_hold_s) },
    { key: "tpd", label: "Trades/day", num: true, sort: (r) => sc(r)?.trades_per_day ?? 0, render: (r) => num(sc(r)?.trades_per_day, 1) },
    { key: "n", label: "Positions", num: true, sort: (r) => sc(r)?.positions ?? 0, render: (r) => sc(r)?.positions ?? 0 },
    { key: "days", label: "History", num: true, sort: (r) => sc(r)?.active_days ?? 0, render: (r) => `${num(sc(r)?.active_days, 1)}d` },
    { key: "last", label: "Last trade", sort: (r) => r.last_trade_at ?? "", render: (r) => ago(r.last_trade_at) },
    { key: "src", label: "Found by", render: (r) => <span className="muted" title={r.reason}>{r.source}</span> },
    { key: "act", label: "", render: (r) => (
      <div className="row nowrap">
        {(r.status === "bench" || r.status === "paused") && <button className="btn small primary" title="Let the auto-trader copy this wallet (paper until live trading is on)"
          onClick={() => confirm(`Make ${r.label || short(r.address)} live? The auto-trader will start copying it.`) && act(() => apiSend(`wallets/${r.address}/status`, "POST", { status: "live" }), `${r.label || short(r.address)} is now live`)}>Make live</button>}
        {r.status === "live" && <button className="btn small" title="Stop the auto-trader copying it; keep watching on paper" onClick={() => act(() => apiSend(`wallets/${r.address}/status`, "POST", { status: "bench" }), `${r.label || short(r.address)} moved back to watching`)}>Stop live</button>}
        {r.status !== "dropped"
          ? <button className="btn small danger" title="Stop tracking this wallet" onClick={() => confirm(`Stop tracking ${r.label || r.address}? You can re-add it later.`) && act(() => apiSend(`wallets/${r.address}`, "DELETE"), `${r.label || short(r.address)} removed`)}>Remove</button>
          : <button className="btn small" onClick={() => act(() => apiSend(`wallets`, "POST", { address: r.address }), "re-added")}>Re-add</button>}
      </div>
    ) },
  ];
  // Core columns first; the rest behind "More columns" so a new trader isn't buried.
  // Status lives in the section headings (Live / Watching); it's a column only under More columns.
  const core = new Set(["rank", "wallet", "tot", "vs", "win", "t24", "hold", "score", "act"]);
  const shown = more ? cols : cols.filter((c) => core.has(c.key));
  const live = ranked((data ?? []).filter((r) => r.status === "live"));
  const rest = ranked((data ?? []).filter((r) => r.status !== "live"));
  const rankOf = new Map<string, number>();
  [live, rest].forEach((list) => list.forEach((r, i) => rankOf.set(r.address, i + 1)));
  return (
    <>
      <div className="page-head">
        <div>
          <h2>Wallets</h2>
          <div className="sub">Ranked by what copying each one would have made us, {delay}s late, after fees. Hover a column name for what it means.</div>
        </div>
      </div>
      <div className="toolbar">
        <label className="field"><span>Rank by</span>
          <Seg value={rankBy} onChange={setRankBy} options={[
            { v: "total", label: "Most profit", title: "Our total paper profit copying them" },
            { v: "best", label: "Best per trade", title: "Our average return per token copied" },
            { v: "active", label: "Most active", title: "Their trades in the last 24 hours" }]} />
        </label>
        <label className="field"><span>Copy delay</span>
          <Seg value={delay} onChange={setDelay} options={[2, 10, 60].map((d) => ({ v: d, label: `+${d}s`, title: help("delay") }))} />
        </label>
        <label className="field"><span>Window</span>
          <Seg value={win} onChange={setWin} options={[{ v: 14, label: "14 days" }, { v: 30, label: "30 days" }]} />
        </label>
        <label className="check"><input type="checkbox" checked={more} onChange={(e) => setMore(e.target.checked)} /> More columns</label>
        <label className="check"><input type="checkbox" checked={showDropped} onChange={(e) => setShowDropped(e.target.checked)} /> Show removed</label>
      </div>
      <Err error={error} />
      {msg && <div className="note" role="status">{msg}</div>}
      <div className="card" style={{ marginBottom: 14 }}>
        <h3><span className="term" title={help("liveWallet")}>Live</span> ({live.length})</h3>
        <Table rows={live} cols={shown} rowKey={(r) => r.address} loading={loading}
          empty="No live wallets yet. Make a wallet live once it has stayed profitable on paper for 2+ weeks." />
      </div>
      <div className="card" style={{ marginBottom: 14 }}>
        <h3><span className="term" title={help("watching")}>Watching</span> ({rest.length})</h3>
        <Table rows={rest} cols={shown} rowKey={(r) => r.address} loading={loading} empty="No wallets yet. Add one below, or let Discovery find some." />
      </div>
      <details className="card">
        <summary>Add a wallet to watch</summary>
        <form className="row" style={{ marginTop: 12 }} onSubmit={(e) => {
          e.preventDefault();
          act(() => apiSend("wallets", "POST", { address: addr.trim(), label: label.trim() }), `${label.trim() || "Wallet"} added`).then(() => { setAddr(""); setLabel(""); });
        }}>
          <input placeholder="Solana wallet address" aria-label="Solana wallet address" value={addr} onChange={(e) => setAddr(e.target.value)} className="grow" required minLength={32} />
          <input placeholder="Name (optional)" aria-label="Name (optional)" value={label} onChange={(e) => setLabel(e.target.value)} />
          <button className="btn primary" type="submit" disabled={!addr.trim()}>Start watching</button>
        </form>
      </details>
    </>
  );
}
