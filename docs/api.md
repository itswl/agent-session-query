# HTTP API

查询端点都是 `GET`（只有 MCP 的 `/mcp` 是 `POST`，见 [mcp.md](mcp.md)）。`/sessions` 系列额外支持
`/api/sessions` 前缀写法；`/`、`/health`、`/stats` 没有别名。

| 端点 | 认证 | 说明 |
|------|------|------|
| `/` `/health` `/stats` | 免 | 服务信息 / 健康检查（含连接统计与 `authRequired`） |
| `/sessions` | 需要 | 列出所有会话（多源合并，按更新时间倒序） |
| `/sessions/<pattern>` | 需要 | 单个会话的信息 |
| `/sessions/<pattern>/messages?limit=50&order=asc` | 需要 | 会话消息；`order=asc`（默认）取最早的 N 条，`order=desc` 取最新的 N 条 |
| `/sessions/<pattern>/final` | 需要 | 会话的最终结果 |
| `/sessions/<pattern>/export?limit=200&order=desc` | 需要 | 导出成 Markdown（`text/markdown` + `Content-Disposition`） |
| `/search?q=&limit=30&per_session=3&since=30d` | 需要 | **全文搜内容**，跨所有数据源 |
| `/projects` | 需要 | 按项目（cwd）归拢的会话统计 |
| `/mcp` | 需要 | MCP 的 Streamable HTTP 传输，`POST`；详见 [mcp.md](mcp.md) |

**认证**：配置了 `--hook_token` 后，标「需要」的端点要带 `Authorization: Bearer <token>`；未配置时
全部免认证（启动时打印警告），令牌比较用常量时间比较。跨域默认关闭，见 `--cors-origin`。

**条件请求**：`/sessions` 返回 `ETag`，带 `If-None-Match` 重复请求且列表没变时返回 `304`。

**过载与上限**：并发超限先排队、排不上返回 503；`?limit=` 夹在 `--max-limit` 以内。参数见 [README 的配置表](../README.md#配置)。

**全文搜索**：`/search?q=nginx` 在所有启用的数据源里搜**消息正文**（`/sessions` 的搜索只匹配元数据）。
大小写无关，不建索引——实测本机 470 MB / 174 个会话冷扫 1.1 秒、热 60 ms。返回里
`matched` 是命中的会话总数、`total` 是实际返回的条数、`scanned` 是扫过的会话数；历史很大时用
`since=30d`（也接受 `12h` / `2026-09-01`）把范围收窄。每个会话最多给 `per_session` 条片段。

**`<pattern>` 匹配规则**（按序先命中先返回，所有数据源一起参与每一轮，模糊命中不会盖掉其它源的精确命中）：① 精确 `sessionId` → ② 精确 `key` → ③ `key` 以 `:<pattern>` 或 `/<pattern>` 结尾 → ④ `key` 子串 → ⑤ `sessionId` 子串。`Session: ` / `Run: ` 前缀自动剥掉；含冒号的 pattern 记得 URL 编码（`%3A`）。

```bash
/sessions/e4b2b405-88ea-4782-a84c-92574380ed16   # Claude Code：完整 uuid，片段也行
/sessions/rollout-2026-09-13T23-07-05           # Codex：rollout 文件名片段
/sessions/hook:alert:prometheus:b5123b01-...    # OpenClaw：完整 key 或后半段
```

**响应**：

- 列表 / 单条：`source`、`key`、`shortKey`、`sessionId`、`file`、`hasFile`、`status`、`updatedAt`、
  `project`（cwd 或 gemini 的项目名）、`isActive`（更新时间在 2 分钟内 = 正被写入），按源附加
  `cwd` / `model` / `totalTokens` / `estimatedCostUsd` / `cliVersion` 等
- 搜索：在列表字段基础上附加 `matches`（`snippet` + `role` + `timestamp`）与 `matchCount`
- 消息：`content` 是块数组，块类型 `text` / `thinking` / `toolCall`（`name`+`arguments`）/ `toolResult`（`toolName`+`content`）；响应里的 `order` 回显这批是从哪一头取的，两个方向拿到的都按时间先后排
- final：`isFinal` / `stopReason` / `text` / `thinking` / `toolCalls` / `usage` / `messageCount`；文件不存在时 `isFinal=false` + `error` 字段（HTTP 仍 200）
- 错误：401 / 404 / 500 返回 `{"error": "..."}`，503 并发超限为纯文本

## 相关

- MCP 传输见 [mcp.md](mcp.md)
- 各数据源的解析细节与性能见 [internals.md](internals.md)
