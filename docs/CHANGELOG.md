# Changelog

One section per release, newest first. A `-beta` tag, cut from the beta branch, ships the section of the release it is a candidate for.

## Unreleased

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
