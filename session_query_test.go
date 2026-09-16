package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	// 1000 个汉字截 3 个：必须按码点，不能把 UTF-8 切碎
	got := truncate(strings.Repeat("好", 10), 3, "...[truncated]")
	if got != "好好好...[truncated]" {
		t.Fatalf("truncate = %q", got)
	}
	if truncate("abc", 3, "!") != "abc" {
		t.Fatal("长度刚好不应截断")
	}
}

func TestContentText(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"abc", "abc"},
		{map[string]any{"text": "t"}, "t"},
		{map[string]any{"text": 1, "content": "c"}, "c"}, // text 不是字符串就继续找下一个键
		{[]any{map[string]any{"text": "a"}, "b"}, "a\nb"},
		{[]any{"a", ""}, "a"},
		{42, ""},
	}
	for _, c := range cases {
		if got := contentText(c.in); got != c.want {
			t.Errorf("contentText(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatchRank(t *testing.T) {
	f := map[string]any{"sessionId": "abc-def", "key": "/r/proj/abc-def.jsonl"}
	cases := []struct {
		pattern string
		want    int
	}{
		{"abc-def", 0},               // 精确 sessionId
		{"/r/proj/abc-def.jsonl", 1}, // 精确 key
		{"proj/abc-def.jsonl", 2},    // key 后缀
		{"proj", 3},                  // key 子串
		{"c-d", 3},                   // key 里也有：key 子串优先于 sessionId 子串
		{"zzz", -1},
	}
	for _, c := range cases {
		if got := matchRank(c.pattern, f); got != c.want {
			t.Errorf("matchRank(%q) = %d, want %d", c.pattern, got, c.want)
		}
	}

	// key 里没有、只有 sessionId 里有 → 4
	f2 := map[string]any{"sessionId": "uniq-sid-9", "key": "/r/other/file.jsonl"}
	if got := matchRank("sid-9", f2); got != 4 {
		t.Errorf("matchRank(sid-9) = %d, want 4", got)
	}
}

// ---------------------------------------------------------------------------
// 数据源
// ---------------------------------------------------------------------------

func TestPiSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "2026-09-13T13-04-12-594Z_aaaa.jsonl")
	write(t, path,
		`{"type":"session","id":"pi-1","cwd":"/home/imwl/proj"}`,
		`{"type":"message","id":"m1","timestamp":"2026-09-13T13:05:00Z","message":{"role":"user","content":[{"type":"text","text":"你好"}]}}`,
		`{"type":"message","id":"m2","timestamp":"2026-09-13T13:05:05Z","message":{"role":"assistant","stopReason":"stop","model":"gemini-3.8-flash","usage":{"input_tokens":10},"content":[{"type":"thinking","thinking":"想想"},{"type":"text","text":"你好呀"}]}}`,
	)

	s := newPiSource(root)
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}
	if list[0].str("sessionId") != "pi-1" || list[0].str("cwd") != "/home/imwl/proj" {
		t.Fatalf("record = %v", list[0].fields)
	}

	msgs := s.Messages(list[0], 50)
	if len(msgs) != 2 || msgs[0]["role"] != "user" {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["id"] != "m1" || msgs[0]["timestamp"] != "2026-09-13T13:05:00Z" {
		t.Fatalf("msg0 = %v", msgs[0])
	}

	final := s.Final(list[0])
	if final["isFinal"] != true || final["text"] != "你好呀" || final["thinking"] != "想想" {
		t.Fatalf("final = %v", final)
	}
	if final["messageCount"] != 2 {
		t.Fatalf("messageCount = %v", final["messageCount"])
	}

	// limit 取最早的前 N 条
	if got := s.Messages(list[0], 1); len(got) != 1 || got[0]["id"] != "m1" {
		t.Fatalf("limit=1 -> %v", got)
	}
	if got := s.Messages(list[0], 0); len(got) != 0 {
		t.Fatalf("limit=0 -> %v", got)
	}
}

func TestClaudeSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj-x", "11111111-2222-3333-4444-555555555555.jsonl")
	write(t, path,
		`{"type":"user","uuid":"u1","sessionId":"cccc","cwd":"/home/imwl/proj","timestamp":"2026-09-14T07:14:06.596Z","message":{"role":"user","content":"写一句问候"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-09-14T07:14:07.000Z","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"子代理"}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-09-14T07:14:09.100Z","message":{"id":"msg_1","model":"claude-x","stop_reason":"end_turn","usage":{"input_tokens":5},"content":[{"type":"thinking","thinking":"想"},{"type":"text","text":"您好"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-09-14T07:14:10.100Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"file1"}]}]}}`,
	)

	s := newClaudeSource(root)
	list := s.List()
	if len(list) != 1 || list[0].str("sessionId") != "cccc" || list[0].str("cwd") != "/home/imwl/proj" {
		t.Fatalf("list = %v", list)
	}

	msgs := s.Messages(list[0], 50)
	if len(msgs) != 3 { // sidechain 那条不算
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["id"] != "u1" || msgs[0]["role"] != "user" {
		t.Fatalf("msg0 = %v", msgs[0])
	}
	toolMsg := msgs[2]["content"].([]map[string]any)[0]
	if toolMsg["type"] != "toolResult" || toolMsg["content"] != "file1" {
		t.Fatalf("toolResult = %v", toolMsg)
	}

	final := s.Final(list[0])
	if final["isFinal"] != true || final["text"] != "您好" || final["thinking"] != "想" {
		t.Fatalf("final = %v", final)
	}
	if final["stopReason"] != "end_turn" || final["id"] != "msg_1" {
		t.Fatalf("final = %v", final)
	}
	calls := final["toolCalls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["name"] != "Bash" {
		t.Fatalf("toolCalls = %v", calls)
	}
}

func TestCodexSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "2026", "09", "13", "rollout-2026-09-13T23-07-05-abc.jsonl")
	write(t, path,
		`{"type":"session_meta","payload":{"session_id":"codex-1","cwd":"/home/imwl/x","cli_version":"0.154.0"}}`,
		`{"type":"response_item","timestamp":"t0","payload":{"type":"message","role":"developer","content":[{"text":"系统"}]}}`,
		`{"type":"response_item","timestamp":"t1","payload":{"type":"message","role":"user","id":"c1","content":[{"text":"问题"}]}}`,
		`{"type":"response_item","timestamp":"t2","payload":{"type":"message","role":"assistant","id":"c2","stop_reason":"stop","content":[{"text":"回答"}]}}`,
		`{"type":"token_usage_record","payload":{"usage":{"input_tokens":7}}}`,
	)

	s := newCodexSource(root)
	list := s.List()
	if len(list) != 1 || list[0].str("sessionId") != "codex-1" || list[0].str("cliVersion") != "0.154.0" {
		t.Fatalf("list = %v", list)
	}

	msgs := s.Messages(list[0], 50)
	if len(msgs) != 2 { // developer 那条不算
		t.Fatalf("messages = %v", msgs)
	}
	final := s.Final(list[0])
	if final["text"] != "回答" || final["isFinal"] != true {
		t.Fatalf("final = %v", final)
	}
	if usage := final["usage"].(map[string]any); usage["input_tokens"] != float64(7) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestGeminiSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "projA", "chats", "session-2026-09-13T12-50-41.jsonl")
	write(t, path,
		`{"sessionId":"g-1","startTime":"2026-09-13T12:50:41Z","lastUpdated":"2026-09-13T12:55:00Z"}`,
		`{"$set":{"messages":[{"type":"user","id":"gu1","timestamp":"t1","content":[{"text":"嗨"}]}]}}`,
		`{"type":"gemini","id":"gg1","timestamp":"t2","model":"gemini-2.5","thoughts":"琢磨","tokens":{"input":3},"content":"你好"}`,
	)

	s := newGeminiSource(root)
	list := s.List()
	if len(list) != 1 || list[0].str("sessionId") != "g-1" || list[0].str("project") != "projA" {
		t.Fatalf("list = %v", list[0].fields)
	}
	if list[0].str("updatedAt") != "2026-09-13T12:55:00Z" { // 用元数据时间，不扫全文件
		t.Fatalf("updatedAt = %v", list[0].str("updatedAt"))
	}

	msgs := s.Messages(list[0], 50)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[1]["role"] != "assistant" {
		t.Fatalf("msg1 = %v", msgs[1])
	}
	// thoughts 作为 thinking 插在最前面
	parts := msgs[1]["content"].([]map[string]any)
	if parts[0]["type"] != "thinking" || parts[0]["content"] != "琢磨" {
		t.Fatalf("parts = %v", parts)
	}

	final := s.Final(list[0])
	if final["text"] != "你好" || final["stopReason"] != "stop" || final["isFinal"] != true {
		t.Fatalf("final = %v", final)
	}
}

func TestJsonMapOpenClaw(t *testing.T) {
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "oc-1.jsonl")
	write(t, sessionFile,
		`{"type":"message","id":"om1","timestamp":"2026-09-13T15:00:00Z","stopReason":"stop","message":{"role":"assistant","content":[{"type":"text","text":"完成"},{"type":"thinking","thinking":"推理"}]},"usage":{"input_tokens":11}}`,
	)
	write(t, filepath.Join(dir, "sessions.json"),
		`{"agent:default:hook:alert:prometheus:b5123b01":{"sessionId":"oc-1","sessionFile":"`+sessionFile+`","updatedAt":1789489016671,"status":"done","model":"m","runtimeMs":123,"totalTokens":9}}`,
	)

	def := openclawDef(dir)
	def.sessionsJSON = filepath.Join(dir, "sessions.json")
	def.sessionsDir = dir
	s := newJsonMapSource(def)

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	if list[0].str("shortKey") != "hook:alert:prometheus:b5123b01" {
		t.Fatalf("shortKey = %v", list[0].str("shortKey"))
	}
	if list[0].str("updatedAt") != "2026-09-15 16:16:56" { // 毫秒时间戳 → UTC，空格分隔
		t.Fatalf("updatedAt = %v", list[0].str("updatedAt"))
	}
	if list[0].get("runtimeMs") != float64(123) || list[0].get("totalTokens") != float64(9) {
		t.Fatalf("record = %v", list[0].fields)
	}
	if list[0].get("file") == nil || list[0].get("hasFile") != true {
		t.Fatalf("file = %v", list[0].fields["file"])
	}

	final := s.Final(list[0])
	if final["isFinal"] != true || final["text"] != "完成" || final["thinking"] != "推理" {
		t.Fatalf("final = %v", final)
	}
	if final["stopReason"] != "stop" || final["messageCount"] != 1 {
		t.Fatalf("final = %v", final)
	}

	// 文件不存在时：hasFile=false / file=null，final 返回带 error 的兜底
	write(t, filepath.Join(dir, "sessions.json"),
		`{"k2":{"sessionId":"missing","updatedAt":0}}`,
	)
	list = s.List()
	if len(list) != 1 || list[0].get("file") != nil || list[0].get("hasFile") != false {
		t.Fatalf("list = %v", list[0].fields)
	}
	final = s.Final(list[0])
	if final["isFinal"] != false || final["error"] == nil {
		t.Fatalf("final = %v", final)
	}
}

func TestJsonMapHermes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "h-1.jsonl"),
		`{"role":"assistant","id":"hm1","timestamp":"2026-09-13T10:00:00Z","finish_reason":"stop","content":"干完了","reasoning":"推理"}`,
	)
	write(t, filepath.Join(dir, "sessions.json"),
		`{"hook:task:x":{"session_id":"h-1","updated_at":"2026-09-13T10:00:00Z","created_at":"2026-09-13T09:00:00Z","display_name":"演示","platform":"feishu","total_tokens":100,"estimated_cost_usd":0.01}}`,
	)

	def := hermesDef(dir)
	def.sessionsJSON = filepath.Join(dir, "sessions.json")
	def.sessionsDir = dir
	s := newJsonMapSource(def)

	list := s.List()
	if len(list) != 1 || list[0].str("status") != "done" || list[0].str("displayName") != "演示" {
		t.Fatalf("list = %v", list[0].fields)
	}
	msgs := s.Messages(list[0], 50)
	if len(msgs) != 1 || msgs[0]["timestamp"] != "2026-09-13T10:00:00Z" {
		t.Fatalf("messages = %v", msgs)
	}
	parts := msgs[0]["content"].([]map[string]any)
	if parts[0]["type"] != "thinking" || parts[1]["content"] != "干完了" {
		t.Fatalf("parts = %v", parts)
	}
	final := s.Final(list[0])
	if final["text"] != "干完了" || final["thinking"] != "推理" || final["isFinal"] != true {
		t.Fatalf("final = %v", final)
	}
}

// ---------------------------------------------------------------------------
// 查询层
// ---------------------------------------------------------------------------

func TestFindSessionPrecedence(t *testing.T) {
	dir := t.TempDir()
	piRoot := filepath.Join(dir, "pi")
	write(t, filepath.Join(piRoot, "p", "2026-01-01T00-00-00_a.jsonl"),
		`{"type":"session","id":"shared-id"}`,
	)
	claudeRoot := filepath.Join(dir, "claude")
	write(t, filepath.Join(claudeRoot, "c", "session-x.jsonl"),
		`{"type":"user","uuid":"u","sessionId":"shared-id","timestamp":"t","message":{"role":"user","content":"hi"}}`,
	)

	api := newSessionQueryAPI([]SessionSource{
		newPiSource(piRoot),
		newClaudeSource(claudeRoot),
	}, 2)

	// 两个源都能精确匹配同一个 sessionId 时，取数据源顺序在前的
	source, rec, ok := api.findSession("shared-id")
	if !ok || source.Mode() != "pi" || rec.str("sessionId") != "shared-id" {
		t.Fatalf("find = %v %v %v", source, rec.fields, ok)
	}

	// 模糊命中不能盖过另一个源里的精确命中
	write(t, filepath.Join(claudeRoot, "c", "session-y.jsonl"),
		`{"type":"user","uuid":"u","sessionId":"deadbeef-shared-id-x","timestamp":"t","message":{"role":"user","content":"hi"}}`,
	)
	source, rec, _ = api.findSession("shared-id")
	if source.Mode() != "pi" {
		t.Fatalf("精确命中应优先，得到 %s %v", source.Mode(), rec.fields)
	}

	// "Session: " 前缀会被剥掉
	if _, _, ok := api.findSession("Session: shared-id"); !ok {
		t.Fatal("Session: 前缀未生效")
	}
	if _, _, ok := api.findSession("   "); ok {
		t.Fatal("空 pattern 不应命中")
	}
}

func TestListSortAndCache(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "p1", "old.jsonl")
	write(t, old, `{"type":"session","id":"old"}`)
	write(t, filepath.Join(root, "p2", "new.jsonl"), `{"type":"session","id":"new"}`)
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	api := newSessionQueryAPI([]SessionSource{newPiSource(root)}, 60)
	sessions := api.listSessions()
	if len(sessions) != 2 || sessions[0]["sessionId"] != "new" {
		t.Fatalf("sessions = %v", sessions)
	}
	// 缓存生效期内新增文件不会出现；TTL=0 时立即可见
	write(t, filepath.Join(root, "p3", "third.jsonl"), `{"type":"session","id":"third"}`)
	if got := api.listSessions(); len(got) != 2 {
		t.Fatalf("缓存应命中，得到 %d 条", len(got))
	}
	uncached := newSessionQueryAPI([]SessionSource{newPiSource(root)}, 0)
	if got := uncached.listSessions(); len(got) != 3 {
		t.Fatalf("TTL=0 应立即看到 3 条，得到 %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// HTTP 层
// ---------------------------------------------------------------------------

func newTestServerFixture(t *testing.T, token, sessionID string) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	sessionPath := filepath.Join(root, "p", "2026-01-01T00-00-00_abc.jsonl")
	write(t, sessionPath,
		`{"type":"session","id":"`+sessionID+`","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"问题"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"答案"}]}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	api := newSessionQueryAPI(sources, 2)
	srv := httptest.NewServer(newAPIServer("auto", sources, api, token, 50))
	t.Cleanup(srv.Close)
	return srv, sessionPath
}

func newTestServer(t *testing.T, token string) (*httptest.Server, string) {
	t.Helper()
	return newTestServerFixture(t, token, "sess-1")
}

func get(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestHTTPRoutes(t *testing.T) {
	srv, sessionPath := newTestServer(t, "secret")

	// 免认证端点
	if code, body := get(t, srv.URL+"/health", ""); code != 200 || body["status"] != "ok" {
		t.Fatalf("/health = %d %v", code, body)
	}
	if code, _ := get(t, srv.URL+"/", ""); code != 200 {
		t.Fatalf("/ = %d", code)
	}
	// /api 前缀只对 /sessions 系列生效：/api/health 会落到「认证之后」的 404
	// （配了 token 时先撞 401，这与 Python 版的判定顺序一致）
	if code, _ := get(t, srv.URL+"/api/health", ""); code != 401 {
		t.Fatalf("/api/health (带 token 配置) = %d", code)
	}

	// 认证
	if code, body := get(t, srv.URL+"/sessions", ""); code != 401 || body["error"] != "Unauthorized" {
		t.Fatalf("无 token = %d %v", code, body)
	}
	if code, _ := get(t, srv.URL+"/sessions", "wrong"); code != 401 {
		t.Fatalf("错误 token = %d", code)
	}

	// 列表
	code, body := get(t, srv.URL+"/sessions", "secret")
	if code != 200 || body["total"] != float64(1) {
		t.Fatalf("/sessions = %d %v", code, body)
	}
	sessions := body["sessions"].([]any)
	first := sessions[0].(map[string]any)
	if first["sessionId"] != "sess-1" || first["source"] != "pi" {
		t.Fatalf("session = %v", first)
	}

	// /api 前缀等价
	if code, body := get(t, srv.URL+"/api/sessions", "secret"); code != 200 || body["total"] != float64(1) {
		t.Fatalf("/api/sessions = %d %v", code, body)
	}

	// 单个会话（含 file 字段）
	if code, body := get(t, srv.URL+"/sessions/sess-1", "secret"); code != 200 || body["file"] != sessionPath {
		t.Fatalf("/sessions/sess-1 = %d %v", code, body)
	}

	// 消息 + limit
	code, body = get(t, srv.URL+"/sessions/sess-1/messages?limit=1", "secret")
	if code != 200 || body["total"] != float64(1) {
		t.Fatalf("messages = %d %v", code, body)
	}
	msgs := body["messages"].([]any)
	if msgs[0].(map[string]any)["id"] != "m1" { // limit 取最早的前 N 条
		t.Fatalf("messages[0] = %v", msgs[0])
	}
	if _, body := get(t, srv.URL+"/sessions/sess-1/messages?limit=abc", "secret"); body["total"] != float64(2) {
		t.Fatalf("非法 limit 应回退 50: %v", body)
	}

	// 最终结果
	code, body = get(t, srv.URL+"/sessions/sess-1/final", "secret")
	if code != 200 || body["text"] != "答案" || body["isFinal"] != true {
		t.Fatalf("final = %d %v", code, body)
	}

	// 找不到 / 未知路径
	if code, body := get(t, srv.URL+"/sessions/nope", "secret"); code != 404 || body["error"] != "Session not found" {
		t.Fatalf("404 = %d %v", code, body)
	}
	if code, body := get(t, srv.URL+"/whatever", "secret"); code != 404 || body["error"] != "Not found" {
		t.Fatalf("未知路径 = %d %v", code, body)
	}

	// 不配 token 时 /api/health 是 404（免认证端点没有 /api 别名）
	openSrv, _ := newTestServer(t, "")
	if code, _ := get(t, openSrv.URL+"/api/health", ""); code != 404 {
		t.Fatalf("/api/health (无 token) = %d", code)
	}
}

func TestHTTPEncodedPattern(t *testing.T) {
	// 含冒号的 pattern 要 URL 编码（%3A），解码后再去匹配
	srv, _ := newTestServerFixture(t, "secret", "hook:alert:x")
	code, body := get(t, srv.URL+"/sessions/hook%3Aalert%3Ax", "secret")
	if code != 200 || body["sessionId"] != "hook:alert:x" {
		t.Fatalf("编码 pattern = %d %v", code, body)
	}
}

func TestHTTPOptionsAndMethods(t *testing.T) {
	srv, _ := newTestServer(t, "")
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("OPTIONS = %d %v", resp.StatusCode, resp.Header)
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/sessions", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("POST = %d", resp.StatusCode)
	}
}

func TestBuildSourcesModes(t *testing.T) {
	// 未检测到任何数据源时回退 OpenClaw（all 与 auto 都一样，与 Python 版一致）
	home := t.TempDir()
	t.Setenv("HOME", home)
	if sources, err := buildSources("auto"); err != nil || len(sources) != 1 || sources[0].Mode() != "openclaw" {
		t.Fatalf("auto 回退 = %v %v", sources, err)
	}
	if sources, err := buildSources("all"); err != nil || len(sources) != 1 || sources[0].Mode() != "openclaw" {
		t.Fatalf("all 回退 = %v %v", sources, err)
	}

	// 建出 pi / claude 两个源：auto 只启用存在的，all 同样只启用存在的
	write(t, filepath.Join(home, ".pi", "agent", "sessions", "p", "x.jsonl"), `{"type":"session","id":"x"}`)
	write(t, filepath.Join(home, ".claude", "projects", "p", "y.jsonl"), `{"type":"user","uuid":"u","message":{"role":"user","content":"hi"}}`)
	if sources, err := buildSources("auto"); err != nil || len(sources) != 2 ||
		sources[0].Mode() != "pi" || sources[1].Mode() != "claude" {
		t.Fatalf("auto = %v %v", sources, err)
	}
	if sources, err := buildSources("all"); err != nil || len(sources) != 2 {
		t.Fatalf("all = %v %v", sources, err)
	}

	// 单模式：不管目录存不存在都启用
	if sources, err := buildSources("pi"); err != nil || len(sources) != 1 || sources[0].Mode() != "pi" {
		t.Fatalf("pi = %v %v", sources, err)
	}
	if _, err := buildSources("nope"); err == nil {
		t.Fatal("未知模式应当报错")
	}
}
