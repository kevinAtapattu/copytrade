"use client";
import { useMemo, useState } from "react";
import { Err, Seg, Stat } from "@/components/ui";
import { useApi, useWalletNames } from "@/lib/api";
import { dur, pnlClass, short, usd } from "@/lib/format";

type Copy = { mint: string; opened_at: string; closed_at?: string; cost_usd: number; us_usd: number; them_usd: number | null };
type DayWallet = { address: string; us_usd: number; them_usd: number; closed: number; won: number; copies?: Copy[] };
type Day = { date: string; us_usd: number; them_usd: number; closed: number; won: number; picks?: string[]; wallets?: DayWallet[] };
type Calendar = {
  tz: string; delay: number; pick: string; n: number; today: string; days: Day[]; open_usd: number; open: number;
  picks_today: string[] | null; wallets?: { address: string; pnl_usd: number }[]; day?: Day; strict?: boolean; loose_us_usd?: number;
  winners?: { address: string; us_usd: number; closed: number }[];
};

const WEEKDAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const pad = (n: number) => String(n).padStart(2, "0");
const key = (y: number, m: number, d: number) => `${y}-${pad(m + 1)}-${pad(d)}`;

export default function CalendarPage() {
  const [delay, setDelay] = useState(2);
  const [mode, setMode] = useState<"win" | "3" | "5" | "all">("win");
  const [wallet, setWallet] = useState("");
  const [sel, setSel] = useState<string | null>(null);
  const [strict, setStrict] = useState(true);
  const q = `calendar?delay=${delay}${wallet ? `&wallet=${wallet}` : mode === "all" ? "&pick=all" : mode === "win" ? "&pick=winners" : `&n=${mode}${strict ? "" : "&strict=0"}`}`;
  const { data, error } = useApi<Calendar>(q, 60000);
  const { data: detail } = useApi<Calendar>(sel ? `${q}&date=${sel}` : null, 60000);
  const { names } = useWalletNames();
  const name = (a: string) => names.get(a) ?? short(a);

  // Today comes only from the API: a date computed here would be baked in when the
  // page is pre-rendered at build time, and hydration keeps those stale attributes.
  const today = data?.today ?? "";
  const [month, setMonth] = useState<{ y: number; m: number } | null>(null);
  const cur = month ?? { y: Number(today.slice(0, 4)), m: Number(today.slice(5, 7)) - 1 };
  const byDate = useMemo(() => new Map((data?.days ?? []).map((d) => [d.date, d])), [data]);
  const prefix = `${cur.y}-${pad(cur.m + 1)}`;
  const monthDays = (data?.days ?? []).filter((d) => d.date.startsWith(prefix));
  const us = monthDays.reduce((s, d) => s + d.us_usd, 0);
  const them = monthDays.reduce((s, d) => s + d.them_usd, 0);
  const green = monthDays.filter((d) => d.us_usd > 0).length;
  const best = monthDays.reduce<Day | null>((b, d) => (!b || d.us_usd > b.us_usd ? d : b), null);
  const worst = monthDays.reduce<Day | null>((b, d) => (!b || d.us_usd < b.us_usd ? d : b), null);
  const maxAbs = Math.max(1, ...monthDays.map((d) => Math.abs(d.us_usd)));

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
  const tz = data?.tz === "America/Toronto" || data?.tz === "America/New_York" ? "Eastern" : data?.tz ?? "";
  const best_ = !wallet && (mode === "3" || mode === "5");
  const win = !wallet && mode === "win";

  if (!data) {
    return (
      <>
        <div className="page-head"><div><h2>P&amp;L calendar</h2></div></div>
        <Err error={error} />
        {!error && <div className="empty">Loading...</div>}
      </>
    );
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h2>P&amp;L calendar</h2>
          <div className="sub">
            {wallet ? <>Every copy of {name(wallet)}</> : win ? (
              <>Our best traders: every wallet whose copies that closed in the last 14 days made us money. Each day shows
                what their copies that closed that day made, win or lose. Open copies count once they close. These are
                today&apos;s best traders, so older days show how they did then, not what we&apos;d have known to copy.</>
            ) : best_ ? (
              <>Copying only our best {mode} wallets. Each morning we pick up to {mode} with the best copy results over the
                previous 14 days: at least 5 closed copies, held a minute or more (snipers can&apos;t be copied 2s late),
                {strict ? " and making us money even without their single best trade" : " ranked without their single best trade, even if losing"}.
                We copy only them that day. No hindsight: picks use only what was known that morning.</>
            ) : <>Copying every wallet we watch</>}
            {" "}<b>Us</b> is our $25 copies {delay}s late after fees; <b>them</b> is what the same trades made the wallet, scaled to $25.
            Counted on the day each copy closed ({tz} time). Click a day for details.
          </div>
        </div>
        <div className="row">
          <Seg value={wallet ? ("" as const) : mode} onChange={(v) => { if (v) { setWallet(""); setMode(v); } }}
            options={[
              { v: "win", label: "Best traders", title: "Wallets whose copies made us money over the last 14 days" },
              { v: "3", label: "Top 3", title: "Top 3 picked each morning from what was known then (no hindsight)" },
              { v: "5", label: "Top 5", title: "Top 5 picked each morning from what was known then (no hindsight)" },
              { v: "all", label: "Everyone" }]} />
          <select value={wallet} onChange={(e) => setWallet(e.target.value)} aria-label="One wallet">
            <option value="">One wallet...</option>
            {(data?.wallets ?? []).map((w) => <option key={w.address} value={w.address}>{name(w.address)} ({usd(w.pnl_usd, true)})</option>)}
            {wallet && !(data?.wallets ?? []).some((w) => w.address === wallet) && <option value={wallet}>{name(wallet)}</option>}
          </select>
          <Seg value={delay} onChange={setDelay} options={[2, 10, 60].map((d) => ({ v: d, label: `+${d}s` }))} />
          {best_ && (
            <label className="check" title="Off: copy the least bad wallets even when they lose money, to see what that would do">
              <input type="checkbox" checked={strict} onChange={(e) => setStrict(e.target.checked)} /> Only wallets making us money
            </label>
          )}
        </div>
      </div>
      <Err error={error} />
      <div className="grid stats">
        <Stat label={`${monthName}, us`} value={usd(us, true)} tone={us}
          hint={monthDays.length ? `them ${usd(them, true)}, ${us > 0 && them > 0 ? `we kept ${Math.round((us / them) * 100)}%` : `us minus them ${usd(us - them, true)}`}` : "no closes yet"} />
        <Stat label="Green days" value={monthDays.length ? `${green} of ${monthDays.length}` : "-"} hint="days we ended up" />
        <Stat label="Best day" value={best ? usd(best.us_usd, true) : "-"} tone={best?.us_usd} hint={best ? fmtDay(best.date) : " "} />
        <Stat label="Worst day" value={worst ? usd(worst.us_usd, true) : "-"} tone={worst?.us_usd} hint={worst ? fmtDay(worst.date) : " "} />
        {win ? (
          <Stat label="Best traders" value={data?.winners?.length ? `${data.winners.length} wallet${data.winners.length > 1 ? "s" : ""}` : "none yet"}
            hint={(data?.winners ?? []).slice(0, 3).map((w) => `${name(w.address)} ${usd(w.us_usd, true)}`).join(", ") || " "} />
        ) : best_ ? (
          <Stat label="Copying today" value={data?.picks_today?.length ? data.picks_today.map(name).join(", ") : "nobody qualifies"}
            hint={`${data?.open ?? 0} open copies, ${usd(data?.open_usd, true)} now`} />
        ) : (
          <Stat label="Still open" value={usd(data?.open_usd, true)} tone={data?.open_usd} hint={`${data?.open ?? 0} copies`} />
        )}
      </div>

      <div className="card" style={{ marginTop: 14 }}>
        <div className="cal-head">
          <button type="button" className="btn" onClick={() => step(-1)} aria-label="Previous month">&#8249;</button>
          <h3>{monthName}</h3>
          <button type="button" className="btn" onClick={() => step(1)} aria-label="Next month">&#8250;</button>
          {month && <button type="button" className="btn" onClick={() => setMonth(null)}>This month</button>}
        </div>
        {best_ && strict && data && (data.days ?? []).length === 0 && (
          <div className="note" role="status">
            No wallet has qualified yet: none of the wallets we watch made us money on its copies over 14 days without its
            best trade, with 5+ copies held a minute or more. So this strategy has sat out every day, at $0. Copying the
            top {mode} anyway would have made <span className={pnlClass(data.loose_us_usd)}>{usd(data.loose_us_usd, true)}</span>
            {" "}(untick &quot;Only wallets making us money&quot; to see those days). The 48 leaderboard wallets added Nov 3 start qualifying once
            they have 5 closed copies.
          </div>
        )}
        {win && data && (data.winners ?? []).length === 0 && (
          <div className="note" role="status">No wallet we watch has closed copies that made us money in the last 14 days yet.</div>
        )}
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
                      onClick={() => setSel(sel === k ? null : k)} disabled={k > today}
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
      </div>

      {sel && <DayDetail key={`${sel}${q}`} date={sel} day={detail?.day} name={name} bestMode={best_} n={mode} onClose={() => setSel(null)} />}
    </>
  );
}

function DayDetail({ date, day, name, bestMode, n, onClose }: {
  date: string; day?: Day; name: (a: string) => string; bestMode: boolean; n: string; onClose: () => void;
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
      {bestMode && (
        <div className="sub" style={{ marginBottom: 10 }}>
          Picked that morning (top {n}): {day?.picks?.length ? day.picks.map(name).join(", ") : "nobody had 5+ closed copies yet"}.
          Copies that closed today could have been opened on an earlier day, when that day&apos;s picks applied.
        </div>
      )}
      {!day ? <div className="empty">Loading...</div> : ws.length === 0 ? <div className="empty">No copies closed this day.</div> : (
        <>
          <div className="tabs" role="tablist" aria-label="Traders this day">
            {ws.map((x, i) => (
              <button key={x.address} type="button" role="tab" aria-selected={x === w} className={x === w ? "on" : ""} onClick={() => setTab(i)}>
                <span className="muted">#{i + 1}</span> {name(x.address)} <span className={pnlClass(x.us_usd)}>{usd(x.us_usd, true)}</span>
              </button>
            ))}
          </div>
          <div role="tabpanel">
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

function fmtDay(d: string) {
  return new Date(`${d}T12:00:00Z`).toLocaleDateString("en-US", { weekday: "short", month: "short", day: "numeric", timeZone: "UTC" });
}
