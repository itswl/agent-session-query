package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// MCP server（stdio 传输，JSON-RPC 2.0，逐行一个 JSON 值）。
//
// 这个工具的用户是「同时装了六个 Agent CLI 的人」，那么让 Agent 自己查自己的历史
// 就很自然：Claude Code 可以去搜你上周用 Codex 解决过的同一个问题。
// HTTP API 本来就能被 Agent 调用，MCP 加的是可发现性和参数 schema。
//
// 铁律：stdout 只能有 JSON-RPC。日志、警告一律走 stderr，否则会把协议流冲烂。
const (
	mcpProtocolVersion = "2025-06-18"
	mcpDefaultLimit    = 20
	maxMCPBodyBytes    = 1 << 20 // 请求体上限 1 MB：查询参数而已，用不了这么多
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type mcpServer struct {
	api      *SessionQueryAPI
	sources  []SessionSource
	maxLimit int
}

// runMCP 跑 stdio 上的 MCP 循环，返回进程退出码。
func runMCP(s *mcpServer, in io.Reader, out io.Writer) int {
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)

	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			// 坏输入：说一声就退出，继续读下去只会一直撞同一个坏字节
			fmt.Fprintf(os.Stderr, "[ERROR] MCP 输入解析失败: %v\n", err)
			return 1
		}

		result, rpcErr := s.dispatch(req.Method, req.Params)
		if len(req.ID) == 0 {
			continue // 通知（没有 id）不需要回包
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] MCP 输出失败: %v\n", err)
			return 1
		}
	}
}

func (s *mcpServer) dispatch(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agent-session-query", "version": buildVersion},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		return s.callTool(params)
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "unknown method: " + method}
	}
}

func (s *mcpServer) callTool(params json.RawMessage) (any, *rpcError) {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &rpcError{Code: -32602, Message: "bad params: " + err.Error()}
	}

	payload, err := s.runTool(call.Name, call.Arguments)
	if err != nil {
		// 工具级错误按 MCP 约定走 isError，而不是协议错误——
		// 这样模型看得到错误内容，可以自己改参数重试
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": err.Error()}},
			"isError": true,
		}, nil
	}
	body, jsonErr := json.MarshalIndent(payload, "", "  ")
	if jsonErr != nil {
		return nil, &rpcError{Code: -32603, Message: jsonErr.Error()}
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(body)}},
	}, nil
}

func (s *mcpServer) runTool(name string, args map[string]any) (any, error) {
	switch name {
	case "search_sessions":
		query := strings.TrimSpace(argString(args, "query"))
		if query == "" {
			return nil, errors.New("缺少参数 query")
		}
		q := searchQuery{
			needle:     query,
			lowered:    appendLowerASCII(nil, []byte(query)),
			limit:      argInt(args, "limit", mcpDefaultLimit, s.maxLimit),
			perSession: argInt(args, "per_session", defaultSearchPerSession, s.maxLimit),
		}
		if raw := strings.TrimSpace(argString(args, "since")); raw != "" {
			since, err := parseSince(raw)
			if err != nil {
				return nil, err
			}
			q.since = since
		}
		found := s.api.search(q)
		return map[string]any{
			"results": found.results, "matched": found.matched,
			"scanned": found.scanned, "truncated": found.matched > len(found.results),
		}, nil

	case "list_sessions":
		sessions, _ := s.api.listSessions()
		if want := strings.TrimSpace(argString(args, "source")); want != "" {
			filtered := sessions[:0:0]
			for _, item := range sessions {
				if toStr(item["source"]) == want {
					filtered = append(filtered, item)
				}
			}
			sessions = filtered
		}
		if project := strings.TrimSpace(argString(args, "project")); project != "" {
			filtered := sessions[:0:0]
			for _, item := range sessions {
				if strings.Contains(toStr(item["project"]), project) {
					filtered = append(filtered, item)
				}
			}
			sessions = filtered
		}
		limit := argInt(args, "limit", mcpDefaultLimit, s.maxLimit)
		total := len(sessions)
		if len(sessions) > limit {
			sessions = sessions[:limit]
		}
		return map[string]any{"sessions": sessions, "total": total, "returned": len(sessions)}, nil

	case "list_projects":
		projects, ungrouped := s.api.listProjects()
		return map[string]any{"projects": projects, "total": len(projects), "ungrouped": ungrouped}, nil

	case "get_session":
		pattern := strings.TrimSpace(argString(args, "pattern"))
		if pattern == "" {
			return nil, errors.New("缺少参数 pattern")
		}
		session, ok := s.api.getSession(pattern)
		if !ok {
			return nil, fmt.Errorf("没有匹配 %q 的会话", pattern)
		}
		final, _ := s.api.getFinalMessage(pattern)
		return map[string]any{"session": session, "final": final}, nil

	case "get_messages":
		pattern := strings.TrimSpace(argString(args, "pattern"))
		if pattern == "" {
			return nil, errors.New("缺少参数 pattern")
		}
		q := messageQuery{
			limit:   argInt(args, "limit", 50, s.maxLimit),
			fromEnd: strings.EqualFold(argString(args, "order"), "desc"),
		}
		messages, ok := s.api.getMessages(pattern, q)
		if !ok {
			return nil, fmt.Errorf("没有匹配 %q 的会话", pattern)
		}
		return map[string]any{"messages": messages, "total": len(messages), "order": orderName(q.fromEnd)}, nil

	default:
		return nil, fmt.Errorf("未知工具: %s", name)
	}
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	return toStr(args[key])
}

func argInt(args map[string]any, key string, def, max int) int {
	n := def
	if args != nil {
		if v, ok := toFloat(args[key]); ok && v >= 0 {
			n = int(v)
		}
	}
	if max > 0 && n > max {
		n = max
	}
	return n
}

func strSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intSchema(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func mcpTools() []map[string]any {
	return []map[string]any{
		{
			"name": "search_sessions",
			"description": "在本机所有 Agent / CLI（Claude Code、Codex、Gemini CLI、Pi、Hermes、OpenClaw）" +
				"的历史会话里全文搜索。用来回答「我之前在哪个会话里处理过 X」。大小写无关。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":       strSchema("要搜的文本"),
					"limit":       intSchema("最多返回多少个会话，默认 20"),
					"per_session": intSchema("每个会话最多返回几条命中，默认 3"),
					"since":       strSchema("只搜这个时间之后更新的会话，如 30d / 12h / 2026-09-01。历史很大时用来收窄范围"),
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "list_sessions",
			"description": "按更新时间倒序列出会话，可按数据源或项目（cwd）过滤。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"source":  strSchema("只看某个数据源：hermes / openclaw / pi / claude / codex / gemini"),
					"project": strSchema("按项目路径（cwd）过滤，子串匹配"),
					"limit":   intSchema("最多返回多少条，默认 20"),
				},
			},
		},
		{
			"name":        "list_projects",
			"description": "把会话按项目（cwd）归拢，看同一个仓库上用过哪几个 Agent、各多少个会话。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "get_session",
			"description": "取一个会话的元信息与最终结果。pattern 可以是完整 sessionId、其片段，或文件路径片段。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"pattern": strSchema("sessionId 或其片段 / 文件路径片段")},
				"required":   []string{"pattern"},
			},
		},
		{
			"name":        "get_messages",
			"description": "取一个会话的消息。长会话最有价值的往往是结尾，用 order=desc 取最新的 N 条。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": strSchema("sessionId 或其片段 / 文件路径片段"),
					"limit":   intSchema("最多返回多少条，默认 50"),
					"order":   strSchema("asc 取最早的 N 条（默认），desc 取最新的 N 条"),
				},
				"required": []string{"pattern"},
			},
		},
	}
}

// mcpStartupBanner MCP 模式下的启动信息只能写 stderr
func mcpStartupBanner(mode string, sources []SessionSource) {
	fmt.Fprintf(os.Stderr, "MCP server（stdio）已就绪 · 模式 %s · 数据源:", mode)
	for _, source := range sources {
		fmt.Fprintf(os.Stderr, " %s", source.Mode())
	}
	fmt.Fprintf(os.Stderr, "\n启动于 %s\n", time.Now().Format(time.RFC3339))
}

// ---------------------------------------------------------------------------
// Streamable HTTP 传输（POST /mcp）
// ---------------------------------------------------------------------------

// MCP 的 Streamable HTTP：单端点收 JSON-RPC。
//
// 这个实现是无状态的——每个请求自带全部上下文，服务端不留会话，所以不签发
// Mcp-Session-Id（规范允许）。也没有服务端主动推送的消息，所以 GET 按规范回 405
// 而不是挂一条空 SSE。2025-06-18 版规范已经去掉了 JSON-RPC 批量，只收单条。
//
// 安全上有一条是规范明写的：必须校验 Origin，否则任意网页都能 POST 到本机的
// MCP 端点（DNS rebinding）。和 /sessions 一样，这个端点也要认证。
func (s *apiServer) handleMCPPost(w http.ResponseWriter, r *http.Request) int {
	if !s.allowedMCPOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "Origin not allowed"})
		return http.StatusForbidden
	}
	if !s.checkAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return http.StatusUnauthorized
	}

	var req rpcRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMCPBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "parse error: " + err.Error()},
		})
		return http.StatusBadRequest
	}

	result, rpcErr := s.mcp.dispatch(req.Method, req.Params)
	if len(req.ID) == 0 {
		// 通知没有 id，按规范不回包，只确认收到
		w.WriteHeader(http.StatusAccepted)
		return http.StatusAccepted
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	// JSON-RPC 层面的错误仍然是一次成功的 HTTP 交互，状态码保持 200
	writeJSON(w, http.StatusOK, resp)
	return http.StatusOK
}

// allowedMCPOrigin 防 DNS rebinding：带 Origin 的请求来自浏览器，
// 原生 MCP 客户端不会带这个头。所以「没有 Origin」放行，「有 Origin」
// 必须和 --cors-origin 对得上，否则拒掉。
func (s *apiServer) allowedMCPOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return s.corsOrigin == "*" || (s.corsOrigin != "" && s.corsOrigin == origin)
}
