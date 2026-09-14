# 本地 Agent 会话查询 HTTP API

一个只读的 HTTP API，用来查询本机上各种 Agent / CLI 的会话记录：列表、消息、最终结果。不改动任何会话数据，只用 Python 标准库（无需 pip 安装）。

**一个接口，六种数据源**——存在哪几种就查哪几种，可以同时合并查询：

| 数据源 | 会话位置 | 会话 ID |
|--------|----------|---------|
| Hermes | `~/.hermes/sessions/` | `sessions.json` 里的 `session_id` |
| OpenClaw | `~/.openclaw/agents/default/sessions/` | `sessions.json` 里的 `sessionId` |
| Pi | `~/.pi/agent/sessions/<项目>/*.jsonl` | 会话文件首行 `id` |
| Claude Code | `~/.claude/projects/<项目>/*.jsonl` | 文件名（uuid）/ `sessionId` 字段 |
| Codex | `~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl` | 首行 `payload.session_id` |
| Gemini CLI | `~/.gemini/tmp/<项目>/chats/session-*.jsonl` | 首行 `sessionId` |

## 快速开始

### 本地运行

```bash
# 自动检测：存在的数据源都启用
python3 session_query_api.py --port 8080

# 只看某一种
python3 session_query_api.py --mode claude

# 强制全部启用（缺哪个会打印警告）
python3 session_query_api.py --mode all

# 带认证
python3 session_query_api.py --port 8080 --hook_token mysecrettoken
```

启动后会打印本次启用的数据源：

```
运行模式: auto
数据源 [pi]: /home/me/.pi/agent/sessions
数据源 [claude]: /home/me/.claude/projects
数据源 [codex]: /home/me/.codex/sessions
已启用认证（Bearer hook_token 已设置，不回显）
```

### Docker

```bash
docker build -t agent-session-query .          # 国内加 --build-arg PYTHON_IMAGE=<镜像源>

docker run -d --name agent-session-query -p 8080:8080 \
  -v ~/.claude/projects:/root/.claude/projects:ro \
  -v ~/.codex/sessions:/root/.codex/sessions:ro \
  -v ~/.pi/agent/sessions:/root/.pi/agent/sessions:ro \
  -v ~/.gemini/tmp:/root/.gemini/tmp:ro \
  -v ~/.hermes/sessions:/root/.hermes/sessions:ro \
  -v ~/.openclaw/agents/default/sessions:/root/.openclaw/agents/default/sessions:ro \
  agent-session-query --hook_token mysecrettoken
```

镜像的 ENTRYPOINT 就是脚本本身，`docker run` 之后的参数直接透传（`--mode`、`--hook_token`、`--port`…）。挂几个目录就查几个源。

### Docker Compose

```bash
docker-compose up -d                                              # 六个源目录都已挂载
HOOK_TOKEN=mysecrettoken SESSION_MODE=auto docker-compose up -d   # 带认证
```

## API

所有端点都是 `GET`。`/sessions` 系列额外支持 `/api/sessions` 前缀写法（`/api/sessions` 与 `/sessions` 等价）；`/`、`/health`、`/stats` 没有别名。

| 端点 | 认证 | 说明 |
|------|------|------|
| `/` | 免 | 服务信息（名称、模式、启用的数据源、端点清单） |
| `/health` | 免 | 健康检查（模式、启用的数据源、连接统计） |
| `/stats` | 免 | 连接统计 |
| `/sessions` | 需要 | 列出所有会话（多源合并，按更新时间倒序） |
| `/sessions/<pattern>` | 需要 | 单个会话的信息 |
| `/sessions/<pattern>/messages?limit=50` | 需要 | 会话消息（默认 50 条，取最早的前 N 条） |
| `/sessions/<pattern>/final` | 需要 | 会话的最终结果 |

**认证**：配置了 `--hook_token` 后，上表标「需要」的端点要带 `Authorization: Bearer <token>`；未配置 token 时全部免认证（启动时会打印警告）。令牌比较用的是常量时间比较。

**过载保护**：并发连接超过 `--max-connections`（默认 50）时，新连接直接返回 503；每个连接有 `--timeout`（默认 30 秒）超时。

### `<pattern>` 的匹配规则

按下面的顺序找，先命中先返回；**所有数据源一起参与每一轮**，所以某个源里的模糊命中不会盖掉另一个源里的精确命中：

1. 精确等于 `sessionId`
2. 精确等于 `key`（完整路径或完整 key）
3. `key` 以 `:<pattern>` 或 `/<pattern>` 结尾
4. `pattern` 是 `key` 的子串
5. `pattern` 是 `sessionId` 的子串

另外 `Session: ` 和 `Run: ` 前缀会被自动去掉。几个实际例子：

```bash
/sessions/e4b2b405-88ea-4782-a84c-92574380ed16          # Claude Code：完整 uuid
/sessions/ee64d13e                                       # 也可以只给片段
/sessions/2026-09-13T12-50-41                            # Pi / Gemini：文件名片段
/sessions/rollout-2026-09-13T23-07-05                    # Codex：rollout 文件名片段
/sessions/hook:alert:prometheus:b5123b01-616a-4da0-...   # OpenClaw：完整 key 或后半段
/sessions/agent%3Amain%3Awebhook%3Aagent%3A1776...       # 含冒号时记得 URL 编码
```

## 响应结构

### `GET /sessions`

```json
{
  "sessions": [
    {
      "source": "claude",
      "key": "/root/.claude/projects/-home-me--proj/e4b2b405-88ea-4782-a84c-92574380ed16.jsonl",
      "shortKey": "e4b2b405-88ea-4782-a84c-92574380ed16",
      "sessionId": "e4b2b405-88ea-4782-a84c-92574380ed16",
      "file": "/root/.claude/projects/-home-me--proj/e4b2b405-88ea-4782-a84c-92574380ed16.jsonl",
      "hasFile": true,
      "status": "done",
      "cwd": "/home/me/proj",
      "updatedAt": "2026-09-14T07:41:48"
    }
  ],
  "total": 1
}
```

各源共有的字段：`source`、`key`、`shortKey`、`sessionId`、`file`、`hasFile`、`status`、`updatedAt`。

按源额外的字段：

| 数据源 | 额外字段 |
|--------|----------|
| OpenClaw | `model`、`runtimeMs`、`totalTokens` |
| Hermes | `createdAt`、`displayName`、`platform`、`totalTokens`、`estimatedCostUsd` |
| Pi / Claude Code | `cwd` |
| Codex | `cwd`、`cliVersion` |
| Gemini CLI | `project` |

字段来源：OpenClaw/Hermes 取自 `sessions.json` 里的记录（OpenClaw 的 `updatedAt` 由毫秒时间戳格式化）；其余四种用会话文件自身的元数据/修改时间。列表按 `updatedAt` 倒序，`source` 表示这条记录来自哪个数据源。

### `GET /sessions/<pattern>`

字段与列表中的同一条记录完全一致（包含 `file`）：

```json
{
  "source": "claude",
  "key": "/root/.claude/projects/-home-me--proj/e4b2b405-88ea-4782-a84c-92574380ed16.jsonl",
  "shortKey": "e4b2b405-88ea-4782-a84c-92574380ed16",
  "sessionId": "e4b2b405-88ea-4782-a84c-92574380ed16",
  "file": "/root/.claude/projects/-home-me--proj/e4b2b405-88ea-4782-a84c-92574380ed16.jsonl",
  "hasFile": true,
  "status": "done",
  "cwd": "/home/me/proj",
  "updatedAt": "2026-09-14T07:41:48"
}
```

找不到返回 `404` + `{"error": "Session not found"}`。

### `GET /sessions/<pattern>/messages?limit=50`

```json
{
  "messages": [
    {
      "id": "252e50d5-f6fe-4682-acaa-4f61e0df4285",
      "role": "user",
      "timestamp": "2026-09-14T07:14:06.596Z",
      "content": [
        { "type": "text", "content": "写一句 10 个字以内的问候语" }
      ]
    },
    {
      "id": "abc123",
      "role": "assistant",
      "timestamp": "2026-09-14T07:14:08.100Z",
      "content": [
        { "type": "thinking", "content": "……（超过 1000 字会截断并加 ...[truncated]）" },
        { "type": "text", "content": "您好，很高兴协助您！" }
      ]
    }
  ],
  "total": 2
}
```

- `content` 是块数组，块类型：`text` / `thinking` / `toolCall` / `toolResult`，各自的键分别为 `content`、`content`、`name`+`arguments`、`toolName`+`content`
- `limit` 取的是**最早的前 N 条**（默认 50），不是最近 N 条
- 会话文件还不存在时返回空数组（不是 404）

### `GET /sessions/<pattern>/final`

```json
{
  "status": "done",
  "isFinal": true,
  "isProcessing": false,
  "messageCount": 4,
  "source": "codex",
  "id": "msg_05c9...",
  "timestamp": "2026-09-13T15:08:57.823Z",
  "stopReason": "",
  "model": "gpt-5.6-luna",
  "text": "已轮询并回复该消息：……",
  "thinking": "",
  "toolCalls": [],
  "usage": { "input_tokens": 18034, "output_tokens": 104 }
}
```

取值规则：

- **OpenClaw / Hermes**：取第一个 `stopReason`/`finish_reason` 为 `stop` 的助手消息；此时 `isFinal=true`。若该消息之后还有指向它的 `toolResult` 且会话仍在 `running`，则 `isProcessing=true`、`isFinal=false`
- **Pi / Claude Code / Codex / Gemini**：取最后一条助手消息；`isFinal` 按各自的状态判定（Pi 看 `stopReason=stop`，Claude Code 看 `stop_reason` 属于 `end_turn`/`stop`/`stop_sequence`，Codex 与 Gemini 视为已完成）
- Hermes 的 webhook 会话如果只把最终消息写进了 `~/.hermes/state.db`，服务会自动回退去查 SQLite
- 会话文件还不存在时，返回 `isFinal=false` + `error` 字段说明原因（HTTP 仍是 200）

### 错误响应

| 场景 | 状态码 | 响应体 |
|------|--------|--------|
| 未带 / 错误的 token | 401 | `{"error": "Unauthorized"}` |
| 找不到会话 | 404 | `{"error": "Session not found"}` |
| 未知路径 | 404 | `{"error": "Not found"}` |
| 服务内部异常 | 500 | `{"error": "Internal server error"}` |
| 并发超限 | 503 | HTTP 状态行 `Service temporarily overloaded` |

## 配置

### 命令行参数

| 参数 | 默认 | 说明 |
|------|------|------|
| `--host` | `0.0.0.0` | 监听地址 |
| `--port` | `8080` | 监听端口 |
| `--mode` | `auto` | `auto`（存在即启用）/ `all`（六个都启用）/ `hermes` / `openclaw` / `pi` / `claude` / `codex` / `gemini` |
| `--hook_token` | 无 | Bearer 令牌；不设置则免认证 |
| `--max-connections` | `50` | 最大并发连接数，超出返回 503 |
| `--timeout` | `30` | 单连接超时（秒） |
| `--cache-ttl` | `2` | 会话列表缓存秒数；`0` = 不缓存（每次都重新扫描） |

### Docker 环境变量 / 构建参数

| 变量 | 用途 |
|------|------|
| `HOOK_TOKEN` | compose 传给 `--hook_token`（留空则不认证） |
| `SESSION_MODE` | compose 传给 `--mode`，默认 `auto` |
| `PYTHON_IMAGE` | 构建参数，基础镜像，默认 `python:3.10-slim`（国内可换镜像源） |

## 各数据源的解析细节

- **Hermes**：`sessions.json` 是 `key → 记录` 的映射，会话内容在同目录的 `<session_id>.jsonl`；消息取 `role` 为 `user`/`assistant` 的行，`content` 是字符串，推理在 `reasoning`
- **OpenClaw**：同上结构，字段名是 `sessionId`/`stopReason`；消息取 `type=message` 的行，`message.content` 是块数组（`text`/`thinking`/`toolCall`/`toolResult`），`stopReason` 可能在 `message` 里也可能在行顶层
- **Pi**：行类型有 `session`（首行元数据）、`model_change`、`message`；消息取 `message` 行（`message.role` + `message.content` 块数组）
- **Claude Code**：消息行是 `type=user`/`assistant`，内容在 `message.content`（`text`/`thinking`/`tool_use`/`tool_result` 块）；跳过 `isSidechain`（子代理）以及 `queue-operation`、`attachment`、`mode` 等非对话行
- **Codex**：消息行是 `type=response_item` 且 `payload.type=message`；`payload.role` 为 `developer` 的行（系统拼装的指令）不计入；用量取自 `token_usage_record`
- **Gemini CLI**：文件是追加日志——首行元数据（`sessionId`/`startTime`）、`{"$set": {...}}` 补丁行、以及消息行；消息取 `type=user`/`type=gemini` 的行，`content` 可能是数组（user）或字符串（gemini），`thoughts` 作为 thinking

## 性能

在 800 个会话（四个数据源各 200 个，其中 1/4 是 3000 行的大会话，共约 208 MB）的合成数据上实测（服务和客户端都在同一台 arm64 机器上）：

| 端点 | 优化前 | 优化后 |
|------|--------|--------|
| `GET /sessions`（列 800 条） | 1236 ms | 18 ms |
| `GET /sessions/<pattern>`（精确查一条） | 1207 ms | 5 ms |
| `GET /sessions/<pattern>/messages?limit=10`（大会话） | 1248 ms | 3 ms |
| `GET /sessions/<pattern>/final`（大会话） | 1266 ms | 30 ms |
| `GET /health` | 3 ms | 3 ms |

优化点：

1. **列表不再扫全文**——Gemini 的 `updatedAt` 原先要读完整个会话文件才能算出来，现在只读首行元数据（缺了才退回文件时间）。单这一项就让列表快了约 12 倍
2. **消息与最终结果改成流式读取**——`messages` 取够 `limit` 条就停（原先先把整个文件解析进内存），`final` 只保留最后一条助手消息
3. **会话列表短 TTL 缓存**（`--cache-ttl`，默认 2 秒）——查找会话与列表都走它，一次请求不必把 800 个会话文件重新 stat/读一遍。**消息与最终结果始终直接读文件**，缓存只影响「有哪些会话」这层元数据；`--cache-ttl 0` 可完全关掉缓存

边界与代价：

- 新建的会话最长 2 秒后才会出现在列表/查询里（可调小或设为 0，实测超过 TTL 立即可见）
- `final` 仍需顺序读完整个会话文件（要取最后一条助手消息），大会话约 30 ms——这是单文件顺序读的固有成本
- HTTP 层已改为 HTTP/1.1：客户端可以复用连接（响应都带 `Content-Length`）

## 安全

- API 能读到完整的会话内容（含工具输出），**对外暴露时务必设置 `--hook_token`**
- 建议放在 HTTPS 反向代理（Nginx/Caddy）之后，不要直接暴露到公网
- 令牌定期轮换；不要把它写进镜像或仓库
- 容器里挂载会话目录用 `:ro`（compose 默认已是只读）

## 故障排除

1. **某个数据源没被启用**（`/health` 的 `sources` 里没有它）
   - 先对照上表确认本机会话目录存在；`--mode all` 会明确打印缺了哪个
   - 容器里跑时确认对应目录挂进去了（compose 默认六个都挂；手动 `docker run` 要自己加 `-v`）
   - 镜像不预建会话目录，空的挂载目录不会被视为"有数据源"
2. **列表为空**
   - `/health` 看启用了哪些源；确认服务进程对该目录有读权限
3. **401 Unauthorized**
   - 请求头是否带了 `Authorization: Bearer <token>`，与启动时的 `--hook_token` 是否一致
4. **端口占用**
   - 换 `--port`，或检查 8080 是否被别的服务占用

## 开发

```
.
├── session_query_api.py    # 主程序（仅标准库；数据源适配器都在这个文件里）
├── Dockerfile              # 镜像构建
├── docker-compose.yml      # 六个源目录的挂载示例
├── LICENSE                 # MIT
└── README.md
```

新增一种数据源的做法：继承 `SessionSource`，实现 `list()` / `messages(record, limit)` / `final(record)`（以及可选的 `exists()`），再在 `build_sources()` 的 `factories` 里注册即可——多源合并、匹配排序、`source` 标记都是框架层统一处理的。

## 许可

MIT，见 [LICENSE](LICENSE)。

---

**注意**：本服务是只读的，不会改动任何 OpenClaw / Hermes / Pi / Claude Code / Codex / Gemini 的会话数据。
