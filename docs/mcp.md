# PAI's memory over MCP

Claude Code, Claude Desktop, Cursor and other [MCP](https://modelcontextprotocol.io) clients can read and write PAI's long-term memory. There is no second copy of it: these tools work on the same store as PAI's own sessions, the TUI and the web app, and what a client writes shows up in the web app's Knowledge page as it happens.

Nothing is ever deleted. A wrong item is invalidated with a reason, and an edit is a new version that names the one it replaces.

## Connect a client

Log in to your server once with `pai login <api key>`, then let your client start `pai mcp`. The key stays in `~/.pai/login.json`, and the client never sees it.

**Claude Code**

```sh
claude mcp add --scope user pai -- pai mcp
```

`--scope user` makes it available in every project. Without it, Claude Code adds the server to the current project only.

**Claude Desktop, Cursor and others**

```json
{
  "mcpServers": {
    "pai": { "command": "pai", "args": ["mcp"] }
  }
}
```

If your client can't find `pai`, give its full path as the `command`.

### Connecting over HTTP instead

Use this for a client that runs where `pai` isn't installed. `/mcp` is on your server's own URL and needs an API key.

```sh
claude mcp add --scope user --transport http pai https://your-server/mcp --header "Authorization: Bearer <secret>"
```

`<secret>` is only the middle part of the API key. A key looks like `pai_key_<server>.<secret>.<url>`, and the server refuses the whole string.

The client saves that value in its own configuration, for Claude Code in `~/.claude.json`. Prefer `pai mcp` when you can.

## What an API key can do

API keys have no permission levels. A key given to an MCP client can do anything your key can on the server, including running sessions on your workers. Give each HTTP client its own key, so you can revoke just that one:

```sh
pai-server key new claude-laptop
pai-server key revoke claude-laptop
```

## Tools

| Tool | What it does |
|---|---|
| `get_context(task, scope?, k?)` | Pinned items and the items most relevant to a task, each once. Clients are told to call it at the start of a task |
| `recall(query, k?, scope?)` | Search |
| `remember(text, kind?, topic?, scope?)` | Save one self-contained sentence. A repeat is reported as already known |
| `forget(id, reason)` | Mark an item as no longer true |
| `list_memory(kind?, scope?, include_invalid?, limit?)` | List newest first, without searching |
| `get_memory(id)` | One item, whole, as JSON |
| `edit_memory(id, text?, kind?, scope?, topic?)` | Correct an item. This makes a new version with a new id |
| `pin_memory(id, pinned)` | Pin or unpin an item |
| `restore_memory(id)` | Make a forgotten item valid again |
| `memory_feedback(id, up)` | Say whether an item was useful, which moves its ranking |
| `memory_stats()` | Counts and how well recall is doing |

A `scope` on `recall` and `get_context` means that scope plus `global`, because what holds everywhere also holds in a project. On `list_memory` it means exactly that scope. Scopes are case-insensitive: `project:PAI` is `project:pai`. `k` defaults to 8, at most 30, and `list_memory` shows 50 by default, at most 200.

Compaction's summaries of sessions stay out of these tools, since sessions aren't exposed over MCP.

## What outside writes can do

Treat anything connected here as able to write into what your PAI agents read:

- What a client remembers is recalled into PAI's own sessions, on any model. A client can be tricked by a page it reads, so PAI marks these items `[unverified, from an MCP client]` when it shows them to a model.
- A pinned item is recalled in every session, whatever it is about, and only the 10 newest pins are carried. So a client's pin can push out one of yours.
- `memory_feedback` moves how items rank for every session.
- `edit_memory` replaces an item with a new version marked as from an MCP client; the old one stays, and `restore_memory` brings it back.
- Text that looks like a credential is refused, here as everywhere in memory.

You can see what clients wrote, and undo it, on the Knowledge page. Performance counts their writes under the source `mcp`.

## Protocol

`/mcp` speaks Streamable HTTP with JSON answers, one message per POST, and keeps no sessions. It serves protocol revisions `2025-11-25` and `2025-06-18`.

Revision `2026-07-28` isn't served yet. A client that can also speak an older revision falls back to one of those. A client that speaks only `2026-07-28` can't connect.

## Not here yet

- **claude.ai custom connectors.** They reach your server from the internet, and they sign in by OAuth, which PAI doesn't offer, or by a fixed request header, which is a limited beta at the time of writing.
- MCP resources and prompts.
- API keys limited to memory.
