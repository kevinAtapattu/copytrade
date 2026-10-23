export const usd = (n: number | undefined | null, sign = false) => {
  if (n == null || Number.isNaN(n)) return "-";
  const s = Math.abs(n).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  return `${n < 0 ? "-" : sign && n > 0 ? "+" : ""}$${s}`;
};
export const pct = (n: number | undefined | null, digits = 1) =>
  n == null || Number.isNaN(n) ? "-" : `${(n * 100).toFixed(digits)}%`;
export const num = (n: number | undefined | null, digits = 2) =>
  n == null || Number.isNaN(n) ? "-" : n.toLocaleString("en-US", { maximumFractionDigits: digits });
export const sol = (n: number | undefined | null) => (n == null ? "-" : `${n.toFixed(4)} SOL`);
export const short = (a: string | undefined) => (!a ? "-" : a.length <= 10 ? a : `${a.slice(0, 4)}..${a.slice(-4)}`);
export const price = (n: number | undefined | null) => (n == null || n === 0 ? "-" : n.toExponential(3));
export const ago = (iso: string | undefined | null) => {
  if (!iso || iso.startsWith("0001")) return "never";
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (s < 0) return "just now";
  if (s < 60) return `${Math.round(s)}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
};
export const dur = (s: number | undefined) => {
  if (!s) return "-";
  if (s < 60) return `${Math.round(s)}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${(s / 3600).toFixed(1)}h`;
  return `${(s / 86400).toFixed(1)}d`;
};
export const time = (iso: string | undefined) => (iso ? new Date(iso).toLocaleString() : "-");
export const pnlClass = (n: number | undefined | null) => (n == null || n === 0 ? "" : n > 0 ? "pos" : "neg");
