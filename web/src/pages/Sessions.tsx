import { useEffect, useMemo, useRef, useState } from "react";
import { ago, call, records, sessionTitle, tokens, type Catalog, type ProviderInfo, type Rec, type Session } from "../api";
import { useLive } from "../live";
import { go } from "../App";
import { splitModel } from "./Graph";

export default function Sessions({ id }: { id?: string }) {
  const { graph, refresh } = useLive();
  const [creating, setCreating] = useState(false);
  const [filter, setFilter] = useState("");
  const list = useMemo(
    () =>
      [...(graph?.sessions ?? [])]
        .sort((a, b) => b.updated.localeCompare(a.updated))
        .filter((s) => !filter || `${sessionTitle(s)} ${s.config.model}`.toLowerCase().includes(filter.toLowerCase())),
    [graph, filter],
  );
  const cur = list.find((s) => s.id === id) ?? (graph?.sessions ?? []).find((s) => s.id === id);
  return (
    <div className="sessions-page">
      <aside className="session-list">
        <div className="row">
          <input placeholder="Filter sessions" value={filter} onChange={(e) => setFilter(e.target.value)} />
          <button className="primary small" onClick={() => setCreating(true)}>
            New
          </button>
        </div>
        {list.length === 0 && <p className="muted small pad">No sessions yet. Start one with New.</p>}
        {list.map((s) => {
          const fill = s.config.context_limit ? s.context_used / s.config.context_limit : 0;
          return (
            <a key={s.id} href={`#/sessions/${s.id}`} className={`session-row ${s.id === id ? "active" : ""}`}>
              <div className="grow-row">
                <span className={`pill ${s.status}`}>{s.status === "stepping" ? "working" : s.status}</span>
                <b className="ellipsis">{sessionTitle(s)}</b>
              </div>
              <div className="muted small ellipsis">
                {splitModel(s.config.model)[1]} · {ago(s.updated)}
              </div>
              <div className="meter thin">
                <i style={{ width: `${Math.min(1, fill) * 100}%` }} className={fill > 0.7 ? "hot" : ""} />
              </div>
            </a>
          );
        })}
      </aside>
      <section className="session-view">
        {cur ? <Transcript key={cur.id} s={cur} /> : <Empty onNew={() => setCreating(true)} />}
      </section>
      {creating && (
        <NewSession
          onClose={() => setCreating(false)}
          onMade={(sid) => {
            setCreating(false);
            refresh();
            go({ page: "sessions", arg: sid });
          }}
        />
      )}
    </div>
  );
}

function Empty({ onNew }: { onNew: () => void }) {
  return (
    <div className="empty">
      <h2>Talk to PAI</h2>
      <p className="muted">Pick a session on the left, or start a new one. Every session shares PAI's memory, whatever model or machine it runs on.</p>
      <button className="primary" onClick={onNew}>
        New session
      </button>
    </div>
  );
}

// Transcript follows a session's records with a long poll, and posts what
// the person types.
function Transcript({ s }: { s: Session }) {
  const [recs, setRecs] = useState<Rec[]>([]);
  const [err, setErr] = useState("");
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const end = useRef<HTMLDivElement>(null);
  const stuck = useRef(true);

  useEffect(() => {
    let alive = true;
    let after = 0;
    (async () => {
      while (alive) {
        try {
          const got = await records(s.id, after, after ? "25s" : "");
          if (!alive) return;
          if (got.length) {
            after = got[got.length - 1].n;
            setRecs((r) => [...r, ...got]);
          }
          setErr("");
        } catch (e: any) {
          setErr(String(e.message ?? e));
          await new Promise((r) => setTimeout(r, 3000));
        }
      }
    })();
    return () => {
      alive = false;
    };
  }, [s.id]);

  useEffect(() => {
    if (stuck.current) end.current?.scrollIntoView({ block: "end" });
  }, [recs]);

  const send = async () => {
    const t = text.trim();
    if (!t) return;
    setSending(true);
    try {
      await call("POST", `/sessions/${s.id}/records?via=web`, t, "text/plain; charset=utf-8");
      setText("");
      stuck.current = true;
    } catch (e: any) {
      setErr(String(e.message ?? e));
    } finally {
      setSending(false);
    }
  };

  const results = useMemo(() => Object.fromEntries(recs.filter((r) => r.kind === "result").map((r) => [r.for, r])), [recs]);
  const fill = s.config.context_limit ? s.context_used / s.config.context_limit : 0;

  return (
    <div className="transcript">
      <header className="transcript-head">
        <div>
          <h2>{sessionTitle(s)}</h2>
          <div className="muted small">
            {s.config.model} · {s.config.workers?.map((w) => w.name).join(", ") || "no machines"} · {s.id}
          </div>
        </div>
        <div className="head-right">
          <div className="ctx" title="Context in use. PAI compacts at 70%.">
            <div className="meter">
              <i style={{ width: `${Math.min(1, fill) * 100}%` }} className={fill > 0.7 ? "hot" : ""} />
              <span className="mark" style={{ left: "70%" }} />
            </div>
            <span className="small muted">
              {tokens(s.context_used)} / {tokens(s.config.context_limit)}
            </span>
          </div>
          {s.status === "stepping" ? (
            <button className="ghost small" onClick={() => call("POST", `/sessions/${s.id}/stop?via=web`)}>
              Stop
            </button>
          ) : s.status === "failed" ? (
            <button className="ghost small" onClick={() => call("POST", `/sessions/${s.id}/retry`)}>
              Retry
            </button>
          ) : null}
        </div>
      </header>
      <div
        className="records"
        onScroll={(e) => {
          const el = e.currentTarget;
          stuck.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
        }}
      >
        {recs.map((r) => (
          <Record key={r.n} r={r} result={r.kind === "call" ? results[r.id] : undefined} />
        ))}
        {s.status === "stepping" && <div className="thinking">PAI is working…</div>}
        <div ref={end} />
      </div>
      {err && <div className="banner error">{err}</div>}
      <footer className="composer">
        <textarea
          value={text}
          placeholder="Message PAI. Enter sends, Shift+Enter is a new line."
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              void send();
            }
          }}
        />
        <button className="primary" disabled={sending || !text.trim()} onClick={send}>
          Send
        </button>
      </footer>
    </div>
  );
}

function Record({ r, result }: { r: Rec; result?: Rec }) {
  const [open, setOpen] = useState(false);
  switch (r.kind) {
    case "message":
      return (
        <div className="msg user">
          <div className="who">{r.via}</div>
          <Text s={r.media ? `[${r.type}]` : r.body} />
        </div>
      );
    case "text":
      return (
        <div className="msg ai">
          <Text s={r.body} />
        </div>
      );
    case "call": {
      let name = "call",
        args: any = {};
      try {
        const c = JSON.parse(r.body);
        name = c.name;
        args = c.arguments;
      } catch {
        /* raw */
      }
      const memoryTool = ["recall", "remember", "forget"].includes(name);
      const head = args.worker ? `${name} on ${args.worker}` : name;
      const gist = args.code ?? args.path ?? args.query ?? args.text ?? args.id ?? "";
      return (
        <div className={`callrow ${memoryTool ? "mem" : ""}`} onClick={() => setOpen(!open)}>
          <div className="call-head">
            <span className="ficon">{memoryTool ? "◆" : "⚙"}</span>
            <b>{head}</b>
            <span className="muted ellipsis mono">{String(gist).split("\n")[0]}</span>
            {result && <span className={`exit ${result.exit ? "bad" : ""}`}>{result.exit !== undefined ? `exit ${result.exit}` : "done"}</span>}
          </div>
          {open && (
            <div className="call-body">
              <pre>{typeof args === "object" ? JSON.stringify(args, null, 2) : String(args)}</pre>
              {result && <pre className="result">{result.body}</pre>}
            </div>
          )}
        </div>
      );
    }
    case "system":
      if (r.source !== "memory") return null;
      return <MemoryCard r={r} />;
    case "compaction":
      return (
        <div className="memcard compaction" onClick={() => setOpen(!open)}>
          <div className="memhead">
            <span className="accent">⟲</span> Compacted records 1–{r.upto} into a summary. The full history stays in the session file.
          </div>
          {open && <Text s={r.body} />}
        </div>
      );
    case "error":
      return (
        <div className="msg err">
          <Text s={r.body} />
        </div>
      );
    case "step":
      return r.usage ? (
        <div className="stepline">
          {r.model} · {tokens(r.usage.input + (r.usage.cache_read ?? 0) + (r.usage.cache_write ?? 0))} in{r.usage.cache_read ? ` (${tokens(r.usage.cache_read)} cached)` : ""} · {tokens(r.usage.output)} out
        </div>
      ) : null;
    default:
      return null;
  }
}

// MemoryCard is what memory recalled for a turn: each item links to the
// knowledge view.
function MemoryCard({ r }: { r: Rec }) {
  const [open, setOpen] = useState(false);
  const items = [...r.body.matchAll(/^- \[(m[0-9a-f]+)\] \(([^,]+), ([^)]+)\) (.*)$/gm)].map((m) => ({ id: m[1], kind: m[2], scope: m[3], text: m[4] }));
  return (
    <div className="memcard" onClick={() => setOpen(!open)}>
      <div className="memhead">
        <span className="accent">◆</span> Recalled {items.length} {items.length === 1 ? "memory" : "memories"} for this turn
        <span className="muted small"> · {open ? "hide" : "show"}</span>
      </div>
      {open && (
        <ul>
          {items.map((it) => (
            <li key={it.id}>
              <span className={`kind k-${it.kind}`}>{it.kind}</span> {it.text}{" "}
              <a
                href={`#/knowledge/${it.id}`}
                onClick={(e) => e.stopPropagation()}
                className="small"
              >
                {it.id}
              </a>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// Text renders text with fenced code blocks kept apart; the rest keeps its
// line breaks.
export function Text({ s }: { s: string }) {
  const parts = s.split(/```[a-z]*\n?/);
  return (
    <div className="text">
      {parts.map((p, i) => (i % 2 ? <pre key={i}>{p}</pre> : <p key={i}>{p}</p>))}
    </div>
  );
}

function NewSession({ onClose, onMade }: { onClose: () => void; onMade: (id: string) => void }) {
  const { graph } = useLive();
  const [cat, setCat] = useState<Catalog | null>(null);
  const [served, setServed] = useState<ProviderInfo[]>([]);
  const [preset, setPreset] = useState("");
  const [custom, setCustom] = useState("");
  const [limit, setLimit] = useState(200000);
  const [workers, setWorkers] = useState<string[]>([]);
  const [first, setFirst] = useState("");
  const [err, setErr] = useState("");
  const machines = (graph?.machines ?? []).filter((m) => m.connected);

  useEffect(() => {
    Promise.all([call<Catalog>("GET", "/catalog"), call<ProviderInfo[]>("GET", "/providers")]).then(([c, p]) => {
      setCat(c);
      setServed(p);
      const names = new Set(p.map((x) => x.name));
      const first = (c.groups["self-hosted-default"] ?? []).find((id) => names.has(c.presets[id]?.provider));
      if (first) {
        setPreset(first);
        setLimit(c.presets[first].context_limit);
      }
    }, (e) => setErr(String(e.message ?? e)));
    setWorkers(machines.map((m) => m.name));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const names = new Set(served.map((p) => p.name));
  const presets = cat ? (cat.groups["self-hosted-all"] ?? []).filter((id) => names.has(cat.presets[id]?.provider)) : [];

  const make = async () => {
    setErr("");
    try {
      const p = cat?.presets[preset];
      const body = custom.trim()
        ? { model: custom.trim(), tools: "default", context_limit: limit, workers: workers.map((name) => ({ name })) }
        : { model: `${p!.provider}/${p!.model}`, tools: p!.tools, context_limit: limit, input: p!.input, params: p!.params, preset, workers: workers.map((name) => ({ name })) };
      const { id } = await call<{ id: string }>("POST", "/sessions?via=web", body);
      if (first.trim()) await call("POST", `/sessions/${id}/records?via=web`, first.trim(), "text/plain; charset=utf-8");
      onMade(id);
    } catch (e: any) {
      setErr(String(e.message ?? e));
    }
  };

  return (
    <div className="modal-back" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h2>New session</h2>
        {served.length === 0 && cat && (
          <p className="banner">
            No model provider yet. <a href="#/settings">Connect one in Settings</a>: a Claude or ChatGPT subscription, or an API key.
          </p>
        )}
        <label className="field">
          <span>Model</span>
          <select
            value={preset}
            onChange={(e) => {
              setPreset(e.target.value);
              if (cat?.presets[e.target.value]) setLimit(cat.presets[e.target.value].context_limit);
            }}
          >
            {presets.map((id) => (
              <option key={id} value={id}>
                {cat!.presets[id].name} — {cat!.providers[cat!.presets[id].provider]?.name ?? cat!.presets[id].provider}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>Or any model, as provider/model</span>
          <input value={custom} onChange={(e) => setCustom(e.target.value)} placeholder="ollama/qwen3:32b" />
        </label>
        <label className="field">
          <span>Context limit (tokens). PAI compacts at 70%.</span>
          <input type="number" min={8000} step={1000} value={limit} onChange={(e) => setLimit(Number(e.target.value))} />
        </label>
        <div className="field">
          <span>Machines this session may use</span>
          {machines.length === 0 && <p className="muted small">No machine is connected. The session can still talk and use memory.</p>}
          <div className="chips">
            {machines.map((m) => (
              <label key={m.name} className={`chip ${workers.includes(m.name) ? "on" : ""}`}>
                <input type="checkbox" checked={workers.includes(m.name)} onChange={(e) => setWorkers(e.target.checked ? [...workers, m.name] : workers.filter((w) => w !== m.name))} />
                {m.name}
              </label>
            ))}
          </div>
        </div>
        <label className="field">
          <span>First message (optional)</span>
          <textarea value={first} onChange={(e) => setFirst(e.target.value)} placeholder="What should PAI do?" />
        </label>
        {err && <p className="error">{err}</p>}
        <div className="row end">
          <button className="ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={!preset && !custom.trim()} onClick={make}>
            Start
          </button>
        </div>
      </div>
    </div>
  );
}
