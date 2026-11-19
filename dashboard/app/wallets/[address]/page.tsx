"use client";
import Link from "next/link";
import { useParams } from "next/navigation";
import { useState } from "react";
import LineChart, { mergeSeries } from "@/components/LineChart";
import { Calendar, DayDetail, MonthGrid, fmtDay, keptHint, monthTotals, useMonth } from "@/components/PnlCalendar";
import { Err, Seg, Stat, Table } from "@/components/ui";
import { CompareRow, CompareSummary, Point, Rotation, Score, SimPosition, SimTrade, Trade, Wallet, useApi } from "@/lib/api";
import { ago, dur, num, pct, pnlClass, price, short, spct, time, usd } from "@/lib/format";
import { STATUS_HELP, STATUS_LABEL } from "@/lib/glossary";

type Detail = {
  wallet: Wallet; scores: Score[]; score_history: Record<string, Score[]>; equity: Record<string, Point[]>;
  trades: (Trade & { price: number; sims: Record<string, SimTrade> })[]; positions: SimPosition[]; rotations: Rotation[] | null;
  compare: CompareRow[] | null; compare_summary: CompareSummary;
};

const DELAYS = [2, 10, 60];
const COLORS: Record<number, string> = { 2: "var(--series-1)", 10: "var(--series-2)", 60: "var(--series-3)" };

export default function WalletDetail() {
  const { address } = useParams<{ address: string }>();
  const { data, error } = useApi<Detail>(`wallets/${address}`);
  const s = (d: number, w: number) => data?.scores?.find((x) => x.delay_s === d && x.window_days === w);
  const rows = mergeSeries(Object.fromEntries(DELAYS.map((d) => [`d${d}`, data?.equity?.[String(d)]])));

  return (
    <>
      <div className="page-head">
        <div>
          <h2>{data?.wallet.label || "Wallet"}</h2>
          <div className="sub mono addr">{address}</div>
          <div className="sub">
            {data && <><span className={`badge ${data.wallet.status === "paused" ? "bench" : data.wallet.status}`} title={STATUS_HELP[data.wallet.status]}>{STATUS_LABEL[data.wallet.status] ?? data.wallet.status}</span> found by {data.wallet.source} · last trade {ago(data.wallet.last_trade_at)}</>}
          </div>
        </div>
        <div className="row">
          <a className="btn" href={`https://solscan.io/account/${address}`} target="_blank" rel="noreferrer">Solscan</a>
          <Link className="btn" href="/wallets">All wallets</Link>
        </div>
      </div>
      <Err error={error} />
      <WalletCalendar address={address} name={data?.wallet.label || short(address)} />
      <div className="grid stats" style={{ marginTop: 14 }}>
        {DELAYS.map((d) => (
          <Stat key={d} label={`Paper PnL, 14 days, +${d}s`} value={usd(s(d, 14)?.pnl_usd, true)} tone={s(d, 14)?.pnl_usd}
            hint={`score ${num(s(d, 14)?.score)} · ex-best ${usd(s(d, 14)?.pnl_ex_best_usd, true)} · DD ${pct(s(d, 14)?.max_drawdown)}`} />
        ))}
        <Stat label="Win rate +2s (14d)" value={pct(s(2, 14)?.win_rate, 0)} hint={`${s(2, 14)?.positions ?? 0} positions, avg hold ${dur(s(2, 14)?.avg_hold_s)}`} />
      </div>
      <div className="card" style={{ marginTop: 14 }}>
        <h3>Them vs us, per token</h3>
        <div className="sub" style={{ marginBottom: 10 }}>
          Their return on each token from their own buys and sells, next to our $25 paper copy at each delay (after delay, fees and slippage).
          Open positions are marked at the latest price.
        </div>
        <div className="grid stats" style={{ marginBottom: 12 }}>
          {DELAYS.map((d) => {
            const cs = data?.compare_summary, k = String(d), n = cs?.copied?.[k] ?? 0;
            return (
              <Stat key={d} label={`Avg per token at +${d}s`}
                value={n ? <><span className={pnlClass(cs!.their_same_pct[k])}>{spct(cs!.their_same_pct[k])}</span> <span className="muted">vs</span> <span className={pnlClass(cs!.our_avg_pct[k])}>{spct(cs!.our_avg_pct[k])}</span></> : "-"}
                hint={n ? `them vs us over ${n} copied tokens` : "no copies yet"} />
            );
          })}
          <Stat label="Tokens they traded" value={String(data?.compare_summary?.tokens ?? 0)}
            hint={`their avg ${spct(data?.compare_summary?.their_avg_pct)} on all of them`} />
        </div>
        <Table rows={data?.compare ?? []} rowKey={(r) => r.mint} empty="No pump.fun trades from this wallet yet" cols={[
          { key: "at", label: "Their buy", sort: (r) => r.first_buy_at, render: (r) => time(r.first_buy_at) },
          { key: "mint", label: "Token", render: (r) => <a className="mono" href={`https://pump.fun/coin/${r.mint}`} target="_blank" rel="noreferrer">{short(r.mint)}</a> },
          { key: "in", label: "They spent", num: true, sort: (r) => r.their_sol_in, render: (r) => `${num(r.their_sol_in, 2)} SOL` },
          { key: "them", label: "Them", num: true, sort: (r) => r.their_pct,
            render: (r) => <><b className={pnlClass(r.their_pct)}>{spct(r.their_pct)}</b>{r.their_open && <span className="muted"> holding</span>}</> },
          ...DELAYS.map((d) => ({
            key: `u${d}`, label: `Us +${d}s`, num: true, sort: (r: CompareRow) => r.ours?.[String(d)]?.pnl_pct ?? -1e9,
            render: (r: CompareRow) => {
              const c = r.ours?.[String(d)];
              if (!c) return <span className="muted" title="Not copied: skipped (late, non-SOL, migrated) or still pending">-</span>;
              return <span title={`${usd(c.pnl_usd, true)} on ${num(c.cost_sol, 3)} SOL`}><span className={pnlClass(c.pnl_pct)}>{spct(c.pnl_pct)}</span>{c.open && <span className="muted"> open</span>}</span>;
            },
          })),
        ]} initialSort={{ key: "at", desc: true }} />
      </div>
      <div className="card" style={{ marginTop: 14 }}>
        <h3>Paper profit over time, by copy delay (30 days)</h3>
        <LineChart rows={rows} series={DELAYS.map((d) => ({ key: `d${d}`, label: `+${d}s`, color: COLORS[d] }))} />
      </div>
      <div className="card" style={{ marginTop: 14 }}>
        <h3>Their trades and our paper copies</h3>
        <Table rows={data?.trades ?? []} rowKey={(t) => t.signature} empty="No trades seen yet" cols={[
          { key: "at", label: "Time", render: (t) => time(t.received_at) },
          { key: "side", label: "Side", render: (t) => <span className={t.side === "buy" ? "pos" : "neg"}>{t.side}</span> },
          { key: "mint", label: "Token", render: (t) => <a className="mono" href={`https://pump.fun/coin/${t.mint}`} target="_blank" rel="noreferrer">{short(t.mint)}</a> },
          { key: "sol", label: "Their SOL", num: true, render: (t) => t.pool === "nonsol" ? <span className="muted" title="Paid in another coin, not SOL">-</span> : num(t.sol_amount, 3) },
          { key: "px", label: "Their price", num: true, render: (t) => <span className="mono">{price(t.price)}</span> },
          ...DELAYS.map((d) => ({
            key: `s${d}`, label: `+${d}s fill`, num: true,
            render: (t: Detail["trades"][number]) => {
              const x = t.sims?.[String(d)];
              if (!x) return <span className="muted">pending</span>;
              if (x.skipped) return <span className="muted" title={x.skip_reason}>skip</span>;
              const gap = t.price ? (x.fill_price / t.price - 1) * (t.side === "buy" ? 1 : -1) : 0;
              return (
                <span title={`slippage ${x.slippage_pct.toFixed(2)}%, pump fee ${x.pump_fee_sol.toFixed(5)} SOL`}>
                  <span className="mono">{price(x.fill_price)}</span> <span className={pnlClass(-gap)}>{gap >= 0 ? "+" : ""}{(gap * 100).toFixed(1)}%</span>
                </span>
              );
            },
          })),
          { key: "sig", label: "Tx", render: (t) => <a className="mono muted" href={`https://solscan.io/tx/${t.signature}`} target="_blank" rel="noreferrer">{short(t.signature)}</a> },
        ]} />
        <div className="sub" style={{ marginTop: 8 }}>Percent = how much worse our all-in price was than theirs (fees + delay + slippage).</div>
      </div>
      <div className="grid two" style={{ marginTop: 14 }}>
        <div className="card">
          <h3>Our paper positions (30 days)</h3>
          <Table rows={data?.positions ?? []} rowKey={(p) => p.id} initialSort={{ key: "opened", desc: true }} cols={[
            { key: "d", label: "Delay", sort: (p) => p.delay_s, render: (p) => `+${p.delay_s}s` },
            { key: "mint", label: "Token", render: (p) => <span className="mono">{short(p.mint)}</span> },
            { key: "opened", label: "Opened", sort: (p) => p.opened_at, render: (p) => time(p.opened_at) },
            { key: "st", label: "Status", render: (p) => p.status },
            { key: "pnl", label: "Realized", num: true, sort: (p) => (p.proceeds_sol - p.cost_sol) * p.sol_usd,
              render: (p) => p.status === "open" ? <span className="muted">open</span> :
                <span className={pnlClass(p.proceeds_sol - p.cost_sol)}>{usd((p.proceeds_sol - p.cost_sol) * p.sol_usd, true)}</span> },
          ]} />
        </div>
        <div className="card">
          <h3>Status history</h3>
          <Table rows={data?.rotations ?? []} rowKey={(r) => String(r.id)} empty="No status changes" cols={[
            { key: "at", label: "When", render: (r) => time(r.at) },
            { key: "ch", label: "Change", render: (r) => `${STATUS_LABEL[r.from_status] ?? r.from_status} to ${STATUS_LABEL[r.to_status] ?? r.to_status}` },
            { key: "why", label: "Reason", render: (r) => <span className="muted">{r.reason}</span> },
          ]} />
        </div>
      </div>
    </>
  );
}

// Their day-by-day P&L next to ours, the same numbers the P&L calendar shows for one wallet.
function WalletCalendar({ address, name }: { address: string; name: string }) {
  const [delay, setDelay] = useState(2);
  const [sel, setSel] = useState<string | null>(null);
  const q = `calendar?delay=${delay}&wallet=${address}`;
  const { data, error } = useApi<Calendar>(q, 60000);
  const { data: detail } = useApi<Calendar>(sel ? `${q}&date=${sel}` : null, 60000);
  const today = data?.today ?? "";
  const month = useMonth(today, data?.days);
  const { us, them, green, best, worst } = monthTotals(month.monthDays);
  const n = month.monthDays.length;
  return (
    <>
      <div className="card">
        <div className="row" style={{ justifyContent: "space-between", alignItems: "flex-start", marginBottom: 12 }}>
          <div className="grow">
            <h3>Daily P&amp;L: them vs us</h3>
            <div className="sub">
              <b>Them</b> is what {name}&apos;s own trades made, scaled to $25 a trade. <b>Us</b> is our $25 copy {delay}s late
              after fees. Counted on the day each copy closed; open copies show up once they close. Click a day for its trades.
            </div>
          </div>
          <Seg value={delay} onChange={(d) => { setDelay(d); setSel(null); }} options={[2, 10, 60].map((d) => ({ v: d, label: `+${d}s` }))} />
        </div>
        <Err error={error} />
        {!data ? !error && <div className="empty">Loading...</div> : (
          <>
            <div className="grid stats" style={{ margin: "12px 0" }}>
              <Stat label={`${month.name}, them`} value={usd(them, true)} tone={them} hint={n ? `on ${n} day${n > 1 ? "s" : ""} with closes` : "no closes yet"} />
              <Stat label={`${month.name}, us`} value={usd(us, true)} tone={us} hint={n ? keptHint(us, them) : " "} />
              <Stat label="Green days (us)" value={n ? `${green} of ${n}` : "-"} hint={best && worst ? `best ${usd(best.us_usd, true)} ${fmtDay(best.date)}, worst ${usd(worst.us_usd, true)}` : " "} />
              <Stat label="Still open" value={usd(data.open_usd, true)} tone={data.open_usd} hint={`${data.open ?? 0} copies, not counted yet`} />
            </div>
            <MonthGrid days={data.days ?? []} today={today} month={month} sel={sel} onSelect={setSel} />
          </>
        )}
      </div>
      {sel && <DayDetail key={`${sel}${q}`} date={sel} day={detail?.day} name={() => name} onClose={() => setSel(null)} />}
    </>
  );
}
