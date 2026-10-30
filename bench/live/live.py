#!/usr/bin/env python3
"""Live benchmark: the same cross-session tasks through PAI, Claude Code and
Codex, each with its own login. Session 1 is told facts and asked to keep
them for later; session 2, fresh, is asked about them. Per agent it records
wall time, tokens and whether each answer is right.

  python3 bench/live/live.py --agents pai,claude,codex --pai-model claude-code/claude-sonnet-5-5 > live.json

Needs: pai logged in (pai login <key>), claude logged in, codex logged in.
Each agent runs in its own empty directory, so its memory files start empty.
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


def run(cmd, cwd):
    t0 = time.time()
    p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=900)
    return p.stdout, p.stderr, time.time() - t0


def claude(prompt, cwd):
    out, err, dt = run(["claude", "-p", prompt, "--output-format", "json", "--permission-mode", "acceptEdits"], cwd)
    j = json.loads(out or "{}")
    u = j.get("usage", {})
    tok = sum(u.get(k, 0) for k in ("input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens"))
    return j.get("result", out + err), tok, dt


def codex(prompt, cwd):
    out, err, dt = run(["codex", "exec", "--json", "--skip-git-repo-check", "--full-auto", prompt], cwd)
    text, tok = "", 0
    for line in out.splitlines():
        try:
            e = json.loads(line)
        except ValueError:
            continue
        item = e.get("item") or {}
        if item.get("type") == "agent_message":
            text = item.get("text", text)
        u = e.get("usage") or {}
        tok += sum(v for v in u.values() if isinstance(v, int))
    return text or out + err, tok, dt


def pai(prompt, cwd, model):
    body = json.dumps({"model": model, "tools": "default", "context_limit": 200000, "workers": []})
    p = subprocess.run(["pai", "new"], input=body, capture_output=True, text=True, check=True)
    sid = p.stdout.strip()
    t0 = time.time()
    subprocess.run(["pai", "message", sid, prompt], capture_output=True, check=True)
    while True:
        info = json.loads(subprocess.run(["pai", "info", sid], capture_output=True, text=True, check=True).stdout)
        if info["status"] in ("closed", "failed"):
            break
        time.sleep(1)
    dt = time.time() - t0
    tail = subprocess.run(["pai", "tail", sid], capture_output=True, text=True).stdout
    u = info.get("usage", {})
    tok = sum(u.get(k, 0) for k in ("input", "output", "cache_read", "cache_write"))
    return tail, tok, dt


def score(text):
    return sum(1 for _, _, a in FACTS if re.search(re.escape(a), text, re.I)) / len(FACTS)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--agents", default="pai,claude,codex")
    ap.add_argument("--pai-model", default="claude-code/claude-sonnet-5-5")
    a = ap.parse_args()
    results = []
    for agent in a.agents.split(","):
        cwd = tempfile.mkdtemp(prefix=f"bench-{agent}-")
        fn = {"claude": claude, "codex": codex, "pai": lambda p, c: pai(p, c, a.pai_model)}[agent]
        _, t1, d1 = fn(TEACH, cwd)
        text, t2, d2 = fn(ASK, cwd)
        results.append({"agent": agent, "accuracy": score(text), "tokens": t1 + t2, "seconds": round(d1 + d2, 1), "answer": text[-1500:]})
    print(json.dumps(results, indent=2))


if __name__ == "__main__":
    main()
