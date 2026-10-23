"use client";
import { useMemo, useState } from "react";
import { pnlClass } from "@/lib/format";

export function Stat({ label, value, hint, tone }: { label: string; value: React.ReactNode; hint?: React.ReactNode; tone?: number }) {
  return (
    <div className="card stat">
      <div className="label">{label}</div>
      <div className={`value ${tone !== undefined ? pnlClass(tone) : ""}`}>{value}</div>
      {hint && <div className="hint">{hint}</div>}
    </div>
  );
}

export function Err({ error }: { error: string | null }) {
  if (!error) return null;
  return <div className="err">Could not load data: {error}. Is the API running? (make up, or go run ./cmd/api)</div>;
}

export function Seg<T extends string | number>({ value, options, onChange }: {
  value: T; options: { v: T; label: string }[]; onChange: (v: T) => void;
}) {
  return (
    <div className="seg">
      {options.map((o) => (
        <button key={String(o.v)} className={o.v === value ? "on" : ""} onClick={() => onChange(o.v)}>
          {o.label}
        </button>
      ))}
    </div>
  );
}

export type Col<T> = {
  key: string;
  label: string;
  num?: boolean;
  sort?: (r: T) => number | string;
  render: (r: T) => React.ReactNode;
};

export function Table<T>({ rows, cols, initialSort, empty = "Nothing yet", rowKey }: {
  rows: T[]; cols: Col<T>[]; initialSort?: { key: string; desc: boolean }; empty?: string; rowKey: (r: T) => string;
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
  if (!rows.length) return <div className="empty">{empty}</div>;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            {cols.map((c) => (
              <th key={c.key} className={`${c.num ? "num" : ""} ${c.sort ? "sortable" : ""}`}
                onClick={() => c.sort && setSort({ key: c.key, desc: sort?.key === c.key ? !sort.desc : true })}>
                {c.label}{sort?.key === c.key ? (sort.desc ? " ↓" : " ↑") : ""}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sorted.map((r) => (
            <tr key={rowKey(r)}>
              {cols.map((c) => (
                <td key={c.key} className={c.num ? "num" : ""}>{c.render(r)}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
