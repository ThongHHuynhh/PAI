import { useEffect, useState } from "react";
import { ago, call, labelled, type Arm, type Hit, type Metrics, type Retrieval, type Stats } from "../api";
import { useLive } from "../live";

// A recall counts as helped above this reward, the same line the Memory page's
// Activity draws.
const HELPED = 0.3;
// The bar lists this many of the newest recalls; the Memory page has the rest.
const SHOWN = 6;
// The bar warns when PAI skipped memory for this many newest messages in a row.
const SKIP_WARN = 3;
// A usage window is flagged when this much of it is spent.
const LIMIT_WARN = 0.8;
// A settled recall asks for a word on its used memory about one time in this many.
const NUDGE_ONE_IN = 3;
// Names of the provider's usage windows.
const WINDOWS: Record<string, string> = { five_hour: "5-hour limit", seven_day: "7-day limit" };

// useRetrievals reads a session's automatic recalls, newest first, and reads
// them again when memory recalls or scores one for the session.
export function useRetrievals(session: string): Retrieval[] {
  const { subscribe } = useLive();
  const [rs, setRs] = useState<Retrieval[]>([]);
  useEffect(() => {
    const get = () => call<Retrieval[]>("GET", `/memory/retrievals?session=${session}`).then(setRs, () => {});
    get();
    return subscribe((e) => {
      if (e.type === "memory" && e.session === session && (e.data?.type === "recalled" || e.data?.type === "settled")) get();
    });
  }, [session, subscribe]);
  return rs;
}

// verdict is what came of a recall: waiting for the reply, or how much of it
// the model used.
export function verdict(r: Retrieval): { text: string; tone: "good" | "bad" | "muted" } {
  if (r.reward === undefined || r.reward === null) return { text: "waiting for the reply", tone: "muted" };
  if (r.reward > HELPED) return { text: "helped", tone: "good" };
  if (r.reward < 0) return { text: "not used", tone: "bad" };
  return { text: "little used", tone: "muted" };
}

const pc = (v: number) => `${Math.round(v * 100)}%`;
const secs = (ms: number) => `${(ms / 1000).toFixed(1)} s`;

function until(iso: string): string {
  const m = Math.max(0, Math.round((new Date(iso).getTime() - Date.now()) / 60000));
  return m >= 1440 ? `${Math.floor(m / 1440)}d ${Math.floor((m % 1440) / 60)}h` : m >= 60 ? `${Math.floor(m / 60)}h ${m % 60}m` : `${m}m`;
}

// ItemActions are the small controls on a recalled memory: recall it more or
// less, and pin it so it is recalled in every turn (the star of the Memory page).
export function ItemActions({ id, pinned }: { id: string; pinned: boolean }) {
  const [star, setStar] = useState(pinned);
  const [voted, setVoted] = useState("");
  const vote = (up: boolean) => call("POST", `/memory/${id}/feedback`, { up }).then(() => setVoted(up ? "more" : "less"), () => {});
  const pin = () => call("PATCH", `/memory/${id}`, { pinned: !star }).then(() => setStar(!star), () => {});
  return (
    <span className="item-actions">
      <button className={`link ${voted === "more" ? "on" : ""}`} disabled={!!voted} title="Recall it more" onClick={() => vote(true)}>
        more
      </button>
      <button className={`link ${voted === "less" ? "on" : ""}`} disabled={!!voted} title="Recall it less" onClick={() => vote(false)}>
        less
      </button>
      <button className="icon-btn" title={star ? "Unpin" : "Pin: recall in every turn"} onClick={pin}>
        {star ? "★" : "☆"}
      </button>
    </span>
  );
}

const nudgeKey = (id: string) => `pai.nudge.${id}`;

// Nudge is a quiet line under a recall card: now and then, once the model has
// used a memory, it asks if that was right. One word is enough; it is asked once.
export function Nudge({ r }: { r: Retrieval }) {
  const [gone, setGone] = useState(() => localStorage.getItem(nudgeKey(r.id)) !== null);
  const [said, setSaid] = useState("");
  const used: Hit | undefined = r.hits?.find((h) => h.used && !h.pinned);
  if (gone || !used || parseInt(r.id.slice(-3), 16) % NUDGE_ONE_IN !== 0) return null;
  const end = () => (localStorage.setItem(nudgeKey(r.id), "1"), setGone(true));
  const vote = (up: boolean) =>
    call("POST", `/memory/${used.item.id}/feedback`, { up }).then(() => (setSaid(up ? "Noted, it will come up more." : "Noted, it will come up less."), setTimeout(end, 2500)), () => {});
  return (
    <div className="nudge small muted">
      {said || (
        <>
          <span className="nudge-text">Was “{used.item.text.slice(0, 70)}{used.item.text.length > 70 ? "…" : ""}” the right thing to recall?</span>
          <button className="link" onClick={() => vote(true)}>yes</button>
          <button className="link" onClick={() => vote(false)}>no</button>
          <button className="link" title="Do not ask" onClick={end}>×</button>
        </>
      )}
    </div>
  );
}

// RetrievalBar is the note above the input: one line about this chat's
// recalls that opens, upward, into how retrieval performed.
export function RetrievalBar({ session, rs }: { session: string; rs: Retrieval[] }) {
  const { memoryTick, labels } = useLive();
  const [open, setOpen] = useState(false);
  const [seen, setSeen] = useState("");
  const [stats, setStats] = useState<{ stats: Stats; bandit: Arm[] } | null>(null);
  const [m, setM] = useState<Metrics | null>(null);
  useEffect(() => {
    if (open) call<{ stats: Stats; bandit: Arm[] }>("GET", "/memory/stats").then(setStats, () => {});
  }, [open, memoryTick]);
  // /metrics reads every session file, so it is read when the panel opens and
  // when a new recall comes or is scored, not on a timer.
  useEffect(() => {
    if (open) call<Metrics>("GET", "/metrics?range=all").then(setM, () => {});
  }, [open, rs[0]?.id, rs[0]?.reward]);

  const recalled = rs.filter((r) => r.k > 0);
  const memories = rs.reduce((n, r) => n + (r.hits?.length ?? 0), 0);
  const scored = rs.filter((r) => r.reward !== undefined && r.reward !== null);
  const helped = scored.filter((r) => (r.reward ?? 0) > HELPED).length;
  const reward = scored.length ? scored.reduce((n, r) => n + (r.reward ?? 0), 0) / scored.length : undefined;
  const best = Math.max(0.0001, ...(stats?.bandit ?? []).map((a) => Math.abs(a.reward)));
  const newestKept = rs.findIndex((r) => r.k > 0);
  const skipRun = newestKept === -1 ? rs.length : newestKept;
  const me = m?.sessions.find((s) => s.id === session);
  const prompt = me ? me.usage.input + (me.usage.cache_read ?? 0) + (me.usage.cache_write ?? 0) : 0;
  const lim = Object.entries(m?.limits ?? {}).flatMap(([prov, l]) => Object.entries(l.windows).map(([w, v]) => ({ prov, w, ...v, status: l.status })));
  const hot = lim.some((l) => l.utilization >= LIMIT_WARN);

  return (
    <div className="retrieval">
      {open && (
        <div className="retrieval-panel">
          <div className="tiles">
            <Tile k="Recalls" v={recalled.length} d={`of ${rs.length} messages`} tip="Messages PAI recalled memory for in this chat; it skips the rest, such as small talk" />
            <Tile k="Memories" v={memories} d="brought in" tip="Memories placed in front of the model in this chat, pinned ones included" />
            <Tile k="Helped" v={scored.length ? pc(helped / scored.length) : "—"} d={scored.length ? `${helped} of ${scored.length} scored` : ""} tip="Recalls whose memories the model used in its reply" />
            <Tile k="Reward" v={reward === undefined ? "—" : reward.toFixed(2)} d="mean in this chat" tip="What was used, less a cost per item. Higher is better" />
            <Tile k="Used, all chats" v={stats ? pc(stats.stats.used_rate) : "—"} d={stats ? `${stats.stats.recalls} scored recalls` : ""} tip="Share of recalled memories the model used in its reply, in every chat" />
          </div>
          {skipRun >= SKIP_WARN && (
            <p className="warn small">
              PAI skipped memory for the last {skipRun} messages. That is normal for small talk. If you asked about something it should know, pin that memory with the star.
            </p>
          )}
          <div className="tiles">
            <Tile k="Cost, this chat" v={me && m?.priced ? `$${me.cost.toFixed(2)}` : "—"} d={me ? `${Math.round(prompt / 1000)}k prompt tokens` : ""} tip="What an API account would have charged for this chat. A subscription does not bill per token" />
            <Tile k="Cache hit" v={prompt ? pc((me?.usage.cache_read ?? 0) / prompt) : "—"} d="of the prompt" tip="Share of the prompt read from the provider's cache. Higher is cheaper and faster" />
            <Tile k="Recall time" v={m?.memory ? `${m.memory.recall_p50_ms} ms` : "—"} d={m?.memory ? `p95 ${m.memory.recall_p95_ms} ms` : ""} tip="How long memory took to find items for a message, in every chat, embedding included" />
            <Tile k="Reply time" v={m ? secs(m.totals.p50_ms) : "—"} d={m ? `p95 ${secs(m.totals.p95_ms)}` : ""} tip="How long the model took per step, in every chat" />
            {lim.length === 0 ? (
              <Tile k="Provider limit" v="—" d="shown after a reply" tip="The provider reports its usage windows with a reply, so there is nothing until one has come since the server started" />
            ) : (
              lim.map((l) => (
                <Tile key={l.prov + l.w} warn={l.utilization >= LIMIT_WARN} k={WINDOWS[l.w] ?? l.w} v={`${pc(1 - l.utilization)} left`} d={`resets in ${until(l.resets)}`} tip={`${labelled(labels, l.prov)}: ${pc(l.utilization)} spent, status ${l.status}`} />
              ))
            )}
          </div>
          {rs.length === 0 ? (
            <p className="muted small">Nothing recalled in this chat yet.</p>
          ) : (
            <ul className="nb-list">
              {rs.slice(0, SHOWN).map((r) => {
                const v = verdict(r);
                return (
                  <li key={r.id} className={`nb-line ${seen === r.id ? "open" : ""}`}>
                    <div className="nb-row" onClick={() => setSeen(seen === r.id ? "" : r.id)} title={r.reward != null ? `Reward ${r.reward.toFixed(2)}` : undefined}>
                      <i className={`dot ${r.k ? "k-procedure" : "off"}`} />
                      <span className="nb-text">“{r.query.slice(0, 160)}”</span>
                      <span className="nb-what small">{r.k ? `recalled ${r.hits?.length ?? r.k}` : "skipped"}{r.ms ? ` · ${r.ms} ms` : ""}</span>
                      <span className={`nb-verdict small ${v.tone}`}>{!r.k ? "" : v.text === "waiting for the reply" ? "…" : v.text}</span>
                      <span className="nb-age muted small">{ago(r.at)}</span>
                    </div>
                    {seen === r.id && r.hits && r.hits.length > 0 && (
                      <ol className="nb-hits small">
                        {r.hits.map((h) => (
                          <li key={h.item.id} title={h.pinned ? "Pinned" : `Match ${h.score.toFixed(2)}`}>
                            <i className={`dot k-${h.item.kind}`} />
                            <a href={`#/knowledge/${h.item.id}`}>{h.item.text}</a>
                            {h.used && <span className="used small"> used</span>}
                          </li>
                        ))}
                      </ol>
                    )}
                  </li>
                );
              })}
            </ul>
          )}
          {stats && stats.bandit.length > 0 && (
            <details className="nb-topic">
              <summary className="small">How many to recall: what the policy learnt</summary>
              <div className="arms">
                {stats.bandit.map((a) => (
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
          <p className="small muted retrieval-links">
            <a href="#/knowledge">All recalls on the Memory page</a> · <a href="#/performance">Performance</a>
          </p>
        </div>
      )}
      <button className="retrieval-bar" onClick={() => setOpen(!open)} aria-expanded={open} title={`Retrieval in this chat (${session})`}>
        <span className="accent">↺</span>
        <b>Retrieval</b>
        {(skipRun >= SKIP_WARN || hot) && <span className="warn" title={hot ? "A provider usage window is nearly spent" : "PAI skipped memory for the last messages"}>⚠</span>}
        <span className="muted">
          {rs.length === 0
            ? "nothing recalled yet"
            : `${recalled.length} ${recalled.length === 1 ? "recall" : "recalls"} · ${memories} ${memories === 1 ? "memory" : "memories"}${scored.length ? ` · ${pc(helped / scored.length)} helped` : ""}`}
        </span>
        <span className="muted retrieval-caret">{open ? "▾" : "▴"}</span>
      </button>
    </div>
  );
}

function Tile({ k, v, d, tip, warn }: { k: string; v: string | number; d?: string; tip?: string; warn?: boolean }) {
  return (
    <div className={`tile ${warn ? "warn" : ""}`} title={tip}>
      <div className="tile-k">{k}</div>
      <div className="tile-v">{v}</div>
      <div className="tile-d muted">{d ?? " "}</div>
    </div>
  );
}
