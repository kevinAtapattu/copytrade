"use client";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { EventsContext, useApi, useEventsState } from "@/lib/api";
import { help } from "@/lib/glossary";

// Grouped by what a trader is trying to do, in the order they'd do it.
const groups: { title: string; links: { href: string; label: string; hint: string }[] }[] = [
  { title: "Results", links: [
    { href: "/", label: "Overview", hint: "How the paper copies are doing today" },
    { href: "/wallets", label: "Wallets", hint: "Every wallet we copy, ranked" },
    { href: "/calendar", label: "P&L calendar", hint: "Paper profit by day" },
    { href: "/trades", label: "Trade log", hint: "Their trades and our copies" },
  ] },
  { title: "Find & trade", links: [
    { href: "/discovery", label: "Discovery", hint: "New wallets found automatically" },
    { href: "/executor", label: "Auto-trader", hint: "Copies live wallets, with safety limits" },
  ] },
  { title: "System", links: [
    { href: "/health", label: "Health", hint: "Data feeds and services" },
    { href: "/guide", label: "How it works", hint: "Start here: what everything means" },
  ] },
];

type Health = { services?: { executor?: { data?: { mode?: string } } } };

export default function Shell({ children }: { children: React.ReactNode }) {
  const events = useEventsState();
  const path = usePathname();
  return (
    <EventsContext.Provider value={events}>
      <div className="shell">
        <nav className="nav" aria-label="Main">
          <Link href="/" className="brand">copytrade</Link>
          <ModeBadge />
          <div className="nav-row">
          {groups.map((g) => (
            <div className="nav-group" key={g.title}>
              <div className="nav-title">{g.title}</div>
              {g.links.map((l) => {
                const active = l.href === "/" ? path === "/" : path.startsWith(l.href);
                return (
                  <Link key={l.href} href={l.href} className={active ? "active" : ""} aria-current={active ? "page" : undefined} title={l.hint}>
                    {l.label}
                  </Link>
                );
              })}
            </div>
          ))}
          </div>
          <div className="conn" title="Live updates from the backend. Pages also refresh on their own.">
            <span className="dot" style={{ background: events.connected ? "var(--good)" : "var(--critical)" }} />
            {events.connected ? "Live updates on" : "Live updates offline"}
          </div>
        </nav>
        <main className="main">{children}</main>
      </div>
    </EventsContext.Provider>
  );
}

// ModeBadge keeps "is real money moving?" visible on every page.
function ModeBadge() {
  const { data } = useApi<Health>("health", 30000);
  const mode = data?.services?.executor?.data?.mode;
  if (!mode) return <div className="mode" aria-live="polite">&nbsp;</div>;
  const live = mode === "live";
  return (
    <div className={`mode ${live ? "mode-live" : "mode-paper"}`} title={help(live ? "live" : "paper")} aria-live="polite">
      <span className="dot" style={{ background: live ? "var(--critical)" : "var(--good)" }} />
      {live ? "Live trading: real money" : "Paper mode: no real money"}
    </div>
  );
}
