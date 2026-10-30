# How PAI works

## Parts

| Part | Where | Does |
|---|---|---|
| `pai-server` | one always-on machine | runs every agent loop, holds credentials, sessions and memory, serves the API, the web app, the mesh coordinator and relay |
| `pai` | every machine | the worker that runs tools (shell, files, a JS runtime with browser control), the TUI, and the CLI |
| web app | the browser, served by the server at `/app/` | live graph, chat, memory, settings |

The server owns the loop. A worker never sees a model or a credential. It only runs the tool calls the server sends it, over a WireGuard mesh the server coordinates. Models are interchangeable because the state is the server's: the session's `.state` file and the memory store.

## A turn

1. A message arrives (web, TUI, CLI) and is appended to the session's `.state` file.
2. **Recall.** The memory store builds a feature vector for the turn: is it a question, how long is it, how full is the context, how much is in memory. A LinUCB bandit picks how many items to recall: 0, 3, 6 or 10. The items are found by hybrid search: BM25 over FTS5 and cosine over embeddings, fused by rank, then weighed by each item's learnt utility and its age. Pinned items always come. The recall is written to the file as a `memory` system record, so the same file always builds the same request and the provider's prompt cache holds.
3. **Compaction.** If the last step's prompt passed 70% of the limit, the oldest records are summarized by the session's model and replaced in the context by a `compaction` record. The cut never splits a call from its result, and the newest quarter of the context stays verbatim. The summary and the lasting items found in it go to memory.
4. **Step.** The context is compiled for the provider (Anthropic, OpenAI Responses, Chat Completions, ChatGPT plan, or Claude Code), streamed back, and tool calls run on the named machines or, for `recall`, `remember` and `forget`, on the server.
5. **Failover.** If the provider's account is out of usage, the session moves to the provider the config's `fallbacks` names, and the step runs again there.
6. **Settle.** When the session closes, recalled items whose distinctive words appear in the model's reply count as used. The share used, less a cost per item, is the bandit's reward and moves each item's utility.
7. **Extract and consolidate.** Every dozen new records, the session's model lists lasting items. Each is checked for usefulness and secrets, then compared with related items by topic and meaning. The model adds, merges, updates, retires or skips it. A throttled background pass merges clusters of related items.

## Never lost

- Session files are append only. Compaction changes what the model sees, not what is kept.
- Memory items are never deleted. Merged, edited, superseded and retired items stay, invalid, with why, and the new version names them.
- The event stream (`GET /events`) and the graph (`GET /graph`) are views. The files are the truth.

## API

Every route but `/app/`, `/id` and `/join` takes `Authorization: Bearer <api key>`.

| Route | |
|---|---|
| `GET/POST /sessions`, `GET /sessions/{id}`, `GET/POST /sessions/{id}/records` | sessions and their records; records long-poll with `?wait=` |
| `PUT /sessions/{id}/model`, `PUT /sessions/{id}/workers`, `/stop`, `/retry` | change or steer a session |
| `GET /events` | Server-Sent Events: records, workers, memory |
| `GET /graph` | the server, machines, sessions and memory stats |
| `GET /memory`, `POST /memory`, `PATCH /memory/{id}`, `/invalidate`, `/restore`, `/feedback`, `POST /memory/consolidate`, `GET /memory/retrievals`, `GET /memory/stats` | memory |
| `GET /providers`, `PUT /providers/{name}`, `/providers/{name}/login`, `GET/PUT /fallbacks`, `GET /catalog` | models and accounts |
| `GET /workers`, `GET /join`, `POST /keys`, `GET/POST /settings` | machines, keys, updates |
