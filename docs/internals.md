# 实现细节

面向维护者的内部文档：各数据源的解析细节与性能说明。使用与部署方式见 [README](../README.md)。

## 各数据源的解析细节

- **Hermes**：`sessions.json` 是 `key → 记录` 的映射，会话内容在同目录的 `<session_id>.jsonl`；消息取 `role` 为 `user`/`assistant` 的行，`content` 是字符串，推理在 `reasoning`。**新版 Hermes 不写 `sessions.json` / jsonl，会话全部落在 `~/.hermes/state.db`**：库存在即启用该源，列表与消息直接查 `sessions` / `messages` 表（时间戳是 epoch 秒，格式化成 UTC；assistant 的 `reasoning` 作为 thinking；`platform` 取 `sessions.source`）；两处都有时按 sessionId 去重，jsonl 优先。没有 jsonl 的会话 `final` 也回退到 `state.db`（只读打开，取最后一条 `active=1` 且 `finish_reason=stop` 的助手消息，`message_count` 缺失时回退成实际条数）
- **OpenClaw**：同上结构，字段名是 `sessionId`/`stopReason`；消息取 `type=message` 的行，`message.content` 是块数组（`text`/`thinking`/`toolCall`/`toolResult`），`stopReason` 可能在 `message` 里也可能在行顶层
- **Pi**：行类型有 `session`（元数据）、`model_change`、`message`；消息取 `message` 行（`message.role` + `message.content` 块数组）。
  列表的元数据**必须认准 `type=session` 那一行**：`model_change` 记录自己也有 `id` 字段，只读第一行就会把事件 id
  当成会话 id 报出去（本机实测踩到过）。完全没有 `session` 行时（被截断或续写的文件）退回文件名里的 uuid
- **Claude Code**：消息行是 `type=user`/`assistant`，内容在 `message.content`（`text`/`thinking`/`tool_use`/`tool_result` 块）；跳过 `isSidechain`（子代理）以及 `queue-operation`、`attachment`、`mode` 等非对话行。
  `cwd` 不在首行，前面有一串非对话行——本机 174 个真实会话的分布是第 2 行 1 个 / 第 3 行 123 / 第 4 行 30 / 第 5 行 19 / 第 6 行 1
- **Codex**：消息行是 `type=response_item` 且 `payload.type=message`；`payload.role` 为 `developer` 的行（系统拼装的指令）不计入；用量取自 `token_usage_record`。
  元数据行认 `type=session_meta`（权威且字段最全，拿到就停）；找不到才退而求其次，从后面的行里补
  ——实测一个真实 rollout 的行类型是互补的：`turn_context` 只带 `cwd`，`token_usage_record` 只带 `session_id`。
  **补的时候绝不碰裸 `id`**：消息行（`response_item`）的 payload 带 `id = "msg_…"`，顺手捡就会把消息 ID 当成会话 ID
- **Gemini CLI**：文件是追加日志——首行元数据（`sessionId`/`startTime`）、`{"$set": {...}}` 补丁行、以及消息行；消息取 `type=user`/`type=gemini` 的行，`content` 可能是数组（user）或字符串（gemini）。工具调用型会话里正文很稀：发起调用时 `content` 是空串、内容在 `toolCalls` 字段（→ `toolCall` 块，只带 name/args），执行结果由后续 user 行 `content` 数组里的 `functionResponse` 项回传（→ `toolResult` 块）；`thoughts` 字符串或 `[{subject, description}]` 数组都作为 thinking（数组取各条 description）

### 会话的「更新时间」：读尾部，别信 mtime

列表按 `updatedAt` 倒序，而文件型数据源早期直接拿文件 mtime 当这个值。mtime 是「文件被写过」
的时间，不是「对话发生」的时间，两者会大幅脱节：

- 实测本机 174 个真实 Claude 会话，**43 个（25%）相差超过 1 小时**，最大差 **235 小时**——
  某些操作会重写会话文件却不追加新内容，于是六天前聊完的会话被顶到列表最前面，
  还会被 `isActive` 误判成「正在写入」
- Gemini 更隐蔽：它用首行元数据的 `lastUpdated`，但那是**会话开始**那一刻的值，之后由
  `$set` 补丁行更新。实测 33 个会话里 **29 个首行时间是陈的**

现在统一改成读文件**尾部**最后一条记录自带的时间（`lastRecordTime`）。不读整个文件——
从尾部 seek 一小段就够，实测 172/174 个文件在最后 64 KB 里就能找到完整记录，凑不出就逐级
放大到 512 KB / 4 MB。三种时间放法都认：顶层 `timestamp`、`message.timestamp`、
`$set.lastUpdated`。取不到、或者取到的时间解析不了（解析不了会被排到列表最末尾，比用 mtime
还糟）才退回 mtime。

开销由文件头缓存吸收：缓存按 `(mtime, size)` 判新鲜度，只有变动过的文件才会重新读尾部。

### 元数据的定位：读到拿齐为止，且认准行类型

三个文件型数据源（Claude / Pi / Codex）的列表元数据都不在固定位置，早期实现用的是固定窗口
（Claude 前 5 行、Pi 和 Codex 只读第 1 行），两种翻车方式都实际发生过：

- **读不到**——Claude 的 `cwd` 落在第 6 行，于是 `project` 为空、项目聚合把它算进 ungrouped，不报错
- **读到错的**——Pi 首行是 `model_change` 时，那条记录自己也有 `id`，于是会话 ID 被报成事件 ID

现在统一成两条：**扫到拿齐需要的字段就停**（上限 50 行只是防御性兜底，常见情况第 1–3 行就停，
比固定窗口还少读），并且**认准行类型再取字段**，而不是「第一行有什么就用什么」。

Gemini 不在此列：它的元数据确实就在首行，而本机 33 个文件里有 2 个压根没有元数据行
（首行直接是消息），那是数据本身缺失，代码正确地退化成用文件名和 mtime。

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

> 表里的 `GET /sessions` 是加文件头缓存（下面第 2 条）之前测的，实测数字见「列表这条路的开销」一节。

实现要点：

1. **列表不扫全文**——每个会话只读首行元数据（Gemini 的 `updatedAt` 优先用元数据里的时间，缺了才退回文件时间），`GET /sessions` 是「stat + 读首行」而不是「读 208 MB」
2. **文件头按 (mtime, size) 记忆化**（`filecache.go`）——头几行的内容只随文件本身变化，stat 一下就知道上次的结果还能不能用。稳态下 `List()` 退化成一轮 stat，完全不打开文件
3. **流式读取**——`messages` 取够 `limit` 条就停；`final` 只保留最后一条助手消息
4. **`final` 用结构体「探测」再物化**——整文件扫描时只解析 `type`/`role` 这类标量字段（Go 的 JSON 解码器会跳过不关心的字段，不建 map），只在最后一条助手消息上做完整解析
5. **行读取复用缓冲**——`bufio.Scanner` 的 `Bytes()` 是内部缓冲的视图，每行不额外分配
6. **会话列表短 TTL 缓存**（`--cache-ttl`，默认 2 秒）——查找会话与列表都走它；**消息与最终结果始终直接读文件**，缓存只影响「有哪些会话」这层元数据；`--cache-ttl 0` 可完全关掉。同一数据源同一时刻只会有一次扫描（缓存刚过期时并发请求不会各扫一遍）
7. **响应流式写出**——不在内存里先缓冲整个 JSON；`?limit=` 夹在 `--max-limit`（默认 1000）以内，单个请求摊不出几十 MB 的响应
8. **`/sessions` 带 ETag**——页面 10 秒轮一次，列表没变就在 304 结束
9. **`?order=desc` 用环形缓冲取尾巴**——取最新 N 条要扫到文件末尾，但只留住最后 N 条，内存跟 `limit` 走而不是跟会话长度走；Hermes 的 SQLite 路径直接让 SQL 倒着取再翻回来

### 列表这条路的开销

本机 173 个真实 Claude Code 会话（`~/.claude/projects`，共 470 MB），M3 Pro，`--cache-ttl 0`（关掉 TTL 缓存，单看每次真实扫描的成本）：

| 动作 | 耗时 |
|------|------|
| glob 目录 | 0.9 ms |
| glob + 逐个 stat | 2.4 ms |
| glob + stat + 解析每个文件的头几行 | 52 ms |

也就是说九成开销花在重复解析没变过的文件头上，而页面每 10 秒就轮询一次。加上 `filecache.go` 之后（同一台机器、同样 173 个会话，端到端量 `GET /sessions`）：

| | 首次（冷） | 之后（文件都没变） |
|---|---|---|
| `GET /sessions` | 154 ms | **2.5 ms** |
| 带 `If-None-Match` | — | **1.7 ms / 0 字节（304）** |

`?limit=` 夹上限的效果（99 MB、16444 条消息的大会话，`?limit=99999999`）：响应 14 MB → 0.9 MB，耗时 890 ms → 48 ms，进程 RSS 峰值 145 MB → 25 MB。

### 页面

页面在 `internal/app/ui/`，用 `go:embed` 打进二进制——不引 npm、不加构建步骤，仍然是单文件分发。
几条硬约束：

- **渲染一律走 `textContent`**。消息里全是第三方文本（工具输出、网页正文），拼 HTML 就是存储型
  XSS，而页面的 token 存在 localStorage，被打中等于泄露。`ui_test.go` 里钉了一条测试，禁止
  `innerHTML` / `outerHTML` / `insertAdjacentHTML` / `document.write` / `eval(` 出现在这三个文件里。
  另外发一条 `Content-Security-Policy`（脚本样式只许同源、不许内联、不许被 iframe 套）兜底
- **页面本身免认证**（它不含数据），但数据仍然只有带 `Authorization` 头才取得到。服务端没设
  token 时页面直接进——`/health` 的 `authRequired` 就是为此返回的

另一个难点不在样式，在「自动刷新不能把正在读的东西掀掉」：

- 列表按 sessionId 做**增量 patch**——命中的节点就地改文本（`setText` 只在内容真变了时写 DOM，否则会把用户选中的文本清掉），顺序变化用 `insertBefore` 移动节点而不是重建
- 详情只在选中会话的 `updatedAt` / `status` / 取值方向真的变了时才重拉；同一个会话刷新时不清空旧内容，不闪
- 消息流重建前记下展开的折叠块（按 `消息 id:块序号` 做稳定 key）与 `scrollTop`，重建后还原；原先贴着底部的话继续贴着底部
- 首次打开按取值方向决定落点：看最新就滚到底，看最早就从头开始

### 全文搜索

`/search` 不建索引。索引要落盘、要维护、要考虑失效，「单二进制、只读、scp 过去就能跑」这条
就没了——而实测根本不需要：

| 本机 174 个真实会话 / 470 MB | 耗时 |
|---|---|
| 冷（页面缓存未命中） | 1095 ms |
| 热 | **60 ms** |
| `since=2d`（收窄到 43 个会话） | 45 ms |

快在顺序上，不在算法上：

1. **先按原始字节过滤**——`bytes.Contains` 在 `eachJSONLLine` 拿到的裸字节上做，命中了才
   `json.Unmarshal` 那一行。99% 的行连解析都省掉，而 JSON 解析才是扫描里最贵的部分
2. **大小写折叠不分配**——`appendLowerASCII` 复用同一个缓冲逐行覆写；按字节折 `A-Z`，
   UTF-8 多字节序列（首字节 ≥ 0x80）原样穿过，中文本来也没有大小写
3. **会话级并行**——`GOMAXPROCS` 个 worker 各扫一个会话，结果按下标回填，顺序不乱
4. **新的先扫**——候选按更新时间倒序，截断到 `limit` 时留下的是最近的

取片段时有个坑：JSON 里到处是字符串，命中可能落在字段名上。所以 `findMatchingText` 只认
正文字段（`text` / `content` / `thinking` / `reasoning` / …），并且为了结果稳定——Go 的 map
遍历是乱序的——先按固定顺序找这几个键，其余键按键名排序后再找。

Hermes 新版会话全在 SQLite 里、没有 jsonl，所以 `JsonMapSource` 实现了可选的
`searchableSource` 接口走 `LIKE`（SQLite 的 LIKE 对 ASCII 本来就大小写无关）；
其余五个源都只有文件，走通用路径就够。

### 后台运行

`-d` 不是 fork。Go 的运行时是多线程的，而 `fork` 只复制调用线程，子进程拿到的是一个半死的
运行时——所以走「重新 exec 自己」：把 `-d` 从参数里摘掉（不摘就无限套娃），stdout/stderr
接到日志文件，`Setsid`（Windows 上是 `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`）
让它脱离当前终端的会话。

父进程不能 Start 完就走：那样端口被占、配置写错这类问题全都静默变成「启动成功」。所以要盯两件事：

1. **端口起来了**——但「端口通了」不等于成功，通的可能是先前那个实例，而我们拉起的这个
   已经因为 bind 失败死了
2. **子进程还活着**——`cmd.Wait()` 放在 goroutine 里和探活赛跑；探活赢了也要再静置
   300 ms 复查一次，因为子进程可能在探活刚成功的那一瞬才死

失败时把日志末尾一并打到 stderr：子进程的输出全进了日志文件，不贴出来用户在终端上什么都看不到。

### MCP

`--mcp` 走 stdio 上的 JSON-RPC 2.0（`json.Decoder` 逐个值读，天然处理换行分隔）。两条要点：

- **stdout 归 JSON-RPC 独占**。启动横幅、警告一律写 stderr，否则会把协议流冲烂
- **工具级错误走 `isError` 而不是 JSON-RPC error**。参数错了要让模型看得到错误内容，
  它才能自己改参数重试；协议错误（未知方法）才返回 `-32601`

### 跨平台

支持 `linux` / `darwin` / `windows` × `amd64` / `arm64`（`windows/386` 也编得过，只是没发）。唯一的依赖是纯 Go 的 SQLite 驱动，六个平台都有移植（Windows 走 `sqlite_windows.go`），`CGO_ENABLED=0` 交叉编译不需要任何 C 工具链。

Windows 上需要单独照顾的几处：

- **路径分隔符**：文件型数据源的 `key` 就是完整路径，Windows 上是 `C:\...\abc.jsonl`。匹配前统一过一遍 `normalizeForMatch`（小写 + 正斜杠），否则用户按习惯敲 `proj/abc.jsonl` 会匹配不上，或者从「后缀精确命中」掉成「子串命中」
- **SQLite 的 file: URI**：反斜杠塞进 URI 有转义歧义，`sqliteURI` 统一换成正斜杠（SQLite 在 Windows 上认 `file:C:/Users/.../state.db`）
- **控制台代码页**：启动横幅是中文，而 cmd.exe 默认是本地代码页（简中 GBK/936），直接打 UTF-8 就是乱码。`console_windows.go` 在启动时调 `SetConsoleOutputCP(65001)`；非 Windows 是空实现
- **测试隔离**：`os.UserHomeDir()` 在 Windows 上读 `%USERPROFILE%` 而不是 `$HOME`，测试里的 `setHome` 两个都设
- **`syscall.SIGTERM`**：在 Windows 上有定义（能编过）但永远不会送达，Ctrl+C 走的是 `os.Interrupt`，优雅退出照常工作

CI 在 ubuntu / windows / macOS 三个 runner 上各跑一遍测试——交叉编译出来的测试二进制没法在别的平台执行，发 Windows 产物却不在 Windows 上验一遍等于盲发。

### 排序

各源的更新时间形态不一（`mtime` 派生的 `2006-01-02T15:04:05`、Gemini 的 RFC3339Nano、Hermes 直接来自 `sessions.json` 的字符串、OpenClaw 的 epoch 毫秒），统一在 `newRecord` 里解析成 `time.Time` 再比。只按字典序比字符串的话，一个带时区偏移的时间戳就能把跨源排序排错。解析不出来的记录排在有时间的那些后面，它们之间退回字符串倒序。

边界与代价：

- 新建的会话最长 2 秒后才会出现在列表/查询里（可调小或设为 0）
- `final` 仍需顺序读完整个会话文件（要取最后一条助手消息），3000 行的大会话约 35 ms——单文件顺序读 + 解析的固有成本
- 单行超过 256 MB 时 `bufio.Scanner` 会中止，**该文件后面的内容整段读不到**（不是「跳过这一行」）；这种情况会往 stderr 打一行警告，不会静默截断。正常会话到不了这个量级
- 文件头缓存按 (mtime, size) 判断新鲜度。同一秒内被改两次、且大小一模一样的文件会读到旧的元数据——往 jsonl 追加内容总会改变大小，实际碰不到
