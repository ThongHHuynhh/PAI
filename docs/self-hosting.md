# Self-hosting PAI

PAI is one server you run for yourself, plus a small client on each machine. The server holds every credential, every session and the memory. Nothing is sent anywhere but to the model providers you connect.

```
 browser, TUI, CLI ──https──► your URL ──► 127.0.0.1:8080 pai-server
 machines (pai)    ──────────────┘             │ sessions/  memory/  config.json
                    WireGuard mesh ◄───────────┘ coordinator + relay
```

## Install the server

```bash
curl -fsSL https://github.com/ThongHHuynhh/PAI/releases/latest/download/install.sh | sh -s -- server quickstart
```

It installs `pai-server` as a service, makes this machine the worker `admin`, prints an API key named `default` and the line that connects other machines, then opens the quickstart.

The server keeps its state in `/var/lib/pai-server` on Linux, `/Library/Application Support/pai-server` on macOS and `%ProgramData%\pai-server` on Windows:

| Path | What |
|---|---|
| `config.json` | providers, fallbacks, links, memory embedder, URL |
| `sessions/*.state` | every session, append only, the source of truth |
| `memory/memory.db` | what PAI learnt (SQLite); rebuildable from sessions |
| `auth.json`, `harness.key` | API key hashes and the server's identity key |

Back up that directory and you have everything.

## Reach the server

The server listens on loopback only. By default it is reachable from this machine alone, which is enough to try it. To use it from other devices, give it a URL in one of these ways:

- **Your own reverse proxy** (Caddy, nginx, Tailscale serve) carrying `https://pai.example.com` to `http://127.0.0.1:8080`, with WebSocket upgrades:
  ```bash
  pai-server url https://pai.example.com
  ```
  Caddy needs one line: `pai.example.com { reverse_proxy 127.0.0.1:8080 }`.
- **A Cloudflare tunnel** you made in the Cloudflare dashboard, with a public hostname pointing at `localhost:8080`:
  ```bash
  pai-server tunnel https://pai.example.com <cloudflared token>
  ```

The URL goes into every API key and join line, so set it before you connect other devices. If it changes later, join machines again with `pai-server join`.

## Connect machines

```bash
pai-server join          # prints the line: <server URL> <device code>
pai-server join cycle    # a new device code; machines already joined stay
```

On each machine, run the printed install line. A machine joins with a password exchange (CPace) on the device code, so the code never travels. Machines see the server; two machines see each other only when `"links": [["laptop", "desktop"]]` in the config names them.

## Models and accounts

Connect providers in the web app (Settings) or the TUI (`/providers`):

- **Claude subscription:** PAI runs Anthropic's own Claude Code with your login. Self-hosted only.
- **ChatGPT subscription:** OpenAI's "Sign in with ChatGPT" for open-source apps. Self-hosted only.
- **API keys:** `pai key anthropic sk-ant-…`, or in Settings.
- **Ollama or any OpenAI-compatible server:** add to `config.json`:
  ```json
  "providers": {"ollama": {"shape": "openai-chat", "url": "http://localhost:11434/v1"}}
  ```

### Several accounts, and failover

Each account is a provider of its own name. In Settings, pick Claude subscription again and name it, for example `claude-code-2`. Then choose what each account hands over to when it hits its usage limit:

```json
"fallbacks": {"claude-code": "claude-code-2", "claude-code-2": "anthropic"}
```

When a step fails because an account is out of usage, PAI moves the session to the next account and carries on. A record in the session says so. History and memory belong to the server, so the new account picks up exactly where the old one stopped. You can also move a session by hand, in the chat's model picker or with `pai model <session> <provider/model>`.

## Memory

New server installs in a terminal ask:

```text
Download private Ollama and enable local embeddings? [y/N]:
```

Choose **yes** to download a pinned Ollama runtime and `embeddinggemma` into
PAI's data directory. PAI starts the private runtime on loopback port 11435,
keeps the model loaded, checks readiness, and stops its child runtime on
shutdown. Binaries, models, runtime HOME, caches, temporary files and logs all
stay under the same PAI data directory. Downloads and storage need several GB.
No global Ollama installation is modified.

Choose **no** (the default) for model-free hash/keyword retrieval. No runtime or
model is downloaded. Unattended installs also use this option unless an
explicit config opts in. Reinstalls and updates retain the installed choice;
old configs without a `memory` field retain model-free retrieval.

For unattended opt-in, pass a config file with:

```json
"memory": {"mode": "semantic", "local": true}
```

To use an already running embedding service, omit `local: true`. For example,
prepare an existing Ollama with `ollama pull embeddinggemma`, then configure:

```json
"memory": {"mode": "semantic", "url": "http://localhost:11434/v1", "model": "embeddinggemma"}
```

`url` is the base URL ending in `/v1`, not `/embeddings`. An optional `key` accepts
an environment variable such as `$EMBEDDING_API_KEY`. Both `url` and `model` must
be supplied when overriding the defaults. Chat subscription credentials do not
automatically provide an embedding endpoint. An external endpoint receives the
memory texts and search queries.

`semantic` is the default for an explicit embedding configuration and does not
use keyword ranking. `hybrid` explicitly
adds BM25 rank fusion for exact identifiers; `hash` explicitly restores the old
model-free word-based behavior. These modes can be selected as
`"memory": {"mode": "hybrid"}` (existing endpoint) or
`"memory": {"mode": "hash"}`. Managed embeddings can also use hybrid mode by
retaining `"local": true`.

Restart the server after changing memory configuration. Existing items are
re-embedded in batches of 64 when the endpoint or model name changes; the text,
ids and history stay intact. The first startup can take longer. Use versioned
model names: replacing weights under an unchanged name cannot be detected
reliably. A failed migration stops startup and can resume on the next restart.
An unavailable service during a turn is recorded for the model as a memory
retrieval failure; PAI does not silently substitute word matching. Explicit
`recall` calls return the service error. Check the active embedder under Memory
stats (`GET /memory/stats`). Repeated inputs use a bounded in-process embedding
cache; persisted item vectors remain in SQLite.

Local semantic quality and latency depend on the chosen model and hardware;
validate on your own questions before relying on it for critical facts. See
[the retrieval and efficiency audit](retrieval-efficiency-audit.md) for remaining
accuracy risks and a measurement plan. The embedding endpoint follows
[Ollama's compatibility API](https://docs.ollama.com/api/openai-compatibility);
[Ollama's embeddings guide](https://docs.ollama.com/capabilities/embeddings)
explains semantic vectors and model consistency.

What PAI keeps, and how, is shown in the web app under Memory. In short:

- Lasting items only. Secrets and trivia are refused before storage.
- New items are checked against related ones. The model adds, merges, updates, retires or skips each one.
- A background pass merges repeats. Merged and edited items are new versions that name what they replaced.
- Nothing is deleted. Retired items stay in History and can be restored.

## Updates

```bash
pai-server update
```

It installs the newest release, checked against the release key built into your binaries. Machines follow at their next connection.
