# 实现细节

面向维护者的内部文档：各数据源的解析细节与性能说明。使用与部署方式见 [README](../README.md)。

## 各数据源的解析细节

- **Hermes**：`sessions.json` 是 `key → 记录` 的映射，会话内容在同目录的 `<session_id>.jsonl`；消息取 `role` 为 `user`/`assistant` 的行，`content` 是字符串，推理在 `reasoning`。**新版 Hermes 不写 `sessions.json` / jsonl，会话全部落在 `~/.hermes/state.db`**：库存在即启用该源，列表与消息直接查 `sessions` / `messages` 表（时间戳是 epoch 秒，格式化成 UTC；assistant 的 `reasoning` 作为 thinking；`platform` 取 `sessions.source`）；两处都有时按 sessionId 去重，jsonl 优先。没有 jsonl 的会话 `final` 也回退到 `state.db`（只读打开，取最后一条 `active=1` 且 `finish_reason=stop` 的助手消息，`message_count` 缺失时回退成实际条数）
- **OpenClaw**：同上结构，字段名是 `sessionId`/`stopReason`；消息取 `type=message` 的行，`message.content` 是块数组（`text`/`thinking`/`toolCall`/`toolResult`），`stopReason` 可能在 `message` 里也可能在行顶层
- **Pi**：行类型有 `session`（首行元数据）、`model_change`、`message`；消息取 `message` 行（`message.role` + `message.content` 块数组）
- **Claude Code**：消息行是 `type=user`/`assistant`，内容在 `message.content`（`text`/`thinking`/`tool_use`/`tool_result` 块）；跳过 `isSidechain`（子代理）以及 `queue-operation`、`attachment`、`mode` 等非对话行
- **Codex**：消息行是 `type=response_item` 且 `payload.type=message`；`payload.role` 为 `developer` 的行（系统拼装的指令）不计入；用量取自 `token_usage_record`
- **Gemini CLI**：文件是追加日志——首行元数据（`sessionId`/`startTime`）、`{"$set": {...}}` 补丁行、以及消息行；消息取 `type=user`/`type=gemini` 的行，`content` 可能是数组（user）或字符串（gemini）。工具调用型会话里正文很稀：发起调用时 `content` 是空串、内容在 `toolCalls` 字段（→ `toolCall` 块，只带 name/args），执行结果由后续 user 行 `content` 数组里的 `functionResponse` 项回传（→ `toolResult` 块）；`thoughts` 字符串或 `[{subject, description}]` 数组都作为 thinking（数组取各条 description）

## 性能

800 个会话（四个数据源各 200 个，其中 1/4 是 3000 行的大会话，共约 208 MB）的合成数据，服务与压测客户端在同一台 arm64 机器上，取三轮中位数：

| 端点 | 耗时 |
|------|------|
| `GET /sessions`（列 800 条） | 14.2 ms |
| `GET /sessions/<pattern>`（精确查一条） | 3.8 ms |
| `GET /sessions/<pattern>/messages?limit=10`（大会话） | 3.2 ms |
| `GET /sessions/<pattern>/final`（大会话） | 35.5 ms |
| `GET /health` | 1.5 ms |
| 常驻内存（压测后 RSS） | 11.0 MB |

实现要点：

1. **列表不扫全文**——每个会话只读首行元数据（Gemini 的 `updatedAt` 优先用元数据里的时间，缺了才退回文件时间），`GET /sessions` 是「stat + 读首行」而不是「读 208 MB」
2. **流式读取**——`messages` 取够 `limit` 条就停；`final` 只保留最后一条助手消息
3. **`final` 用结构体「探测」再物化**——整文件扫描时只解析 `type`/`role` 这类标量字段（Go 的 JSON 解码器会跳过不关心的字段，不建 map），只在最后一条助手消息上做完整解析
4. **行读取复用缓冲**——`bufio.Scanner` 的 `Bytes()` 是内部缓冲的视图，每行不额外分配
5. **会话列表短 TTL 缓存**（`--cache-ttl`，默认 2 秒）——查找会话与列表都走它；**消息与最终结果始终直接读文件**，缓存只影响「有哪些会话」这层元数据；`--cache-ttl 0` 可完全关掉

边界与代价：

- 新建的会话最长 2 秒后才会出现在列表/查询里（可调小或设为 0）
- `final` 仍需顺序读完整个会话文件（要取最后一条助手消息），3000 行的大会话约 35 ms——单文件顺序读 + 解析的固有成本
- 单行超过 256 MB 的文件会被跳过（防御性上限，正常会话不可能到这个量级）
