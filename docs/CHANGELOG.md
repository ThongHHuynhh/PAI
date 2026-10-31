# Changelog

One section per release, newest first. A `-beta` tag, cut from the beta branch, ships the section of the release it is a candidate for.

## Unreleased

- TUI paste works in a classic console window: Ctrl+V, Shift+Insert and a right click paste from the clipboard, and a paste the console types in key by key arrives as one paste, so its line breaks no longer send the message.
- TUI message history: Up on the input's first row brings back earlier messages, Down goes forward and back to what you were typing. Kept in `~/.pai/history.jsonl`, the newest 500.
- Install of a server with no URL or tunnel no longer stops at "initializing network": the check now tries the server's own address. Before, `install.ps1 server` (and `install.sh server` with no terminal) failed after 20 s although the server was fine.

- Web chat: a recalled-memories card expands into a readable list (kind, scope, match, what PAI chose, and whether the model used it). A Retrieval bar above the input shows this chat's recall performance, cost, cache hit, recall and reply time, the provider limits Claude Code reports (after its first reply), and a warning after several skipped turns. Each recalled memory has more, less and pin buttons, and a one-time "was this the right memory?" question.
- Web chat: the session list stays beside an open session and folds to a thin rail with a button; the choice is kept in the browser, and on a phone it starts folded and never goes blank.
- Web app: a small footer, "Copyright © 2026 MTR".
- Settings > Server shows where the binary came from: branch, commit, whether the tree was dirty, who built it and when, Go version and platform. `GET /settings` carries it as `version.build`. A field the build did not record reads "unknown". Release builds stamp branch, builder and time; for a local build add `-ldflags "-X pai/pkg/version.Branch=<branch> -X pai/pkg/version.Builder=<user@host> -X pai/pkg/version.Built=<UTC time>"`, and build inside the git checkout for the commit.
- Memory records which recalled items the model used (`used` on each hit of `GET /memory/retrievals`), and `GET /metrics` reports recall latency and the Claude Code usage limits.

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
