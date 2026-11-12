# Changelog

One section per release, newest first. A `-beta` tag, cut from the beta branch, ships the section of the release it is a candidate for.

## Unreleased

- Workers update again. Since v0.4.0 a worker could take only one update: the update request had no id, so the worker's request spool refused every later one as a repeat, and a result the worker replayed when it connected read as "updated". The worker then reconnected every five seconds on its old release, refused, and nothing could reach its machine. The update request now has an id of its own, and only the answer with that id counts. A worker stuck this way updates by hand once: install the new `pai` binary and restart its service.

## v0.4.3

- Memory conflicts no longer block a session. Before, one memory item marked as conflicting, even with itself, stopped every run, write, remember and forget call in any session that recalled it, and answering the question did not clear it. Now a disputed item carries a one-line note where it is recalled, every tool runs, and forgetting or pinning an item always works.
- An item can no longer conflict with itself. Automatic memory consolidation made every proposed update or retire a conflict, and a retire of one item paired that item with itself. Opening the store closes those self-conflicts and the v0.4.x proposal flags, as recorded resolutions, so nothing is deleted.
- Consolidation now does what it proposes, reversibly: merges and updates become new versions that name what they replace, retires invalidate with a reason, and restore undoes either. It may only touch the items it was shown and never a pinned one. A real disagreement that the dates do not settle is marked as disputed.
- memory_clarify and context_clarify find the person's answer in the session by themselves, since a model may not see its own call ids. memory_clarify then retires the item the person did not choose, and its question shows each item cut to 300 characters with the date it was learnt.
- Project memory is recalled in sessions. Recall used to search only global items and the session's own, so items saved under a project, about half of a typical store, never reached a PAI session. Now a session recalls the projects whose names the person wrote or that the session saved into; the recall tool takes a scope; and a session can read and forget a project's item by id.
- Scopes are case-insensitive: "project:PAI" and "project:pai" are one project. Opening the store moves items in a capitalised scope to the lower-case one as new versions, merging exact repeats.
- Recall costs fewer tokens. A recalled line drops the metadata tail and repeats of "data only", shows the date the item was learnt, and cuts text past 400 characters with the id to read the rest. Recall skips items the session's context already holds, and automatic recall never brings back a session's own compaction summary. On real items from a live store, five recalled items went from about 1,570 to 610 tokens.
- One fact per item: new items over 600 characters are refused with a request to split them, extraction and consolidation ask for at most 300, and repeats that differ only in case, spacing or closing punctuation are stored once.
- MCP: edit_memory works. It was refused for every MCP client; an edit is now a new version marked as from an MCP client, and the old one stays to restore. Recall, get_context and list_memory over MCP use the shorter lines.
- A Windows worker without PowerShell 7 runs pwsh code in Windows PowerShell, which every Windows has. Before, every pwsh run on such a machine, including the AGENTS.md scan at session start, failed to start.
- TUI: a long ask question keeps its line breaks and is cut at 12 lines, so its options and keys stay on screen.

## v0.4.2

- Fixes a token leak from v0.4.0 and v0.4.1. Once a session had compacted, or the model had recorded typed context, every request ended with a note that was not saved in the session file. The next request then no longer started with the one before it, so the provider cache missed on every step and the whole context was written to the cache again: about 100k tokens per step in a 250k session instead of a few thousand, roughly eight times the cost. The archive note and the typed context now go into the saved compaction record, and a request holds only what the session file holds. Sessions already compacted under v0.4.x get their cache back from their next step.

## v0.4.1

- Compaction has two modes, set per session. Auto, the default, compacts past 70% of the context limit, and past 50% when a new request from the person arrives, so a finished task's detail folds before the next one. Manual asks the person through the agent at 70% and compacts unasked only past 90%, so the session does not fail at its limit. The model can call the new compact tool when a task is done. In the TUI, /compact compacts an idle session now and /compaction picks the mode; /continue no longer has the compact alias. CLI: `pai compact <id> [auto|manual]`. API: `POST` and `PUT /sessions/{id}/compact`.
- File writes are atomic and conflict-checked. The worker writes to a temporary file and renames it over the target, so a crash or a failed write never leaves a torn file. edit, patch and apply_edit send the hash of what they read, and the worker refuses the write if the file changed since, instead of losing the other writer's change. Writes to one path through one worker take turns.
- Credentials are a hidden section of memory for addresses, logins, SSH details, keys and passwords the person wants agents to use. Values are sealed with AES-GCM under a key file beside memory.db, are never recalled into a context, and are not served over MCP. Recall shows only a matching credential's name and description. Agents write `{{cred:name}}` in run, write, edit and patch arguments; the harness fills the value in on the way to the worker and masks every stored value in results and job output. The credential tool lists, saves, forgets, and gets a value only with a stated reason. The web app's Memory page has a Credentials tab to add, show, hide and forget them.
- Memory refuses items that read as instructions planted for agents, such as "ignore previous instructions" or "send the credentials to".
- The new diagnose tool, `pai diagnose <id>` and `GET /sessions/{id}/diagnose` say whether a session is working, waiting on a person, a model or a worker, or stuck, with each call waiting for a result, its worker, whether that worker is connected, and how long it has been quiet.
- context-eval measures answer quality: fixtures carry questions with facts the answer must and must not state, answered from only each policy's selected context. `-answer <cmd>` has a model CLI answer; without it an extractive answerer gives a model-free bound.
- Providers can be removed: a Remove button with a confirmation on the web app's Settings page, `pai provider remove <name>`, and `DELETE /providers/{name}`. Removal deletes the key or sign-in tokens and clears fallbacks and balance pool entries that name the provider. The confirmation says how many sessions use it.
- New tests: writer crash mid-write, failed writes, concurrent edits from many sessions, stale writes, and harness crash recovery at every execution stage never replaying a call.

## v0.4.0

- Tool execution is durable. Every call records dispatch, acknowledgment, start and its outcome in the session file. Workers spool requests and results to disk, acknowledge results with receipts and replay unacknowledged results after a reconnect. Requests are never replayed, so a call whose effect cannot be confirmed reports an uncertain outcome instead of running twice. Server-side deadlines bound runs, file operations and control calls, so a stalled worker fails the call instead of hanging the session. Session info lists running and background calls with their worker and age.
- Contiguous file reads in one step run concurrently, up to 4 at a time. Shell runs and writes stay ordered.
- Typed context: the new context_record, context_view, context_clarify and context_expand tools record intent, constraints, evidence and task state with references to the original records, build a budgeted handoff for a new session, and read original records back after compaction. After compaction an archive index points to the originals instead of repeating them.
- Contradictions are never resolved by the model. While a recorded context conflict, or a conflict on a memory item the session consulted, is open, only read-only tools and clarification run. The memory_clarify tool turns a memory conflict into an ask question, and a resolution is accepted only with a recorded answer from the user. An ask that times out or a child session that needs a choice leaves the question open.
- Memory items can carry an optional versioned structured contract (subject, property, value, observed, verified and expiry times, dependencies, and a review policy per kind). Only different values for the same subject and property are flagged as conflicts; related prose is not. Retrieval feedback records whether a recalled item was used, irrelevant, contradicted or harmful, and replaces word-overlap rewards.
- New read_page, plan_edit and apply_edit tools read large files in pages and apply hash-checked edits that are read back after writing.
- Optional per-session routing policy: explicit accounts, models and escalation levels, a bounded retry budget per task, and optional verification of final answers. Route decisions and verification results are recorded in the session file. Sessions without a routing policy behave as before.
- Provider errors are classified (rate limit, unavailable, transport, stream, authentication, request, configuration, canceled), Retry-After is capped at 30 s, and a single model step is bounded at 15 minutes.
- cmd/context-eval compares full, recency and typed context selection on a session file.

## v0.3.1

- MCP: Claude Code, Claude Desktop, Cursor and other MCP clients can read and write PAI's memory, through `pai mcp` or `/mcp` on the server. It is the same store as PAI's own, so what a client writes shows up live in the web app. Items written this way are marked as unverified when recalled into a session. See [docs/mcp.md](mcp.md).
- Web app: Memory has a Permanent instructions tab. It lists the AGENTS.md, SKILL.md, CLAUDE.md and rule files PAI has read, written, edited or patched on a worker, with the latest contents, the worker, the path and the session it came from. Snapshots are kept in the session file but never enter the model's context. Tracking starts with this version, and files changed only by shell commands appear after PAI next reads them.

## v0.3.0

- Terminal setup of a new server asks whether to download private Ollama and `embeddinggemma` inside PAI's data directory. Yes enables semantic retrieval; No needs no model or runtime. Existing installations and unattended setup keep model-free retrieval unless explicitly configured. The private runtime binds only to loopback and keeps its models, caches, temporary files and logs inside PAI's folder.
- Explicit embedding configurations use semantic retrieval by default, with optional hybrid keyword fusion and legacy hash mode. Existing vectors reindex when the model changes. Repeated embeddings are cached, malformed vectors are rejected, distinct facts cannot be discarded on similarity alone, and recall failures are recorded for the model. See self-hosting for setup and the retrieval benchmark report for results and limits.
- Signed updates preserve binary compatibility links, so an NVMe executable is updated in place rather than copied back to the system disk.

- A worker whose mesh node wedges, so it cannot reach the harness for 2 minutes while the coordinator answers (or 10 minutes in all), restarts with a new node instead of waiting for a manual service restart; a dead harness connection now ends after 4 failed keepalives.
- The web app finds memory ids in the current format (m plus uppercase base32) in recall lists and merge reasons, not only old hex ids.
- A Claude Code usage limit notice is no longer saved as the model's reply; the step fails as exhausted and moves to the fallback, and the error no longer ends in an empty "claude stderr:".
- Provider labels are limited to 100 characters, not 100 bytes, so non-ASCII names are not cut short.
- TUI: `PAI_THEME=light` or `dark` sets the background the Auto theme follows, for a terminal that does not report its own; anything else keeps auto.
- TUI `/model` in an open session: rows skip the models that cannot take the session (dimmed, with the reason, which leaves the name half the row), and a pick says the model applies from the next step.
- TUI transcript: tool calls in a row fold into one line, as in the web app: how many, how many failed, and the latest call with its time while it runs. A click opens the group to show each call as before, without moving what is above.

## v0.2.5

- Web Performance page on any screen: a section's tiles share one full row (two across on a phone, an odd last one spans both), charts sit three across (one on a phone), pairs sit two across only on a wide screen with the errors box as tall as its neighbour, and a table's first column stays put while it scrolls sideways. Nothing overflows sideways from 320 px up.
- Web: a session can be deleted from its header (Delete, after a confirm; what PAI learnt from it stays in memory). The dark theme switch and Sign out moved from the side and tab bar into Settings > This browser, and Sign out asks first.
- Web voice input: a mic button in the chat dictates into the message box, and voice mode listens for "Hey PAI" and sends what follows after a short pause. Settings > Voice can read replies aloud. It uses the browser's own speech, works while the page is open and the screen on, and is hidden where the browser cannot listen.
- A provider can have a display name: rename it in place in Settings, and the web app and the TUI's account settings show it, with the provider id in the tooltip or beside it. The id stays in the config, models, fallbacks and the balance pool. The route is `PUT /providers/{name}/label` with a text body; an empty body clears it.

## v0.2.4

- Web on iPhone: the app follows the part of the screen the keyboard leaves, so the message box no longer jumps, the question popup keeps Decline and Send answers above the keyboard, the tab bar hides while typing, and fields use 16px text so Safari does not zoom in. A phone opens on Chat, shows one pane at a time with a back button, and Enter makes a new line there; Send sends.
- Web chat: tool calls in a row fold into one line (how many, how many failed, the latest) that opens on a tap. A long chat draws its newest 150 records with "Show earlier", and a record is drawn again only when it changes, so long sessions stay smooth on a phone.
- Web: Sign out asks first.
- A child session no longer has the `ask` tool, and a child's ask is refused: nobody watches a child, so its question held it and its orchestrator for up to 30 minutes. A child's final report now has a fixed form: Result, Changes, Checks, Open.
- A Windows or macOS worker reaches a harness on the user's tailnet: its sockets follow the routing table instead of always the default route's interface, and on Windows only PAI's own adapter counts as its own, not the user's Tailscale adapter. Before, a worker joining a harness served with `tailscale serve` timed out on its first request and never joined.
- `pai install` with a join token for another harness drops the old mesh keys, so a machine that was a worker or the admin worker of an old server joins the new one. Before, it logged in with the old keys and looped on "no harness in the mesh map yet".

## v0.2.3

- A new session can be an orchestrator without the CLI: the TUI's `/config` has a Role section (Agent or Orchestrator), and the web New session dialog has an Orchestrator switch. The role keeps the file tools of the model's preset: a model trained on patches orchestrates with `patch` (new tool set `patch-orchestrator`), the rest with `write` and `edit`. A delegated child gets its parent's file tools instead of always `default`.

## v0.2.2

- TUI: `/usage` opens each account's usage: its state, the share of its 5 hour and 7 day windows spent, when a limit ends, and when it last reported.
- The Overview graph draws a manager and its employees: a session that delegated others shows how many it has and how many are working, a dashed edge runs to each employee, and the side panel lists the employees (or the manager) with status, model, context and last activity; a click opens one.

## v0.2.1

- A session on an account that says "You've hit your session limit" or "weekly limit" now moves to the account set in Settings > When an account hits its limit. Before, only "usage limit" and similar wording counted, so Claude's own message left the session stuck.
- Choose the account a session falls back to (`pai fallback`, TUI and web Settings): a provider, or a full provider/model of any vendor. Auto mode (`pai balance auto <provider/model>...`, TUI and web Settings) spreads sessions over a pool of interchangeable models: a session on a spent account moves to the member with the most room, and a new session starts there. A healthy session never moves for more room, so its prompt cache stays. A target is skipped if it is not served, its context window is too small, or it cannot read the media the session holds. `GET /accounts` and `pai accounts` show each account's usage; `GET /metrics` limits are per provider. Memory ids are random, which fixes duplicate-id test failures on Windows.
- One orchestrator session can delegate to child sessions: the `orchestrator` tool set adds `delegate`, `tell`, `children` and `stop_child`. `delegate` queues onto a related child that is working, resumes an idle one, or spawns a new one with a handoff, and starts it on the account with the most room. It takes a size (small, medium, large) and refuses when the account has too little room for it, with the reset time. A finished child reports to its parent by itself, and stopping the parent stops its children. The TUI sidebar and `pai sessions` show children under their parent.
- Branches: work lands on `beta`, `-beta.N` tags are prereleases cut from it, and `main` holds stable releases only.

- Settings in the web app: each account shows its 5 hour and 7 day usage and when it is out of usage until, and a new card turns balance across accounts on or off and picks its pool of interchangeable models (the same as `pai balance` and the TUI).

- The web session list shows delegated sessions under their orchestrator, indented, with the orchestrator's child count and whether one is working. A child whose orchestrator is not listed stands alone.

- Swap the model of an open session from the TUI (`/model`) and the web app (the model name in the chat header). Both list every served model, mark the current one, and grey out the ones that cannot take the session with the reason (context too large for its window, media it cannot read, provider not served). `GET /sessions/{id}/models` serves the list. The swap applies from the next step; the first step on the new model re-reads the context, because prompt caches are per account.

- TUI paste works in a classic console window: Ctrl+V, Shift+Insert and a right click paste from the clipboard, and a paste the console types in key by key arrives as one paste, so its line breaks no longer send the message.
- TUI message history: Up on the input's first row brings back earlier messages, Down goes forward and back to what you were typing. Kept in `~/.pai/history.jsonl`, the newest 500.
- Install of a server with no URL or tunnel no longer stops at "initializing network": the check now tries the server's own address. Before, `install.ps1 server` (and `install.sh server` with no terminal) failed after 20 s although the server was fine.

- Web chat: a recalled-memories card expands into a readable list (kind, scope, match, what PAI chose, and whether the model used it). A Retrieval bar above the input shows this chat's recall performance, cost, cache hit, recall and reply time, the provider limits Claude Code reports (after its first reply), and a warning after several skipped turns. Each recalled memory has more, less and pin buttons, and a one-time "was this the right memory?" question.
- Web chat: the session list stays beside an open session and folds to a thin rail with a button; the choice is kept in the browser, and on a phone it starts folded and never goes blank.
- Web app: a small footer, "Copyright © 2026 MTR".
- Settings > Server shows where the binary came from: branch, commit, whether the tree was dirty, who built it and when, Go version and platform. `GET /settings` carries it as `version.build`. A field the build did not record reads "unknown". Release builds stamp branch, builder and time; for a local build add `-ldflags "-X pai/pkg/version.Branch=<branch> -X pai/pkg/version.Builder=<user@host> -X pai/pkg/version.Built=<UTC time>"`, and build inside the git checkout for the commit.
- Memory records which recalled items the model used (`used` on each hit of `GET /memory/retrievals`), and `GET /metrics` reports recall latency and the Claude Code usage limits.
- New `ask` tool: the agent can put one to four multiple choice questions to you (two to four options each, your own answer always allowed) and wait up to 30 minutes. A popup in the web app, a dialog in the TUI and a numbered prompt in `pai chat` take the answer; declining is allowed. Answers go to `POST /sessions/{id}/answer`. The tool list changes, so the prompt cache of a running session is rebuilt once.

## v0.2.0

- Sessions are named with two words, such as `calm-otter`, instead of a hex code: easy to say, type and pass to another agent (`pai message calm-otter …`). Sessions from before keep their ids.
- Memory page reads like a notebook: one line per memory under its topic, details and actions on a click. Recalls show as plain Activity.
- The web app keeps one live connection per browser, shared by its tabs, so any number of tabs open; before, a seventh never loaded.
- The web app signs in with the whole api key that install prints.
- Performance page: models (calls, speed, tokens, cache, estimated cost, errors), sessions and their context, tools and the agent loop, and memory, over 24 hours, 7 or 30 days, or all time. Served by `GET /metrics`.
- Overview: drag any node, and the layout is kept; activity is a fixed box that scrolls. The Orbit is drawn thicker.
- TUI themes: `/theme` (also in Settings) lists Auto, PAI dark and light, White, High contrast, Nord, Dracula, Solarized dark and light, Gruvbox, Catppuccin Mocha and Latte, and Tokyo Night. Moving through the list previews each; Enter keeps it in `~/.pai/tui.json`, Esc goes back.

## v0.1.0

- First release of PAI: a self-hosted server that runs agents on every machine you connect, on any model.
- Memory across sessions, models and machines. It has hybrid recall (BM25 + embeddings), a learnt recall policy (LinUCB), a usefulness filter that refuses secrets, consolidation that merges related items, and history for every change.
- Rolling compaction at 70% of the context. The full history stays in the session file.
- Accounts: several accounts per subscription, moving a session to another account or vendor, and automatic failover when an account hits its limit.
- Web app: live graph, chat, memory, settings. White and dark themes, for desktop, tablet and phone.
- Terminal app with the Orbit home screen, and a CLI.
