# agent-session-query

[English](README.md) · **简体中文**

查询本机各个 agent CLI 留下的会话记录：**一个只读网页、一套 HTTP API、以及一个供 agent 调用的
MCP server**。它不会修改任何会话数据。

单个二进制（不依赖 cgo，没有常驻运行时）—— `scp` 到任意同架构的机器上直接跑。唯一的外部依赖是
一个纯 Go 的 SQLite 驱动（用于读取 Hermes 的 `state.db`），所以交叉编译照常可用。

支持八个数据源；存在哪个就查哪个，也可以在一次查询里合并：

| 数据源 | 会话存放位置 | 会话 ID |
|--------|--------------|---------|
| Hermes | `~/.hermes/sessions/`（`sessions.json`）或 `~/.hermes/state.db`（较新版本，全 SQLite） | `sessions.json` 里的 `session_id` / `sessions` 表里的 `id` |
| OpenClaw | `~/.openclaw/agents/<agent>/agent/openclaw-agent.sqlite`（SQLite，2026.9+；旧版 `sessions.json` 布局仍可读） | `session_windows` 表的 `session_id` |
| Pi | `~/.pi/agent/sessions/<project>/*.jsonl`，以及 `--session-dir` 目录下平铺的 `*.jsonl` | session 行上的 `id` |
| Claude Code | `~/.claude/projects/<project>/*.jsonl` | 文件名（一个 uuid）/ `sessionId` 字段 |
| Codex | `~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl`，以及 `~/.codex/archived_sessions/` | 元数据行上的 `payload.session_id` |
| Gemini CLI | `~/.gemini/tmp/<project>/chats/session-*.jsonl` | 首行的 `sessionId` |
| OpenCode | `${XDG_DATA_HOME:-~/.local/share}/opencode/opencode.db`（SQLite；Windows 为 `%LOCALAPPDATA%\opencode`；1.x 的 `session` 表与 2.x 的 `session_v2` / `session_message` 表都读） | `session` 或 `session_v2` 表的 `id` |
| Grok CLI | `~/.grok/sessions/<url 编码的 cwd>/<session-id>/`（`summary.json` + `updates.jsonl`），以及 `~/.grok/archived_sessions/` | `summary.json` 里的 `info.id` |

表中的 `~` 指向运行用户的家目录：Linux 和 macOS 上是 `$HOME`，Windows 上是 `%USERPROFILE%`
（也就是 `C:\Users\<you>\.claude\projects` 之类）。每个目录也按各家 CLI 自己的方式解析——
`CLAUDE_CONFIG_DIR`、`CODEX_HOME`、`GROK_HOME`、`HERMES_HOME`、`OPENCLAW_STATE_DIR`、pi 的
`PI_CODING_AGENT_DIR` / `PI_CODING_AGENT_SESSION_DIR`、opencode 的 `XDG_DATA_HOME` 都会生效
（读的是本服务自身环境里的值）。各数据源的解析细节见
[docs/internals.md](docs/internals.md)；目录迁移与额外实例见
[docs/sources.md](docs/sources.md)。

## 快速开始

### 一行命令安装

不需要 Go 工具链；下载**用发布页的 `SHA256SUMS` 校验**，Windows 上还会自动解除系统拦截标记
（SmartScreen / Zone.Identifier）：

**macOS / Linux:**
```bash
curl -fsSL https://raw.githubusercontent.com/itswl/agent-session-query/main/install.sh | sh
```

**Windows (PowerShell):**
```powershell
irm https://raw.githubusercontent.com/itswl/agent-session-query/main/install.ps1 | iex
```

脚本会自动检测系统与架构（`darwin` / `linux` / `windows` × `amd64` / `arm64`），拉取最新 Release
产物、**用 `SHA256SUMS` 校验**、解压安装。Windows 上会解除下载文件的信任标记并把安装目录加进用户
PATH；macOS / Linux 上 `curl` 下载不带隔离属性、无需解除，只有在回退安装到 `~/.local/bin` 时才会
提示 PATH。（v0.25.0 之前的发布没有 `SHA256SUMS`，脚本会说明并跳过校验。）

### 手动下载构建产物

推送一个形如 `v0.1.0` 的 tag 会触发
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

- **左栏——列表**：每个会话都有一个可读的名字（opencode 用它自带的标题，其他源取首条真实
  用户消息），可按时间或项目分组；最近有新消息的会话带呼吸绿点（悬停可看多久之前），消息数由
  后台统计补上。Claude Code 的**子 agent** 按派生关系缩在所属会话下面（「▸ 12 agents」），
  CLI 归档的会话由 **Archived** 开关带回来。输入即过滤 sessionId / 路径 / cwd，同一次查询还会在
  服务端**搜索消息正文**——正文命中带片段附在分隔线下方；来源过滤选了某个来源时，正文搜索只在该来源内
- **中栏——按轮阅读的对话**：你提问，agent 干活，agent 回答。提问与回复之间的工作折叠成一行
  （*72 steps · 61 commands · changed 2 files · 1 failed · 24m39s*），失败步骤保持在视野里，
  连续的读和搜合并成「explored」。每一步点开是参数与输出，编辑一律以 diff 展开，结果带 exit code
  与耗时；两轮之间的事（压缩、中断、hook 失败）各占一行。五种视图：**all**、**conversation**，
  以及单看一类：**changes** / **commands** / **failures**。工具栏下方是本窗口汇总，失败数可逐个
  跳转。**会话内查找**（<kbd>f</kbd>）全篇标记、跨窗口跳转；从列表点开的正文命中会预填查找栏。
  默认打开最新 200 条、滚动自动翻页；工具输出先给预览、「Show full output」取全文；Markdown
  渲染，步骤按类别着色。复制就在内容旁边：代码块、一轮的交接摘要、步骤的命令 / diff / 输出
  （只加载了开头时先读全再复制）
- **右栏——结果与地图**：stopReason、答案与思考常驻；**对话目录**每轮两行——问了什么、得出了
  什么，有失败带红点——点哪行跳到消息流对应位置。往下是会话信息：恢复命令（连 `cd` 一起复制）、
  文件路径、导出（Markdown / JSONL / JSON / 独立 HTML）、用量与成本，以及同项目的其他会话和它的
  **pack**（最近各会话问了什么、得出了什么），可复制或下载交给另一个 agent

它可以装成 Web 应用（手机上「添加到主屏幕」），服务端升级后页面自行重载。手机上三栏是一层压一层
的三个屏幕，系统返回手势按原路退回。页面每 10 秒刷新一次且不打断阅读（展开的块、打开的轮次和
位置都保留），视图设置刷新后仍在；`/ui#<sessionId>` 可作深链接。明暗跟随系统并可手动切换；
左右两栏可折叠成窄条，状态被记住。

快捷键：<kbd>j</kbd> <kbd>k</kbd> 切换会话 · <kbd>/</kbd> 聚焦搜索 · <kbd>Enter</kbd> 立刻搜
正文 · <kbd>g</kbd> <kbd>G</kbd> 跳首尾 · <kbd>f</kbd> 会话内查找 · <kbd>[</kbd> <kbd>]</kbd>
上/下一个失败 · <kbd>r</kbd> 刷新 · <kbd>Esc</kbd> 清空搜索。

## 接口一览

所有查询都是 `GET`（MCP 的 `/mcp` 是 `POST`）。一旦设置了 `--hook_token`，标注"是"的接口就需要
`Authorization: Bearer <token>`。

| 接口 | 鉴权 | 作用 |
|------|------|------|
| `/` `/health` `/stats` | 否 | 服务信息与健康检查 |
| `/ui` `/favicon.ico` | 否 | 内嵌页面（本身不含任何数据） |
| `/sessions` | 是 | 列出全部会话（跨数据源合并，最新在前） |
| `/sessions/<pattern>` | 是 | 单个会话；可接 `/messages`（支持 `order` / `limit` / `at`，`full=1` 让工具输出不截断）、`/final`、`/rounds`（按每条真实提问切轮，带每轮改了哪些文件、几次工具调用失败）、`/brief`（一轮的交接简报，附恢复命令）或 `/export`（不带 `limit` 即全文，文件标明覆盖范围；`format=jsonl` 取结构化形式）|
| `/search?q=` | 是 | 跨全部数据源的**全文搜索**；`source` / `project` / `branch` / `since` / `until` 限定扫描哪些会话，`pattern` 限定在单个会话内，`role` 只看 user / assistant 的命中 |
| `/files?path=` | 是 | **哪些会话动过某个文件**，从新到旧：改过它、读过它或跑过点名它的命令的那几轮，以及当时问的是什么。`changed=1` 只留改动 |
| `/projects` | 是 | 按项目（cwd）分组的会话数 |
| `/export?project=&since=` | 是 | **把多个会话导出成一份文档**——每个会话问了什么、得出了什么，按时间从旧到新。`mode=full` 则把正文一并内联 |
| `/mcp` | 是 | MCP 的 Streamable HTTP 传输（`POST`） |

完整参数、`<pattern>` 的匹配规则和每个响应字段见 **[docs/api.md](docs/api.md)**。

## MCP

`--mcp` 走 stdio，HTTP 模式下另有 `POST /mcp`。这让 **agent 能查询自己的历史**——Claude Code
可以去翻你上周用 Codex 解决过的同一个问题。工具有：`search_sessions` / `list_sessions` /
`list_projects` / `recent_project_activity` / `get_session_pack` / `find_decisions` /
`find_similar_question` / `get_session` / `get_messages` / `list_rounds` / `session_brief` / `file_history`。
`get_session_pack` 一次调用拿到一组会话各自问了什么、得出了什么，按时间从旧到新；`file_history` 回答哪些会话改过某个文件、
在哪一轮、当时问的是什么。转写里的每个工具结果都带着它是
怎么结束的（状态、exit code、耗时）以及它回答的是哪次调用；`list_rounds` 说明哪一轮有失败、改了哪些
文件，`session_brief` 把一轮交接给另一个 agent，并附上恢复这个会话的命令。**会话正文是不可信输入**：
转写原样（无标记、不删改，脱敏针对的是密钥而非诱导性文字）交给调用方 agent，因此会话里出现的
"指令"只能当作内容来报告，绝不照做。

客户端配置和安全注意事项见 **[docs/mcp.md](docs/mcp.md)**。

## 配置

| 参数 | 默认值 | 作用 |
|------|--------|------|
| `--host` | `127.0.0.1` | 绑定地址；对外暴露用 `0.0.0.0`（非回环且未设 `--hook_token` 时拒绝启动） |
| `--port` | `8080` | 监听端口 |
| `--mode` | `auto` | `auto`（存在哪个启用哪个）/ `all`（八个全启用）/ `hermes` / `openclaw` / `pi` / `claude` / `codex` / `gemini` / `opencode` / `grok` |
| `--path` | 无 | 迁移或复制数据源：`--path claude=/mnt/disk/.claude/projects` 迁移；`--path claude:box2=/mnt/box2/.claude/projects` 增加名为 `claude:box2` 的实例。支持目录型（`pi` / `claude` / `codex` / `gemini` / `grok`）与 opencode 的数据库（其数据目录或 `opencode.db` 文件本身）。可重复——被包装器 `--session-dir` 托管的 pi 就是这么接进来的 |
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

Docker：每次发版都会把多架构镜像发布到 GitHub Container Registry，不必自己构建；环境变量为 `HOOK_TOKEN` 与 `SESSION_MODE`（默认 `auto`），本地构建时可用 `GO_IMAGE` 构建参数。见 [docs/deploy.md](docs/deploy.md)。

不带 `--hook_token` 时会转而读取 `HOOK_TOKEN` 环境变量——**命令行参数会出现在 `ps` 里，环境变量
不会**，所以长期运行的场景优先用后者。

## 安全

- 它能读到完整的会话内容，包括工具输出，所以**对外暴露前一定要设 `--hook_token`**；非回环地址
  且未设 token 时进程会**拒绝启动**，而不是只警告
- 默认绑定 `127.0.0.1` 且不发任何 CORS 头；在此之上，除 `OPTIONS` 预检外，所有路由都校验 `Origin`，且在没有 token
  且绑定回环时，`Host` 不是回环名字（`localhost`、`127.0.0.0/8`、`::1`）的请求一律 403——这正是
  DNS rebinding 页面发出的形状。设了 token、或显式绑非回环地址时 Host 检查关闭：反代转发过来的
  是公网名字，那里靠的是 token
- 放在 HTTPS 反向代理（Nginx、Caddy）之后，而不是直接挂到公网。`/ui` 本身不含数据，但也请在代理
  层加上鉴权
- `/ui` 带有 `Content-Security-Policy`（只允许同源脚本与样式，禁止内联，禁止被 frame），并且一律
  通过 `textContent` 渲染
- 发布产物附带 `SHA256SUMS`，两个安装脚本装前都会校验；v0.25.0 之前的发布没有该文件，脚本会说明并跳过
- 定期轮换 token；不要把它写进镜像和仓库；挂载会话目录时加 `:ro`

## 排查

1. **某个数据源没被启用**（`/health` 的 `sources` 里没有它）：对照上面的表检查目录是否存在；
   `--mode all` 会打印哪些缺失。Hermes 有 `sessions.json` **或** `state.db` 之一即可。容器里
   请确认目录真的挂进来了。若机器上什么都没装，`auto`（和 `all`）会回退启用 OpenClaw 让服务能起来,
   `/health` 里会有它，只是内容为空。`auto`/`all` 下这个集合在进程运行期间会持续重新探测，
   启动之后才产生第一个会话的 CLI 无需重启即可被识别。
2. **列表是空的**：看 `/health` 里启用了哪些数据源，以及进程是否有权限读这些目录。如果某个
   数据源读不出来，`/health` 的 `warnings` 会指出是哪个源、报什么错——读不出来和本来就没数据
   都表现为空列表，但只有前者是问题（设置了 token 时，`warnings` 只随带鉴权的请求返回）。
3. **401**：检查 `Authorization: Bearer <token>` 与服务端启动时的 `--hook_token` 是否一致。
4. **端口被占用**：换一个 `--port`。

## 文档

下列文档均为英文。

| | |
|---|---|
| [docs/api.md](docs/api.md) | 完整 HTTP API：参数、匹配规则、响应字段 |
| [docs/mcp.md](docs/mcp.md) | MCP：两种传输、客户端配置、工具表 |
| [docs/deploy.md](docs/deploy.md) | 常驻部署：systemd / launchd / Windows 计划任务 / Docker |
| [docs/sources.md](docs/sources.md) | 数据源与 `--path` 自定义路径 |
| [docs/development.md](docs/development.md) | 目录结构、测试、发版、新增数据源 |
| [docs/internals.md](docs/internals.md) | 实现细节：各源解析、性能、搜索、跨平台 |
| [docs/roadmap.md](docs/roadmap.md) | 已经做了什么、还剩什么以及值不值得做、明确不做什么 |

## 许可证

MIT，见 [LICENSE](LICENSE)。

---

**说明**：本服务是只读的。它不会修改 Hermes、OpenClaw、Pi、Claude Code、Codex、Gemini、OpenCode
或 Grok 的任何会话数据。
