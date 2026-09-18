# agent-session-query

[English](README.md) · **简体中文**

查询本机各个 agent CLI 留下的会话记录：**一个只读网页、一套 HTTP API、以及一个供 agent 调用的
MCP server**。它不会修改任何会话数据。

单个二进制（不依赖 cgo，没有常驻运行时）—— `scp` 到任意同架构的机器上直接跑。唯一的外部依赖是
一个纯 Go 的 SQLite 驱动（用于读取 Hermes 的 `state.db`），所以交叉编译照常可用。

支持七个数据源；存在哪个就查哪个，也可以在一次查询里合并：

| 数据源 | 会话存放位置 | 会话 ID |
|--------|--------------|---------|
| Hermes | `~/.hermes/sessions/`（`sessions.json`）或 `~/.hermes/state.db`（较新版本，全 SQLite） | `sessions.json` 里的 `session_id` / `sessions` 表里的 `id` |
| OpenClaw | `~/.openclaw/agents/<agent>/agent/openclaw-agent.sqlite`（SQLite，2026.9+；旧版 `sessions.json` 布局仍可读） | `session_windows` 表的 `session_id` |
| Pi | `~/.pi/agent/sessions/<project>/*.jsonl` | session 行上的 `id` |
| Claude Code | `~/.claude/projects/<project>/*.jsonl` | 文件名（一个 uuid）/ `sessionId` 字段 |
| Codex | `~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl` | 元数据行上的 `payload.session_id` |
| Gemini CLI | `~/.gemini/tmp/<project>/chats/session-*.jsonl` | 首行的 `sessionId` |
| OpenCode | `${XDG_DATA_HOME:-~/.local/share}/opencode/opencode.db`（SQLite；Windows 为 `%LOCALAPPDATA%\opencode`） | `session` 表的 `id` |

表中的 `~` 指向运行用户的家目录：Linux 和 macOS 上是 `$HOME`，Windows 上是 `%USERPROFILE%`
（也就是 `C:\Users\<you>\.claude\projects` 之类）。各数据源的解析细节见
[docs/internals.md](docs/internals.md)。

## 快速开始

### 下载构建产物

不需要 Go 工具链。推送一个形如 `v0.1.0` 的 tag 会触发
[GitHub Actions](.github/workflows/release.yml)，交叉编译 **六个平台**
（`linux` / `darwin` / `windows` × `amd64` / `arm64`）并把产物挂到 Releases 页面。解压即用。
Unix 是 `.tar.gz`，Windows 是 `.zip`：

```bash
tar xzf agent-session-query-darwin-arm64.tar.gz
./agent-session-query --port 8080
```

```powershell
# Windows
Expand-Archive agent-session-query-windows-amd64.zip -DestinationPath .
.\agent-session-query.exe --port 8080
```

产物没有签名，所以系统会拦你一次：macOS 上执行
`xattr -d com.apple.quarantine agent-session-query`；Windows 上 SmartScreen 提示
"Windows 已保护你的电脑"时，选"更多信息 → 仍要运行"。

### 本地构建

```bash
go build -o agent-session-query ./cmd/agent-session-query
./agent-session-query --port 8080                     # 自动探测：存在哪个就启用哪个
./agent-session-query --mode claude                    # 只启用单个数据源
./agent-session-query --hook_token mysecrettoken       # 带鉴权
```

启动时会打印启用了哪些数据源，然后用浏览器打开 `http://127.0.0.1:8080/ui`。

加 `-d` 可以让它转入后台；父进程会先确认端口真的起来了，再打印 PID 并退出：

```bash
./agent-session-query -d --port 8080
# Started in the background
#   PID:     82575
#   Address: http://127.0.0.1:8080
#   Log:     /tmp/agent-session-query-8080.log
#   Stop:    kill 82575
```

`-d` 只适合临时跑在后台——进程挂了没人拉起来。要做成有监管的常驻服务
（Linux systemd / macOS launchd / Windows 计划任务 / Docker），见
**[docs/deploy.md](docs/deploy.md)**。

## 网页（`/ui`）

三栏：**会话列表 / 消息流 / 最终结果**。只有服务端带 `--hook_token` 启动时才会要求填 token，
填完存在浏览器的 localStorage 里。

- **左栏**：每个会话显示一个可读的名字——opencode 用它自带的会话标题，其他源取首条真实
  用户消息——而不是一串 sessionId。输入会即时过滤 sessionId / 路径 / cwd，同时这些击键也会在服务端**搜索消息正文**。
  正文命中以「N more in message bodies」分隔线附在元数据结果下方，带上命中片段——一次查询
  同时覆盖两边，不用切模式。<kbd>Enter</kbd> 只是免去等待。列表可以按时间或按项目分组，
  正在写入的会话会有一个呼吸的绿点；项目分组标题可点击折叠。行上显示该会话的消息数——文件源会在
  首次列出时于后台统计，所以数字是页面出来之后稍晚一点出现，而不是让列表请求去等它
- **中栏**：消息时间线（text / thinking / toolCall / toolResult 各类块）。默认打开最新 200 条，
  **向上滚到顶会自动加载上一页**（从最早端阅读时则是滚到底加载下一页），头部的计数会告诉你已经
  读到会话的哪个位置。也可以直接切换首尾，或只看 **user**（人的话）、**assistant**（模型的话）
  或 **tools**（工具调用与结果）——三者是互不重叠的类别，而不是按角色字段判定，所以工具往来不会再混进发言者里。
  工具块按类别着色（执行 / 读取 / 写入 / 搜索 / 网络 / 委派）
- **右栏**：最终结果常驻——stopReason、答案、思考过程、用量与成本、会话元数据
  （一键复制 sessionId，一键导出 Markdown），以及**对话目录**：用户的每一轮提问按序号排列，
  点击即可跳到消息流中的对应位置。目录覆盖当前加载的窗口，会话更长时会明确标注

手机上下滑时标题栏和切换栏会自动收起，上滑或回到顶部时回来；三栏变成一次一屏，用
**Sessions / Conversation / Details** 切换——搜索框和列表过滤器
也归到 Sessions 栏里（它们对会话和详情没有任何作用），所以 Conversation 栏从标题行直接进入
内容；点列表里的会话会直接跳过去。页面每 10 秒刷新一次，且不会打断你
正在看的内容（展开的块和滚动位置都会保留）；你的视图设置（分组方式、源过滤、排序、过滤条件、
折叠的分组）刷新后仍然保留。`/ui#<sessionId>` 可作为深链接，明暗主题跟随系统。

左右两栏可以用头部的 ‹ / › 按钮折起来，把整个宽度让给会话内容；折叠状态会被记住。

快捷键：<kbd>j</kbd> <kbd>k</kbd> 切换会话（元数据命中与正文命中一并遍历）·
<kbd>/</kbd> 聚焦搜索框 · <kbd>Enter</kbd> 立刻搜正文而不等待 ·
<kbd>g</kbd> <kbd>G</kbd> 跳到消息流首尾 · <kbd>r</kbd> 刷新 · <kbd>Esc</kbd> 清空搜索。

## 接口一览

所有查询都是 `GET`（MCP 的 `/mcp` 是 `POST`）。一旦设置了 `--hook_token`，标注"是"的接口就需要
`Authorization: Bearer <token>`。

| 接口 | 鉴权 | 作用 |
|------|------|------|
| `/` `/health` `/stats` | 否 | 服务信息与健康检查 |
| `/ui` `/favicon.ico` | 否 | 内嵌页面（本身不含任何数据） |
| `/sessions` | 是 | 列出全部会话（跨数据源合并，最新在前） |
| `/sessions/<pattern>` | 是 | 单个会话；可接 `/messages`（支持 `order` / `limit` / `at`）、`/final` 或 `/export` |
| `/search?q=` | 是 | 跨全部数据源的**全文搜索** |
| `/projects` | 是 | 按项目（cwd）分组的会话数 |
| `/mcp` | 是 | MCP 的 Streamable HTTP 传输（`POST`） |

完整参数、`<pattern>` 的匹配规则和每个响应字段见 **[docs/api.md](docs/api.md)**。

## MCP

`--mcp` 走 stdio，HTTP 模式下另有 `POST /mcp`。这让 **agent 能查询自己的历史**——Claude Code
可以去翻你上周用 Codex 解决过的同一个问题。五个工具：`search_sessions` / `list_sessions` /
`list_projects` / `get_session` / `get_messages`。

客户端配置和安全注意事项见 **[docs/mcp.md](docs/mcp.md)**。

## 配置

| 参数 | 默认值 | 作用 |
|------|--------|------|
| `--host` | `127.0.0.1` | 绑定地址；对外暴露用 `0.0.0.0`（同时务必设 `--hook_token`） |
| `--port` | `8080` | 监听端口 |
| `--mode` | `auto` | `auto`（存在哪个启用哪个）/ `all`（七个全启用）/ `hermes` / `openclaw` / `pi` / `claude` / `codex` / `gemini` / `opencode` |
| `--hook_token` | 无 | Bearer token；不设则 API 不鉴权 |
| `--max-connections` | `50` | 最大并发连接数，超出的排队 |
| `--accept-queue` | `0`（自动） | 满载时的排队位；`0` 表示 `2 × max-connections`，且不低于 32。队列满则立即返回 503 |
| `--timeout` | `30` | 连接超时（秒） |
| `--cache-ttl` | `2` | 会话列表的缓存秒数；`0` 关闭缓存 |
| `--max-limit` | `1000` | `?limit=` 的上限，超出则截断 |
| `--cors-origin` | 无（关闭） | 允许的 CORS 来源；`*` 或具体来源。不设则完全不发 CORS 头 |
| `-d` | 关闭 | 后台运行，脱离终端，日志写入文件 |
| `--log-file` | 由端口推导 | `-d` 模式的日志路径，默认 `<tmp>/agent-session-query-<port>.log` |
| `--mcp` | 关闭 | 以 MCP server 身份跑在 stdio 上（见上），不监听端口 |
| `--version` | — | 打印版本并退出 |

Docker 环境变量：`HOOK_TOKEN`、`SESSION_MODE`（默认 `auto`），以及 `GO_IMAGE`（构建参数）。

不带 `--hook_token` 时会转而读取 `HOOK_TOKEN` 环境变量——**命令行参数会出现在 `ps` 里，环境变量
不会**，所以长期运行的场景优先用后者。

## 安全

- 它能读到完整的会话内容，包括工具输出，所以**对外暴露前一定要设 `--hook_token`**
- 默认绑定 `127.0.0.1` 且不发任何 CORS 头。在没有 token 的情况下，这两点是你访问的任意网页与
  `fetch` 你本机 `/sessions` 之间仅有的屏障——改动之前想清楚
- 放在 HTTPS 反向代理（Nginx、Caddy）之后，而不是直接挂到公网。`/ui` 本身不含数据，但也请在代理
  层加上鉴权
- `/ui` 带有 `Content-Security-Policy`（只允许同源脚本与样式，禁止内联，禁止被 frame），并且一律
  通过 `textContent` 渲染
- 定期轮换 token；不要把它写进镜像和仓库；挂载会话目录时加 `:ro`

## 排查

1. **某个数据源没被启用**（`/health` 的 `sources` 里没有它）：对照上面的表检查目录是否存在；
   `--mode all` 会打印哪些缺失。Hermes 有 `sessions.json` **或** `state.db` 之一即可。容器里
   请确认目录真的挂进来了。
2. **列表是空的**：看 `/health` 里启用了哪些数据源，以及进程是否有权限读这些目录。
3. **401**：检查 `Authorization: Bearer <token>` 与服务端启动时的 `--hook_token` 是否一致。
4. **端口被占用**：换一个 `--port`。

## 文档

下列文档均为英文。

| | |
|---|---|
| [docs/api.md](docs/api.md) | 完整 HTTP API：参数、匹配规则、响应字段 |
| [docs/mcp.md](docs/mcp.md) | MCP：两种传输、客户端配置、工具表 |
| [docs/deploy.md](docs/deploy.md) | 常驻部署：systemd / launchd / Windows 计划任务 / Docker |
| [docs/development.md](docs/development.md) | 目录结构、测试、发版、新增数据源 |
| [docs/internals.md](docs/internals.md) | 实现细节：各源解析、性能、搜索、跨平台 |

## 许可证

MIT，见 [LICENSE](LICENSE)。

---

**说明**：本服务是只读的。它不会修改 Hermes、OpenClaw、Pi、Claude Code、Codex 或 Gemini 的任何
会话数据。
