# Sources

A *source* is one CLI's store of sessions. `asq doctor` shows which were found on this machine
and whether each reads.

| `--source` | CLI | default location | moved by | resumes with |
|---|---|---|---|---|
| `claude` | Claude Code | `~/.claude/projects` | `CLAUDE_CONFIG_DIR` | `claude --resume` |
| `codex` | Codex | `~/.codex/sessions`, and `archived_sessions` beside it | `CODEX_HOME` | `codex resume` |
| `gemini` | Gemini CLI | `~/.gemini/tmp` | — | `gemini --resume` |
| `opencode` | OpenCode | `~/.local/share/opencode/opencode.db` (`%LOCALAPPDATA%\opencode` on Windows) | `XDG_DATA_HOME` | `opencode --session` |
| `grok` | Grok CLI | `~/.grok/sessions`, and `archived_sessions` beside it | `GROK_HOME` | `grok -r` |
| `antigravity` | Antigravity | `~/.gemini/antigravity-cli/brain` (the `agy` CLI) and `~/.gemini/antigravity/brain` (the IDE) | — | `agy --conversation` |
| `pi` | Pi | `~/.pi/agent/sessions` | `PI_CODING_AGENT_DIR`, `PI_CODING_AGENT_SESSION_DIR` | `pi --session` |
| `dsh` | DeepSeek Harness | `~/.dsh/sessions` | `DSH_HOME` | — |
| `hermes` | Hermes | `~/.hermes/state.db` (and the older `sessions/`) | `HERMES_HOME` | `hermes --resume` |
| `openclaw` | OpenClaw | `~/.openclaw/agents` | `OPENCLAW_STATE_DIR` | — |

Each store is found the way its CLI finds it, so a CLI whose data was moved by its own variable
is read where the variable says. The variables are read from `asq`'s environment: a service has
to be given the ones its CLIs run with.

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
- A label is lowercase letters, digits and dashes.
- Repeat the flag for several, or set `ASQ_PATH` (comma-separated) so every `asq` process on
  the machine agrees.

`hermes` and `openclaw` keep their sessions across a whole state directory and cannot be pointed
at with `--path`; move them with `HERMES_HOME` / `OPENCLAW_STATE_DIR`.

Pi has two layouts and both are read: the interactive default nests a directory per project
under `~/.pi/agent/sessions`; `pi --session-dir <dir>`, which is how a host embeds Pi, writes
its files flat in the directory it was given. A hosted directory is added as a store of its own:
`--path pi@host=/path/to/that/dir`.

An `agy` started with a private `--gemini_dir` writes the same `antigravity-cli/brain` shape
inside that directory: `--path antigravity=/path/to/the/gemini_dir`.

## What each adapter reads

Every adapter converts its CLI's transcript into [the record](model.md). The differences that
show through:

**Claude Code** — one JSONL file per session. Subagents (`<session>/subagents/agent-*.jsonl`)
and workflow agents are read as sessions beneath the one they ran in, tied to the tool call that
started them. A response written as several rows is one model call. A forked agent opens with a
copy of its parent's conversation; those rows are marked inherited and counted nowhere. Tool
output that Claude Code saved to a file beside the transcript is read in place of the stub left
in it. Slash commands, hook output, task notifications and compaction summaries are context or
events, not asks. The session's own cost, when Claude Code recorded one, is used as it is.

**Codex** — one rollout file per session, `.zst` included. Token usage is recorded per response
and read that way; cached tokens are taken out of the input they were counted in. A rollout
with no such records — an older Codex, or a host that embeds it — is read from the running
snapshots Codex writes instead: one model call per snapshot, by what that request used. A
snapshot written twice is one call; one that only repeats the total after a compaction is none;
and the compaction's own request, which no snapshot includes, is the one thing this path cannot
see. A forked thread's copied turns are inherited. A thread that is a subagent hangs beneath
its parent.

**OpenCode** — one SQLite database, both the 1.x and 2.x schemas. A tool part that finished with
a non-zero exit is a failure whatever its status says. Reasoning is reported beside the output
it is not part of, and is added to it. OpenCode's own totals for a session (2.3 and later) are
the session's usage: they include the call that gave the session its title, which leaves no
message. A fork is made by copying the parent's messages into a new session; the copies are
inherited — each response is counted once, by the oldest session that still holds it — and
since deleting a session in OpenCode leaves its forks behind, a fork whose parent is gone keeps
the whole conversation as its own.

**Hermes** — `state.db`, read in the order rows were written; sessions it hides are archived.
Hermes keeps token counts per session, not per response: those totals are used as recorded, on
the session's last turn. It records no outcome for a tool call, so a Hermes tool result has no
status — which is not the same as success.

**OpenClaw** — per-agent SQLite databases, and the older `sessions.json` layout. A large event
is stored compressed and read all the same. Every counter a response carries is read as what it
is: the cache written as well as the cache read, and `reasoningTokens` as the part of the output
that was thinking (OpenClaw's output already includes it). OpenClaw's agents call one tool, `exec`, with a
program, and the program calls the real tools; what was actually read and run — the file, the
command, its exit code and duration — is recorded in rows of its own, and each is read as the
tool call it was. So a command that failed inside a program that "succeeded" still shows in
`asq failures`.

**Pi** — one JSONL file per session; a forked session's rows up to the fork point are inherited.

**DeepSeek Harness** — a session is a directory of rotating, zstd-framed generations; the
newest is read, a half-written last frame and all. The session's title is the harness's own
(the one a model wrote, once there is one). A command's exit code is not on its result — the
harness marks a result an error only when the tool itself broke — but on the job that ran it,
and is read from there: `ls` of a missing directory is a failed call.

**Gemini CLI** — per-project chat files, the project's directory from the `.project_root`
beside them, and subagents (`chats/<session>/<agent>.jsonl`) as sessions beneath the one that
started them. A command that ran is "success" to Gemini CLI whatever it exited with: the exit
code, or the signal that ended it, is read from what the shell tool told the model. A call
cancelled with Esc is one call, interrupted, however many times the file writes it. What `@file`
added to a question is context; the question is what was typed. Resumed from the session's
project directory, as Gemini CLI requires.

**Grok CLI** — the update stream, or, before a session's first line of it, the messages it keeps
beside it. Input tokens include the cache reads, which are taken out; cache writes are read.
A command's exit code is in its raw output, not its status, and is read from there — a search
that matched nothing exits 1 and is not a failure. Times are the events' own, not the
whole-second time each line was written.

**Antigravity** — the transcripts, and the summaries database that lists them. A model step's
calls carry no ids; the steps that answer them follow their order. A command's exit code is in
the step that answers it.

How far each adapter has been checked differs, and is worth knowing. Claude Code, Codex,
OpenCode, Pi and Hermes were verified against real stores — some 1,300 transcripts in all —
with their token totals matching independent sums exactly. OpenClaw (2026.11.3), DeepSeek
Harness (0.2.2), Gemini CLI (0.68), Grok CLI (1.0.77) and Antigravity's `agy` (1.5.8) were
verified against sessions run for the purpose on those releases — commands that failed, files
written and edited, turns cancelled and killed, conversations resumed — and every one of them
turned out to write things the earlier samples did not show; the tests keep trimmed copies of
what they wrote. Antigravity was run against a model of its own (an API key's), not one Google
hosts, which may write its steps differently.

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

## Adding a CLI

One package and one line. See [Development](development.md#adding-a-cli).
