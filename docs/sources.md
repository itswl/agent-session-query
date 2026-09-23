# Data sources and custom paths

The sources read the session directories the agent CLIs write by default:

| Source | Default location |
|--------|------------------|
| pi | `~/.pi/agent/sessions` |
| claude | `~/.claude/projects` |
| codex | `~/.codex/sessions` |
| gemini | `~/.gemini/tmp` |
| grok | `~/.grok/sessions` |
| hermes | `~/.hermes/sessions` (plus `state.db`) |
| openclaw | `~/.openclaw/agents/*` |
| opencode | XDG data dir, or `~/Library/Application Support` on macOS |

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

Supported: the directory-shaped sources `pi`, `claude`, `codex`, `gemini` and `grok`. The JSON-map
and SQLite sources (`hermes`, `openclaw`, `opencode`) keep their layout across several
files and reject `--path`.

To run the service so these flags survive a reboot, see [deploy.md](deploy.md).
