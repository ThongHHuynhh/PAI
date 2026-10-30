#!/usr/bin/env python3
"""Live benchmark: the same cross-session tasks through PAI, Claude Code and
Codex, each with its own login. Session 1 is told facts and asked to keep
them for later; session 2, fresh, is asked about them. Per agent it records
wall time, tokens and whether each answer is right.

  python3 bench/live/live.py --agents pai,claude,codex --pai-model claude-code/claude-sonnet-5-5 > live.json

Needs: pai logged in (pai login <key>), claude logged in, codex logged in.
Each agent runs in its own empty directory, so its memory files start empty.
PAI_BIN names the pai binary when it is not on PATH.

Tokens are split three ways, the same for every agent: fresh (input the
model read without a cache hit, cache writes included), cached (cache
reads) and output. Codex counts cached input inside input_tokens, so it is
taken out of fresh rather than added twice.
"""
import argparse, json, os, re, subprocess, tempfile, time

FACTS = [
    ("The staging database for project harbor is Postgres 17 on port 5433.", "What port does the harbor staging database use?", "5433"),
    ("Deploys of project ledger go through Kamal to the Hetzner VPS named atlas.", "Where do ledger deploys go?", "atlas"),
    ("Mai prefers Rust for CLI tools.", "Which language does Mai prefer for CLI tools?", "Rust"),
    ("To rotate the secrets of project pulse, run ./rotate-pulse.sh on machine forge.", "Which machine do I rotate pulse's secrets on?", "forge"),
    ("The backup vault lives on the NAS named quartz.", "Which machine holds the backup vault?", "quartz"),
]
TEACH = "Please remember these facts for future sessions, using whatever memory you have:\n" + "\n".join("- " + f for f, _, _ in FACTS)
ASK = "Answer each question in one short line, from what you know from earlier sessions:\n" + "\n".join(f"{i+1}. {q}" for i, (_, q, _) in enumerate(FACTS))

PAI = os.environ.get("PAI_BIN", "pai")


def run(cmd, cwd):
    t0 = time.time()
    p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, encoding="utf-8", stdin=subprocess.DEVNULL, timeout=900)
    return p.stdout, p.stderr, time.time() - t0


def tokens(fresh, cached, output):
    return {"fresh": fresh, "cached": cached, "output": output, "total": fresh + cached + output}


def claude(prompt, cwd, model):
    out, err, dt = run(["claude", "-p", prompt, "--model", model, "--output-format", "json", "--permission-mode", "acceptEdits"], cwd)
    j = json.loads(out or "{}")
    # modelUsage covers every model the run called, side calls included.
    mu = j.get("modelUsage", {}).values()
    tok = tokens(sum(m.get("inputTokens", 0) + m.get("cacheCreationInputTokens", 0) for m in mu),
                 sum(m.get("cacheReadInputTokens", 0) for m in mu),
                 sum(m.get("outputTokens", 0) for m in mu))
    return j.get("result", out + err), tok, dt


def codex(prompt, cwd, model, config, sandbox):
    cmd = ["codex", "exec", "--json", "--skip-git-repo-check", "--sandbox", sandbox]
    if model:
        cmd += ["-m", model]
    for c in config:
        cmd += ["-c", c]
    out, err, dt = run(cmd + [prompt], cwd)
    text, fresh, cached, output = "", 0, 0, 0
    for line in out.splitlines():
        try:
            e = json.loads(line)
        except ValueError:
            continue
        item = e.get("item") or {}
        if item.get("type") == "agent_message":
            text = item.get("text", text)
        u = e.get("usage") or {}
        c = u.get("cached_input_tokens", 0)
        fresh += u.get("input_tokens", 0) - c
        cached += c
        output += u.get("output_tokens", 0)  # reasoning tokens are part of it
    return text or out + err, tokens(fresh, cached, output), dt


def pai(prompt, cwd, model):
    body = json.dumps({"model": model, "tools": "default", "context_limit": 200000, "workers": []})
    p = subprocess.run([PAI, "new"], input=body, capture_output=True, text=True, check=True)
    sid = p.stdout.strip()
    t0 = time.time()
    subprocess.run([PAI, "message", sid, prompt], capture_output=True, check=True)
    while True:
        info = json.loads(subprocess.run([PAI, "info", sid], capture_output=True, text=True, check=True).stdout)
        if info["status"] in ("closed", "failed"):
            break
        time.sleep(0.5)
    dt = time.time() - t0
    tail = subprocess.run([PAI, "tail", sid], capture_output=True, text=True, encoding="utf-8").stdout
    u = info.get("usage", {})
    return tail, tokens(u.get("input", 0) + u.get("cache_write", 0), u.get("cache_read", 0), u.get("output", 0)), dt


def score(text):
    return sum(1 for _, _, a in FACTS if re.search(re.escape(a), text, re.I)) / len(FACTS)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--agents", default="pai,claude,codex")
    ap.add_argument("--pai-model", default="claude-code/claude-sonnet-5-5")
    ap.add_argument("--claude-model", default="claude-sonnet-5-5")
    ap.add_argument("--codex-model", default="", help="none uses codex's configured model")
    # On Windows, sandbox = "elevated" writes files as a separate sandbox user,
    # which the next session cannot read: -c windows.sandbox="unelevated".
    ap.add_argument("--codex-config", action="append", default=[], help="a codex -c override; repeatable")
    ap.add_argument("--codex-sandbox", default="workspace-write", help="danger-full-access turns the sandbox off")
    a = ap.parse_args()
    fns = {"claude": lambda p, c: claude(p, c, a.claude_model),
           "codex": lambda p, c: codex(p, c, a.codex_model, a.codex_config, a.codex_sandbox),
           "pai": lambda p, c: pai(p, c, a.pai_model)}
    results = []
    for agent in a.agents.split(","):
        cwd = tempfile.mkdtemp(prefix=f"bench-{agent}-")
        _, t1, d1 = fns[agent](TEACH, cwd)
        text, t2, d2 = fns[agent](ASK, cwd)
        results.append({"agent": agent, "accuracy": score(text),
                        "tokens": t1["total"] + t2["total"], "seconds": round(d1 + d2, 1),
                        "teach": {"tokens": t1, "seconds": round(d1, 1)},
                        "ask": {"tokens": t2, "seconds": round(d2, 1)},
                        "files": sorted(os.listdir(cwd)), "answer": text[-1500:]})
    print(json.dumps(results, indent=2))


if __name__ == "__main__":
    main()
