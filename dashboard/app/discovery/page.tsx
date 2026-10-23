"use client";
import Link from "next/link";
import { Err, Table } from "@/components/ui";
import { Wallet, apiSend, useApi } from "@/lib/api";
import { num, short, sol, time } from "@/lib/format";

type Hit = { id: number; wallet: string; mint: string; token_mult: number; entry_price: number; exit_price: number;
  exit_mult: number; profit_sol: number; found_at: string; wallet_status: string };
type Tok = { mint: string; first_seen: string; first_price: number; max_price: number; last_price: number; discovered: boolean };
type Disc = { hits: Hit[] | null; candidates: Wallet[] | null; tokens_5x: Tok[] | null };

export default function DiscoveryPage() {
  const { data, error, reload } = useApi<Disc>("discovery", 30000);
  return (
    <>
      <div className="page-head">
        <div><h2>Discovery queue</h2><div className="sub">Early buyers who exited well on tokens that ran 5x+. Repeat names join the bench automatically.</div></div>
      </div>
      <Err error={error} />
      <div className="card" style={{ marginBottom: 14 }}>
        <h3>Discovered wallets ({data?.candidates?.length ?? 0})</h3>
        <Table rows={data?.candidates ?? []} rowKey={(w) => w.address} empty="None yet" cols={[
          { key: "st", label: "Status", render: (w) => <span className={`badge ${w.status}`}>{w.status}</span> },
          { key: "w", label: "Wallet", render: (w) => <Link className="mono" href={`/wallets/${w.address}`}>{short(w.address)}</Link> },
          { key: "added", label: "Added", sort: (w) => w.added_at, render: (w) => time(w.added_at) },
          { key: "why", label: "Reason", render: (w) => <span className="muted">{w.reason}</span> },
          { key: "a", label: "", render: (w) => w.status === "dropped"
            ? <button className="btn small" onClick={() => apiSend("wallets", "POST", { address: w.address }).then(reload)}>Re-add</button>
            : <button className="btn small danger" onClick={() => apiSend(`wallets/${w.address}`, "DELETE").then(reload)}>Reject</button> },
        ]} />
      </div>
      <div className="grid two">
        <div className="card">
          <h3>Hits (one per wallet per token)</h3>
          <Table rows={data?.hits ?? []} rowKey={(h) => String(h.id)} empty="No hits yet" initialSort={{ key: "x", desc: true }} cols={[
            { key: "w", label: "Wallet", render: (h) => <span className="mono">{short(h.wallet)}</span> },
            { key: "s", label: "Status", render: (h) => h.wallet_status ? <span className={`badge ${h.wallet_status}`}>{h.wallet_status}</span> : <span className="muted">not tracked</span> },
            { key: "m", label: "Token", render: (h) => <span className="mono">{short(h.mint)}</span> },
            { key: "t", label: "Token ran", num: true, sort: (h) => h.token_mult, render: (h) => `${num(h.token_mult, 1)}x` },
            { key: "x", label: "Their exit", num: true, sort: (h) => h.exit_mult, render: (h) => `${num(h.exit_mult, 1)}x` },
            { key: "p", label: "Profit", num: true, sort: (h) => h.profit_sol, render: (h) => sol(h.profit_sol) },
          ]} />
        </div>
        <div className="card">
          <h3>Tokens that ran 5x+</h3>
          <Table rows={data?.tokens_5x ?? []} rowKey={(t) => t.mint} empty="None yet" initialSort={{ key: "x", desc: true }} cols={[
            { key: "m", label: "Token", render: (t) => <a className="mono" href={`https://pump.fun/coin/${t.mint}`} target="_blank">{short(t.mint)}</a> },
            { key: "seen", label: "First seen", render: (t) => time(t.first_seen) },
            { key: "x", label: "Peak", num: true, sort: (t) => t.max_price / t.first_price, render: (t) => `${num(t.max_price / t.first_price, 1)}x` },
            { key: "now", label: "Now", num: true, render: (t) => `${num(t.last_price / t.first_price, 1)}x` },
            { key: "d", label: "Scanned", render: (t) => (t.discovered ? "yes" : "pending") },
          ]} />
        </div>
      </div>
    </>
  );
}
