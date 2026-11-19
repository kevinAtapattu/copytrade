"use client";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useState } from "react";
import { EventsContext, useApi, useEventsState } from "@/lib/api";
import { help } from "@/lib/glossary";
import { ViewProvider, useView } from "@/lib/view";
import { Seg } from "@/components/ui";

type NavLink = {
  href: string; label: string; hint: string;
  watching?: boolean; // about everyone we watch: hidden in the real-money view
  tab?: string; icon?: Icon; // on phones: a bottom tab with this short label; the rest go under More
};

// Grouped by what a trader is trying to do, in the order they'd do it.
const groups: { title: string; links: NavLink[] }[] = [
  { title: "Results", links: [
    { href: "/", label: "Overview", hint: "How the paper copies are doing today", tab: "Overview", icon: "home" },
    { href: "/wallets", label: "Wallets", hint: "Every wallet we copy, ranked", watching: true, tab: "Wallets", icon: "list" },
    { href: "/calendar", label: "P&L calendar", hint: "Paper profit by day", tab: "Calendar", icon: "calendar" },
    { href: "/trades", label: "Trade log", hint: "Their trades and our copies", watching: true },
    { href: "/other-coins", label: "Other coins", hint: "How the copied wallet does on coins priced in other tokens" },
  ] },
  { title: "Find & trade", links: [
    { href: "/discovery", label: "Discovery", hint: "New wallets found automatically", watching: true },
    { href: "/executor", label: "Auto-trader", hint: "Copies live wallets, with safety limits", tab: "Trader", icon: "bolt" },
  ] },
  { title: "System", links: [
    { href: "/health", label: "Health", hint: "Data feeds and services" },
    { href: "/guide", label: "How it works", hint: "Start here: what everything means" },
  ] },
];

type Health = { services?: { executor?: { data?: { mode?: string } } } };

export default function Shell({ children }: { children: React.ReactNode }) {
  const events = useEventsState();
  return (
    <EventsContext.Provider value={events}>
      <ViewProvider>
        <div className="shell">
          <Nav connected={events.connected} />
          <main className="main">{children}</main>
        </div>
      </ViewProvider>
    </EventsContext.Provider>
  );
}

function Nav({ connected }: { connected: boolean }) {
  const path = usePathname();
  const { view, setView, live } = useView();
  const shown = groups.map((g) => ({ ...g, links: g.links.filter((l) => view === "all" || !l.watching) })).filter((g) => g.links.length);
  const isActive = (href: string) => (href === "/" ? path === "/" : path.startsWith(href));
  return (
    <>
    <nav className="nav" aria-label="Main">
      <Link href="/" className="brand">copytrade</Link>
      <ModeBadge />
      {live && (
        <div style={{ marginBottom: 10 }}>
        <Seg value={view} onChange={setView} options={[
          { v: "real", label: "Real money", title: "Only your wallet and the wallets you copy" },
          { v: "all", label: "Everything", title: "Every wallet we watch, with paper copies" }]} />
        </div>
      )}
      <div className="nav-row">
      {groups.map((g) => ({ ...g, links: g.links.filter((l) => view === "all" || !l.watching) })).filter((g) => g.links.length).map((g) => (
        <div className="nav-group" key={g.title}>
          <div className="nav-title">{g.title}</div>
          {g.links.map((l) => {
            const active = isActive(l.href);
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
        <span className="dot" style={{ background: connected ? "var(--good)" : "var(--critical)" }} />
        {connected ? "Live updates on" : "Live updates offline"}
      </div>
    </nav>
    <TabBar links={shown.flatMap((g) => g.links)} isActive={isActive} />
    </>
  );
}

// TabBar is the phone navigation: the main pages as tabs along the bottom, within
// thumb reach, and the rest behind More.
function TabBar({ links, isActive }: { links: NavLink[]; isActive: (href: string) => boolean }) {
  const [open, setOpen] = useState(false);
  const path = usePathname();
  useEffect(() => setOpen(false), [path]);
  useEffect(() => {
    if (!open) return;
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("keydown", esc);
    return () => window.removeEventListener("keydown", esc);
  }, [open]);
  const tabs = links.filter((l) => l.tab);
  const more = links.filter((l) => !l.tab);
  const moreActive = more.some((l) => isActive(l.href));
  return (
    <>
      {open && <div className="sheet-backdrop" onClick={() => setOpen(false)} />}
      {open && (
        <div className="sheet" id="more-menu" role="menu" aria-label="More pages">
          {more.map((l) => (
            <Link key={l.href} href={l.href} role="menuitem" className={isActive(l.href) ? "active" : ""} aria-current={isActive(l.href) ? "page" : undefined}>
              <span>{l.label}</span><span className="muted">{l.hint}</span>
            </Link>
          ))}
        </div>
      )}
      <nav className="tabbar" aria-label="Main">
        {tabs.map((l) => (
          <Link key={l.href} href={l.href} className={isActive(l.href) ? "active" : ""} aria-current={isActive(l.href) ? "page" : undefined}>
            <TabIcon name={l.icon!} />{l.tab}
          </Link>
        ))}
        <button type="button" className={moreActive || open ? "active" : ""} aria-expanded={open} aria-controls="more-menu" onClick={() => setOpen(!open)}>
          <TabIcon name="more" />More
        </button>
      </nav>
    </>
  );
}

type Icon = "home" | "calendar" | "bolt" | "list" | "more";

function TabIcon({ name }: { name: Icon }) {
  const p = { fill: "none", stroke: "currentColor", strokeWidth: 1.8, strokeLinecap: "round" as const, strokeLinejoin: "round" as const };
  return (
    <svg width="22" height="22" viewBox="0 0 24 24" aria-hidden="true">
      {name === "home" && <path {...p} d="M4 11.5 12 5l8 6.5V19a1 1 0 0 1-1 1h-4.5v-5h-5v5H5a1 1 0 0 1-1-1z" />}
      {name === "calendar" && <><rect {...p} x="4" y="5.5" width="16" height="14.5" rx="2" /><path {...p} d="M4 10h16M8.5 3.5v4M15.5 3.5v4" /></>}
      {name === "bolt" && <path {...p} d="M13 3 5.5 13.5H12L11 21l7.5-10.5H12z" />}
      {name === "list" && <path {...p} d="M9 7h11M9 12h11M9 17h11M4.5 7h.01M4.5 12h.01M4.5 17h.01" />}
      {name === "more" && <g fill="currentColor"><circle cx="12" cy="5.5" r="1.7" /><circle cx="12" cy="12" r="1.7" /><circle cx="12" cy="18.5" r="1.7" /></g>}
    </svg>
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
      {live ? "Live trading" : "Paper mode"}
    </div>
  );
}
