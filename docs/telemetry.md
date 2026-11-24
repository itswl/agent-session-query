# Telemetry: sessions as traces

`asq otel` sends the record to an OpenTelemetry collector — Langfuse, Phoenix, Tempo, Jaeger,
an OTel Collector, anything that takes OTLP over HTTP. Nothing is sent unless you run it.

```sh
asq otel --endpoint https://collector.example:4318
```

## What a turn becomes

One trace per turn:

```
turn                       the root: the ask, how long the work took, how it ended
├─ chat claude-opus-5      one span per model response, with its tokens
│  ├─ execute_tool Bash    the calls that response made, beneath it
│  └─ execute_tool Agent
│     └─ turn              a subagent that call started: its own turns, in this trace
├─ chat claude-opus-5
└─ compaction              what happened between the words, as an event
```

- **Names and attributes** follow the OpenTelemetry GenAI conventions (`gen_ai.operation.name`,
  `gen_ai.request.model`, `gen_ai.usage.*`, `gen_ai.tool.name`, `gen_ai.conversation.id`), plus
  the `langfuse.*` attributes Langfuse needs to show a generation as a generation, group traces
  by session, and carry tags, levels and costs.
- **Tokens** are sent the way the conventions count them — the input is everything the model was
  shown, cached or not, with the cached parts named beside it; the output includes reasoning.
  The record keeps those parts apart in [one shape](model.md#usage-one-shape) for every CLI, so
  this is one sum rather than a table of who counts what.
- **Timing.** A CLI writes a response's rows as they finish, so a response's first row is not
  when the model was asked. Each response's span starts when whatever came before it ended — the
  ask, or the last result of the calls before — which is the time the model took, and what a
  timeline is read for. Where a CLI measured a duration itself, that is used. Every such span
  says which, in `asq.timing`: `recorded`, `derived` or `inferred`.
- **A subagent** hangs beneath the tool call that started it, in its parent's trace, found by
  the call's id (or the workflow run the call began). Its traces are filed under the session it
  ran in.
- **A CLI that counts tokens per session, not per response** (Hermes) has model-call spans
  without token counts: there is no response to hang a session's total on. `asq usage` still
  reports those totals.
- **A failed tool call** is a span with an error status; a turn with failures is marked
  `WARNING` and tagged `tool-failure`.
- **A turn whose work ended on an error** ([the record](model.md#session--turn--item)) has a
  root with an error status, marked `ERROR` and tagged `error`. Its status message is what the
  error said, the first line of it, from `--content asks` on, and *ended on an error* below.

## How much leaves the machine

`--content` decides, and the default is the least:

| | sent |
|---|---|
| `none` (default) | timings, models, token counts, tool names and how each ended, the project's directory *name*. **No text of the conversation.** A trace is named `claude turn 4` |
| `asks` | also what the person asked and what the agent answered — or the error its work ended on — on the turn's root; traces are named after the ask; the full working directory |
| `all` | also everything in between: reasoning, every tool's input and output, commands, file paths |

At every level, secret-shaped strings are masked and terminal control sequences removed, and one
body is held to 256 KiB — the head and the tail kept. An endpoint that is plain `http://` on
another machine gets a warning when content is on.

## Sent once

Span and trace ids are derived from what a span *is* — the store, the session, the turn, the
step — not from when it was sent. The same turn always produces the same ids, so on a backend
that keys by id a turn sent twice is one turn.

On top of that, a **ledger** remembers for each collector the hash of every turn it took. A turn
whose hash has not moved is not sent again; one that grew is sent again under the same ids.
Changing `--content` sends again too: what the collector has is not what was asked for now.

The ledger is deliberately not part of the index. The index is a cache that can be deleted at
any time; what a collector has already been sent cannot be read back off the transcripts. It
lives with the program's state (`ASQ_STATE`; otherwise `$XDG_STATE_HOME/asq`,
`~/.local/state/asq` on Linux, the user configuration directory elsewhere) and names turns by
what they are, so it survives every rebuild.

A turn still being written waits: `--settle` (default `2m`) holds a turn back until it has been
quiet that long, so it is sent once, whole, rather than again with every row.

If the collector is down, the export stops with an error and what was not taken is not written
down as sent; the next run picks up there.

## Flags

| | |
|---|---|
| `--endpoint URL` | the collector: its base address (`/v1/traces` is appended) or the traces path itself |
| `--header Name=value` | a header to send; repeatable |
| `--content none\|asks\|all` | how much of the conversation is sent |
| `--since 7d` | which turns: asked within this window. The default is the last 7 days — a first run does not send a year of history by surprise; `--since all` does |
| `--source`, `--project`, `--branch`, `--until` | narrow the selection |
| `--settle 2m` | leave a turn until it has been quiet this long |
| `--every 10m` | keep running, exporting this often |
| `--resend` | send every selected turn again, whatever the ledger says |
| `--dry-run` | print the OTLP requests to stdout instead of sending them, a line of JSON each; at a terminal, only what they hold, unless with `--json` |

The endpoint and headers also come from the variables OpenTelemetry's own tools read —
`OTEL_EXPORTER_OTLP_ENDPOINT` (or `…_TRACES_ENDPOINT`) and `OTEL_EXPORTER_OTLP_HEADERS`.

## Langfuse

With `LANGFUSE_PUBLIC_KEY` and `LANGFUSE_SECRET_KEY` set, no endpoint is needed: spans go to
`LANGFUSE_HOST` (default `https://cloud.langfuse.com`) with the keys as Basic auth.

```sh
export LANGFUSE_PUBLIC_KEY=pk-lf-… LANGFUSE_SECRET_KEY=sk-lf-…
asq otel --content asks --every 10m
```

Each turn is a trace, the model responses are generations with usage (and cost, where the CLI
computed one — otherwise Langfuse prices it from the model name), and a session's traces are
grouped under its id. Tags: the store, `project:<name>`, `subagent`, `tool-failure`, `error`,
`interrupted`.

## Seeing what would be sent

```sh
asq otel --dry-run --since 1d --content none | jq '.resourceSpans[].scopeSpans[].spans[] | {name, attributes: [.attributes[].key]}'
```

`--dry-run` neither sends nor writes the ledger. Run it at the level you intend to use before
pointing `asq otel` at a collector someone else operates. The requests go to a pipe or a file as
they would to the collector, one line of JSON each and often hundreds of kilobytes; at a
terminal it says only how many turns, spans and requests they hold, and how to read them —
`--json` prints them there too.
