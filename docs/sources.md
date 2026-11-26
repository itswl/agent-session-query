# Sources

A *source* is one CLI's store of sessions. `asq doctor` shows which were found on this machine,
whether each reads, and how far back each goes.

| `--source` | CLI | default location | moved by | resumes with |
|---|---|---|---|---|
| `claude` | Claude Code | `~/.claude/projects` | `CLAUDE_CONFIG_DIR` | `claude --resume` |
| `codex` | Codex | `~/.codex/sessions`, and `archived_sessions` beside it | `CODEX_HOME` | `codex resume` |
| `gemini` | Gemini CLI | `~/.gemini/tmp` | — | `gemini --resume` |
| `opencode` | OpenCode | `~/.local/share/opencode/opencode.db`, on Windows too | `XDG_DATA_HOME`, `OPENCODE_DB` | `opencode --session` |
| `grok` | Grok CLI | `~/.grok/sessions`, and `archived_sessions` beside it | `GROK_HOME` | `grok -r` |
| `antigravity` | Antigravity | `~/.gemini/antigravity-cli/brain` (the `agy` CLI) and `~/.gemini/antigravity/brain` (the IDE) | — | `agy --conversation` |
| `pi` | Pi | `~/.pi/agent/sessions` | `PI_CODING_AGENT_DIR`, `PI_CODING_AGENT_SESSION_DIR` | `pi --session` |
| `dsh` | DeepSeek Harness | `~/.dsh/sessions` | `DSH_HOME` | — |
| `hermes` | Hermes | `~/.hermes/state.db` (and the older `sessions/`) | `HERMES_HOME` | `hermes --resume` |
| `openclaw` | OpenClaw | `~/.openclaw/agents` | `OPENCLAW_STATE_DIR` | — |
| `copilot` | GitHub Copilot CLI | `~/.copilot/session-state` | `COPILOT_HOME` | `copilot --resume` |

Each store is found the way its CLI finds it, so a CLI whose data was moved by its own variable
is read where the variable says. `OPENCODE_DB` names OpenCode's database file itself: an absolute
path as it is, a relative one inside OpenCode's data directory. The variables are read from
`asq`'s environment: a service has to be given the ones its CLIs run with.

Which stores exist is checked again before every sync, not once at start. A CLI you run for the
first time while `asq serve` is up creates its store with that first session, and its sessions
appear without a restart.

`--only claude,codex` (or `ASQ_ONLY`) reads only some of them.

## Stores somewhere else: `--path`

```sh
asq ls --path claude=/mnt/disk/.claude/projects          # Claude Code's store is here instead
asq ls --path claude@box2=/mnt/box2/.claude/projects     # … or: this one as well
```

- **`name=dir` relocates** a CLI's store. Its sessions keep the plain source name.
- **`name@label=dir` adds a store** beside the default one — another machine's home directory,
  a backup, the bind mount a fleet of agent containers writes to. Its sessions carry the source
  `claude@box2`: that is what refs begin with (`claude@box2/3f2a9c1e`), what `--source` takes,
  and what the page's filter lists. `--source claude` selects every Claude Code store, labeled
  or not. A labeled store is read whatever `--only` says: it was asked for by name.
- **A session two stores of one CLI both hold is one session** — a backup beside the store it
  was copied from, two copies of another machine's. It is listed, found and counted once: as
  the CLI's own store holds it, while it does; once the CLI has deleted it, as the store that
  holds the most of it, under that store's name. A ref to either copy names it. A subagent's
  transcript is the same one when it ran in the same session and has the same id.
- A session only a labeled store holds is not in its CLI's store on this machine, which is
  where the CLI's resume command looks: `asq resume` gives no command for it here, and says the
  one for the machine the store came from.
- A label is lowercase letters, digits and dashes. `kept` is asq's own while it keeps copies of
  what a CLI deletes ([`asq keep`](cli.md#maintaining)): `claude@kept` is those copies.
- Repeat the flag for several, or set `ASQ_PATH` (comma-separated) so every `asq` process on
  the machine agrees.

`hermes` and `openclaw` keep their sessions across a whole state directory and cannot be pointed
at with `--path`; move them with `HERMES_HOME` / `OPENCLAW_STATE_DIR`.

Pi has two layouts and both are read: the interactive default nests a directory per project
under `~/.pi/agent/sessions`; `pi --session-dir <dir>`, which is how a host embeds Pi, writes
its files flat in the directory it was given. A hosted directory is added as a store of its own:
`--path pi@host=/path/to/that/dir`.

### Another machine's sessions

Copy its stores here, each as its CLI lays it out, and add each copy by name. `rsync -a`
without `--delete` copies what changed since it last ran and keeps what the other machine has
deleted since:

```sh
c=$HOME/asq-copies/box2
rsync -a box2:.claude/projects/ "$c/claude/"
rsync -a box2:.codex/sessions "$c/codex/"
rsync -a box2:.codex/session_index.jsonl "$c/codex/"      # the names Codex's threads were given
export ASQ_PATH="claude@box2=$c/claude,codex@box2=$c/codex/sessions"
```

- Copy the sessions and nothing beside them: next to them sit the CLI's credentials
  (`~/.codex/auth.json`, `~/.gemini/oauth_creds.json`). Another store is its directory as the
  table above names it.
- Codex keeps a thread's name in `session_index.jsonl` beside `sessions`, and it is read from
  there when the copy is named by its `sessions` directory, as above; `archived_sessions` beside
  it is read too.
- Run the copy again — by hand, from cron — to bring it up to date: what changed is read again,
  and only that.
- A session from there is resumed there. `asq resume` names the command for that machine, and
  gives none for this one.

An `agy` started with a private `--gemini_dir` writes the same `antigravity-cli/brain` shape
inside that directory: `--path antigravity=/path/to/the/gemini_dir`.

## What each adapter reads

Every adapter converts its CLI's transcript into [the record](model.md). The differences that
show through:

A picture, a document or a sound a message carried is [marked where it came](model.md#session--turn--item)
by the adapters of the CLIs known to record one: Claude Code, Codex, Gemini CLI, OpenCode, Grok,
Pi and OpenClaw — in an ask, and in the result of a tool that returned one. Gemini CLI sends a
tool's picture on the row after the call; it is read as that call's.

**Claude Code** — one JSONL file per session. Subagents (`<session>/subagents/agent-*.jsonl`)
and workflow agents are read as sessions beneath the one they ran in, tied to the tool call that
started them. A response written as several rows is one model call. A forked agent opens with a
copy of its parent's conversation; those rows are marked inherited and counted nowhere. Tool
output that Claude Code saved to a file beside the transcript is read in place of the stub left
in it. Slash commands, hook output, task notifications and compaction summaries are context or
events, not asks. Another model answering in place of the one asked for — after it refused, when
it failed or is not available, or when using it needed a consent the person did not give — is a
`model_change` event in the turn, in Claude Code's words. The session's own cost, when Claude
Code recorded one, is used as it is.
A failed command's exit code is the line its result opens with (`Exit code 1`); a command that
passed, or whose code means no failure (`grep` finding nothing), has none recorded. A call the
person declined at the permission prompt, or stopped before or while it ran, is marked
interrupted rather than an error — newer versions say which on the row, older ones only in the
words Claude Code gives the model in its place. Like any interrupted call, it is still listed by
`asq failures` and counted among a turn's failures. A call refused by a permission rule, a hook
or auto mode is an error.
A request to the model that failed once Claude Code stopped retrying it is written in the model's
place, with no model (`<synthetic>`), as "API Error: 529 …": it is an `error` event, the error
the turn ended on, and the turn is over — no model call, and no answer. Claude Code marks such a
row (`isApiErrorMessage`); one it did not mark is known by its words. A resumed session's "No
response requested.", written in the model's place too, is no error, and stays as it was.
Claude Code deletes the transcripts of sessions not used for 30 days by default
(`cleanupPeriodDays` in its `settings.json`, under `~/.claude` or `CLAUDE_CONFIG_DIR`); from
v2.1.248 it keeps those of sessions started or last continued in Claude Desktop or Cowork, unless
`desktopSessionCleanupPeriodDays` or a managed `cleanupPeriodDays` limits them. A session it
deletes leaves the index at the next sync, unless [`asq keep`](cli.md#maintaining) keeps a copy of it.

**Codex** — one rollout file per session, `.zst` included. Token usage is recorded per response
and read that way; cached tokens are taken out of the input they were counted in. A rollout
with no such records — an older Codex, or a host that embeds it — is read from the running
snapshots Codex writes instead: one model call per snapshot, by what that request used. A
snapshot written twice is one call; one that only repeats the total after a compaction is none;
and the compaction's own request, which no snapshot includes, is the one thing this path cannot
see. A forked thread's copied turns are inherited. A thread that is a subagent hangs beneath
its parent. A thread's title is the name it was given (`/rename`), which Codex keeps in
`session_index.jsonl` beside `sessions`; a thread with none is titled by its first ask. A file a
call names relative to the turn's directory — as a patch always names them — is named by its
whole path, as the other CLIs name them, so a file asked for by its whole path finds Codex's
edits too. A tool search is a call to `tool_search`, its result the tools it loaded; an image the
API generated is a call to `image_generation`, its prompt the input and the image its result; a
compaction the API made inside a response is a compaction event. The picture an older Codex's
`view_image` sent the model as a message of its own, after the call's output, is context beside
the call, not an ask. A rollback (`thread_rolled_back`) took the thread's last turns out of what
the model is given, and the turns after it go on from before them: it is a `rollback` event at
the end of the last turn it took back, and the turns it took back are read and counted, since
they ran and were paid for. A thread reverted (`thread/revert`, which the clients that keep a
paginated history offer) goes on in a new rollout under the same id, and the earlier one is kept
as it was. The new one is a session of its own, holding what came after the revert, and is named
for the thread and the rollout as its file is — `codex/<thread>_<rollout>` — so that a ref tells
the two apart; the turns before the revert are in the earlier one's session. Either reopens the
thread, with `codex resume <thread>`.

**OpenCode** — one SQLite database, both the 1.x and 2.x schemas. A tool part that finished with
a non-zero exit is a failure whatever its status says. Reasoning is reported beside the output
it is not part of, and is added to it. OpenCode's own totals for a session (2.3 and later) are
the session's usage: they include the call that gave the session its title, which leaves no
message. Since 2.4 a response keeps its parts in rows of their own (`session_part`) instead of
in its message, and a response is read the way it was written. A fork is made by copying the
parent's messages into a new session; the copies are
inherited — each response is counted once, by the oldest session that still holds it — and
since deleting a session in OpenCode leaves its forks behind, a fork whose parent is gone keeps
the whole conversation as its own.

**Hermes** — `state.db`, read in the order rows were written; sessions it hides are archived.
Hermes keeps token counts per session, not per response: those totals are used as recorded, on
the session's last turn. It records no outcome for a tool call, so a Hermes tool result has no
status — which is not the same as success. A turn Hermes could not finish is closed by a row of
its own in the model's seat, a notice and not the model's answer: it is read as the `error` the
turn ended on, in the words of what went wrong that Hermes keeps beside it, or in the notice's
own where it keeps none.

**OpenClaw** — per-agent SQLite databases, and the older `sessions.json` layout. A large event
is stored compressed and read all the same. Every counter a response carries is read as what it
is: the cache written as well as the cache read, and `reasoningTokens` as the part of the output
that was thinking (OpenClaw's output already includes it). OpenClaw's agents call one tool, `exec`, with a
program, and the program calls the real tools; what was actually read and run — the file, the
command, its exit code and duration — is recorded in rows of its own, and each is read as the
tool call it was. So a command that failed inside a program that "succeeded" still shows in
`asq failures`.

**Pi** — one JSONL file per session; a forked session's rows up to the fork point are inherited.
A file a tool names relative to the session's directory is named by its whole path. A failed
command's exit code is the line pi's shell tool ends its result with (`Command exited with code
1`). The calls a tool made of other tools — a code-mode script's — are kept beside its result,
and each is read as the call it was, with how it ended and how long it ran: a command that
failed inside a script is a failure. pi keeps no output of theirs, only a failed one's error.

**DeepSeek Harness** — a session is a directory of rotating, zstd-framed generations; the
newest is read, a half-written last frame and all. The session's title is the harness's own
(the one a model wrote, once there is one). A command's exit code is not on its result — the
harness marks a result an error only when the tool itself broke — but on the job that ran it,
and is read from there: `ls` of a missing directory is a failed call. The harness writes its
own work beside the conversation, and some of it is read as events. Each step — a call to the
model and the tools it asked for — is a `step` event where it ended, saying how long it took
(`Step 2 · 7s`); a step still under way, or one the harness never finished, has none. The
permissions, sandbox and approval policy a session runs with are a `settings` event before
its first ask, and another wherever one of them changes, from what to what. The steps, and the
settings a session opens with, are [routine](model.md#session--turn--item) events; a change of
setting is not. A request to a model other than the last request's is a `model_change` event.
A call to the model that failed, and a turn that ended in an error, are each an `error` event
with what the provider said — once, where the turn's end repeats what its last failed call
said. Like any CLI's error but Gemini CLI's, it does not mark the turn interrupted; a turn the
harness says was stopped is. A turn whose work ended on such an error — the step the harness
writes after it is none of the work — says so, in the error's words
([`error`](model.md#session--turn--item)); one whose failed call the harness made again, and
had answered, does not. The prompt the harness takes from its inbox is written twice, as
it enters the conversation and as the message after it, and is read once.

**Gemini CLI** — per-project chat files, the project's directory from the `.project_root`
beside them, and subagents (`chats/<session>/<agent>.jsonl`) as sessions beneath the one that
started them. A command that ran is "success" to Gemini CLI whatever it exited with: the exit
code, or the signal that ended it, is read from what the shell tool told the model. A call
cancelled with Esc is one call, interrupted, however many times the file writes it. What `@file`
added to a question is context; the question is what was typed. A session's title is the summary
Gemini CLI writes of it, which its own session list shows, and its first ask until there is one;
a subagent's summary is the start of its conclusion, so a subagent is titled by the task it was
given. An error Gemini CLI showed — a request refused for quota, a stream cut off — is an `error`
event; one that came while the model owed an answer, with nothing from the model after it, ended
the turn, which is marked interrupted and ended on that error. The row Gemini CLI writes in the model's place once such a
turn is over ("The previous response was interrupted…") is not the model's, and is not read. An
error that ended nothing does not move the turn's end. Resumed from the session's project
directory, as Gemini CLI requires. Gemini CLI deletes a project's sessions not used for 30 days
when it next starts in that project — `general.sessionRetention` in `~/.gemini/settings.json`
sets the age, or turns it off — and a session it deletes leaves the index at the next sync, unless
[`asq keep`](cli.md#maintaining) keeps a copy of it.

**Grok CLI** — the update stream, or, before a session's first line of it, the messages it keeps
beside it. Input tokens include the cache reads, which are taken out; cache writes are read.
A command's exit code is in its raw output, not its status, and is read from there — a search
that matched nothing exits 1 and is not a failure. Times are the events' own, not the
whole-second time each line was written.

**Antigravity** — the transcripts, and the summaries database that lists them. A model step's
calls carry no ids; the steps that answer them follow their order. A command's exit code is in
the step that answers it. Since `agy` 1.5.17 a model step names its model — except a model the
person brought themselves (`customModelsConfig` in the CLI's `settings.json`), which every such
model is written as by one id. That step is named as `agy` lists the model, the label of the one
custom model in the settings; where the settings hold none or several, it is `custom model
(Gemini API)`.

**GitHub Copilot CLI** — a directory per session: its event log, `events.jsonl`, and beside it
`workspace.yaml`, which holds the session's name (one the person gave it, or the CLI's summary).
A log the CLI rotated is read from its first part on, and the copies of earlier events it starts
the log again with are not read twice. A subagent writes into the session's own log: its tool
calls, failures and model calls are those of the turn it ran in, and what it says is that turn's
reasoning — the turn's answer stays the session's own agent's. A run a session starts detached,
to carry on without it, is read beneath that session. What a call used is recorded per model
call; Copilot counts the prompt tokens read from and written to the cache inside the input, and
they are taken out of it. The cost is what Copilot counts the call at in AI credits, at GitHub's
$0.01 a credit, whatever the plan. A command that ran is a success to the CLI whatever it exited
with: the exit code is read from the result, or from the `<exited with exit code N>` the shell
tool ends its output with. A call the person declined, or that Ctrl+C stopped, is interrupted;
one a rule or a hook refused is an error. A `/fork` copies its parent's events into the new log
as they were, and the copies are inherited. What a command, a schedule or an `/autopilot`
objective sent on the person's behalf is an ask; the steps autopilot sends itself to carry the
objective on, and what a skill or another agent put in the user's seat, are context.
Copilot CLI keeps every session until it is deleted.

How far each adapter has been checked differs, and is worth knowing. Claude Code, Codex,
OpenCode, Pi and Hermes were verified against real stores — some 1,300 transcripts in all —
with their token totals matching independent sums exactly. OpenCode was checked again on 2.4.1,
whose responses keep their parts in rows beside them, a fork included. OpenClaw (2026.11.3),
DeepSeek Harness (0.2.2), Gemini CLI (0.68), Grok CLI (1.0.77) and Antigravity's `agy` (1.5.8,
and 1.5.17 since) were verified against sessions run for the purpose on those releases —
commands that failed, files written and edited, turns cancelled and killed, conversations
resumed — and every one of them turned out to write things the earlier samples did not show;
the tests keep trimmed copies of what they wrote. Antigravity was run against a model of its
own — an API key's, and on 1.5.17 one brought by hand — not one Google hosts, which may write
its steps differently. GitHub Copilot CLI's adapter was written against
Copilot CLI 1.0.114 from what that release ships — the schema of every event it writes, and the
code that writes and reads them — and is tested with sessions written to that schema; sessions
1.0.114 and 1.0.115 recorded since, on a model brought by hand (BYOK), read whole, with the two
events 1.0.115 added among those passed over on purpose.

If a CLI changes how it writes, nothing fails. An adapter passes over a record it does not know —
a new row type, a renamed block — and the session reads as less than it holds: a turn short, a
cost missing. So each adapter counts what it passes over without knowing what it is (what it
skips on purpose is not counted), and `asq doctor` reads up to three of the newest
conversations of each store — and any newer session that reads as empty, which is how a format
that drifted whole shows — and lists what they held that was not understood, most frequent
first:

```text
  ! not understood in the newest transcripts, and passed over: block:server_tool_use ×3 — the CLI may have changed how it writes them; what is shown is what was understood
```

It is a warning, not a failure: what is shown is still right as far as it goes. A format that
drifted further shows the same way it always did — the store lists, and its newest
conversation reads as empty.

Sessions a CLI has archived — Codex's and Grok's moved into `archived_sessions`, OpenCode's
marked archived, the ones Hermes hides — are read like any other, marked `archived`, and left
out unless `--archived` asks for them, as each CLI leaves them out of its own list.

## Where a turn stands

Whether an agent is at work, waiting on a call, or done and waiting for the person
([the record](model.md#where-a-turn-stands)) is read from what each CLI writes as it goes:

- **Claude Code** writes its rows as it goes, the model's a content block at a time. A call
  with no result is waited on; the model's words with nothing after them are its answer,
  unless the response says calls follow; the row saying how long a turn took marks it over,
  and so does a call the person declined or stopped. While a permission prompt is open
  Claude Code writes nothing, so a command waiting for permission and one still running both
  read as waiting.
- **Codex** writes `task_complete` as each turn ends and `turn_aborted` as one is stopped,
  and a turn without either is in flight. A rollout old enough to write neither is read by
  its last rows, as Claude Code's are.
- **Gemini CLI** marks no turn's end, and writes a model row once its response is whole and,
  in the versions recorded, a call once it has finished. A call running, or waiting for
  permission, therefore reads as working; and words the model said before calls of the same
  response read as its answer until the calls are written. A file that holds a call before it
  has finished — written with the status of one still running or awaiting approval, or
  answered on a later row — reads as waiting on it until it is answered.
- **OpenCode** 2.x writes a row as each turn ends, and a turn without it is in flight. 1.x
  writes none: the last response says where the turn stands, by how it finished and whether
  it has.
- **DeepSeek Harness** writes `turn/start` and `turn/end` around every turn, and a turn
  without its end is in flight: working, or waiting on a call it made that has no result
  yet. A transcript that writes neither says nothing of where its turn stands.

The other CLIs' turns are not read this way: their sessions are marked *live* by when they
were last written alone.

## Adding a CLI

One package and one line. See [Development](development.md#adding-a-cli).
