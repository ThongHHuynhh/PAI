// The PAI server's HTTP API, as the web app uses it. Every call carries the
// api key the person pasted at login; it stays in this browser only.

const KEY = "pai.key";

export function getKey(): string | null {
  try {
    return localStorage.getItem(KEY);
  } catch {
    return null;
  }
}

export function setKey(k: string | null) {
  try {
    if (k) localStorage.setItem(KEY, k);
    else localStorage.removeItem(KEY);
  } catch {
    /* private window: the key lasts this tab */
  }
  memoryKey = k;
}

let memoryKey: string | null = getKey();

// bearer is what the server checks: the secret of a whole api key
// (pai_key_<server key>.<secret>.<url>, as install prints it), or the key as
// pasted when it is the secret alone.
function bearer(): string {
  const k = memoryKey ?? "";
  const parts = k.startsWith("pai_key_") ? k.split(".") : [];
  return `Bearer ${parts.length === 3 ? parts[1] : k}`;
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

export async function call<T>(method: string, path: string, body?: unknown, type = "application/json"): Promise<T> {
  const headers: Record<string, string> = { Authorization: bearer() };
  let payload: BodyInit | undefined;
  if (body !== undefined) {
    headers["Content-Type"] = type;
    payload = typeof body === "string" ? body : JSON.stringify(body);
  }
  const r = await fetch(path, { method, headers, body: payload });
  if (!r.ok) throw new ApiError(r.status, (await r.text()).trim() || r.statusText);
  if (r.status === 204) return undefined as T;
  const ct = r.headers.get("Content-Type") ?? "";
  if (ct.includes("json")) return (await r.json()) as T;
  return (await r.text()) as unknown as T;
}

// --- types, as the server sends them ---

export type Usage = { input: number; output: number; cache_read?: number; cache_write?: number };

export type SessionModel = {
  model: string;
  name: string;
  provider: string;
  context_limit: number;
  current: boolean;
  ok: boolean;
  reason: string;
};

export function sessionModels(id: string): Promise<SessionModel[]> {
  return call("GET", `/sessions/${id}/models`);
}

export type Session = {
  id: string;
  name?: string;
  parent?: string; // the orchestrator session that delegated this one
  status: "open" | "closed" | "interrupted" | "failed" | "stepping";
  config: { model: string; tools: string; workers: { name: string }[] | null; context_limit: number; preset?: string };
  created: string;
  updated: string;
  n: number;
  context_used: number;
  usage: Usage;
};

export type Machine = {
  name: string;
  address: string;
  os?: string;
  hostname?: string;
  online: boolean;
  connected: boolean;
  os_arch?: string;
  interps?: string[];
  version?: string;
  refused?: string;
};

export type Item = {
  id: string;
  kind: string;
  text: string;
  scope: string;
  topic?: string;
  merged?: string[];
  session?: string;
  from?: number;
  to?: number;
  source: string;
  created: string;
  updated: string;
  invalid?: string;
  reason?: string;
  pinned: boolean;
  utility: number;
  shown: number;
  used: number;
};

export type Hit = { item: Item; score: number; lexical?: number; vector?: number; cosine: number; pinned?: boolean; used?: boolean };

export type Stats = {
  items: number;
  invalid: number;
  pinned: number;
  by_kind: Record<string, number>;
  by_scope: Record<string, number>;
  recalls: number;
  reward: number;
  used_rate: number;
  embedder: string;
};

export type Arm = { k: number; pulls: number; reward: number };

export type Retrieval = {
  id: string;
  session: string;
  at: string;
  query: string;
  k: number;
  features: number[];
  hits: Hit[] | null;
  reward?: number;
  ms: number;
};

// Metrics is GET /metrics: how PAI performed over a range (pkg/server/metrics.go).
export type MetricsRange = "24h" | "7d" | "30d" | "all";
export type Metrics = {
  range: MetricsRange;
  since: string;
  bucket: "hour" | "day" | "week";
  priced: boolean;
  totals: { sessions: number; messages: number; steps: number; usage: Usage; cost: number; tool_calls: number; errors: number; p50_ms: number; p95_ms: number; cache_hit: number };
  series: { at: string; steps: number; tokens: number; p50_ms: number; errors: number; learnt: number; reward?: number }[];
  models: { model: string; steps: number; usage: Usage; cost?: number; priced?: string; p50_ms: number; p95_ms: number; cache_hit: number; errors: number; stops: Record<string, number> }[];
  sessions: { id: string; name?: string; model: string; status: string; context_used: number; context_limit: number; steps: number; messages: number; compactions: number; usage: Usage; cost: number; updated: string; growth: number[] }[];
  tools: { name: string; calls: number; failures: number; truncated: number; p50_ms: number; p95_ms: number }[];
  harness: { steps_per_message: number; compactions: number; failovers: number; recent_errors: { session: string; at: string; text: string }[] };
  stops: Record<string, number>;
  limits?: Record<string, { at: string; status: string; windows: Record<string, { utilization: number; resets: string }> }>;
  memory?: {
    items: number;
    learnt: number;
    by_kind: Record<string, number>;
    by_source: Record<string, number>;
    merged: number;
    forgotten: number;
    recalls: number;
    skipped: number;
    reward?: number;
    used_rate: number;
    recall_p50_ms: number;
    recall_p95_ms: number;
    bandit: Arm[];
  };
};

export type Graph = {
  server: { version: string; url: string };
  machines: Machine[] | null;
  sessions: Session[] | null;
  memory?: Stats;
  bandit?: Arm[];
};

export type PaiEvent = {
  type: "hello" | "record" | "worker" | "memory" | "session";
  at: string;
  session?: string;
  worker?: string;
  data?: any;
};

export type Rec = {
  n: number;
  kind: string;
  id: string;
  time: string;
  type?: string;
  via?: string;
  source?: string;
  for?: string;
  step?: string;
  upto?: number;
  model?: string;
  usage?: Usage;
  stop?: string;
  exit?: number;
  body: string;
  media?: boolean;
};

export type Preset = { name: string; provider: string; model: string; tools: string; context_limit: number; input: string[]; params: Record<string, unknown> };
export type Catalog = { providers: Record<string, { name: string; shape: string; url?: string; group?: string }>; presets: Record<string, Preset>; groups: Record<string, string[]> };
// Account is what PAI knows of one account's usage; a null is not known yet.
export type Account = { name: string; status: "ok" | "limited"; limited_until: string; util_five_hour: number | null; util_seven_day: number | null; updated: string };
// Balance, in auto mode, spreads sessions over a pool of interchangeable provider/model entries.
export type Balance = { mode: "off" | "auto"; pool: string[] };
export type ProviderInfo = { name: string; shape: string; url?: string; label?: string };
// labelled is a provider id as plain text: its label and the id, if it has a label, else fallback.
export const labelled = (labels: Record<string, string>, id: string, fallback = id) => (labels[id] ? `${labels[id]} (${id})` : fallback);

// --- records: the state file's own bytes, a JSON header line, len bytes of
// body, and a newline each ---

export function parseRecords(buf: ArrayBuffer): Rec[] {
  const bytes = new Uint8Array(buf);
  const dec = new TextDecoder();
  const out: Rec[] = [];
  let i = 0;
  while (i < bytes.length) {
    const nl = bytes.indexOf(10, i);
    if (nl < 0) break;
    const h = JSON.parse(dec.decode(bytes.subarray(i, nl)));
    const start = nl + 1;
    const end = start + (h.len ?? 0);
    const type: string = h.type ?? "";
    const text = type === "" || type.startsWith("text/") || type.includes("json");
    out.push({ ...h, body: text ? dec.decode(bytes.subarray(start, end)) : "", media: !text });
    i = end + 1;
  }
  return out;
}

export async function records(id: string, after: number, wait = ""): Promise<Rec[]> {
  const r = await fetch(`/sessions/${id}/records?after=${after}${wait ? `&wait=${wait}` : ""}`, {
    headers: { Authorization: bearer() },
  });
  if (!r.ok) throw new ApiError(r.status, await r.text());
  return parseRecords(await r.arrayBuffer());
}

// --- the event stream: Server-Sent Events read with fetch, so the key goes
// in a header, not the URL. It reconnects after a drop. ---

// SHARE names the lock and the channel by which one tab per browser holds the
// stream and hands its events to the others. Over plain http a browser opens
// six connections to a server, and a stream per tab held them all: a seventh
// tab never loaded.
const SHARE = "pai.events";

// streamEvents feeds on every event and status whether the stream is up. The
// tab holding the lock streams and broadcasts; the rest listen. When it
// closes, the next tab waiting on the lock takes over. A browser without
// locks or channels streams in every tab.
export function streamEvents(on: (e: PaiEvent) => void, status: (live: boolean) => void): () => void {
  if (!navigator.locks || typeof BroadcastChannel === "undefined") return stream(on, status);
  const bc = new BroadcastChannel(SHARE);
  const post = (m: unknown) => {
    try {
      bc.postMessage(m);
    } catch {
      /* closed: this tab is going */
    }
  };
  let quit = new AbortController();
  let lead: { live: boolean } | null = null;
  let release = () => {};
  bc.onmessage = ({ data }) => {
    if (data.event) on(data.event);
    else if (typeof data.live === "boolean") status(data.live);
    else if (data.ask && lead) post({ live: lead.live });
  };
  const queue = () => {
    quit = new AbortController();
    navigator.locks
      .request(SHARE, { signal: quit.signal }, () =>
        new Promise<void>((done) => {
          const me = { live: false };
          lead = me;
          const stop = stream(
            (e) => {
              on(e);
              post({ event: e });
            },
            (l) => {
              me.live = l;
              status(l);
              post({ live: l });
            },
          );
          release = () => {
            lead = null;
            release = () => {};
            stop();
            done();
          };
        }),
      )
      .catch(() => {
        /* the tab went while it waited for the lock */
      });
  };
  // A tab that closes, or goes into the back-forward cache, hands the stream
  // on at once; a cached page queues again when it is shown.
  const hide = () => {
    quit.abort();
    release();
  };
  const show = (e: PageTransitionEvent) => {
    if (e.persisted) queue();
  };
  window.addEventListener("pagehide", hide);
  window.addEventListener("pageshow", show);
  queue();
  post({ ask: true }); // a tab that opens while the stream is up learns so
  return () => {
    window.removeEventListener("pagehide", hide);
    window.removeEventListener("pageshow", show);
    hide();
    bc.close();
  };
}

// stream holds one connection to /events until it is stopped.
function stream(on: (e: PaiEvent) => void, status: (live: boolean) => void): () => void {
  let stopped = false;
  let ctrl = new AbortController();
  const run = async () => {
    while (!stopped) {
      try {
        ctrl = new AbortController();
        const r = await fetch("/events", { headers: { Authorization: bearer() }, signal: ctrl.signal });
        if (!r.ok || !r.body) throw new Error(String(r.status));
        status(true);
        const reader = r.body.getReader();
        const dec = new TextDecoder();
        let buf = "";
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          let cut;
          while ((cut = buf.indexOf("\n\n")) >= 0) {
            const chunk = buf.slice(0, cut);
            buf = buf.slice(cut + 2);
            for (const line of chunk.split("\n")) {
              if (line.startsWith("data: ")) {
                try {
                  on(JSON.parse(line.slice(6)));
                } catch {
                  /* a torn event is skipped */
                }
              }
            }
          }
        }
      } catch {
        /* dropped: try again */
      }
      if (stopped) return;
      status(false);
      await new Promise((res) => setTimeout(res, 2000));
    }
  };
  void run();
  return () => {
    stopped = true;
    ctrl.abort();
  };
}

// --- small formatting helpers ---

export function ago(iso?: string): string {
  if (!iso) return "";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}

export function tokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

// sessionTitle is the session's name, else its id: calm-otter reads as it is,
// while an id from before sessions had names is a hex code that needs a word.
export function sessionTitle(s: Session): string {
  return s.name || (/^[0-9a-f]{8}$/.test(s.id) ? `Session ${s.id}` : s.id);
}
