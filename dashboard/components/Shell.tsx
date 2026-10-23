"use client";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { EventsContext, useEventsState } from "@/lib/api";

const links = [
  ["/", "Overview"],
  ["/wallets", "Wallets"],
  ["/trades", "Trade log"],
  ["/discovery", "Discovery"],
  ["/executor", "Executor"],
  ["/health", "Health"],
];

export default function Shell({ children }: { children: React.ReactNode }) {
  const events = useEventsState();
  const path = usePathname();
  return (
    <EventsContext.Provider value={events}>
      <div className="shell">
        <nav className="nav">
          <h1>copytrade</h1>
          {links.map(([href, label]) => {
            const active = href === "/" ? path === "/" : path.startsWith(href);
            return (
              <Link key={href} href={href} className={active ? "active" : ""}>
                {label}
              </Link>
            );
          })}
          <div className="conn">
            <span className="dot" style={{ background: events.connected ? "var(--good)" : "var(--critical)" }} />
            {events.connected ? "live stream connected" : "live stream offline"}
          </div>
        </nav>
        <main className="main">{children}</main>
      </div>
    </EventsContext.Provider>
  );
}
