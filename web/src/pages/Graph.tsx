import { useEffect, useMemo, useRef, useState } from "react";
import { Background, ControlButton, Controls, Handle, Position, ReactFlow, applyNodeChanges, type Edge, type Node, type NodeChange, type NodeProps, type XYPosition } from "@xyflow/react";
import { ago, sessionTitle, tokens, type Machine, type Session, type Stats } from "../api";
import { describe, useLive } from "../live";
import { go } from "../App";
import { Orbit } from "../components/Orbit";

// How long a node glows after it did something, and how many sessions the
// graph draws: the newest; the rest are a click away in Sessions.
const GLOW_MS = 3500;
const MAX_SESSIONS = 14;

// MOVED keeps where the person dragged each node, in this browser, so the
// layout they made survives the graph rebuilding twice a second and a reload.
const MOVED = "pai.graph.moved";
function loadMoved(): Record<string, XYPosition> {
  try {
    return JSON.parse(localStorage.getItem(MOVED) ?? "{}");
  } catch {
    return {};
  }
}
function saveMoved(m: Record<string, XYPosition>) {
  try {
    localStorage.setItem(MOVED, JSON.stringify(m));
  } catch {
    /* no storage: the layout lasts this tab */
  }
}

type CoreData = { version: string; url: string; machines: number; working: number; glow: boolean };
type MachineData = { m: Machine; sessions: number; glow: boolean };
type SessionData = { s: Session; kids: Session[]; glow: boolean };
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
      {data.kids.length > 0 && (
        <div className="gsub">
          <b>Manager</b> <span className="muted">· {data.kids.length} employee{data.kids.length > 1 ? "s" : ""}, {data.kids.filter((k) => k.status === "stepping").length} working</span>
        </div>
      )}
      <Handle type="source" position={Position.Right} id="team" />
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
  const [moved, setMoved] = useState(loadMoved);
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
    nodes.push({ id: "core", type: "core", position: { x: -70, y: -80 }, data: { version: graph?.server.version, url: graph?.server.url, machines: machines.length, working, glow: glow("core") } });
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

    // A session whose manager is drawn sits to the right of it, joined by a
    // team edge. The others go right of their first machine, or below the core.
    const shown = new Set(sessions.map((s) => s.id));
    const kidsOf = (id: string) => sessions.filter((s) => s.parent === id);
    const at: Record<string, { x: number; y: number }> = {};
    const stack: Record<string, number> = {};
    sessions.forEach((s) => {
      if (s.parent && shown.has(s.parent)) return;
      const first = s.config.workers?.find((w) => mpos[w.name])?.name;
      const key = first ?? "_";
      const k = (stack[key] = (stack[key] ?? 0) + 1) - 1;
      const base = first ? { x: mpos[first].x + 300, y: mpos[first].y - 40 } : { x: -110 + (k % 3) * 300, y: 170 + Math.floor(k / 3) * 150 };
      const p = first ? { x: base.x + (k % 2) * 40, y: base.y + k * 125 } : base;
      at[s.id] = p;
      const g = glow(`s:${s.id}`);
      nodes.push({ id: `s:${s.id}`, type: "session", position: p, data: { s, kids: kidsOf(s.id), glow: g } });
      const ws = (s.config.workers ?? []).filter((w) => mpos[w.name]);
      if (ws.length === 0) edges.push({ id: `e:core-s-${s.id}`, source: "core", sourceHandle: "b", target: `s:${s.id}`, targetHandle: "top", animated: s.status === "stepping" || g, className: g ? "hot" : "" });
      ws.forEach((w) => edges.push({ id: `e:${w.name}-${s.id}`, source: `m:${w.name}`, target: `s:${s.id}`, targetHandle: "left", animated: s.status === "stepping" || g, className: g ? "hot" : "" }));
    });
    const team = (m: Session) => {
      kidsOf(m.id).forEach((k, i) => {
        const p = { x: at[m.id].x + 330, y: at[m.id].y + i * 130 };
        at[k.id] = p;
        const g = glow(`s:${k.id}`);
        nodes.push({ id: `s:${k.id}`, type: "session", position: p, data: { s: k, kids: kidsOf(k.id), glow: g } });
        edges.push({ id: `e:team-${k.id}`, source: `s:${m.id}`, sourceHandle: "team", target: `s:${k.id}`, targetHandle: "left", animated: k.status === "stepping" || g, className: `team ${g ? "hot" : ""}` });
        team(k);
      });
    };
    sessions.forEach((s) => at[s.id] && team(s));
    for (const n of nodes) if (moved[n.id]) n.position = moved[n.id];
    return { nodes, edges };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [graph, sessions, machines, now, pulse, moved]);

  // The flow keeps its own nodes, so a drag runs on them undisturbed; each
  // rebuild brings new data and keeps a node's place while it is dragged.
  const [flow, setFlow] = useState<Node[]>(nodes);
  const flowRef = useRef(flow);
  flowRef.current = flow;
  useEffect(() => {
    setFlow((prev) => {
      const old = new Map(prev.map((n) => [n.id, n]));
      return nodes.map((n) => {
        const o = old.get(n.id);
        return o ? { ...o, type: n.type, data: n.data, position: o.dragging ? o.position : n.position } : n;
      });
    });
  }, [nodes]);
  const onNodesChange = (changes: NodeChange[]) => {
    const next = applyNodeChanges(changes, flowRef.current);
    flowRef.current = next;
    setFlow(next);
    const ended = changes.filter((c) => c.type === "position" && c.dragging === false).map((c) => (c as { id: string }).id);
    if (ended.length) {
      const m = { ...moved };
      for (const n of next) if (ended.includes(n.id)) m[n.id] = n.position;
      setMoved(m);
      saveMoved(m);
    }
  };

  const selected = sel?.startsWith("s:") ? sessions.find((s) => `s:${s.id}` === sel) : undefined;
  const selectedKids = selected ? sessions.filter((s) => s.parent === selected.id) : [];
  const selectedManager = selected?.parent ? sessions.find((s) => s.id === selected.parent) : undefined;
  const selMachine = sel?.startsWith("m:") ? machines.find((m) => `m:${m.name}` === sel) : undefined;

  return (
    <div className="graph-page">
      <div className="graph-canvas">
        <ReactFlow
          key={nodes.map((n) => n.id).join(",")}
          nodes={flow}
          edges={edges}
          nodeTypes={nodeTypes}
          fitView
          fitViewOptions={{ padding: 0.2 }}
          minZoom={0.3}
          proOptions={{ hideAttribution: true }}
          onNodesChange={onNodesChange}
          onNodeClick={(_, n) => setSel(n.id)}
          onPaneClick={() => setSel(null)}
          nodesConnectable={false}
        >
          <Background gap={28} size={1} color="var(--grid)" />
          <Controls showInteractive={false}>
            <ControlButton
              title="Reset layout"
              aria-label="Reset layout"
              onClick={() => {
                setMoved({});
                saveMoved({});
              }}
            >
              ⟲
            </ControlButton>
          </Controls>
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
        {selected && (selectedKids.length > 0 || selectedManager) && (
          <section className="card">
            <h3>{selectedKids.length > 0 ? "Employees" : "Manager"}</h3>
            <ul className="plain team">
              {(selectedKids.length > 0 ? selectedKids : [selectedManager!]).map((k) => (
                <li key={k.id} onClick={() => setSel(`s:${k.id}`)}>
                  <span className={`pill ${k.status}`}>{k.status === "stepping" ? "working" : k.status}</span> <b className="ellipsis">{sessionTitle(k)}</b>
                  <span className="muted small">
                    {splitModel(k.config.model)[1]} · {tokens(k.context_used)} ctx · {ago(k.updated)}
                  </span>
                </li>
              ))}
            </ul>
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
