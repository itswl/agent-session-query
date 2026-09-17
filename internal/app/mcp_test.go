package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// mcpRoundTrip 把若干请求喂给 MCP 循环，返回解析好的回包
func mcpRoundTrip(t *testing.T, s *mcpServer, requests ...any) []rpcResponse {
	t.Helper()
	var in bytes.Buffer
	for _, req := range requests {
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		in.Write(raw)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if code := runMCP(s, &in, &out); code != 0 {
		t.Fatalf("runMCP 退出码 %d", code)
	}
	responses := []rpcResponse{}
	dec := json.NewDecoder(&out)
	for dec.More() {
		var resp rpcResponse
		if err := dec.Decode(&resp); err != nil {
			t.Fatal(err)
		}
		responses = append(responses, resp)
	}
	return responses
}

func newMCPServer(t *testing.T) *mcpServer {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "proj-a", "2026-01-01T00-00-00_a.jsonl"),
		`{"type":"session","id":"mcp-1","cwd":"/w/proj-a"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"怎么配 Nginx 反代"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"用 proxy_pass"}]}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	return &mcpServer{api: newSessionQueryAPI(sources, 2), sources: sources, maxLimit: defaultMaxLimit}
}

// toolText 取出 tools/call 回包里的正文，并报告是不是工具级错误
func toolText(t *testing.T, resp rpcResponse) (string, bool) {
	t.Helper()
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result 不是对象: %#v", resp.Result)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	return text, truthy(result["isError"])
}

func TestMCPHandshakeAndTools(t *testing.T) {
	s := newMCPServer(t)
	responses := mcpRoundTrip(t, s,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, // 通知，不该有回包
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
	)
	if len(responses) != 2 {
		t.Fatalf("通知不该有回包，收到 %d 条", len(responses))
	}

	init := responses[0].Result.(map[string]any)
	if init["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("protocolVersion = %v", init["protocolVersion"])
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("没有声明 tools 能力: %v", init["capabilities"])
	}

	tools := responses[1].Result.(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		names[name] = true
		// 每个工具都得有描述和 schema，否则模型不知道怎么调
		if tool["description"] == "" || tool["inputSchema"] == nil {
			t.Fatalf("工具 %s 缺描述或 schema", name)
		}
	}
	for _, want := range []string{"search_sessions", "list_sessions", "list_projects", "get_session", "get_messages"} {
		if !names[want] {
			t.Fatalf("少了工具 %s（有 %v）", want, names)
		}
	}
}

func TestMCPSearchAndMessages(t *testing.T) {
	s := newMCPServer(t)
	call := func(id int, name string, args map[string]any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}}
	}
	responses := mcpRoundTrip(t, s,
		call(1, "search_sessions", map[string]any{"query": "nginx"}),
		call(2, "get_messages", map[string]any{"pattern": "mcp-1", "order": "desc", "limit": 1}),
		call(3, "list_projects", map[string]any{}),
	)

	text, isErr := toolText(t, responses[0])
	if isErr {
		t.Fatalf("搜索报错: %s", text)
	}
	var found map[string]any
	if err := json.Unmarshal([]byte(text), &found); err != nil {
		t.Fatalf("工具返回的不是 JSON: %s", text)
	}
	if found["matched"] != float64(1) {
		t.Fatalf("search matched = %v", found["matched"])
	}

	text, _ = toolText(t, responses[1])
	if !strings.Contains(text, "proxy_pass") || !strings.Contains(text, `"order": "desc"`) {
		t.Fatalf("get_messages desc 应当拿到最后一条: %s", text)
	}

	text, _ = toolText(t, responses[2])
	if !strings.Contains(text, "/w/proj-a") {
		t.Fatalf("list_projects = %s", text)
	}
}

func TestMCPErrors(t *testing.T) {
	s := newMCPServer(t)
	responses := mcpRoundTrip(t, s,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "search_sessions", "arguments": map[string]any{}}},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{"name": "no_such_tool", "arguments": map[string]any{}}},
		map[string]any{"jsonrpc": "2.0", "id": 3, "method": "no/such/method"},
	)

	// 工具级错误走 isError，不走协议错误——模型看得到内容才能改参数重试
	for i, wantFragment := range []string{"query", "no_such_tool"} {
		text, isErr := toolText(t, responses[i])
		if !isErr || !strings.Contains(text, wantFragment) {
			t.Fatalf("第 %d 条应当是 isError 且提到 %q，得到 %q / isError=%v", i, wantFragment, text, isErr)
		}
		if responses[i].Error != nil {
			t.Fatalf("工具错误不该变成 JSON-RPC error: %v", responses[i].Error)
		}
	}
	// 未知方法才是协议错误
	if responses[2].Error == nil || responses[2].Error.Code != -32601 {
		t.Fatalf("未知方法应当返回 -32601，得到 %v", responses[2].Error)
	}
}

// ---------------------------------------------------------------------------
// Streamable HTTP 传输
// ---------------------------------------------------------------------------

func newMCPHTTPServer(t *testing.T, token, corsOrigin string) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_a.jsonl"),
		`{"type":"session","id":"http-1","cwd":"/w/x"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"关于 Nginx"}]}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2),
		token: token, corsOrigin: corsOrigin, maxConnections: 50,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// postMCP 往 /mcp 发一条 JSON-RPC，返回状态码与（可能为空的）响应体
func postMCP(t *testing.T, srv *httptest.Server, body, token, origin string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

func TestMCPHTTPTransport(t *testing.T) {
	srv := newMCPHTTPServer(t, "", "")

	code, body := postMCP(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, "", "")
	if code != 200 {
		t.Fatalf("initialize = %d", code)
	}
	result := body["result"].(map[string]any)
	if result["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}

	// 工具能调通
	_, body = postMCP(t, srv,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"nginx"}}}`, "", "")
	text := body["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"matched": 1`) {
		t.Fatalf("搜索结果 = %s", text)
	}

	// 通知没有 id：按规范回 202、不带正文
	code, body = postMCP(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "", "")
	if code != http.StatusAccepted || len(body) != 0 {
		t.Fatalf("通知应当是 202 空响应，得到 %d %v", code, body)
	}

	// 坏 JSON → -32700
	code, body = postMCP(t, srv, `{ not json`, "", "")
	if code != 400 || body["error"].(map[string]any)["code"] != float64(-32700) {
		t.Fatalf("坏 JSON = %d %v", code, body)
	}

	// 服务端不提供 SSE 流，GET 必须回 405（规范明写），且带 Allow
	resp, err := http.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("GET /mcp = %d Allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}
}

// TestMCPHTTPSecurity：规范要求校验 Origin 防 DNS rebinding——
// 否则用户访问的任意网页都能 POST 到本机的 MCP 端点，把会话内容读走。
func TestMCPHTTPSecurity(t *testing.T) {
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`

	// 配了 token 就必须带
	guarded := newMCPHTTPServer(t, "secret", "")
	if code, _ := postMCP(t, guarded, init, "", ""); code != 401 {
		t.Fatalf("无 token 应当 401，得到 %d", code)
	}
	if code, _ := postMCP(t, guarded, init, "secret", ""); code != 200 {
		t.Fatalf("带 token 应当 200，得到 %d", code)
	}

	// 默认没配 --cors-origin：带任何 Origin 的请求都拒掉（原生 MCP 客户端不带这个头）
	open := newMCPHTTPServer(t, "", "")
	if code, _ := postMCP(t, open, init, "", "https://evil.example"); code != 403 {
		t.Fatalf("跨源 Origin 应当 403，得到 %d", code)
	}
	if code, _ := postMCP(t, open, init, "", ""); code != 200 {
		t.Fatalf("不带 Origin（原生客户端）应当放行，得到 %d", code)
	}

	// 显式放行的 origin 才通得过
	allowed := newMCPHTTPServer(t, "", "https://ops.example")
	if code, _ := postMCP(t, allowed, init, "", "https://ops.example"); code != 200 {
		t.Fatalf("已放行的 origin 应当 200，得到 %d", code)
	}
	if code, _ := postMCP(t, allowed, init, "", "https://evil.example"); code != 403 {
		t.Fatalf("未放行的 origin 应当 403，得到 %d", code)
	}
}
