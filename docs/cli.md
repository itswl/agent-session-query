# Command line

```
asq <command> [flags] [arguments]
```

Flags may stand anywhere: `asq search retry --since 7d` and `asq search --since 7d retry` are
the same command. `asq <command> -h` lists a command's flags. Output is plain text, coloured
only on a terminal (`--no-color`, or `NO_COLOR`, turns that off); `--json` prints the same
answer as JSON, with the field names of [the record](model.md). Results go to stdout, everything
else to stderr. Exit status: `0` success, `1` the command failed, `2` it was called wrongly.

## Flags every command takes

| | |
|---|---|
| `--json` | JSON instead of text |
| `--only claude,codex` | read only these CLIs' stores (`ASQ_ONLY`) |
| `--path name=dir` | a store outside its default place; `--path name@label=dir` adds one beside it. Repeatable (`ASQ_PATH`, comma-separated). See [Sources](sources.md) |
| `--index file` | the index file (`ASQ_INDEX`; default: the user cache directory) |
| `--no-color` | plain text, even on a terminal |

## Flags that select sessions

Taken by `ls`, `search`, `pack`, `usage`, `failures`, `digest`, `files`, `projects`, `otel`.

| | |
|---|---|
| `--source claude` | one CLI's sessions, or one labeled store (`claude@box2`) |
| `--project api` | sessions whose working directory contains this |
| `--branch main` | sessions that opened on a branch containing this |
| `--since 7d` · `--until 2026-09-01` | a span back from now — `90m`, `12h`, `7d`, `2w` — or a date, or a date and time; `all` for no bound |
| `--archived` | include sessions their CLI has archived |
| `--agents` | include subagents' transcripts as sessions of their own |
| `--limit N` / `-n N` · `--offset N` | paging |

## Finding

**`asq ls [word]`** — recent sessions, newest first. A word narrows to sessions whose title, id
or directory contains it.

**`asq search <query>`** — the turns that match, best first. Each hit shows the turn's ref, the
ask, and the lines the words were found in, labelled with where: `reply`, `thinking`, a tool's
`input`, `command` or `output`.

| query | finds turns where |
|---|---|
| `retry backoff` | both words appear, anywhere in the turn |
| `"exit status 1"` | the phrase appears as written |
| `flaky -windows` | `flaky` appears and `windows` does not |
| `ask:deploy` | the person's ask has the word; also `reply:` `thinking:` `tool:` (a tool's input) `output:` (a tool's output) |
| `rebind` | any word *starting* with it — bare words are prefixes; a quoted phrase is exact |
| `数据库` | the characters appear together, in any position |

Punctuation separates words, as it does in the index: `session_query`, `session-query` and
`session query` are the same search. `--recent` orders by time instead of relevance;
`--per-session N` bounds how many turns of one session are shown (default 3, `0` for no bound).

**`asq files <path>`** — the turns that named a file, newest first, each with the ask that led
to it. `--changed` keeps only turns that changed it.

**`asq projects`** — the working directories sessions ran in, most recently used first.

## Reading

**`asq show <ref>`** — at three depths:

| | |
|---|---|
| `asq show claude/3f2a` | the outline: every turn's ask and how it ended, and the session's subagents |
| `asq show claude/3f2a#4` · `--turns 3-5` · `--turns last` · `--turns all` | those turns step by step, each step numbered `[seq]` |
| `asq show claude/3f2a --seq 12` | one step whole: a tool call's full input and output |

Step by step, a tool call is one line — name, command, outcome — and only a failed one shows the
end of its output. `--full` prints every output and all reasoning whole. An outcome is `✓`,
`✗ failed`, `⚠ interrupted` or `(no result)`; a call with no mark at all is one whose CLI did not
record how it ended.

`asq show <ref> -f` **follows** a session: it prints the latest turn, then each step as the agent
takes it — a new turn's opening line when one starts, and a tool call a second time when the
result it was waiting for arrives — until interrupted. It is `tail -f` for an agent that is
working somewhere you are not looking.

**`asq export <ref> --format md|json|jsonl|html`** — the whole session, to stdout. Markdown and
HTML are documents; JSON is the record; JSONL is one tagged object per line (`session`, `turn`,
`item`), so a `jq` query is a filter rather than a parse. The HTML page stands alone: no
scripts, no requests.

**`asq resume <ref>`** — prints the command that reopens the session in the CLI that wrote it,
including the `cd` it needs (`cd /work/api && claude --resume …`). `--run` runs it.

## Handing over

**`asq brief <ref>`** — a handoff brief, in Markdown: the session's turns in one line each, then
for the latest turn what was asked, which files changed, the tool calls that failed (command,
exit status, last line of output), the tokens it took and how it ended, then where things stood
at the end and how to dig deeper. It is extracted from the record, not written by a model —
deterministic, instant, and unable to invent.

- `--turn N` briefs another turn.
- `--since 2h` briefs everything after a moment, opening with what the stretch adds up to — the
  brief for handing the same session over a second time.

**`asq pack`** — several sessions as one document, oldest first: for each, what was asked and
what it concluded. `--project` and `--since` choose them; `--turns` spells out every turn. A
pack is a record, not a summary: where two sessions disagree, the later one was said later, and
nothing in it claims to be what is true now.

## Measuring

**`asq usage`** — rolling windows (24h, 3d, 7d, 30d, all), then model, source and project
breakdowns: calls, input, output, total tokens, cache hit rate, and cost where the CLI recorded
one. `--by model|day|source|project|session` prints one breakdown, `--since` bounds it. Token
counts are in [one shape](model.md#usage-one-shape) across CLIs. A cost is only ever one a CLI
computed itself; `asq` holds no price list.

**`asq failures`** — tool calls that failed or were interrupted in the last 7 days (`--since`),
newest first, with the count per tool on top. `--tool Bash` narrows to one tool, named exactly —
Claude Code's `Bash` is not Pi's `bash` — and `--tool 'mcp__playwright*'` to every tool whose
name begins that way.

**`asq digest`** — the last 7 days (`--since`) per project: sessions, turns, tool calls,
failures, tokens, files changed by kind (code / test / docs / config), models and CLIs used.

## Serving

**`asq serve`** — the page and the HTTP API, on `127.0.0.1:7717`. See [Deploying](deploy.md).

| | |
|---|---|
| `--host`, `--port` | where to listen (`ASQ_HOST`, `ASQ_PORT`). Anything but loopback requires a token |
| `--token` | what every caller must hold. Prefer `ASQ_TOKEN`: a flag shows in the process list |
| `--cors-origin URL` | another origin whose pages may call the API (repeatable; `ASQ_CORS_ORIGIN`) |
| `--open` | open the page in the browser |
| `--page-dir DIR` | serve the page from a directory, re-read on every request — for working on it |

**`asq ping`** — exits 0 if a server is answering on `--port`; what a container's health check
runs.

**`asq mcp`** — an MCP server on stdio. See [MCP](mcp.md).

**`asq otel`** — export to an OpenTelemetry collector. See [Telemetry](telemetry.md).

## Maintaining

**`asq doctor`** — for each store: whether it was found, how many transcripts it lists, and
whether its newest conversation reads whole. Listing alone proves little — a format that drifted
lists fine and reads as empty — so it reads one. Exits 1 if a store that is there cannot be
read.

**`asq index`** — where the index is and what it holds. `asq index path` prints the file;
`asq index rebuild` reads everything again; `asq index rm` deletes it (the next command rebuilds
it).

## Environment

| | |
|---|---|
| `ASQ_INDEX` | the index file |
| `ASQ_ONLY` | read only these CLIs, comma-separated |
| `ASQ_PATH` | stores outside their default place: `name[@label]=dir`, comma-separated |
| `ASQ_HOST`, `ASQ_PORT`, `ASQ_TOKEN`, `ASQ_CORS_ORIGIN` | for `asq serve` |
| `ASQ_STATE` | where the export ledger is kept |
| `NO_COLOR` | plain text |

Setting `ASQ_ONLY` and `ASQ_PATH` in the environment rather than as flags is what keeps a page
server, an MCP server some agent started and a command typed in a terminal reading the same
stores.
