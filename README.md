# agent-session-query

在本机上查询各种 Agent / CLI 的会话记录：**自带一个只读的 Web 页面，提供 HTTP API，也能当 MCP server 给 Agent 用**。不改动任何会话数据。

单二进制（无 cgo、无常驻运行时），`scp` 到任何同架构的机器上就能跑。唯一的外部依赖是纯 Go 的 SQLite 驱动（读 Hermes 的 `state.db`），交叉编译照旧。

六种数据源，存在哪几种就查哪几种，可同时合并查询：

| 数据源 | 会话位置 | 会话 ID |
|--------|----------|---------|
| Hermes | `~/.hermes/sessions/`（`sessions.json`）或 `~/.hermes/state.db`（新版全 SQLite） | `sessions.json` 里的 `session_id` / `sessions` 表的 `id` |
| OpenClaw | `~/.openclaw/agents/default/sessions/` | `sessions.json` 里的 `sessionId` |
| Pi | `~/.pi/agent/sessions/<项目>/*.jsonl` | 会话文件首行 `id` |
| Claude Code | `~/.claude/projects/<项目>/*.jsonl` | 文件名（uuid）/ `sessionId` 字段 |
| Codex | `~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl` | 首行 `payload.session_id` |
| Gemini CLI | `~/.gemini/tmp/<项目>/chats/session-*.jsonl` | 首行 `sessionId` |

表里的 `~` 按运行用户的 home 解析：Linux/macOS 是 `$HOME`，Windows 是 `%USERPROFILE%`（即 `C:\Users\<你>\.claude\projects` 这种）。各数据源的解析细节见 [docs/internals.md](docs/internals.md)。

## 快速开始

### 下载编译产物

不用装 Go：push 形如 `v0.1.0` 的 tag 会触发 [GitHub Actions](.github/workflows/release.yml) 自动交叉编译**六个平台**（`linux` / `darwin` / `windows` × `amd64` / `arm64`），产物挂在 Releases 页，解包即用。Unix 是 `.tar.gz`，Windows 是 `.zip`：

```bash
tar xzf agent-session-query-darwin-arm64.tar.gz
./agent-session-query --port 8080
```

```powershell
# Windows
Expand-Archive agent-session-query-windows-amd64.zip -DestinationPath .
.\agent-session-query.exe --port 8080
```

产物没有签名，系统会拦一下：macOS 用 `xattr -d com.apple.quarantine agent-session-query`；Windows 的 SmartScreen 弹「已保护你的电脑」时点「更多信息 → 仍要运行」。

### 本地构建

```bash
go build -o agent-session-query ./cmd/agent-session-query
./agent-session-query --port 8080                     # 自动检测：存在的数据源都启用
./agent-session-query --mode claude                    # 只看某一种
./agent-session-query --hook_token mysecrettoken       # 带认证
```

启动后打印本次启用的数据源，浏览器打开 `http://127.0.0.1:8080/ui` 就能看到页面。

加 `-d` 就丢到后台，父进程确认端口真的起来了才打印 PID 退出：

```bash
./agent-session-query -d --port 8080
# 已在后台启动
#   PID:   82575
#   地址:  http://127.0.0.1:8080
#   日志:  /tmp/agent-session-query-8080.log
#   停止:  kill 82575
```

`-d` 是临时后台跑，进程挂了不会自己起来；要真正常驻用下面的 systemd / 计划任务。

要常驻运行（Linux systemd / macOS launchd / Windows 计划任务 / Docker）见 **[docs/deploy.md](docs/deploy.md)**。

## Web 页面（`/ui`）

三栏：**会话列表 / 消息流 / 最终结果**。服务端设了 `--hook_token` 才会要令牌，令牌存在浏览器的
localStorage 里。

- **左栏**：搜索框打字是即时过滤元数据，按 <kbd>Enter</kbd> 是**全文搜正文**、结果带命中片段；
  可切「按时间 / 按项目」分组；正在被写入的会话带一个呼吸绿点
- **中栏**：消息时间线（text / thinking / toolCall / toolResult 分块）。可切「最早 / 最新 200 条」
  和「只看 user / assistant」
- **右栏**：最终结果常驻——stopReason、正文、思考过程、用量与费用、会话元信息
  （sessionId 一键复制、一键导出 Markdown）

10 秒自动刷新且不会掀掉正在读的内容（展开的折叠块、滚动位置都保住）；`/ui#<sessionId>` 可当深链；
亮 / 暗随系统；窄屏自动退成两栏 / 单栏。

快捷键：<kbd>j</kbd> <kbd>k</kbd> 切换会话 · <kbd>/</kbd> 聚焦搜索 · <kbd>Enter</kbd> 全文搜内容 ·
<kbd>g</kbd> <kbd>G</kbd> 跳消息流首 / 末 · <kbd>r</kbd> 刷新 · <kbd>Esc</kbd> 清空 / 退出搜索。

## 端点速查

所有查询都是 `GET`（MCP 的 `/mcp` 是 `POST`）。配置了 `--hook_token` 后，标「需要」的端点要带
`Authorization: Bearer <token>`。

| 端点 | 认证 | 说明 |
|------|------|------|
| `/` `/health` `/stats` | 免 | 服务信息 / 健康检查 |
| `/ui` `/favicon.ico` | 免 | 内嵌页面（不含数据） |
| `/sessions` | 需要 | 列出所有会话（多源合并，按更新时间倒序） |
| `/sessions/<pattern>` | 需要 | 单个会话；加 `/messages` `/final` `/export` |
| `/search?q=` | 需要 | **全文搜正文**，跨所有数据源 |
| `/projects` | 需要 | 按项目（cwd）归拢的统计 |
| `/mcp` | 需要 | MCP 的 Streamable HTTP 传输（`POST`） |

完整参数、`<pattern>` 匹配规则、响应字段见 **[docs/api.md](docs/api.md)**。

## MCP

`--mcp` 跑在 stdio 上，HTTP 模式下另有 `POST /mcp`。于是 **Agent 可以查自己的历史**——
让 Claude Code 去搜你上周用 Codex 解决过的同一个问题。五个工具：`search_sessions` /
`list_sessions` / `list_projects` / `get_session` / `get_messages`。

客户端配置与安全说明见 **[docs/mcp.md](docs/mcp.md)**。

## 配置

| 参数 | 默认 | 说明 |
|------|------|------|
| `--host` | `127.0.0.1` | 监听地址；对外暴露改 `0.0.0.0`（并配上 `--hook_token`） |
| `--port` | `8080` | 监听端口 |
| `--mode` | `auto` | `auto`（存在即启用）/ `all`（六个都启用）/ `hermes` / `openclaw` / `pi` / `claude` / `codex` / `gemini` |
| `--hook_token` | 无 | Bearer 令牌；不设置则免认证 |
| `--max-connections` | `50` | 最大并发连接数，超出的先排队 |
| `--accept-queue` | `0`（自动） | 满载时的排队位数；`0` = `2 × max-connections`，不低于 32。排满了立刻返回 503 |
| `--timeout` | `30` | 连接超时（秒） |
| `--cache-ttl` | `2` | 会话列表缓存秒数；`0` = 不缓存 |
| `--max-limit` | `1000` | `?limit=` 的上限，超出按上限截断 |
| `--cors-origin` | 无（关闭） | 允许的跨域来源；填 `*` 或具体 origin。不填则不发任何 CORS 头 |
| `-d` | 关 | 后台运行：脱离终端，输出写到日志文件 |
| `--log-file` | 按端口推导 | `-d` 时的日志路径，默认 `<临时目录>/agent-session-query-<端口>.log` |
| `--mcp` | 关 | 以 MCP server 跑在 stdio 上（见下），不监听端口 |
| `--version` | — | 打印版本后退出 |

Docker 环境变量：`HOOK_TOKEN`、`SESSION_MODE`（默认 `auto`）、`GO_IMAGE`（构建参数）。

不给 `--hook_token` 时会读环境变量 `HOOK_TOKEN`——**命令行参数会出现在 `ps` 里，环境变量不会**，常驻部署建议用后者。

## 安全

- 能读到完整会话内容（含工具输出），**对外暴露务必设置 `--hook_token`**
- 默认只绑 `127.0.0.1`、默认不发 CORS 头：不设 token 时，这两条是拦住「随便哪个网页 fetch 本机 `/sessions` 把会话读走」的唯一屏障，改之前想清楚
- 放在 HTTPS 反向代理（Nginx/Caddy）之后，不要直接暴露到公网；`/ui` 页面不含数据但请在反代层加认证
- `/ui` 带 `Content-Security-Policy`（脚本样式只许同源、不许内联、不许被 iframe 套），页面渲染一律走 `textContent`
- 令牌定期轮换；不要写进镜像或仓库；容器挂载会话目录用 `:ro`

## 故障排除

1. **某个数据源没被启用**（`/health` 的 `sources` 里没有它）：对照上面的表确认目录存在，`--mode all`
   会打印缺了哪个。Hermes 看 `sessions.json` **或** `state.db`，任一存在即启用。容器里确认目录挂进去了。
2. **列表为空**：`/health` 看启用了哪些源，确认进程对目录有读权限。
3. **401**：`Authorization: Bearer <token>` 与启动时的 `--hook_token` 是否一致。
4. **端口占用**：换 `--port`。

## 文档

| | |
|---|---|
| [docs/api.md](docs/api.md) | 完整 HTTP API：参数、匹配规则、响应字段 |
| [docs/mcp.md](docs/mcp.md) | MCP：两种传输、客户端配置、工具表 |
| [docs/deploy.md](docs/deploy.md) | 常驻部署：systemd / launchd / Windows 计划任务 / Docker |
| [docs/development.md](docs/development.md) | 目录结构、测试、发版、新增数据源 |
| [docs/internals.md](docs/internals.md) | 实现细节：各源解析、性能、全文搜索、跨平台 |

## 许可

MIT，见 [LICENSE](LICENSE)。

---

**注意**：本服务是只读的，不会改动任何 Hermes / OpenClaw / Pi / Claude Code / Codex / Gemini 的会话数据。
