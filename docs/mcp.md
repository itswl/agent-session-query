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
| `search_sessions` | Full-text search over message bodies (ids and timestamps are not searched as text); takes `query` / `limit` / `per_session` / `since` / `until` |
| `list_sessions` | List sessions newest first, optionally filtered by `source` / `project` / `since` / `until` |
| `list_projects` | Group by project to see which agents were used on a given repository |
| `get_session` | One session's metadata and final result; optional `source` disambiguates when ids collide |


Every tool declares `readOnlyHint` / `idempotentHint` annotations, so clients that honour
them can skip call confirmations for what is a read-only query service.

### Pagination

`search_sessions`, `list_sessions` and `get_messages` paginate: when a page is not the
last, the result carries `nextCursor`; pass it back as the `cursor` argument to continue.
A cursor is only meaningful for the same tool and the same other arguments. Pages never
overlap, and a past-the-end cursor yields an empty page.

One caveat on `get_messages`: without `role`, the server reads only as deep into the
session as the page needs (window + one probe message), so `total` there is the window
size, not the session's full message count. With `role` set the whole session is read and
`total` counts the filtered messages exactly. `list_sessions` and `search_sessions`
report the true totals — `matched` / `total` cover everything before pagination.

### Time bounds

`since` / `until` bound a session's update time on either side, in any of the relative
forms (`24h`, `7d`) or an absolute date (`2026-09-01`). A session with no parseable time
falls outside a bounded query rather than being guessed onto either side.
| `get_messages` | A session's messages; `order=desc` returns the latest N, `role` narrows to `user` or `assistant` |
