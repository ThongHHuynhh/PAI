import { useEffect, useMemo, useState } from "react";
import { Background, Controls, Handle, Position, ReactFlow, type Edge, type Node, type NodeProps } from "@xyflow/react";
import { ago, sessionTitle, tokens, type Machine, type Session, type Stats } from "../api";
import { describe, useLive } from "../live";
import { go } from "../App";
import { Orbit } from "../components/Orbit";

// How long a node glows after it did something, and how many sessions the
// graph draws: the newest; the rest are a click away in Sessions.
const GLOW_MS = 3500;
const MAX_SESSIONS = 14;

type CoreData = { version: string; url: string; machines: number; working: number; glow: boolean };
type MachineData = { m: Machine; sessions: number; glow: boolean };
type SessionData = { s: Session; glow: boolean };
type MemoryData = { st?: Stats; glow: boolean };

function CoreNode({ data }: NodeProps<Node<CoreData>>) {
  return (
    <div className={`gnode core ${data.glow ? "glow" : ""}`}>
      <Orbit size={74} nodes={Math.max(1, data.machines)} />
      <div className="gtitle">PAI</div>
      <div className="gsub">{data.working ? `${data.working} working` : "idle"}</div>
      <Handle type="source" position={Position.Right} id="r" />
      <Handle type="target" position={Position.Left} id="in" />
      <Handle type="source" position={Position.Top} id="t" />
      <Handle type="source" position={Position.Bottom} id="b" />
    </div>
  );
}

function MachineNode({ data }: NodeProps<Node<MachineData>>) {
  const m = data.m;
  const state = m.connected ? "connected" : m.online ? "on mesh" : "offline";
  return (
    <div className={`gnode machine ${m.connected ? "" : "dim"} ${data.glow ? "glow" : ""}`}>
      <Handle type="target" position={Position.Left} />
      <div className="grow-row">
        <span className={`dot ${m.connected ? "on" : ""}`} />
        <span className="gtitle">{m.name}</span>
      </div>
      <div className="gsub">{m.os_arch ?? m.os ?? "machine"}</div>
      <div className="gsub">
        {state}
        {data.sessions ? ` · ${data.sessions} session${data.sessions > 1 ? "s" : ""}` : ""}
      </div>
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

function SessionNode({ data }: NodeProps<Node<SessionData>>) {
  const s = data.s;
  const fill = s.config.context_limit ? Math.min(1, s.context_used / s.config.context_limit) : 0;
  const [provider, model] = splitModel(s.config.model);
  return (
    <div className={`gnode session ${s.status} ${data.glow ? "glow" : ""}`}>
      <Handle type="target" position={Position.Left} id="left" />
      <Handle type="target" position={Position.Top} id="top" />
      <div className="grow-row">
        <span className={`pill ${s.status}`}>{s.status === "stepping" ? "working" : s.status}</span>
        <span className="gtitle ellipsis">{sessionTitle(s)}</span>
      </div>
      <div className="gsub">
        <b>{model}</b> <span className="muted">via {provider}</span>
      </div>
      <div className="meter" title={`context ${tokens(s.context_used)} of ${tokens(s.config.context_limit)}`}>
        <i style={{ width: `${fill * 100}%` }} className={fill > 0.7 ? "hot" : ""} />
      </div>
      <div className="gsub muted">
        {tokens(s.context_used)} / {tokens(s.config.context_limit)} ctx · {ago(s.updated)}
      </div>
    </div>
  );
}

function MemoryNode({ data }: NodeProps<Node<MemoryData>>) {
  const st = data.st;
  return (
    <div className={`gnode memory ${data.glow ? "glow" : ""}`}>
      <div className="grow-row">
        <span className="accent">◆</span>
        <span className="gtitle">Knowledge</span>
      </div>
      <div className="gsub">{st ? `${st.items} items learnt` : "no store"}</div>
      {st && (
        <div className="gsub muted">
          {Math.round(st.used_rate * 100)}% of recalls used · {st.recalls} recalls
        </div>
      )}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

const nodeTypes = { core: CoreNode, machine: MachineNode, session: SessionNode, memory: MemoryNode };

export function splitModel(m: string): [string, string] {
  const i = m.indexOf("/");
  return i < 0 ? ["", m] : [m.slice(0, i), m.slice(i + 1)];
}

export default function GraphPage() {
  const { graph, pulse, feed } = useLive();
  const [now, setNow] = useState(Date.now());
  const [sel, setSel] = useState<string | null>(null);
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 500);
    return () => window.clearInterval(t);
  }, []);
  const glow = (id: string) => now - (pulse[id] ?? 0) < GLOW_MS;

  const machines = graph?.machines ?? [];
  const sessions = useMemo(
    () => [...(graph?.sessions ?? [])].sort((a, b) => b.updated.localeCompare(a.updated)).slice(0, MAX_SESSIONS),
    [graph],
  );

  const { nodes, edges } = useMemo(() => {
    const nodes: Node[] = [];
    const edges: Edge[] = [];
    const working = sessions.filter((s) => s.status === "stepping").length;
    nodes.push({ id: "core", type: "core", position: { x: -70, y: -80 }, data: { version: graph?.server.version, url: graph?.server.url, machines: machines.length, working, glow: glow("core") }, draggable: true });
    nodes.push({ id: "memory", type: "memory", position: { x: -330, y: -40 }, data: { st: graph?.memory, glow: glow("memory") } });
    edges.push({ id: "e:memory", source: "memory", target: "core", targetHandle: "in", animated: glow("memory"), className: glow("memory") ? "hot" : "" });

    // machines on an arc right of the core
    const mpos: Record<string, { x: number; y: number }> = {};
    const count = Math.max(1, machines.length);
    machines.forEach((m, i) => {
      const a = (count === 1 ? 0 : -50 + (100 * i) / (count - 1)) * (Math.PI / 180);
      const p = { x: 260 + Math.cos(a) * 60, y: Math.sin(a) * 260 - 40 };
      mpos[m.name] = p;
      const used = sessions.filter((s) => s.config.workers?.some((w) => w.name === m.name)).length;
      nodes.push({ id: `m:${m.name}`, type: "machine", position: p, data: { m, sessions: used, glow: glow(`m:${m.name}`) } });
      edges.push({ id: `e:core-${m.name}`, source: "core", sourceHandle: "r", target: `m:${m.name}`, animated: m.connected, className: `${m.connected ? "" : "off"} ${glow(`m:${m.name}`) ? "hot" : ""}` });
    });

    // sessions right of their first machine, or below the core
    const stack: Record<string, number> = {};
    sessions.forEach((s) => {
      const first = s.config.workers?.find((w) => mpos[w.name])?.name;
      const key = first ?? "_";
      const k = (stack[key] = (stack[key] ?? 0) + 1) - 1;
      const base = first ? { x: mpos[first].x + 300, y: mpos[first].y - 40 } : { x: -110 + (k % 3) * 300, y: 170 + Math.floor(k / 3) * 150 };
      const p = first ? { x: base.x + (k % 2) * 40, y: base.y + k * 125 } : base;
      const g = glow(`s:${s.id}`);
      nodes.push({ id: `s:${s.id}`, type: "session", position: p, data: { s, glow: g } });
      const ws = (s.config.workers ?? []).filter((w) => mpos[w.name]);
      if (ws.length === 0) edges.push({ id: `e:core-s-${s.id}`, source: "core", sourceHandle: "b", target: `s:${s.id}`, targetHandle: "top", animated: s.status === "stepping" || g, className: g ? "hot" : "" });
      ws.forEach((w) => edges.push({ id: `e:${w.name}-${s.id}`, source: `m:${w.name}`, target: `s:${s.id}`, targetHandle: "left", animated: s.status === "stepping" || g, className: g ? "hot" : "" }));
    });
    return { nodes, edges };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [graph, sessions, machines, now, pulse]);

  const selected = sel?.startsWith("s:") ? sessions.find((s) => `s:${s.id}` === sel) : undefined;
  const selMachine = sel?.startsWith("m:") ? machines.find((m) => `m:${m.name}` === sel) : undefined;

  return (
    <div className="graph-page">
      <div className="graph-canvas">
        <ReactFlow
          key={nodes.map((n) => n.id).join(",")}
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          fitView
          fitViewOptions={{ padding: 0.2 }}
          minZoom={0.3}
          proOptions={{ hideAttribution: true }}
          onNodeClick={(_, n) => setSel(n.id)}
          onPaneClick={() => setSel(null)}
          nodesConnectable={false}
        >
          <Background gap={28} size={1} color="var(--grid)" />
          <Controls showInteractive={false} />
        </ReactFlow>
        {graph && machines.length === 0 && (
          <div className="graph-empty">
            <b>No machines yet.</b> Connect one from <a href="#/settings">Settings</a>: every machine becomes a node here.
          </div>
        )}
      </div>
      <aside className="graph-side">
        {selected && (
          <section className="card">
            <h3>{sessionTitle(selected)}</h3>
            <dl className="kv">
              <dt>Model</dt>
              <dd>{selected.config.model}</dd>
              <dt>Status</dt>
              <dd>
                <span className={`pill ${selected.status}`}>{selected.status}</span>
              </dd>
              <dt>Machines</dt>
              <dd>{selected.config.workers?.map((w) => w.name).join(", ") || "none"}</dd>
              <dt>Context</dt>
              <dd>
                {tokens(selected.context_used)} of {tokens(selected.config.context_limit)}
              </dd>
              <dt>Tokens</dt>
              <dd>
                {tokens(selected.usage.input + (selected.usage.cache_read ?? 0))} in · {tokens(selected.usage.output)} out
                {selected.usage.cache_read ? ` · ${tokens(selected.usage.cache_read)} cached` : ""}
              </dd>
            </dl>
            <button className="primary small" onClick={() => go({ page: "sessions", arg: selected.id })}>
              Open session
            </button>
          </section>
        )}
        {selMachine && (
          <section className="card">
            <h3>{selMachine.name}</h3>
            <dl className="kv">
              <dt>State</dt>
              <dd>{selMachine.connected ? "connected" : selMachine.online ? "on the mesh, worker not connected" : "offline"}</dd>
              <dt>System</dt>
              <dd>{selMachine.os_arch ?? selMachine.os ?? "unknown"}</dd>
              <dt>Shells</dt>
              <dd>{selMachine.interps?.join(", ") || "—"}</dd>
              <dt>Mesh</dt>
              <dd className="mono small">{selMachine.address}</dd>
              {selMachine.refused && (
                <>
                  <dt>Refused</dt>
                  <dd className="error">{selMachine.refused}</dd>
                </>
              )}
            </dl>
          </section>
        )}
        {sel === "memory" && graph?.memory && (
          <section className="card">
            <h3>Knowledge</h3>
            <p className="muted small">{graph.memory.items} items learnt across every session, model and machine.</p>
            <button className="primary small" onClick={() => go({ page: "knowledge" })}>
              See what PAI knows
            </button>
          </section>
        )}
        <section className="card glance only-phone-block">
          <h3>At a glance</h3>
          <ul>
            {machines.map((m) => (
              <li key={m.name}>
                <span className={`dot ${m.connected ? "on" : ""}`} /> <b>{m.name}</b> <span className="muted small">{m.connected ? "connected" : "offline"}</span>
              </li>
            ))}
            {sessions.slice(0, 6).map((s) => (
              <li key={s.id} onClick={() => go({ page: "sessions", arg: s.id })}>
                <span className={`pill ${s.status}`}>{s.status === "stepping" ? "working" : s.status}</span> <span className="ellipsis">{sessionTitle(s)}</span>
                <span className="muted small">{splitModel(s.config.model)[1]}</span>
              </li>
            ))}
          </ul>
        </section>
        <section className="card feed">
          <h3>Activity</h3>
          {feed.length === 0 && <p className="muted small">Waiting for something to happen…</p>}
          <ul>
            {feed.slice(0, 40).map((e, i) => {
              const d = describe(e);
              return (
                <li key={i} className={d.tone ?? ""} onClick={() => e.session && go({ page: "sessions", arg: e.session })}>
                  <span className="ficon">{d.icon}</span>
                  <span className="ftext">{d.text}</span>
                  <span className="ftime">{ago(e.at)}</span>
                </li>
              );
            })}
          </ul>
        </section>
      </aside>
    </div>
  );
}
