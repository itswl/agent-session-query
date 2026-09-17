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

// mcpRoundTrip feeds a series of requests through the MCP loop and returns the decoded
// replies
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
		t.Fatalf("runMCP exit code %d", code)
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
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"how do I set up an Nginx reverse proxy"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"use proxy_pass"}]}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	return &mcpServer{api: newSessionQueryAPI(sources, 2), sources: sources, maxLimit: defaultMaxLimit}
}

// toolText pulls the body out of a tools/call reply and reports whether it was a
// tool-level error
func toolText(t *testing.T, resp rpcResponse) (string, bool) {
	t.Helper()
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %#v", resp.Result)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	return text, truthy(result["isError"])
}

func TestMCPHandshakeAndTools(t *testing.T) {
	s := newMCPServer(t)
	responses := mcpRoundTrip(t, s,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, // a notification; there should be no reply
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
	)
	if len(responses) != 2 {
		t.Fatalf("a notification should draw no reply, got %d", len(responses))
	}

	init := responses[0].Result.(map[string]any)
	if init["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("protocolVersion = %v", init["protocolVersion"])
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("the tools capability was not declared: %v", init["capabilities"])
	}

	tools := responses[1].Result.(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		names[name] = true
		// Every tool needs a description and a schema, or the model cannot tell how to call it
		if tool["description"] == "" || tool["inputSchema"] == nil {
			t.Fatalf("tool %s is missing its description or schema", name)
		}
	}
	for _, want := range []string{"search_sessions", "list_sessions", "list_projects", "get_session", "get_messages"} {
		if !names[want] {
			t.Fatalf("tool %s is missing (have %v)", want, names)
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
		t.Fatalf("the search errored: %s", text)
	}
	var found map[string]any
	if err := json.Unmarshal([]byte(text), &found); err != nil {
		t.Fatalf("the tool did not return JSON: %s", text)
	}
	if found["matched"] != float64(1) {
		t.Fatalf("search matched = %v", found["matched"])
	}

	text, _ = toolText(t, responses[1])
	if !strings.Contains(text, "proxy_pass") || !strings.Contains(text, `"order": "desc"`) {
		t.Fatalf("get_messages with desc should return the last message: %s", text)
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

	// Tool-level failures travel as isError, not as a protocol error: the model has to see
	// the content to retry with different arguments
	for i, wantFragment := range []string{"query", "no_such_tool"} {
		text, isErr := toolText(t, responses[i])
		if !isErr || !strings.Contains(text, wantFragment) {
			t.Fatalf("reply %d should be isError and mention %q, got %q / isError=%v", i, wantFragment, text, isErr)
		}
		if responses[i].Error != nil {
			t.Fatalf("a tool failure must not become a JSON-RPC error: %v", responses[i].Error)
		}
	}
	// An unknown method is the protocol error
	if responses[2].Error == nil || responses[2].Error.Code != -32601 {
		t.Fatalf("an unknown method should return -32601, got %v", responses[2].Error)
	}
}

// ---------------------------------------------------------------------------
// Streamable HTTP transport
// ---------------------------------------------------------------------------

func newMCPHTTPServer(t *testing.T, token, corsOrigin string) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_a.jsonl"),
		`{"type":"session","id":"http-1","cwd":"/w/x"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"about Nginx"}]}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2),
		token: token, corsOrigin: corsOrigin, maxConnections: 50,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// postMCP sends one JSON-RPC message to /mcp and returns the status plus the (possibly
// empty) response body
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

	// The tools are callable
	_, body = postMCP(t, srv,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"nginx"}}}`, "", "")
	text := body["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"matched": 1`) {
		t.Fatalf("search results = %s", text)
	}

	// A notification has no id: per the spec that is a 202 with no body
	code, body = postMCP(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "", "")
	if code != http.StatusAccepted || len(body) != 0 {
		t.Fatalf("a notification should be an empty 202, got %d %v", code, body)
	}

	// Malformed JSON gives -32700
	code, body = postMCP(t, srv, `{ not json`, "", "")
	if code != 400 || body["error"].(map[string]any)["code"] != float64(-32700) {
		t.Fatalf("malformed JSON = %d %v", code, body)
	}

	// With no SSE stream on offer, GET must return 405 (the spec says so) along with Allow
	resp, err := http.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("GET /mcp = %d Allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}
}

// TestMCPHTTPSecurity: the spec requires validating Origin to prevent DNS rebinding —
// otherwise any web page the user visits could POST to the local MCP endpoint and read the
// session content out.
func TestMCPHTTPSecurity(t *testing.T) {
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`

	// With a token configured, it must be supplied
	guarded := newMCPHTTPServer(t, "secret", "")
	if code, _ := postMCP(t, guarded, init, "", ""); code != 401 {
		t.Fatalf("no token should be 401, got %d", code)
	}
	if code, _ := postMCP(t, guarded, init, "secret", ""); code != 200 {
		t.Fatalf("with a token should be 200, got %d", code)
	}

	// Without --cors-origin, any request carrying an Origin is refused (native MCP clients
	// never send that header)
	open := newMCPHTTPServer(t, "", "")
	if code, _ := postMCP(t, open, init, "", "https://evil.example"); code != 403 {
		t.Fatalf("a cross-origin request should be 403, got %d", code)
	}
	if code, _ := postMCP(t, open, init, "", ""); code != 200 {
		t.Fatalf("no Origin (a native client) should pass, got %d", code)
	}

	// Only an explicitly allowed origin gets through
	allowed := newMCPHTTPServer(t, "", "https://ops.example")
	if code, _ := postMCP(t, allowed, init, "", "https://ops.example"); code != 200 {
		t.Fatalf("an allowed origin should be 200, got %d", code)
	}
	if code, _ := postMCP(t, allowed, init, "", "https://evil.example"); code != 403 {
		t.Fatalf("a disallowed origin should be 403, got %d", code)
	}
}
