"use client";
import { createContext, useContext, useEffect, useState } from "react";
import { useApi } from "@/lib/api";

// "real": only what the auto-trader does with your money (the default while it
// trades live). "all": everything we watch, paper copies included.
export type View = "real" | "all";
// ready: the executor's mode is known, so pages don't flash the wrong view.
type Ctx = { view: View; setView: (v: View) => void; live: boolean; ready: boolean };
const ViewContext = createContext<Ctx>({ view: "all", setView: () => {}, live: false, ready: false });

type Health = { services?: { executor?: { data?: { mode?: string } } } };

export function ViewProvider({ children }: { children: React.ReactNode }) {
  const { data } = useApi<Health>("health", 30000);
  const live = data?.services?.executor?.data?.mode === "live";
  const [picked, setPicked] = useState<View | null>(null);
  useEffect(() => {
    try {
      const v = localStorage.getItem("view");
      if (v === "real" || v === "all") setPicked(v);
    } catch { /* storage blocked: follow the mode */ }
  }, []);
  const setView = (v: View) => {
    setPicked(v);
    try { localStorage.setItem("view", v); } catch { /* not remembered, still applied */ }
  };
  const view: View = live ? (picked ?? "real") : "all";
  return <ViewContext.Provider value={{ view, setView, live, ready: data != null }}>{children}</ViewContext.Provider>;
}

export const useView = () => useContext(ViewContext);
