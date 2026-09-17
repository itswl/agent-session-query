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
| `search_sessions` | Full-text search over message bodies; takes `query` / `limit` / `per_session` / `since` |
| `list_sessions` | List sessions newest first, optionally filtered by `source` / `project` |
| `list_projects` | Group by project to see which agents were used on a given repository |
| `get_session` | One session's metadata and final result |
| `get_messages` | A session's messages; `order=desc` returns the latest N |
