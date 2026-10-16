# Roadmap

Where the project stands and what might come next, kept current with every release. An
open item is a candidate, not a promise: each says what it would answer and how worth doing
it looks now, and an item can leave the list as readily as it joins it. A deferred one was
decided against for now.

## Shipped

| Release | What it brought |
|---|---|
| v0.38.0 | **What the sessions consumed**: `/usage` and MCP `get_usage_report` — tokens and cost for today / this week / this month / all, by model, by day, by source, by project; Claude Code's closing `cost-state` row is read for the cost no message row knows; the page's services panel opens with the week and the models that did the work |
| v0.37.0 | **Storage, sight and live follow**: `/storage` and MCP `get_storage_report` size the history three ways — by source (with the overhead kept besides sessions), by project, by session age; the page's header panel shows which sources are on and what failed; an open, active session is followed as it grows, new rounds arriving marked |
| v0.36.0 | **The drift defence**: `--check` names every source with found-or-not, a session count and a deep read of the newest session (exit 1 when one fails); a Grok session still running is read from its `chat_history.jsonl` — `isProcessing`, usage from `usage.json` — instead of waiting for the ACP stream Grok only flushes at the end; a source whose directory disappears drops out of the enabled set instead of lingering in `/health`; `?source=` reaches `/messages` and `/final` the way it already reached rounds and briefs |
| v0.35.0 | **All eight sources follow their CLI's own relocation, verified against the CLIs themselves**: Grok's `GROK_HOME`, Hermes's `HERMES_HOME` and OpenClaw's `OPENCLAW_STATE_DIR` join the others — every source installed and one GLM-5.3 session driven through each, read back through this service. Gemini CLI has no such variable; its fixed `~/.gemini` is official and read as is |
| v0.34.0 | **Each CLI's own relocation followed**: pi's `PI_CODING_AGENT_DIR` / `PI_CODING_AGENT_SESSION_DIR`, Claude Code's `CLAUDE_CONFIG_DIR` and Codex's `CODEX_HOME` are read the way the CLIs read them; no tool-specific layouts anywhere — a hosted directory is a `--path`, a moved one an environment variable |
| v0.33.1 | **Hosted pi by `--path`, not by a host's directory shape**: the flat `--session-dir` layout stays; the scan of one host's state layout is gone — a hosted directory is pointed at explicitly, like every other session store outside the defaults |
| v0.33.0 | **Sessions outside the default locations**: pi's flat `--session-dir` layout read, and each agent's pi that Larkin hosts found as its own labeled instance; `--path` relocates and labels opencode's single database |
| v0.32.0 | **Copy and pack buttons in the page**: a code block, a round's brief, a step's command, diff or output; a project's pack copied or saved from the details pane or its header in the list. Epoch-millisecond times are exact, so a pi output reads whole again |
| v0.31.0 | **Archived sessions, one rule for all**: what Grok, Codex, OpenCode and Hermes put away is read the same way — left out by default as each CLI does, brought in by `include_archived` or the page's *Archived* switch, marked `archived` |
| v0.30.0 | **File history** — which sessions touched a file, in which round, and what was asked there (`/files?path=`, MCP `file_history`, and the page's *Files changed* card); rounds now see every file a call names, Codex's patches included |
| v0.29.1 | Prompts typed while the agent worked appear where they were typed; a harness's closing blocks fold under the words; find in the session opens the fold a match is in |
| v0.29.0 | Claude Code subagents as sessions of their own, nested under the session they ran in and searched with it; a full read returns the output Claude saved beside the transcript |
| v0.28.1 | Claude search keeps to the rows the reader draws; harness blocks out of titles and asked lines; SQLite reads wait out a write |
| v0.28.0 | Search narrowed by source, project and branch before the scan; MCP `get_session_pack`; find in the session and failure navigation in the page |

## Open

Roughly in the order they look worth doing.

| Item | What it would answer | How it looks |
|---|---|---|
| Bookmarks and notes | A session or a round marked to come back to | Maybe. Browser storage keeps the server read-only; worth it only if the list is hard to find things in |
| Edits made with shell commands | `sed -i`, a redirect or a script counted as changing a file, not only as naming it | Maybe. A command's effect has to be guessed from its text, and a wrong guess is worse than the honest "named in a command" file history gives now |
| Search inside saved outputs | Words in the large tool outputs Claude Code keeps beside the transcript | Maybe. They are read whole on demand already; searching them means scanning their size again on every query |
| Config file and `--check` | Long multi-path setups out of service units and compose files | Maybe, once setups grow past a few flags |
| Two-session compare | Two attempts at the same task side by side: conclusions, failures, files changed, time | Doubtful. Most of what it would line up is already per session in the pack, `/rounds` and file history; it needs a concrete case first |
| `/metrics` | Prometheus scraping: latency, scan volume, queueing, source errors | Doubtful unless the service runs under monitoring; `/health` and `/stats` cover the rest |

## Deferred

Decided against for now (2026-10-14), kept for when that changes.

| Item | What it would answer | Why not now |
|---|---|---|
| Remote sources | One view over several machines: another instance's sessions read through its HTTP API, labelled like a `--path` instance | Each machine's own page and API answer for its sessions today; joining them is the larger piece of work, with a network in the middle |
| `--path` for the remaining database sources | Hermes and OpenClaw data on a mounted disk, a backup or a container volume | Their layout spans several files and a whole data directory, not the one database opencode turns out to be; no such copy waiting to be read |

## Not planned

- **Sending history to a cloud model** for summaries or tags: it would end local, offline,
  private reading, which is the point of the service.
- **A vector database or embedding model**: it would end the single binary with no cgo that
  builds for every platform, and lexical search over the raw bytes answers in well under a
  second here.
- **A persistent full-text index**: eight CLIs append to, compact, truncate and rotate their
  files concurrently, and an index of them goes stale in ways a scan cannot; the scan is
  fast enough (see [internals](internals.md#full-text-search)).
- **Discovering Aider's history**: Aider keeps Markdown files inside each repository, with no
  common directory to find them in; looking for them means scanning the disk and guessing.
