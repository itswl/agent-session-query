# Data sources and custom paths

The sources read the session directories the agent CLIs write by default:

| Source | Default location |
|--------|------------------|
| pi | `~/.pi/agent/sessions` |
| claude | `~/.claude/projects` |
| codex | `~/.codex/sessions`, plus `~/.codex/archived_sessions` beside it |
| gemini | `~/.gemini/tmp` |
| grok | `~/.grok/sessions`, plus `~/.grok/archived_sessions` beside it |
| hermes | `~/.hermes/sessions` (plus `state.db`) |
| openclaw | `~/.openclaw/agents/*` |
| opencode | The XDG data dir — `${XDG_DATA_HOME:-~/.local/share}/opencode`, macOS included (`%LOCALAPPDATA%\opencode` on Windows) |

Each directory is resolved the way the CLI itself resolves it, so a CLI whose data moved
by environment variable is read where the variable says — pi's `PI_CODING_AGENT_DIR` and
`PI_CODING_AGENT_SESSION_DIR` (the flat `--session-dir` layout), Claude Code's
`CLAUDE_CONFIG_DIR`, Codex's `CODEX_HOME`, opencode's `XDG_DATA_HOME` / `LOCALAPPDATA`.
The variables are read from this service's own environment, so a daemon has to be given
the ones its CLIs run with (an `EnvironmentFile`, a unit's `Environment=`). Gemini CLI,
Grok CLI, Hermes and OpenClaw define no such variable; their defaults are what is read.

Sessions a CLI has archived — Grok's and Codex's moved into `archived_sessions`, OpenCode's
marked `time_archived`, the ones Hermes hides (`archived`, or `hidden` in older schemas) —
are read like any other and marked `archived: true`, and left out of the list, the search,
the pack, file history and the project counts unless `include_archived=1` (or the MCP
argument of the same name) asks for them, as each CLI leaves them out of its own list.

## Hosted pi sessions

pi has two storage layouts, both read: the interactive default nests one directory per
project under `~/.pi/agent/sessions`, while `--session-dir <dir>` — the convention a
host or wrapper uses to embed pi — stores the `*.jsonl` files flat in the directory it
was given. Those sessions never reach `~/.pi/agent/sessions`, so a hosted directory is
pointed at explicitly, as its own labeled instance:

```
--path pi:host=/path/to/the/session-dir
```

## `--path`

`--path mode[:label]=dir`, repeatable, reads sessions that live somewhere else — another
disk, a backup, agent containers whose sessions land on a host bind mount:

```
--path claude=/mnt/disk/.claude/projects          relocate the source
--path claude:box2=/mnt/box2/.claude/projects     add an instance named claude:box2
```

- **Relocating** (no label) keeps the plain source name and the usual behaviour: in
  `--mode auto` the directory must exist, in `--mode all` a missing one warns and is
  skipped
- **A labeled instance** is an explicit request: it is enabled in every `--mode`, even
  when the directory does not exist yet (the startup log warns), and it appears as its
  own source — `claude:box2` in the startup banner, the `source` field, the page's source
  filter and `/projects`
- The label is prefixed onto each session's project and cwd (`box2:/data`), so sessions
  from different machines sharing one working directory stay separate projects
- Labels are lowercase letters, digits and dashes; a directory path may contain colons
- Finding a session by the bare mode name still reaches into labeled instances, so an MCP
  client holding `claude` keeps working

Supported: the directory-shaped sources `pi`, `claude`, `codex`, `gemini` and `grok`, and
`opencode` — it is one SQLite file, so both the data directory (the shape a redirected
`XDG_DATA_HOME` leaves behind) and the database file itself relocate it. The JSON-map and
SQLite sources that keep their layout across several files (`hermes`, `openclaw`) reject
`--path`.

In `auto` and `all`, which sources exist is re-checked while the process runs, not only at
startup: a CLI you use for the first time after the server started creates its data
directory with that first session, and it starts being listed without a restart. `all`
names each missing source once, at first check.

To run the service so these flags survive a reboot, see [deploy.md](deploy.md).
