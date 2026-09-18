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
  assistant's `reasoning` becomes thinking; `platform` comes from `sessions.source`). When
  both exist, sessions are deduplicated by sessionId with the jsonl winning. A session with
  no jsonl also falls back to `state.db` for `final` (opened read-only, taking the last
  `active=1` assistant message with `finish_reason=stop`, and falling back to the real count
  when `message_count` is missing)
- **OpenClaw**: the same structure, with the fields named `sessionId` / `stopReason`. Messages
  are the `type=message` rows, `message.content` is a block array (`text` / `thinking` /
  `toolCall` / `toolResult`), and `stopReason` may sit inside `message` or at the top level
- **Pi**: row types are `session` (metadata), `model_change` and `message`; messages come from
  the `message` rows (`message.role` plus the `message.content` block array).
  Listing metadata **must come from the `type=session` row specifically**: a `model_change`
  record carries its own `id` field, so reading only the first line reports an event id as the
  session id (measured locally, this really happened). With no `session` row at all (a
  truncated or resumed file) it falls back to the uuid in the filename
- **Claude Code**: message rows are `type=user` / `assistant` with the content in
  `message.content` (`text` / `thinking` / `tool_use` / `tool_result` blocks). `isSidechain`
  rows (subagents) are skipped, as are non-conversation rows such as `queue-operation`,
  `attachment` and `mode`.
  `cwd` is not on the first line — a run of non-conversation rows precedes it. Across 174 real
  local sessions the distribution was: line 2 once, line 3 123 times, line 4 30 times, line 5
  19 times, line 6 once
- **Codex**: message rows are `type=response_item` with `payload.type=message`; rows whose
  `payload.role` is `developer` (machine-assembled instructions) do not count, and usage comes
  from `token_usage_record`.
  The metadata row is `type=session_meta` (authoritative and complete, so the scan stops
  there); only when it is absent does the scan salvage fields from later rows — measured
  against a real rollout, the row types are complementary: `turn_context` carries only `cwd`,
  `token_usage_record` only `session_id`.
  **Salvaging never touches a bare `id`**: a message row (`response_item`) payload carries
  `id = "msg_..."`, and picking that up would report a message ID as the session ID
- **Gemini CLI**: the file is an append log — a metadata first line (`sessionId` /
  `startTime`), `{"$set": {...}}` patch rows, and message rows. Messages are the `type=user` /
  `type=gemini` rows, where `content` may be an array (user) or a string (gemini). Tool-driven
  sessions carry very little prose: issuing a call leaves `content` empty with the substance in
  `toolCalls` (becoming a `toolCall` block with just name and args), and the result comes back
  as a `functionResponse` entry in a later user row's `content` array (becoming a `toolResult`
  block). `thoughts` becomes thinking whether it is a string or a `[{subject, description}]`
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
  block followed by a `toolResult` block once its state is `completed` or `error`.
  `step-start` / `step-finish` are boundaries, not content, and are dropped. The final
  result is the newest assistant message carrying a `finish` field; timestamps are unix
  milliseconds throughout (opened read-only — verified that reads work through the live
  write-ahead log, so sessions still being written are visible)

### A session's update time: read the tail, do not trust mtime

The list sorts by `updatedAt`, and file-backed sources originally took that straight from the
file's mtime. mtime is when the file was written, not when the conversation happened, and the
two diverge badly:

- Measured over 174 real local Claude sessions, **43 of them (25%) differed by more than an
  hour**, the worst by **235 hours**. Something rewrites session files without appending
  anything, which floats a conversation that ended six days ago to the top of the list and
  has `isActive` report it as currently being written
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
- **The page itself needs no authentication** (it holds no data), while the data still requires
  an `Authorization` header. With no token configured the page goes straight in — that is what
  `authRequired` on `/health` is for

The other difficulty is not visual, it is that **auto-refresh must not rip away what you are
reading**:

- The list is **patched incrementally** by sessionId — a matched node has its text updated in
  place (`setText` only writes to the DOM when the content really changed, or it would clear the
  user's selection), and reordering moves nodes with `insertBefore` instead of rebuilding them
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
name. So `findMatchingText` only accepts body fields (`text` / `content` / `thinking` /
`reasoning`, ...) and — because Go map iteration is randomised, and results have to be stable —
looks through those keys in a fixed order before falling back to the remaining keys sorted by
name.

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
