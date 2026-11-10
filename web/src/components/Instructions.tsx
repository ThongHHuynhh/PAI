import { useEffect, useState } from "react";
import { call } from "../api";

type Instruction = {
  worker: string;
  path: string;
  kind: string;
  status: string;
  content: string;
  session: string;
  n: number;
  time: string;
  truncated?: boolean;
};

const kinds: Record<string, string> = { agents: "Agent instructions", skill: "Skill", instruction: "Instructions" };

export default function Instructions() {
  const [files, setFiles] = useState<Instruction[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [query, setQuery] = useState("");
  useEffect(() => {
    let active = true;
    setLoading(true);
    setError("");
    call<Instruction[]>("GET", "/instructions").then(
      (rows) => { if (active) setFiles(rows); },
      (e: unknown) => { if (active) setError(e instanceof Error ? e.message : String(e)); },
    ).finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [refresh]);
  const needle = query.trim().toLowerCase();
  const shown = files.filter((f) => `${f.worker} ${f.path} ${kinds[f.kind] ?? f.kind}`.toLowerCase().includes(needle));
  return (
    <section aria-label="Permanent instructions">
      <p className="muted small">
        Skills, hard rules and instruction Markdown observed through successful PAI reads, writes, edits and patch writes on workers
        since this feature was added. Saved snapshots may no longer be current or active. Directory listings and
        shell-only operations are not tracked until read with PAI's read tool; this is not a complete machine inventory.
        Refresh reloads saved observations.
      </p>
      <p className="muted small">
        Covers AGENTS.md, SKILL.md, CLAUDE.md, instructions.md, rules.md and copilot-instructions.md,
        plus Markdown under skills, instructions or rules directories.
      </p>
      <div className="instruction-tools">
        <input className="search" aria-label="Filter instruction files" placeholder="Filter worker, path or kind"
          value={query} onChange={(e) => setQuery(e.target.value)} />
        <span className="muted small">{shown.length} files</span>
        <button className="ghost small" disabled={loading} onClick={() => setRefresh((n) => n + 1)}>
          {loading ? "Loading…" : "Refresh"}
        </button>
      </div>
      {error && <p role="alert">Could not load instructions: {error}</p>}
      {!loading && !error && shown.length === 0 && <div className="empty small">
        {needle ? "No matching instruction files." : "No instruction-file observations recorded yet."}
      </div>}
      {shown.map((file) => (
        <details className="instruction-file" key={`${file.worker}\0${file.path}\0${file.session}\0${file.n}`}>
          <summary>
            <span className="scope">{kinds[file.kind] ?? file.kind}</span>{" "}
            <strong>{file.worker}</strong>{" "}
            <code>{file.path}</code>{" "}
            <span className="muted small">{file.status}</span>
          </summary>
          <div className="small muted">
            Snapshot from <a href={`#/sessions/${encodeURIComponent(file.session)}`}>{file.session}</a>, record {file.n}.
            {file.time && <> Observed <time dateTime={file.time}>{new Date(file.time).toLocaleString()}</time>.</>}
          </div>
          {file.truncated && <p className="small">Partial snapshot — the recorded content was truncated.</p>}
          <pre className="instruction-content">{file.content || "(Empty file)"}</pre>
        </details>
      ))}
    </section>
  );
}
