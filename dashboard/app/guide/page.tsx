"use client";
import Link from "next/link";
import { GLOSSARY } from "@/lib/glossary";

const steps: { title: string; body: React.ReactNode; href: string; cta: string }[] = [
  {
    title: "Find wallets",
    body: <>Discovery looks at tokens that ran 5x or more and finds the early buyers who also sold well. You can add wallets by hand too.</>,
    href: "/discovery", cta: "See discovery",
  },
  {
    title: "Copy them on paper",
    body: <>Every trade a watched wallet makes is copied with a pretend $25, at three delays (+2s, +10s, +60s), after fees and slippage. No money moves.</>,
    href: "/trades", cta: "See the trade log",
  },
  {
    title: "Rank them",
    body: <>The Wallets page ranks everyone by what copying them would have made us. A good wallet stays profitable for 2+ weeks and doesn't rely on one lucky trade.</>,
    href: "/wallets", cta: "See the ranking",
  },
  {
    title: "Copy the best for real (later)",
    body: <>Only wallets you make Live get copied by the auto-trader, with a daily loss limit, a stop-loss and a kill switch. It stays on paper until you turn live trading on.</>,
    href: "/executor", cta: "See the auto-trader",
  },
];

const order: (keyof typeof GLOSSARY)[] = [
  "paper", "live", "delay", "watching", "liveWallet", "score", "simPnl", "exBest", "themVsUs", "winRate",
  "drawdown", "hold", "open", "closed", "slippage", "nonsol", "graduated", "discovery", "killSwitch", "lowData",
];

export default function GuidePage() {
  return (
    <div className="guide">
      <div className="page-head">
        <div>
          <h2>How it works</h2>
          <div className="sub">The short version, for when you&apos;re new to copy trading.</div>
        </div>
      </div>

      <p className="lede">
        Copying famous wallets usually loses money: by the time you copy a trade, the price has already moved.
        This app measures which wallets are still worth copying <b>after</b> that delay, using paper trades, before any real money is risked.
      </p>

      <h3 className="guide-h">The loop</h3>
      <ol className="steps">
        {steps.map((s) => (
          <li key={s.title}>
            <div>
              <b>{s.title}</b>
              <p>{s.body}</p>
              <Link href={s.href} className="link">{s.cta}</Link>
            </div>
          </li>
        ))}
      </ol>

      <h3 className="guide-h">Reading the numbers</h3>
      <ul className="plain">
        <li><span className="pos">Green</span> is profit and <span className="neg">red</span> is loss, always in US dollars on a $25 copy.</li>
        <li><b>Closed</b> results are final. <b>Open</b> results are estimates at the latest price and can swing a lot.</li>
        <li>A wallet needs weeks of results before its ranking means much. One good day is mostly luck.</li>
        <li>Hover any <span className="term">underlined</span> word or column name for a plain explanation.</li>
      </ul>

      <h3 className="guide-h">Words you&apos;ll see</h3>
      <dl className="glossary">
        {order.map((k) => (
          <div key={k}>
            <dt>{GLOSSARY[k].term}</dt>
            <dd>{GLOSSARY[k].def}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}
