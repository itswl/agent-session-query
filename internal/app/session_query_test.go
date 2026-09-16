package app

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
// 查询层（数据源各自的测试在 source_*_test.go，SQLite 在 hermes_sqlite_test.go）
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
	// （配了 token 时先撞 401）
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
	// 未检测到任何数据源时回退 OpenClaw（all 与 auto 都一样）
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
