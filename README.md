# agent-session-query

**English** · [简体中文](README.zh-CN.md)

Query the session records left behind by the agent CLIs on your machine: **a read-only web
page, an HTTP API, and an MCP server for agents to call**. It never modifies session data.

A single binary (no cgo, no resident runtime) — `scp` it to any machine of the same
architecture and run it. The only external dependency is a pure-Go SQLite driver (for
reading Hermes's `state.db`), so cross-compilation still works as usual.

Seven sources; whichever exist are queried, and they can be merged in one query:

| Source | Where sessions live | Session ID |
|--------|---------------------|------------|
| Hermes | `~/.hermes/sessions/` (`sessions.json`) or `~/.hermes/state.db` (newer, all SQLite) | `session_id` in `sessions.json` / `id` in the `sessions` table |
| OpenClaw | `~/.openclaw/agents/<agent>/agent/openclaw-agent.sqlite` (SQLite, 2026.9+; older `sessions.json` layouts still read) | `session_id` in `session_windows` |
| Pi | `~/.pi/agent/sessions/<project>/*.jsonl` | `id` on the session row |
| Claude Code | `~/.claude/projects/<project>/*.jsonl` | the filename (a uuid) / the `sessionId` field |
| Codex | `~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl` | `payload.session_id` on the metadata row |
| Gemini CLI | `~/.gemini/tmp/<project>/chats/session-*.jsonl` | `sessionId` on the first line |
| OpenCode | `${XDG_DATA_HOME:-~/.local/share}/opencode/opencode.db` (SQLite; `%LOCALAPPDATA%\opencode` on Windows) | `id` in the `session` table |

The `~` in that table resolves to the running user's home: `$HOME` on Linux and macOS,
`%USERPROFILE%` on Windows (so `C:\Users\<you>\.claude\projects` and the like). Per-source
parsing details are in [docs/internals.md](docs/internals.md).

## Quick start

### Download a build

No Go toolchain needed. Pushing a tag like `v0.1.0` triggers
[GitHub Actions](.github/workflows/release.yml), which cross-compiles **six platforms**
(`linux` / `darwin` / `windows` × `amd64` / `arm64`) and attaches the artifacts to the
Releases page. Unpack and run. Unix gets `.tar.gz`, Windows `.zip`:

```bash
tar xzf agent-session-query-darwin-arm64.tar.gz
./agent-session-query --port 8080
```

```powershell
# Windows
Expand-Archive agent-session-query-windows-amd64.zip -DestinationPath .
.\agent-session-query.exe --port 8080
```

The artifacts are unsigned, so the OS will stop you once: on macOS run
`xattr -d com.apple.quarantine agent-session-query`; on Windows, when SmartScreen says
"Windows protected your PC", choose "More info → Run anyway".

### Build locally

```bash
go build -o agent-session-query ./cmd/agent-session-query
./agent-session-query --port 8080                     # auto-detect: enable whatever exists
./agent-session-query --mode claude                    # just one source
./agent-session-query --hook_token mysecrettoken       # with authentication
```

It prints the sources it enabled, then open `http://127.0.0.1:8080/ui` in a browser.

Add `-d` to put it in the background; the parent confirms the port really came up before
printing the PID and exiting:

```bash
./agent-session-query -d --port 8080
# Started in the background
#   PID:     82575
#   Address: http://127.0.0.1:8080
#   Log:     /tmp/agent-session-query-8080.log
#   Stop:    kill 82575
```

`-d` is for running in the background temporarily — nothing restarts the process if it dies.
For a supervised service (Linux systemd / macOS launchd / Windows scheduled task / Docker),
see **[docs/deploy.md](docs/deploy.md)**.

## Web page (`/ui`)

Three panes: **session list / message stream / final result**. A token is only requested
when the server was started with `--hook_token`, and it stays in the browser's localStorage.

- **Left**: each session shows a readable name — opencode's own session title, or for the
  other sources the first real user message — instead of a bare sessionId. Typing filters
  sessionId, path and cwd instantly, and the same keystrokes also
  **search the message bodies** on the server. Body matches are appended below the metadata
  ones under a "N more in message bodies" divider, carrying the matching snippet, so one
  query covers both without a mode to switch. <kbd>Enter</kbd> only skips the wait. The list
  can be grouped by time or by project — project headers fold away when clicked — and a
  session being written gets a pulsing green dot. Rows carry the message count where the
  source knows it (the SQLite-backed ones) or once the session has been opened
- **Middle**: the message timeline (text / thinking / toolCall / toolResult blocks). You can
  switch between the earliest and latest 200 messages, and filter to user or assistant
- **Right**: the final result stays visible — stopReason, the answer, the thinking, usage and
  cost, session metadata (one click to copy the sessionId, one to export Markdown), and a
  **conversation table of contents**: the user's turns, numbered, click to jump to one in
  the stream. It covers the loaded window and says so when the session is longer

It refreshes every 10 seconds without disturbing what you are reading (expanded blocks and
scroll position are preserved). `/ui#<sessionId>` works as a deep link, light and dark follow
the system, and narrow windows collapse to two panes and then one.

Shortcuts: <kbd>j</kbd> <kbd>k</kbd> move between sessions (metadata matches and body
matches alike) · <kbd>/</kbd> focus search · <kbd>Enter</kbd> search the bodies now instead
of waiting · <kbd>g</kbd> <kbd>G</kbd> jump to the start/end of the stream · <kbd>r</kbd>
refresh · <kbd>Esc</kbd> clear the search.

## Endpoints at a glance

Every query is a `GET` (MCP's `/mcp` is a `POST`). Once `--hook_token` is set, endpoints
marked "yes" require `Authorization: Bearer <token>`.

| Endpoint | Auth | What it does |
|----------|------|--------------|
| `/` `/health` `/stats` | no | Service info and health check |
| `/ui` `/favicon.ico` | no | The embedded page (which holds no data) |
| `/sessions` | yes | List every session (merged across sources, newest first) |
| `/sessions/<pattern>` | yes | One session; add `/messages`, `/final` or `/export` |
| `/search?q=` | yes | **Full-text search** across every source |
| `/projects` | yes | Session counts grouped by project (cwd) |
| `/mcp` | yes | MCP's Streamable HTTP transport (`POST`) |

Full parameters, the `<pattern>` matching rules and every response field are in
**[docs/api.md](docs/api.md)**.

## MCP

`--mcp` runs on stdio, and in HTTP mode there is also `POST /mcp`. That lets **an agent query
its own history** — Claude Code can go looking for the same problem you solved with Codex
last week. Five tools: `search_sessions` / `list_sessions` / `list_projects` / `get_session` /
`get_messages`.

Client configuration and the security notes are in **[docs/mcp.md](docs/mcp.md)**.

## Configuration

| Flag | Default | What it does |
|------|---------|--------------|
| `--host` | `127.0.0.1` | Bind address; use `0.0.0.0` to expose it (and set `--hook_token`) |
| `--port` | `8080` | Listen port |
| `--mode` | `auto` | `auto` (enable whatever exists) / `all` (enable all seven) / `hermes` / `openclaw` / `pi` / `claude` / `codex` / `gemini` / `opencode` |
| `--hook_token` | none | Bearer token; without it the API is unauthenticated |
| `--max-connections` | `50` | Maximum concurrent connections; anything past it queues |
| `--accept-queue` | `0` (auto) | Queue slots when at capacity; `0` means `2 × max-connections`, never below 32. A full queue returns 503 immediately |
| `--timeout` | `30` | Connection timeout in seconds |
| `--cache-ttl` | `2` | Seconds to cache the session list; `0` disables caching |
| `--max-limit` | `1000` | Upper bound for `?limit=`; anything larger is clamped |
| `--cors-origin` | none (off) | Allowed CORS origin; `*` or a specific origin. Unset means no CORS headers at all |
| `-d` | off | Run in the background, detached from the terminal, logging to a file |
| `--log-file` | derived from the port | Log path used with `-d`; defaults to `<tmp>/agent-session-query-<port>.log` |
| `--mcp` | off | Run as an MCP server on stdio (see above); does not listen on a port |
| `--version` | — | Print the version and exit |

Docker environment variables: `HOOK_TOKEN`, `SESSION_MODE` (default `auto`), and `GO_IMAGE`
(a build argument).

Without `--hook_token` the `HOOK_TOKEN` environment variable is read instead — **command-line
arguments show up in `ps`, environment variables do not**, so prefer the latter for anything
long-running.

## Security

- It can read complete session content, tool output included, so **always set
  `--hook_token`** before exposing it
- It binds `127.0.0.1` and sends no CORS headers by default. Without a token, those two are
  the only things standing between any web page you visit and a `fetch` of your local
  `/sessions` — think it through before changing either
- Put it behind an HTTPS reverse proxy (Nginx, Caddy) rather than on the public internet.
  `/ui` carries no data, but authenticate it at the proxy anyway
- `/ui` ships a `Content-Security-Policy` (same-origin scripts and styles only, nothing
  inline, no framing) and renders exclusively through `textContent`
- Rotate tokens; keep them out of images and repositories; mount session directories `:ro`

## Troubleshooting

1. **A source was not enabled** (it is missing from `sources` in `/health`): check the
   directory exists using the table above; `--mode all` prints which ones were missing.
   Hermes needs `sessions.json` **or** `state.db`, either is enough. In a container, confirm
   the directory was actually mounted.
2. **The list is empty**: check `/health` for which sources are enabled, and that the process
   can read those directories.
3. **401**: check that `Authorization: Bearer <token>` matches the `--hook_token` the server
   started with.
4. **Port already in use**: pick another `--port`.

## Documentation

| | |
|---|---|
| [docs/api.md](docs/api.md) | The full HTTP API: parameters, matching rules, response fields |
| [docs/mcp.md](docs/mcp.md) | MCP: both transports, client configuration, the tool table |
| [docs/deploy.md](docs/deploy.md) | Supervised deployment: systemd / launchd / Windows scheduled task / Docker |
| [docs/development.md](docs/development.md) | Layout, tests, releasing, adding a source |
| [docs/internals.md](docs/internals.md) | Implementation: per-source parsing, performance, search, cross-platform |

## License

MIT, see [LICENSE](LICENSE).

---

**Note**: this service is read-only. It never modifies the session data of Hermes, OpenClaw,
Pi, Claude Code, Codex or Gemini.
