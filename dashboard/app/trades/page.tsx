"use client";
import Link from "next/link";
import { useState } from "react";
import { Err, Seg, Table } from "@/components/ui";
import { LiveFill, SimTrade, Trade, Wallet, useApi } from "@/lib/api";
import { num, pnlClass, price, short, sol } from "@/lib/format";
import { help } from "@/lib/glossary";

// Today: "9:43 PM"; earlier: "Oct 23, 9:43 PM".
const when = (iso?: string) => {
  if (!iso) return "-";
  const d = new Date(iso), today = new Date().toDateString() === d.toDateString();
  return d.toLocaleString([], today ? { hour: "numeric", minute: "2-digit", second: "2-digit" } : { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
};

type Log = { target_trades: Trade[] | null; sim_trades: SimTrade[] | null; live_fills: LiveFill[] | null };

export default function TradesPage() {
  const [tab, setTab] = useState<"target" | "sim" | "live">("target");
  const [delay, setDelay] = useState(0);
  const [wallet, setWallet] = useState("");
  const [copyable, setCopyable] = useState(true);
  const { data, error, loading } = useApi<Log>(`trades?limit=300&delay=${delay}${wallet ? `&wallet=${wallet}` : ""}`);
  const { data: wallets } = useApi<Wallet[]>("wallets?dropped=1", 60000);
  const names = new Map((wallets ?? []).map((w) => [w.address, w.label]));
  // Wallet name (or short address) linking to its page.
  const who = (addr: string) => (
    <Link href={`/wallets/${addr}`} title={addr}>
      {names.get(addr) ? <b>{names.get(addr)}</b> : <span className="mono">{short(addr)}</span>}
    </Link>
  );
  return (
    <>
      <div className="page-head">
        <div>
          <h2>Trade log</h2>
          <div className="sub">
            {tab === "target" ? "Every trade the wallets we watch made, newest first." : tab === "sim" ? "Our $25 paper copy of each of their trades, at each delay." : "Orders the auto-trader placed (paper or real)."} Latest 300.
          </div>
        </div>
      </div>
      <div className="toolbar">
        <Seg value={tab} onChange={setTab} options={[{ v: "target", label: "Their trades" }, { v: "sim", label: "Our paper copies" }, { v: "live", label: "Auto-trader" }]} />
          <select value={wallet} onChange={(e) => setWallet(e.target.value)} aria-label="Filter by wallet">
            <option value="">All wallets</option>
            {[...(wallets ?? [])].sort((a, b) => (a.label || a.address).localeCompare(b.label || b.address)).map((w) => (
              <option key={w.address} value={w.address}>{w.label || short(w.address)}</option>
            ))}
          </select>
          {tab === "sim" && <label className="field"><span>Delay</span><Seg value={delay} onChange={setDelay} options={[0, 2, 10, 60].map((d) => ({ v: d, label: d ? `+${d}s` : "All", title: help("delay") }))} /></label>}
          {tab === "target" && <label className="check" title={help("nonsol")}><input type="checkbox" checked={copyable} onChange={(e) => setCopyable(e.target.checked)} /> Only copyable tokens</label>}
      </div>
      <Err error={error} />
      <div className="card">
        {tab === "target" && (
          <Table rows={(data?.target_trades ?? []).filter((t) => !copyable || t.pool !== "nonsol")} rowKey={(t) => t.signature + t.trader} loading={loading}
            empty={copyable ? "No copyable trades yet. Untick \"Only copyable tokens\" to see all." : "No trades yet."} cols={[
            { key: "at", label: "Time", render: (t) => when(t.received_at) },
            { key: "w", label: "Wallet", render: (t) => who(t.trader) },
            { key: "side", label: "Side", render: (t) => <span className={t.side === "buy" ? "pos" : "neg"}>{t.side}</span> },
            { key: "mint", label: "Token", render: (t) => <span className="mono">{short(t.mint)}</span> },
            { key: "sol", label: "SOL", num: true, render: (t) => t.pool === "nonsol" ? <span className="muted" title="Paid in another coin, not SOL">-</span> : num(t.sol_amount, 3) },
            { key: "tok", label: "Tokens", num: true, render: (t) => num(t.token_amount, 0) },
            { key: "px", label: "Price after", help: "Token price in SOL right after their trade.", num: true, render: (t) => <span className="mono">{price(t.v_sol / t.v_tokens)}</span> },
            { key: "pool", label: "Copyable", help: help("nonsol"), render: (t) => t.pool === "nonsol" ? <span className="muted">no, other coin</span> : "yes" },
            { key: "sig", label: "Tx", render: (t) => t.signature.startsWith("exit-check:")
              ? <span className="muted" title="They no longer hold this token but we saw no sell (moved to another account, or sold on an exchange we don't read). Treated as a full exit.">exit found by balance check</span>
              : <a className="mono muted" href={`https://solscan.io/tx/${t.signature}`} target="_blank" rel="noreferrer">{short(t.signature)}</a> },
          ]} />
        )}
        {tab === "sim" && (
          <Table rows={data?.sim_trades ?? []} rowKey={(s) => s.id} loading={loading} empty="No paper copies yet." cols={[
            { key: "at", label: "Time", render: (s) => when(s.fill_at) },
            { key: "d", label: "Delay", help: help("delay"), render: (s) => `+${s.delay_s}s` },
            { key: "w", label: "Wallet", render: (s) => who(s.wallet) },
            { key: "side", label: "Side", render: (s) => <span className={s.side === "buy" ? "pos" : "neg"}>{s.side}</span> },
            { key: "mint", label: "Token", render: (s) => <span className="mono">{short(s.mint)}</span> },
            { key: "sol", label: "SOL", num: true, render: (s) => (s.skipped ? "-" : sol(s.sol_amount)) },
            { key: "px", label: "Fill price", num: true, render: (s) => <span className="mono">{price(s.fill_price)}</span> },
            { key: "tp", label: "Their price", num: true, render: (s) => <span className="mono">{price(s.target_price)}</span> },
            { key: "slip", label: "Slippage", help: help("slippage"), num: true, render: (s) => (s.skipped ? "-" : `${s.slippage_pct.toFixed(2)}%`) },
            { key: "fee", label: "Fees SOL", num: true, render: (s) => (s.skipped ? "-" : (s.pump_fee_sol + s.bot_fee_sol + s.network_fee_sol).toFixed(5)) },
            { key: "note", label: "Note", render: (s) => <span className="muted">{s.skipped ? `not copied: ${SKIP[s.skip_reason ?? ""] ?? s.skip_reason}` : s.skip_reason || (s.sell_fraction ? `sold ${(s.sell_fraction * 100).toFixed(0)}% like they did` : "")}</span> },
          ]} />
        )}
        {tab === "live" && (
          <Table rows={data?.live_fills ?? []} rowKey={(f) => f.id} loading={loading} empty="No auto-trader orders yet. It only copies wallets you make live." cols={[
            { key: "at", label: "Sent", render: (f) => when(f.sent_at) },
            { key: "m", label: "Mode", render: (f) => <span className="badge">{f.mode}</span> },
            { key: "st", label: "Status", render: (f) => <span className={`badge ${f.status === "failed" ? "failed" : ""}`} title={f.error}>{f.status}</span> },
            { key: "w", label: "Wallet", render: (f) => who(f.wallet) },
            { key: "side", label: "Side", render: (f) => `${f.side}${f.reason !== "copy" ? ` (${f.reason})` : ""}` },
            { key: "sol", label: "SOL", num: true, render: (f) => sol(f.sol_amount) },
            { key: "px", label: "Fill price", num: true, render: (f) => <span className="mono">{price(f.fill_price)}</span> },
            { key: "sim", label: "Paper price", num: true, render: (f) => <span className="mono">{price(f.sim_fill_price)}</span> },
            { key: "gap", label: "Gap vs paper", help: "How much worse (+) or better (-) the real price was than the paper copy's.", num: true, render: (f) => {
              if (!f.sim_fill_price || !f.fill_price) return "-";
              const g = (f.fill_price / f.sim_fill_price - 1) * (f.side === "buy" ? 1 : -1) * 100;
              return <span className={pnlClass(-g)}>{g >= 0 ? "+" : ""}{g.toFixed(1)}%</span>;
            } },
            { key: "tries", label: "Tries", num: true, render: (f) => f.attempts },
            { key: "sig", label: "Tx", render: (f) => f.signature ? <a className="mono muted" href={`https://solscan.io/tx/${f.signature}`} target="_blank">{short(f.signature)}</a> : "-" },
          ]} />
        )}
      </div>
    </>
  );
}

// Plain reasons a trade wasn't copied.
const SKIP: Record<string, string> = {
  "already in position": "we already hold their first buy of this token ($25 per token, no add-ons)",
  "no curve state": "no price data for this token yet",
};
