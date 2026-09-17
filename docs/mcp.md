# MCP server

两种传输都支持：`--mcp` 走 stdio，HTTP 模式下另有 `POST /mcp`。工具集是同一套。

## stdio

`--mcp` 让它跑在 stdio 上当 MCP server，于是 **Agent 可以查自己的历史**——让 Claude Code
去搜你上周用 Codex 解决过的同一个问题。不监听端口，也不需要令牌（stdio 本来就只有本机进程能连）。

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

HTTP 模式下还有一个 `POST /mcp`，走 MCP 现行规范（2025-06-18）的 Streamable HTTP 传输：

```jsonc
// 客户端配置（以支持 HTTP 传输的 MCP 客户端为例）
{ "url": "http://127.0.0.1:8080/mcp", "headers": { "Authorization": "Bearer <token>" } }
```

- 无状态实现：每个请求自带全部上下文，服务端不留会话，因此不签发 `Mcp-Session-Id`（规范允许）
- 没有服务端主动推送的消息，所以 `GET /mcp` 按规范返回 `405` + `Allow: POST`，
  而不是挂一条空 SSE 让客户端干等
- 通知（没有 `id` 的消息）返回 `202` 空响应
- **安全**：和 `/sessions` 一样要认证；另外按规范校验 `Origin` 防 DNS rebinding——
  原生 MCP 客户端不带 `Origin`，带了就必须和 `--cors-origin` 对得上，否则 `403`

## 工具

| 工具 | 说明 |
|------|------|
| `search_sessions` | 全文搜正文，参数 `query` / `limit` / `per_session` / `since` |
| `list_sessions` | 按更新时间倒序列会话，可按 `source` / `project` 过滤 |
| `list_projects` | 按项目归拢，看同一个仓库上用过哪几个 Agent |
| `get_session` | 取一个会话的元信息与最终结果 |
| `get_messages` | 取消息，`order=desc` 拿最新的 N 条 |
