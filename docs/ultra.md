# PAI ULTRA

PAI ULTRA is a second mode beside PAI Classic. Classic stays the default; its behaviour is unchanged apart from a mode label on the TUI status line and the new `/mode` command. ULTRA coordinates a task with one executor, checks the result itself, and keeps durable, portable state so the work can be resumed or handed to another executor, mode or machine.

| | Classic | ULTRA |
|---|---|---|
| Who runs the loop | the PAI server's agent loop | the executor you pick: Claude Code, Codex, or a PAI-server session |
| Needs a server | yes | no, except for the `pai` executor, Classic memory import and handoffs |
| Classic memory writes | yes | none from ULTRA; the `pai` executor's server session behaves as Classic |
| Model calls PAI makes itself | recall grading, extraction, consolidation, compaction | none in the default profile |
| "Done" means | the model stopped | the task's acceptance criteria pass, checked by PAI |
| Memory | automatic recall every turn | explicit, scoped knowledge; candidates apart from verified facts |
| Instructions | AGENTS.md scan | manifest, lockfile, skills, rules, precedence, path scope |

## Choosing a mode

- `pai mode` shows the project's default mode; `pai mode ultra` or `pai mode classic` sets it. In the TUI, `/mode` opens a picker; `/mode ultra` and `/mode classic` act at once.
- The default applies to new sessions only. A session keeps the mode it was made in (`"mode"` in its config; none is Classic). Switching never calls a model and never interrupts running work.
- The default is kept on this machine in `~/.pai/ultra/projects/<id>/mode.json`, not in the repository.
- If ULTRA cannot open (a broken manifest, say), the TUI says why and opens Classic.

## Project setup

```
pai ultra init --profile go-service --executor claude --check "go test ./..."
pai ultra skills                 # installed skills and their state here
pai ultra skills enable go-testing
pai ultra rules new testing      # .pai/rules/testing.md
pai ultra instructions --path pkg/api/x.go --executor codex
pai ultra context --objective "fix the parser"
```

Files:

| Path | Kept in git | What |
|---|---|---|
| `.pai/project.yaml` | yes | the manifest: profiles, skills, rules, precedence, budgets, executor, default criteria |
| `.pai/lock.json` | yes | exact content hashes of every enabled package, and the configuration revision |
| `.pai/rules/*.md` | yes | project rules |
| `.pai/overrides/` | yes | project edits of package rules or skills; the package itself is never edited |
| `.pai/library/` | yes | project-local skills, rules and profiles |
| `~/.pai/ultra/library/` | no | skills, rules and profiles installed on this machine |
| `~/.pai/ultra/cache/` | no | locked package content by hash, so a lock keeps its bytes after a library update |
| `~/.pai/ultra/projects/<id>/` | no | tasks, checkpoints, knowledge, run metrics, mode |

Skills use the Agent Skills `SKILL.md` format (`name`, `description`, optional `license`, `compatibility`, `metadata`). PAI reads `metadata.version`, `metadata.tags` and `metadata.requires` (capabilities such as `shell` or `browser`). `allowed-tools` is recorded but never widens what an executor may do.

Rules are Markdown with optional front matter: `description`, `mandatory` (default true), `activation` (`auto`, `always`, `manual`), `paths` (globs, `**` spans directories), and `key`/`value` to declare a stance that can conflict with another rule's.

States kept apart:

- **installed**: in a library on this machine;
- **enabled**: selected by the manifest, directly or by profile, and locked;
- **selected**: applies to the next execution, by activation and path scope;
- **loaded**: in the context PAI built for the newest run; for files an executor reads on its own (CLAUDE.md for Claude Code, AGENTS.md for Codex) the status is `unknown`, because neither reports it.

Changes are staged, previewed as a diff, and applied all or nothing: the manifest, lock and project files are written through a journal that a crash finishes or discards. An apply refuses invalid configuration and configuration that changed since you began. A library update shows as "update available" and is taken only when you accept it; `pin` holds a version.

Instruction precedence is configured (`override > project > profile > package > global` by default). PAI reports declared-key conflicts (resolved by precedence or not) and word-for-word duplicate lines. It does not judge contradictions in free text: review those yourself.

## Running work

```
pai ultra run "Fix the date parsing in pkg/when" --check "go test ./pkg/when/..."
pai ultra tasks
pai ultra show <task>
pai ultra resume <task>
```

A run:

1. claims the task with a lease (renewed while it runs; a dead owner's lease expires);
2. builds the context by rule: objective, criteria, constraints, applicable mandatory rules, skill descriptions (a skill's file is read by the executor when needed), the checkpoint on resume, and knowledge as data. Each item records why it is there and its estimated tokens. Budgets are soft (bootstrap 1,000, resume 2,000 by default): mandatory items are never cut; overruns are reported;
3. records the executor run as an operation, starts the executor, and streams its progress;
4. checks the criteria itself (commands exit 0, files exist or contain text; review criteria wait for a person: `pai ultra approve`). The executor's "done" is a claim;
5. retries with the failing checks in the next context, up to `max_attempts` (3), then fails;
6. checkpoints after every attempt.

The configuration revision is pinned per attempt. Applying changes affects the next attempt; the TUI shows the running task's revision, the latest, and when a restart is needed.

Executors:

| | Claude Code | Codex | PAI |
|---|---|---|---|
| Interface | `claude -p --output-format stream-json` | `codex exec --json` | a PAI-server session, mode "ultra" |
| Native resume | `--resume` | `codex exec resume` | the same session |
| Usage | reported | reported | reported |
| Permissions | its own settings; `executor.settings.claude.permission_mode` | its sandbox; `executor.settings.codex.sandbox` | the server's workers |
| Tool interception by PAI | no | no | yes (server runs tools) |
| Reports loaded instruction files | no | no | no |

Cancel and timeout stop the executor's whole process tree.

## Checkpoints, resume and recovery

A checkpoint (version 1) holds the contract, decisions, git HEAD and branch, a binary patch of tracked changes, copies of untracked files (2 MB each, 32 MB total; larger ones are listed as not recoverable), verification results with environment fingerprints, failed attempts, blockers, the next action, unknown operations, owner and revision. `pai ultra restore <task> <dir>` rebuilds it in a new git worktree.

On resume, PAI compares the workspace with the newest checkpoint. Unchanged, and the executor supports it: the native session is reused. Otherwise a fresh executor starts with a bounded handoff that names the changed files.

A run whose owner died is recovered (`pai ultra recover`, or at TUI start): unfinished operations become unknown and the task is blocked until a person reconciles them (`pai ultra reconcile`). PAI never replays a possibly completed side effect on its own, and does not promise exactly-once execution.

Delegation (`pai ultra delegate`) runs a child task in its own git worktree from a recorded base; `pai ultra integrate` applies only the accepted checkpoint.

## Moving work between modes

- **ULTRA to Classic** (`pai ultra handoff <task> --to classic`, or `/mode` → Continue in Classic): at a safe boundary, PAI checkpoints the task, creates an empty Classic session, and saves the handoff text. Nothing is sent to a model: the TUI puts the handoff in the input and you send it. The task is marked handed off and both are linked.
- **Classic to ULTRA** (`pai ultra handoff --from-classic <session>`, or `/mode` → Continue this session in ULTRA): PAI builds a task contract from the session's messages, by rule, checkpoints the workspace and links both. The task does not start until you run it.

Both say first what does not carry over (tool history, model context, Classic memory recall, criteria enforcement in Classic).

## Knowledge

`pai ultra memory` keeps preferences, decisions, facts and hypotheses for the project, with scope, sources, verification, file dependencies (a changed file makes an item stale), supersession and invalidation. A model's conclusion is always a candidate; only you or a check with evidence verify, and only you or a check retire verified knowledge. Nothing is deleted. Retrieval is SQLite full-text search with exact filters; there is no embedding service.

Classic memory is never imported automatically. `pai ultra memory import` previews; `--apply` imports as candidates with provenance (`classic:<id>`).

Extraction and consolidation are off (`memory.extraction`, `memory.consolidation` in the manifest exist for a later release and do nothing yet).

## The TUI

`pai` opens ULTRA when it is the project's default. One line at the bottom, as in Classic: `◆ ULTRA`, the views (`Work Skill Ins Mem Runs Cont`), the status message, then what needs attention only when it does (staged edits, a hand-edited manifest, a restart, offline), the model, the context PAI sends and the newest run's tokens.

| View | Alt+ | Does |
|---|---|---|
| Workspace | 1 | conversation (replies rendered as Markdown); while a task runs, Classic's spinner over the input; then ✓ and the time when PAI's checks passed, or ✗ and why. Check details are in Runs |
| Skills | 2 | browse, search by name/tag/source, inspect, enable (Space), activation (m), scope (s), update (u), pin (P), override (o), profile (r) |
| Instructions | 3 | every source with owner, revision and why it applies; new (n), edit project files (e), override or copy external ones (o), conflicts (c) |
| Memory | 4 | candidates, verified, stale; verify (v), invalidate (x), add (n), Classic import preview (i) |
| Runs | 5 | tasks, owners, checkpoints; resume (r), checkpoint (c), approve (A), reconcile (u), hand off (h), recover (R) |
| Context | 6 | the effective context: items, reasons, tokens, warnings |

Keys: Tab/Shift+Tab move focus, arrows or j/k move, Enter inspects, `/` searches, Esc closes or returns, `[` `]` change view in a list, `?` or F1 shows the view's keys. In Skills and Instructions, `p` previews staged changes, `a` applies, `d` discards. Editing opens an in-app editor: Enter adds a line, Ctrl+S stages, Esc cancels. Management views read and write local files only; they never call a model. Below 110 columns the detail replaces its list.

## Measuring

`go run ./bench/ultra -modes bare-claude,ultra-claude,bare-codex,ultra-codex,classic -n 3` runs one fixture task each way with the same model, starting commit and permissions, checks the result itself, and writes `bench/ultra/results.json`: success, tokens (reported, per attempt), elapsed time, and a model-free crash-recovery check. Per-attempt metrics for real work are in `~/.pai/ultra/projects/<id>/runs.jsonl` (`pai ultra metrics`).

## Compatibility

- Classic is unchanged and the default. Sessions without a mode are Classic. The only shared change is an optional `"mode"` field in a session's config and the harness's create request; older servers ignore it.
- ULTRA state is apart from Classic's. ULTRA itself never writes Classic memory or sessions, with two exceptions: a handoff you confirm creates an empty Classic session; and the `pai` executor runs each attempt as a session on the PAI server (labelled mode "ultra"), where Classic's own recall, extraction and compaction run as they do for any session and may write Classic memory. The `claude` and `codex` executors touch neither.
- To roll back: `pai mode classic`, or delete `~/.pai/ultra/projects/<id>/mode.json`. Removing `.pai/` and `~/.pai/ultra` removes ULTRA entirely.

## Known limits

- Executors run with their own permissions; PAI does not intercept native tool calls, so a mandatory rule is given to the executor before it acts but not enforced on each action.
- Whether Claude Code or Codex loaded their own instruction files is unknown to PAI.
- `project.yaml` comments are not kept when PAI rewrites it.
- Checkpoints record staged and unstaged changes together; a restore leaves them unstaged. Untracked symlinks and files over the size limits are not recoverable.
- The `pai` executor needs a worker on the machine that holds the workspace.
- Model review of review criteria is not implemented: a person approves them.
