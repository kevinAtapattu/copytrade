"use client";
import { useMemo, useState } from "react";
import { Err, Seg, Stat } from "@/components/ui";
import { useApi, useWalletNames } from "@/lib/api";
import { pnlClass, short, usd } from "@/lib/format";

type Day = { date: string; pnl_usd: number; closed: number; won: number };
type CalWallet = { address: string; pnl_usd: number; copies: number };
type Calendar = { tz: string; delay: number; today: string; days: Day[]; open_usd: number; open: number; wallets: CalWallet[] };

const WEEKDAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const pad = (n: number) => String(n).padStart(2, "0");
const key = (y: number, m: number, d: number) => `${y}-${pad(m + 1)}-${pad(d)}`;

export default function CalendarPage() {
  const [delay, setDelay] = useState(2);
  const [wallet, setWallet] = useState("");
  const { data, error } = useApi<Calendar>(`calendar?delay=${delay}${wallet ? `&wallet=${wallet}` : ""}`, 60000);
  const { names } = useWalletNames();
  const today = data?.today ?? new Date().toISOString().slice(0, 10);
  const [month, setMonth] = useState<{ y: number; m: number } | null>(null);
  const cur = month ?? { y: Number(today.slice(0, 4)), m: Number(today.slice(5, 7)) - 1 };

  const byDate = useMemo(() => new Map((data?.days ?? []).map((d) => [d.date, d])), [data]);
  const prefix = `${cur.y}-${pad(cur.m + 1)}`;
  const monthDays = (data?.days ?? []).filter((d) => d.date.startsWith(prefix));
  const total = monthDays.reduce((s, d) => s + d.pnl_usd, 0);
  const green = monthDays.filter((d) => d.pnl_usd > 0).length;
  const best = monthDays.reduce<Day | null>((b, d) => (!b || d.pnl_usd > b.pnl_usd ? d : b), null);
  const worst = monthDays.reduce<Day | null>((b, d) => (!b || d.pnl_usd < b.pnl_usd ? d : b), null);
  const maxAbs = Math.max(1, ...monthDays.map((d) => Math.abs(d.pnl_usd)));

  // Weeks start on Sunday; leading blanks for the first week.
  const first = new Date(Date.UTC(cur.y, cur.m, 1)).getUTCDay();
  const count = new Date(Date.UTC(cur.y, cur.m + 1, 0)).getUTCDate();
  const cells: (number | null)[] = [...Array(first).fill(null), ...Array.from({ length: count }, (_, i) => i + 1)];
  while (cells.length % 7) cells.push(null);
  const weeks = Array.from({ length: cells.length / 7 }, (_, i) => cells.slice(i * 7, i * 7 + 7));

  const step = (n: number) => {
    const d = new Date(Date.UTC(cur.y, cur.m + n, 1));
    setMonth({ y: d.getUTCFullYear(), m: d.getUTCMonth() });
  };
  const monthName = new Date(Date.UTC(cur.y, cur.m, 1)).toLocaleDateString("en-US", { month: "long", year: "numeric", timeZone: "UTC" });
  const name = (a: string) => names.get(a) ?? short(a);
  const tz = data?.tz === "America/Toronto" || data?.tz === "America/New_York" ? "Eastern" : data?.tz ?? "";

  return (
    <>
      <div className="page-head">
        <div>
          <h2>P&amp;L calendar</h2>
          <div className="sub">
            Paper profit from $25 copies made {delay}s late, after fees, on the day each copy closed ({tz} time).
            Every wallet we have copied counts, dropped ones too, so past days don&apos;t change.
          </div>
        </div>
        <div className="row">
          <select className="select" value={wallet} onChange={(e) => setWallet(e.target.value)} aria-label="Wallet">
            <option value="">All wallets</option>
            {(data?.wallets ?? []).map((w) => (
              <option key={w.address} value={w.address}>{name(w.address)} ({usd(w.pnl_usd, true)})</option>
            ))}
            {wallet && !(data?.wallets ?? []).some((w) => w.address === wallet) && <option value={wallet}>{name(wallet)}</option>}
          </select>
          <Seg value={delay} onChange={setDelay} options={[2, 10, 60].map((d) => ({ v: d, label: `+${d}s` }))} />
        </div>
      </div>
      <Err error={error} />
      <div className="grid stats">
        <Stat label={monthName} value={usd(total, true)} tone={total} hint={`${monthDays.reduce((s, d) => s + d.closed, 0)} copies closed`} />
        <Stat label="Green days" value={monthDays.length ? `${green} of ${monthDays.length}` : "-"} hint="days with closes that ended up" />
        <Stat label="Best day" value={best ? usd(best.pnl_usd, true) : "-"} tone={best?.pnl_usd} hint={best ? fmtDay(best.date) : " "} />
        <Stat label="Worst day" value={worst ? usd(worst.pnl_usd, true) : "-"} tone={worst?.pnl_usd} hint={worst ? fmtDay(worst.date) : " "} />
        <Stat label="Still open" value={usd(data?.open_usd, true)} tone={data?.open_usd} hint={`${data?.open ?? 0} copies, not on the calendar yet`} />
      </div>

      <div className="card" style={{ marginTop: 14 }}>
        <div className="cal-head">
          <button type="button" className="btn" onClick={() => step(-1)} aria-label="Previous month">&#8249;</button>
          <h3>{monthName}</h3>
          <button type="button" className="btn" onClick={() => step(1)} aria-label="Next month">&#8250;</button>
          {month && <button type="button" className="btn" onClick={() => setMonth(null)}>This month</button>}
        </div>
        <div className="cal" role="grid" aria-label={`P&L for ${monthName}`}>
          <div className="cal-row cal-dow" role="row">
            {WEEKDAYS.map((d) => <div key={d} role="columnheader">{d}</div>)}
            <div role="columnheader" className="cal-week">Week</div>
          </div>
          {weeks.map((wk, i) => {
            const wkDays = wk.map((d) => (d ? byDate.get(key(cur.y, cur.m, d)) : undefined)).filter(Boolean) as Day[];
            const wkSum = wkDays.reduce((s, d) => s + d.pnl_usd, 0);
            return (
              <div className="cal-row" role="row" key={i}>
                {wk.map((d, j) => {
                  if (!d) return <div key={j} className="cal-cell blank" role="gridcell" />;
                  const k = key(cur.y, cur.m, d);
                  const x = byDate.get(k);
                  const label = x ? `${fmtDay(k)}: ${usd(x.pnl_usd, true)}, ${x.closed} copies closed, ${x.won} won` : `${fmtDay(k)}: no copies closed`;
                  return (
                    <div key={j} role="gridcell" aria-label={label} title={label}
                      className={`cal-cell${k === today ? " today" : ""}${k > today ? " future" : ""}`} style={x ? tint(x.pnl_usd, maxAbs) : undefined}>
                      <span className="cal-day">{d}</span>
                      {x && <span className={`cal-pnl ${pnlClass(x.pnl_usd)}`}><span className="full">{usd(x.pnl_usd, true)}</span><span className="compact">{compact(x.pnl_usd)}</span></span>}
                      {x && <span className="cal-n">{x.closed} closed · {x.won} won</span>}
                    </div>
                  );
                })}
                <div className={`cal-cell cal-week ${pnlClass(wkSum)}`} role="gridcell" aria-label={`Week total ${usd(wkSum, true)}`}>
                  {wkDays.length ? usd(wkSum, true) : ""}
                </div>
              </div>
            );
          })}
        </div>
        <details style={{ marginTop: 12 }}>
          <summary className="muted">Show as a list</summary>
          {monthDays.length === 0 ? <div className="empty">No copies closed this month.</div> : (
            <table className="table" style={{ marginTop: 8 }}>
              <thead><tr><th>Day</th><th className="num">P&amp;L</th><th className="num">Closed</th><th className="num">Won</th></tr></thead>
              <tbody>
                {[...monthDays].reverse().map((d) => (
                  <tr key={d.date}><td>{fmtDay(d.date)}</td><td className={`num ${pnlClass(d.pnl_usd)}`}>{usd(d.pnl_usd, true)}</td>
                    <td className="num">{d.closed}</td><td className="num">{d.won}</td></tr>
                ))}
              </tbody>
            </table>
          )}
        </details>
      </div>
    </>
  );
}

// Day color grows with the size of the day's result (square root, so small days still show).
function tint(pnl: number, maxAbs: number) {
  const a = 8 + Math.round(30 * Math.sqrt(Math.min(Math.abs(pnl) / maxAbs, 1)));
  return { background: `color-mix(in srgb, var(${pnl >= 0 ? "--good" : "--critical"}) ${a}%, var(--surface-1))` };
}

// Whole dollars for phone-width cells.
function compact(n: number) {
  const a = Math.abs(n);
  return `${n < 0 ? "-" : "+"}$${a >= 1000 ? `${(a / 1000).toFixed(1)}k` : Math.round(a)}`;
}

function fmtDay(d: string) {
  return new Date(`${d}T12:00:00Z`).toLocaleDateString("en-US", { weekday: "short", month: "short", day: "numeric", timeZone: "UTC" });
}
