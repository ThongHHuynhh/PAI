# Self-hosting PAI

PAI is one server you run for yourself, plus a small client on each machine. The server holds every credential, every session and the memory. Nothing is sent anywhere but to the model providers you connect.

```
 browser, TUI, CLI ──https──► your URL ──► 127.0.0.1:8080 pai-server
 machines (pai)    ──────────────┘             │ sessions/  memory/  config.json
                    WireGuard mesh ◄───────────┘ coordinator + relay
```

## Install the server

```bash
curl -fsSL https://github.com/OWNER/PAI/releases/latest/download/install.sh | sh -s -- server quickstart
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

Memory works with no setup: the built-in embedder needs no model. For better recall of reworded questions, point it at an embeddings endpoint:

```json
"memory": {"url": "https://api.openai.com/v1", "key": "$OPENAI_API_KEY", "model": "text-embedding-3-small"}
```

Ollama works too (`http://localhost:11434/v1` with `nomic-embed-text`). Items are embedded again when the embedder changes.

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
