# OpenClaw/Hermes Session HTTP API

一个用于查询和管理 OpenClaw/Hermes Agent 会话的轻量级 HTTP API 服务。该服务通过 RESTful API 提供对会话数据的访问，包括会话列表、消息查询和最终结果获取。

**✨ 自动检测模式**: 无需手动指定，自动识别数据源并适配格式！

**🔀 两个数据源可以同时用**: OpenClaw 和 Hermes 都在跑时，同一个接口同时查两边，每条结果带 `source` 字段标明来源。

## 功能特性

- 🔍 **自动检测**: 自动识别 OpenClaw / Hermes 数据源，无需手动配置
- 🔀 **多数据源合并**: 两个服务同时开着时一并查询，结果按更新时间倒序、每条带 `source` 标记（`--mode all` 可强制两个都开）
- 📋 根据 Run ID、Session ID 或 Session pattern 查询单个会话
- 💬 获取会话的详细消息内容
- ✅ 获取会话的最终结果（第一个 `finish_reason/stopReason="stop"` 的助手消息）
- 🔒 支持 Bearer hook_token 认证
- 🐳 提供 Docker 和 Docker Compose 部署方式
- 🏥 内置健康检查端点

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
# 自动检测模式（推荐）- 存在的数据源都用（两个都开着就都查）
python3 openclaw_session_query_api.py [--port 8080]

# 强制指定模式（可选）
python3 openclaw_session_query_api.py --mode all       # 两个数据源都用（缺的会警告）
python3 openclaw_session_query_api.py --mode hermes    # 强制 Hermes
python3 openclaw_session_query_api.py --mode openclaw  # 强制 OpenClaw

# 带认证运行
python3 openclaw_session_query_api.py --port 8080 --hook_token mysecrethooktoken
```

### Docker 运行

```bash
# 构建镜像
docker build -t openclaw-session-query-api .

# 运行容器
docker run -d \
  --name openclaw-session-query-api \
  -p 8080:8080 \
  -v ~/.openclaw/agents/default/sessions:/root/.openclaw/agents/default/sessions \
  -e HOOK_TOKEN=your_hook_token_here \
  openclaw-session-query-api
```

### Docker Compose 运行

```bash
# 使用默认配置运行
docker-compose up -d

# 或者设置环境变量后运行
OPENCLAW_HOOK_TOKEN=your_hook_token_here docker-compose up -d
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

**注意**: 返回的是第一个 `stopReason/finish_reason="stop"` 的助手消息，而非最后一条消息。

### 5. 带认证的请求

```bash
curl -H 'Authorization: Bearer your_hook_token_here' http://localhost:8080/sessions
```

### 6. 健康检查

```bash
curl http://localhost:8080/health
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
| `--mode` | `auto` | 运行模式：`auto`（自动检测，存在的数据源都用）/`all`（两个都用）/`openclaw`/`hermes` |
| `--hook_token` | `None` | Bearer 认证令牌 |
| `--max-connections` | `50` | 最大并发连接数，超出返回 503 |
| `--timeout` | `30` | 单连接超时秒数 |

### 环境变量（Docker）

| 环境变量 | 描述 |
|----------|------|
| `HOOK_TOKEN` | 用于 API 认证的 Bearer hook_token |
| `OPENCLAW_HOOK_TOKEN` | docker-compose.yml 中使用的 hook_token 变量 |

## 数据源

### 自动检测逻辑

服务启动时会检测两个数据源，**存在的数据源都启用**（两个服务同时开着就同时查询）：

1. **Hermes**: `~/.hermes/sessions/sessions.json`
2. **OpenClaw**: `~/.openclaw/agents/default/sessions/sessions.json`
3. 如果都不存在，默认使用 OpenClaw 路径（`--mode all` 时会明确警告缺了哪个）

**无需手动配置**，服务会自动识别并适配对应的数据格式。

### 多数据源的查询语义

- `/sessions` 返回两个数据源合并后的列表，按更新时间倒序，每条带 `source` 字段
- `/sessions/<pattern>` 等接口先在所有数据源里按「精确 ID → 精确 key → key 后缀 → 子串 → 模糊 ID」的顺序匹配——一个数据源里的模糊命中不会盖掉另一个数据源里的精确命中
- `/health` 的 `sources` 字段列出本次实际启用的数据源

## 安全注意事项

- 建议在生产环境中始终启用 Bearer hook_token 认证
- 避免在公共网络暴露无认证的 API 服务
- 使用 HTTPS 反向代理（如 Nginx）加密传输
- 定期更新和轮换认证令牌

## 故障排除

### 常见问题

1. **会话文件未找到**
   - 确保 OpenClaw 已运行并生成了会话数据
   - 检查 `~/.openclaw/agents/default/sessions/` 目录权限

2. **认证失败 (401 Unauthorized)**
   - 确认请求中包含了正确的 Bearer hook_token
   - 检查启动时设置的 hook_token 是否与请求中的一致

3. **端口冲突**
   - 修改 `--port` 参数使用其他端口
   - 检查是否有其他服务占用了 8080 端口

### 日志查看

```bash
# Docker 容器日志
docker logs openclaw-session-query-api

# Docker Compose 日志
docker-compose logs -f
```

## 开发

### 项目结构

```
.
├── openclaw_session_query_api.py    # 主应用程序（仅标准库）
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

### 本地开发

```bash
# 克隆项目
git clone <repository-url>
cd openclaw-session-query-api

# 无需安装依赖：只用 Python 标准库（3.8+），没有 requirements.txt

# 自动检测模式运行（推荐）
python3 openclaw_session_query_api.py --port 8080

# 或强制指定模式
python3 openclaw_session_query_api.py --port 8080 --mode hermes
python3 openclaw_session_query_api.py --port 8080 --mode openclaw
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
