# 本地 Agent 会话查询 HTTP API

一个只读的 HTTP API，用来查询本机上各种 Agent / CLI 的会话记录：列表、消息、最终结果。不改动任何会话数据。

**Go 实现，无 cgo、无常驻运行时**——编译出来是一个静态二进制（约 15 MB），`scp` 到任何同架构的机器上就能跑。唯一的外部依赖是纯 Go 的 SQLite 驱动（`modernc.org/sqlite`，读 Hermes 的 `state.db`），交叉编译照旧。

**一个接口，六种数据源**——存在哪几种就查哪几种，可以同时合并查询：

| 数据源 | 会话位置 | 会话 ID |
|--------|----------|---------|
| Hermes | `~/.hermes/sessions/`（`sessions.json`）或 `~/.hermes/state.db`（新版全 SQLite） | `sessions.json` 里的 `session_id` / `sessions` 表的 `id` |
| OpenClaw | `~/.openclaw/agents/default/sessions/` | `sessions.json` 里的 `sessionId` |
| Pi | `~/.pi/agent/sessions/<项目>/*.jsonl` | 会话文件首行 `id` |
| Claude Code | `~/.claude/projects/<项目>/*.jsonl` | 文件名（uuid）/ `sessionId` 字段 |
| Codex | `~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl` | 首行 `payload.session_id` |
| Gemini CLI | `~/.gemini/tmp/<项目>/chats/session-*.jsonl` | 首行 `sessionId` |

浏览器界面：`http://127.0.0.1:8787/ui`。各数据源的解析细节、与 Python 版的对拍结论、性能数据见 [docs/internals.md](docs/internals.md)。

## 快速开始

### 下载编译产物

不用装 Go：push 形如 `v0.1.0` 的 tag 会触发 [GitHub Actions](.github/workflows/release.yml) 自动交叉编译四个平台（`linux` / `darwin` × `amd64` / `arm64`），产物挂在 Releases 页，解包即用（macOS 被 Gatekeeper 拦时 `xattr -d com.apple.quarantine agent-session-query`）：

```bash
tar xzf agent-session-query-darwin-arm64.tar.gz
./agent-session-query --port 8080
```

### 本地构建

```bash
go build -o agent-session-query ./cmd/agent-session-query
./agent-session-query --port 8080                     # 自动检测：存在的数据源都启用
./agent-session-query --mode claude                    # 只看某一种
./agent-session-query --hook_token mysecrettoken       # 带认证
```

启动后打印本次启用的数据源（浏览器里看的话，直接打开 `http://127.0.0.1:8787/ui`）。交叉编译：`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o agent-session-query ./cmd/agent-session-query`。

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
```

默认只监听 `127.0.0.1`：要给别人用，改成 `0.0.0.0` 并挂在反向代理后面，别裸奔到公网。

### Docker（可选）

单二进制已经够省事，Docker 不再是推荐方式，仓库保留 `Dockerfile` / `docker-compose.yml` 备用：

```bash
HOOK_TOKEN=mysecrettoken docker compose up -d   # 六个源目录的挂载配置见 docker-compose.yml
```

容器里数据源路径由 `$HOME` 推导（挂载点都在 `/root/...` 下）；Hermes 记得挂整个 `~/.hermes`（`state.db` 及其 `-wal`/`-shm` 都要跟着）。

## 页面（`/ui`）

浏览器里直接看：`http://127.0.0.1:8787/ui`。第一次打开会要 `hook_token`，之后存在这个浏览器的 localStorage 里。

- **左侧**：会话列表，数据源过滤 + 搜索框（匹配 sessionId / 文件名 / 路径 / cwd），按更新时间倒序；数据源彩色标签，相对时间（悬浮看完整时间）
- **右侧**：会话信息、**最终结果**卡片（isFinal / stopReason / 用量）、消息时间线（text / thinking / toolCall / toolResult 分块；超过 600 字符的块折起来）
- 10 秒自动刷新（页面后台时不打接口）；`/ui#<sessionId>` 可直接当深链贴给别人；亮 / 暗随系统切换

实现决定：页面用 `go:embed` 打进二进制（单文件分发，无 npm）；**渲染一律走 `textContent`**——消息里全是第三方文本，拼 HTML 就是存储型 XSS，这条规则有测试钉着（`ui_test.go`）；页面本身免认证（不含数据），数据仍需 `Authorization` 头，对外暴露请在反向代理上加一层认证。

## API

所有端点都是 `GET`。`/sessions` 系列额外支持 `/api/sessions` 前缀写法；`/`、`/health`、`/stats` 没有别名。

| 端点 | 认证 | 说明 |
|------|------|------|
| `/` `/health` `/stats` | 免 | 服务信息 / 健康检查（含连接统计） |
| `/sessions` | 需要 | 列出所有会话（多源合并，按更新时间倒序） |
| `/sessions/<pattern>` | 需要 | 单个会话的信息 |
| `/sessions/<pattern>/messages?limit=50` | 需要 | 会话消息（取最早的前 N 条） |
| `/sessions/<pattern>/final` | 需要 | 会话的最终结果 |

**认证**：配置了 `--hook_token` 后，标「需要」的端点要带 `Authorization: Bearer <token>`；未配置时全部免认证（启动时打印警告），令牌比较用常量时间比较。

**过载保护**：并发连接超过 `--max-connections`（默认 50）返回 503；每个连接有 `--timeout`（默认 30 秒）超时。

**`<pattern>` 匹配规则**（按序先命中先返回，所有数据源一起参与每一轮，模糊命中不会盖掉其它源的精确命中）：

1. 精确等于 `sessionId`
2. 精确等于 `key`（完整路径或完整 key）
3. `key` 以 `:<pattern>` 或 `/<pattern>` 结尾
4. `pattern` 是 `key` 的子串
5. `pattern` 是 `sessionId` 的子串

`Session: ` 和 `Run: ` 前缀会被自动去掉；含冒号的 pattern 记得 URL 编码（`%3A`）。

```bash
/sessions/e4b2b405-88ea-4782-a84c-92574380ed16   # Claude Code：完整 uuid
/sessions/ee64d13e                              # 片段也行
/sessions/rollout-2026-09-13T23-07-05           # Codex：rollout 文件名片段
/sessions/hook:alert:prometheus:b5123b01-...    # OpenClaw：完整 key 或后半段
```

### 响应结构

`GET /sessions` 返回 `{"sessions": [...], "total": n}`，每条记录：

```json
{
  "source": "claude", "key": "/root/.claude/projects/.../e4b2b405-....jsonl",
  "shortKey": "e4b2b405-...", "sessionId": "e4b2b405-...",
  "file": "...", "hasFile": true, "status": "done",
  "cwd": "/home/me/proj", "updatedAt": "2026-09-14T07:41:48"
}
```

各源共有：`source` / `key` / `shortKey` / `sessionId` / `file` / `hasFile` / `status` / `updatedAt`；按源额外：OpenClaw 加 `model` `runtimeMs` `totalTokens`，Hermes 加 `createdAt` `displayName` `platform` `totalTokens` `estimatedCostUsd`（SQLite 记录另带 `model`），Pi / Claude Code 加 `cwd`，Codex 加 `cwd` `cliVersion`，Gemini 加 `project`。

`GET /sessions/<pattern>` 与列表中该条记录完全一致（找不到返回 404 + `{"error": "Session not found"}`）。

`GET /sessions/<pattern>/messages?limit=50` 返回 `{"messages": [...], "total": n}`，`content` 是块数组，块类型 `text` / `thinking` / `toolCall`（`name`+`arguments`）/ `toolResult`（`toolName`+`content`）；会话文件不存在时返回空数组（不是 404）。

`GET /sessions/<pattern>/final`：

```json
{
  "status": "done", "isFinal": true, "isProcessing": false,
  "messageCount": 4, "source": "codex", "id": "msg_05c9...",
  "timestamp": "2026-09-13T15:08:57.823Z", "stopReason": "",
  "model": "gpt-5.6-luna", "text": "已轮询并回复该消息：……",
  "thinking": "", "toolCalls": [], "usage": {"input_tokens": 18034, "output_tokens": 104}
}
```

取值规则：OpenClaw / Hermes 取第一个 `stopReason`/`finish_reason` 为 `stop` 的助手消息；其余四种取最后一条助手消息，`isFinal` 按各自状态判定。会话文件不存在时返回 `isFinal=false` + `error` 字段（HTTP 仍 200）。

**错误响应**：401 `{"error": "Unauthorized"}`（token 缺失/错误）、404 `{"error": "Session not found"}` / `{"error": "Not found"}`、500 `{"error": "Internal server error"}`、503 纯文本 `Service temporarily overloaded`（并发超限）。

## 配置

| 参数 | 默认 | 说明 |
|------|------|------|
| `--host` | `0.0.0.0` | 监听地址 |
| `--port` | `8080` | 监听端口 |
| `--mode` | `auto` | `auto`（存在即启用）/ `all`（六个都启用）/ `hermes` / `openclaw` / `pi` / `claude` / `codex` / `gemini` |
| `--hook_token` | 无 | Bearer 令牌；不设置则免认证 |
| `--max-connections` | `50` | 最大并发连接数，超出返回 503 |
| `--timeout` | `30` | 连接超时（秒） |
| `--cache-ttl` | `2` | 会话列表缓存秒数；`0` = 不缓存 |

Docker 环境变量：`HOOK_TOKEN`、`SESSION_MODE`（默认 `auto`）、`GO_IMAGE`（构建参数）。

不给 `--hook_token` 时会读环境变量 `HOOK_TOKEN`——**命令行参数会出现在 `ps` 里，环境变量不会**，常驻部署建议用后者。

## 安全

- API 能读到完整会话内容（含工具输出），**对外暴露务必设置 `--hook_token`**
- 放在 HTTPS 反向代理（Nginx/Caddy）之后，不要直接暴露到公网；`/ui` 页面不含数据但请在反代层加认证
- 令牌定期轮换；不要写进镜像或仓库；容器挂载会话目录用 `:ro`

## 故障排除

1. **某个数据源没被启用**（`/health` 的 `sources` 里没有它）：对照上表确认目录存在；`--mode all` 会打印缺了哪个。Hermes 看 `sessions.json` **或** `state.db`，任一存在即启用。容器里确认目录挂进去了（挂载配置见 `docker-compose.yml`）；数据源路径由 `$HOME` 推导，容器里是 `/root/...`，`sudo` 跑时可能是 `/root`。
2. **列表为空**：`/health` 看启用了哪些源，确认进程对目录有读权限。
3. **401**：请求头 `Authorization: Bearer <token>` 与启动时的 `--hook_token` 是否一致。
4. **端口占用**：换 `--port`。

## 开发

```
.
├── cmd/agent-session-query/   # 入口（实现在 internal/app）
├── cmd/healthcheck/           # 容器探活小程序
├── internal/app/              # 全部实现：run.go（参数与启动）、http.go（路由/认证/限流）、
│                              #   api.go（多源合并/匹配/缓存）、record.go、source*.go（各数据源）、
│                              #   hermes_sqlite.go、ui.go + ui/（go:embed 单页）
│                              #   测试与源文件一一对应（source_*_test.go 等）
├── docs/internals.md          # 解析细节 / Python 对拍 / 性能
├── .github/workflows/release.yml  # 打 tag 自动交叉编译 + 发 Release
├── Dockerfile / docker-compose.yml
└── go.mod / go.sum            # 唯一依赖：纯 Go 的 SQLite 驱动
```

```bash
go test ./...     # 全部单测（不碰网络、不依赖本机装了什么）
gofmt -l .        # 格式检查
go vet ./...
```

新增一种数据源：实现 `SessionSource` 接口（`Mode` / `Location` / `Exists` / `List` / `Messages` / `Final`），在 `buildSources()` 的 `factories` 里注册，再把模式名加进 `knownModes`——多源合并、匹配排序、`source` 标记都是框架层统一处理的。

**从 Python 版升级**：原来用 `python3 session_query_api.py ...` 启的，换成 `./agent-session-query ...` 即可，命令行参数、端点、响应结构都没变。差异明细见 [docs/internals.md](docs/internals.md)。

## 许可

MIT，见 [LICENSE](LICENSE)。

---

**注意**：本服务是只读的，不会改动任何 Hermes / OpenClaw / Pi / Claude Code / Codex / Gemini 的会话数据。
