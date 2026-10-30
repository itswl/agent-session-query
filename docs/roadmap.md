# Roadmap

Where the project stands and what might come next, kept current with every release. An
open item is a candidate, not a promise: each says what it would answer and how worth doing
it looks now, and an item can leave the list as readily as it joins it. A deferred one was
decided against for now.

## Shipped

| Release | What it brought |
|---|---|
| v0.39.2 | **DeepSeek Harness** joins the sources: `$DSH_HOME/sessions/<project>/<session>/session.v<N>.jsonl[.zstd]`, the highest rotating generation read, concatenated zstd frames decoded in order with the half-written tail dropped, compaction `surfaceOp.replace` rows skipped, duplicate message ids kept once, and per-message usage summed with `inputTokens` excluding cache. `DSH_HOME` moves the store; `--path dsh=` relocates it. No resume command — the harness's own Web UI owns resume |
| v0.39.1 | **OTLP round-level increments and metrics**: the export cursor now keeps a fingerprint per round, so a session that grows sends only the round that changed (an id-keyed backend updates it, an append-only one sees one round twice instead of the whole conversation); `--otel-metrics` adds delta sums to `/v1/metrics` — rounds, wall-clock duration, tokens by model and type, failed tools — for collectors that store metrics, off by default because Langfuse takes traces only |
| v0.39.0 | **OTLP export, generic and opt-in**: one exporter renders every session as traces — one per round, with generation / tool / event spans, GenAI semantic-convention attributes and the few `langfuse.*` names — and posts OTLP/HTTP JSON to any collector, Langfuse included. `--otel-since` backfills 7 days by default or the whole history; server mode exports settled changes on `--otel-every` with a cursor; `--otel-content=io` adds bodies (masked, capped); every span records where its times came from (`timing.source=recorded/derived/inferred`) instead of pretending model latency is measured |
| v0.38.11 | **What a round cost, what keeps failing, what a week added up to**: `/rounds` carries each round's `usage` (summed from its messages, with the cache hit rate), the fold line shows it, and a brief gains `- Tokens: 18.2k in · 900 out · cache 71%`; `/failures` and MCP `find_failures` collect the failed tool results of a window — tool, command, output, the session and round — and the page's **Failures** switch browses them; `/digest` and MCP `get_digest` answer what a stretch of work added up to per project (sessions, rounds, failures, tokens, files by kind, models and CLIs); the page warns when `/health` reports a source it could not read |
| v0.38.10 | **A Codex fork's copied turns are counted once**: a resume or subagent rollout opens with its parent's turns copied in, timestamps rewritten but `turn_id` kept; `token_usage_record`s are now keyed by turn and a fork's final drops the turns its parent also carries, so `/usage` and `get_usage_report` stop counting the prefix twice — and a fork's own number no longer includes it. Rollouts too old to have turn ids are summed as before |
| v0.38.9 | **Antigravity** joins the sources: the `agy` CLI and the Antigravity IDE write one shape under `~/.gemini` — a brain directory per conversation with `.system_generated/logs/transcript_full.jsonl`, rewritten step by step and deduplicated by `step_index` — and `conversation_summaries.db` beside each brain answers the list (title, workspace, last-active time) without opening a transcript. `USER_INPUT`'s `<USER_REQUEST>` wrapper is the user's own words, `PLANNER_RESPONSE` the model's answer with agy's token counters, and any other step kind stays visible as an event named by its type. Resume is `agy --conversation <id>`; a private `--gemini_dir` profile is a `--path antigravity=<dir>` |
| v0.38.8 | **Pay per change, not per ask**: SQLite sources answer `List()` / `Usage()` from a fingerprint of `db` / `-wal` / `-shm` / `-journal` (a steady poll costs four stats — `/usage` on a 492 MB opencode database fell from ~70 ms to ~1 ms); file-shaped finals are remembered by `(mtime, size)` and re-read only when their files move; scans reuse a pooled buffer sized by the file, and a line longer than it is gathered once at its own length; message counts warm with up to four workers. Rounds, briefs and the page's fold line also count a round's changes as **code / test / docs / config** by path convention |
| v0.38.6 | **The host opens the page**: a browser arriving at `/` — a navigation asks for `text/html` — is redirected to `/ui`; curl and API clients keep the endpoint index they always got |
| v0.38.5 | **File history is gone** — "which other sessions changed this file" was a question the detail pane answered that nobody kept asking: `/files`, MCP `file_history` and the page's *Files changed* card are removed. A round's own `files` / `filesChanged` stay where they were — in the rounds index, the brief, and the timeline's fold |
| v0.38.4 | **The page shows nothing — fixed**: the storage removal swept `refresh()` (and the error/version/warmup helpers beside it) out of app.js with the panel, so the page booted into an empty list while the API kept answering; the functions are back, and a test now guards the boot path's names |
| v0.38.3 | **The storage report is gone** — a byte count answered a question nobody kept asking, and the walk over every file it cost went with it; the page's header button and panel too. `/usage` and MCP `get_usage_report` are where the numbers live |
| v0.38.2 | **Rolling windows**: the usage report answers 24h / 3d / 7d / 30d / all — a calendar window reads differently depending on which day it is asked |
| v0.38.1 | **Cost out, cache hit in**: the usage report carries no dollar figure — that would mean maintaining a price table — and every row carries `cacheHit` instead, the share of prompt tokens served from cache |
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
