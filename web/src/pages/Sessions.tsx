import { Ask, questionsOf } from "../components/Ask";
import { memo, useEffect, useMemo, useRef, useState } from "react";
import { ago, call, records, sessionModels, sessionTitle, tokens, type Catalog, type ProviderInfo, type Rec, type Retrieval, type Session, type SessionModel } from "../api";
import { useLive } from "../live";
import { go } from "../App";
import { ItemActions, Nudge, RetrievalBar, useRetrievals, verdict } from "../components/Retrieval";
import { splitModel } from "./Graph";

// LIST is where this browser keeps whether the session list is shown.
const LIST = "pai.sessions.list";
// SHOWN is how many of the newest records a transcript draws at first: a
// long session drawn whole is too heavy for a phone.
const SHOWN = 150;
// A touch keyboard has no Shift+Enter: there Enter is a new line and Send sends.
const touch = window.matchMedia("(pointer: coarse)").matches;

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
  // A child sits under its orchestrator. One whose orchestrator is not listed, deleted or filtered out, stands alone.
  const tree = useMemo(() => {
    const ids = new Set(list.map((s) => s.id));
    const kids = new Map<string, Session[]>();
    for (const s of list) if (s.parent && ids.has(s.parent)) kids.set(s.parent, [...(kids.get(s.parent) ?? []), s]);
    const rows: { s: Session; depth: number; kids: Session[] }[] = [];
    const add = (s: Session, depth: number) => {
      rows.push({ s, depth, kids: kids.get(s.id) ?? [] });
      for (const k of kids.get(s.id) ?? []) add(k, depth + 1);
    };
    for (const s of list) if (!s.parent || !ids.has(s.parent)) add(s, 0);
    return rows;
  }, [list]);
  const cur = list.find((s) => s.id === id) ?? (graph?.sessions ?? []).find((s) => s.id === id);
  // The list stays beside an open session; a person folds it to a thin rail.
  // Unset, it starts folded on a phone. With no session open it is the page.
  const [open, setOpen] = useState(() => (localStorage.getItem(LIST) ?? (window.matchMedia("(max-width: 699px)").matches ? "0" : "1")) === "1");
  const fold = () => {
    localStorage.setItem(LIST, open ? "0" : "1");
    setOpen(!open);
  };
  const shown = open || !cur;
  return (
    <div className={`sessions-page ${cur ? "has-cur" : ""} ${shown ? "" : "folded"}`}>
      <aside className="session-list">
        {!shown && (
          <button className="icon-btn" onClick={fold} title="Show sessions" aria-label="Show sessions">
            ›
          </button>
        )}
        {shown && (
        <div className="row">
          <input placeholder="Filter sessions" value={filter} onChange={(e) => setFilter(e.target.value)} />
          <button className="primary small" onClick={() => setCreating(true)}>
            New
          </button>
          {cur && (
            <button className="icon-btn" onClick={fold} title="Hide sessions" aria-label="Hide sessions">
              ‹
            </button>
          )}
        </div>
        )}
        {shown && list.length === 0 && <p className="muted small pad">No sessions yet. Start one with New.</p>}
        {shown && tree.map(({ s, depth, kids }) => {
          const fill = s.config.context_limit ? s.context_used / s.config.context_limit : 0;
          return (
            <a key={s.id} href={`#/sessions/${s.id}`} className={`session-row ${s.id === id ? "active" : ""} ${depth ? "child" : ""}`}>
              <div className="grow-row">
                <span className={`pill ${s.status}`}>{s.status === "stepping" ? "working" : s.status}</span>
                <b className="ellipsis">{sessionTitle(s)}</b>
                {kids.length > 0 && (
                  <span className="muted small nowrap" title="Sessions this one delegated to">
                    {kids.length} child{kids.length > 1 ? "ren" : ""}
                    {kids.some((k) => k.status === "stepping") ? ", working" : ""}
                  </span>
                )}
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
  const [hidden, setHidden] = useState(""); // the ask call whose popup was dismissed
  const box = useRef<HTMLDivElement>(null);
  const stuck = useRef(true);
  const [shown, setShown] = useState(SHOWN);

  // New records are read when the event stream says the session has them,
  // not by a long poll: a request held open per chat tab would use up the
  // six connections a browser opens to the server.
  const { live, subscribe } = useLive();
  const pull = useRef<() => void>(() => {});
  useEffect(() => {
    let alive = true;
    let after = 0;
    let seen = 0; // the newest record an event named
    let busy = false;
    let again = false;
    const get = async () => {
      if (busy) {
        again = true;
        return;
      }
      busy = true;
      try {
        do {
          again = false;
          const got = await records(s.id, after);
          if (!alive) return;
          if (got.length) {
            after = got[got.length - 1].n;
            setRecs((r) => [...r, ...got]);
          }
          setErr("");
        } while (again && alive);
      } catch (e: any) {
        if (alive) setErr(String(e.message ?? e));
      } finally {
        busy = false;
      }
      if (alive && seen > after) window.setTimeout(get, 500); // the event came before the record could be read
    };
    pull.current = () => void get();
    void get();
    const off = subscribe((e) => {
      if (e.type !== "record" || e.session !== s.id) return;
      seen = Math.max(seen, Number(e.data?.n ?? 0));
      if (seen > after) void get();
    });
    const every = window.setInterval(get, 15000); // catches what a dropped stream missed
    return () => {
      alive = false;
      off();
      window.clearInterval(every);
    };
  }, [s.id, subscribe]);
  useEffect(() => {
    if (live) pull.current(); // the stream is back: read what it missed
  }, [live]);

  useEffect(() => {
    const el = box.current;
    if (el && stuck.current) el.scrollTop = el.scrollHeight;
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
  // Tool calls and step lines in a row fold into one line that opens on a tap,
  // so a long run of commands does not bury the conversation.
  const groups = useMemo(() => {
    const out: (Rec | Rec[])[] = [];
    for (const r of recs.slice(-shown)) {
      if (r.kind === "result" || r.kind === "thinking") continue;
      if (r.kind !== "call" && r.kind !== "step") out.push(r);
      else if (Array.isArray(out[out.length - 1])) (out[out.length - 1] as Rec[]).push(r);
      else out.push([r]);
    }
    return out;
  }, [recs, shown]);
  const asking = s.status === "stepping" ? recs.filter((r) => r.kind === "call" && !results[r.id] && questionsOf(r).length > 0).pop() : undefined;
  const retrievals = useRetrievals(s.id);
  const recalls = useMemo(() => Object.fromEntries(retrievals.map((r) => [r.id, r])), [retrievals]);
  const fill = s.config.context_limit ? s.context_used / s.config.context_limit : 0;

  return (
    <div className="transcript">
      <header className="transcript-head">
        <button className="icon-btn back" onClick={() => go({ page: "sessions" })} title="All sessions" aria-label="All sessions">
          ‹
        </button>
        <div className="grow" style={{ minWidth: 0 }}>
          <h2 className="ellipsis">{sessionTitle(s)}</h2>
          <div className="muted small ellipsis">
            <ModelSwitch s={s} /> · {s.config.workers?.map((w) => w.name).join(", ") || "no machines"}
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
        ref={box}
        onScroll={(e) => {
          const el = e.currentTarget;
          stuck.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
        }}
      >
        {recs.length > shown && (
          <button className="ghost small earlier" onClick={() => setShown(shown + SHOWN)}>
            Show earlier ({recs.length - shown} more)
          </button>
        )}
        {groups.map((g) =>
          Array.isArray(g) ? (
            <Calls key={g[0].n} rs={g} results={results} recalls={recalls} />
          ) : (
            <Record key={g.n} r={g} recalls={recalls} />
          ),
        )}
        {s.status === "stepping" && <div className="thinking">PAI is working…</div>}
      </div>
      {asking && (
        hidden === asking.id ? (
          <button className="banner ask-bar" onClick={() => setHidden("")}>
            PAI is waiting for your answers. Open the questions.
          </button>
        ) : (
          <Ask key={asking.id} session={s.id} call={asking} onDismiss={() => setHidden(asking.id)} />
        )
      )}
      <RetrievalBar session={s.id} rs={retrievals} />
      {err && <div className="banner error">{err}</div>}
      <footer className="composer">
        <textarea
          value={text}
          placeholder={touch ? "Message PAI" : "Message PAI. Enter sends, Shift+Enter is a new line."}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey && !touch) {
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

// ModelSwitch asks the server which models can take the session's context.
function ModelSwitch({ s }: { s: Session }) {
  const { refresh } = useLive();
  const [open, setOpen] = useState(false);
  const [models, setModels] = useState<SessionModel[] | null>(null);
  const [err, setErr] = useState("");
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    if (!open) return;
    let alive = true;
    setModels(null);
    setErr("");
    sessionModels(s.id).then(
      (rows) => { if (alive) setModels(rows); },
      (e) => { if (alive) setErr(String(e.message ?? e)); },
    );
    return () => { alive = false; };
  }, [open, s.id]);
  const change = async (to: string) => {
    setSaving(true);
    setErr("");
    try {
      await call("PUT", `/sessions/${s.id}/model?via=web`, to, "text/plain");
      setOpen(false);
      refresh();
    } catch (e: any) {
      setErr(String(e.message ?? e));
    } finally {
      setSaving(false);
    }
  };
  return (
    <>
      <button className="link" onClick={() => setOpen(true)} aria-haspopup="dialog" title="Change model from the next step">
        {s.config.model}
      </button>
      {open && (
        <div className="modal-back" onClick={() => setOpen(false)} onKeyDown={(e) => { if (e.key === "Escape") setOpen(false); }}>
          <div className="modal model-picker" role="dialog" aria-modal="true" aria-labelledby="model-title" onClick={(e) => e.stopPropagation()}>
            <div className="grow-row">
              <h3 id="model-title">Model for this session</h3>
              <button className="ghost small" autoFocus onClick={() => setOpen(false)}>Close</button>
            </div>
            <p className="muted small">Changes apply from the next step. History and memory come along.</p>
            {!models && !err && <p className="muted">Loading models…</p>}
            {models?.map((m) => (
              <button key={m.model} className="ghost model-choice" disabled={!m.ok || m.current || saving} title={m.reason || m.model} onClick={() => void change(m.model)}>
                <span>{m.name}{m.current ? " · current" : ""}</span>
                <span className="muted small">{m.provider} · {tokens(m.context_limit)} context</span>
                {!m.ok && <span className="small">{m.reason}</span>}
              </button>
            ))}
            {err && <p className="error" role="alert">{err}</p>}
          </div>
        </div>
      )}
    </>
  );
}

// Record is drawn again only when its own props change, not on every new record.
const Record = memo(function Record({ r, result, recalls }: { r: Rec; result?: Rec; recalls: { [id: string]: Retrieval } }) {
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
      const { args, head, gist, memoryTool } = parseCall(r);
      return (
        <div className={`callrow ${memoryTool ? "mem" : ""}`} onClick={() => setOpen(!open)}>
          <div className="call-head">
            <span className="ficon">{memoryTool ? "◆" : "⚙"}</span>
            <b>{head}</b>
            <span className="muted ellipsis mono">{gist}</span>
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
      if (r.source === "failover")
        return (
          <div className="memcard compaction">
            <div className="memhead">
              <span className="accent">⇄</span> {r.body}
            </div>
          </div>
        );
      if (r.source !== "memory") return null;
      return <MemoryCard r={r} recalls={recalls} />;
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
});

// Calls is a run of tool calls, folded to one line: how many, how many
// failed, and the latest. Open, it shows each call as before.
function Calls({ rs, results, recalls }: { rs: Rec[]; results: { [id: string]: Rec }; recalls: { [id: string]: Retrieval } }) {
  const [open, setOpen] = useState(false);
  const calls = rs.filter((r) => r.kind === "call");
  if (!calls.length) return <>{rs.map((r) => <Record key={r.n} r={r} recalls={recalls} />)}</>;
  const failed = calls.filter((c) => results[c.id]?.exit).length;
  const last = parseCall(calls[calls.length - 1]);
  return (
    <div className="calls">
      <button className="calls-head" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span className="ficon">{open ? "▾" : "▸"}</span>
        <b className="nowrap">
          {calls.length} tool call{calls.length > 1 ? "s" : ""}
        </b>
        {failed > 0 && <span className="exit bad nowrap">{failed} failed</span>}
        {!open && <span className="muted ellipsis mono">{last.head}: {last.gist}</span>}
      </button>
      {open && rs.map((r) => <Record key={r.n} r={r} result={r.kind === "call" ? results[r.id] : undefined} recalls={recalls} />)}
    </div>
  );
}

function parseCall(r: Rec) {
  let name = "call",
    args: any = {};
  try {
    const c = JSON.parse(r.body);
    name = c.name;
    args = c.arguments;
  } catch {
    /* raw */
  }
  if (name === "ask") args = { ...args, questions: undefined, asked: questionsOf(r).map((q) => q.question) };
  return {
    args,
    head: args.worker ? `${name} on ${args.worker}` : name,
    gist: String(args.code ?? args.path ?? args.query ?? args.text ?? args.id ?? args.asked?.[0] ?? "").split("\n")[0],
    memoryTool: ["recall", "remember", "forget"].includes(name),
  };
}

// MemoryCard is what memory recalled for a turn: each item is a line with its
// kind, scope and score, and links to the knowledge view. The recall's own
// record, when it is loaded, adds the scores and what came of it.
function MemoryCard({ r, recalls }: { r: Rec; recalls: { [id: string]: Retrieval } }) {
  const [open, setOpen] = useState(false);
  const items = [...r.body.matchAll(/^- \[(m[0-9a-f]+)\] \(([^,]+), ([^)]+)\) (.*)$/gm)].map((m) => ({ id: m[1], kind: m[2], scope: m[3], text: m[4] }));
  const rv = recalls[r.body.match(/\(retrieval (\w+)\)/)?.[1] ?? ""];
  const hits = new Map((rv?.hits ?? []).map((h) => [h.item.id, h]));
  return (
    <div className="memcard">
      <div className="memhead" onClick={() => setOpen(!open)}>
        <span className="accent">◆</span> Recalled {items.length} {items.length === 1 ? "memory" : "memories"} for this turn
        <span className="muted small"> · {open ? "hide" : "show"}</span>
      </div>
      {open && (
        <>
          <ul className="recalls">
            {items.map((it) => {
              const h = hits.get(it.id);
              return (
                <li key={it.id}>
                  <div>{it.text}</div>
                  <div className="recall-meta small muted">
                    <span className={`kind k-${it.kind}`}>{it.kind}</span>
                    <span>{it.scope}</span>
                    {h && <span title="How well it matched the message">{h.pinned ? "pinned" : `match ${h.score.toFixed(2)}`}</span>}
                    {rv?.reward != null && h && <span className={h.used ? "used" : ""} title="Whether the model's reply carried this memory's words">{h.used ? "used" : "not used"}</span>}
                    <a href={`#/knowledge/${it.id}`}>{it.id}</a>
                    <ItemActions id={it.id} pinned={h?.item.pinned ?? false} />
                  </div>
                </li>
              );
            })}
          </ul>
          {rv && (
            <div className="small muted">
              {rv.k ? `PAI chose to recall ${rv.k}` : "PAI chose to skip"} · {verdict(rv).text}
            </div>
          )}
        </>
      )}
      {rv && <Nudge r={rv} />}
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
  const [orchestrator, setOrchestrator] = useState(false);
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
      // the role keeps the preset's file tools: patch for a model trained on patches
      const patch = !custom.trim() && p!.tools === "patch";
      const tools = orchestrator ? (patch ? "patch-orchestrator" : "orchestrator") : custom.trim() ? "default" : p!.tools;
      const body = custom.trim()
        ? { model: custom.trim(), tools, context_limit: limit, workers: workers.map((name) => ({ name })) }
        : { model: `${p!.provider}/${p!.model}`, tools, context_limit: limit, input: p!.input, params: p!.params, preset, workers: workers.map((name) => ({ name })) };
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
        <div className="field">
          <span>Role</span>
          <label className={`chip ${orchestrator ? "on" : ""}`}>
            <input type="checkbox" checked={orchestrator} onChange={(e) => setOrchestrator(e.target.checked)} />
            Orchestrator: delegates big work to child sessions and reports back
          </label>
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
