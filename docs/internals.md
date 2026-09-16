# 实现细节

面向维护者的内部文档：各数据源的解析细节、与 Python 版的对拍结论、性能数据。
使用与部署方式见 [README](../README.md)。

## 各数据源的解析细节

- **Hermes**：`sessions.json` 是 `key → 记录` 的映射，会话内容在同目录的 `<session_id>.jsonl`；消息取 `role` 为 `user`/`assistant` 的行，`content` 是字符串，推理在 `reasoning`。**新版 Hermes 不写 `sessions.json` / jsonl，会话全部落在 `~/.hermes/state.db`**：库存在即启用该源，列表与消息直接查 `sessions` / `messages` 表（时间戳是 epoch 秒，格式化成 UTC；assistant 的 `reasoning` 作为 thinking；`platform` 取 `sessions.source`）；两处都有时按 sessionId 去重，jsonl 优先。没有 jsonl 的会话 `final` 也回退到 `state.db`（只读打开，取最后一条 `active=1` 且 `finish_reason=stop` 的助手消息，`message_count` 缺失时回退成实际条数）
- **OpenClaw**：同上结构，字段名是 `sessionId`/`stopReason`；消息取 `type=message` 的行，`message.content` 是块数组（`text`/`thinking`/`toolCall`/`toolResult`），`stopReason` 可能在 `message` 里也可能在行顶层
- **Pi**：行类型有 `session`（首行元数据）、`model_change`、`message`；消息取 `message` 行（`message.role` + `message.content` 块数组）
- **Claude Code**：消息行是 `type=user`/`assistant`，内容在 `message.content`（`text`/`thinking`/`tool_use`/`tool_result` 块）；跳过 `isSidechain`（子代理）以及 `queue-operation`、`attachment`、`mode` 等非对话行
- **Codex**：消息行是 `type=response_item` 且 `payload.type=message`；`payload.role` 为 `developer` 的行（系统拼装的指令）不计入；用量取自 `token_usage_record`
- **Gemini CLI**：文件是追加日志——首行元数据（`sessionId`/`startTime`）、`{"$set": {...}}` 补丁行、以及消息行；消息取 `type=user`/`type=gemini` 的行，`content` 可能是数组（user）或字符串（gemini）。工具调用型会话里正文很稀：发起调用时 `content` 是空串、内容在 `toolCalls` 字段（→ `toolCall` 块，只带 name/args），执行结果由后续 user 行 `content` 数组里的 `functionResponse` 项回传（→ `toolResult` 块）；`thoughts` 字符串或 `[{subject, description}]` 数组都作为 thinking（数组取各条 description）

## 与 Python 版的差异

本服务是从 Python 单文件版重写的（最后的 Python 版本是 `c7da626`，`git show c7da626:session_query_api.py` 可取回）。

两者做了逐请求对拍验证：**60 条**固定用例（六个源的构造数据，覆盖匹配优先级、编码、limit 边界、错误分支，以及 Hermes `state.db` 回退）+ **33 项**真实数据（本机 11 个 Pi 会话、8 个 Claude Code 会话、6 个 Codex、3 个 Gemini 的列表/详情/消息/结果/按文件名查找），响应**逐字段一致**。

已知的行为差异只有这些：

1. **`HEAD` 请求**：Python 版返回 501；这一版按 `GET` 处理（响应无 body）。对探活工具更友好。
2. **503 的响应体**：都是 503，但 Go 的 HTTP 状态原因短语是标准的 `Service Unavailable`。
3. **`/stats` 的 `bad_requests`**：HTTP 协议层的畸形请求由 Go 的 `net/http` 自行处理并直接断开，不会逐条进这个计数；这一版统计的是服务端记录的协议层错误。其余三个计数字段（max/active/total connections）语义不变。
4. 启动横幅与请求日志的格式略有不同（内容等价）。
5. **Hermes `state.db` 的查询少查了一列**（这一版更宽）。Python 版的 SQL 里 `SELECT` 了 `token_count` 却从未使用它——要是哪个版本的 Hermes 库没有这一列，它会直接抛 `no such column`，整个回退静默失效（只留一行 WARN，接口返回「会话文件不存在」）。这一版只查真正用得到的列，遇到这种库仍然能给出结果。

Go 版之后新增的能力（Python 版没有）：Hermes `state.db` 作为一等数据源（列表 / 消息直读）、Gemini 工具调用会话的解析、内嵌 `/ui` 页面。

## 性能

800 个会话（四个数据源各 200 个，其中 1/4 是 3000 行的大会话，共约 208 MB）的合成数据，服务与压测客户端在同一台 arm64 机器上，各跑三轮取中位数：

| 端点 | Python 版 | Go 版 |
|------|-----------|-------|
| `GET /sessions`（列 800 条） | 19.4 ms | **14.2 ms** |
| `GET /sessions/<pattern>`（精确查一条） | 5.9 ms | **3.8 ms** |
| `GET /sessions/<pattern>/messages?limit=10`（大会话） | 6.2 ms | **3.2 ms** |
| `GET /sessions/<pattern>/final`（大会话） | 30.9 ms | 35.5 ms |
| `GET /health` | 3.5 ms | **1.5 ms** |
| 常驻内存（压测后 RSS） | 20.8 MiB | **11.0 MB** |

几个实现上的点：

1. **列表不扫全文**——每个会话只读首行元数据（Gemini 的 `updatedAt` 优先用元数据里的时间，缺了才退回文件时间），所以 `GET /sessions` 是「stat + 读首行」而不是「读 208 MB」
2. **流式读取**——`messages` 取够 `limit` 条就停；`final` 只保留最后一条助手消息
3. **`final` 用结构体「探测」再物化**——整文件扫描时只解析 `type`/`role` 这类标量字段（Go 的 JSON 解码器会跳过不关心的字段，不建 map），只在最后一条助手消息上做完整解析。不做这一步时这个端点是 50 ms（Go 的 `encoding/json` 比 Python 的 C 实现慢），做了之后追平
4. **行读取复用缓冲**——`bufio.Scanner` 的 `Bytes()` 是内部缓冲的视图，每行不额外分配（实测比逐行 `ReadBytes` 快约 40%）
5. **会话列表短 TTL 缓存**（`--cache-ttl`，默认 2 秒）——查找会话与列表都走它；**消息与最终结果始终直接读文件**，缓存只影响「有哪些会话」这层元数据；`--cache-ttl 0` 可完全关掉

边界与代价：

- 新建的会话最长 2 秒后才会出现在列表/查询里（可调小或设为 0）
- `final` 仍需顺序读完整个会话文件（要取最后一条助手消息），3000 行的大会话约 35 ms——这是单文件顺序读 + 解析的固有成本
- 单行超过 256 MB 的文件会被跳过（防御性上限，正常会话不可能到这个量级）
