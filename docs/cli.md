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
| `--since 7d` · `--until 2026-09-01` | a span back from now — `90m`, `12h`, `7d`, `2w` — or a date, or a date and time; `today`, `yesterday` or a weekday (`monday`, `mon`) for the midnight that day began with, a weekday's the latest, today included; `all` for no bound |
| `--archived` | include sessions their CLI has archived |
| `--agents` | include subagents' transcripts as sessions of their own |
| `--limit N` / `-n N` · `--offset N` | paging |

A day is the one on the clock of the machine `asq` runs on: `--since yesterday --until today`
is yesterday there, midnight to midnight.

`asq ls --project .` is the sessions of the directory you are in. The directory is resolved to
an absolute path where `asq` runs, and a session is in it whether its CLI recorded the path the
shell gives or the one its symbolic links lead to (`/tmp/x` and `/private/tmp/x` on a Mac) —
whichever of the two `asq` is given, as a process with no `PWD`, or after `cd -P`, is given the
second; `/work/api` holds `/work/api/web` and not `/work/api-old`. Paths are compared without regard
to case. An empty answer says on stderr which directory was looked in, since `.` is wherever
the command happened to run.

When no store holds a session at all, every command says so on stderr beside its answer, with
where it looked — each store's place, and whether it is there and could be read — and that
`asq doctor` says more.

## Held to a budget

`show`, `search`, `brief` and `pack` take `--max-chars N`: the answer is held to about N
characters (at least 500), as an [MCP tool's](mcp.md#how-the-answers-are-shaped) is — for an
agent that reads `asq` from a shell, and pays for what it reads by the token. An outline keeps
its latest turns; turns read step by step leave out whole turns; a brief is written to fit,
and keeps where things stood at the end. What is still over — an outline whose latest four
turns overrun it, a brief whose head and end do — and anything else is cut on a line
boundary. Each says what it left out and the flag that asks for it. It is for text: not with
`--json`, which is several times the size, nor `show -f`. Without it, an answer is as long as
it is.

## Finding

**`asq ls [word]`** — recent sessions, newest first. A word narrows to sessions whose title, id
or directory contains it. A session written to in the last two minutes is marked `●`; where its
transcript tells ([which CLIs](sources.md#where-a-turn-stands)), its line opens with where its
latest turn stands — `working`, `waiting on Bash: go test ./...` (a command still running, or a
prompt asking permission to run it: transcripts do not tell the two apart), or `idle`, the
agent done and the person to answer. `--state` keeps the sessions that stand so: `--state
waiting,idle` is what waits on you. Working is said for ten minutes after the transcript was
last written, waiting and idle for a day ([why](api.md#sessions)).
A session's failed calls are counted with how many of them the same command passed later, as
`failures` [judges it](#measuring): `2 failed, 1 passed later`, in red while one has not, muted
once each has.

**`asq search <query>`** — the turns that match, best first. Each hit shows the turn's ref and
how the turn ended — `✓`, `✗2 failed` (with how many of those calls the same command passed
later, in that turn or the three after it: `✗2 failed, 1 passed later`; once each has,
`✓ 1 failed, passed later`), `✗ error` when its work ended on an error, `⚠ interrupted` —
then the ask, the lines the words were found in, labelled with where (`reply`, `thinking`, a
tool's `input`, `command` or `output`, the session's `title`), the turn's `answer`, and the
first line of the `error` it ended on: enough to tell which turn to open without opening each.

| query | finds turns where |
|---|---|
| `retry backoff` | both words appear, anywhere in the turn — a session's first turn has its title too |
| `"exit status 1"` | the phrase appears as written |
| `flaky -windows` | `flaky` appears and `windows` does not |
| `ask:deploy` | the person's ask has the word; also `reply:` `thinking:` `tool:` (a tool's input) `output:` (a tool's output) `title:` (the session's title) |
| `rebind` | any word *starting* with it — bare words are prefixes |
| `retries` | … or any other form of the same English word: `retried`, `retry`, `retrying` |
| `3f9c2ab4e5f6…` | a commit's id, as long as it is given: also the turn whose `git commit` printed seven characters of it |
| `数据库` | the characters appear together, in any position |

Punctuation separates words, as it does in the index: `session_query`, `session-query` and
`session query` are the same search. A word alone, unquoted, finds the other forms of an English
word by its stem, Porter's, as SQLite's FTS5 has it: `logs` finds `log` and `logging` but not
`login`, `deploy` finds `deployment`. A quoted word or phrase, a word of several parts
(`session_query`), one with digits in it, and words in other scripts match as written — except
a commit's id: eight to sixty-four hexadecimal digits, letters and digits both, also finds the
same id cut short to as few as seven, the way `git commit` and `git log --oneline` print it, so
the whole id GitHub shows finds the turn that made the commit. An excluded word is excluded as
typed, by how a word begins: `-windows` leaves in a turn that says only `window`. What is typed
after a word says that it ends there: `c++` finds the word *c* — the index holds no `+` — and
not every word that begins with c; a `*` says the opposite, and `retr*` finds what `retr` does.

The session's title is searched with its first turn, so a word of it finds the session once,
there: `webhook retries` finds the session titled *Cap webhook retries* at its first turn,
whatever that turn says, and its other turns where they say both words themselves. It decides
which turns are found, not which come first: that is for their own words. A session its CLI did
not name is titled by how its first ask begins, and `title:` finds that title too.

When no turn holds every word, the answer says so, names any word no turn holds at all, and
lists the turns that hold the most of the rest instead, closest first, each marked with the
words it `lacks`:

```console
$ asq search webhook retries queue
No turn holds all 3 words; none holds “queue”.
Partial matches, closest first: 2 turns in 1 session, the closest holding 2 of the 3.

1. claude/8a80b919-fbeb-4925-968b-46642528af23#1  2026-11-13 10:04 · ~/work/billing-api  ✓
   lacks  queue
   ask    Failed webhook deliveries are retried every minute, forever. Cap the attempts and back …
   answer Each retry now waits twice as long as the one before — exponential backoff from a …
```

A search some turn matches whole lists those turns and no others, and one that turns would
match whole but for a word it excludes lists none.

When the filters given — `--since`, `--project`, `--source`, `--branch`, `--until` — keep a
search from matching whole, the answer speaks of the turns they select, and says how many
turns outside them match it and which flags to drop to see them; or that none do, so that
dropping them is not tried for nothing, and which of its words turns outside them hold. A search
leaves out archived sessions unless `--archived` is given, and says the same of a match only
they hold:

```console
$ asq search retry --since 1h
No turn the filters select holds “retry”.
7 turns in 7 sessions outside them match: drop --since to see them.

$ asq search retry zeromq --since 1h
No turn the filters select holds “retry” or “zeromq”.
None outside them matches either, though some hold “retry”.
```

`--recent` orders by time instead of relevance; `--per-session N` bounds how many turns of one
session are shown (default 3, `0` for no bound).

**`asq files <path>`** — the turns that named a file, newest first, each with the ask that led
to it. `--changed` keeps only turns that changed it. Each says how the file stands now against
what that turn left: *as this turn left it* (the text it wrote is there, and nothing has written
the file since, or it is to the byte the whole of what the turn wrote), *written since* (with that text still in it, or by its modification time alone),
*changed since* (the text is not all there), *not written since*, *deleted* — the difference
between a history that still holds and one that has moved on
([how it is judged](api.md#reports)). Only this machine's files are judged.

**`asq why <file>:<line>`** — which session wrote a line of a file, and why. `git blame` names the
commit that last changed the line, and the answer is the turns whose calls made that commit —
those whose `git commit` printed its id, however much of it — then the other turns that name it,
each shown as `search` shows a turn. git is only asked to read: it takes no lock and writes
nothing in the repository. A line committed outside the sessions asq reads, or not committed yet,
is said to be, with the `asq files` that names the turns that changed the file.

**`asq projects`** — the working directories sessions ran in, most recently used first.

## Reading

**`asq show <ref>`** — at three depths:

| | |
|---|---|
| `asq show claude/3f2a` | the outline: where the latest turn stands, every turn's ask and how it ended — `✗2` for failed calls, `✓` with `1 failed, passed later` once the same command passed later for each, `✗` and the first line of the error for a turn whose work ended on one, `⚠` for one the person stopped — and the session's subagents |
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
signals are the agent's alone. A session read from a store added by name (`claude@box2`) is not
in its CLI's store here, which is where the CLI looks: resume prints nothing and fails, saying the
command for the machine it came from. `show` and `brief` give that one as
`resume where claude@box2 came from: …`.

## Handing over

**`asq brief <ref>`** — a handoff brief of the whole session, in Markdown: its turns in one line
each; what it added up to — every file it changed, against the last of its turns that changed
it, the tool calls, the tokens; its latest twelve turns spelled out, each with what was asked,
which files changed, the tool calls that failed, the tokens it took and how it ended — its
answer, or, for a turn whose work ended on an error, that it failed and what the error said;
then where things stood at the end, which for a session whose latest turn failed is that
failure first, and how to dig deeper. It is extracted from the record, not written by a
model — deterministic, instant, and unable to invent.

The whole session is what a handoff needs: a session whose last ask was "delete the scratch
script" hands over, in its latest turn, only that, and not the fix made before it. The page's
brief opens at the latest turn, with the whole session beside it ([The page](ui.md#a-session)).

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

Which turns it covers:

- `--turn N` briefs that turn alone, and so does a ref that ends `#N`.
- `--from N` briefs turn N and every turn after it; `--since 2h` everything after a moment.
  Each opens with what that stretch adds up to, as the whole session's brief does, and spells
  out its latest twelve turns. This is the brief for handing the same session over a second
  time.
- With no ref it briefs the newest session here, [as `show` takes it](#reading).

`--turn`, `--from`, `--since` and a ref's `#N` each choose the turns, so a brief takes one of
them: given two, it says which and briefs nothing.

**`asq pack`** — several sessions as one document, oldest first: for each, what was asked — the
first ask, and of a session of several turns the last one too — and what it concluded.
`--project` and `--since` choose them; `--turns` spells out every turn. A pack is a record, not
a summary: where two sessions disagree, the later one was said later, and nothing in it claims
to be what is true now.

## Measuring

**`asq usage`** — rolling windows (24h, 7d, 30d, all), then model, source, project and session
breakdowns: calls, the prompt (`in`), the output, how much of the output was reasoning, total
tokens and cache hit rate. `--by model|day|week|month|source|project|session` prints one
breakdown, `--since` bounds it. Days, weeks and months are this machine's calendar's, a week
named by the Monday it begins on, and come in order, the latest `--limit` of them; the other
breakdowns come largest first. A session is named as `ls` names it, with its title and
directory beneath its figures; a subagent's calls are counted in the session it ran in.

Every breakdown but by model — a turn may call on several models — ends with `agent time`:
how long agents worked on the turns asked in that group, from each ask to the last thing the
agent did, summed. Two agents at work at once count twice, as two people would; a subagent's
time is inside the turn that started it, and is not counted again. A dash is a group with no
turn asked in it — a day whose calls were all of a turn asked the day before — or none whose end
was recorded. Token counts are in [one shape](model.md#usage-one-shape) across CLIs. A dash under
`reasoning` means the provider did not say, which is not the same as none.

The report is in tokens, not money. `asq` holds no price list, and the costs some CLIs work out
for themselves do not add up to one: summed over sessions most of which recorded none, the
figure would read as the cost of tokens it is not the cost of. What every CLI does record, and
what moves a bill most, is the last column — how much of the prompt was served from cache. A
session's own cost, where its CLI recorded one, is shown with the session (`asq show`).

**`asq failures [words]`** — tool calls that failed or were interrupted in the last 7 days
(`--since`), newest first, with how many of them the same command passed later and the count
per tool on top. `--tool Bash` narrows to one tool,
named exactly — Claude Code's `Bash` is not Pi's `bash` — and `--tool 'mcp__playwright*'` to
every tool whose name begins that way. Words narrow to the calls whose command or the end of
whose output (the part shown) holds each of them, anywhere and in any case: `asq failures
ECONNREFUSED`, or `asq failures '"go test" -race'` for a phrase as written and a word that must
not be there. The answer names the window it looked in; when no call in it matches, it says how
many match before it — `No failed calls hold those words in the last 7 days; 1 from before then:
--since all.` — since a week is all it looks at unless told otherwise.
When the same program and subcommand succeeded later in the session — `go test` after
`go test`, in that turn or the next three — the failure says where
(`↳ passed 40m later: claude/3f2a…#5 step 212`), and how it was run if that differs; the turns
between are how it was got past. Succeeded means it said so: exit 0, with nothing on the line
after it that could hide a failure (`| tail`, `|| true`), and not merely started in the
background ([the rules](api.md#reports)).

A failure that did not pass later is *open*: `--open` keeps those alone, and says how many of the
rest it left out (`2 of 7 failures still open`). `--by command` prints one entry per command
that failed instead — the program and subcommand, as passing later is judged: `go test`,
`terraform plan`, or for a tool that runs none, the tool — with how often it failed, how many of
those are still open, in how many sessions, and the latest of them; the commands with failures
still open first, then those that failed most. `asq failures --by command --open` is what is
still broken, and how often it broke.

**`asq digest`** — the last 7 days (`--since`) per project: sessions, turns, tool calls,
failures and how many of them the same command passed later, tokens, files changed by kind
(code / test / docs / config), models and CLIs used. Then the sessions behind the counts, the
latest twelve of each project: the ref, when it was last at work in the window, its turns and
tool calls there, how they ended as a search's hit says it (`✓`, `✗2 failed, 1 passed later`,
`✓ 1 failed, passed later`, `✗ error`, `⚠ interrupted`), its title, the last thing its agent
said, and the error its last turn there ended on; a subagent's work is in the session it ran in. A project with more says how to list them.
Passing later is judged as `failures` judges it.

It says how long agents worked, as `usage` counts it, and on how many days, in all, per project
and per session, and draws when: a row for each weekday, Monday first, and a mark for each hour
of this machine's clock, as high as the time agents worked in it against the busiest hour, each
day's time at the end of its row. A turn's time falls in every hour it ran through.

The digest, and `usage` given `--since` or `--until`, open with the moments they cover on the
clock of the machine `asq` runs on — `Since Mon 2026-11-23 00:00 +08:00` — which is what
`today` was taken to mean.

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

**`asq service install | status | uninstall`** — keeps `asq serve` running under the system's
service manager: a LaunchAgent under launchd, a user unit under systemd, a task at logon in Task
Scheduler. `install` writes its definition — this binary by its full path, then `serve` with
the `--host`, `--port`, `--proxy-header`, `--cors-origin`, `--index`, `--only` and `--path` given,
and the variables `asq` reads as this shell has them (a task has the user's environment instead,
and install names those it gets only from there) — has the manager start it, and waits for it
to answer; it exits 1 when it does not, under launchd quoting what the server logged. Installed
again, the definition is written anew and the server started again. `--dry-run` prints what
would be written and run, with `ASQ_TOKEN`'s value left out. `status`, or `asq service` alone,
says which definition there is and whose, what it runs, whether the manager holds it and it
answers, and where its log is; `uninstall` stops it and removes the definition. One `asq` did
not write is neither replaced nor removed without `--force`, and status gives the commands that
take it down by hand. Install refuses a port something else answers on, a second service beside
the one `brew services` set up, and to run as root. `--json` answers what was planned, done or
found. See [Deploying](deploy.md#keeping-it-running).

**`asq mcp`** — an MCP server on stdio. See [MCP](mcp.md).

**`asq skill`** — prints the skill that tells an agent when to reach for asq and how, built into
the binary, to save where its CLI reads skills: `~/.claude/skills/agent-sessions/SKILL.md` for
Claude Code, `~/.agents/skills/agent-sessions/SKILL.md` for Codex, Gemini CLI, OpenCode and Pi
([A skill](mcp.md#a-skill)). On a terminal it says so on stderr. It writes nothing itself.

**`asq otel`** — export to an OpenTelemetry collector. See [Telemetry](telemetry.md).

## Maintaining

**`asq keep`** — copies of the sessions a CLI deletes of its own accord. Claude Code and Gemini
CLI delete the sessions not used for 30 days by default, and what a CLI deletes leaves the index
too. Nothing is kept unless asked:

| | |
|---|---|
| `asq keep --all` | a copy of every session of Claude Code and Gemini CLI: made now, and again once a session that changed has rested five minutes |
| `asq keep <ref>` | a copy of one session and its subagents, made now and kept up the same way; `--drop <ref>` stops keeping it |
| `asq keep --off` | no longer every session; what is kept stays, and so do the sessions kept one by one |
| `asq keep --dir <folder>` | where the copies go, each CLI's in a folder of its name: by default `kept` in the state directory (`ASQ_STATE`); never one that would put them in a store, whether it names the store as asq does, through a link, or in another case where the file system ignores it |
| `asq keep` | what is kept, where, and how much disk it takes |

A copy is a session's files as its CLI wrote them, laid out as in the CLI's store, and is read as
a store of its own: `claude@kept`. While the CLI's store holds a session the copy is not read, so
nothing is listed or counted twice; once the CLI deleted it, the session is read from the copy, as
`claude@kept/…`, and stays in every list, search and report. The copies are made by whichever
`asq` brings the index up to date — a running `asq serve`, an MCP server, a command — and only
into asq's folder or the one named, never into a CLI's store. Copies are of what the CLI's own
store holds; a store added by name is not copied. No longer keeping a session one by one deletes
its copy while the CLI still holds it; a copy of one the CLI deleted is all there is of it, and
stays until it is deleted from the folder. A folder that holds copies is not left for another until
they are moved there; one emptied, or new, is filled again, and `asq keep --all` or `asq keep <ref>`
copies again whatever the folder lacks. Every `asq` that shares the state directory keeps the same.

**`asq doctor`** — for each store: whether it was found, how many transcripts it lists and the
day the oldest of them was last written, and whether its newest conversation reads whole. Of a
CLI that deletes its own old sessions — Claude Code and Gemini CLI, after 30 days unused by
default — it names the setting that decides it: what the CLI deletes leaves the index too, so
that day is as far back as `asq` goes for it, unless `asq keep --all` keeps a copy of each, which
the line then says. A labelled store is a copy or another machine's, which this CLI does not
sweep, and gets no such line; the copies `asq keep` made are counted under `claude@kept`. Listing alone proves little — a format that drifted
lists fine and reads as empty — so it reads one, and then the next two, and lists what the three
held that the adapter did not understand: records a CLI has started to write since the adapter
was written. Those are a warning (see [Sources](sources.md#what-each-adapter-reads)). Sessions
whose CLI moved them, whole or in part, where `asq` does not read them (OpenClaw's cold storage)
are counted, and are not taken for a format that drifted. Then it reads the whole index file
and says whether it holds together, and how much of it is free; and it asks the server on this
machine (`127.0.0.1` on `--port` or `ASQ_PORT`, either `0` asking none, and with neither on the
port the service `asq service install` set up listens on, or `7717`; with `ASQ_TOKEN` if it takes
one) which `asq` it runs — one started before an upgrade, or from another install, is a
warning that says so ([Upgrading](deploy.md#upgrading)). Exits 1 if a store that is there
cannot be read, if the index is damaged, or if doctor was stopped before it looked at every
store. Its first line is the version of `asq` that read them, which is what a report of a store
that will not read needs first.

**`asq index`** — where the index is, what it holds, and how much of the file is free. `asq
index path` prints the file; `asq index rebuild` reads everything again into a new file; `asq
index rm` deletes it (the next command rebuilds it), with a damaged one set aside beside it and
any index an earlier version of `asq` left there, which `asq index` names. A server or an MCP
server running meanwhile goes on with the new file at its next look at the stores; on Windows,
which does not remove a file a program holds open, stop it first. A damaged index is set aside and made anew without being asked ([The index](model.md#the-index)). A
new release that reads a CLI's files differently has that CLI's sessions read again by itself, at
the first sync after the upgrade; a rebuild is not needed for that. One that keeps a search's
words differently writes them again from what the index holds, at the first sync too, reading
no transcript; until it is done a search finds what has been written so far. One that judges
differently whether a failed command passed later judges every failure again the same way.

**`asq uninstall`** — takes asq off the machine. The index and the state are removed — an
`ASQ_INDEX` followed where it points, when what is there is an index: a SQLite database of an
index's tables alone, not a CLI's database it was pointed at, which holds tables of its own; of
a folder `ASQ_STATE` names, or the state directory is a link to, the files asq writes there
alone, and the folder once that leaves it empty — but for the copies `asq keep` made, which may
be all there is of what a CLI deleted. Each CLI's folder of them stays, in the folder
`asq keep --dir` named and in `kept` in the state directory, where copies made before it was
named are; each folder they are in is said with how many transcripts it holds, how many of
those the CLI has deleted since, and the command that would remove them: the folder when they
are all it holds, or else each CLI's folder of them. A state dir that is a home, or holds one —
an `ASQ_STATE` pointed too wide — is refused, not removed, and so is one whose `keep.json`,
which says where the copies are, does not read. What cannot be removed — on Windows, a file a
running server holds open — is said as left, and the rest goes on. What the command will not do
itself is said as commands, in the order to run them. First what keeps `asq serve` running:
`asq service uninstall` for the service it set up, which needs the binary;
`brew services stop asq` for Homebrew's; for one set up by hand — a LaunchAgent or a user unit
that runs `asq serve` or names this binary, or the task named `asq` — the manager's own
commands. Then, when there was one, `asq uninstall` again, for the index the server wrote until
it was down. Last the binary: `brew uninstall asq`, with `brew untap` when the tap is this
repository, or `scoop uninstall asq`, with `--global` for one in Scoop's folder for every user,
for one they installed; otherwise `rm`, `sudo` when its directory is not this account's to
write, or on Windows `Remove-Item` of its folder when that holds nothing else. The CLIs' stores
are never touched, nor the MCP servers and skills the agent CLIs were given, nor what a browser
keeps of the page offline. `--json` answers `removed`, `left` and `run`.

## Environment

| | |
|---|---|
| `ASQ_INDEX` | the index file |
| `ASQ_ONLY` | read only these CLIs, comma-separated |
| `ASQ_PATH` | stores outside their default place: `name[@label]=dir`, comma-separated |
| `ASQ_HOST`, `ASQ_PORT`, `ASQ_TOKEN`, `ASQ_CORS_ORIGIN`, `ASQ_PROXY_HEADER` | for `asq serve` |
| `ASQ_STATE` | the directory for what must outlive the index: the export ledger, the secret a server signs its cookies with, what `asq keep` keeps and, by default, the copies |
| `NO_COLOR` | plain text |

Setting `ASQ_ONLY` and `ASQ_PATH` in the environment rather than as flags is what keeps a page
server, an MCP server some agent started and a command typed in a terminal reading the same
stores.
