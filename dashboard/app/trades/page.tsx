"use client";
import Link from "next/link";
import { useState } from "react";
import { Err, Seg, Table } from "@/components/ui";
import { LiveFill, SimTrade, Trade, Wallet, useApi } from "@/lib/api";
import { num, pnlClass, price, short, sol, time } from "@/lib/format";

type Log = { target_trades: Trade[] | null; sim_trades: SimTrade[] | null; live_fills: LiveFill[] | null };

export default function TradesPage() {
  const [tab, setTab] = useState<"target" | "sim" | "live">("target");
  const [delay, setDelay] = useState(0);
  const [wallet, setWallet] = useState("");
  const { data, error } = useApi<Log>(`trades?limit=300&delay=${delay}${wallet ? `&wallet=${wallet}` : ""}`);
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
        <div><h2>Trade log</h2><div className="sub">Latest 300 of each</div></div>
        <div className="row">
          <Seg value={tab} onChange={setTab} options={[{ v: "target", label: "Their trades" }, { v: "sim", label: "Our paper copies (sim)" }, { v: "live", label: "Executor (paper / live)" }]} />
          <select value={wallet} onChange={(e) => setWallet(e.target.value)} aria-label="Filter by wallet">
            <option value="">All wallets</option>
            {[...(wallets ?? [])].sort((a, b) => (a.label || a.address).localeCompare(b.label || b.address)).map((w) => (
              <option key={w.address} value={w.address}>{w.label || short(w.address)}</option>
            ))}
          </select>
          {tab === "sim" && <Seg value={delay} onChange={setDelay} options={[0, 2, 10, 60].map((d) => ({ v: d, label: d ? `+${d}s` : "all" }))} />}
        </div>
      </div>
      <Err error={error} />
      <div className="card">
        {tab === "target" && (
          <Table rows={data?.target_trades ?? []} rowKey={(t) => t.signature + t.trader} cols={[
            { key: "at", label: "Time", render: (t) => time(t.received_at) },
            { key: "w", label: "Wallet", render: (t) => who(t.trader) },
            { key: "side", label: "Side", render: (t) => <span className={t.side === "buy" ? "pos" : "neg"}>{t.side}</span> },
            { key: "mint", label: "Token", render: (t) => <span className="mono">{short(t.mint)}</span> },
            { key: "sol", label: "SOL", num: true, render: (t) => num(t.sol_amount, 3) },
            { key: "tok", label: "Tokens", num: true, render: (t) => num(t.token_amount, 0) },
            { key: "px", label: "Price after", num: true, render: (t) => <span className="mono">{price(t.v_sol / t.v_tokens)}</span> },
            { key: "pool", label: "Pool", render: (t) => t.pool },
            { key: "sig", label: "Tx", render: (t) => <a className="mono muted" href={`https://solscan.io/tx/${t.signature}`} target="_blank">{short(t.signature)}</a> },
          ]} />
        )}
        {tab === "sim" && (
          <Table rows={data?.sim_trades ?? []} rowKey={(s) => s.id} cols={[
            { key: "at", label: "Fill time", render: (s) => time(s.fill_at) },
            { key: "d", label: "Delay", render: (s) => `+${s.delay_s}s` },
            { key: "w", label: "Wallet", render: (s) => who(s.wallet) },
            { key: "side", label: "Side", render: (s) => <span className={s.side === "buy" ? "pos" : "neg"}>{s.side}</span> },
            { key: "mint", label: "Token", render: (s) => <span className="mono">{short(s.mint)}</span> },
            { key: "sol", label: "SOL", num: true, render: (s) => (s.skipped ? "-" : sol(s.sol_amount)) },
            { key: "px", label: "Fill price", num: true, render: (s) => <span className="mono">{price(s.fill_price)}</span> },
            { key: "tp", label: "Their price", num: true, render: (s) => <span className="mono">{price(s.target_price)}</span> },
            { key: "slip", label: "Slippage", num: true, render: (s) => (s.skipped ? "-" : `${s.slippage_pct.toFixed(2)}%`) },
            { key: "fee", label: "Fees SOL", num: true, render: (s) => (s.skipped ? "-" : (s.pump_fee_sol + s.bot_fee_sol + s.network_fee_sol).toFixed(5)) },
            { key: "note", label: "Note", render: (s) => <span className="muted">{s.skipped ? `skipped: ${s.skip_reason}` : s.skip_reason || (s.sell_fraction ? `sold ${(s.sell_fraction * 100).toFixed(0)}%` : "")}</span> },
          ]} />
        )}
        {tab === "live" && (
          <Table rows={data?.live_fills ?? []} rowKey={(f) => f.id} empty="No executor fills yet" cols={[
            { key: "at", label: "Sent", render: (f) => time(f.sent_at) },
            { key: "m", label: "Mode", render: (f) => <span className="badge">{f.mode}</span> },
            { key: "st", label: "Status", render: (f) => <span className={`badge ${f.status === "failed" ? "failed" : ""}`} title={f.error}>{f.status}</span> },
            { key: "w", label: "Wallet", render: (f) => who(f.wallet) },
            { key: "side", label: "Side", render: (f) => `${f.side}${f.reason !== "copy" ? ` (${f.reason})` : ""}` },
            { key: "sol", label: "SOL", num: true, render: (f) => sol(f.sol_amount) },
            { key: "px", label: "Fill price", num: true, render: (f) => <span className="mono">{price(f.fill_price)}</span> },
            { key: "sim", label: "Sim price", num: true, render: (f) => <span className="mono">{price(f.sim_fill_price)}</span> },
            { key: "gap", label: "Gap vs sim", num: true, render: (f) => {
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
