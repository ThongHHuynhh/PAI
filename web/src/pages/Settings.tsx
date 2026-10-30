import { useEffect, useState } from "react";
import { call, type Catalog, type ProviderInfo } from "../api";
import { useLive } from "../live";

type Join = { join: string; unix?: string; windows?: string };
type LoginState = { state: string; error?: string; text?: string; url?: string; code?: string; paste?: boolean };
type SettingsT = { version: { current: string; latest?: string } };

export default function Settings() {
  return (
    <div className="settings-page">
      <header className="page-head">
        <h1>Settings</h1>
      </header>
      <Providers />
      <Machines />
      <Keys />
      <Server />
    </div>
  );
}

function Providers() {
  const [cat, setCat] = useState<Catalog | null>(null);
  const [served, setServed] = useState<ProviderInfo[]>([]);
  const [pick, setPick] = useState("");
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
        const st = await call<LoginState>("POST", `/providers/${pick}/login`, { shape: p!.shape });
        setLogin({ name: pick, st });
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
        Use a Claude or ChatGPT subscription through their official sign-in, on this self-hosted server only, or any API key. Ollama and any
        OpenAI-compatible server work too: add them in the server config with shape <code>openai-chat</code> and their URL.
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
        <select value={pick} onChange={(e) => setPick(e.target.value)}>
          <option value="">Connect a provider…</option>
          {ids.map((id) => (
            <option key={id} value={id}>
              {cat!.providers[id].name}
              {sub(cat!, id) ? " (subscription)" : ""}
            </option>
          ))}
        </select>
        {pick && !subscription && <input type="password" placeholder="API key" value={key} onChange={(e) => setKey(e.target.value)} />}
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
