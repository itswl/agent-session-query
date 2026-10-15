# agent-session-query

**English** · [简体中文](README.zh-CN.md)

Query the session records left behind by the agent CLIs on your machine: **a read-only web
page, an HTTP API, and an MCP server for agents to call**. It never modifies session data.

A single binary (no cgo, no resident runtime) — `scp` it to any machine of the same
architecture and run it. The only external dependency is a pure-Go SQLite driver (for
reading Hermes's `state.db`), so cross-compilation still works as usual.

Eight sources; whichever exist are queried, and they can be merged in one query:

| Source | Where sessions live | Session ID |
|--------|---------------------|------------|
| Hermes | `~/.hermes/sessions/` (`sessions.json`) or `~/.hermes/state.db` (newer, all SQLite) | `session_id` in `sessions.json` / `id` in the `sessions` table |
| OpenClaw | `~/.openclaw/agents/<agent>/agent/openclaw-agent.sqlite` (SQLite, 2026.9+; older `sessions.json` layouts still read) | `session_id` in `session_windows` |
| Pi | `~/.pi/agent/sessions/<project>/*.jsonl`, plus flat `*.jsonl` under a `--session-dir` | `id` on the session row |
| Claude Code | `~/.claude/projects/<project>/*.jsonl` | the filename (a uuid) / the `sessionId` field |
| Codex | `~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl`, plus `~/.codex/archived_sessions/` | `payload.session_id` on the metadata row |
| Gemini CLI | `~/.gemini/tmp/<project>/chats/session-*.jsonl` | `sessionId` on the first line |
| OpenCode | `${XDG_DATA_HOME:-~/.local/share}/opencode/opencode.db` (SQLite; `%LOCALAPPDATA%\opencode` on Windows; the 1.x `session` tables and the 2.x `session_v2` / `session_message` tables both) | `id` in the `session` or `session_v2` table |
| Grok CLI | `~/.grok/sessions/<url-encoded-cwd>/<session-id>/` (`summary.json` + `updates.jsonl`), plus `~/.grok/archived_sessions/` | `info.id` in `summary.json` |

The `~` in that table resolves to the running user's home: `$HOME` on Linux and macOS,
`%USERPROFILE%` on Windows (so `C:\Users\<you>\.claude\projects` and the like). Each
directory is also resolved the way its CLI resolves it — `CLAUDE_CONFIG_DIR`,
`CODEX_HOME`, `GROK_HOME`, `HERMES_HOME`, `OPENCLAW_STATE_DIR`, pi's
`PI_CODING_AGENT_DIR` / `PI_CODING_AGENT_SESSION_DIR` and opencode's `XDG_DATA_HOME` are
honoured, read from this service's own environment. Per-source parsing details are in
[docs/internals.md](docs/internals.md); relocations and extra instances in
[docs/sources.md](docs/sources.md).

## Quick start

### Install with one line

No Go toolchain needed, and **avoids macOS Gatekeeper / Windows SmartScreen quarantine blocks**:

**macOS / Linux:**
```bash
curl -fsSL https://raw.githubusercontent.com/itswl/agent-session-query/main/install.sh | sh
```

**Windows (PowerShell):**
```powershell
irm https://raw.githubusercontent.com/itswl/agent-session-query/main/install.ps1 | iex
```

The script automatically detects your OS/architecture (`darwin` / `linux` / `windows` ×
`amd64` / `arm64`), downloads the latest release, **verifies it against the release's
published `SHA256SUMS`**, unpacks it, and installs it. On Windows it also clears the
download's trust marking and adds the install directory to your user PATH; on macOS and
Linux, `curl` downloads carry no quarantine attribute to clear, and the script prints a
PATH note only when it had to fall back to `~/.local/bin`.

### Manual download

Pushing a tag like `v0.1.0` triggers
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
when the server was started with `--hook_token`; it stays in the browser's localStorage.

- **Left — the list**: every session under a readable name (opencode's own title, or the
  first real user message elsewhere), grouped by time or project, with a pulsing dot for
  recent activity and a message count filled in by a background scan. Claude Code
  subagents nest under the session that spawned them (▸ 12 agents), and an **Archived**
  switch brings in the sessions a CLI put away. Typing filters sessionId / path / cwd
  and, in the same query, **searches message bodies** on the server — body hits append
  under a divider with their snippets, and a source picked in the filter scopes the body
  search to it
- **Middle — the conversation, read by rounds**: you asked, the agent worked, the agent
  answered. The work between an ask and its reply folds to one line (*72 steps · 61
  commands · changed 2 files · 1 failed · 24m39s*), keeping failures in view; runs of
  reads and searches fold to "explored". Every step opens to its arguments and output,
  edits as diffs whatever way the CLI spelled them, results with exit code and duration;
  what happened between turns (a compaction, an interruption, a failed hook) gets a line.
  Five views: **all**, **conversation**, and one kind of step alone — **changes**,
  **commands**, **failures**. Under the toolbar the window's sums, and the failure count
  steps through failures one at a time. **Find** (<kbd>f</kbd>) marks words across the
  session and walks matches beyond the loaded window; a body match picked in the list
  opens with its words already in the find bar. It opens on the latest 200 messages and
  pages on scroll; tool output is cut to a preview with "Show full output"; Markdown
  renders; steps are coloured by category. Copying is where the thing is: a code block,
  a round's brief, a step's command, diff or output — whole, when the window held only
  its start
- **Right — the result and the map**: stopReason, the answer and the thinking stay
  visible; then the **conversation table of contents** — two lines a round, what was
  asked and what was concluded, a red mark when something failed — clicking either jumps
  into the stream. Below, the session's facts: the resume command with its `cd`, the
  file path, export to Markdown / JSONL / JSON / a standalone HTML page, usage and cost,
  and the project's other sessions with its **pack** — what each latest session asked
  and concluded — to copy into another agent or save as a file

It installs as a web app ("Add to Home Screen" on a phone) and reloads itself after a
server upgrade. On a phone the three panes are screens behind one another, and the
system's back gesture walks the same way back. It refreshes every 10 seconds without
disturbing what you are reading — expanded blocks, opened rounds and your place are
kept — and your view controls survive a reload. `/ui#<sessionId>` is a deep link. Light
and dark follow the system, with a manual switch; each side pane folds away to a slim
rail and remembers.

Shortcuts: <kbd>j</kbd> <kbd>k</kbd> sessions · <kbd>/</kbd> search · <kbd>Enter</kbd>
bodies now · <kbd>g</kbd> <kbd>G</kbd> start/end · <kbd>f</kbd> find · <kbd>[</kbd>
<kbd>]</kbd> failures · <kbd>r</kbd> refresh · <kbd>Esc</kbd> clear.

## Endpoints at a glance

Every query is a `GET` (MCP's `/mcp` is a `POST`). Once `--hook_token` is set, endpoints
marked "yes" require `Authorization: Bearer <token>`.

| Endpoint | Auth | What it does |
|----------|------|--------------|
| `/` `/health` `/stats` | no | Service info and health check |
| `/ui` `/favicon.ico` | no | The embedded page (which holds no data) |
| `/sessions` | yes | List every session (merged across sources, newest first) |
| `/sessions/<pattern>` | yes | One session; add `/messages` (with `order` / `limit` / `at`, and `full=1` for tool output whole rather than cut to a preview), `/final`, `/rounds` (the session split at each real user message, with files changed and tool calls failed per round), `/brief` (a handoff brief of one round, resume command included), or `/export` (no `limit` means the whole session; `format=jsonl` for the data form) |
| `/search?q=` | yes | **Full-text search** across every source; `source` / `project` / `branch` / `since` / `until` narrow which sessions are scanned, `pattern` scopes it to one session, `role` to `user` or `assistant` hits |
| `/files?path=` | yes | **Which sessions touched a file**, newest first: the rounds that changed it, read it, or ran a command naming it, with what was asked there. `changed=1` keeps the changes |
| `/projects` | yes | Session counts grouped by project (cwd) |
| `/export?project=&since=` | yes | **A pack of several sessions as one document** — what was asked and what each concluded, oldest first. `mode=full` inlines the transcripts |
| `/mcp` | yes | MCP's Streamable HTTP transport (`POST`) |

Full parameters, the `<pattern>` matching rules and every response field are in
**[docs/api.md](docs/api.md)**.

## MCP

`--mcp` runs on stdio, and in HTTP mode there is also `POST /mcp`. That lets **an agent query
its own history** — Claude Code can go looking for the same problem you solved with Codex
last week. The tools: `search_sessions` / `list_sessions` / `list_projects` /
`recent_project_activity` / `get_session_pack` / `find_decisions` / `find_similar_question` /
`get_session` / `get_messages` / `list_rounds` / `session_brief` / `file_history`. `get_session_pack` is the pack
as one call: what each session in a selection asked and concluded, oldest first; `file_history` says which
sessions changed a file, in which round, and what was asked there. A tool result in a transcript carries how
it ended (status, exit code, duration) and which call it answers; `list_rounds` says which
round carried failures and which files it changed, and `session_brief` hands one round to
another agent with the command that resumes the session. Session text is untrusted input:
transcripts are handed to calling agents as data, unmarked and unmodified — redaction
covers secrets, not persuasion — so instructions found inside a session are content to
report on, never commands to follow.

Client configuration, the security notes, and the Claude Code skill that ships with the
repository are in **[docs/mcp.md](docs/mcp.md)**.

## Configuration

| Flag | Default | What it does |
|------|---------|--------------|
| `--host` | `127.0.0.1` | Bind address; use `0.0.0.0` to expose it (refused without `--hook_token`) |
| `--port` | `8080` | Listen port |
| `--mode` | `auto` | `auto` (enable whatever exists) / `all` (enable all eight) / `hermes` / `openclaw` / `pi` / `claude` / `codex` / `gemini` / `opencode` / `grok` |
| `--path` | none | Relocate or duplicate a source: `--path claude=/mnt/disk/.claude/projects` points the source elsewhere; `--path claude:box2=/mnt/box2/.claude/projects` adds a second instance named `claude:box2`. Directory-shaped sources (`pi`, `claude`, `codex`, `gemini`, `grok`) and opencode's database (its data dir or the `opencode.db` file). Repeatable — the way a pi hosted behind a wrapper's `--session-dir` is pointed at |
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

Docker: every release publishes a multi-arch image to GitHub Container Registry, so nothing
has to be built; `HOOK_TOKEN` and `SESSION_MODE` (default `auto`) are its environment
variables, and `GO_IMAGE` is a build argument for a local build. See
[docs/deploy.md](docs/deploy.md).

Without `--hook_token` the `HOOK_TOKEN` environment variable is read instead — **command-line
arguments show up in `ps`, environment variables do not**, so prefer the latter for anything
long-running.

## Security

- It can read complete session content, tool output included, so **always set
  `--hook_token`** before exposing it. A non-loopback `--host` without a token is refused
  at startup, not warned about
- It binds `127.0.0.1` and sends no CORS headers by default; on top of that, `Origin` is
  validated on every route except `OPTIONS` preflights, and with no token on a loopback
  bind any request whose `Host`
  is not a loopback name (`localhost`, `127.0.0.0/8`, `::1`) is refused — the shape a
  DNS-rebinding page sends. A token, or a deliberate non-loopback bind, turns the `Host`
  check off: behind a reverse proxy the forwarded `Host` is the public name, and the token
  is what protects the data there
- Put it behind an HTTPS reverse proxy (Nginx, Caddy) rather than on the public internet.
  `/ui` carries no data, but authenticate it at the proxy anyway
- `/ui` ships a `Content-Security-Policy` (same-origin scripts and styles only, nothing
  inline, no framing) and renders exclusively through `textContent`
- Release archives are covered by a published `SHA256SUMS`, and both install scripts verify
  the download against it before installing anything (releases before v0.25.0 have no sums
  file; the scripts say so and continue)
- Rotate tokens; keep them out of images and repositories; mount session directories `:ro`

## Troubleshooting

1. **A source was not enabled** (it is missing from `sources` in `/health`): check the
   directory exists using the table above; `--mode all` prints which ones were missing.
   Hermes needs `sessions.json` **or** `state.db`, either is enough. In a container, confirm
   the directory was actually mounted. With nothing installed at all, `auto` (and `all`)
   falls back to OpenClaw so the service still starts — `/health` lists it and it will
   simply be empty. In `auto`/`all` the set is re-checked while the process runs, so a CLI
   whose first session appears after startup is picked up without a restart.
2. **The list is empty**: check `/health` for which sources are enabled, and that the process
   can read those directories. A `warnings` entry there names the source that could not be
   read and why — a source that broke and a source with nothing in it both answer with an
   empty list, and only one of them is a problem (with a token configured, `warnings` is
   included only for an authenticated request).
3. **401**: check that `Authorization: Bearer <token>` matches the `--hook_token` the server
   started with.
4. **Port already in use**: pick another `--port`.

## Documentation

| | |
|---|---|
| [docs/api.md](docs/api.md) | The full HTTP API: parameters, matching rules, response fields |
| [docs/mcp.md](docs/mcp.md) | MCP: both transports, client configuration, the tool table |
| [docs/deploy.md](docs/deploy.md) | Supervised deployment: systemd / launchd / Windows scheduled task / Docker |
| [docs/sources.md](docs/sources.md) | Data sources, and `--path` for sessions outside the default home |
| [docs/development.md](docs/development.md) | Layout, tests, releasing, adding a source |
| [docs/internals.md](docs/internals.md) | Implementation: per-source parsing, performance, search, cross-platform |
| [docs/roadmap.md](docs/roadmap.md) | What has shipped, what is still open and how worth doing it looks, and what is not planned |

## License

MIT, see [LICENSE](LICENSE).

---

**Note**: this service is read-only. It never modifies the session data of Hermes, OpenClaw,
Pi, Claude Code, Codex, Gemini, OpenCode or Grok.
