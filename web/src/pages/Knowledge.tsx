import { useCallback, useEffect, useMemo, useState, type MouseEvent } from "react";
import { ago, call, type Arm, type Hit, type Item, type Retrieval, type Stats } from "../api";
import { useLive } from "../live";
import { go } from "../App";

// Memory reads like a notebook: what PAI knows as plain lines under their
// topics, the kind a colored dot. Where an item came from, how often it helped
// and the scores behind a recall open on a click or sit in a tooltip.

const KINDS: [string, string][] = [
  ["fact", "fact"],
  ["preference", "preference"],
  ["decision", "decision"],
  ["todo", "todo"],
  ["procedure", "how-to"],
  ["summary", "summary"],
];
const KIND = Object.fromEntries(KINDS);
const SOURCES: Record<string, string> = {
  extract: "Learnt from",
  model: "Saved by the model in",
  compaction: "Summary of",
  consolidation: "Merged",
  person: "Added by you",
};
const HOW =
  "PAI keeps what lasts from your sessions: facts, preferences, decisions, todos and how-tos. " +
  "It refuses secrets, merges what repeats and never deletes: forgotten items stay under Forgotten. " +
  "For each message it recalls what fits, and learns from what the model uses.";
type Tab = "knows" | "activity" | "forgotten";

export default function Knowledge({ focus }: { focus?: string }) {
  const { memoryTick } = useLive();
  const [tab, setTab] = useState<Tab>("knows");
  const [stats, setStats] = useState<Stats | null>(null);
  const [arms, setArms] = useState<Arm[]>([]);
  const [q, setQ] = useState("");
  const [adding, setAdding] = useState(false);
  const [note, setNote] = useState("");
  const [merging, setMerging] = useState(false);
  useEffect(() => {
    call<{ stats: Stats; bandit: Arm[] }>("GET", "/memory/stats").then((r) => {
      setStats(r.stats);
      setArms(r.bandit);
    }, () => {});
  }, [memoryTick]);
  const merge = async () => {
    setMerging(true);
    try {
      const r = await call<{ clusters: number }>("POST", "/memory/consolidate");
      setNote(r.clusters ? `Merged ${r.clusters} group${r.clusters > 1 ? "s" : ""}` : "Nothing to merge");
    } catch (e: any) {
      setNote(String(e.message ?? e));
    } finally {
      setMerging(false);
    }
  };
  const tabs: [Tab, string, number | undefined][] = [
    ["knows", "Knows", stats?.items],
    ["activity", "Activity", stats?.recalls],
    ["forgotten", "Forgotten", stats?.invalid],
  ];
  return (
    <div className="knowledge-page">
      <header className="nb-head">
        <h1>Memory</h1>
        <span className="nb-info" title={HOW}>
          ⓘ
        </span>
        <div className="grow" />
        <input
          className="search nb-search"
          placeholder="Search"
          title="Finds by words and by meaning, the way PAI recalls"
          value={q}
          onChange={(e) => {
            setQ(e.target.value);
            setTab("knows");
          }}
        />
        <button className="ghost small nb-btn" disabled={merging} onClick={merge} title="Merge items that say the same thing">
          {merging ? "Merging…" : "Merge"}
        </button>
        <button
          className="primary small nb-btn"
          onClick={() => {
            setTab("knows");
            setAdding(true);
          }}
          title="Teach PAI something"
        >
          Add
        </button>
      </header>
      <div className="tabs nb-tabs">
        {tabs.map(([t, l, n]) => (
          <button key={t} className={tab === t ? "active" : ""} onClick={() => setTab(t)}>
            {l} <span className="muted">{n ?? ""}</span>
          </button>
        ))}
        <div className="grow" />
        <span className="nb-note muted small" title={note}>
          {note}
        </span>
      </div>
      {tab === "knows" && <Knows q={q} focus={focus} tick={memoryTick} adding={adding} onAdded={() => setAdding(false)} />}
      {tab === "activity" && <Activity tick={memoryTick} stats={stats} arms={arms} />}
      {tab === "forgotten" && <Forgotten tick={memoryTick} />}
    </div>
  );
}

function Knows({ q, focus, tick, adding, onAdded }: { q: string; focus?: string; tick: number; adding: boolean; onAdded: () => void }) {
  const [kind, setKind] = useState("");
  const [hits, setHits] = useState<Hit[]>([]);
  const load = useCallback(() => {
    const p = new URLSearchParams();
    if (q.trim()) p.set("q", q.trim());
    call<Hit[]>("GET", `/memory?${p}`).then(setHits, () => {});
  }, [q]);
  useEffect(() => {
    const t = window.setTimeout(load, q ? 250 : 0);
    return () => window.clearTimeout(t);
  }, [load, tick, q]);
  useEffect(() => {
    if (focus) window.setTimeout(() => document.getElementById(`item-${focus}`)?.scrollIntoView({ block: "center", behavior: "smooth" }), 300);
  }, [focus, hits.length]);

  const shown = hits.filter((h) => !kind || h.item.kind === kind);
  const counts = useMemo(() => {
    const m: Record<string, number> = {};
    for (const h of hits) m[h.item.kind] = (m[h.item.kind] ?? 0) + 1;
    return m;
  }, [hits]);
  const groups = useMemo(() => {
    if (q.trim()) return [["Matches", shown]] as [string, Hit[]][];
    const m = new Map<string, Hit[]>();
    for (const h of shown) if (h.item.kind !== "summary") m.set(h.item.topic || "other", [...(m.get(h.item.topic || "other") ?? []), h]);
    return [...m.entries()].sort((a, b) => (a[0] === "other" ? 1 : b[0] === "other" ? -1 : a[0].localeCompare(b[0])));
  }, [shown, q]);
  const summaries = q.trim() ? [] : shown.filter((h) => h.item.kind === "summary");

  return (
    <div>
      <div className="nb-kinds">
        {KINDS.map(([k, label]) => (
          <button
            key={k}
            className={`nb-kind ${kind === k ? "on" : ""} ${counts[k] ? "" : "none"}`}
            onClick={() => setKind(kind === k ? "" : k)}
            title={kind === k ? "Show every kind" : `Show only ${label}s`}
          >
            <i className={`dot k-${k}`} />
            {label} <span className="muted">{counts[k] ?? 0}</span>
          </button>
        ))}
      </div>
      {adding && <AddItem onDone={() => { onAdded(); load(); }} />}
      {shown.length === 0 && <div className="empty small">{q ? "No match." : "Nothing yet. PAI learns as you work."}</div>}
      {groups.map(([topic, hs]) => (
        <section key={topic} className="nb-topic">
          <h3>{topic}</h3>
          <ul className="nb-list">
            {hs.map((h) => (
              <Line key={h.item.id} hit={h} focus={focus === h.item.id} onChange={load} searched={!!q.trim()} />
            ))}
          </ul>
        </section>
      ))}
      {summaries.length > 0 && (
        <details className="nb-topic nb-summaries">
          <summary>
            <h3>
              Session summaries <span className="muted">{summaries.length}</span>
            </h3>
          </summary>
          <ul className="nb-list">
            {summaries.map((h) => (
              <Line key={h.item.id} hit={h} focus={focus === h.item.id} onChange={load} searched={false} />
            ))}
          </ul>
        </details>
      )}
    </div>
  );
}

// Line is one memory: its text, and on a click where it came from, how much
// it helped, and its versions. Pin, edit and forget show on hover, in a slot
// that is always there, so nothing moves.
function Line({ hit, focus, onChange, searched }: { hit: Hit; focus: boolean; onChange: () => void; searched: boolean }) {
  const it = hit.item;
  const [open, setOpen] = useState(focus);
  useEffect(() => {
    if (focus) setOpen(true);
  }, [focus]);
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(it.text);
  const [lineage, setLineage] = useState<Item[] | null>(null);
  const act = async (fn: () => Promise<unknown>) => {
    try {
      await fn();
      onChange();
    } catch (e: any) {
      alert(String(e.message ?? e));
    }
  };
  useEffect(() => {
    if (!open || !it.merged?.length || lineage) return;
    call<Hit[]>("GET", "/memory?invalid=1&limit=1000").then((all) => setLineage(all.map((h) => h.item).filter((x) => it.merged?.includes(x.id))), () => {});
  }, [open, it.merged, lineage]);
  const project = it.scope.startsWith("project:") ? it.scope.slice(8) : "";
  const why = searched
    ? `Match ${hit.score.toFixed(2)}${hit.lexical ? ` · words #${hit.lexical}` : ""}${hit.vector ? ` · meaning #${hit.vector}` : ""}`
    : undefined;
  const stop = (fn: () => void) => (e: MouseEvent) => {
    e.stopPropagation();
    fn();
  };
  const acts = (
    <>
      <button className="icon-btn" title={it.pinned ? "Unpin" : "Pin: recall in every turn"} onClick={stop(() => act(() => call("PATCH", `/memory/${it.id}`, { pinned: !it.pinned })))}>
        {it.pinned ? "★" : "☆"}
      </button>
      <button className="icon-btn" title="Edit" onClick={stop(() => setEditing(true))}>
        ✎
      </button>
      <button
        className="icon-btn danger"
        title="Forget: no longer true. It stays under Forgotten."
        onClick={stop(() => {
          const reason = prompt("Why is this no longer true?", "no longer true");
          if (reason !== null) void act(() => call("POST", `/memory/${it.id}/invalidate`, { reason }));
        })}
      >
        ✕
      </button>
    </>
  );
  return (
    <li id={`item-${it.id}`} className={`nb-line ${open ? "open" : ""} ${focus ? "focus" : ""}`}>
      <div className="nb-row" onClick={() => !editing && setOpen(!open)} title={why}>
        <i className={`dot k-${it.kind}`} title={KIND[it.kind] ?? it.kind} />
        {editing ? (
          <div className="nb-edit" onClick={(e) => e.stopPropagation()}>
            <textarea autoFocus value={text} onChange={(e) => setText(e.target.value)} />
            <div className="row end">
              <button className="ghost small" onClick={() => setEditing(false)}>
                Cancel
              </button>
              <button className="primary small" onClick={() => act(() => call("PATCH", `/memory/${it.id}`, { text })).then(() => setEditing(false))}>
                Save
              </button>
            </div>
          </div>
        ) : (
          <span className={`nb-text ${it.kind === "summary" ? "summary" : ""}`}>{it.text}</span>
        )}
        {project && <span className="scope">{project}</span>}
        {it.pinned && (
          <span className="nb-pin" title="Pinned: recalled in every turn">
            ★
          </span>
        )}
        <span className="nb-acts">{acts}</span>
        <span className="nb-age muted small">{ago(it.updated)}</span>
      </div>
      {open && !editing && (
        <div className="nb-detail muted small">
          <span>
            {SOURCES[it.source] ?? it.source}
            {it.session && it.source !== "person" && (
              <>
                {" "}
                <a href={`#/sessions/${it.session}`}>{it.session}</a>
              </>
            )}
          </span>
          <span title="Times it was recalled, and how many of those the model used">
            used {it.used} of {it.shown}
          </span>
          <span className="nb-vote">
            <button className="link" title="Recall it more" onClick={() => act(() => call("POST", `/memory/${it.id}/feedback`, { up: true }))}>
              ▲ useful
            </button>
            <button className="link" title="Recall it less" onClick={() => act(() => call("POST", `/memory/${it.id}/feedback`, { up: false }))}>
              ▼ not useful
            </button>
          </span>
          <span className="nb-acts-narrow">{acts}</span>
          {lineage && lineage.length > 0 && (
            <ul className="nb-lineage">
              {lineage.map((o) => (
                <li key={o.id}>
                  <s>{o.text}</s>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </li>
  );
}

function AddItem({ onDone }: { onDone: () => void }) {
  const [text, setText] = useState("");
  const [kind, setKind] = useState("fact");
  const [topic, setTopic] = useState("");
  const [scope, setScope] = useState("global");
  const [err, setErr] = useState("");
  return (
    <div className="card nb-add">
      <textarea autoFocus placeholder="One sentence, e.g. Production deploys go to the VPS named atlas." value={text} onChange={(e) => setText(e.target.value)} />
      <div className="row">
        <select value={kind} onChange={(e) => setKind(e.target.value)}>
          {KINDS.filter(([k]) => k !== "summary").map(([k, label]) => (
            <option key={k} value={k}>
              {label}
            </option>
          ))}
        </select>
        <input placeholder="topic" value={topic} onChange={(e) => setTopic(e.target.value)} />
        <input placeholder="scope" title="global, or project:name" value={scope} onChange={(e) => setScope(e.target.value)} />
        <div className="grow" />
        <button className="ghost small" onClick={onDone}>
          Cancel
        </button>
        <button
          className="primary small"
          disabled={!text.trim()}
          onClick={() => call("POST", "/memory", { text, kind, topic, scope }).then(onDone, (e) => setErr(String(e.message ?? e)))}
        >
          Remember
        </button>
      </div>
      {err && <p className="error">{err}</p>}
    </div>
  );
}

// Activity is what PAI recalled for each message, in plain words; the scores
// sit in tooltips.
function Activity({ tick, stats, arms }: { tick: number; stats: Stats | null; arms: Arm[] }) {
  const [rs, setRs] = useState<Retrieval[]>([]);
  const [open, setOpen] = useState("");
  useEffect(() => {
    call<Retrieval[]>("GET", "/memory/retrievals").then(setRs, () => {});
  }, [tick]);
  if (rs.length === 0) return <div className="empty small">No recalls yet.</div>;
  const best = Math.max(0.0001, ...arms.map((a) => Math.abs(a.reward)));
  return (
    <div>
      {stats && (
        <p className="muted small nb-sum" title="A recalled memory counts as used when its distinctive words show up in the reply">
          The model used {Math.round(stats.used_rate * 100)}% of what PAI recalled.
        </p>
      )}
      <ul className="nb-list">
        {rs.map((r) => {
          const verdict =
            r.reward === undefined || r.reward === null ? ["…", "Waiting for the reply"] : r.reward > 0.3 ? ["helped", ""] : r.reward < 0 ? ["not used", ""] : ["", ""];
          return (
            <li key={r.id} className={`nb-line ${open === r.id ? "open" : ""}`}>
              <div className="nb-row" onClick={() => setOpen(open === r.id ? "" : r.id)} title={r.reward != null ? `Reward ${r.reward.toFixed(2)}` : undefined}>
                <i className={`dot ${r.k ? "k-procedure" : "off"}`} />
                <span className="nb-text">“{r.query.slice(0, 160)}”</span>
                <span className="nb-what small">{r.k ? `recalled ${r.k}` : "skipped"}</span>
                <span className={`nb-verdict small ${verdict[0] === "helped" ? "good" : verdict[0] === "not used" ? "bad" : "muted"}`} title={verdict[1]}>
                  {verdict[0]}
                </span>
                <a className="small nb-sess" href={`#/sessions/${r.session}`} onClick={(e) => e.stopPropagation()}>
                  {r.session}
                </a>
                <span className="nb-age muted small">{ago(r.at)}</span>
              </div>
              {open === r.id && r.hits && r.hits.length > 0 && (
                <ol className="nb-hits small">
                  {r.hits.map((h) => (
                    <li
                      key={h.item.id}
                      title={h.pinned ? "Pinned" : `Match ${h.score.toFixed(2)}${h.lexical ? ` · words #${h.lexical}` : ""}${h.vector ? ` · meaning #${h.vector}` : ""}`}
                    >
                      <i className={`dot k-${h.item.kind}`} />
                      <a
                        href={`#/knowledge/${h.item.id}`}
                        onClick={(e) => {
                          e.preventDefault();
                          go({ page: "knowledge", arg: h.item.id });
                        }}
                      >
                        {h.item.text}
                      </a>
                    </li>
                  ))}
                </ol>
              )}
            </li>
          );
        })}
      </ul>
      {arms.length > 0 && (
        <details className="nb-topic">
          <summary>
            <h3>How recall learns</h3>
          </summary>
          <p className="muted small">For each message PAI picks how much to recall, then scores the choice by what the model used.</p>
          <div className="arms">
            {arms.map((a) => (
              <div key={a.k} className="arm" title={`Average reward ${a.reward.toFixed(2)}, chosen ${a.pulls} times`}>
                <div className="arm-label small">{a.k === 0 ? "skip" : `recall ${a.k}`}</div>
                <div className="arm-bar">
                  <i className={a.reward < 0 ? "neg" : ""} style={{ width: `${(Math.abs(a.reward) / best) * 100}%` }} />
                </div>
                <div className="arm-num small muted">{a.pulls}×</div>
              </div>
            ))}
          </div>
        </details>
      )}
    </div>
  );
}

function Forgotten({ tick }: { tick: number }) {
  const [items, setItems] = useState<Item[]>([]);
  const load = useCallback(() => {
    call<Hit[]>("GET", "/memory?invalid=1&limit=1000").then((hs) => setItems(hs.map((h) => h.item).filter((i) => i.invalid)), () => {});
  }, []);
  useEffect(load, [load, tick]);
  if (items.length === 0) return <div className="empty small">Nothing forgotten.</div>;
  return (
    <ul className="nb-list">
      {items.map((it) => {
        const into = it.reason?.match(/(?:merged into|superseded by summary) (m[0-9A-Za-z]+)/)?.[1];
        const reason = into ? it.reason?.split(" m")[0] : it.reason;
        return (
          <li key={it.id} className="nb-line">
            <div className="nb-row">
              <i className={`dot k-${it.kind}`} title={KIND[it.kind] ?? it.kind} />
              <span className="nb-text">
                <s>{it.text}</s>
              </span>
              <span className="nb-what small muted">
                {into ? (
                  <a href={`#/knowledge/${into}`} title="See what replaced it">
                    {reason}
                  </a>
                ) : (
                  reason
                )}
              </span>
              <span className="nb-age muted small">{ago(it.invalid)}</span>
              <button className="ghost small nb-restore" onClick={() => call("POST", `/memory/${it.id}/restore`).then(load)} title="Make it recallable again">
                Restore
              </button>
            </div>
          </li>
        );
      })}
    </ul>
  );
}
