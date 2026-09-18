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

// newMCPWindowServer builds three sessions at known times (old / mid / recent) for the
// time-window and pagination tests.
func newMCPWindowServer(t *testing.T) *mcpServer {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "w", "2026-01-01T00-00-00_old.jsonl"),
		`{"type":"session","id":"w-old","cwd":"/w/proj"}`,
		`{"type":"message","id":"o1","message":{"role":"user","content":[{"type":"text","text":"an old question"}],"timestamp":"2026-09-01T10:00:00Z"}}`,
	)
	write(t, filepath.Join(root, "w", "2026-01-02T00-00-00_mid.jsonl"),
		`{"type":"session","id":"w-mid","cwd":"/w/proj"}`,
		`{"type":"message","id":"i1","message":{"role":"user","content":[{"type":"text","text":"a question from the middle"}],"timestamp":"2026-09-10T10:00:00Z"}}`,
	)
	write(t, filepath.Join(root, "w", "2026-01-03T00-00-00_new.jsonl"),
		`{"type":"session","id":"w-new","cwd":"/w/proj"}`,
		`{"type":"message","id":"n1","message":{"role":"user","content":[{"type":"text","text":"a question about nginx"}],"timestamp":"2026-09-17T10:00:00Z"}}`,
		`{"type":"message","id":"n2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"an answer"}],"timestamp":"2026-09-17T10:01:00Z"}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	return &mcpServer{api: newSessionQueryAPI(sources, 2), sources: sources, maxLimit: defaultMaxLimit}
}

func mcpCallTool(t *testing.T, s *mcpServer, args map[string]any) map[string]any {
	t.Helper()
	resp := mcpRoundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_sessions", "arguments": args},
	})
	text, isErr := toolText(t, resp[0])
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("tool result is not JSON: %v (%s)", err, text)
	}
	return out
}

func TestMCPAnnotations(t *testing.T) {
	s := newMCPServer(t)
	responses := mcpRoundTrip(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	tools := responses[0].Result.(map[string]any)["tools"].([]any)
	for _, raw := range tools {
		tool := raw.(map[string]any)
		ann := tool["annotations"].(map[string]any)
		if ann["readOnlyHint"] != true {
			t.Errorf("%s: readOnlyHint missing or false: %v", tool["name"], ann)
		}
		if ann["openWorldHint"] != false {
			t.Errorf("%s: openWorldHint must be false: %v", tool["name"], ann)
		}
	}
}

func TestMCPTimeWindow(t *testing.T) {
	s := newMCPWindowServer(t)

	// since=2026-09-15: only the newest session
	out := mcpCallTool(t, s, map[string]any{"since": "2026-09-15"})
	ids := windowIDs(out)
	if len(ids) != 1 || ids[0] != "w-new" {
		t.Fatalf("since=2026-09-15 → %v, want [w-new]", ids)
	}

	// until=2026-09-05: only the oldest
	out = mcpCallTool(t, s, map[string]any{"until": "2026-09-05"})
	ids = windowIDs(out)
	if len(ids) != 1 || ids[0] != "w-old" {
		t.Fatalf("until=2026-09-05 → %v, want [w-old]", ids)
	}

	// a window keeps only what falls inside it
	out = mcpCallTool(t, s, map[string]any{"since": "2026-09-05", "until": "2026-09-15"})
	ids = windowIDs(out)
	if len(ids) != 1 || ids[0] != "w-mid" {
		t.Fatalf("window → %v, want [w-mid]", ids)
	}

	// a bad value is a tool error, not a silent ignore
	resp := mcpRoundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_sessions", "arguments": map[string]any{"since": "not-a-date"}},
	})
	if text, isErr := toolText(t, resp[0]); !isErr {
		t.Fatalf("a bad since must be a tool error, got %s", text)
	}
}

func windowIDs(out map[string]any) []string {
	ids := []string{}
	for _, raw := range out["sessions"].([]any) {
		ids = append(ids, raw.(map[string]any)["sessionId"].(string))
	}
	return ids
}

func TestMCPCursorPagination(t *testing.T) {
	s := newMCPWindowServer(t)

	// Page through three sessions two at a time
	page1 := mcpCallTool(t, s, map[string]any{"limit": 2})
	if ids := windowIDs(page1); len(ids) != 2 {
		t.Fatalf("page 1 → %v", ids)
	}
	cursor, ok := page1["nextCursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("page 1 must carry a nextCursor: %v", page1["nextCursor"])
	}
	page2 := mcpCallTool(t, s, map[string]any{"limit": 2, "cursor": cursor})
	ids2 := windowIDs(page2)
	if len(ids2) != 1 {
		t.Fatalf("page 2 → %v, want the one remaining session", ids2)
	}
	if _, has := page2["nextCursor"]; has {
		t.Fatalf("the last page must carry no nextCursor, got %v", page2["nextCursor"])
	}
	// Pages must not overlap
	for _, id := range windowIDs(page1) {
		if ids2[0] == id {
			t.Fatalf("pages overlap at %s", id)
		}
	}
	// A cursor past the end yields an empty page, not an error
	last := mcpCallTool(t, s, map[string]any{"limit": 2, "cursor": encodeCursor(99)})
	if n := len(last["sessions"].([]any)); n != 0 {
		t.Fatalf("an out-of-range cursor → %d sessions, want 0", n)
	}
}

func TestMCPRoleFilter(t *testing.T) {
	s := newMCPWindowServer(t)
	call := func(args map[string]any) map[string]any {
		t.Helper()
		resp := mcpRoundTrip(t, s, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "get_messages", "arguments": args},
		})
		text, isErr := toolText(t, resp[0])
		if isErr {
			t.Fatalf("tool error: %s", text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := call(map[string]any{"pattern": "w-new", "role": "user"})
	msgs := out["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("role=user → %v", msgs)
	}
	if out["total"] != float64(1) {
		t.Fatalf("total must count the filtered set: %v", out["total"])
	}

	out = call(map[string]any{"pattern": "w-new", "role": "assistant"})
	if len(out["messages"].([]any)) != 1 {
		t.Fatalf("role=assistant → %v", out["messages"])
	}

	// An unknown role is rejected rather than returning everything
	resp := mcpRoundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "get_messages", "arguments": map[string]any{"pattern": "w-new", "role": "system"}},
	})
	if _, isErr := toolText(t, resp[0]); !isErr {
		t.Fatal("role=system must be a tool error")
	}
}

// TestMCPGetMessagesPagination: the earlier implementation decided "another page?" from
// len(messages) after the source had already truncated to limit — always false, and
// nextCursor never appeared. This is the case that regression slipped through.
func TestMCPGetMessagesPagination(t *testing.T) {
	s := newMCPWindowServer(t)
	call := func(args map[string]any) map[string]any {
		t.Helper()
		resp := mcpRoundTrip(t, s, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "get_messages", "arguments": args},
		})
		text, isErr := toolText(t, resp[0])
		if isErr {
			t.Fatalf("tool error: %s", text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Ascending pages: 1 and 2 must be disjoint, consecutive, and a third must not exist
	p1 := call(map[string]any{"pattern": "w-new", "limit": 1, "order": "asc"})
	if len(p1["messages"].([]any)) != 1 {
		t.Fatalf("page 1 size = %d", len(p1["messages"].([]any)))
	}
	cur, ok := p1["nextCursor"].(string)
	if !ok || cur == "" {
		t.Fatal("a full page must carry nextCursor — this is the regression")
	}
	p2 := call(map[string]any{"pattern": "w-new", "limit": 1, "order": "asc", "cursor": cur})
	m1 := p1["messages"].([]any)[0].(map[string]any)
	m2 := p2["messages"].([]any)[0].(map[string]any)
	if m1["id"] == m2["id"] {
		t.Fatalf("pages overlap at %v", m1["id"])
	}
	if m1["timestamp"].(string) > m2["timestamp"].(string) {
		t.Fatalf("ascending pages must move forward: %v then %v", m1["timestamp"], m2["timestamp"])
	}
	if _, has := p2["nextCursor"]; has {
		t.Fatalf("the last page must carry no nextCursor, got %v", p2["nextCursor"])
	}

	// Descending: page 1 is the newest message, page 2 the one before it
	d1 := call(map[string]any{"pattern": "w-new", "limit": 1, "order": "desc"})
	d2 := call(map[string]any{"pattern": "w-new", "limit": 1, "order": "desc", "cursor": d1["nextCursor"].(string)})
	dm1 := d1["messages"].([]any)[0].(map[string]any)
	dm2 := d2["messages"].([]any)[0].(map[string]any)
	if dm1["role"] != "assistant" {
		t.Fatalf("desc page 1 must be the newest (assistant) message, got %v", dm1["role"])
	}
	if dm2["role"] != "user" {
		t.Fatalf("desc page 2 must be the one before it, got %v", dm2["role"])
	}
	if _, has := d2["nextCursor"]; has {
		t.Fatalf("the last desc page must carry no nextCursor")
	}

	// list_sessions must reject an unknown source the same way get_session does
	resp := mcpRoundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_sessions", "arguments": map[string]any{"source": "nope"}},
	})
	if text, isErr := toolText(t, resp[0]); !isErr || !strings.Contains(text, "choose from") {
		t.Fatalf("list_sessions with a bad source must fail with the valid list, got %q", text)
	}
}
