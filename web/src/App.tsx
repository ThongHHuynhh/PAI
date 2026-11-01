import { useCallback, useEffect, useState } from "react";
import { call, getKey, setKey, type Graph } from "./api";
import { LiveProvider, useLive } from "./live";
import GraphPage from "./pages/Graph";
import Sessions from "./pages/Sessions";
import Knowledge from "./pages/Knowledge";
import Settings from "./pages/Settings";
import Performance from "./pages/Performance";
import { Orbit } from "./components/Orbit";

export type Route = { page: "graph" | "sessions" | "knowledge" | "performance" | "settings"; arg?: string };

function parse(hash: string): Route {
  const [page, arg] = hash.replace(/^#\/?/, "").split("/");
  if (page === "sessions" || page === "knowledge" || page === "performance" || page === "settings") return { page, arg: arg ? decodeURIComponent(arg) : undefined };
  // a phone opens on the chat: the live graph is heavy and small there
  return { page: page !== "graph" && window.matchMedia("(max-width: 699px)").matches ? "sessions" : "graph" };
}

export function go(r: Route) {
  window.location.hash = `/${r.page}${r.arg ? `/${encodeURIComponent(r.arg)}` : ""}`;
}

export default function App() {
  const [authed, setAuthed] = useState(() => !!getKey());
  const out = useCallback(() => {
    setKey(null);
    setAuthed(false);
  }, []);
  if (!authed) return <Login onIn={() => setAuthed(true)} />;
  return (
    <LiveProvider onAuthFail={out}>
      <Shell onOut={out} />
    </LiveProvider>
  );
}

function Login({ onIn }: { onIn: () => void }) {
  useTheme();
  const [key, setK] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    setKey(key.trim());
    try {
      await call<Graph>("GET", "/graph");
      onIn();
    } catch (x: any) {
      setKey(null);
      setErr(x.status === 401 || x.status === 403 ? "That key is not accepted by this server." : String(x.message ?? x));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="login">
      <div className="login-art">
        <Orbit size={260} />
      </div>
      <form className="login-card" onSubmit={submit}>
        <h1>PAI</h1>
        <p className="muted">Your personal AI cloud. Sign in with an API key from this server.</p>
        <label className="field">
          <span>API key</span>
          <input autoFocus type="password" value={key} onChange={(e) => setK(e.target.value)} placeholder="pai_key_…" />
        </label>
        {err && <p className="error">{err}</p>}
        <button className="primary" disabled={!key.trim() || busy}>
          {busy ? "Checking…" : "Sign in"}
        </button>
        <details className="muted small">
          <summary>Where do I get a key?</summary>
          <p>
            On the server machine, run <code>pai-server key new web</code>. The install printed one too, named <code>default</code>.
          </p>
        </details>
      </form>
    </div>
  );
}

const NAV: { page: Route["page"]; label: string; icon: string; hint: string }[] = [
  { page: "graph", label: "Overview", icon: "◎", hint: "Your machines, models and sessions, live" },
  { page: "sessions", label: "Chat", icon: "☰", hint: "Talk to PAI" },
  { page: "knowledge", label: "Memory", icon: "◆", hint: "What PAI has learnt" },
  { page: "performance", label: "Performance", icon: "▤", hint: "How the models, sessions, tools and memory perform" },
  { page: "settings", label: "Settings", icon: "⚙", hint: "Accounts, machines, keys" },
];

// The theme: white unless the person picked dark. Kept in this browser.
const THEME = "pai.theme";
function initialTheme(): "light" | "dark" {
  try {
    const t = localStorage.getItem(THEME);
    if (t === "light" || t === "dark") return t;
  } catch {
    /* no storage: white */
  }
  return "light";
}

export function useTheme(): ["light" | "dark", () => void] {
  const [t, setT] = useState(initialTheme);
  useEffect(() => {
    document.documentElement.dataset.theme = t;
    try {
      localStorage.setItem(THEME, t);
    } catch {
      /* the choice lasts this tab */
    }
  }, [t]);
  return [t, () => setT(t === "light" ? "dark" : "light")];
}

function Shell({ onOut }: { onOut: () => void }) {
  const [route, setRoute] = useState<Route>(() => parse(window.location.hash));
  const { live, graph, error } = useLive();
  useTheme(); // the stored theme applies from the start; Settings switches it
  useEffect(() => {
    const on = () => setRoute(parse(window.location.hash));
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  const online = (graph?.machines ?? []).filter((m) => m.connected).length;
  const stepping = (graph?.sessions ?? []).filter((s) => s.status === "stepping").length;
  return (
    <div className="shell">
      <nav className="nav">
        <div className="brand">
          <Orbit size={34} still />
          <span>PAI</span>
        </div>
        {NAV.map((n) => (
          <a key={n.page} href={`#/${n.page}`} className={route.page === n.page ? "active" : ""} title={n.hint}>
            <span className="icon">{n.icon}</span>
            <span className="label">{n.label}</span>
          </a>
        ))}
        <div className="grow" />
        <div className="nav-status" title={`${online} machine${online === 1 ? "" : "s"} connected, ${stepping} working${graph ? `, PAI ${graph.server.version}` : ""}`}>
          <span className={`dot ${live ? "on" : ""}`} />
          <span className="label">{live ? `${online} machine${online === 1 ? "" : "s"} · ${stepping} working` : "Reconnecting…"}</span>
        </div>
      </nav>
      <main className="main">
        {error && <div className="banner error">{error}</div>}
        {route.page === "graph" && <GraphPage />}
        {route.page === "sessions" && <Sessions id={route.arg} />}
        {route.page === "knowledge" && <Knowledge focus={route.arg} />}
        {route.page === "performance" && <Performance />}
        {route.page === "settings" && <Settings onOut={onOut} />}
      </main>
      <footer className="foot">Copyright © 2026 MTR</footer>
    </div>
  );
}
