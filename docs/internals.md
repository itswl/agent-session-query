# Implementation notes

Maintainer-facing notes: how each source is parsed, and where the performance goes. For usage
and deployment see the [README](../README.md).

## Per-source parsing

- **Hermes**: `sessions.json` maps `key → record`, with the conversation in
  `<session_id>.jsonl` alongside it. Messages are the rows whose `role` is `user` or
  `assistant`; `content` is a string and the reasoning lives in `reasoning`. **Newer Hermes
  writes neither `sessions.json` nor a jsonl — everything lands in `~/.hermes/state.db`**: the
  source is enabled as soon as the database exists, and listing and messages query the
  `sessions` / `messages` tables directly (timestamps are epoch seconds, formatted as UTC; an
  assistant's `reasoning` becomes thinking; `platform` comes from `sessions.source`).
  Verified against the 2026.9 schema: `sessions.title` is the display name (Hermes generates
  one per session; `display_name` is the older field), `sessions.cwd` carries the working
  directory, rows Hermes itself hides are skipped (see below), an
  assistant's `tool_calls` column (OpenAI-shaped JSON with the arguments as a string)
  becomes `toolCall` blocks carrying each call's `id`, and a `role = 'tool'` row — the
  result, with `tool_name` and `tool_call_id` — becomes one `toolResult` block paired to its
  call. Hermes does not record whether a tool succeeded, so those results carry no status.

  Message rows are read the way Hermes displays them. Undo, rewind and regenerate set
  `active = 0`; context compression also sets `active = 0` on the rows it folded into a
  summary but marks them `compacted = 1`, and Hermes keeps showing those — the person did
  say and read them, only the model stopped seeing them. Filtering on `active` alone, which
  this did, hid the older half of every compressed session. A row is shown when it is live
  or compacted (`hermesShownClause`, built from the columns the database has), rows read in
  insertion order (`ORDER BY id`: timestamps regress when compression re-inserts rows with
  their original time), duplicates collapsed on Hermes's own display identity (role,
  content, timestamp, tool call id, tool calls, tool name — compression re-inserts the
  protected head and tail of a conversation as live copies of archived originals, and the
  most live copy is shown at the first position), and rows marked
  `display_metadata.model_only` left out. The window (`limit` / `order` / `at`) is cut after
  that projection, not in SQL, where a `LIMIT` would count hidden rows and duplicates as
  messages. When
  both exist, sessions are deduplicated by sessionId with the jsonl winning. A session with
  no jsonl also falls back to `state.db` for `final` (opened read-only, taking the last
  `active=1` assistant message with `finish_reason=stop`, and falling back to the real count
  when `message_count` is missing).

  The visibility column is read from the database rather than assumed, because it has not
  stayed the same: Bot Mode sessions were marked in `hidden`, and current Hermes (schema 25)
  marks them in `archived`, which is also what Hermes filters on itself
  (`COALESCE(archived, 0)`). SQLite rejects an unknown column outright, so naming the wrong
  one does not degrade the query, it fails it — and the failure looks exactly like a Hermes
  with no sessions in it. `hermesVisibilityClause` reads `pragma_table_info` and builds the
  filter from whichever column is there, and leaves it off when neither is: listing one
  session Hermes would have hidden beats reporting a full database as an empty one. A list
  that fails for any other reason (a renamed column, a schema mid-migration) comes back as an
  error rather than as zero records, and the API says so — see below
- **OpenClaw 2026.9+**: one SQLite database per agent at
  `~/.openclaw/agents/<agent>/agent/openclaw-agent.sqlite` — `session_windows` is the
  session list (status, model, display_name, unix-millisecond times) and
  `transcript_events` holds the transcript rows, whose JSON shape is what the jsonl used
  to be (`type=session` carries the cwd; `message` rows carry role / content /
  stopReason / usage). A tool result arrives as its own message with role `toolResult`
  carrying `toolCallId` / `toolName`, and becomes one `toolResult` block. `display_name`
  is the title when set (chat-channel sessions); CLI runs leave it empty and the first
  user message is the title. Successive CLI turns on the same session key append to the
  same session window, so one conversation is one session. The database opens read-only
  and reads work through the live write-ahead log. The pre-SQLite layout
  (`~/.openclaw/agents/default/sessions/sessions.json` plus a jsonl per session) still
  reads through the old path when no agent database exists
- **OpenClaw (pre-SQLite)** and **Hermes jsonl**: the same sessions.json structure, with the
  fields named `sessionId` / `stopReason`. Messages
  are the `type=message` rows, `message.content` is a block array (`text` / `thinking` /
  `toolCall` / `toolResult`), and `stopReason` may sit inside `message` or at the top level
- **Pi**: row types are `session` (metadata), `model_change` and `message`; messages come from
  the `message` rows (`message.role` plus the `message.content` block array). A tool call is
  a `toolCall` block with `id`, `name` and `arguments` on the assistant message; its result
  is a message of its own with role `toolResult`, carrying `toolCallId`, `toolName` and
  `isError` on the message and the output in its content array — it becomes one
  `toolResult` block paired by id, with `status` from `isError` (measured locally over the
  newest 40 sessions: 554 pairs, 49 failures).
  Listing metadata **must come from the `type=session` row specifically**: a `model_change`
  record carries its own `id` field, so reading only the first line reports an event id as the
  session id (measured locally, this really happened). With no `session` row at all (a
  truncated or resumed file) it falls back to the uuid in the filename
- **Claude Code**: message rows are `type=user` / `assistant` with the content in
  `message.content` (`text` / `thinking` / `tool_use` / `tool_result` blocks). `isSidechain`
  rows (subagents) are skipped, as are non-conversation rows such as `queue-operation`,
  `attachment` and `mode`. A `tool_use` carries its `id`; the `tool_result` that answers it
  names that id as `tool_use_id`, and the tool's name travels from the call to the result
  through a map kept over the scan (a result row names only the call). Claude records how
  every call ended: `is_error` on the result, `interrupted` on the row-level
  `toolUseResult`, and the placeholder `[Request interrupted by user for tool use]` a
  stopped call gets as its output — all three become the result's `status`. The same notice
  as a user text block becomes an `interrupted` event rather than the user's words, so it
  does not start a round. Two kinds of `system` row become events too: `compact_boundary`
  (the context the model sees was rewritten here) and a `stop_hook_summary` with
  `hookErrors`; `turn_duration`, `informational` and the rest yield nothing and are not
  counted. Measured locally over 60 sessions: 562 system rows, 12 compactions, 289 hook
  summaries of which most carried no error.
  `cwd` is not on the first line — a run of non-conversation rows precedes it. Across 174 real
  local sessions the distribution was: line 2 once, line 3 123 times, line 4 30 times, line 5
  19 times, line 6 once
- **Codex**: the conversation is spread over several row types, and for a long time only
  the `response_item` / `message` rows were read — a Codex session showed two people talking
  with nothing in between, and its brief listed no tools. Now each of these yields one
  message: `message` (role `developer` is machine-assembled instructions and does not
  count; a user row that opens with `# AGENTS.md`, `<environment_context`, `<permissions
  instructions` or is wholly one XML-style element is kept but flagged `injected`, so a
  round does not start at it), `reasoning` (the `summary` entries become thinking; a row
  with only `encrypted_content` yields nothing), `function_call` (name, `arguments` as a
  JSON string — decoded, or kept under `raw` — and `call_id`), `custom_tool_call` (the
  freeform tools `exec` and `apply_patch`, with `input` in place of arguments),
  `local_shell_call` (the older shell: `action.command`), `web_search_call` (which has no
  output row and so answers itself), and `function_call_output` /
  `custom_tool_call_output` (a `role=tool` message paired by `call_id`). An output is a
  string or a list of `{type, text}` items, and it often opens with a header that is
  information rather than output: the older shell tool wrote a JSON document
  (`{"output": …, "metadata": {"exit_code", "duration_seconds"}}`), the newer tools write
  known lines — `Exit code: N`, `Process exited with code N`, `Wall time: 0.3 seconds`,
  `Script completed` / `Script failed`, `Chunk ID:` — closed by a line reading `Output:`.
  The header is read into `exitCode`, `durationMs` and `status` and removed; anything that
  is not that exact shape is output and stays whole. An `apply_patch` output that begins
  `apply_patch verification failed` or `patch rejected` is an error with no exit code.
  `event_msg` / `turn_aborted` becomes an `interrupted` event; the other events are
  bookkeeping. One rule — `codexProbe.counts` — decides what is a message for the list's
  background count, for `Final` and for the reader, so the three agree. Usage comes from
  the `token_usage_record` rows summed, or, in a rollout without them, from the last
  `event_msg` / `token_count` (`info.total_token_usage` is cumulative, so the last one is
  the total). The model is on `turn_context`, which the metadata scan runs on to.
  The metadata row is `type=session_meta` (authoritative and complete); only when it is
  absent does the scan salvage fields from later rows — measured against a real rollout,
  the row types are complementary: `turn_context` carries only `cwd`,
  `token_usage_record` only `session_id`.
  **Salvaging never touches a bare `id`**: a message row (`response_item`) payload carries
  `id = "msg_..."`, and picking that up would report a message ID as the session ID.

  The session id does come from a bare `id` on one row, though: a subagent's rollout is a
  fork, and its first `session_meta` says which session it was forked from — `session_id`
  there is the *parent's* id and the file's own is in `id`. Verified across every rollout on
  a real install: on the seven subagent files `id` equals the UUID in the filename and
  `session_id` equals the parent's, while a normal rollout carries the same value in both.
  Reading `session_id` first listed each subagent as its parent, so several rows shared one
  sessionId — and everything that keys sessions by id (the reader, `/sessions/<id>`, the
  page's row reuse) could not tell them apart. `codexSessionID` therefore prefers the
  session_meta `id`, falls back to the filename when a forked meta has none, and only uses a
  salvaged `session_id` when there is no session_meta at all
- **Gemini CLI**: the file is an append log — a metadata first line (`sessionId` /
  `startTime`), `{"$set": {...}}` patch rows, and message rows. Messages are the `type=user` /
  `type=gemini` rows, where `content` may be an array (user) or a string (gemini). Tool-driven
  sessions carry very little prose: issuing a call leaves `content` empty with the substance in
  `toolCalls` (becoming a `toolCall` block with just name and args), and the result comes back
  as a `functionResponse` entry in a later user row's `content` array (becoming a `toolResult`
  block paired by `id`). Where the result lands has changed across Gemini CLI's versions:
  older files answer on the user row and echo the response under the call's `result` field
  without the real output, so a `result` field alone is not an answer; newer files write a
  `status` onto the `toolCalls` entry (`success` / `error` / `cancelled`) with the output as
  `resultDisplay` (a string, or a file-diff object) or under `result`. A status is the
  signal: with one the call is answered in place and the user row's copy, if any, is
  skipped; a status that arrived without output is carried to the user row that brings it.
  `thoughts` becomes thinking whether it is a string or a `[{subject, description}]`
  array (each entry's description is taken)
- **OpenCode**: everything lives in one SQLite database at
  `${XDG_DATA_HOME:-~/.local/share}/opencode/opencode.db` — there is no jsonl, so it is the
  second all-SQLite source after newer Hermes and implements `searchableSource` (a `LIKE`
  over the `part` table). Sessions whose `time_archived` is set are skipped, matching what
  opencode itself shows. `session.directory` is the cwd, `session.title` becomes the display
  name, and the model column's JSON (`{"id":...,"providerID":...}`) becomes
  `provider/model`. Messages are `message` rows joined to their `part` rows: `text` /
  `reasoning` map to text / thinking, and a `tool` part — which carries the call and its
  result together (`state.input` / `state.output` / `state.error`) — becomes a `toolCall`
  block followed by a `toolResult` block once its state is `completed` or `error`, paired by
  `callID`; `status` follows the state (an error whose text says `abort` is an
  interruption), the exit code sits in `state.metadata.exit` and the duration is
  `state.time.start` to `end`. `step-start` / `step-finish` are boundaries, not content,
  and are dropped.

  **OpenCode 2.x** moved sessions to `session_v2` and messages to `session_message` and
  stopped writing the V1 tables, so a 2.x install listed nothing here and said nothing — an
  empty source and a moved one answer alike. The schema is detected per database: V2 when
  both tables exist (one alone is a migration caught halfway, and the V1 tables are still
  the truth then). A `session_message` row is one message, ordered by `seq`, its `type` the
  role and its parts inline under `data.content` (the same shapes as the part rows, except
  that a tool item names its tool as `name` and its call as `id`); a user row may carry its
  words as `data.text`. The final result is the newest assistant row that said something,
  with usage summed over the assistant rows' own `tokens` and `cost`; search is a `LIKE`
  over the rows. A database that saw 2.x and then 1.x again holds both layouts: the V1
  sessions never migrated are listed beside the V2 ones (a migrated session keeps its id,
  so `NOT EXISTS` keeps each to one row), and each session reads through the layout it
  lives in. The final
  result is the newest assistant message carrying a `finish` field; timestamps are unix
  milliseconds throughout (opened read-only — verified that reads work through the live
  write-ahead log, so sessions still being written are visible)
- **Grok CLI**: a session is a directory, not a file:
  `~/.grok/sessions/<url-encoded-cwd>/<session-id>/`. `summary.json` is the index entry the
  list is built from and is what the file cache is keyed on, because Grok rewrites it on
  every turn; `updates.jsonl` beside it is the transcript and is what `file`, search and
  `Final` read. Each transcript line is a JSON-RPC envelope,
  `{timestamp, method, params}`, with the update under `params.update` and the timestamp in
  epoch seconds. Two methods share the file: `session/update` carries the Agent Client
  Protocol stream, and `_x.ai/session/update` carries Grok's own events — hook runs, and the
  `turn_completed` rows the usage total is summed from. A turn arrives as many small
  updates, so consecutive updates from one speaker are folded into a single message and
  consecutive text chunks into a single block: `user_message_chunk` / `agent_message_chunk`
  become text, `agent_thought_chunk` thinking, `tool_call` a `toolCall` block, and the
  `tool_call_update` that reports `completed` or `failed` a `toolResult` paired by
  `toolCallId`, with `status` from which of the two it was (a status-less one is the call
  being re-titled mid-flight and carries no output). A session the user archives moves to
  `~/.grok/archived_sessions/` with the same layout; it is listed from there too, marked
  `archived`. The cwd comes from
  `info.cwd`; the group directory name is that same path URL-encoded, which is the fallback,
  and above 255 bytes Grok substitutes a slug plus a hash and records the real path in a
  `.cwd` file beside the sessions

### Blocks: one shape for eight transcripts

Every parser produces the same block array (`blocks.go`): `text`, `thinking`, `toolCall`
(`id`, `name`, `arguments`), `toolResult` (`callId`, `toolName`, `content`, and `status` /
`exitCode` / `durationMs` when recorded) and `event` (`kind`, `content`). The id pairing is
what lets the page, the brief and an MCP caller say *which* command failed rather than only
that one did; `status` is written only when the source said something, so a Hermes result
with no status is "not recorded", not "fine".

Tool output is cut to 500 characters and thinking to 1 000 on the ordinary read, and a cut
block carries `truncated: true`. The cut is a window-size decision, not a storage one: a
page of two hundred messages carrying every build log in full is megabytes. A full read —
`?full=1`, MCP's `full`, and always the export — keeps every byte, which is what an agent
reading why a command failed needs: the end of the output is exactly the part a preview
drops. The page fetches a cut block's message again with `full=1` when asked.

### Rounds and the brief

`brief.go` segments a session at each real user message (`isRoundStart`: a user row with
words of its own, not command plumbing, not `injected`). A round carries the files any tool
named and, separately, the files a write-kind call changed and did not fail at — a write
waits for its result by call id, so a failed edit is not a change — plus `failures`, the
count of results that reported an error or an interruption. An `interrupted` event in the
round marks it interrupted outright; the ask-after-ask heuristic covers the sources that
record no such event. The brief carries the resume command with the `cd` the page prepends
for the same reason (see `resumeCommand`).

**A round ends when the work ends, not when its rows do.** `endAt` is the last row that is
the ask, an assistant message, or a row carrying a tool call, a tool result or an event.
A notification or a block of context a CLI delivers while nobody is at the keyboard is a
user row that starts no round, so it joins whichever round was last; counted as the end, it
turned a round of two commands into ninety-four hours. `lastAt` (`spanEnd`) keeps that final
row, because a search hit inside it still has to resolve to its round, and the two are
reported apart by `/rounds`. The page applies the same rule in `summarizeRound`, and sums
the rounds rather than spanning the window for the figure above a conversation.

**`since` briefs a stretch.** `round` and `at` select one round; `since` selects every round
that ran after a moment and renders a `## Since` summary (rounds covered, the union of files
changed and touched, tool calls, failures) before the rounds themselves, capped at
`briefMaxSections`. It is the second handoff of a session: what has happened since the other
side last looked, rather than one round again.

### The branch a session opened on

Claude Code writes `gitBranch` on nearly every row, so `List()` picks it up in the head
scan it already runs for the session id and the cwd, and stops as soon as all three are in
place. A session started outside a repository has none, which is the one case that scan
runs to `claudeHeadLines` — the cap exists for it. An empty value is dropped in `public()`
rather than carried: a key that is sometimes a branch and sometimes `""` reads as a branch
that failed to load. No other source on this machine records one.

It is the branch at the time, not now. The checkout has moved on, the branch may be gone,
and resolving it live would answer a question nobody asked.

### When a source cannot be read

A source that returns no records and no error is empty; a source that could not be read at
all says so, through `listErrorReporter` in `source.go`. Everything else follows from keeping
those two apart, because they answer a caller identically: an empty list.

`JsonMapSource` is the one that can fail this way today (both its Hermes databases are other
programs' schemas), so it keeps the failure its last `List()` hit, and the API reads it
rather than being handed it. `/sessions`, `/health` and the MCP `list_sessions` tool carry it
as `warnings: [{source, error}]`, present only when there is something to say.

`/health` is unauthenticated and reports what the last scan hit instead of starting one: an
anonymous request must not be able to set off a walk over every session directory on the
machine, so it has nothing to report until something has listed.

The list ETag folds the warnings in. A source going from readable-and-empty to unreadable
changes no record at all, so without that the validator would not move and a client polling
with `If-None-Match` would keep its 304 — hiding the one thing that did change.

### Usage: one shape, and a session total

Each provider names the same quantities differently — claude's `cache_read_input_tokens`,
codex's `cached_input_tokens`, pi's `cacheRead`, gemini's `cached` — and bundles fields
the card has no use for (`service_tier`, `speed`, `iterations`). `normalizeUsage` folds
all seven onto one set of names, matching keys case- and separator-insensitively, so the
card labels one shape rather than knowing about four.

They also disagree about scope: hermes and opencode store session totals, while the
file-backed sources could only offer the last message's usage. That is what made the
numbers look wrong — claude's final message reads `in 312 / out 1201`, which is that
turn's cost, not the session's. Every source now sums across the session. The Final scan
already walks every message for the last one, so a small extra decode per message is the
whole cost; measured on a 16 561-message session the endpoint still answers in ~0.4 s.

The sums are large and should be: a long session re-reads its cached context every turn,
so 5.6 G cache-read tokens over 11 200 turns is arithmetic, not a bug.

### Session titles

opencode writes a real title for every session (`session.title`), and that is what its
list shows. The other sources write none, so their lists used to show a sessionId or a
filename. The closest equivalent is the first real user message — which is also where
opencode's own titles come from, it just has a model summarise the line. So each file
source's head scan now also picks up the first user message, and the title lands in
`shortKey` (with the old stem as fallback, so a noisy file loses nothing).

Not every first user row is a question: Claude Code opens with command plumbing and
caveat rows, Codex with AGENTS.md instructions and environment context — all of them
`role=user` but machine-assembled. `titleFromUserText` rejects them by their openings
(each prefix was seen in real local data), takes the first line of what survives, and
truncates to 80 runes. Coverage measured locally: 190/192 Claude sessions, 31/33 Gemini,
2/2 Pi, 2/2 Codex come out with a title.

The extraction runs inside the `fileRecordCache` build callback, so it costs nothing on
unchanged files; the first read after a change reads up to the head-scan cap, which is
also where cwd already comes from.

### A session's update time: read the tail, do not trust mtime

The list sorts by `updatedAt`, and file-backed sources originally took that straight from the
file's mtime. mtime is when the file was written, not when the conversation happened, and the
two diverge badly:

- Measured over 174 real local Claude sessions, **43 of them (25%) differed by more than an
  hour**, the worst by **235 hours**. Something rewrites session files without appending
  anything, which floats a conversation that ended six days ago to the top of the list and
  has `isActive` report recent activity on it
- Gemini hides it better: it used the `lastUpdated` on the metadata first line, but that is
  its value at **session start**, updated afterwards by `$set` patch rows. Measured across 33
  sessions, **29 had a stale first-line time**

Both now read the time carried by the last record in the **tail** of the file
(`lastRecordTime`). It does not read the whole file — seeking a small window from the end is
enough, and 172 of 174 files yield a complete record within the final 64 KB, with the window
growing to 512 KB and then 4 MB when they do not. All three placements count: a top-level
`timestamp`, `message.timestamp`, and `$set.lastUpdated`. Only when nothing is found, or what
is found will not parse (an unparseable time sorts to the very end of the list, which is worse
than using mtime), does it fall back to mtime.

The cost is absorbed by the file-head cache: freshness is keyed by `(mtime, size)`, so only a
changed file has its tail read again.

### Locating metadata: scan until complete, and check the row type

None of the three file-backed sources (Claude / Pi / Codex) keep their listing metadata at a
fixed position, and the original implementation used fixed windows (the first 5 lines for
Claude, only the first line for Pi and Codex). Both ways of failing actually happened:

- **Not found** — Claude's `cwd` landed on line 6, leaving `project` empty and the session
  counted as ungrouped, with no error reported
- **Found, but wrong** — when Pi's first line was a `model_change`, that record carries its own
  `id`, so the session ID came back as an event ID

Both are now handled the same way: **scan until the needed fields are in hand** (the 50-line
cap is only a defensive backstop; most files stop at line 1-3, fewer than the fixed window read)
**and check the row type before taking a field**, rather than using whatever the first line
happens to hold.

Gemini is not in this group: its metadata really is on the first line, and the 2 of 33 local
files without one (the first line is a message) are missing the data entirely — the code
correctly degrades to the filename and mtime.

## Performance

Synthetic data of 800 sessions (200 per source across four sources, a quarter of them 3000-line
sessions, about 208 MB total), with the service and the load generator on the same arm64
machine, median of three runs:

| Endpoint | Time |
|----------|------|
| `GET /sessions` (listing 800) | 14.2 ms |
| `GET /sessions/<pattern>` (exact lookup) | 3.8 ms |
| `GET /sessions/<pattern>/messages?limit=10` (large session) | 3.2 ms |
| `GET /sessions/<pattern>/final` (large session) | 35.5 ms |
| `GET /health` | 1.5 ms |
| Resident memory (RSS after the run) | 11.0 MB |

> The `GET /sessions` row predates the file-head cache (point 2 below); for current numbers see
> "What listing actually costs".

Implementation notes:

1. **Listing never reads whole files** — each session contributes only its first metadata lines
   (Gemini prefers the time in its metadata and falls back to the file time), so `GET /sessions`
   is "stat plus read the head", not "read 208 MB"
2. **File heads memoized by (mtime, size)** (`filecache.go`) — the first few lines only change
   when the file does, so a stat says whether last time's result still holds. In steady state
   `List()` degrades to a round of stat calls and opens nothing
3. **Streaming reads** — `messages` stops once it has `limit`; `final` keeps only the last
   assistant message
4. **`final` probes with a struct before materialising** — the whole-file scan decodes only
   scalar fields like `type` / `role` (Go's JSON decoder skips what it is not asked for and
   builds no map), and only the last assistant message is decoded in full
5. **Line reads reuse the buffer** — `bufio.Scanner`'s `Bytes()` is a view into the internal
   buffer, so no allocation per line
6. **Short-TTL session list cache** (`--cache-ttl`, 2 seconds by default) — both lookup and
   listing go through it, while **messages and final always read from disk**; the cache only
   covers the "which sessions exist" layer, and `--cache-ttl 0` turns it off entirely. One
   source is scanned at most once at a time (concurrent requests arriving just after expiry do
   not each rescan)
7. **Responses stream out** — no buffering the whole JSON in memory, and `?limit=` is clamped to
   `--max-limit` (1000 by default) so a single request cannot spread into tens of megabytes
8. **`/sessions` carries an ETag** — the page polls every 10 seconds, and an unchanged list ends
   at a 304
9. **`?order=desc` takes the tail with a ring buffer** — the latest N still means scanning to the
   end of the file, but only the last N are retained, so memory tracks `limit` rather than
   session length. Hermes's SQLite path simply queries in reverse and flips the result

### What listing actually costs

173 real local Claude Code sessions (`~/.claude/projects`, 470 MB in total), M3 Pro,
`--cache-ttl 0` (TTL cache off, so this is the cost of one genuine scan):

| Step | Time |
|------|------|
| glob the directories | 0.9 ms |
| glob + stat each file | 2.4 ms |
| glob + stat + parse each file's head | 52 ms |

Nine tenths of the cost was re-parsing unchanged heads, and the page polls every 10 seconds.
With `filecache.go` in place (same machine, same 173 sessions, measured end to end over
`GET /sessions`):

| | First (cold) | Afterwards (nothing changed) |
|---|---|---|
| `GET /sessions` | 154 ms | **2.5 ms** |
| With `If-None-Match` | — | **1.7 ms / 0 bytes (304)** |

What clamping `?limit=` achieves (a 99 MB, 16444-message session with `?limit=99999999`): the
response drops from 14 MB to 0.9 MB, the request from 890 ms to 48 ms, and peak process RSS
from 145 MB to 25 MB.

### The page

The page lives in `internal/app/ui/` and is baked in with `go:embed` — no npm, no build step,
still a single-file distribution. A few hard constraints:

- **Render exclusively through `textContent`.** Messages are full of third-party text (tool
  output, web page bodies), so building HTML would be stored XSS, and the token in localStorage
  is what an attacker would get. `ui_test.go` pins a test forbidding `innerHTML` / `outerHTML` /
  `insertAdjacentHTML` / `document.write` / `eval(` anywhere in those three files. A
  `Content-Security-Policy` (same-origin scripts and styles only, nothing inline, no framing)
  backs that up
- **The typeface is embedded, not fetched.** `ui/fonts/` holds Geist and Geist Mono as
  variable WOFF2 files (SIL Open Font License, the licence text alongside them), served from
  `/ui/fonts/` with the other assets. A font CDN would be the one request the page made to
  another origin, and the CSP would block it anyway. CJK text falls through to the system
  fonts named after them in the stack
- **The page itself needs no authentication** (it holds no data), while the data still requires
  an `Authorization` header. With no token configured the page goes straight in — that is what
  `authRequired` on `/health` is for

The other difficulty is not visual, it is that **auto-refresh must not rip away what you are
reading**:

- The list is **patched incrementally** by sessionId — a matched node has its text updated in
  place (`setText` only writes to the DOM when the content really changed, or it would clear the
  user's selection), and reordering moves nodes with `insertBefore` instead of rebuilding them.
  That patch keys rows by id and can hold one node per id, so a repeated id has to drop the
  earlier node on sight: it is in no later render's row list, and the sweep at the end only
  reaches what the map holds — the row would outlive every filter that should have removed it
- The detail pane is only refetched when the selected session's `updatedAt` / `status` / chosen
  direction really changed; refreshing the same session leaves the old content on screen and
  does not flash
- Before rebuilding the message stream it records which blocks are expanded (keyed stably by
  `message id:block index`) and the `scrollTop`, then restores both; if it was pinned to the
  bottom it stays pinned
- On first open the landing position follows the direction: latest scrolls to the bottom,
  earliest starts at the top

### Full-text search

`/search` builds no index. An index has to be written somewhere, maintained, and reasoned about
when it goes stale, and that costs the "single binary, read-only, scp it anywhere" property —
and measurement says it is not needed:

| 210 real local sessions / 516 MB | Time |
|---|---|
| Cold (page cache missed) | 2100 ms |
| Warm, common term (stops at the first hit per file) | **17–67 ms** |
| Warm, no match anywhere (every file read to the end) | 117 ms |

The speed comes from ordering, not from the algorithm:

1. **Filter on raw bytes first** — `bytes.Contains` runs on the bare bytes from
   `eachJSONLLine`, and only a match gets `json.Unmarshal`. That skips decoding for 99% of
   lines, and decoding is the expensive part of the scan
2. **Case folding allocates nothing** — `appendLowerASCII` overwrites one reused buffer per
   line, folding `A-Z` byte by byte, while UTF-8 multi-byte sequences (lead byte ≥ 0x80) pass
   through untouched
3. **Parallel per session** — `GOMAXPROCS` workers each scan one session and results are written
   back by index, so the order never shifts
4. **Newest first** — candidates are ordered by update time, so truncating at `limit` keeps the
   most recent
5. **Cancellable** — the page searches on every keystroke and only ever displays the last
   result, so an abandoned scan has to stop rather than run to completion. `search` takes the
   request context; workers stop picking up sessions once it is done, and inside a file the
   check is sampled every `cancelCheckLines` lines (a channel receive per line would cost more
   than the `Contains` that is the actual work). A cancelled request logs 499 and writes no
   body. Without this, typing five characters would leave four full scans competing for every
   core — measured at 20 ms → 384 ms per search with five in flight

There is a trap in extracting snippets: JSON is full of strings, and a match can land in a field
name — or worse, in a field's *value*: a short or numeric needle matches inside timestamps
and uuids constantly (measured: "502" matched a timestamp's `.502Z` millisecond part and a
uuid's tail often enough to dominate the first page). So `findMatchingText` only accepts
body fields (`text` / `content` / `thinking` / `reasoning`, ...) in a fixed order (Go map
iteration is randomised and results must be stable), and skips metadata keys when falling
back to the rest: timestamps and ordinals by list, and anything id-shaped by rule — a key
that ends in `id` after folding camelCase and snake_case together (`turnId`,
`root_turn_id`, `callID`, ...) names an identifier, never body text. A match that lands
nowhere else produces no hit at all: the row matched, but it had nothing to say.

Newer Hermes sessions live entirely in SQLite with no jsonl, so `JsonMapSource` implements the
optional `searchableSource` interface and uses `LIKE` (SQLite's LIKE is already
case-insensitive for ASCII). The other five sources have nothing but files, so the generic path
suffices.

### Running in the background

`-d` is not a fork. Go's runtime is multi-threaded and `fork` only clones the calling thread,
leaving the child with a half-dead runtime — so the route is to re-exec ourselves: strip `-d`
from the arguments (or it recurses forever), attach stdout/stderr to a log file, and `Setsid`
(on Windows, `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`) to leave the terminal's session.

The parent cannot simply Start and walk away: that turns a taken port or a bad configuration
silently into "started successfully". So it watches two things:

1. **The port came up** — but "the port answers" does not mean success, since what answers could
   be an earlier instance while the child we launched has already died on a failed bind
2. **The child is still alive** — `cmd.Wait()` runs in a goroutine racing the probe, and even
   when the probe wins there is a further 300 ms settle before rechecking, because the child can
   die in the instant right after the probe succeeded

On failure the tail of the log goes to stderr as well: all of the child's output went to the log
file, so without printing it there is nothing to see in the terminal.

### MCP

`--mcp` speaks JSON-RPC 2.0 over stdio (`json.Decoder` reads value by value, which handles
newline delimiting naturally). Two points matter:

- **stdout belongs to JSON-RPC alone.** The startup banner and every warning go to stderr, or
  they corrupt the protocol stream
- **Tool-level failures travel as `isError`, not as a JSON-RPC error.** A model needs to see
  what went wrong to retry with different arguments; only protocol errors (an unknown method)
  return `-32601`

### Cross-platform

Supported: `linux` / `darwin` / `windows` × `amd64` / `arm64` (`windows/386` also compiles, it
is simply not released). The only dependency is a pure-Go SQLite driver, ported to all six
(Windows uses `sqlite_windows.go`), so `CGO_ENABLED=0` cross-compilation needs no C toolchain.

What needed specific attention for Windows:

- **Path separators**: a file-backed source's `key` is a full path, which on Windows is
  `C:\...\abc.jsonl`. Everything goes through `normalizeForMatch` (lowercase + forward slashes)
  before matching, or a user typing the habitual `proj/abc.jsonl` matches nothing, and an exact
  suffix hit degrades into a substring hit
- **SQLite's file: URI**: backslashes inside a URI are ambiguous with escapes, so `sqliteURI`
  normalises to forward slashes (SQLite on Windows accepts `file:C:/Users/.../state.db`)
- **Console code page**: the legacy cmd.exe console defaults to the local code page, where
  UTF-8 bytes come out as garbage. `console_windows.go` calls `SetConsoleOutputCP(65001)` at
  startup; everywhere else it is a no-op
- **Test isolation**: `os.UserHomeDir()` reads `%USERPROFILE%` on Windows rather than `$HOME`,
  so `setHome` in the tests sets both
- **`syscall.SIGTERM`**: it is defined on Windows (so the code compiles) but never delivered.
  Ctrl+C arrives as `os.Interrupt`, and graceful shutdown works as usual

CI runs the tests on ubuntu, windows and macOS runners — a cross-compiled test binary cannot
execute on another platform, and shipping a Windows artifact without a Windows runner verifying
it means releasing blind.

### Sorting

Sources spell their update times differently (`2006-01-02T15:04:05` derived from mtime, Gemini's
RFC3339Nano, whatever string `sessions.json` holds for Hermes, OpenClaw's epoch milliseconds),
and they are all parsed into a `time.Time` in `newRecord` before comparison. Compare the strings
lexicographically instead and a single offset-bearing timestamp misorders the whole cross-source
list. Records whose time will not parse sort after those that have one, falling back to reverse
string order among themselves.

Edges and costs:

- A new session takes up to 2 seconds to appear in the list or a query (tunable, or 0 to
  disable)
- `final` still reads a session file end to end (it needs the last assistant message), about
  35 ms for a 3000-line session — the inherent cost of one sequential read plus parsing
- A line over 256 MB aborts `bufio.Scanner`, and **nothing after it in that file is read** (not
  "that line is skipped"). A warning goes to stderr rather than truncating silently. Real
  sessions never reach that size
- The file-head cache judges freshness by (mtime, size). A file modified twice within the same
  second at exactly the same size would serve stale metadata — but appending to a jsonl always
  changes its size, so this does not occur in practice
