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
├─ edit                               type=tool
└─ compaction                         type=event
```

Every span carries `langfuse.session.id`, `langfuse.trace.name` (the session title),
`session.source`, `session.project` (when known), `round.index`, and
`timing.source`. Model spans also carry `gen_ai.operation.name=chat`,
`gen_ai.request.model` / `gen_ai.response.model`, and
`gen_ai.usage.input_tokens` / `output_tokens` / `cache_read.input_tokens` /
`cache_creation.input_tokens` / `reasoning.output_tokens`. Tool spans carry
`gen_ai.tool.name`, `gen_ai.tool.call.id`, `tool.command`, `tool.status` and
`tool.exit_code`. A failed tool is a span with status `ERROR` and the failure text as its
message; a round with a failed tool is marked too.

Span and trace ids are derived from source, session, round and step with SHA-256, so a
re-export of the same session sends the same ids: a backend that updates by id (Langfuse
does) treats it as a correction, not a duplicate.

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
  (model responses on Codex, Claude, Pi, Gemini, Grok, Antigravity; OpenCode records both
  ends).

| Source | Model span timing | Tool span timing |
|---|---|---|
| OpenCode | inferred (endpoints are recorded; the message carries both, the exporter uses the row time) | derived, or recorded with `durationMs` |
| Codex | inferred (completion = `token_usage_record`) | recorded/derived from the call and its output, incl. the tool's wall time when present |
| Claude Code | inferred | derived from the call/result rows |
| Pi | inferred | derived |
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
| `--otel-export` | export once and exit, instead of serving |
| `--otel-since` | backfill window: `7d` (default), `48h`, a date, or `all` for the whole history |
| `--otel-every` | in server mode, export settled changes on this interval (default `10m`, `0` disables) |
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
service sends no cost.

## Continuous export

With an endpoint set and the server running, the first pass covers `--otel-since` (7 days
by default) and every `--otel-every` after that exports sessions whose `UpdatedAt` moved,
skipping any written within `--otel-settle` (so a turn is pressed while it is still being
written). The cursor file remembers the last exported version per session; it is the only
file this service writes, and only when export is on. `--otel-state=off` disables it —
useful for a one-off backfill into a backend that does not update by id.

A session that is exported and then grows is exported again under the same trace and span
ids with its new rounds and a corrected last span. Backends that update by id are fine;
append-only stores (Tempo, plain collectors writing object storage) see it twice. That is
the cost of "correct rather than missing" for a live session.

## Limits

- Full history is read for an export (the API's rounds read only the tail): a very large
  session costs its whole length, once per export, and the round tail cap does not apply.
- Spans are batched at 250 or 8 MiB per POST, retried once, then logged and skipped — a
  collector that is down never fails the service or the request path.
- Subagent transcripts are exported as traces of their own; Langfuse can filter them by
  `session.source`, but they are not nested under the parent trace.
- Only sessions the service can list are exported; archived sessions are not.
