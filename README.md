# 本地 Agent 会话查询 HTTP API

一个只读的 HTTP API，用来查询本机上各种 Agent / CLI 的会话记录：列表、消息、最终结果。不改动任何会话数据。

**Go 实现，无 cgo、无常驻运行时**——编译出来是一个静态二进制，不需要 venv，`scp` 到任何同架构的机器上就能跑。唯一的外部依赖是纯 Go 的 SQLite 驱动（`modernc.org/sqlite`，为了读 Hermes 的 `state.db`），它同样不引入 cgo，交叉编译照旧。

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
go build -o agent-session-query .

# 自动检测：存在的数据源都启用
./agent-session-query --port 8080

# 只看某一种
./agent-session-query --mode claude

# 强制全部启用（缺哪个会打印警告）
./agent-session-query --mode all

# 带认证
./agent-session-query --port 8080 --hook_token mysecrettoken
```

启动后会打印本次启用的数据源：

```
运行模式: auto
数据源 [pi]: /home/me/.pi/agent/sessions
数据源 [claude]: /home/me/.claude/projects
数据源 [codex]: /home/me/.codex/sessions
已启用认证（Bearer hook_token 已设置，不回显）
```

交叉编译（在别的机器上跑同一个二进制）：

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -o agent-session-query-arm64 .
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o agent-session-query-mac .
```

### Docker

```bash
docker build -t agent-session-query .          # 国内加 --build-arg GO_IMAGE=<镜像源>

docker run -d --name agent-session-query -p 8080:8080 \
  -v ~/.claude/projects:/root/.claude/projects:ro \
  -v ~/.codex/sessions:/root/.codex/sessions:ro \
  -v ~/.pi/agent/sessions:/root/.pi/agent/sessions:ro \
  -v ~/.gemini/tmp:/root/.gemini/tmp:ro \
  -v ~/.hermes/sessions:/root/.hermes/sessions:ro \
  -v ~/.openclaw/agents/default/sessions:/root/.openclaw/agents/default/sessions:ro \
  agent-session-query --hook_token mysecrettoken
```

镜像是两段构建：运行层用 `scratch`，里面只有两个静态二进制（服务本体 13.4 MB + 探活小程序 2 MB），镜像 **约 16 MB**，没有 shell、没有包管理器。数据源的路径由 `$HOME` 推导，镜像里 `HOME=/root`，所以挂载点都在 `/root/...` 下。挂几个目录就查几个源。

### Docker Compose

```bash
docker compose up -d                                              # 六个源目录都已挂载
HOOK_TOKEN=mysecrettoken SESSION_MODE=auto docker compose up -d   # 带认证
```

### 常驻部署（systemd user service）

单二进制，不需要 Docker。装到 `~/.local/bin`，配一个用户级 service：

```ini
# ~/.config/systemd/user/agent-session-query.service
[Unit]
Description=本地 Agent 会话查询 HTTP API（只读）
After=network.target

[Service]
Type=simple
ExecStart=%h/.local/bin/agent-session-query --host 127.0.0.1 --port 8787 --mode auto
EnvironmentFile=%h/.config/agent-session-query/env    # 里面一行 HOOK_TOKEN=...
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=true

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload && systemctl --user enable --now agent-session-query
sudo loginctl enable-linger "$USER"     # 不开的话，退出登录服务就停
journalctl --user -u agent-session-query -f
```

默认只监听 `127.0.0.1`：要给别人用，改成 `0.0.0.0` 并挂在反向代理后面，别裸奔到公网。

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

字段来源：OpenClaw/Hermes 取自 `sessions.json` 里的记录（OpenClaw 的 `updatedAt` 由毫秒时间戳格式化成 UTC）；其余四种用会话文件自身的元数据/修改时间。列表按 `updatedAt` 倒序，`source` 表示这条记录来自哪个数据源。

### `GET /sessions/<pattern>`

字段与列表中的同一条记录完全一致（包含 `file`）。找不到返回 `404` + `{"error": "Session not found"}`。

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
    }
  ],
  "total": 1
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
- 会话文件还不存在时，返回 `isFinal=false` + `error` 字段说明原因（HTTP 仍是 200）

### 错误响应

| 场景 | 状态码 | 响应体 |
|------|--------|--------|
| 未带 / 错误的 token | 401 | `{"error": "Unauthorized"}` |
| 找不到会话 | 404 | `{"error": "Session not found"}` |
| 未知路径 | 404 | `{"error": "Not found"}` |
| 服务内部异常 | 500 | `{"error": "Internal server error"}` |
| 并发超限 | 503 | 纯文本 `Service temporarily overloaded` |

## 配置

| 参数 | 默认 | 说明 |
|------|------|------|
| `--host` | `0.0.0.0` | 监听地址 |
| `--port` | `8080` | 监听端口 |
| `--mode` | `auto` | `auto`（存在即启用）/ `all`（六个都启用）/ `hermes` / `openclaw` / `pi` / `claude` / `codex` / `gemini` |
| `--hook_token` | 无 | Bearer 令牌；不设置则免认证 |
| `--max-connections` | `50` | 最大并发连接数，超出返回 503 |
| `--timeout` | `30` | 连接超时（秒） |
| `--cache-ttl` | `2` | 会话列表缓存秒数；`0` = 不缓存（每次都重新扫描） |

Docker 环境变量：`HOOK_TOKEN`（传给 `--hook_token`）、`SESSION_MODE`（传给 `--mode`，默认 `auto`）、`GO_IMAGE`（构建参数，基础镜像）。

不给 `--hook_token` 时会读环境变量 `HOOK_TOKEN`——**命令行参数会出现在 `ps` 里，环境变量不会**，常驻部署建议用后者。

## 各数据源的解析细节

- **Hermes**：`sessions.json` 是 `key → 记录` 的映射，会话内容在同目录的 `<session_id>.jsonl`；消息取 `role` 为 `user`/`assistant` 的行，`content` 是字符串，推理在 `reasoning`。**webhook 会话可能只有 SQLite 记录**：没有 jsonl 时，`final` 回退到 `~/.hermes/state.db`（只读打开，取最后一条 `active=1` 且 `finish_reason=stop` 的助手消息，`message_count` 缺失时回退成实际条数）
- **OpenClaw**：同上结构，字段名是 `sessionId`/`stopReason`；消息取 `type=message` 的行，`message.content` 是块数组（`text`/`thinking`/`toolCall`/`toolResult`），`stopReason` 可能在 `message` 里也可能在行顶层
- **Pi**：行类型有 `session`（首行元数据）、`model_change`、`message`；消息取 `message` 行（`message.role` + `message.content` 块数组）
- **Claude Code**：消息行是 `type=user`/`assistant`，内容在 `message.content`（`text`/`thinking`/`tool_use`/`tool_result` 块）；跳过 `isSidechain`（子代理）以及 `queue-operation`、`attachment`、`mode` 等非对话行
- **Codex**：消息行是 `type=response_item` 且 `payload.type=message`；`payload.role` 为 `developer` 的行（系统拼装的指令）不计入；用量取自 `token_usage_record`
- **Gemini CLI**：文件是追加日志——首行元数据（`sessionId`/`startTime`）、`{"$set": {...}}` 补丁行、以及消息行；消息取 `type=user`/`type=gemini` 的行，`content` 可能是数组（user）或字符串（gemini），`thoughts` 作为 thinking

## 与 Python 版的差异

这一版是从 Go 重写的。**升级方式**：拉下来 `go build` 即可，命令行参数、端点、响应结构都没有变化——原来用 `python3 session_query_api.py ...` 启的，换成 `./agent-session-query ...` 就行。原 Python 单文件实现保留在 git 历史里（最后的 Python 版本是 `c7da626`，`git show c7da626:session_query_api.py` 可取回）。

两者做了逐请求对拍验证：**60 条**固定用例（六个源的构造数据，覆盖匹配优先级、编码、limit 边界、错误分支，以及 Hermes `state.db` 回退）+ **33 项**真实数据（本机 11 个 Pi 会话、8 个 Claude Code 会话、6 个 Codex、3 个 Gemini 的列表/详情/消息/结果/按文件名查找），响应**逐字段一致**。

已知的行为差异只有这些：

1. **`HEAD` 请求**：Python 版返回 501；这一版按 `GET` 处理（响应无 body）。对探活工具更友好。
2. **503 的响应体**：都是 503，但 Go 的 HTTP 状态原因短语是标准的 `Service Unavailable`。
3. **`/stats` 的 `bad_requests`**：HTTP 协议层的畸形请求由 Go 的 `net/http` 自行处理并直接断开，不会逐条进这个计数；这一版统计的是服务端记录的协议层错误。其余三个计数字段（max/active/total connections）语义不变。
4. 启动横幅与请求日志的格式略有不同（内容等价）。
5. **Hermes `state.db` 的查询少查了一列**（这一版更宽）。Python 版的 SQL 里 `SELECT` 了 `token_count` 却从未使用它——要是哪个版本的 Hermes 库没有这一列，它会直接抛 `no such column`，整个回退静默失效（只留一行 WARN，接口返回「会话文件不存在」）。这一版只查真正用得到的列，遇到这种库仍然能给出结果。

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
   - 数据源路径是 `$HOME` 推导的：容器里是 `/root/...`，`sudo` 跑时可能是 `/root`、普通用户是 `/home/<你>`
2. **列表为空**
   - `/health` 看启用了哪些源；确认服务进程对该目录有读权限
3. **401 Unauthorized**
   - 请求头是否带了 `Authorization: Bearer <token>`，与启动时的 `--hook_token` 是否一致
4. **端口占用**
   - 换 `--port`，或检查 8080 是否被别的服务占用

## 开发

```
.
├── main.go               # 参数解析、启动、优雅退出
├── http.go               # 路由、认证、连接限制、统计
├── api.go                # 查询层（多源合并、匹配、TTL 缓存）
├── record.go             # 统一的会话记录结构与工具函数
├── source.go             # 数据源接口与装配
├── source_jsonmap.go     # Hermes / OpenClaw
├── hermes_sqlite.go      # Hermes state.db 回退（modernc.org/sqlite）
├── source_pi.go          # Pi
├── source_claude.go      # Claude Code
├── source_codex.go       # Codex
├── source_gemini.go      # Gemini CLI
├── session_query_test.go # 单元测试（解析、匹配、HTTP 路由）
├── hermes_sqlite_test.go # 单元测试（state.db 回退）
├── go.mod / go.sum       # 唯一依赖：纯 Go 的 SQLite 驱动
├── cmd/healthcheck/      # 容器探活用的小程序
├── Dockerfile            # 两段构建 → scratch（约 16 MB）
└── docker-compose.yml    # 六个源目录的挂载示例
```

```bash
go test ./...            # 全部单测（不碰网络、不依赖本机装了什么）
gofmt -l .               # 格式检查
go vet ./...
```

新增一种数据源的做法：实现 `SessionSource` 接口（`Mode` / `Location` / `Exists` / `List` / `Messages` / `Final`），在 `buildSources()` 的 `factories` 里注册，再把模式名加进 `knownModes`——多源合并、匹配排序、`source` 标记都是框架层统一处理的。

## 许可

MIT，见 [LICENSE](LICENSE)。

---

**注意**：本服务是只读的，不会改动任何 Hermes / OpenClaw / Pi / Claude Code / Codex / Gemini 的会话数据。
