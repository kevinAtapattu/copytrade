"use client";
import { useMemo, useState } from "react";
import { Stat } from "@/components/ui";
import { dur, pnlClass, short, usd } from "@/lib/format";

export type Copy = { mint: string; opened_at: string; closed_at?: string; cost_usd: number; us_usd: number; them_usd: number | null };
export type DayWallet = { address: string; us_usd: number; them_usd: number; closed: number; won: number; copies?: Copy[] };
export type Day = { date: string; us_usd: number; them_usd: number; closed: number; won: number; picks?: string[]; wallets?: DayWallet[] };
export type Calendar = {
  tz: string; delay: number; pick: string; n: number; today: string; days: Day[]; open_usd: number; open: number;
  picks_today: string[] | null; wallets?: { address: string; pnl_usd: number }[]; day?: Day; strict?: boolean; loose_us_usd?: number;
  winners?: { address: string; us_usd: number; closed: number }[];
};

const WEEKDAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const pad = (n: number) => String(n).padStart(2, "0");
const key = (y: number, m: number, d: number) => `${y}-${pad(m + 1)}-${pad(d)}`;

// The shown month and its days. Today comes only from the API: a date computed here
// would be baked in when the page is pre-rendered at build time, and hydration keeps
// those stale attributes.
export function useMonth(today: string, days: Day[] | undefined) {
  const [month, setMonth] = useState<{ y: number; m: number } | null>(null);
  const cur = month ?? { y: Number(today.slice(0, 4)), m: Number(today.slice(5, 7)) - 1 };
  const prefix = `${cur.y}-${pad(cur.m + 1)}`;
  const monthDays = (days ?? []).filter((d) => d.date.startsWith(prefix));
  const step = (n: number) => {
    const d = new Date(Date.UTC(cur.y, cur.m + n, 1));
    setMonth({ y: d.getUTCFullYear(), m: d.getUTCMonth() });
  };
  const name = new Date(Date.UTC(cur.y, cur.m, 1)).toLocaleDateString("en-US", { month: "long", year: "numeric", timeZone: "UTC" });
  return { cur, name, monthDays, step, moved: month !== null, reset: () => setMonth(null) };
}

export function monthTotals(monthDays: Day[]) {
  const us = monthDays.reduce((s, d) => s + d.us_usd, 0);
  const them = monthDays.reduce((s, d) => s + d.them_usd, 0);
  const green = monthDays.filter((d) => d.us_usd > 0).length;
  const best = monthDays.reduce<Day | null>((b, d) => (!b || d.us_usd > b.us_usd ? d : b), null);
  const worst = monthDays.reduce<Day | null>((b, d) => (!b || d.us_usd < b.us_usd ? d : b), null);
  return { us, them, green, best, worst };
}

export function keptHint(us: number, them: number) {
  return `them ${usd(them, true)}, ${us > 0 && them > 0 ? `we kept ${Math.round((us / them) * 100)}%` : `us minus them ${usd(us - them, true)}`}`;
}

export function MonthGrid({ days, today, month, sel, onSelect, children }: {
  days: Day[]; today: string; month: ReturnType<typeof useMonth>; sel: string | null; onSelect: (d: string | null) => void;
  children?: React.ReactNode;
}) {
  const { cur, name: monthName, monthDays, step, moved, reset } = month;
  const byDate = useMemo(() => new Map(days.map((d) => [d.date, d])), [days]);
  const maxAbs = Math.max(1, ...monthDays.map((d) => Math.abs(d.us_usd)));
  const first = new Date(Date.UTC(cur.y, cur.m, 1)).getUTCDay();
  const count = new Date(Date.UTC(cur.y, cur.m + 1, 0)).getUTCDate();
  const cells: (number | null)[] = [...Array(first).fill(null), ...Array.from({ length: count }, (_, i) => i + 1)];
  while (cells.length % 7) cells.push(null);
  const weeks = Array.from({ length: cells.length / 7 }, (_, i) => cells.slice(i * 7, i * 7 + 7));
  return (
    <>
      <div className="cal-head">
        <button type="button" className="btn" onClick={() => step(-1)} aria-label="Previous month">&#8249;</button>
        <h3>{monthName}</h3>
        <button type="button" className="btn" onClick={() => step(1)} aria-label="Next month">&#8250;</button>
        {moved && <button type="button" className="btn" onClick={reset}>This month</button>}
      </div>
      {children}
      <div className="cal" role="grid" aria-label={`P&L for ${monthName}`}>
        <div className="cal-row cal-dow" role="row">
          {WEEKDAYS.map((d) => <div key={d} role="columnheader">{d}</div>)}
          <div role="columnheader" className="cal-week">Week</div>
        </div>
        {weeks.map((wk, i) => {
          const wkDays = wk.map((d) => (d ? byDate.get(key(cur.y, cur.m, d)) : undefined)).filter(Boolean) as Day[];
          const wkUs = wkDays.reduce((s, d) => s + d.us_usd, 0);
          const wkThem = wkDays.reduce((s, d) => s + d.them_usd, 0);
          return (
            <div className="cal-row" role="row" key={i}>
              {wk.map((d, j) => {
                if (!d) return <div key={j} className="cal-cell blank" role="gridcell" />;
                const k = key(cur.y, cur.m, d);
                const x = byDate.get(k);
                const label = x ? `${fmtDay(k)}: us ${usd(x.us_usd, true)}, them ${usd(x.them_usd, true)}, ${x.closed} copies closed, ${x.won} won`
                  : `${fmtDay(k)}: no copies closed`;
                return (
                  <button key={j} type="button" role="gridcell" aria-label={label} title={label} aria-pressed={sel === k}
                    onClick={() => onSelect(sel === k ? null : k)} disabled={k > today}
                    className={`cal-cell${k === today ? " today" : ""}${k > today ? " future" : ""}${sel === k ? " sel" : ""}`}
                    style={x ? tint(x.us_usd, maxAbs) : undefined}>
                    <span className="cal-day">{d}</span>
                    {x && <span className={`cal-pnl ${pnlClass(x.us_usd)}`}><span className="full">{usd(x.us_usd, true)}</span><span className="compact">{compact(x.us_usd)}</span></span>}
                    {x && <span className="cal-n">them {usd(x.them_usd, true)}</span>}
                  </button>
                );
              })}
              <div className="cal-cell cal-week" role="gridcell" aria-label={`Week: us ${usd(wkUs, true)}, them ${usd(wkThem, true)}`}>
                {wkDays.length > 0 && <><span className={pnlClass(wkUs)}>{usd(wkUs, true)}</span><span className="cal-n">them {usd(wkThem, true)}</span></>}
              </div>
            </div>
          );
        })}
      </div>
    </>
  );
}

// One day's closed copies, as a tab per trader (one tab when showing a single wallet).
export function DayDetail({ date, day, name, note, onClose }: {
  date: string; day?: Day; name: (a: string) => string; note?: React.ReactNode; onClose: () => void;
}) {
  const ws = day?.wallets ?? [];
  const [tab, setTab] = useState(0);
  const us = ws.reduce((s, w) => s + w.us_usd, 0);
  const them = ws.reduce((s, w) => s + w.them_usd, 0);
  const w = ws[Math.min(tab, ws.length - 1)];
  return (
    <div className="card" style={{ marginTop: 14 }}>
      <div className="cal-head" style={{ justifyContent: "space-between" }}>
        <h3>{fmtDay(date)}: us <span className={pnlClass(us)}>{usd(us, true)}</span>, them <span className={pnlClass(them)}>{usd(them, true)}</span></h3>
        <button type="button" className="btn" onClick={onClose}>Close</button>
      </div>
      {note && <div className="sub" style={{ marginBottom: 10 }}>{note}</div>}
      {!day ? <div className="empty">Loading...</div> : ws.length === 0 ? <div className="empty">No copies closed this day.</div> : (
        <>
          {ws.length > 1 && (
            <div className="tabs" role="tablist" aria-label="Traders this day">
              {ws.map((x, i) => (
                <button key={x.address} type="button" role="tab" aria-selected={x === w} className={x === w ? "on" : ""} onClick={() => setTab(i)}>
                  <span className="muted">#{i + 1}</span> {name(x.address)} <span className={pnlClass(x.us_usd)}>{usd(x.us_usd, true)}</span>
                </button>
              ))}
            </div>
          )}
          <div role={ws.length > 1 ? "tabpanel" : undefined}>
            <div className="grid stats" style={{ marginBottom: 12 }}>
              <Stat label="Us" value={usd(w.us_usd, true)} tone={w.us_usd} hint={`our $25 copies`} />
              <Stat label="Them" value={usd(w.them_usd, true)} tone={w.them_usd} hint="their trades, scaled to $25" />
              <Stat label="Us minus them" value={usd(w.us_usd - w.them_usd, true)} hint="what copying late cost or gained" />
              <Stat label="Closed" value={`${w.won} of ${w.closed} won`} hint={w.address.slice(0, 4) + ".." + w.address.slice(-4)} />
            </div>
            <div className="table-wrap">
              <table>
                <thead><tr><th>Token</th><th className="num">Held</th><th className="num hide-sm">In</th><th className="num">Them</th><th className="num">Us</th><th className="num hide-sm">Us minus them</th></tr></thead>
                <tbody>
                  {(w.copies ?? []).map((c) => (
                    <tr key={c.mint + c.opened_at}>
                      <td><a className="mono" href={`https://pump.fun/coin/${c.mint}`} target="_blank" rel="noreferrer">{short(c.mint)}</a></td>
                      <td className="num muted">{c.closed_at ? dur((Date.parse(c.closed_at) - Date.parse(c.opened_at)) / 1000) : "-"}</td>
                      <td className="num hide-sm muted">{usd(c.cost_usd)}</td>
                      <td className={`num ${pnlClass(c.them_usd)}`}>{c.them_usd == null ? "-" : usd(c.them_usd, true)}</td>
                      <td className={`num ${pnlClass(c.us_usd)}`}>{usd(c.us_usd, true)}</td>
                      <td className="num hide-sm muted">{c.them_usd == null ? "-" : usd(c.us_usd - c.them_usd, true)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </>
      )}
    </div>
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

export function fmtDay(d: string) {
  return new Date(`${d}T12:00:00Z`).toLocaleDateString("en-US", { weekday: "short", month: "short", day: "numeric", timeZone: "UTC" });
}
