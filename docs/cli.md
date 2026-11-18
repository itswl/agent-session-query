# Command line

```
asq <command> [flags] [arguments]
```

Flags may stand anywhere: `asq search retry --since 7d` and `asq search --since 7d retry` are
the same command. `asq <command> -h` lists a command's flags; `asq` alone lists the commands
under the version it is, and `asq --version` says only that. Output is plain text, coloured
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
| `--project api` | sessions whose working directory contains this. `.`, `..` or a path that begins with one (`./web`) is that directory instead: the sessions that ran in it or below it |
| `--branch main` | sessions that opened on a branch containing this |
| `--since 7d` · `--until 2026-09-01` | a span back from now — `90m`, `12h`, `7d`, `2w` — or a date, or a date and time; `all` for no bound |
| `--archived` | include sessions their CLI has archived |
| `--agents` | include subagents' transcripts as sessions of their own |
| `--limit N` / `-n N` · `--offset N` | paging |

`asq ls --project .` is the sessions of the directory you are in. The directory is resolved to
an absolute path where `asq` runs, and a session is in it whether its CLI recorded the path the
shell gives or the one its symbolic links lead to (`/tmp/x` and `/private/tmp/x` on a Mac);
`/work/api` holds `/work/api/web` and not `/work/api-old`. Paths are compared without regard
to case. An empty answer says on stderr which directory was looked in, since `.` is wherever
the command happened to run.

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
to it. `--changed` keeps only turns that changed it. Each says how the file stands now against
what that turn left: *as this turn left it* (the text it wrote is there, and nothing has written
the file since, or it is to the byte the whole of what the turn wrote), *written since* (with that text still in it, or by its modification time alone),
*changed since* (the text is not all there), *not written since*, *deleted* — the difference
between a history that still holds and one that has moved on
([how it is judged](api.md#reports)). Only this machine's files are judged.

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

**With no ref**, `asq show`, `asq show -f` and `asq brief` take the newest session that ran in
the current directory or below it, as `--project .` finds them — in a repository, the one the
agent working there is writing, or last wrote. Which one they took is said on stderr, beside
the answer; where no session ran, they fail and say which directory they looked in.

**`asq export <ref> --format md|json|jsonl|html`** — the whole session, to stdout. Markdown and
HTML are documents; JSON is the record; JSONL is one tagged object per line (`session`, `turn`,
`item`), so a `jq` query is a filter rather than a parse. The HTML page stands alone: no
scripts, no requests.

**`asq resume <ref>`** — prints the command that reopens the session in the CLI that wrote it,
including the `cd` it needs (`cd /work/api && claude --resume …`). `--run` runs it: on macOS and
Linux asq hands itself over to the agent, in the session's directory, so that the terminal and its
signals are the agent's alone.

## Handing over

**`asq brief <ref>`** — a handoff brief, in Markdown: the session's turns in one line each, then
for the latest turn what was asked, which files changed, the tool calls that failed, the tokens
it took and how it ended, then where things stood at the end and how to dig deeper. It is
extracted from the record, not written by a model — deterministic, instant, and unable to
invent.

It says where things stand now, as `failures` and `files` do:

- **A failed call** whose command later passed in the session — `go test` after `go test`, in
  that turn or the next three, by [the rules `failures` uses](api.md#reports) — says where
  (`Failed, passed later: … · passed 40m later: #5 step 212`), and how it was run then if that
  differs. It is a step of the work, not an open problem: a turn whose failures all passed later
  is marked `✓` in the list of turns, `1 failed, passed later`. A failed call that did not pass
  later quotes the last four lines of what it printed, which is where a command says why.
- **A changed file** says how it stands now against what the turn left: *as this turn left it*,
  *written since*, *changed since*, *not written since*, *deleted* — the words of `files`, judged
  the same way, and only for this machine's files. A file in a folder macOS is asking about is
  *not looked at*, and the brief does not wait for the answer. Files beneath the session's
  directory are named relative to it.

Which turns it spells out:

- `--turn N` briefs another turn.
- `--from N` briefs turn N and every turn after it; `--since 2h` everything after a moment;
  `--since all` the whole session. Each opens with what the stretch adds up to — every file it
  changed, against the last of its turns that changed it, the tool calls, the tokens — and then
  spells out its latest twelve turns. This is the brief for handing the same session over a
  second time, or whole.
- With no ref it briefs the newest session here, [as `show` takes it](#reading).

**`asq pack`** — several sessions as one document, oldest first: for each, what was asked and
what it concluded. `--project` and `--since` choose them; `--turns` spells out every turn. A
pack is a record, not a summary: where two sessions disagree, the later one was said later, and
nothing in it claims to be what is true now.

## Measuring

**`asq usage`** — rolling windows (24h, 7d, 30d, all), then model, source and project
breakdowns: calls, the prompt (`in`), the output, how much of the output was reasoning, total
tokens and cache hit rate. `--by model|day|source|project|session` prints one breakdown,
`--since` bounds it. Token counts are in [one shape](model.md#usage-one-shape) across CLIs. A
dash under `reasoning` means the provider did not say, which is not the same as none.

The report is in tokens, not money. `asq` holds no price list, and the costs some CLIs work out
for themselves do not add up to one: summed over sessions most of which recorded none, the
figure would read as the cost of tokens it is not the cost of. What every CLI does record, and
what moves a bill most, is the last column — how much of the prompt was served from cache. A
session's own cost, where its CLI recorded one, is shown with the session (`asq show`).

**`asq failures`** — tool calls that failed or were interrupted in the last 7 days (`--since`),
newest first, with the count per tool on top. `--tool Bash` narrows to one tool, named exactly —
Claude Code's `Bash` is not Pi's `bash` — and `--tool 'mcp__playwright*'` to every tool whose
name begins that way.
When the same program and subcommand succeeded later in the session — `go test` after
`go test`, in that turn or the next three — the failure says where
(`↳ passed 40m later: claude/3f2a…#5 step 212`), and how it was run if that differs; the turns
between are how it was got past. Succeeded means it said so: exit 0, with nothing on the line
after it that could hide a failure (`| tail`, `|| true`), and not merely started in the
background ([the rules](api.md#reports)).

**`asq digest`** — the last 7 days (`--since`) per project: sessions, turns, tool calls,
failures, tokens, files changed by kind (code / test / docs / config), models and CLIs used.

## Serving

**`asq serve`** — the page and the HTTP API, on `127.0.0.1:7717`. See [Deploying](deploy.md).

| | |
|---|---|
| `--host`, `--port` | where to listen (`ASQ_HOST`, `ASQ_PORT`). Anything but loopback requires a token |
| `--token` | what every caller must hold. Prefer `ASQ_TOKEN`: a flag shows in the process list |
| `--cors-origin URL` | another origin whose pages may call the API (repeatable; `ASQ_CORS_ORIGIN`) |
| `--proxy-header NAME` | behind a proxy: the header in which it names the caller, so that wrong tokens are counted per caller (`ASQ_PROXY_HEADER`). Takes a token: without one, `asq serve` does not start. See [Deploying](deploy.md#who-may-ask) |
| `--open` | open the page in the browser |
| `--page-dir DIR` | serve the page from a directory, re-read on every request — for working on it |

**`asq ping`** — exits 0 if a server is answering on `--port`; what a container's health check
runs.

**`asq mcp`** — an MCP server on stdio. See [MCP](mcp.md).

**`asq otel`** — export to an OpenTelemetry collector. See [Telemetry](telemetry.md).

## Maintaining

**`asq doctor`** — for each store: whether it was found, how many transcripts it lists and the
day the oldest of them was last written, and whether its newest conversation reads whole. Of a
CLI that deletes its own old sessions — Claude Code and Gemini CLI, after 30 days unused by
default — it names the setting that decides it: what the CLI deletes leaves the index too, so
that day is as far back as `asq` goes for it. A labelled store is a copy or another machine's,
which this CLI does not sweep, and gets no such line. Listing alone proves little — a format that drifted
lists fine and reads as empty — so it reads one, and then the next two, and lists what the three
held that the adapter did not understand: records a CLI has started to write since the adapter
was written. Those are a warning (see [Sources](sources.md#what-each-adapter-reads)). Sessions
whose CLI moved them, whole or in part, where `asq` does not read them (OpenClaw's cold storage)
are counted, and are not taken for a format that drifted. Exits 1 if a store that is there cannot
be read, or if doctor was stopped before it looked at every store. Its first line is the version
of `asq` that read them, which is what a report of a store that will not read needs first.

**`asq index`** — where the index is and what it holds. `asq index path` prints the file;
`asq index rebuild` reads everything again; `asq index rm` deletes it (the next command rebuilds
it), and with it any index an earlier version of `asq` left beside it, which `asq index` names. A
new release that reads a CLI's files differently has that CLI's sessions read again by itself, at
the first sync after the upgrade; a rebuild is not needed for that.

## Environment

| | |
|---|---|
| `ASQ_INDEX` | the index file |
| `ASQ_ONLY` | read only these CLIs, comma-separated |
| `ASQ_PATH` | stores outside their default place: `name[@label]=dir`, comma-separated |
| `ASQ_HOST`, `ASQ_PORT`, `ASQ_TOKEN`, `ASQ_CORS_ORIGIN`, `ASQ_PROXY_HEADER` | for `asq serve` |
| `ASQ_STATE` | the directory for what must outlive the index: the export ledger, and the secret a server signs its cookies with |
| `NO_COLOR` | plain text |

Setting `ASQ_ONLY` and `ASQ_PATH` in the environment rather than as flags is what keeps a page
server, an MCP server some agent started and a command typed in a terminal reading the same
stores.
