import { useCallback, useEffect, useMemo, useState } from "react";
import { ago, call, type Arm, type Hit, type Item, type Retrieval, type Stats } from "../api";
import { useLive } from "../live";
import { go } from "../App";

const KINDS = ["fact", "preference", "decision", "todo", "procedure", "summary"];
const SOURCES: Record<string, string> = {
  extract: "learnt from a session",
  model: "saved by the model",
  compaction: "a compaction summary",
  consolidation: "merged by consolidation",
  person: "added by you",
};
type Tab = "knows" | "recalls" | "history";

export default function Knowledge({ focus }: { focus?: string }) {
  const { memoryTick } = useLive();
  const [tab, setTab] = useState<Tab>("knows");
  const [stats, setStats] = useState<Stats | null>(null);
  const [arms, setArms] = useState<Arm[]>([]);
  useEffect(() => {
    call<{ stats: Stats; bandit: Arm[] }>("GET", "/memory/stats").then((r) => {
      setStats(r.stats);
      setArms(r.bandit);
    }, () => {});
  }, [memoryTick]);
  return (
    <div className="knowledge-page">
      <header className="page-head">
        <div>
          <h1>Memory</h1>
          <p className="muted">What PAI has learnt from your sessions, on every model and machine. It keeps what lasts, merges what repeats, and never deletes.</p>
        </div>
      </header>
      <div className="stat-row">
        <StatCard label="Things PAI knows" value={stats?.items ?? "—"} sub={stats?.pinned ? `${stats.pinned} pinned` : "across every session"} />
        <StatCard label="Helpful when recalled" value={stats ? `${Math.round(stats.used_rate * 100)}%` : "—"} sub={stats ? `${stats.recalls} recalls so far` : ""} />
        <StatCard label="Merged or retired" value={stats?.invalid ?? "—"} sub="kept in History" />
      </div>
      <div className="tabs">
        {(
          [
            ["knows", "What PAI knows"],
            ["recalls", "Recalls"],
            ["history", "History"],
          ] as [Tab, string][]
        ).map(([t, l]) => (
          <button key={t} className={tab === t ? "active" : ""} onClick={() => setTab(t)}>
            {l}
          </button>
        ))}
      </div>
      {tab === "knows" && <Knows focus={focus} tick={memoryTick} />}
      {tab === "recalls" && (
        <>
          <Recalls tick={memoryTick} />
          <details className="more">
            <summary>How PAI decides what to keep and recall</summary>
            <Learning arms={arms} stats={stats} />
          </details>
        </>
      )}
      {tab === "history" && <History tick={memoryTick} />}
    </div>
  );
}

function StatCard({ label, value, sub }: { label: string; value: string | number; sub?: string }) {
  return (
    <div className="stat">
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value}</div>
      {sub && <div className="stat-sub">{sub}</div>}
    </div>
  );
}

function Knows({ focus, tick }: { focus?: string; tick: number }) {
  const [q, setQ] = useState("");
  const [kind, setKind] = useState("");
  const [hits, setHits] = useState<Hit[]>([]);
  const [adding, setAdding] = useState(false);
  const [busy, setBusy] = useState("");
  const [note, setNote] = useState("");
  const load = useCallback(() => {
    const p = new URLSearchParams();
    if (q.trim()) p.set("q", q.trim());
    if (kind) p.set("kind", kind);
    call<Hit[]>("GET", `/memory?${p}`).then(setHits, () => {});
  }, [q, kind]);
  useEffect(() => {
    const t = window.setTimeout(load, q ? 250 : 0);
    return () => window.clearTimeout(t);
  }, [load, tick, q]);
  useEffect(() => {
    if (focus) window.setTimeout(() => document.getElementById(`item-${focus}`)?.scrollIntoView({ block: "center", behavior: "smooth" }), 300);
  }, [focus, hits.length]);

  const shown = hits.filter((h) => !kind || h.item.kind === kind);
  const groups = useMemo(() => {
    if (q.trim()) return [["Best matches", shown]] as [string, Hit[]][];
    const m = new Map<string, Hit[]>();
    for (const h of shown) {
      const t = h.item.kind === "summary" ? "session summaries" : h.item.topic || "other";
      m.set(t, [...(m.get(t) ?? []), h]);
    }
    return [...m.entries()].sort((a, b) => (a[0] === "session summaries" ? 1 : b[0] === "session summaries" ? -1 : b[1].length - a[1].length || a[0].localeCompare(b[0])));
  }, [shown, q]);

  const consolidate = async () => {
    setBusy("consolidate");
    setNote("");
    try {
      const r = await call<{ clusters: number }>("POST", "/memory/consolidate");
      setNote(r.clusters ? `Merged ${r.clusters} group${r.clusters > 1 ? "s" : ""} of related items.` : "Nothing to merge: every subject is said once.");
      load();
    } catch (e: any) {
      setNote(String(e.message ?? e));
    } finally {
      setBusy("");
    }
  };

  return (
    <div>
      <div className="toolbar">
        <input className="search" placeholder="Search memory the way PAI recalls it: words and meaning" value={q} onChange={(e) => setQ(e.target.value)} />
        <div className="chips">
          <button className={`chip ${kind === "" ? "on" : ""}`} onClick={() => setKind("")}>
            all
          </button>
          {KINDS.map((k) => (
            <button key={k} className={`chip k-${k} ${kind === k ? "on" : ""}`} onClick={() => setKind(kind === k ? "" : k)}>
              {k}
            </button>
          ))}
        </div>
        <div className="grow" />
        <button className="ghost small" disabled={busy !== ""} onClick={consolidate} title="Merge items that say the same or are about one subject">
          {busy ? "Merging…" : "Consolidate"}
        </button>
        <button className="primary small" onClick={() => setAdding(true)}>
          Add memory
        </button>
      </div>
      {note && <div className="banner">{note}</div>}
      {adding && <AddItem onDone={() => { setAdding(false); load(); }} />}
      {shown.length === 0 && (
        <div className="empty small">
          {q ? "Nothing matches." : "PAI has not learnt anything yet. It learns as you work: from what sessions decide and discover, and from what you tell it to remember."}
        </div>
      )}
      {groups.map(([topic, hs]) => (
        <section key={topic} className="topic">
          <h3>
            {topic} <span className="muted small">{hs.length}</span>
          </h3>
          <div className="items">
            {hs.map((h) => (
              <ItemCard key={h.item.id} hit={h} focus={focus === h.item.id} onChange={load} searched={!!q.trim()} />
            ))}
          </div>
        </section>
      ))}
    </div>
  );
}

function ItemCard({ hit, focus, onChange, searched }: { hit: Hit; focus: boolean; onChange: () => void; searched: boolean }) {
  const it = hit.item;
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
  const showLineage = async () => {
    if (lineage) return setLineage(null);
    const all = await call<Hit[]>("GET", "/memory?invalid=1&limit=1000");
    setLineage(all.map((h) => h.item).filter((x) => it.merged?.includes(x.id)));
  };
  return (
    <article id={`item-${it.id}`} className={`item ${focus ? "focus" : ""} ${it.pinned ? "pinned" : ""}`}>
      <div className="item-top">
        <span className={`kind k-${it.kind}`}>{it.kind}</span>
        {it.scope !== "global" && <span className="scope">{it.scope}</span>}
        {it.pinned && <span className="scope accent">pinned</span>}
        <div className="grow" />
        <span className="muted small">{ago(it.updated)}</span>
      </div>
      {editing ? (
        <div>
          <textarea value={text} onChange={(e) => setText(e.target.value)} />
          <div className="row end">
            <button className="ghost small" onClick={() => setEditing(false)}>
              Cancel
            </button>
            <button className="primary small" onClick={() => act(() => call("PATCH", `/memory/${it.id}`, { text })).then(() => setEditing(false))}>
              Save as new version
            </button>
          </div>
        </div>
      ) : (
        <p className={`item-text ${it.kind === "summary" ? "summary" : ""}`}>{it.text}</p>
      )}
      <div className="item-meta muted small">
        {SOURCES[it.source] ?? it.source}
        {it.session && (
          <>
            {" in "}
            <a href={`#/sessions/${it.session}`}>session {it.session}</a>
            {it.from ? ` (records ${it.from}${it.to && it.to !== it.from ? `–${it.to}` : ""})` : ""}
          </>
        )}
        {it.merged?.length ? (
          <>
            {" · "}
            <button className="link" onClick={showLineage}>
              merged from {it.merged.length} item{it.merged.length > 1 ? "s" : ""}
            </button>
          </>
        ) : null}
      </div>
      {lineage && (
        <ul className="lineage">
          {lineage.map((o) => (
            <li key={o.id}>
              <s>{o.text}</s> <span className="muted small">{SOURCES[o.source] ?? o.source}</span>
            </li>
          ))}
        </ul>
      )}
      <div className="item-bottom">
        <div className="utility" title="How often this item helped when it was recalled. It rises when the model uses it, falls when it does not.">
          <div className="meter thin">
            <i style={{ width: `${it.utility * 100}%` }} />
          </div>
          <span className="small muted">
            worth {Math.round(it.utility * 100)}% · used {it.used} of {it.shown} recalls
          </span>
        </div>
        {searched && (
          <span className="small muted why" title="Place in each list, then the fused score">
            score {hit.score.toFixed(2)}
            {hit.lexical ? ` · words #${hit.lexical}` : ""}
            {hit.vector ? ` · meaning #${hit.vector} (${hit.cosine.toFixed(2)})` : ""}
          </span>
        )}
        <div className="actions">
          <button className="icon-btn" title="Useful: recall it more" onClick={() => act(() => call("POST", `/memory/${it.id}/feedback`, { up: true }))}>
            ▲
          </button>
          <button className="icon-btn" title="Not useful: recall it less" onClick={() => act(() => call("POST", `/memory/${it.id}/feedback`, { up: false }))}>
            ▼
          </button>
          <button className="icon-btn" title={it.pinned ? "Unpin" : "Pin: recall in every turn"} onClick={() => act(() => call("PATCH", `/memory/${it.id}`, { pinned: !it.pinned }))}>
            {it.pinned ? "★" : "☆"}
          </button>
          <button className="icon-btn" title="Edit" onClick={() => setEditing(true)}>
            ✎
          </button>
          <button
            className="icon-btn danger"
            title="Retire: no longer true. It stays in History."
            onClick={() => {
              const reason = prompt("Why is this no longer true?", "no longer true");
              if (reason !== null) void act(() => call("POST", `/memory/${it.id}/invalidate`, { reason }));
            }}
          >
            ✕
          </button>
        </div>
      </div>
    </article>
  );
}

function AddItem({ onDone }: { onDone: () => void }) {
  const [text, setText] = useState("");
  const [kind, setKind] = useState("fact");
  const [topic, setTopic] = useState("");
  const [scope, setScope] = useState("global");
  const [err, setErr] = useState("");
  return (
    <div className="card add">
      <h3>Teach PAI something</h3>
      <textarea autoFocus placeholder="One self-contained sentence, such as: Production deploys go to the VPS named atlas through GitHub Actions." value={text} onChange={(e) => setText(e.target.value)} />
      <div className="row">
        <select value={kind} onChange={(e) => setKind(e.target.value)}>
          {KINDS.filter((k) => k !== "summary").map((k) => (
            <option key={k}>{k}</option>
          ))}
        </select>
        <input placeholder="topic, such as deploy" value={topic} onChange={(e) => setTopic(e.target.value)} />
        <input placeholder="scope: global or project:name" value={scope} onChange={(e) => setScope(e.target.value)} />
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

function Recalls({ tick }: { tick: number }) {
  const [rs, setRs] = useState<Retrieval[]>([]);
  useEffect(() => {
    call<Retrieval[]>("GET", "/memory/retrievals").then(setRs, () => {});
  }, [tick]);
  if (rs.length === 0) return <div className="empty small">No recalls yet. PAI recalls when you send a message and memory holds something.</div>;
  return (
    <div className="recalls">
      <p className="muted small">Every time you send a message, PAI decides whether to recall and how much, then checks whether the model used what it got. That check is the reward the policy learns from.</p>
      {rs.map((r) => (
        <article key={r.id} className="recall">
          <div className="item-top">
            <span className={`pill ${r.k ? "open" : "closed"}`}>{r.k ? `recall ${r.k}` : "skipped"}</span>
            <b className="ellipsis">“{r.query.slice(0, 140)}”</b>
            <div className="grow" />
            {r.reward !== undefined && r.reward !== null ? (
              <span className={`reward ${r.reward > 0.3 ? "good" : r.reward < 0 ? "bad" : ""}`}>reward {r.reward.toFixed(2)}</span>
            ) : (
              <span className="muted small">waiting for the reply</span>
            )}
            <a className="small" href={`#/sessions/${r.session}`}>
              {r.session}
            </a>
            <span className="muted small">{ago(r.at)}</span>
          </div>
          {r.hits && r.hits.length > 0 && (
            <ol className="hits">
              {r.hits.map((h) => (
                <li key={h.item.id}>
                  <span className={`kind k-${h.item.kind}`}>{h.item.kind}</span> {h.item.text}{" "}
                  <span className="muted small">
                    {h.pinned ? "pinned" : `score ${h.score.toFixed(2)}${h.lexical ? ` · words #${h.lexical}` : ""}${h.vector ? ` · meaning #${h.vector}` : ""}`}
                  </span>{" "}
                  <a className="small" href={`#/knowledge/${h.item.id}`} onClick={(e) => { e.preventDefault(); go({ page: "knowledge", arg: h.item.id }); }}>
                    {h.item.id}
                  </a>
                </li>
              ))}
            </ol>
          )}
        </article>
      ))}
    </div>
  );
}

function Learning({ arms, stats }: { arms: Arm[]; stats: Stats | null }) {
  const best = Math.max(0.0001, ...arms.map((a) => Math.abs(a.reward)));
  return (
    <div className="learning">
      <div className="card">
        <h3>Recall policy</h3>
        <p className="muted small">
          A contextual bandit (LinUCB) picks, for each message, how many memories to recall: none, 3, 6 or 10. It looks at whether the message asks
          something, how long it is, how full the context is and how much memory holds. After the model replies, recalled items whose distinctive
          words show up in the reply count as used: the share used, less a small cost per item, is the reward. A turn where the model had to call
          recall itself is penalized. Over time PAI learns to skip memory on small talk and to recall more for real questions.
        </p>
        <div className="arms">
          {arms.map((a) => (
            <div key={a.k} className="arm">
              <div className="arm-label">{a.k === 0 ? "skip" : `recall ${a.k}`}</div>
              <div className="arm-bar">
                <i className={a.reward < 0 ? "neg" : ""} style={{ width: `${(Math.abs(a.reward) / best) * 100}%` }} />
              </div>
              <div className="arm-num small">
                {a.reward.toFixed(2)} <span className="muted">· chosen {a.pulls}×</span>
              </div>
            </div>
          ))}
        </div>
      </div>
      <div className="card">
        <h3>What gets kept</h3>
        <ul className="muted small bullets">
          <li>Only lasting items: facts about you, your machines and projects, preferences, decisions and why, todos, and procedures that worked.</li>
          <li>Never chatter, passing steps, guesses or secrets: anything that looks like a key or password is refused before it is stored.</li>
          <li>Each new item is checked against related ones, by topic and meaning. The model then adds it, merges it into one complete statement, updates an older item, retires a contradicted one, or skips it.</li>
          <li>A background pass merges what memory says more than once. Merged and edited items are new versions that name what they replaced.</li>
          <li>Sessions compact at 70% of their context: a summary replaces the oldest records in the model's context, and the full history stays in the session file.</li>
        </ul>
      </div>
      {stats && (
        <div className="card">
          <h3>By scope</h3>
          <ul className="muted small bullets">
            {Object.entries(stats.by_scope).map(([s, n]) => (
              <li key={s}>
                {s}: {n}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

function History({ tick }: { tick: number }) {
  const [items, setItems] = useState<Item[]>([]);
  const load = useCallback(() => {
    call<Hit[]>("GET", "/memory?invalid=1&limit=1000").then((hs) => setItems(hs.map((h) => h.item).filter((i) => i.invalid)), () => {});
  }, []);
  useEffect(load, [load, tick]);
  if (items.length === 0) return <div className="empty small">Nothing retired yet.</div>;
  return (
    <div className="items">
      <p className="muted small">Items that were merged, updated, superseded or retired. Nothing is deleted; restore one to make it recallable again.</p>
      {items.map((it) => {
        const into = it.reason?.match(/(?:merged into|superseded by summary) (m[0-9a-f]+)/)?.[1];
        return (
          <article key={it.id} className="item retired">
            <div className="item-top">
              <span className={`kind k-${it.kind}`}>{it.kind}</span>
              {it.topic && <span className="scope">{it.topic}</span>}
              <div className="grow" />
              <span className="muted small">{ago(it.invalid)}</span>
            </div>
            <p className="item-text">
              <s>{it.text}</s>
            </p>
            <div className="item-bottom">
              <span className="muted small">
                {into ? (
                  <>
                    {it.reason?.split(" m")[0]} <a href={`#/knowledge/${into}`}>{into}</a>
                  </>
                ) : (
                  it.reason
                )}
              </span>
              <div className="grow" />
              <button className="ghost small" onClick={() => call("POST", `/memory/${it.id}/restore`).then(load)}>
                Restore
              </button>
            </div>
          </article>
        );
      })}
    </div>
  );
}
