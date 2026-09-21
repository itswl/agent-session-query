# MCP server

Both transports are supported: `--mcp` speaks stdio, and in HTTP mode there is also
`POST /mcp`. The tool set is identical either way.

## stdio

`--mcp` runs the program as an MCP server on stdio, which lets **an agent query its own
history** — Claude Code can go looking for the same problem you solved with Codex last
week. It listens on no port and needs no token (only local processes can reach stdio in the
first place).

```json
{
  "mcpServers": {
    "agent-sessions": {
      "command": "/path/to/agent-session-query",
      "args": ["--mcp", "--mode", "auto"]
    }
  }
}
```

A Claude Code skill ships with the repository at
[`.claude/skills/agent-sessions/SKILL.md`](../.claude/skills/agent-sessions/SKILL.md). It
carries the part a tool schema cannot: which tool answers which question, how to land on a
search hit instead of paging towards it, when the answer is a file rather than context, and
what to warn about when handing a pack to another agent. Install it by pointing the skills
directory at it:

```bash
ln -s "$PWD/.claude/skills/agent-sessions" ~/.claude/skills/agent-sessions
```

## Streamable HTTP

In HTTP mode there is also `POST /mcp`, implementing the Streamable HTTP transport from the
current MCP specification (2025-06-18):

```jsonc
// Client configuration, for an MCP client that supports the HTTP transport
{ "url": "http://127.0.0.1:8080/mcp", "headers": { "Authorization": "Bearer <token>" } }
```

- Stateless: every request carries its own context and the server keeps no session, so it
  issues no `Mcp-Session-Id` (which the spec permits)
- There are no server-initiated messages, so `GET /mcp` returns `405` with `Allow: POST` as
  the spec requires, rather than holding an empty SSE stream open while the client waits
- Notifications (messages without an `id`) get an empty `202`
- **Security**: like `/sessions` it requires authentication, and it validates `Origin` to
  prevent DNS rebinding as the spec requires — native MCP clients send no `Origin`, and one
  that is present must match `--cors-origin` or the request gets a `403`

## Tools

| Tool | What it does |
|------|--------------|
| `search_sessions` | Full-text search over message bodies (ids and timestamps are not searched as text); takes `query` / `limit` / `per_session` / `since` / `until` / `cursor`, plus `pattern` to search inside one session and `role` to keep only `user` or `assistant` hits |
| `list_sessions` | List sessions newest first, filtered by `source` / `project` / `since` / `until`; paginates |
| `list_projects` | Group sessions by project (cwd) — which agents were used on a given repository, and how many sessions each |
| `get_session` | One session's metadata and final result; `source` disambiguates when an id collides |
| `get_messages` | A session's messages; `order` picks the end, `role` narrows to `user` or `assistant`, `at` anchors the window at an instant, `cursor` pages |
| `session_brief` | A compact handoff brief of one round — the ask, files touched, tools by category, how it ended. Sessions are segmented into rounds at each real user message; the default is the latest round, `round` picks one, `at` briefs the round a timestamp (e.g. a search hit) falls in |

Every tool declares `readOnlyHint` / `idempotentHint` annotations, so clients that honour
them can skip call confirmations for what is a read-only query service.

`list_sessions` carries a `warnings: [{source, error}]` field when a source could not be
read. An empty `sessions` array means an empty source only when there is no warning: without
it, "this machine has no Hermes sessions" and "the Hermes database could not be read" arrive
as the same answer, which is worth checking before concluding the history is not there.

## How the tools fit together

Three questions, in the order they usually get asked. Knowing the shape saves a client from
reading far more than it needs.

**"Which session was X in?"** — `search_sessions`. The hits carry a `sessionId`, a role and
a snippet, so the answer is often readable without opening anything. `role=user` narrows to
what the human asked, which is usually what you are looking for when the answer echoes the
question. `pattern` scopes the search to one session when you already know which one.

**"What did that session come to?"** — `get_session`. Metadata, and the final result: what
the session concluded. For most questions this is the whole answer, and it costs a
kilobyte.

**"Show me what happened around there."** — `get_messages`. `order=desc` gives the latest N,
which is where a session's outcome lives; `at=<timestamp>` starts the window at a given
instant, which is how you land on a search hit rather than paging towards it — pass the
`timestamp` of the match you got from `search_sessions`.

### Reading a whole session

`get_messages` returns a window, not a session, and that is deliberate: a long-running
session can hold tens of thousands of messages, and one response carrying all of them would
be tens of megabytes of context. Page through with `cursor` when you need everything.

If you need the whole thing **as a file rather than as context** — to grep, to analyse with
code, to hand to something else — that is the HTTP export, which is not an MCP tool:

```
GET /sessions/<sessionId>/export?format=jsonl     # one JSON object per line, for jq
GET /sessions/<sessionId>/export                  # Markdown, for reading
GET /export?project=&since=                       # a pack: many sessions, one document
```

A pack is worth knowing about for "what has been going on lately": one entry per session —
what was asked and what it concluded, oldest first — on the order of a few kilobytes per
session, because it carries the pair rather than the transcripts.

## Pagination

`search_sessions`, `list_sessions` and `get_messages` paginate: when a page is not the
last, the result carries `nextCursor`; pass it back as the `cursor` argument to continue.
A cursor is only meaningful for the same tool and the same other arguments. Pages never
overlap, and a past-the-end cursor yields an empty page.

One caveat on `get_messages`: without `role`, the server reads only as deep into the
session as the page needs (window + one probe message), so `total` there is the window
size, not the session's full message count. With `role` set the whole session is read and
`total` counts the filtered messages exactly. `list_sessions` and `search_sessions`
report the true totals — `matched` / `total` cover everything before pagination.

## Time bounds

`since` / `until` bound a session's update time on either side, in any of the relative
forms (`24h`, `7d`) or an absolute date (`2026-09-01`). A session with no parseable time
falls outside a bounded query rather than being guessed onto either side.
