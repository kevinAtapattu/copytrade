"use client";
import { useMemo, useState } from "react";
import Link from "next/link";
import { pnlClass, short } from "@/lib/format";

// Term underlines a word and explains it on hover or focus (plain-language help).
export function Term({ help, children }: { help: string; children: React.ReactNode }) {
  return <span className="term" title={help} tabIndex={0}>{children}</span>;
}

export function Stat({ label, value, hint, tone, help }: { label: string; value: React.ReactNode; hint?: React.ReactNode; tone?: number; help?: string }) {
  return (
    <div className="card stat">
      <div className="label">{help ? <Term help={help}>{label}</Term> : label}</div>
      <div className={`value ${tone !== undefined ? pnlClass(tone) : ""}`}>{value}</div>
      {hint && <div className="hint">{hint}</div>}
    </div>
  );
}

export function Err({ error }: { error: string | null }) {
  if (!error) return null;
  return <div className="err" role="alert">Could not load data: {error}. Is the API running? Start it with <span className="mono">make up</span>.</div>;
}

export function Seg<T extends string | number>({ value, options, onChange }: {
  value: T; options: { v: T; label: string; title?: string }[]; onChange: (v: T) => void;
}) {
  return (
    <div className="seg" role="group">
      {options.map((o) => (
        <button key={String(o.v)} type="button" className={o.v === value ? "on" : ""} aria-pressed={o.v === value} title={o.title} onClick={() => onChange(o.v)}>
          {o.label}
        </button>
      ))}
    </div>
  );
}

export type Col<T> = {
  key: string;
  label: string;
  // Plain-language explanation shown on hover over the column header.
  help?: string;
  num?: boolean;
  // Hidden on phone widths so the key numbers stay on screen.
  hideSm?: boolean;
  sort?: (r: T) => number | string;
  render: (r: T) => React.ReactNode;
};

export function Table<T>({ rows, cols, initialSort, empty = "Nothing yet", rowKey, loading }: {
  rows: T[]; cols: Col<T>[]; initialSort?: { key: string; desc: boolean }; empty?: React.ReactNode; rowKey: (r: T) => string; loading?: boolean;
}) {
  const [sort, setSort] = useState(initialSort);
  const sorted = useMemo(() => {
    const c = cols.find((x) => x.key === sort?.key);
    if (!c?.sort || !sort) return rows;
    const f = c.sort;
    return [...rows].sort((a, b) => {
      const x = f(a), y = f(b);
      const r = x < y ? -1 : x > y ? 1 : 0;
      return sort.desc ? -r : r;
    });
  }, [rows, cols, sort]);
  if (!rows.length) return <div className="empty">{loading ? "Loading..." : empty}</div>;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            {cols.map((c) => (
              <th key={c.key} className={`${c.num ? "num" : ""} ${c.sort ? "sortable" : ""} ${c.hideSm ? "hide-sm" : ""}`} title={c.help}
                aria-sort={sort?.key === c.key ? (sort.desc ? "descending" : "ascending") : undefined}
                tabIndex={c.sort ? 0 : undefined}
                onKeyDown={(e) => c.sort && (e.key === "Enter" || e.key === " ") && (e.preventDefault(), setSort({ key: c.key, desc: sort?.key === c.key ? !sort.desc : true }))}
                onClick={() => c.sort && setSort({ key: c.key, desc: sort?.key === c.key ? !sort.desc : true })}>
                <span className={c.help ? "term" : ""}>{c.label}</span>{sort?.key === c.key ? (sort.desc ? " ↓" : " ↑") : ""}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sorted.map((r) => (
            <tr key={rowKey(r)}>
              {cols.map((c) => (
                <td key={c.key} className={`${c.num ? "num" : ""} ${c.hideSm ? "hide-sm" : ""}`}>{c.render(r)}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// WalletName shows a wallet by its label ("dash") instead of 4ESz..EAau, linking to its page.
export function WalletName({ addr, names }: { addr: string; names: Map<string, string> }) {
  const n = names.get(addr);
  return (
    <Link href={`/wallets/${addr}`} title={addr}>
      {n ? <b>{n}</b> : <span className="mono">{short(addr)}</span>}
    </Link>
  );
}
