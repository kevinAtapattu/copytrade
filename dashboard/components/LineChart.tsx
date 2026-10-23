"use client";
import { CartesianGrid, Line, LineChart as RLineChart, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { usd } from "@/lib/format";

export type Series = { key: string; label: string; color: string };

// Multi-series line chart (one y-axis, USD). Rows are {x, [series.key]: number}.
export default function LineChart({ rows, series, height = 260 }: {
  rows: Record<string, number | string>[]; series: Series[]; height?: number;
}) {
  if (!rows.length) return <div className="empty" style={{ height }}>No closed or open positions yet</div>;
  return (
    <div>
      {series.length > 1 && (
        <div className="legend">
          {series.map((s) => (
            <span key={s.key}><i style={{ background: s.color }} />{s.label}</span>
          ))}
        </div>
      )}
      <ResponsiveContainer width="100%" height={height}>
        <RLineChart data={rows} margin={{ top: 8, right: 12, bottom: 0, left: 0 }}>
          <CartesianGrid stroke="var(--grid)" vertical={false} />
          <XAxis dataKey="x" tick={{ fill: "var(--text-muted)", fontSize: 11 }} stroke="var(--axis)" tickLine={false} minTickGap={24} />
          <YAxis tick={{ fill: "var(--text-muted)", fontSize: 11 }} stroke="var(--axis)" tickLine={false} axisLine={false}
            width={64} tickFormatter={(v) => usd(Number(v))} />
          <ReferenceLine y={0} stroke="var(--axis)" />
          <Tooltip
            cursor={{ stroke: "var(--text-muted)", strokeWidth: 1 }}
            content={({ active, payload, label }) =>
              active && payload?.length ? (
                <div className="tip">
                  <div className="k">{String(label)}</div>
                  {series.map((s) => {
                    const p = payload.find((x) => x.dataKey === s.key);
                    return p ? (
                      <div className="r" key={s.key}>
                        <i style={{ background: s.color }} />
                        <b>{usd(Number(p.value), true)}</b>
                        <span>{s.label}</span>
                      </div>
                    ) : null;
                  })}
                </div>
              ) : null
            }
          />
          {series.map((s) => (
            <Line key={s.key} type="monotone" dataKey={s.key} stroke={s.color} strokeWidth={2} dot={false}
              activeDot={{ r: 4, stroke: "var(--surface-1)", strokeWidth: 2 }} connectNulls isAnimationActive={false} />
          ))}
        </RLineChart>
      </ResponsiveContainer>
    </div>
  );
}

// mergeSeries joins several {date,cum} series into chart rows keyed by date.
export function mergeSeries(input: Record<string, { date: string; cum: number }[] | undefined>) {
  const by = new Map<string, Record<string, number | string>>();
  for (const [key, pts] of Object.entries(input)) {
    for (const p of pts || []) {
      const row = by.get(p.date) || { x: p.date };
      row[key] = p.cum;
      by.set(p.date, row);
    }
  }
  return [...by.values()].sort((a, b) => String(a.x).localeCompare(String(b.x)));
}
