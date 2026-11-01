import { useEffect, useState } from "react";
import { ago, call, type Account, type Balance as BalanceT, type Catalog, type ProviderInfo } from "../api";
import { useLive } from "../live";
import { useTheme } from "../App";
import { READ } from "../components/Voice";

type Join = { join: string; unix?: string; windows?: string };
type LoginState = { state: string; error?: string; text?: string; url?: string; code?: string; paste?: boolean };
type Build = { branch: string; commit: string; dirty: string; builder: string; built: string; go: string; platform: string };
type SettingsT = { version: { current: string; latest?: string; build: Build } };

export default function Settings({ onOut }: { onOut: () => void }) {
  return (
    <div className="settings-page">
      <header className="page-head">
        <h1>Settings</h1>
      </header>
      <Browser onOut={onOut} />
      <Providers />
      <Failover />
      <Balance />
      <Machines />
      <Keys />
      <Voice />
      <Server />
    </div>
  );
}

// Browser is what this browser keeps: its theme and its sign-in.
function Browser({ onOut }: { onOut: () => void }) {
  const [theme, toggle] = useTheme();
  return (
    <section className="card">
      <h2>This browser</h2>
      <ul className="plain">
        <li>
          <label className="switch">
            <input type="checkbox" checked={theme === "dark"} onChange={toggle} />
            Dark theme
          </label>
        </li>
        <li>
          <button className="ghost small danger" onClick={() => confirm("Sign out of PAI in this browser? You will need the API key to sign in again.") && onOut()}>
            Sign out
          </button>
        </li>
      </ul>
    </section>
  );
}

function Providers() {
  const [cat, setCat] = useState<Catalog | null>(null);
  const [served, setServed] = useState<ProviderInfo[]>([]);
  const [pick, setPick] = useState("");
  const [account, setAccount] = useState("");
  const [key, setKey] = useState("");
  const [login, setLogin] = useState<{ name: string; st: LoginState } | null>(null);
  const [paste, setPaste] = useState("");
  const [err, setErr] = useState("");
  const load = () => call<ProviderInfo[]>("GET", "/providers").then(setServed, () => {});
  useEffect(() => {
    call<Catalog>("GET", "/catalog").then(setCat, () => {});
    void load();
  }, []);
  // a sign-in in progress: poll its state until it ends
  useEffect(() => {
    if (!login || login.st.state !== "pending") return;
    const t = window.setInterval(() => {
      call<LoginState>("GET", `/providers/${login.name}/login`).then((st) => {
        setLogin({ name: login.name, st });
        if (st.state === "done") void load();
      }, () => {});
    }, 2000);
    return () => window.clearInterval(t);
  }, [login]);

  const p = cat?.providers[pick];
  const subscription = p && (p.shape === "chatgpt" || p.shape === "claude-code");
  const connect = async () => {
    setErr("");
    try {
      if (subscription) {
        const name = (account.trim() || pick).toLowerCase().replace(/[^a-z0-9-]+/g, "-");
        const st = await call<LoginState>("POST", `/providers/${name}/login`, { shape: p!.shape });
        setLogin({ name, st });
        return;
      }
      for (const id of Object.entries(cat!.providers).filter(([id, x]) => id === pick || (p!.group && x.group === p!.group)).map(([id]) => id)) {
        const x = cat!.providers[id];
        await call("PUT", `/providers/${id}`, { shape: x.shape, url: x.url ?? "", key });
      }
      setKey("");
      setPick("");
      void load();
    } catch (e: any) {
      setErr(String(e.message ?? e));
    }
  };
  const ids = cat ? Object.keys(cat.providers).sort((a, b) => sub(cat, b) - sub(cat, a) || cat.providers[a].name.localeCompare(cat.providers[b].name)) : [];
  return (
    <section className="card">
      <h2>Model providers</h2>
      <p className="muted small">
        Sign in with a Claude or ChatGPT subscription, or add an API key. You can add several accounts of one subscription, each under its own name.
      </p>
      <ul className="plain">
        {served.length === 0 && <li className="muted">None connected yet.</li>}
        {served.map((s) => (
          <li key={s.name}>
            <span className="dot on" /> <b>{cat?.providers[s.name]?.name ?? s.name}</b> <span className="muted small">{s.shape}</span>
          </li>
        ))}
      </ul>
      <div className="row">
        <select
          value={pick}
          onChange={(e) => {
            const id = e.target.value;
            setPick(id);
            // a second account of a subscription gets its own name
            let n = id, i = 2;
            while (served.some((s) => s.name === n)) n = `${id}-${i++}`;
            setAccount(n);
          }}
        >
          <option value="">Connect a provider…</option>
          {ids.map((id) => (
            <option key={id} value={id}>
              {cat!.providers[id].name}
              {sub(cat!, id) ? " (subscription)" : ""}
            </option>
          ))}
        </select>
        {pick && !subscription && <input type="password" placeholder="API key" value={key} onChange={(e) => setKey(e.target.value)} />}
        {pick && subscription && <input placeholder="account name" value={account} onChange={(e) => setAccount(e.target.value)} title="Each account is its own provider, so a second Claude or ChatGPT account can take over when the first hits its limit." />}
        {pick && (
          <button className="primary small" disabled={!subscription && !key} onClick={connect}>
            {subscription ? "Sign in" : "Save key"}
          </button>
        )}
      </div>
      {login && (
        <div className="banner">
          {login.st.state === "done" ? (
            <>Connected {cat?.providers[login.name]?.name}.</>
          ) : login.st.state === "failed" ? (
            <span className="error">Sign-in failed: {login.st.error}</span>
          ) : (
            <>
              <p>{login.st.text || "Open this address, sign in, then paste the address the browser lands on."}</p>
              {login.st.url && (
                <p>
                  <a href={login.st.url} target="_blank" rel="noreferrer">
                    Open sign-in page
                  </a>
                  {login.st.code && (
                    <>
                      {" "}
                      code <code>{login.st.code}</code>
                    </>
                  )}
                </p>
              )}
              {login.st.paste && (
                <div className="row">
                  <input placeholder="Paste the address the browser landed on" value={paste} onChange={(e) => setPaste(e.target.value)} />
                  <button className="primary small" onClick={() => call("POST", `/providers/${login.name}/login/code`, { code: paste }).then(() => setPaste(""), (e) => setErr(String(e.message ?? e)))}>
                    Finish
                  </button>
                </div>
              )}
            </>
          )}
        </div>
      )}
      {err && <p className="error">{err}</p>}
    </section>
  );
}

// Failover is which account takes a session over when another hits its usage
// limit. History and memory belong to PAI, not to an account, so the new
// account goes on where the old one stopped.
function Failover() {
  const [served, setServed] = useState<ProviderInfo[]>([]);
  const [fb, setFb] = useState<Record<string, string>>({});
  const [saved, setSaved] = useState("");
  const [accounts, setAccounts] = useState<Account[]>([]);
  useEffect(() => {
    call<ProviderInfo[]>("GET", "/providers").then(setServed, () => {});
    call<Record<string, string>>("GET", "/fallbacks").then(setFb, () => {});
    call<Account[]>("GET", "/accounts").then(setAccounts, () => {});
  }, []);
  if (served.length < 2) return null;
  const set = async (from: string, to: string) => {
    const next = { ...fb, [from]: to };
    setFb(next);
    await call("PUT", "/fallbacks", next);
    setSaved("Saved");
    window.setTimeout(() => setSaved(""), 1500);
  };
  return (
    <section className="card">
      <h2>When an account hits its limit</h2>
      <p className="muted small">PAI moves the chat to the account you pick here and carries on, with the same history and memory.</p>
      <ul className="plain">
        {served.map((p) => (
          <li key={p.name}>
            <b>{p.name}</b>
            <Usage a={accounts.find((a) => a.name === p.name || a.name.startsWith(p.name + "/"))} />
            <span className="muted small">continue on</span>
            <select value={fb[p.name] ?? ""} onChange={(e) => set(p.name, e.target.value)}>
              <option value="">nothing, stop</option>
              {served
                .filter((o) => o.name !== p.name)
                .map((o) => (
                  <option key={o.name} value={o.name}>
                    {o.name}
                  </option>
                ))}
            </select>
          </li>
        ))}
      </ul>
      {saved && <p className="muted small">{saved}</p>}
    </section>
  );
}

// Usage is an account's share of its 5 hour and 7 day windows spent, as it last
// reported them, and until when it is out of usage.
function Usage({ a }: { a?: Account }) {
  if (!a) return null;
  const pc = (u: number | null) => (u === null ? "?" : `${Math.round(u * 100)}%`);
  return (
    <span className="muted small" title={a.updated ? `seen ${ago(a.updated)}` : "no report yet"}>
      {a.status === "limited" ? "out of usage" : "ok"} · 5h {pc(a.util_five_hour)} · 7d {pc(a.util_seven_day)}
      {a.status === "limited" && a.limited_until > "1" ? ` · until ${new Date(a.limited_until).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}` : ""}
    </span>
  );
}

// Balance spreads new and running sessions over interchangeable models: a
// session on a spent account moves to the pool member with the most room, and
// a new one starts there. A healthy session never moves for more room.
function Balance() {
  const [served, setServed] = useState<ProviderInfo[]>([]);
  const [cat, setCat] = useState<Catalog | null>(null);
  const [b, setB] = useState<BalanceT | null>(null);
  const [err, setErr] = useState("");
  const [saved, setSaved] = useState("");
  useEffect(() => {
    call<ProviderInfo[]>("GET", "/providers").then(setServed, () => {});
    call<Catalog>("GET", "/catalog").then(setCat, () => {});
    call<BalanceT>("GET", "/balance").then(setB, () => {});
  }, []);
  if (served.length < 2 || !cat || !b) return null;
  const models = new Set(b.pool);
  for (const p of served) {
    const id = cat.providers[p.name] ? p.name : p.shape;
    for (const pr of Object.values(cat.presets)) if (pr.provider === id) models.add(`${p.name}/${pr.model}`);
  }
  const save = async (next: BalanceT) => {
    setErr("");
    try {
      await call("PUT", "/balance", next);
      setB(next);
      setSaved("Saved");
      window.setTimeout(() => setSaved(""), 1500);
    } catch (e: any) {
      setErr(String(e.message ?? e));
    }
  };
  const toggle = (m: string) => save({ ...b, pool: b.pool.includes(m) ? b.pool.filter((x) => x !== m) : [...b.pool, m].sort() });
  return (
    <section className="card">
      <h2>Balance across accounts</h2>
      <p className="muted small">With auto on, a chat whose account is spent moves to the pool member with the most room left, and a new chat starts there. A healthy chat never moves, so its prompt cache stays. Pick at least two models that may stand in for each other.</p>
      <label className="row">
        <input type="checkbox" checked={b.mode === "auto"} onChange={(e) => save({ ...b, mode: e.target.checked ? "auto" : "off" })} /> Auto
      </label>
      <ul className="plain">
        {[...models].sort().map((m) => (
          <li key={m}>
            <label>
              <input type="checkbox" checked={b.pool.includes(m)} onChange={() => toggle(m)} /> {m}
            </label>
          </li>
        ))}
      </ul>
      {saved && <p className="muted small">{saved}</p>}
      {err && <p className="error">{err}</p>}
    </section>
  );
}

function sub(cat: Catalog, id: string): number {
  const s = cat.providers[id]?.shape;
  return s === "chatgpt" || s === "claude-code" ? 1 : 0;
}

function Machines() {
  const { graph } = useLive();
  const [join, setJoin] = useState<Join | null>(null);
  useEffect(() => {
    call<Join>("GET", "/join").then(setJoin, () => {});
  }, []);
  return (
    <section className="card">
      <h2>Machines</h2>
      <p className="muted small">
        Run this on any Linux, macOS or Windows machine to make it a node of PAI that your agents can work on. It joins over an encrypted mesh, with
        no inbound port.
      </p>
      {join?.unix ? (
        <>
          <div className="small muted">Linux, macOS</div>
          <Copy text={join.unix} />
          <div className="small muted">Windows</div>
          <Copy text={join.windows ?? ""} />
        </>
      ) : (
        join && <Copy text={`pai install ${join.join}`} />
      )}
      <ul className="plain">
        {(graph?.machines ?? []).map((m) => (
          <li key={m.name}>
            <span className={`dot ${m.connected ? "on" : ""}`} /> <b>{m.name}</b> <span className="muted small">{m.os_arch ?? m.os ?? ""} · {m.connected ? "connected" : "offline"}</span>
            <button className="link small danger" onClick={() => confirm(`Remove ${m.name} from PAI for good?`) && call("DELETE", `/workers/${m.name}`)}>
              remove
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function Keys() {
  const [name, setName] = useState("");
  const [made, setMade] = useState<{ name: string; key: string } | null>(null);
  const [err, setErr] = useState("");
  return (
    <section className="card">
      <h2>API keys</h2>
      <p className="muted small">A key signs in the web app, the TUI (<code>pai login &lt;key&gt;</code>) or any script. It is shown once.</p>
      <div className="row">
        <input placeholder="name, such as laptop" value={name} onChange={(e) => setName(e.target.value)} />
        <button
          className="primary small"
          disabled={!name.trim()}
          onClick={() =>
            call<{ name: string; key: string }>("POST", "/keys", { name: name.trim() }).then(
              (r) => {
                setMade(r);
                setName("");
              },
              (e) => setErr(String(e.message ?? e)),
            )
          }
        >
          Make key
        </button>
      </div>
      {made && (
        <>
          <p className="small">Key {made.name}. Copy it now:</p>
          <Copy text={made.key} />
        </>
      )}
      {err && <p className="error">{err}</p>}
    </section>
  );
}

function Voice() {
  const [read, setRead] = useState(localStorage.getItem(READ) === "1");
  return (
    <section className="card">
      <h2>Voice</h2>
      <p className="muted small">
        In a chat, the mic button dictates into the message box. The wave button turns on voice mode: say “Hey PAI” and your message, and it is sent after a short pause. Voice mode works only while the page is open and the screen is on. This choice is kept in this browser.
      </p>
      <label className="row">
        <input
          type="checkbox"
          checked={read}
          onChange={(e) => {
            localStorage.setItem(READ, e.target.checked ? "1" : "0");
            setRead(e.target.checked);
          }}
        />{" "}
        Read replies aloud in voice mode
      </label>
    </section>
  );
}

function Server() {
  const [s, setS] = useState<SettingsT | null>(null);
  const [msg, setMsg] = useState("");
  useEffect(() => {
    call<SettingsT>("GET", "/settings").then(setS, () => {});
  }, []);
  const newer = s?.version.latest && s.version.latest !== s.version.current;
  return (
    <section className="card">
      <h2>Server</h2>
      <p className="small">
        PAI {s?.version.current ?? "…"}
        {s?.version.latest ? <span className="muted"> · latest {s.version.latest}</span> : null}
      </p>
      {s && (
        <dl className="kv">
          <dt>Branch</dt>
          <dd>{s.version.build.branch}</dd>
          <dt>Commit</dt>
          <dd>{s.version.build.commit}</dd>
          <dt>Tree</dt>
          <dd>{{ yes: "dirty (uncommitted changes)", no: "clean" }[s.version.build.dirty] ?? s.version.build.dirty}</dd>
          <dt>Built by</dt>
          <dd>{s.version.build.builder}</dd>
          <dt>Built</dt>
          <dd>{s.version.build.built}</dd>
          <dt>Go</dt>
          <dd>{s.version.build.go}</dd>
          <dt>Platform</dt>
          <dd>{s.version.build.platform}</dd>
        </dl>
      )}
      {newer && (
        <button
          className="primary small"
          onClick={() => {
            setMsg("Updating; the server restarts on the new release.");
            call("POST", "/settings", { version: s!.version.latest }).catch(() => {});
          }}
        >
          Update to {s!.version.latest}
        </button>
      )}
      {msg && <p className="muted small">{msg}</p>}
    </section>
  );
}

function Copy({ text }: { text: string }) {
  const [done, setDone] = useState(false);
  return (
    <div className="copy">
      <code>{text}</code>
      <button
        className="ghost small"
        onClick={() => {
          void navigator.clipboard?.writeText(text);
          setDone(true);
          window.setTimeout(() => setDone(false), 1500);
        }}
      >
        {done ? "Copied" : "Copy"}
      </button>
    </div>
  );
}
