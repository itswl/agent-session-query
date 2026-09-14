# 本地 Agent 会话查询 HTTP API

一个用于查询本机各种 Agent 会话的轻量级 HTTP API 服务。只读，不改动任何会话数据。

支持六种数据源，**都在跑就一起查**：

| 数据源 | 位置 |
|--------|------|
| **Hermes** | `~/.hermes/sessions/` |
| **OpenClaw** | `~/.openclaw/agents/default/sessions/` |
| **Pi** | `~/.pi/agent/sessions/` |
| **Claude Code** | `~/.claude/projects/` |
| **Codex** | `~/.codex/sessions/` |
| **Gemini CLI** | `~/.gemini/tmp/` |

**✨ 自动检测模式**: 无需手动指定，存在的数据源都启用，两个服务同时开着就同时查询。

## 功能特性

- 🔍 **自动检测**: 自动识别上述六种数据源，无需手动配置
- 🔀 **多数据源合并**: 结果按更新时间倒序合并，每条带 `source` 字段标明来源（`--mode all` 强制全部启用）
- 📋 按 Session ID、会话 key 或路径片段查询单个会话（精确匹配优先于模糊匹配，跨数据源不互相遮蔽）
- 💬 获取会话的详细消息内容
- ✅ 获取会话的最终结果（OpenClaw/Hermes 取第一个 `stopReason=stop` 的助手消息；其余取最后一条助手消息）
- 🔒 支持 Bearer hook_token 认证
- 🐳 提供 Docker 和 Docker Compose 部署方式
- 🏥 内置健康检查端点（含启用的数据源清单与连接统计）

## API 端点

| 端点 | 方法 | 描述 |
|------|------|------|
| `/` | GET | API 信息和使用说明 |
| `/health` | GET | 健康检查（含连接统计） |
| `/stats` | GET | 服务器统计（连接数等） |
| `/sessions` 或 `/api/sessions` | GET | 列出所有会话 |
| `/sessions/<pattern>` 或 `/api/sessions/<pattern>` | GET | 查询单个会话信息 |
| `/sessions/<pattern>/messages?limit=50` 或 `/api/sessions/<pattern>/messages?limit=50` | GET | 获取会话消息 |
| `/sessions/<pattern>/final` 或 `/api/sessions/<pattern>/final` | GET | 获取会话最终结果 |

## 快速开始

### 本地运行

```bash
# 自动检测模式（推荐）- 存在的数据源都用，同时查询
python3 session_query_api.py [--port 8080]

# 强制指定模式（可选）
python3 session_query_api.py --mode all      # 六个数据源都启用（缺的会警告）
python3 session_query_api.py --mode claude   # 只看 Claude Code
python3 session_query_api.py --mode pi       # 只看 Pi
python3 session_query_api.py --mode codex    # 只看 Codex
python3 session_query_api.py --mode gemini   # 只看 Gemini CLI
python3 session_query_api.py --mode hermes   # 只看 Hermes
python3 session_query_api.py --mode openclaw # 只看 OpenClaw

# 带认证运行
python3 session_query_api.py --port 8080 --hook_token mysecrethooktoken
```

### Docker 运行

```bash
# 构建镜像（默认官方 python:3.10-slim；国内加 --build-arg PYTHON_IMAGE=... 换源）
docker build -t agent-session-query .

# 运行容器：把要查的会话目录挂进去，挂几个就查几个
docker run -d \
  --name agent-session-query \
  -p 8080:8080 \
  -v ~/.claude/projects:/root/.claude/projects:ro \
  -v ~/.codex/sessions:/root/.codex/sessions:ro \
  -v ~/.pi/agent/sessions:/root/.pi/agent/sessions:ro \
  -v ~/.gemini/tmp:/root/.gemini/tmp:ro \
  -v ~/.hermes/sessions:/root/.hermes/sessions:ro \
  -v ~/.openclaw/agents/default/sessions:/root/.openclaw/agents/default/sessions:ro \
  agent-session-query --hook_token your_hook_token_here
```

镜像的 ENTRYPOINT 就是脚本本身，`docker run` 后面的参数会直接透传（`--mode`、`--hook_token`、`--port` 等）；只想查某一种时加 `--mode claude` 这样即可。

### Docker Compose 运行

```bash
# 六个数据源目录都已挂上，没数据的会自动跳过
docker-compose up -d

# 设置认证令牌 / 指定模式
HOOK_TOKEN=your_hook_token_here SESSION_MODE=auto docker-compose up -d
```

## 使用示例

### 1. 列出所有会话

```bash
# OpenClaw 模式
curl http://localhost:8080/sessions

# Hermes 模式
curl http://localhost:8080/sessions
```

### 2. 查询特定会话

```bash
# OpenClaw: 通过 Run ID 查询
curl http://localhost:8080/sessions/5ab8e024-2740-422c-8503-89c01313f792

# OpenClaw: 通过 Session pattern 查询
curl http://localhost:8080/sessions/hook:alert:prometheus:b5123b01-616a-4da0-ac48-d9c81e3be63c

# Hermes: 通过 delivery_id 查询
curl http://localhost:8080/sessions/1776580775689

# Hermes: 通过 session_id 查询
curl http://localhost:8080/sessions/20260419_143935_73e269b4

# Hermes: 通过完整 key 查询
curl http://localhost:8080/sessions/agent:main:webhook:webhook:webhook:agent:1776580775689:webhook:agent
```

### 3. 获取会话消息

```bash
# OpenClaw: 获取前 50 条消息
curl http://localhost:8080/sessions/hook:alert:prometheus:b5123b01/messages

# OpenClaw: 自定义消息数量
curl http://localhost:8080/sessions/hook:alert:prometheus:b5123b01/messages?limit=20

# Hermes: 获取消息
curl http://localhost:8080/sessions/20260419_143935_73e269b4/messages
```

### 4. 获取会话最终结果

```bash
# OpenClaw: 获取最终结果
curl http://localhost:8080/sessions/hook:alert:prometheus:b5123b01/final

# Hermes: 获取最终结果
curl http://localhost:8080/sessions/20260419_143935_73e269b4/final
```

**注意**: OpenClaw/Hermes 返回的是第一个 `stopReason/finish_reason="stop"` 的助手消息（不是最后一条）；Pi / Claude Code / Codex / Gemini 返回最后一条助手消息。

### 5. 带认证的请求

```bash
curl -H 'Authorization: Bearer your_hook_token_here' http://localhost:8080/sessions
```

### 6. 健康检查

```bash
curl http://localhost:8080/health
```

### 7. 其它数据源（Pi / Claude Code / Codex / Gemini）

```bash
# 列出全部（六源合并，每条带 source 字段）
curl -H 'Authorization: Bearer xxx' http://localhost:8080/sessions

# Claude Code：直接用 session uuid（文件名）
curl -H 'Authorization: Bearer xxx' http://localhost:8080/sessions/e4b2b405-88ea-4782-a84c-92574380ed16/final

# Codex：用 rollout 文件名的一部分
curl -H 'Authorization: Bearer xxx' http://localhost:8080/sessions/rollout-2026-09-13T23-07-05

# Pi / Gemini：用文件名的片段
curl -H 'Authorization: Bearer xxx' http://localhost:8080/sessions/2026-09-13T12-50-41

# 只看某一类：把结果按 source 过滤（服务端也可以直接用 --mode 只启用一个源）
curl -s -H 'Authorization: Bearer xxx' http://localhost:8080/sessions | jq '.sessions[] | select(.source=="claude")'
```

## 响应格式

### 会话列表示例（OpenClaw）

```json
{
  "sessions": [
    {
      "key": "agent:default:hook:alert:prometheus:b5123b01-616a-4da0-ac48-d9c81e3be63c",
      "shortKey": "hook:alert:prometheus:b5123b01-616a-4da0-ac48-d9c81e3be63c",
      "sessionId": "b5123b01-616a-4da0-ac48-d9c81e3be63c",
      "source": "openclaw",
      "status": "done",
      "updatedAt": "2024-01-15 10:30:45",
      "hasFile": true,
      "model": "gpt-4",
      "runtimeMs": 1234,
      "totalTokens": 5678
    }
  ],
  "total": 1
}
```

### 会话列表示例（Hermes）

```json
{
  "sessions": [
    {
      "key": "agent:main:webhook:webhook:webhook:agent:1776580775689:webhook:agent",
      "shortKey": "agent:main:webhook:webhook:webhook:agent:1776580775689:webhook:agent",
      "sessionId": "20260419_143935_73e269b4",
      "source": "hermes",
      "status": "done",
      "updatedAt": "2026-04-19T14:40:16.669448",
      "hasFile": true,
      "createdAt": "2026-04-19T14:39:35.690352",
      "displayName": "webhook/agent",
      "platform": "webhook",
      "totalTokens": 0,
      "estimatedCostUsd": 0.0
    }
  ],
  "total": 1
}
```

### 消息列表示例

```json
{
  "messages": [
    {
      "id": "msg_123",
      "role": "assistant",
      "timestamp": "2024-01-15T10:30:45Z",
      "content": [
        {
          "type": "text",
          "content": "Hello! How can I help you today?"
        }
      ]
    }
  ],
  "total": 1
}
```

### 最终结果示例（OpenClaw）

```json
{
  "status": "done",
  "isFinal": true,
  "isProcessing": false,
  "messageCount": 5,
  "id": "msg_123",
  "timestamp": "2024-01-15T10:30:45Z",
  "stopReason": "stop",
  "text": "Task completed successfully.",
  "thinking": "",
  "toolCalls": [],
  "usage": {
    "inputTokens": 100,
    "outputTokens": 50,
    "totalTokens": 150
  }
}
```

### 最终结果示例（Hermes）

```json
{
  "status": "done",
  "isFinal": true,
  "isProcessing": false,
  "messageCount": 2,
  "id": "",
  "timestamp": "2026-04-19T14:40:16.636994",
  "stopReason": "stop",
  "text": "# 🔴 HighCPU 告警分析\n\n## 告警概要...",
  "thinking": "The user is asking me to analyze a CPU alert...",
  "toolCalls": [],
  "usage": {}
}
```

## 配置选项

### 命令行参数

| 参数 | 默认值 | 描述 |
|------|--------|------|
| `--host` | `0.0.0.0` | 绑定主机地址 |
| `--port` | `8080` | 监听端口 |
| `--mode` | `auto` | 运行模式：`auto`（自动检测，存在的数据源都用）/`all`（六个都启用）/`hermes`/`openclaw`/`pi`/`claude`/`codex`/`gemini` |
| `--hook_token` | `None` | Bearer 认证令牌 |
| `--max-connections` | `50` | 最大并发连接数，超出返回 503 |
| `--timeout` | `30` | 单连接超时秒数 |

### 环境变量 / 构建参数（Docker）

| 变量 | 用途 |
|------|------|
| `HOOK_TOKEN` | compose 传给 `--hook_token` 的 Bearer 令牌（留空则不启用认证） |
| `SESSION_MODE` | compose 传给 `--mode`，默认 `auto` |
| `PYTHON_IMAGE` | 构建参数：基础镜像，默认 `python:3.10-slim`，国内可用镜像源覆盖 |

## 数据源

### 自动检测逻辑

服务启动时会检测六种数据源，**存在的数据源都启用**（可以同时查询多个）：

| 数据源 | 会话文件 | 会话 ID 来源 |
|--------|----------|--------------|
| Hermes | `~/.hermes/sessions/sessions.json` + `<id>.jsonl` | `session_id` |
| OpenClaw | `~/.openclaw/agents/default/sessions/sessions.json` + jsonl | `sessionId` |
| Pi | `~/.pi/agent/sessions/<项目>/*.jsonl` | 首行 `id` |
| Claude Code | `~/.claude/projects/<项目>/<uuid>.jsonl` | 文件名 / `sessionId` |
| Codex | `~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl` | 首行 `payload.session_id` |
| Gemini CLI | `~/.gemini/tmp/<项目>/chats/session-*.jsonl` | 首行 `sessionId` |

一个都检测不到时默认使用 OpenClaw 路径（`--mode all` 会明确警告缺了哪个）。

### 多数据源的查询语义

- `/sessions` 返回所有启用数据源合并后的列表，按更新时间倒序，每条带 `source` 字段
- `/sessions/<pattern>` 先在所有数据源里按「精确 ID → 精确 key → key 后缀 → 子串 → 模糊 ID」的顺序匹配——一个数据源里的模糊命中不会盖掉另一个数据源里的精确命中
- `/health` 的 `sources` 字段列出本次实际启用的数据源
- 排序用的更新时间：OpenClaw/Hermes 用记录里的时间字段，其余四种用会话文件的修改时间

### 各数据源的解析说明

- **Pi**：行类型为 `session` / `model_change` / `message`；消息取 `message` 行，最终结果取最后一条 assistant 消息（`stopReason=stop` 视为已完成）
- **Claude Code**：取 `type=user/assistant` 的行，跳过 `isSidechain`（子代理）与 `queue-operation`/`attachment` 等噪声行；最终结果取最后一条 assistant 消息
- **Codex**：取 `response_item` 中 `payload.type=message` 的行（跳过 `developer` 角色）；最终结果取最后一条 assistant 消息
- **Gemini CLI**：文件是「首行元数据 + `$set` 补丁 + 消息行」的追加日志；消息取 `type=user/gemini` 的行，最终结果取最后一条 gemini 消息

## 安全注意事项

- 建议在生产环境中始终启用 Bearer hook_token 认证
- 避免在公共网络暴露无认证的 API 服务
- 使用 HTTPS 反向代理（如 Nginx）加密传输
- 定期更新和轮换认证令牌

## 故障排除

### 常见问题

1. **某个数据源查不到（/health 的 sources 里没有它）**
   - 该数据源在本机不存在：对照上表检查会话文件/目录是否存在
   - 跑在容器里时，还要确认对应目录挂载进去了（compose 默认挂了六个，手动 docker run 时按需加 `-v`）
   - 注意：镜像不预建会话目录；空的挂载目录不会被当成"有数据源"

2. **会话列表是空的**
   - `/health` 看启用了哪些源；再用 `--mode all` 强制启用全部（会打印缺哪个）
   - 确认服务进程有读取这些目录的权限

3. **认证失败 (401 Unauthorized)**
   - 确认请求中包含了正确的 Bearer hook_token
   - 检查启动时设置的 hook_token 是否与请求中的一致

4. **端口冲突**
   - 修改 `--port` 参数使用其他端口
   - 检查是否有其他服务占用了 8080 端口

### 日志查看

```bash
# Docker 容器日志
docker logs agent-session-query

# Docker Compose 日志
docker-compose logs -f
```

## 开发

### 项目结构

```
.
├── session_query_api.py    # 主应用程序（仅标准库）
├── Dockerfile               # Docker 镜像构建文件
├── docker-compose.yml       # Docker Compose 配置
├── LICENSE                  # MIT
└── README.md               # 项目文档
```

### 支持的数据格式

#### OpenClaw 格式

- **sessions.json 字段**: `sessionId`, `sessionFile`, `status`, `updatedAt`, `model`, `runtimeMs`, `totalTokens`
- **jsonl 格式**: `{"type": "message", "message": {"role": "assistant", "content": [...], "stopReason": "stop"}}`
- **content 类型**: 数组 `[{type: "text", ...}, {type: "thinking", ...}]`

#### Hermes 格式

- **sessions.json 字段**: `session_id`, `created_at`, `updated_at`, `display_name`, `platform`, `total_tokens`, `estimated_cost_usd`
- **jsonl 格式**: `{"role": "assistant", "content": "text", "reasoning": "thinking", "finish_reason": "stop"}`
- **content 类型**: 字符串

#### Pi 格式

- **会话文件**: `~/.pi/agent/sessions/<项目>/<时间>_<uuid>.jsonl`
- **首行**: `{"type": "session", "id": "...", "timestamp": "...", "cwd": "..."}`
- **消息行**: `{"type": "message", "message": {"role": "assistant", "content": [{"type": "text", "text": "..."}], "stopReason": "stop"}}`

#### Claude Code 格式

- **会话文件**: `~/.claude/projects/<项目 slug>/<session-uuid>.jsonl`
- **消息行**: `{"type": "user|assistant", "message": {"role": "...", "content": [{"type": "text|thinking|tool_use|tool_result", ...}]}}`
- 跳过 `isSidechain`（子代理）与 `queue-operation` / `attachment` 等非对话行

#### Codex 格式

- **会话文件**: `~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl`
- **首行**: `{"type": "session_meta", "payload": {"session_id": "...", "cwd": "..."}}`
- **消息行**: `{"type": "response_item", "payload": {"type": "message", "role": "user|assistant", "content": [{"type": "input_text|output_text", "text": "..."}]}}`

#### Gemini CLI 格式

- **会话文件**: `~/.gemini/tmp/<项目>/chats/session-*.jsonl`
- **形式**: 追加日志——首行元数据（`sessionId` / `startTime`）、`{"$set": {...}}` 补丁、以及消息行
- **消息行**: `{"type": "user|gemini", "timestamp": "...", "content": [...] 或 "字符串", "thoughts": "...", "tokens": {...}}`

### 本地开发

```bash
# 克隆项目
git clone <repository-url>
cd agent-session-query

# 无需安装依赖：只用 Python 标准库（3.8+），没有 requirements.txt

# 自动检测模式运行（推荐）
python3 session_query_api.py --port 8080

# 或强制指定单个数据源
python3 session_query_api.py --port 8080 --mode claude   # 也支持 pi / codex / gemini / hermes / openclaw / all
```

## 许可证

本项目采用 MIT 许可证。详见 [LICENSE](LICENSE) 文件。

## 贡献

欢迎提交 Issue 和 Pull Request 来改进本项目。

## 支持

如有问题或建议，请通过以下方式联系：

- 提交 GitHub Issue
- 查看现有文档和示例
- 参考代码注释

---

**注意**: 本服务是一个只读 API，不会对 OpenClaw 的会话数据进行任何修改。
