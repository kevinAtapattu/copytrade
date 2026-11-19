"use client";
import { useState } from "react";
import { Calendar, DayDetail, MonthGrid, fmtDay, keptHint, monthTotals, useMonth } from "@/components/PnlCalendar";
import { Err, Seg, Stat } from "@/components/ui";
import { useApi, useWalletNames } from "@/lib/api";
import { pnlClass, short, usd } from "@/lib/format";
import { useView } from "@/lib/view";

export default function CalendarPage() {
  const { view, ready } = useView();
  if (!ready) return <div className="empty">Loading...</div>;
  return view === "real" ? <RealCalendar /> : <PaperCalendar />;
}

// RealCalendar is your wallet's actual result each day: the auto-trader's real
// trades, counted on the day they closed. The wallet you copied shows small: what
// the same trades made them, scaled to what you put in.
function RealCalendar() {
  const [sel, setSel] = useState<string | null>(null);
  const { data, error } = useApi<Calendar>("calendar?mode=live", 60000);
  const { data: detail } = useApi<Calendar>(sel ? `calendar?mode=live&date=${sel}` : null, 60000);
  const { names } = useWalletNames();
  const name = (a: string) => names.get(a) ?? short(a);
  const today = data?.today ?? "";
  const month = useMonth(today, data?.days);
  const { name: monthName, monthDays } = month;
  const { us, them, green, best, worst } = monthTotals(monthDays);
  const copied = (data?.wallets ?? []).map((w) => name(w.address));
  return (
    <>
      <div className="page-head">
        <div>
          <h2>P&amp;L calendar</h2>
          <div className="sub">
            Real money: what your wallet made each day{copied.length ? <> copying {copied.join(", ")}</> : ""}, after all fees,
            counted on the day each trade closed (Eastern time). Small: what the same trades made {copied.length === 1 ? copied[0] : "the wallet you copied"},
            scaled to what you put in. Click a day for its trades.
          </div>
        </div>
      </div>
      <Err error={error} />
      <div className="card">
        {data ? <MonthGrid days={data.days ?? []} today={today} month={month} sel={sel} onSelect={setSel} />
          : !error && <div className="empty">Loading...</div>}
      </div>
      {sel && <DayDetail key={sel} date={sel} day={detail?.day} name={name} onClose={() => setSel(null)}
        size="real trades" themHint="their result on the same tokens, scaled to what you put in" />}
      <div className="grid stats" style={{ marginTop: 14 }}>
        <Stat label={`${monthName}, real`} value={usd(us, true)} tone={us}
          hint={monthDays.length ? keptHint(us, them) : "no trades closed yet"} />
        <Stat label="Green days" value={monthDays.length ? `${green} of ${monthDays.length}` : "-"} hint="days you ended up" />
        <Stat label="Best day" value={best ? usd(best.us_usd, true) : "-"} tone={best?.us_usd} hint={best ? fmtDay(best.date) : " "} />
        <Stat label="Worst day" value={worst ? usd(worst.us_usd, true) : "-"} tone={worst?.us_usd} hint={worst ? fmtDay(worst.date) : " "} />
        <Stat label="Still open" value={usd(data?.open_usd, true)} tone={data?.open ? data.open_usd : undefined}
          hint={`${data?.open ?? 0} trade${data?.open === 1 ? "" : "s"}, not counted until closed`} />
      </div>
    </>
  );
}

function PaperCalendar() {
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

  const today = data?.today ?? "";
  const month = useMonth(today, data?.days);
  const { name: monthName, monthDays } = month;
  const { us, them, green, best, worst } = monthTotals(monthDays);

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
      <div className="card">
        <MonthGrid days={data.days ?? []} today={today} month={month} sel={sel} onSelect={setSel}>
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
        </MonthGrid>
      </div>
      {sel && <DayDetail key={`${sel}${q}`} date={sel} day={detail?.day} name={name} onClose={() => setSel(null)}
        note={best_ && <>Picked that morning (top {mode}): {detail?.day?.picks?.length ? detail.day.picks.map(name).join(", ") : "nobody had 5+ closed copies yet"}.
          Copies that closed today could have been opened on an earlier day, when that day&apos;s picks applied.</>} />}
      <div className="grid stats" style={{ marginTop: 14 }}>
        <Stat label={`${monthName}, us`} value={usd(us, true)} tone={us}
          hint={monthDays.length ? keptHint(us, them) : "no closes yet"} />
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
    </>
  );
}
