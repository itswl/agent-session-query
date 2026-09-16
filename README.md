# agent-session-query

在本机上查询各种 Agent / CLI 的会话记录：**自带一个只读的 Web 页面，也提供 HTTP API**。不改动任何会话数据。

单二进制（约 15 MB、无 cgo、无常驻运行时），`scp` 到任何同架构的机器上就能跑。唯一的外部依赖是纯 Go 的 SQLite 驱动（读 Hermes 的 `state.db`），交叉编译照旧。

六种数据源，存在哪几种就查哪几种，可同时合并查询：

| 数据源 | 会话位置 | 会话 ID |
|--------|----------|---------|
| Hermes | `~/.hermes/sessions/`（`sessions.json`）或 `~/.hermes/state.db`（新版全 SQLite） | `sessions.json` 里的 `session_id` / `sessions` 表的 `id` |
| OpenClaw | `~/.openclaw/agents/default/sessions/` | `sessions.json` 里的 `sessionId` |
| Pi | `~/.pi/agent/sessions/<项目>/*.jsonl` | 会话文件首行 `id` |
| Claude Code | `~/.claude/projects/<项目>/*.jsonl` | 文件名（uuid）/ `sessionId` 字段 |
| Codex | `~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl` | 首行 `payload.session_id` |
| Gemini CLI | `~/.gemini/tmp/<项目>/chats/session-*.jsonl` | 首行 `sessionId` |

各数据源的解析细节与性能说明见 [docs/internals.md](docs/internals.md)。

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

启动后打印本次启用的数据源，浏览器打开 `http://127.0.0.1:8080/ui` 就能看到页面。

### 常驻部署（systemd user service）

单二进制，不需要 Docker。装到 `~/.local/bin`，配一个用户级 service：

```ini
# ~/.config/systemd/user/agent-session-query.service
[Unit]
Description=本地 Agent 会话查询（只读）
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

单二进制已经够省事，Docker 不是推荐方式，仓库保留 `Dockerfile` / `docker-compose.yml` 备用：

```bash
HOOK_TOKEN=mysecrettoken docker compose up -d   # 六个源目录的挂载配置见 docker-compose.yml
```

容器里数据源路径由 `$HOME` 推导（挂载点都在 `/root/...` 下）；Hermes 记得挂整个 `~/.hermes`（`state.db` 及其 `-wal`/`-shm` 都要跟着）。

## Web 页面（`/ui`）

第一次打开会要 `hook_token`，之后存在这个浏览器的 localStorage 里。

- **左侧**：会话列表，数据源过滤 + 搜索框（匹配 sessionId / 文件名 / 路径 / cwd），按更新时间倒序；数据源彩色标签，相对时间（悬浮看完整时间）
- **右侧**：会话信息、**最终结果**卡片（isFinal / stopReason / 用量）、消息时间线（text / thinking / toolCall / toolResult 分块；超过 600 字符的块折起来）
- 10 秒自动刷新（页面后台时不打接口）；`/ui#<sessionId>` 可直接当深链贴给别人；亮 / 暗随系统切换

实现决定：页面用 `go:embed` 打进二进制（单文件分发，无 npm）；**渲染一律走 `textContent`**——消息里全是第三方文本，拼 HTML 就是存储型 XSS，这条规则有测试钉着（`ui_test.go`）；页面本身免认证（不含数据），数据仍需 `Authorization` 头，对外暴露请在反向代理上加一层认证。

## HTTP API

所有端点都是 `GET`。`/sessions` 系列额外支持 `/api/sessions` 前缀写法；`/`、`/health`、`/stats` 没有别名。

| 端点 | 认证 | 说明 |
|------|------|------|
| `/` `/health` `/stats` | 免 | 服务信息 / 健康检查（含连接统计） |
| `/sessions` | 需要 | 列出所有会话（多源合并，按更新时间倒序） |
| `/sessions/<pattern>` | 需要 | 单个会话的信息 |
| `/sessions/<pattern>/messages?limit=50` | 需要 | 会话消息（取最早的前 N 条） |
| `/sessions/<pattern>/final` | 需要 | 会话的最终结果 |

**认证**：配置了 `--hook_token` 后，标「需要」的端点要带 `Authorization: Bearer <token>`；未配置时全部免认证（启动时打印警告），令牌比较用常量时间比较。**过载保护**：并发超过 `--max-connections`（默认 50）返回 503，每个连接 `--timeout`（默认 30 秒）超时。

**`<pattern>` 匹配规则**（按序先命中先返回，所有数据源一起参与每一轮，模糊命中不会盖掉其它源的精确命中）：① 精确 `sessionId` → ② 精确 `key` → ③ `key` 以 `:<pattern>` 或 `/<pattern>` 结尾 → ④ `key` 子串 → ⑤ `sessionId` 子串。`Session: ` / `Run: ` 前缀自动剥掉；含冒号的 pattern 记得 URL 编码（`%3A`）。

```bash
/sessions/e4b2b405-88ea-4782-a84c-92574380ed16   # Claude Code：完整 uuid，片段也行
/sessions/rollout-2026-09-13T23-07-05           # Codex：rollout 文件名片段
/sessions/hook:alert:prometheus:b5123b01-...    # OpenClaw：完整 key 或后半段
```

**响应**：

- 列表 / 单条：`source`、`key`、`shortKey`、`sessionId`、`file`、`hasFile`、`status`、`updatedAt`，按源附加 `cwd` / `model` / `totalTokens` / `estimatedCostUsd` / `cliVersion` / `project` 等
- 消息：`content` 是块数组，块类型 `text` / `thinking` / `toolCall`（`name`+`arguments`）/ `toolResult`（`toolName`+`content`）
- final：`isFinal` / `stopReason` / `text` / `thinking` / `toolCalls` / `usage` / `messageCount`；文件不存在时 `isFinal=false` + `error` 字段（HTTP 仍 200）
- 错误：401 / 404 / 500 返回 `{"error": "..."}`，503 并发超限为纯文本

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

- 能读到完整会话内容（含工具输出），**对外暴露务必设置 `--hook_token`**
- 放在 HTTPS 反向代理（Nginx/Caddy）之后，不要直接暴露到公网；`/ui` 页面不含数据但请在反代层加认证
- 令牌定期轮换；不要写进镜像或仓库；容器挂载会话目录用 `:ro`

## 故障排除

1. **某个数据源没被启用**（`/health` 的 `sources` 里没有它）：对照上表确认目录存在；`--mode all` 会打印缺了哪个。Hermes 看 `sessions.json` **或** `state.db`，任一存在即启用。容器里确认目录挂进去了；数据源路径由 `$HOME` 推导，容器里是 `/root/...`，`sudo` 跑时可能是 `/root`。
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
├── docs/internals.md          # 解析细节 / 性能
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

## 许可

MIT，见 [LICENSE](LICENSE)。

---

**注意**：本服务是只读的，不会改动任何 Hermes / OpenClaw / Pi / Claude Code / Codex / Gemini 的会话数据。
