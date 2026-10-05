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
| Pi | `~/.pi/agent/sessions/<project>/*.jsonl` | `id` on the session row |
| Claude Code | `~/.claude/projects/<project>/*.jsonl` | the filename (a uuid) / the `sessionId` field |
| Codex | `~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl` | `payload.session_id` on the metadata row |
| Gemini CLI | `~/.gemini/tmp/<project>/chats/session-*.jsonl` | `sessionId` on the first line |
| OpenCode | `${XDG_DATA_HOME:-~/.local/share}/opencode/opencode.db` (SQLite; `%LOCALAPPDATA%\opencode` on Windows; the 1.x `session` tables and the 2.x `session_v2` / `session_message` tables both) | `id` in the `session` or `session_v2` table |
| Grok CLI | `~/.grok/sessions/<url-encoded-cwd>/<session-id>/` (`summary.json` + `updates.jsonl`), plus `~/.grok/archived_sessions/` | `info.id` in `summary.json` |

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
  session written within the last couple of minutes gets a pulsing dot (recent activity,
  not a liveness check — hovering it says how long ago). Rows carry the session's message count
  — counted from the file in the background the first time it is listed, so the count
  appears a moment after the page does rather than being paid for on the request path
- **Middle**: the conversation, read by rounds — you asked, the agent worked, the agent
  answered. The work between an ask and its reply folds to one line: *72 steps · 61
  commands · changed 2 files · 1 failed · 24m39s*. A failed step stays in view when the
  rest is folded, with the start of its output; a run of reads and searches folds to
  "explored — read 4 files, searched twice"; every step opens to its arguments and its
  output, and a change opens as a diff, whichever way the CLI spelled the edit. A result
  says how it ended — exit code, duration — and what happened between the turns (a
  compaction, an interruption, a hook that failed) is a line of its own. Three views:
  **all**, **conversation** (the words alone) and **changes** (the diffs alone, by round).
  Under the toolbar, the window's sums: rounds, tool calls, failures, how long it ran. It
  opens on the latest 200 messages and **loads the previous page when you scroll to the
  top** (or the next one at the bottom, when reading from the earliest) — the count in the
  header says how far into the session you are. Tool output comes cut to a preview, and
  "Show full output" fetches the rest. Replies render their Markdown — headings, lists,
  code, tables, links — and a message's time shows the clock alone on the session's own
  day, the date in front on any other (the full timestamp is in the tooltip). Steps are
  coloured by category (run / read / write / search / network / delegate)
- **Right**: the final result stays visible — stopReason, the answer, the thinking — then
  the **conversation table of contents**: two lines a round — what you asked, and what the
  agent concluded, with how many steps it took and a red mark when one failed or the round
  was interrupted — click either to jump to it in the stream (it covers the loaded window
  and says so when the session is longer). Below it the session's facts: the command that
  reopens it in its own CLI (copied with the `cd` it needs), the file path, one click to
  export in any of the four formats (Markdown, JSONL, JSON or a standalone HTML page),
  usage and cost, and the other sessions of the same project

It installs as a web app: open `/ui` on a phone and "Add to Home Screen" gives it an
icon, a name and a window without browser chrome, and it honours the notch and home
indicator when it does. (The icons are drawn by `go run tools/icongen.go` — standard
library only, so the same source produces the same bytes anywhere.)

On a phone the three panes become one screen at a time, picked from a **Sessions /
Conversation / Details** tab bar at the bottom of the screen — under the thumb, above the
home indicator, the way an installed app's is. Picking a session opens its conversation and
the system's back gesture brings the list back; the title row slides away as you read down
and returns as you scroll up. The search box and list filters belong to Sessions — they do
nothing to a conversation or a details pane, so neither has to open with five rows of
controls it cannot use. It refreshes every 10 seconds without disturbing
what you are reading (expanded blocks and scroll position are preserved), your view
controls — grouping, source filter, order, filters, folded groups — survive a reload, and
`/ui#<sessionId>` works as a deep link. Light and dark follow the system; the half-disc
button in the header switches, and switching back to what the system shows hands control
back to the system.

Each side pane has a bar of its own — its name, and the controls that act on it: "Collapse
all" over the project groups, and a chevron at the edge that folds the pane away, giving the
conversation the whole width. A folded pane keeps a slim rail with the chevron pointing back,
and both remember what you chose.

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
| `/sessions/<pattern>` | yes | One session; add `/messages` (with `order` / `limit` / `at`, and `full=1` for tool output whole rather than cut to a preview), `/final`, `/rounds` (the session split at each real user message, with files changed and tool calls failed per round), `/brief` (a handoff brief of one round, resume command included), or `/export` (no `limit` means the whole session; `format=jsonl` for the data form) |
| `/search?q=` | yes | **Full-text search** across every source; `pattern` scopes it to one session, `role` to `user` or `assistant` hits |
| `/projects` | yes | Session counts grouped by project (cwd) |
| `/export?project=&since=` | yes | **A pack of several sessions as one document** — what was asked and what each concluded, oldest first. `mode=full` inlines the transcripts |
| `/mcp` | yes | MCP's Streamable HTTP transport (`POST`) |

Full parameters, the `<pattern>` matching rules and every response field are in
**[docs/api.md](docs/api.md)**.

## MCP

`--mcp` runs on stdio, and in HTTP mode there is also `POST /mcp`. That lets **an agent query
its own history** — Claude Code can go looking for the same problem you solved with Codex
last week. The tools: `search_sessions` / `list_sessions` / `list_projects` /
`recent_project_activity` / `find_decisions` / `find_similar_question` / `get_session` /
`get_messages` / `list_rounds` / `session_brief`. A tool result in a transcript carries how
it ended (status, exit code, duration) and which call it answers; `list_rounds` says which
round carried failures and which files it changed, and `session_brief` hands one round to
another agent with the command that resumes the session.

Client configuration, the security notes, and the Claude Code skill that ships with the
repository are in **[docs/mcp.md](docs/mcp.md)**.

## Configuration

| Flag | Default | What it does |
|------|---------|--------------|
| `--host` | `127.0.0.1` | Bind address; use `0.0.0.0` to expose it (and set `--hook_token`) |
| `--port` | `8080` | Listen port |
| `--mode` | `auto` | `auto` (enable whatever exists) / `all` (enable all eight) / `hermes` / `openclaw` / `pi` / `claude` / `codex` / `gemini` / `opencode` / `grok` |
| `--path` | none | Relocate or duplicate a file-backed source (`pi`, `claude`, `codex`, `gemini`, `grok`): `--path claude=/mnt/disk/.claude/projects` points the source elsewhere; `--path claude:box2=/mnt/box2/.claude/projects` adds a second instance named `claude:box2` in the source list, the page's source filter and project grouping. Repeatable |
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
   can read those directories. A `warnings` entry there names the source that could not be
   read and why — a source that broke and a source with nothing in it both answer with an
   empty list, and only one of them is a problem.
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

## License

MIT, see [LICENSE](LICENSE).

---

**Note**: this service is read-only. It never modifies the session data of Hermes, OpenClaw,
Pi, Claude Code, Codex, Gemini or Grok.
