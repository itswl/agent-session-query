# OTLP export

The service can send what it reads to any OpenTelemetry collector as OTLP/HTTP JSON —
traces only, no new dependency, no vendor code. Langfuse is one destination among many: it
is the same exporter with its endpoint and two headers, plus the `langfuse.*` attribute
names every span already carries.

**Off by default.** Export starts only when `--otel-endpoint` is set (and a one-shot
`--otel-export` needs it explicitly). Nothing is sent without it.

## What is sent

One **trace per round** — a user ask and the work that answered it — with a root span and
children:

```
round 12                              langfuse.observation.type=span
├─ model gemini-3.8-flash             type=generation, gen_ai.usage.*
├─ shell                              type=tool, gen_ai.tool.name, status, exit code
├─ Agent                              type=tool
│  └─ subagent general-purpose        type=agent — the subagent's round, its spans beneath
├─ edit                               type=tool
└─ compaction                         type=event
```

Every span carries `langfuse.session.id`, `langfuse.trace.name`, `session.source`,
`session.project` (when known), `round.index`, and `timing.source`; the root also carries
`session.title`. The trace name is the round's ask (its first line, masked, cut to 100
characters) when content is exported, and the session title when it is not — metadata
only keeps the asks at home. Model spans also carry `gen_ai.operation.name=chat`,
`gen_ai.request.model` / `gen_ai.response.model` — the model of that response when the
source names it, the session's last model otherwise — and
`gen_ai.usage.input_tokens` / `output_tokens` / `cache_read.input_tokens` /
`cache_creation.input_tokens` / `reasoning.output_tokens`. Tool spans carry
`gen_ai.tool.name`, `gen_ai.tool.call.id`, `tool.command`, `tool.status` and
`tool.exit_code`. A failed tool is a span with status `ERROR` and the failure text as its
message. A round whose last tool call failed is an `ERROR`; a round that had failed calls
but went on is a `WARNING` (`langfuse.observation.level`) with the count in its status
message — a command that exits 1 and is retried is not a failed round.

**One generation per model response.** Claude Code writes a response as several rows (one
per content block, the tool results in between), every row carrying the response's usage;
the rows share a `response` id and fold into one generation, keyed by the first row, whose
usage is the response's, once (measured: 19,288 of 23,430 local responses were split, up to
18 rows). The response's figures are its final ones: CLI 2.1.284–2.1.304 wrote a running
output count, the first row holding part of it and the last the whole (1,917 of those
responses, about a tenth of their output tokens).
A generation's body is chat messages, the way a backend draws a conversation: its input is
what reached the model since its previous call — the ask for the round's first call, then
the tool results (`role: tool`, by `tool_call_id`) and any prompt typed mid-round — and its
output the response as an assistant message with its `tool_calls`. The ask is not repeated
on every call, and a tool result or a call's arguments is cut at 32 KiB inside them. A row the
CLI made itself (`<synthetic>`, e.g. a resumed session's "No response requested.") is no
generation and does not stretch its round.

A round's root carries `langfuse.trace.tags`: the source, `project:<folder>`, `subagent`
for a subagent that keeps a trace of its own (below), `tool-failure` when a tool call
failed and `failed` when the round ended on one. A generation carries `langfuse.observation.completion_start_time`
(time to first token) where the source records when the response began to arrive —
OpenCode 2.x, from a part's own `time.created` — and `langfuse.observation.cost_details`
when the CLI worked out a cost above zero (Pi's `usage.cost.total`, OpenCode's `cost`). A
zero is a CLI without a price for the model and is not sent: give the backend the price
(Langfuse: Settings › Models) and it prices the tokens itself. Nothing in a transcript is a
prompt from Langfuse's prompt management or a score: those stay empty.

**Subagents hang beneath the call that started them.** A Claude Code subagent is a
transcript of its own, exported as a session of its own, but it ran inside one of its
parent's calls — an Agent call, or the Workflow call that started a workflow's agents. Its
rounds go into the parent round's trace, each round's root an `agent` observation
(`subagent <type>`) beneath the call's span, with the top-level session's
`langfuse.session.id`; one spawned by another subagent hangs beneath that agent's call, in
the same trace. The trace's name and tags stay the parent round's: the subagent's spans
carry none, and say whose they are in `subagent.session.id` (and `subagent.type` on the
root). The call is the one the meta file names (`toolUseId`), or for a workflow's agent
the Workflow call whose result names its run. Measured here, 223 of 228 subagent
transcripts were placed that way; the rest, a team's members, name no call and keep a
trace of their own, tagged `subagent`.

What a source does not record cannot be exported: Hermes keeps usage per session, not
per message, so its generations carry the model (from `state.db` when `sessions.json`
does not name it) but no token counts; Codex and Grok report usage per turn
(`token_usage_record`, `turn_completed`), which lands on the turn's assistant message.

Span and trace ids are derived from source, session, round and step with SHA-256, so a
re-export of the same session sends the same ids: a backend that updates by id (Langfuse
does) treats it as a correction, not a duplicate. That holds within one span format; see
[Upgrading](#upgrading) for what changes between them.

## Bodies are opt-in

By default only metadata leaves the machine: no prompts, no replies, no tool output.
`--otel-content=io` adds them as `langfuse.observation.input` / `output`; secrets are
masked (`RedactSecrets`) and each body is cut at 256 KiB. The session title, project path
and tool names still travel either way.

## Timing: what is recorded and what is inferred

The CLIs disagree about time. Every span says where its times came from, in
`timing.source`:

- **recorded** — the source wrote a duration or both ends (a tool result's `durationMs`).
- **derived** — both ends are real row timestamps; the span is the elapsed time between
  them (tool call row → result row, the round from ask to last work).
- **inferred** — the start is the previous event, because the CLI records only completion
  (model responses on Codex, Claude, Gemini, Grok, Antigravity; OpenCode records both
  ends). A gap of more than 30 minutes is idle or a resumed session, not a call: the span
  is drawn at its completion instead.

Pi and OpenCode measure their calls: an assistant message's `durationMs` from its creation
gives the generation outright, marked **recorded**.

| Source | Model span timing | Tool span timing |
|---|---|---|
| OpenCode | recorded (`time.created` → `time.completed`; 2.x also the first token) | derived, or recorded with `durationMs` |
| Codex | inferred (completion = `token_usage_record`) | recorded/derived from the call and its output, incl. the tool's wall time when present |
| Claude Code | inferred | derived from the call/result rows |
| Pi | recorded (`durationMs`) | derived |
| Gemini, Grok, Hermes, OpenClaw, Antigravity | inferred | derived |

**Model latency is not a measurement.** A generation span runs from the previous event to
the completion, so tool time and user time sit inside it. Dashboards that need real p99
must filter on `timing.source=recorded`; the token counts are exact.

## Flags

| Flag | Meaning |
|---|---|
| `--otel-endpoint` | OTLP/HTTP base URL; `/v1/traces` is appended. `https://cloud.langfuse.com/api/public/otel` for Langfuse |
| `--otel-headers` | comma-separated `name=value`, percent-encoded; e.g. `Authorization=Basic%20<pk:sk>` |
| `--otel-content` | `none` (default) or `io` — prompts, replies and tool output, masked and cut at 256 KiB |
| `--otel-export` | export once and exit, instead of serving. No time limit: a whole-history backfill runs to the end, and Ctrl-C keeps the cursor of what was sent, so the next run goes on from there |
| `--otel-since` | backfill window: `7d` (default), `48h`, a date, or `all` for the whole history |
| `--otel-every` | in server mode, export settled changes on this interval (default `10m`, `0` disables) |
| `--otel-metrics` | also POST delta sums to `/v1/metrics` (rounds, token sums, tool failures); off by default — Langfuse does not take metrics |
| `--otel-settle` | leave a session written more recently than this to a later pass (default `2m`) |
| `--otel-state` | cursor file (default `~/.local/state/agent-session-query/otel.json`); `off` re-sends the window every run |
| `--otel-resend` | ignore the cursor and export the window again |
| `--otel-dry-run` | with `--otel-export`, print the OTLP JSON instead of sending |

## Langfuse

```bash
agent-session-query --otel-export \
  --otel-endpoint https://cloud.langfuse.com/api/public/otel \
  --otel-headers 'Authorization=Basic%20<base64(public-key:secret-key)>,x-langfuse-ingestion-version=4' \
  --otel-since 7d
```

`x-langfuse-ingestion-version: 4` makes spans visible immediately (without it Langfuse may
take up to ten minutes). Langfuse takes traces only; this exporter sends nothing else. Its
UI groups the rounds under the session id, renders generations and tools from the
`langfuse.observation.type` names, and prices tokens with its own model table — the
service sends a cost only where the CLI worked one out (see above).

## Metrics (optional)

`--otel-metrics` adds OTLP sums to `/v1/metrics`, delta temporality (each pass reports what
it sent; a collector adds them up):

| Metric | Attributes | Meaning |
|---|---|---|
| `agent_session.rounds` | `session.source` | rounds exported in this pass |
| `agent_session.round.duration` | `session.source` | their wall-clock time, ms |
| `agent_session.tokens` | `session.source`, `gen_ai.request.model`, `gen_ai.token.type` | input / output / cache_read / cache_creation / reasoning tokens |
| `agent_session.tool.failures` | `session.source`, `gen_ai.tool.name` | failed tool calls |

A pass that is stopped part-way (Ctrl-C, a server shutting down) still posts the sums of
what it sent, with a ten-second deadline of its own.

Langfuse takes traces only, so this is off by default; point it at a collector that stores
metrics (Grafana, Prometheus via the collector) when that reader exists. The numbers come
from the same spans the trace export just built, so both signals agree.

## Continuous export

With an endpoint set and the server running, the first pass covers `--otel-since` (7 days
by default) and every `--otel-every` after that exports sessions whose `UpdatedAt` moved,
skipping any written within `--otel-settle` (so a turn is pressed while it is still being
written). The cursor file remembers the last exported version per session; it is the only
file this service writes, and only when export is on. `--otel-state=off` disables it —
useful for a one-off backfill into a backend that does not update by id.

A session that is exported and then grows is sent again **only for the rounds that
changed**: the cursor keeps a fingerprint per round, so a finished round is never re-sent.
The changed round keeps its deterministic ids, so an id-keyed backend updates it and an
append-only store sees one round twice, not the whole conversation. A round is pressed
once the session has been quiet for `--otel-settle`; a round that changes afterwards (a
late message) is re-sent for that reason alone.

The cursor is version 2: `sessions → {updatedAt, at, rounds → {index: fingerprint}}`.
Version 1 (a session-level mark) is discarded on sight: it cannot say which rounds were
sent, so the next pass re-exports one window rather than guessing.

## State

The cursor lives at `--otel-state` (default `$XDG_STATE_HOME/agent-session-query/otel.json`,
falling back to `~/.local/state/...`), is written atomically and holds no session content —
only versions and round fingerprints:

```json
{
  "version": 2,
  "format": 3,
  "lastRun": "2026-10-28T07:05:02Z",
  "sessions": {
    "opencode|ses_ebc6c0083ffeoPUbq6IcTfGBNV": {
      "updatedAt": "2026-10-28T07:04:25",
      "at": "2026-10-28T07:05:01Z",
      "rounds": {
        "1": "5c0f8a4e9b7d2f13a6c4e8b0d1f3a5c7",
        "2": "9a1d3f5b7c9e0a2d4f6b8c0e1a3d5f7b"
      }
    }
  }
}
```

`--otel-state=off` disables it; `--otel-resend` ignores it for one run. Deleting it makes
the next pass treat the window as new. `format` is the span format the cursor's sessions
were sent in (below); a cursor without it was written before format 2.

## Upgrading

The span format is the shape a transcript takes as spans. It changes when the same
transcript would now be sent under other ids or with other contents, and a backend that
updates by id cannot drop what it got under the old ids: those spans stay beside the new
ones.

**Format 2** (v0.39.7) makes one generation per model response. Under format 1
every row of a Claude Code response was a generation of its own, each with the response's
whole usage, so Langfuse counted those tokens up to 18 times; the round attributes (trace
name, tags, levels) changed as well, so every round exported before is re-sent the next
time its session changes. Run on a cursor of an earlier format, the exporter says so once
on stderr. To start clean:

1. Delete the traces exported before from the backend, or point the exporter at a new
   Langfuse project.
2. Run one backfill over the window you exported before:
   `agent-session-query --otel-export --otel-resend --otel-since all …` (the endpoint and
   headers as usual).

**Format 3** (v0.39.8) hangs a subagent's rounds beneath the call that
started it, in the parent round's trace. Under format 2 each subagent had traces of its
own; the spans move, keeping their ids, but the traces they left stay behind in a backend,
named and tagged and empty. The same clean start applies.

Without that, sessions that never change again keep their earlier spans, and those that do
change carry both.

## Limits

- Full history is read for an export (the API's rounds read only the tail): a very large
  session costs its whole length, once per export, and the round tail cap does not apply.
- Spans are batched at 250 or 8 MiB per POST, retried once, then logged and skipped — a
  collector that is down never fails the service or the request path.
- A subagent hangs beneath its call only when the call can be found: a team's members, and
  an agent whose parent transcript is gone or archived, keep a trace of their own. Placing
  one reads its parent's transcript once per pass.
- Only sessions the service can list are exported; archived sessions are not.
