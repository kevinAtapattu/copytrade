"use client";
import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";

export type Score = {
  wallet: string; delay_s: number; window_days: number; computed_at: string; positions: number; closed: number;
  pnl_usd: number; pnl_ex_best_usd: number; best_trade_usd: number; win_rate: number; max_drawdown: number;
  avg_hold_s: number; trades_per_day: number; active_days: number; lucky_trade: boolean; score: number;
};
export type Wallet = {
  address: string; label: string; status: "live" | "bench" | "dropped"; source: string; reason: string;
  added_at: string; status_at: string; last_trade_at?: string;
};
// Them vs us: a wallet's own return on each token next to our paper copy's (by delay).
export type CopyResult = { cost_sol: number; pnl_usd: number; pnl_pct: number; open: boolean };
export type CompareRow = {
  mint: string; first_buy_at: string; last_at: string; their_sol_in: number; their_pct: number; their_open: boolean;
  ours: Record<string, CopyResult>;
};
export type CompareSummary = {
  tokens: number; their_avg_pct: number; our_avg_pct: Record<string, number>; copied: Record<string, number>;
  their_same_pct: Record<string, number>;
};
export type Trade = {
  signature: string; mint: string; trader: string; side: "buy" | "sell"; sol_amount: number; token_amount: number;
  new_token_balance: number; v_sol: number; v_tokens: number; pool: string; received_at: string; is_target: boolean;
};
export type SimTrade = {
  id: string; position_id: string; target_sig: string; wallet: string; mint: string; delay_s: number; side: string;
  target_at: string; fill_at: string; sol_amount: number; token_amount: number; spot_price: number; fill_price: number;
  target_price: number; slippage_pct: number; pump_fee_sol: number; bot_fee_sol: number; network_fee_sol: number;
  sol_usd: number; sell_fraction?: number; skipped: boolean; skip_reason?: string;
};
export type SimPosition = {
  id: string; wallet: string; mint: string; delay_s: number; opened_at: string; closed_at?: string; status: string;
  cost_sol: number; proceeds_sol: number; tokens_held: number; tokens_bought: number; sol_usd: number;
  mark_pnl_usd?: number; last_price?: number;
};
export type LiveFill = {
  id: string; mode: string; target_sig: string; wallet: string; mint: string; side: string; reason: string;
  target_at: string; sent_at: string; filled_at?: string; signature?: string; status: string; error?: string;
  attempts: number; sol_amount: number; token_amount: number; fill_price: number; fees_sol: number; sol_usd: number;
  sim_fill_price?: number;
};
export type LivePosition = {
  id: string; mode: string; wallet: string; mint: string; opened_at: string; closed_at?: string; status: string;
  cost_sol: number; proceeds_sol: number; tokens_held: number; entry_price: number; sol_usd: number;
};
export type Point = { date: string; pnl: number; cum: number };
export type Rotation = { id: number; wallet: string; from_status: string; to_status: string; reason: string; at: string };
export type AppEvent = { kind: string; at: string; service: string; message: string; data?: unknown };

export async function apiSend(path: string, method: string, body?: unknown) {
  const res = await fetch(`/api/${path}`, {
    method, headers: { "content-type": "application/json" }, body: body ? JSON.stringify(body) : undefined,
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(json.error || res.statusText);
  return json;
}

// ---- live events (SSE) shared by all pages

type EventsCtx = { tick: number; events: AppEvent[]; connected: boolean };
const Ctx = createContext<EventsCtx>({ tick: 0, events: [], connected: false });
export const EventsContext = Ctx;

export function useEventsState(): EventsCtx {
  const [state, setState] = useState<EventsCtx>({ tick: 0, events: [], connected: false });
  useEffect(() => {
    const es = new EventSource("/api/stream");
    let pending = false;
    es.onopen = () => setState((s) => ({ ...s, connected: true }));
    es.onerror = () => setState((s) => ({ ...s, connected: false }));
    es.onmessage = (m) => {
      let ev: AppEvent;
      try { ev = JSON.parse(m.data); } catch { return; }
      setState((s) => ({ ...s, events: [ev, ...s.events].slice(0, 100) }));
      // Throttle refetches to one every 2s no matter how busy the stream is.
      if (!pending) {
        pending = true;
        setTimeout(() => { pending = false; setState((s) => ({ ...s, tick: s.tick + 1 })); }, 2000);
      }
    };
    return () => es.close();
  }, []);
  return state;
}

export function useEvents() { return useContext(Ctx); }

// useApi fetches path, refetching on live events and every refreshMs.
export function useApi<T>(path: string | null, refreshMs = 15000) {
  const { tick } = useEvents();
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const alive = useRef(true);
  const load = useCallback(async () => {
    if (!path) return;
    try {
      const res = await fetch(`/api/${path}`, { cache: "no-store" });
      const json = await res.json();
      if (!res.ok) throw new Error(json.error || res.statusText);
      if (alive.current) { setData(json); setError(null); }
    } catch (e) {
      if (alive.current) setError(String((e as Error).message || e));
    } finally {
      if (alive.current) setLoading(false);
    }
  }, [path]);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => { load(); }, [load, tick]);
  useEffect(() => {
    const t = setInterval(load, refreshMs);
    return () => clearInterval(t);
  }, [load, refreshMs]);
  return { data, error, loading, reload: load };
}
