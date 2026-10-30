import { useEffect, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import { ago, call, sessionTitle, tokens, type Metrics, type MetricsRange, type Session, type Usage } from "../api";
import { useLive } from "../live";
import { go } from "../App";

// Performance is how PAI is doing over a range: the models it steps, the
// sessions and their context, the tools the harness runs, and what memory
// collects and recalls. Every number comes from GET /metrics, which reads the
// session files; explanations sit in tooltips.

const RANGES: [MetricsRange, string][] = [
  ["24h", "24h"],
  ["7d", "7 days"],
  ["30d", "30 days"],
  ["all", "All"],
];
const KINDS: [string, string][] = [
  ["fact", "fact"],
  ["preference", "preference"],
  ["decision", "decision"],
  ["todo", "todo"],
  ["procedure", "how-to"],
  ["summary", "summary"],
];
const SOURCES: Record<string, string> = { extract: "learnt from sessions", model: "saved by the model", compaction: "summaries", consolidation: "merges", person: "added by you" };
const NO_PRICES = "This build has no model prices. Release builds do: costs then show what the API would charge.";

// REFRESH is how often the page reads /metrics again while it is open.
const REFRESH_MS = 30_000;

export default function Performance() {
  const { memoryTick, feed } = useLive();
  const [range, setRange] = useState<MetricsRange>("24h");
  const [m, setM] = useState<Metrics | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const steps = feed.filter((e) => e.type === "record" && e.data?.kind === "step").length;
  useEffect(() => {
    let alive = true;
    const load = () => {
      setBusy(true);
      call<Metrics>("GET", `/metrics?range=${range}`)
        .then((r) => alive && (setM(r), setErr("")), (e) => alive && setErr(String(e.message ?? e)))
        .finally(() => alive && setBusy(false));
    };
    load();
    const t = window.setInterval(load, REFRESH_MS);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, [range, memoryTick, steps]);

  return (
    <div className="perf-page">
      <header className="nb-head">
        <h1>Performance</h1>
        <span className="nb-info" title="How the models, sessions, tools and memory performed in the range. Read from the session files; updates live.">
          ⓘ
        </span>
        <span className={`perf-busy muted small ${busy ? "on" : ""}`}>updating…</span>
        <div className="grow" />
        <div className="seg" role="group" aria-label="Range">
          {RANGES.map(([r, l]) => (
            <button key={r} className={range === r ? "on" : ""} onClick={() => setRange(r)}>
              {l}
            </button>
          ))}
        </div>
      </header>
      {err && <div className="banner error">{err}</div>}
      {!m ? <div className="empty small">Loading…</div> : <Body m={m} />}
    </div>
  );
}

function Body({ m }: { m: Metrics }) {
  const t = m.totals;
  const mem = m.memory;
  const fails = m.tools.reduce((a, x) => a + x.failures, 0);
  const asked = mem ? mem.recalls + mem.skipped : 0;
  return (
    <>
      <section className="perf-sec">
        <h2>Models</h2>
        <div className="tiles">
          <Tile k="Calls" v={t.steps} tip="Requests to a model: one per step of the agent loop" />
          <Tile k="Speed" v={ms(t.p50_ms)} d={`slowest 5%: ${ms(t.p95_ms)}`} tip="Median time from sending a request to the full reply" />
          <Tile k="Tokens" v={tokens(prompt(t.usage))} d={`in · ${tokens(t.usage.output)} out`} tip="Prompt tokens read, cached ones included, and tokens written" />
          <Tile k="Cache" v={pc(t.cache_hit)} tip="Share of prompt tokens served from the provider's cache: cheaper and faster" />
          <Tile k="Est. cost" v={m.priced ? usd(t.cost) : "—"} tip={m.priced ? "At API list prices; a subscription shows what the API would have charged" : NO_PRICES} />
          <Tile k="Errors" v={t.errors} tone={t.errors ? "bad" : ""} tip="Steps that failed: provider errors, limits, stops" />
        </div>
        <div className="charts">
          <Chart title="Calls" m={m} get={(p) => p.steps} fmt={String} kind="bars" />
          <Chart title="Tokens" m={m} get={(p) => p.tokens} fmt={tokens} kind="bars" />
          <Chart title="Median speed" m={m} get={(p) => (p.steps ? p.p50_ms : null)} fmt={ms} kind="line" />
        </div>
        <Table
          head={["Model", "Calls", "Speed", "Slowest 5%", "In", "Out", "Cache", "Est. cost", "Errors", "Stops"]}
          tips={[, , "Median time to the full reply", , "Prompt tokens, cached ones included", , "Share of the prompt served from cache", NO_PRICES]}
          empty="No model calls in this range."
          rows={m.models.map((x) => [
            <b key="m">{x.model}</b>,
            x.steps,
            ms(x.p50_ms),
            ms(x.p95_ms),
            tokens(prompt(x.usage)),
            tokens(x.usage.output),
            pc(x.cache_hit),
            x.cost !== undefined ? <span title={x.priced && x.priced !== "list" ? `At ${x.priced} prices` : "At list prices"}>{usd(x.cost)}</span> : "—",
            <span key="e" className={x.errors ? "bad" : ""}>{x.errors}</span>,
            <span key="s" className="muted small">{stops(x.stops)}</span>,
          ])}
        />
      </section>

      <section className="perf-sec">
        <h2>Sessions & context</h2>
        <div className="tiles">
          <Tile k="Active" v={t.sessions} tip="Sessions with any activity in the range" />
          <Tile k="Messages" v={t.messages} tip="Messages from people and connectors" />
          <Tile k="Near full" v={m.sessions.filter((s) => s.context_limit && s.context_used / s.context_limit >= 0.7).length} tip="Sessions at 70% of their context or more: PAI compacts them" />
          <Tile k="Compactions" v={m.harness.compactions} tip="Times PAI summarized the oldest records to free context" />
        </div>
        <Table
          scroll
          head={["Session", "Model", "Context", "Growth", "Calls", "Tokens", "Compacted", "Est. cost", "Updated"]}
          tips={[, , "Context used now, of its limit", "Prompt size over its newest steps"]}
          empty="No sessions in this range."
          rows={[...m.sessions]
            .sort((a, b) => fill(b) - fill(a))
            .map((s) => [
              <a key="n" href={`#/sessions/${s.id}`} onClick={(e) => (e.preventDefault(), go({ page: "sessions", arg: s.id }))}>
                {sessionTitle({ id: s.id, name: s.name } as Session)}
              </a>,
              <span key="m" className="muted small">{s.model.split("/").pop()}</span>,
              <Fill key="c" used={s.context_used} limit={s.context_limit} />,
              <Spark key="g" values={s.growth} />,
              s.steps,
              tokens(prompt(s.usage) + s.usage.output),
              s.compactions || "",
              m.priced ? usd(s.cost) : "—",
              <span key="u" className="muted small">{ago(s.updated)}</span>,
            ])}
        />
      </section>

      <section className="perf-sec">
        <h2>Tools & harness</h2>
        <div className="tiles">
          <Tile k="Tool calls" v={t.tool_calls} tip="Calls the model made to tools: run, read, write, edit, patch, memory" />
          <Tile k="Failed" v={t.tool_calls ? pc(fails / t.tool_calls) : "—"} tone={fails ? "bad" : ""} tip="Tool calls that exited non-zero or errored" />
          <Tile k="Calls per message" v={m.harness.steps_per_message ? m.harness.steps_per_message.toFixed(1) : "—"} tip="Model calls the agent loop made for each message: higher means more tool rounds" />
          <Tile k="Failovers" v={m.harness.failovers} tip="Times a session moved to another account at a usage limit" />
        </div>
        <div className="perf-two">
          <Table
            head={["Tool", "Calls", "Failed", "Cut", "Speed", "Slowest 5%"]}
            tips={[, , , "Output too long, cut to fit the context", "Median time the tool took"]}
            empty="No tool calls in this range."
            rows={m.tools.map((x) => [<b key="n">{x.name}</b>, x.calls, <span key="f" className={x.failures ? "bad" : ""}>{x.failures}</span>, x.truncated || "", ms(x.p50_ms), ms(x.p95_ms)])}
          />
          <div className="card perf-errors">
            <h3>Recent errors</h3>
            {m.harness.recent_errors.length === 0 ? (
              <p className="muted small">None in this range.</p>
            ) : (
              <ul>
                {m.harness.recent_errors.map((e, i) => (
                  <li key={i} onClick={() => go({ page: "sessions", arg: e.session })} title={e.text}>
                    <span className="bad small">{e.session}</span> <span className="ellipsis small">{e.text}</span> <span className="muted small">{ago(e.at)}</span>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      </section>

      {mem && (
        <section className="perf-sec">
          <h2>Memory & data</h2>
          <div className="tiles">
            <Tile k="Knows" v={mem.items} tip="Memories that hold now" />
            <Tile k="Learnt" v={mem.learnt} tip="New memories in the range" />
            <Tile k="Merged" v={mem.merged} tip="Memories merged into others or replaced by a newer version" />
            <Tile k="Forgotten" v={mem.forgotten} tip="Memories retired as no longer true" />
            <Tile k="Recalled" v={asked ? pc(mem.recalls / asked) : "—"} d={asked ? `of ${asked} messages` : ""} tip="Messages PAI recalled memory for; it skips the rest, such as small talk" />
            <Tile k="Used" v={pc(mem.used_rate)} tip="Share of recalled memories the model used in its reply, all time" />
            <Tile k="Reward" v={mem.reward !== undefined ? mem.reward.toFixed(2) : "—"} tip="Mean score of recall choices: what was used, less a cost per item. Higher is better" />
          </div>
          <div className="charts">
            <Chart title="Learnt" m={m} get={(p) => p.learnt} fmt={String} kind="bars" />
            <Chart title="Recall reward" m={m} get={(p) => p.reward ?? null} fmt={(v) => v.toFixed(2)} kind="line" />
            <div className="card chart">
              <h3>By kind</h3>
              <Hbars rows={KINDS.map(([k, l]) => [l, mem.by_kind[k] ?? 0, `k-${k}`] as [string, number, string])} />
            </div>
          </div>
          <div className="perf-two">
            <div className="card chart">
              <h3>By source</h3>
              <Hbars rows={Object.entries(mem.by_source).map(([s, n]) => [SOURCES[s] ?? s, n, ""] as [string, number, string])} />
            </div>
            <div className="card chart" title="For each message PAI picks how much to recall, and learns which choice pays: bar length is the mean reward">
              <h3>Recall policy</h3>
              <Hbars rows={mem.bandit.map((a) => [a.k ? `recall ${a.k}` : "skip", a.reward, "", `${a.pulls}×`] as [string, number, string, string])} fmt={(v) => v.toFixed(2)} />
            </div>
          </div>
        </section>
      )}
    </>
  );
}

function Tile({ k, v, d, tip, tone }: { k: string; v: ReactNode; d?: string; tip?: string; tone?: string }) {
  return (
    <div className="tile" title={tip}>
      <div className="tile-k">{k}</div>
      <div className={`tile-v ${tone ?? ""}`}>{v}</div>
      <div className="tile-d muted">{d ?? " "}</div>
    </div>
  );
}

function Table({ head, rows, tips = [], empty, scroll }: { head: string[]; rows: ReactNode[][]; tips?: (string | undefined)[]; empty: string; scroll?: boolean }) {
  return (
    <div className={`perf-table ${scroll ? "scroll" : ""}`}>
      <table>
        <thead>
          <tr>
            {head.map((h, i) => (
              <th key={h} title={tips[i]} className={i ? "num" : ""}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td colSpan={head.length} className="muted small">
                {empty}
              </td>
            </tr>
          ) : (
            rows.map((r, i) => (
              <tr key={i}>
                {r.map((c, j) => (
                  <td key={j} className={j ? "num" : ""}>
                    {c}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}

// useWidth is an element's width in pixels, so charts draw at their real size.
function useWidth<T extends HTMLElement>(): [RefObject<T>, number] {
  const ref = useRef<T>(null);
  const [w, setW] = useState(0);
  useLayoutEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver(([e]) => setW(Math.floor(e.contentRect.width)));
    ro.observe(ref.current);
    return () => ro.disconnect();
  }, []);
  return [ref, w];
}

type Pt = Metrics["series"][number];
const CH = 96; // chart height in px

// Chart is one measure over the range, as bars or a line, with the value of
// the bucket under the pointer.
function Chart({ title, m, get, fmt, kind }: { title: string; m: Metrics; get: (p: Pt) => number | null; fmt: (v: number) => string; kind: "bars" | "line" }) {
  const [ref, w] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const vs = m.series.map(get);
  const max = Math.max(0, ...vs.map((v) => v ?? 0));
  const lo = Math.min(0, ...vs.map((v) => v ?? 0)); // a reward can be below zero
  const n = Math.max(1, vs.length);
  const step = w / n;
  const y = (v: number) => CH - 4 - (max - lo ? ((v - lo) / (max - lo)) * (CH - 12) : 0);
  const label = (i: number) => when(m.series[i]?.at, m.bucket);
  const h = hover === null ? null : vs[hover];
  let path = "";
  vs.forEach((v, i) => {
    if (v === null) return;
    path += `${path && vs[i - 1] !== null ? "L" : "M"}${(i + 0.5) * step},${y(v)}`;
  });
  return (
    <div className="card chart">
      <h3>
        {title}
        <span className="chart-read muted small">{hover !== null ? `${label(hover)} · ${h === null ? "—" : fmt(h)}` : max ? `max ${fmt(max)}` : ""}</span>
      </h3>
      <div ref={ref} className="chart-box" onMouseLeave={() => setHover(null)}>
        {w > 0 && (
          <svg width={w} height={CH} onMouseMove={(e) => setHover(Math.min(n - 1, Math.max(0, Math.floor((e.nativeEvent.offsetX / w) * n))))}>
            <line x1={0} x2={w} y1={y(0)} y2={y(0)} className="axis" />
            {kind === "bars"
              ? vs.map((v, i) =>
                  v ? <rect key={i} x={i * step + Math.min(2, step * 0.15)} width={Math.max(1, step - Math.min(4, step * 0.3))} y={y(v)} height={CH - 4 - y(v)} rx={Math.min(3, step / 4)} className={`bar ${hover === i ? "on" : ""}`} /> : null,
                )
              : (
                <>
                  <path d={path} className="line" />
                  {vs.map((v, i) => (v !== null && (hover === i || n <= 31) ? <circle key={i} cx={(i + 0.5) * step} cy={y(v)} r={hover === i ? 4 : 2.5} className="pt" /> : null))}
                </>
              )}
            {hover !== null && <line x1={(hover + 0.5) * step} x2={(hover + 0.5) * step} y1={0} y2={CH - 4} className="cross" />}
          </svg>
        )}
      </div>
      <div className="chart-x muted small">
        <span>{when(m.series[0]?.at, m.bucket, true)}</span>
        <span>now</span>
      </div>
    </div>
  );
}

function Hbars({ rows, fmt = String }: { rows: [string, number, string, string?][]; fmt?: (v: number) => string }) {
  const max = Math.max(0.0001, ...rows.map((r) => Math.abs(r[1])));
  if (rows.length === 0) return <p className="muted small">Nothing in this range.</p>;
  return (
    <ul className="hbars">
      {rows.map(([l, v, cls, extra]) => (
        <li key={l}>
          <span className="hb-l">{l}</span>
          <span className="hb-t">
            <i className={`${cls} ${v < 0 ? "neg" : ""}`} style={{ width: `${(Math.abs(v) / max) * 100}%` }} />
          </span>
          <span className="hb-v small">
            {fmt(v)}
            {extra && <span className="muted"> · {extra}</span>}
          </span>
        </li>
      ))}
    </ul>
  );
}

function Fill({ used, limit }: { used: number; limit: number }) {
  const f = limit ? used / limit : 0;
  return (
    <span className="fill" title={`${tokens(used)} of ${tokens(limit)} tokens`}>
      <span className="meter thin">
        <i className={f >= 0.7 ? "hot" : ""} style={{ width: `${Math.min(100, f * 100)}%` }} />
      </span>
      <span className="small">{pc(f)}</span>
    </span>
  );
}

// Spark is a session's prompt over its newest steps, scaled to its own range
// so the shape shows; the numbers are in the tooltip.
function Spark({ values }: { values: number[] }) {
  if (values.length < 2) return <span className="muted small">—</span>;
  const W = 80;
  const H = 20;
  const lo = Math.min(...values);
  const span = Math.max(1, Math.max(...values) - lo);
  const d = values.map((v, i) => `${i ? "L" : "M"}${(i / (values.length - 1)) * W},${H - 2 - ((v - lo) / span) * (H - 4)}`).join("");
  return (
    <svg width={W} height={H} className="spark">
      <title>{`${tokens(values[0])} → ${tokens(values[values.length - 1])} tokens over ${values.length} calls`}</title>
      <path d={d} />
    </svg>
  );
}

const prompt = (u: Usage) => u.input + (u.cache_read ?? 0) + (u.cache_write ?? 0);
const fill = (s: Metrics["sessions"][number]) => (s.context_limit ? s.context_used / s.context_limit : 0);
const pc = (f: number) => `${Math.round(f * 100)}%`;
const usd = (v: number) => (v === 0 ? "$0" : v < 0.01 ? "<$0.01" : `$${v.toFixed(v < 10 ? 2 : 0)}`);
function ms(v: number): string {
  if (!v) return "—";
  return v < 1000 ? `${v} ms` : `${(v / 1000).toFixed(v < 10_000 ? 1 : 0)} s`;
}
function stops(s: Record<string, number>): string {
  return Object.entries(s)
    .sort((a, b) => b[1] - a[1])
    .map(([k, n]) => `${k.replace(/_/g, " ")} ${n}`)
    .join(" · ");
}
// when is a bucket's start: its hour, or its day; with day, an hour bucket
// says which day too.
function when(iso: string | undefined, bucket: Metrics["bucket"], day = false): string {
  if (!iso) return "";
  const d = new Date(iso);
  const date = d.toLocaleDateString([], { month: "short", day: "numeric" });
  if (bucket !== "hour") return date;
  const time = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return day ? `${date}, ${time}` : time;
}
