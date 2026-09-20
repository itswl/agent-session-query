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
| `/sessions/<pattern>/messages?limit=50&order=asc` | yes | A session's messages; `order=asc` (the default) takes the earliest N, `order=desc` the latest N, and `at=<time>` anchors the window at that instant instead of at an end. Capped by `--max-limit` |
| `/sessions/<pattern>/final` | yes | A session's final result |
| `/sessions/<pattern>/export?order=desc&format=md` | yes | Export the session. **No `limit` means the whole session** (up to 20 000 messages); the header states what it covers, and says so when it is partial. Four formats: |

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
`If-None-Match` while the list is unchanged returns `304`.

**Limits and overload**: connections past the cap queue, and get a 503 if they cannot;
`?limit=` is clamped to `--max-limit`. See the
[configuration table in the README](../README.md#configuration).

**Full-text search**: `/search?q=nginx` searches **message bodies** across every enabled
source (the search on `/sessions` only matches metadata). It is case-insensitive and builds
no index — measured locally, a cold scan of 470 MB across 174 sessions takes 1.1 s and a
warm one 60 ms. In the response, `matched` is how many sessions matched, `total` how many
were returned, and `scanned` how many were examined; with a lot of history, narrow the scan
with `since=30d` (which also accepts `12h` or `2026-09-01`), `until` for the other end of
that window, `pattern` to search inside one session instead of all of them, and `role`
(`user` or `assistant`) to keep only hits from those messages. Each session contributes at
most `per_session` snippets — with `role` set, that counts hits matching the role rather
than whichever hits happened to come first in the file.

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
  `status`, `updatedAt`, `project` (cwd, or gemini's project name) and `isActive` (updated
  within the last 2 minutes, i.e. currently being written), plus per-source extras such as
  `cwd` / `model` / `totalTokens` / `estimatedCostUsd` / `cliVersion`
- Search: the list fields plus `matches` (`snippet` + `role` + `timestamp`) and `matchCount`
- Messages: `content` is an array of blocks typed `text` / `thinking` / `toolCall`
  (`name` + `arguments`) / `toolResult` (`toolName` + `content`). The `order` field echoes
  which end the batch came from; either direction is returned chronologically
- final: `isFinal` / `stopReason` / `text` / `thinking` / `toolCalls` / `usage` /
  `messageCount`. When the file does not exist you get `isFinal=false` plus an `error` field
  (the HTTP status is still 200)
- Errors: 401 / 404 / 500 return `{"error": "..."}`; a 503 from the connection cap is plain
  text

## See also

- MCP transports: [mcp.md](mcp.md)
- Per-source parsing details and performance: [internals.md](internals.md)
