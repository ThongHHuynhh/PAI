import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { call, streamEvents, type Graph, type PaiEvent } from "./api";

// Live is the server's state as every page shares it: the graph snapshot,
// refreshed when machines or sessions change, the activity feed, and when
// each node last did something, so the graph can pulse.
type Live = {
  graph: Graph | null;
  live: boolean;
  feed: PaiEvent[];
  pulse: Record<string, number>; // node id -> time of its last activity
  memoryTick: number; // grows on every memory event
  refresh: () => void;
  subscribe: (fn: (e: PaiEvent) => void) => () => void; // every event, until the returned function is called
  error: string;
};

const Ctx = createContext<Live | null>(null);

export function useLive(): Live {
  const v = useContext(Ctx);
  if (!v) throw new Error("useLive outside LiveProvider");
  return v;
}

const FEED = 80;

export function LiveProvider({ children, onAuthFail }: { children: ReactNode; onAuthFail: () => void }) {
  const [graph, setGraph] = useState<Graph | null>(null);
  const [live, setLive] = useState(false);
  const [feed, setFeed] = useState<PaiEvent[]>([]);
  const [pulse, setPulse] = useState<Record<string, number>>({});
  const [memoryTick, setMemoryTick] = useState(0);
  const [error, setError] = useState("");
  const timer = useRef<number | undefined>(undefined);
  const listeners = useRef(new Set<(e: PaiEvent) => void>());
  const subscribe = useCallback((fn: (e: PaiEvent) => void) => {
    listeners.current.add(fn);
    return () => {
      listeners.current.delete(fn);
    };
  }, []);

  const refresh = useCallback(() => {
    call<Graph>("GET", "/graph")
      .then((g) => {
        setGraph(g);
        setError("");
      })
      .catch((e) => {
        if (e.status === 401 || e.status === 403) onAuthFail();
        else setError(String(e.message ?? e));
      });
  }, [onAuthFail]);

  // a burst of events refreshes the snapshot once
  const soon = useCallback(() => {
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(refresh, 400);
  }, [refresh]);

  useEffect(() => {
    refresh();
    const every = window.setInterval(refresh, 15000); // catches what a dropped stream missed
    const stop = streamEvents(
      (e) => {
        listeners.current.forEach((fn) => fn(e));
        if (e.type === "hello") return;
        setFeed((f) => [e, ...f].slice(0, FEED));
        const now = Date.now();
        const hit: Record<string, number> = {};
        if (e.session) hit[`s:${e.session}`] = now;
        if (e.worker) hit[`m:${e.worker}`] = now;
        if (e.type === "memory") {
          hit["memory"] = now;
          setMemoryTick((t) => t + 1);
        }
        if (e.type === "record") hit["core"] = now;
        setPulse((p) => ({ ...p, ...hit }));
        if (e.type === "worker" || (e.type === "record" && ["step", "message", "config", "name", "error"].includes(e.data?.kind))) soon();
      },
      (l) => {
        setLive(l);
        if (l) refresh();
      },
    );
    return () => {
      stop();
      window.clearInterval(every);
    };
  }, [refresh, soon]);

  return <Ctx.Provider value={{ graph, live, feed, pulse, memoryTick, refresh, subscribe, error }}>{children}</Ctx.Provider>;
}

// describe is an event in a few words, for the activity feed.
export function describe(e: PaiEvent): { icon: string; text: string; tone?: "accent" | "error" | "muted" } {
  const d = e.data ?? {};
  switch (e.type) {
    case "worker":
      return d.connected ? { icon: "●", text: `${e.worker} connected` } : { icon: "○", text: `${e.worker} disconnected`, tone: "muted" };
    case "memory":
      switch (d.type) {
        case "remembered":
          return { icon: "◆", text: `Learnt: ${d.items?.[0]?.text ?? ""}`, tone: "accent" };
        case "merged":
          return { icon: "◇", text: `Confirmed: ${d.items?.[0]?.text ?? ""}`, tone: "muted" };
        case "recalled":
          return { icon: "↺", text: `Recalled ${d.detail?.hits?.length ?? 0} memories (chose ${d.detail?.k ?? 0})`, tone: "accent" };
        case "settled":
          return { icon: "★", text: `Recall reward ${Number(d.detail?.reward ?? 0).toFixed(2)}: ${d.detail?.used ?? 0}/${d.detail?.recalled ?? 0} used`, tone: "muted" };
        case "compacted":
          return { icon: "⟲", text: `Compacted a session through record ${d.detail?.upto}`, tone: "accent" };
        case "invalidated":
          return { icon: "✕", text: `Forgot: ${d.items?.[0]?.text ?? ""}`, tone: "muted" };
        default:
          return { icon: "◆", text: `Memory ${d.type}`, tone: "muted" };
      }
    case "record":
      switch (d.kind) {
        case "message":
          return { icon: "›", text: `${d.via ?? "someone"}: ${d.text ?? ""}` };
        case "text":
          return { icon: "‹", text: d.text ?? "", tone: "muted" };
        case "call":
          return { icon: "⚙", text: callName(d.text), tone: "muted" };
        case "step":
          return { icon: "∴", text: `${d.model} step, ${d.usage?.input ?? 0} in / ${d.usage?.output ?? 0} out`, tone: "muted" };
        case "error":
          return { icon: "!", text: d.text ?? "error", tone: "error" };
        case "compaction":
          return { icon: "⟲", text: "Context compacted", tone: "accent" };
        default:
          return { icon: "·", text: d.kind, tone: "muted" };
      }
  }
  return { icon: "·", text: e.type, tone: "muted" };
}

function callName(text?: string): string {
  try {
    const c = JSON.parse(text ?? "");
    const a = c.arguments ?? {};
    return `${c.name}${a.worker ? ` on ${a.worker}` : ""}${a.query ? `: ${a.query}` : ""}`;
  } catch {
    return (text ?? "call").slice(0, 60);
  }
}
