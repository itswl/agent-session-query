# The record

Eleven CLIs write eleven shapes. What they all describe is the same thing: a person asked, an
agent worked, the agent answered. `asq` converts every transcript into one record built around
that exchange, and everything it offers — the list, search, briefs, usage, failures, traces, the
page — is a projection of this record and of nothing else.

The types are in [`internal/model/model.go`](../internal/model/model.go); their JSON tags are
the field names of `--json`, the HTTP API and the JSON exports.

## Session → Turn → Item

```
Session ─┬─ Turn 0   what came before the first ask: injected context, a fork's copy
         ├─ Turn 1   Ask → Items (reply, thinking, tool, event …) → Answer
         └─ Turn n
```

**Session** — one conversation with an agent CLI.

| field | |
|---|---|
| `source` | the store it was read from: `claude`, or a labeled one such as `claude@box2`. A session two stores of one CLI hold is one session, from the store that counts it ([Sources](sources.md#stores-somewhere-else---path)) |
| `agent` | the CLI that wrote it (`claude`) — what decides how it resumes |
| `id` | the CLI's own id; `source/id` is the session's **ref** |
| `title`, `cwd`, `branch`, `model`, `version` | as the CLI recorded them; `branch` is the one the session *opened* on |
| `startedAt`, `updatedAt` | first and last row |
| `archived` | put away by its CLI |
| `live` | where its latest turn stands, as the transcript's last rows say: `working`, `waiting` on a call, or `idle` — [below](#where-a-turn-stands) |
| `parent` | set on a subagent's transcript: the session it ran in, the tool call that started it, what kind of agent it is |
| `stats` | turns, items, tool calls, failures and how many of them passed later, model calls, usage, and the cost its CLI worked out, where it did |

**Turn** — one exchange. A turn starts when *a person* asks and at no other time: text a
harness injected in the user's place (a slash command's expansion, a notification, a hook's
output, a compaction summary) is not an ask and opens nothing.

| field | |
|---|---|
| `index` | 1 for the first ask. `0` is not an exchange: it holds whatever precedes the first ask |
| `ask` | the person's words, without what a harness wrapped around them, then the mark of each file they sent with them |
| `answer` | the last thing the agent said |
| `startedAt`, `endedAt` | the ask, and the last thing the agent *did* — a notification dropped in hours later does not stretch the turn |
| `interrupted` | it stopped before the agent was done: the person stopped it, or, in a Gemini CLI session, an error cut it off — an `error` event in the turn says which. Another CLI's error is an `error` event that marks nothing. A turn whose ask is the last thing written is interrupted too, except the session's latest while the session is said to be working or waiting: the agent is still answering it |
| `tools`, `failures`, `toolKinds` | tool calls, how many failed, and what they were for (`exec` `write` `read` `search` `net` `agent` `other`) |
| `passedLater` | of the failures, how many the same command passed later, in this turn or the three after it — what the index judges, which no transcript says |
| `files` | every file a tool named, and whether it was changed — a write counts only if it did not fail |
| `calls` | the model responses, in order, each with its usage |
| `usage`, `costUsd` | their sum; a cost only where the CLI priced its own responses |

**Item** — one thing that happened in a turn, in order. `seq` is its position in the session,
stable while the transcript only grows; it is the `[12]` beside a step, and what `--seq 12`
takes.

| `kind` | |
|---|---|
| `prompt` | what the person typed. One typed while the agent was working is `queued` and opens no turn |
| `context` | text that reached the model in the user's place but was not typed by a person |
| `reply` | the agent's words |
| `thinking` | its reasoning, where the CLI kept it |
| `tool` | **a tool call together with its result** |
| `event` | something between the words: `compaction`, `interrupted`, `hook_error`, `model_change`, or a CLI's own |

A picture, a document or a sound an item carried — pasted into a prompt, returned by a tool —
holds no words, and is marked in the item's text where it came: `[image · png · 48 KB]` after
the words of a prompt or the output of a call, `[image · png · plot.png]` for one the transcript
only names. A picture sent with no words is an ask all the same. `attachments` lists them, each
with its `mime`, its size in `bytes`, the `name` it was given by, and `inline` when the
transcript holds its bytes. The record keeps what each is, not its bytes: a picture is read from
the transcript when [it is asked for](api.md#sessions).

A tool call and its result are one item, paired once, when the transcript is read — by the call's
id; failing that, to the latest call of the same tool still waiting. No reader pairs them again.
`tool.status` is `ok`, `error` or `interrupted`; an **empty status is a result whose writer did
not record how it ended, which is not the same as success**. A non-zero `exit` with no status is
an error.

An item with `inherited: true` is a fork's copy of its parent's row. It is shown, dimmed, and
counted nowhere: its tokens were the parent's. That holds for as long as the row it copies is
there to be counted. With the parent deleted, the copy is the only record left of what was
spent, and it is read as the fork's own.

An event with `routine: true` is one its CLI writes as a matter of course, in every session or
every turn — how long a step took, the settings a session opened with. It says how the work went
on, not something that happened in it, and is not searched, where a word of it would find every
session.

## Where a turn stands

A session's `live` says what its transcript's last rows say of the latest turn, for the CLIs
that record enough to tell ([which, and how](sources.md#where-a-turn-stands)):

| `state` | |
|---|---|
| `working` | the agent is at its turn: the model answering, or a result on its way back to it |
| `waiting` | a tool call it made has no result yet: a command still running, or a prompt asking the person's permission to run it. Transcripts do not tell the two apart; `tool`, `command` (or `file`) and `seq` name the call |
| `idle` | its turn is over — it answered, gave up, or was stopped — and the next move is the person's |

`since` is when it came to be so: the ask, or the moment the agent went back to work; the
call; the end of the turn. It is what the transcript says, not a look at a process. A CLI
closed in the middle of a turn leaves it in flight for good, which is why what reads the
state weighs it by how long ago the transcript was last written (see the
[HTTP API](api.md#sessions)).

## Usage: one shape

Providers count tokens differently — some put cached tokens inside the input, some beside it;
some put reasoning inside the output, some beside it. Summed as recorded, the numbers of two
CLIs do not add. Each adapter converts to one shape whose parts never overlap:

| | |
|---|---|
| `input` | prompt tokens that were neither read from nor written to cache |
| `cacheRead`, `cacheWrite` | those that were |
| `output` | everything the model produced, **reasoning included** |
| `reasoning` | the part of `output` that was thinking, where the provider says |

So `input + cacheRead + cacheWrite` is everything the model was shown, `+ output` is everything
that was billed, and cache hit is `cacheRead / (input + cacheRead + cacheWrite)` — tokens written
to cache count against it, because they were sent in full.

A model call is one *response*: a CLI that writes a response as several rows, each repeating a
running count (Claude Code), is folded to one call at its final figures.

Some CLIs also keep totals for the session. Where those are all there is — Hermes keeps no
counters per response — they are the session's usage as recorded. Where a CLI keeps both and
the totals hold more — OpenCode names a session with a model call that leaves no message — the
totals are the session's usage, and what no response accounts for is entered once, under the
session's model, so the reports sum to the same figure. Totals that hold *less* than the
responses do are describing something else, and the responses stand.

**Cost is not usage.** A dollar figure exists only where a CLI worked one out, by prices `asq`
does not know and does not keep. It is recorded where it was said — on a response, a turn, a
session — and shown there. The reports do not sum it: over sessions most of which have none,
the sum would be read as the cost of all the tokens beside it. They carry the cache hit rate
instead, which every CLI records.

## Subagents

A subagent's transcript is a session of its own — it has turns and items like any other — whose
`parent` ties it to where it came from. It is linked beneath the transcript that spawned it (the
session, or another agent of it) and stays out of the list unless asked for (`--agents`), but its
turns are found by search, its tool calls and failures are counted with its session's project,
and its tokens are in every usage figure: they were spent.

In counts shown to a person, a *session* and a *turn* are the person's — the conversation, and
what they asked in it. Subagent transcripts are counted beside them, not among them.

## Refs

A session is named `source/id` — `claude/3f2a9c1e-7b1d-4c58-9e0a-2f6d1c9a4b10` — and, like a
commit, by any unambiguous start of its id: `claude/3f2a9c1e`, or just `3f2a9c1e`. `#4` appended
names a turn. An ambiguous prefix is an error that lists the candidates.

## What is cleaned, and what is not

The record is the data and goes back as stored: `asq export --format json` holds exactly what the
transcript said.

Anything *assembled* for someone to read is cleaned first:

- **Terminal control sequences are removed** from everything printed to a terminal or laid out
  as a document. Tool output is full of colour codes; printed raw they are at best mojibake and
  at worst content the reader never chose to run (an OSC 52 sequence writes the clipboard).
- **Secret-shaped strings are masked** in what is assembled for someone else — one-line quotes,
  briefs, packs, every MCP answer, every exported trace. This recognises the shapes that announce
  themselves: provider keys with a known prefix, GitHub and Slack tokens, AWS access key ids,
  JWTs, PEM private-key headers. It is best effort and says so: a password written in prose is
  not detectable.

A transcript can also hold text *shaped like an instruction*. Masking does nothing about that,
and cannot. The MCP server says so before any tool is called; an agent reading history must treat
it as evidence, not as orders.

## The index

The record is kept in one SQLite file — `asq index path` — that is entirely a cache: delete it
and the next command rebuilds it from the stores.

- **A damaged file is made anew.** A file that is not a database, or whose pages do not hold
  together, is set aside beside where it was, as `<name>.damaged` — the last one only — and a
  new one is read from the stores: by the process that opens it, by a sync that meets the
  damage, and by the next sync of a server or an MCP server one of whose questions met it. A
  command in a terminal whose question meets it says so, and names `asq index rebuild`; `asq
  doctor` reads the whole file, and fails when it is damaged.
- **The file at the path is the one used.** A process that runs for long — a server, an MCP
  server — looks before it reads the stores whether the file at the index's path is still the
  one it opened. When `asq index rebuild` or `asq index rm` made another, or none, it lets go of
  the old file and uses the new one, making it if need be: what it reads after is what every
  other process reads, and the old file's space goes back to the disk. (Windows does not remove
  a file a program holds open: there the server is stopped first.)
- **Space comes back.** Pages that sessions leaving the index free are given back to the file
  system once they are more than a quarter of the file and at least 16 MB: at the end of a sync,
  for at most half a second of it, and at the next while they are still that much; less than
  that, and they are kept for the next sessions. A file can do this only if it was made to —
  `asq index` and `asq doctor` say when it was not, and `asq index rebuild` makes one that does.
- **A store that cannot be read is not an empty store.** If a store, or one directory or
  database of it, cannot be listed, everything already known of it is kept and `asq` says which
  store and why — on stderr, in `/api/health`, in the page, at the end of an MCP answer.
  Sessions leave the index only when a listing that succeeded no longer has them.
- **Sync is by fingerprint.** Each adapter lists its sessions with a stamp that moves when the
  content may have (size and modification time for a file; a row's own counters for a
  database). A session whose stamp has not moved is not read. Checking a store of a thousand
  transcripts that did not change costs a thousand `stat` calls. A database is listed again
  when one of its files has moved, and at every look while any of them was written less than
  three seconds before: a filesystem's clock moves in ticks — a few milliseconds on Linux, two
  seconds on FAT — and a page rewritten in place within the tick of the write before leaves
  the file's size and time as they were.
- **Only changed turns are rewritten.** Each turn has a hash of its content; a transcript that
  grew by one turn writes one turn. Nor are the others made again: their words and their
  compressed text, most of what reading a turn costs, are kept as the index holds them, so
  reading a long session again costs about what parsing its file does.
- **A Claude Code session is taken up where its latest turn began.** Claude Code only adds to
  the end of a session's file. A read of one marks where its latest turn's first row is, and
  the next read starts there when the bytes before it are the ones it was found after (their
  CRC-32C says so): only that turn and what follows are read, and the turns before stay as
  the index holds them. A file changed before that place is read whole. With a turn added to a
  session of 6,500 turns and 89 MB, `asq ls` takes 20 ms and 18 MB of memory; reading the
  session whole again would take 0.6 s and 170 MB. A subagent's transcript, and every other
  CLI's, is read whole again.
- **A transcript being written is not chased.** One that keeps changing is read again no sooner
  than eight times what its last read cost (at most 45 seconds apart): a small session is
  followed as it is written, a hundred-megabyte one is not re-read on every append — unless
  it is taken up where its latest turn began, which costs what that turn does.
- **A report reads its window.** What a digest adds up over the turns of a week — tool calls,
  failures, when — is in an index of its own, because a turn's row holds its text: a month of
  turns read for four numbers each would be most of the file. So are which turn each hit of a
  search is and when it was, for search and `files` alike: a word most turns hold is counted
  and ranked without a turn's text being read, until the page shown is.
- **A failure says whether it passed later.** Where the same command next succeeded is worked
  out when the session is read, and kept beside the failure: the failures report, the list, a
  session's turns, a search's hits, a brief and the digest read it rather than the turns after
  each failure. A turn the session gains has the failures of the three before it worked out
  again, whichever `asq` wrote it.
- **Long text is kept twice.** A turn holds a preview of each long output — the head and, larger,
  the tail, since the end of an output is where a command says why it failed — and the whole text
  sits beside it, read only when asked for.
- **Search is over turns.** One full-text row per turn, in five columns — ask, reply, thinking,
  tool input, tool output — weighted in that spirit: a match in what was asked outranks one in a
  build log; and a sixth, the session's title, held with its first turn, which lets the session
  be found and weighs nothing in the ranking. Ranking is BM25 with a lift for recency. Each column keeps its words as written,
  for phrases and prefixes, and beside them the Porter stems of the English words among them, so
  that a word finds its other forms. Scripts written without spaces (Han, kana, Hangul, Thai,
  Lao, Khmer, Myanmar) are indexed a character at a time, so a query matches as a substring. A
  release that keeps these words differently writes them again from what the index holds, at
  its first sync, and an older `asq` sharing the file goes on using it.

Several `asq` processes — a server, an MCP server an agent started, a command in a terminal —
share one index file safely.
