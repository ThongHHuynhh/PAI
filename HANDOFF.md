# PAI handoff

State of the project for whoever continues it: what was asked, what is done, how to check it, and what is next. Read `README.md`, `docs/architecture.md` and `AGENTS.md` next.

## What the owner asked for

1. **A personal AI cloud named PAI.** Agents work across platforms and models. A web app connects machines and agents.
2. **Cross-platform.** The web app plus subscription agents: Claude Code (Claude Pro/Max) and Codex (ChatGPT plan), and any API key.
3. **Smart memory.** Compact automatically, save memory automatically, and retrieve smartly: never reload everything blindly. Use learning (RL-style) for retrieval and caching. Knowledge is permanent and never lost.
4. **Memory quality.** Compaction and extraction keep only useful information, dedup, and merge related topics and fields.
5. **Web UI.**
   - Shows which machines are connected and which model runs which conversation, as an interactive real-time graph: every machine is a node connected to PAI.
   - Shows what knowledge and context PAI has learnt.
   - White and minimal by default, switchable to dark, simple to use.
   - Works on desktop, tablet and phone.
6. **Local machine.** An advanced TUI that logs in to the providers and chats through PAI: user → PAI → model → cloud.
7. **Accounts don't matter.** Claude account 2 can take over from Claude account 1.
8. **Clean origin.** The project started as a fork of an open-source agent harness (MIT). Nothing of the old brand may remain, including the TUI animation. A new animation for PAI.
9. **Benchmark.** Compare PAI with Claude Code and Codex on speed, accuracy, recall and tokens, and publish the result.
10. **Push** to the owner's GitHub as a new repo named `PAI`. Done: private repo, module path and `OWNER` placeholders not yet replaced (see Next).

## What is done

| Area | Where | State |
|---|---|---|
| Rebrand, remove the old hosted services (lookup, quick tunnel, data sharing, bug and crash upload), drives, Gemini, Bedrock | whole tree | done; `grep -ri mainplane` finds nothing; old license notices only in `THIRD_PARTY_NOTICES` |
| Join with server URL + device code; tokens carry the URL | `pkg/identity`, `pkg/auth`, `cmd/pai` | done, tested on loopback |
| Plain-http relay for loopback/LAN servers | `pkg/coordinator`, `third_party/tailscale` (patched, vendored, pruned) | done |
| Model catalog embedded; releases from GitHub, signed with own ed25519 key | `pkg/catalog`, `pkg/release` | done; catalog is an offline seed until a release build regenerates it |
| TUI: Orbit animation (machines as nodes), PAI wordmark, violet theme, memory/compaction blocks | `pkg/connector/tui` | done |
| Memory store: SQLite (pure Go), FTS5 BM25 + vectors, rank fusion, utility, recency, pins | `pkg/memory` | done, tested |
| Learnt recall: LinUCB bandit picks 0/3/6/10 items per turn; reward = recalled items used, less cost | `pkg/memory/bandit.go`, `recall.go` | done, tested |
| Usefulness filter (secrets, trivia), topics, merge with lineage, clusters, superseding summaries | `pkg/memory/consolidate.go` | done, tested |
| Agent loop: auto recall per message, `recall`/`remember`/`forget` tools, compaction at 70%, extraction, model-driven consolidation (add/merge/update/retire/skip) | `pkg/harness/memory.go` | done; e2e tested with a scripted mock model |
| Accounts: several accounts per subscription, `PUT /sessions/{id}/model`, `pai model`, automatic failover via `fallbacks` | `pkg/harness`, `pkg/provider` (`Exhausted`), `pkg/server` | done, unit tested |
| Event bus and API: `/events` SSE, `/graph`, `/memory*`, `/fallbacks`, `/catalog`, `/join` with install lines | `pkg/events`, `pkg/server/web.go` | done |
| Web app: Overview (React Flow live graph + activity), Chat, Memory (by topic, lineage, recall log, history), Settings (providers, account names, failover, machines, keys, update); white/dark; desktop/tablet/phone | `web/src`, built into `web/dist`, embedded | done; screenshots checked at 3 sizes, no horizontal overflow |
| Docs, CI (gofmt, vet, race tests, cross-build, web build current), release workflow | `docs/`, `.github/workflows` | done, not yet run on GitHub |
| Benchmark, model-free | `bench/main.go`, `bench/results.json` | done; published as a private claude.ai artifact |
| Live benchmark harness (PAI vs `claude -p` vs `codex exec`) | `bench/live/live.py` | run once on Windows 2026-10-30: `bench/live/results.json`, report `bench/report.html` |

### Benchmark result so far (2,000 facts, model-free)

| | Recall | Memory tokens per turn |
|---|---|---|
| PAI | 99.8% | ~97 |
| Claude Code style (CLAUDE.md loaded whole) | 100% | 40,276 |
| Codex style (AGENTS.md read up to 32 KiB) | 21% | 8,192 |

Outdated facts reached the model in every strategy. With updates applied, PAI drops that to 11%. The learnt policy skips recall on 96% of chatter turns, with no loss on questions. Live run (one run, Sonnet 5.5 for PAI and Claude Code): PAI 5/5 recall, 19,430 tokens, 8.7 s; Claude Code 5/5, 192,620 tokens, 19.9 s; Codex 0/5, 84,887 tokens, 26.7 s. See `bench/report.html`.

## How to check it locally

```bash
go test ./...                       # memory, harness, statefile tests
go vet ./...
cd web && npm ci && npm run build && cd ..
go build -o working_dir/ ./cmd/pai ./cmd/pai-server
```

Run a server on loopback:

```bash
mkdir -p /tmp/pai && echo '{"http":"127.0.0.1:8080","providers":{}}' > /tmp/pai/config.json
working_dir/pai-server up /tmp/pai/config.json
```

Make an API key for it. `pai-server key new` works on an installed server only; for this loopback test, issue one with `auth.Token(auth.Key, identity.Encode(k), secret, "http://127.0.0.1:8080")` the way `cmd/pai-server` does. Then `pai login <key>`, and open http://127.0.0.1:8080/app/.

Connect a provider in Settings, or add an OpenAI-compatible URL to `config.json`, e.g. Ollama:

```json
{"providers": {"ollama": {"shape": "openai-chat", "url": "http://localhost:11434/v1"}}}
```

## Next, in order

1. **Push to GitHub.** Create an empty `PAI` repo.
   - Replace the module path `pai` with `github.com/<owner>/PAI` in every `.go` file and `go.mod`.
   - Set the placeholder `OWNER` in `README.md` and `docs/self-hosting.md`, and the default `Repo` in `pkg/release/release.go`.
   - Build, test, commit, push to `main`.
2. **Release keys.** Run `go run ./pkg/release/keygen`. Add the secret `RELEASE_SIGNING_KEY` and the variable `RELEASE_PUBLIC_KEY` in the repo settings, then tag `v0.1.0`.
3. **Check on real machines.** The sandbox had no IPv6, so a second machine joining the WireGuard mesh and running tools was never exercised end to end. On real Linux, macOS and Windows machines:
   - `pai-server install` with `--url` or a Cloudflare tunnel;
   - `pai install <url> <code>` on another machine;
   - a session that runs `run` on it.
4. **Check the subscriptions.** Sign in with a Claude subscription (`claude-code` shape) and a ChatGPT plan (`chatgpt` shape). Test failover by naming a second account and setting `fallbacks`. `provider.Exhausted` matches limit errors by status (429/529) and words: confirm against the real error text of both plans and adjust `exhaustedWords`.
5. **Repeat the live benchmark.** Run once so far (`bench/report.html`). Run it several times, and on Linux/macOS, where the Codex sandbox can write files its next session reads.
6. **Memory performance.**
   - Search compares every vector (52 ms at 2,000 items).
   - `Remember` scans its scope and kind for duplicates on every insert, and `Related` reads every item, so loading many facts is slow.
   - Fix: keep vectors in memory, per scope, invalidated on write, or use an ANN index.
7. **Better embeddings by default.** The built-in hash embedder needs no model but misses synonyms. Suggest `memory.url/model` in the quickstart when OpenAI or Ollama is connected.
8. **Graph polish.**
   - Machines currently sit on an arc. Consider a force layout when there are many machines.
   - Show which session runs which account, and failover events as edge changes.
9. **Desktop tray app.** Optional, later: a small Tauri app wrapping `pai` and the web app.

## Facts worth knowing

- The server owns the loop. Workers run tools only and never see a model or a credential. Subscriptions run on the server: Claude Code via its unmodified binary, one `claude -p` per step; ChatGPT via "Sign in with ChatGPT". Both are self-hosted only.
- Session files (`sessions/*.state`) are append only and the source of truth. Compaction is a record (`kind: compaction`, `upto`); the context builder replaces covered records with it (`statefile.compacted`).
- Memory never deletes. `Merge` and `Edit` create new versions that name the old ones; old ones are invalid with a reason.
- `web/dist` is committed and embedded by `web/embed.go`. Rebuild it after any change to `web/src`; CI fails if it is stale.
- `pkg/catalog/catalog.json` is an offline seed with no prices. The release workflow regenerates it from models.dev.
- `working_dir/` is gitignored: build there.
