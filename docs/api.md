# HTTP API

Query endpoints are all `GET` (only MCP's `/mcp` is `POST`, see [mcp.md](mcp.md)). The
`/sessions` family also accepts an `/api/sessions` prefix; `/`, `/health` and `/stats` have
no alias.

| Endpoint | Auth | What it does |
|----------|------|--------------|
| `/` `/health` `/stats` | no | Service info and health check. `/` is the one to read first: it lists every endpoint below and says which of them need a token. `/health` adds the enabled sources, the version, `authRequired` and connection stats |
| `/sessions` | yes | List every session (merged across sources, newest first) |
| `/export?project=&since=&until=&source=&mode=index` | yes | **Export several sessions as one document.** Oldest first, because a pack answers "how did this get here" where the list answers "what am I doing". `mode=index` (the default) gives one block per session — when, where, what was asked, what it concluded, and the id to check it against; `mode=full` inlines the transcripts. `format=md` (default) or `jsonl`. `limit` defaults to 20 sessions of the matching set, and the header always states how many of that set it holds |
| `/sessions/<pattern>` | yes | One session's metadata |
| `/sessions/<pattern>/messages?limit=50&order=asc&full=1` | yes | A session's messages; `order=asc` (the default) takes the earliest N, `order=desc` the latest N, and `at=<time>` anchors the window at that instant instead of at an end. Tool output and thinking come cut to a preview and marked `truncated: true`; `full=1` returns them whole (ask for a narrow window, a build log is large). Capped by `--max-limit` |
| `/sessions/<pattern>/final` | yes | A session's final result |
| `/sessions/<pattern>/rounds?source=` | yes | The session split into rounds — one per real user message (command plumbing, tool results and the rows a CLI injects do not start one): index, times, message and tool-call counts, `failures` (tool results that reported an error or an interruption), `files` touched by any tool, `filesChanged` (write-kind calls that did not fail), whether the round ended interrupted. The scan covers the **latest** 20 000 messages — `partial` / `messagesScanned` / `messagesTotal` say when it is shorter than the session, and round numbering is over that tail |
| `/sessions/<pattern>/brief?round=&at=&source=` | yes | A compact handoff brief (`text/markdown`, deterministic extraction, not a summary): the ask in the user's words, the files changed and the files touched, tools by category with how many failed, how the round ended, the state at the end, and the command that reopens the session in its own CLI (with the `cd` it needs) — with the same partial-scan note when it applies (the scan covers the latest messages; round numbering is over that tail). The rounds index marks a round with failures `✗` and an interrupted one `⚠`. Defaults to the latest round; `round=N` picks one, `at=<time>` briefs the round a timestamp falls in; `source` disambiguates when an id collides across sources |
| `/sessions/<pattern>/export?order=desc&format=md` | yes | Export the session. **No `limit` means the whole session** (up to 20 000 messages), and tool output and thinking are carried whole, never cut to a preview; the header states what it covers, and says so when it is partial. A result says how it ended (`↳ Bash — failed · exit 1 · 2.3s`) and events between the turns are quoted. Four formats: |

| `format` | Served as | For |
|---|---|---|
| `md` (default) | `text/markdown` | reading, pasting into an issue |
| `jsonl` | `application/x-ndjson` | one tagged JSON object per line (`session` / `message` / `final`) — a query with `jq` is a filter, not a parse |
| `json` | `application/json` | the same data as one document, for a consumer that wants a single parse |
| `html` | `text/html` | a page that stands on its own: inline stylesheet, no scripts, no requests, so it opens from disk and can be attached to anything |
| `/search?q=&limit=30&per_session=3&since=30d&until=&pattern=&role=` | yes | **Full-text search** across every source; `pattern` scopes it to one session, `role` keeps only `user` or `assistant` hits |
| `/projects` | yes | Session counts grouped by project (cwd) |
| `/mcp` | yes | MCP's Streamable HTTP transport, `POST`; see [mcp.md](mcp.md) |

**Authentication**: once `--hook_token` is set, endpoints marked "yes" require
`Authorization: Bearer <token>`. Without it everything is open (with a warning at startup).
Tokens are compared in constant time. CORS is off by default, see `--cors-origin`.

**Conditional requests**: `/sessions` returns an `ETag`; repeating the request with
`If-None-Match` while the list is unchanged returns `304`. The tag also changes with the
server's version, and the body carries it as `version`: a page served by the previous
version gets a full answer on its next poll and reloads itself.

**Limits and overload**: connections past the cap queue, and get a 503 if they cannot;
`?limit=` is clamped to `--max-limit`. See the
[configuration table in the README](../README.md#configuration).

**Full-text search**: `/search?q=nginx` searches **message bodies** across every enabled
source (the search on `/sessions` only matches metadata). It is case-insensitive and builds
no index — measured locally, a cold scan of 562 MB across 143 sessions takes 1.9 s and a
warm one 43-112 ms. Snippets are stripped of ANSI escape sequences and other control
characters before being returned: tool output in a transcript is full of them, and a
snippet is built for display rather than chosen by the caller. The same text is scanned for
secret-shaped runs — a known key prefix followed by a long opaque tail, a JWT, a PEM header
— and each is replaced with `[redacted]`. That covers snippets, session titles, and the
`asked` and `outcome` lines of `/rounds` and the brief: everything this service assembles
for someone to read. It is best effort by construction. It finds what announces itself and
cannot find a password written in prose, so treat it as one less sharp edge rather than a
guarantee.

Message bodies from `/sessions/<id>/messages` and the output of `/export` are returned
exactly as stored, redaction included. Those are the data; what to do with them is the
caller's policy.

In the response, `matched` is how many sessions matched, `total` how many were returned,
and `scanned` how many were examined. `truncated` carries the reasons results are short as
two separate booleans rather than one flag, because they are two different knobs:
`sessions` means more sessions matched than `limit` returned, and `hits` means some session
had more hits than `per_session` returned.

With a lot of history, narrow the scan with `since=30d` (which also accepts `12h` or
`2026-09-01`), `until` for the other end of that window, `pattern` to search inside one
session instead of all of them, and `role` (`user` or `assistant`) to keep only hits from
those messages. Each session contributes at most `per_session` snippets — with `role` set,
that counts hits matching the role rather than whichever hits happened to come first in the
file.

**`<pattern>` matching rules** (first rank to hit wins; every source takes part in every
round, so a fuzzy hit never shadows an exact hit in another source): ① exact `sessionId`
→ ② exact `key` → ③ `key` ending in `:<pattern>` or `/<pattern>` → ④ `key` substring
→ ⑤ `sessionId` substring. A leading `Session: ` or `Run: ` is stripped automatically, and a
pattern containing a colon needs URL encoding (`%3A`).

```bash
/sessions/e4b2b405-88ea-4782-a84c-92574380ed16   # Claude Code: the full uuid, or a fragment
/sessions/rollout-2026-09-13T23-07-05           # Codex: part of the rollout filename
/sessions/hook:alert:prometheus:b5123b01-...    # OpenClaw: the full key or its tail
```

**Responses**:

- List / single session: `source`, `key`, `shortKey`, `sessionId`, `file`, `hasFile`,
  `status`, `updatedAt`, `project` (cwd, or gemini's project name) and `isActive` (the
  session's newest message is less than 2 minutes old — recent activity, **not** a check
  that a process is running: a session that ended a minute ago still reports true, and one
  whose agent has been working for longer than the window reports false). `resumeCommand`
  is how to reopen the session in the CLI that wrote it. Run it in the session's `cwd`:
  the session itself is found from anywhere, but a resumed agent inherits the directory it
  was launched in, so anywhere else it carries on talking about files its tools can no
  longer reach. It is absent for `gemini`, which
  addresses sessions by position rather than identity, so a session id cannot be turned
  into a command at all; and for `openclaw`, which can resume by session id but only over
  the last 50 sessions still inside its recent-activity window and only with its Gateway
  running, so the command would work for the newest handful and fail for the rest. Then per-source extras such as
  `cwd` / `model` / `totalTokens` / `estimatedCostUsd` / `cliVersion`; a Grok session
  the user archived (it lives under `archived_sessions/`) carries `archived: true`
- Search: the list fields plus `matches` (`snippet` + `role` + `timestamp`) and `matchCount`
- Messages: `content` is an array of blocks typed `text` / `thinking` / `toolCall` /
  `toolResult` / `event`. A `toolCall` carries `id`, `name` and `arguments`; a `toolResult`
  carries `callId` (the call it answers), `toolName`, `content`, and — when the source
  recorded them — `status` (`ok` / `error` / `interrupted`), `exitCode` and `durationMs`. A
  result without `status` is one whose writer did not record the outcome (Hermes), which is
  not the same as success. An `event` is something that happened between the turns, with
  `kind` (`compaction` / `interrupted` / `hook_error` / `model_change`) and `content`. Tool
  output is cut to 500 characters and thinking to 1 000 unless `full=1` is given; a cut
  block carries `truncated: true`. A user message the CLI assembled rather than the person
  typed (Codex's AGENTS.md and environment rows) carries `injected: true` and does not start
  a round. The `order` field echoes which end the batch came from; either direction is
  returned chronologically
- final: `isFinal` / `stopReason` / `text` / `thinking` / `toolCalls` / `usage` /
  `messageCount`. When the file does not exist you get `isFinal=false` plus an `error` field
  (the HTTP status is still 200)
- Errors: 401 / 404 / 500 return `{"error": "..."}`; a 503 from the connection cap is plain
  text
- `warnings`: `[{source, error}]`, present on `/sessions`, `/health` and MCP
  `list_sessions` only when a source could not be read. Without it "no sessions" reads the
  same whether you have none or the source broke — the list is empty either way, and the
  status is still 200. A source that is merely empty reports nothing. `/health` cannot scan
  (it answers without a token), so it shows the failure the last list hit; a server that has
  not listed yet has nothing to report

## See also

- MCP transports: [mcp.md](mcp.md)
- Per-source parsing details and performance: [internals.md](internals.md)
