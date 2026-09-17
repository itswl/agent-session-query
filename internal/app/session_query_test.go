package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

// setHome 把 home 目录指到临时目录。
// Windows 上 os.UserHomeDir() 读的是 %USERPROFILE% 而不是 $HOME，
// 两个都设，测试才真的被隔离在临时目录里。
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

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
	// 小写形式在建记录时就算好了（record.lowerSID / lowerKey）
	r := newRecord(map[string]any{"sessionId": "abc-def", "key": "/r/proj/abc-def.jsonl"}, "")
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
		if got := r.matchRank(c.pattern); got != c.want {
			t.Errorf("matchRank(%q) = %d, want %d", c.pattern, got, c.want)
		}
	}

	// key 里没有、只有 sessionId 里有 → 4
	r2 := newRecord(map[string]any{"sessionId": "uniq-sid-9", "key": "/r/other/file.jsonl"}, "")
	if got := r2.matchRank("sid-9"); got != 4 {
		t.Errorf("matchRank(sid-9) = %d, want 4", got)
	}

	// 大小写不敏感：pattern 传进来时已经是小写的
	upper := newRecord(map[string]any{"sessionId": "ABC-DEF", "key": "/R/P.jsonl"}, "")
	if got := upper.matchRank("abc-def"); got != 0 {
		t.Errorf("大小写不敏感匹配 = %d, want 0", got)
	}
}

// TestRecordSortAcrossFormats：跨源排序按解析出来的时间，不是字典序。
// Gemini 写 RFC3339Nano、文件源用 mtime 派生的形态、OpenClaw 是 epoch 毫秒，
// 只比字符串的话带时区偏移的那个会排到完全错误的位置。
func TestRecordSortAcrossFormats(t *testing.T) {
	records := []record{
		newRecord(map[string]any{"key": "mtime"}, "2026-09-14T03:16:50"),        // UTC
		newRecord(map[string]any{"key": "offset"}, "2026-09-14T11:20:00+08:00"), // = 03:20 UTC，最新
		newRecord(map[string]any{"key": "nano"}, "2026-09-14T03:16:50.601Z"),    //
		newRecord(map[string]any{"key": "epochms"}, float64(1789197000000)),     // 2026-09-14T02:30 UTC
		newRecord(map[string]any{"key": "bad"}, "看不懂的时间"),                       // 解析不出来 → 垫底
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].newerThan(records[j]) })

	want := []string{"offset", "nano", "mtime", "epochms", "bad"}
	for i, key := range want {
		if got := records[i].str("key"); got != key {
			t.Fatalf("第 %d 位 = %q, want %q（完整顺序 %v）", i, got, key, keysOf(records))
		}
	}
}

func keysOf(records []record) []string {
	out := []string{}
	for _, r := range records {
		out = append(out, r.str("key"))
	}
	return out
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
	sessions, etag := api.listSessions()
	if len(sessions) != 2 || sessions[0]["sessionId"] != "new" {
		t.Fatalf("sessions = %v", sessions)
	}
	if etag == "" {
		t.Fatal("应当有 ETag")
	}
	// 缓存生效期内新增文件不会出现；TTL=0 时立即可见
	write(t, filepath.Join(root, "p3", "third.jsonl"), `{"type":"session","id":"third"}`)
	if got, again := api.listSessions(); len(got) != 2 || again != etag {
		t.Fatalf("缓存应命中，得到 %d 条 etag=%s", len(got), again)
	}
	uncached := newSessionQueryAPI([]SessionSource{newPiSource(root)}, 0)
	got, newETag := uncached.listSessions()
	if len(got) != 3 {
		t.Fatalf("TTL=0 应立即看到 3 条，得到 %d", len(got))
	}
	if newETag == etag {
		t.Fatal("列表变了，ETag 也该变")
	}
}

// TestPublicIsACopy：public() 必须给副本——记录被列表缓存长期持有，
// 交出去的 map 被改一下，后面所有读者拿到的都是脏数据。
func TestPublicIsACopy(t *testing.T) {
	r := newRecord(map[string]any{"sessionId": "s", "status": "done"}, "")
	out := r.public()
	out["status"] = "tampered"
	if r.str("status") != "done" {
		t.Fatalf("内部字段被改成了 %q", r.str("status"))
	}
}

// TestFileRecordCacheReusesUnchanged：文件没变就不该再解析一次文件头。
func TestFileRecordCacheReusesUnchanged(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "p", "a.jsonl")
	write(t, path, `{"type":"session","id":"cached"}`)

	cache := newFileRecordCache()
	builds := 0
	build := func(p, modISO string) record {
		builds++
		return newRecord(map[string]any{"key": p, "sessionId": "cached"}, modISO)
	}

	if got := cache.records([]string{path}, build); len(got) != 1 || builds != 1 {
		t.Fatalf("首次 = %d 条 / %d 次解析", len(got), builds)
	}
	if got := cache.records([]string{path}, build); len(got) != 1 || builds != 1 {
		t.Fatalf("文件没变却又解析了一次：%d 次", builds)
	}

	// 内容变了（大小变化）就要重新解析
	write(t, path, `{"type":"session","id":"cached"}`, `{"type":"message"}`)
	if cache.records([]string{path}, build); builds != 2 {
		t.Fatalf("文件变了应重新解析，实际 %d 次", builds)
	}

	// 文件没了就不再列出，缓存条目也要清掉
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := cache.records([]string{path}, build); len(got) != 0 {
		t.Fatalf("文件已删除仍列出 %d 条", len(got))
	}
	if len(cache.entries) != 0 {
		t.Fatalf("缓存没清干净: %v", cache.entries)
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
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: api, token: token, maxConnections: 50,
	}))
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

	// 默认不放任何 CORS 头：不设 token 时是免认证的，
	// 一个 Access-Control-Allow-Origin: * 就等于让任何网页都能读走本机会话内容
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("默认不该有 CORS 头: %d %v", resp.StatusCode, resp.Header)
	}

	code, _ := get(t, srv.URL+"/sessions", "")
	if code != 200 {
		t.Fatalf("GET /sessions = %d", code)
	}
	resp, err = http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("数据端点默认不该有 CORS 头，得到 %q", got)
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

// TestHTTPCORSOptIn：--cors-origin 显式配了才放头，并且允许带 Authorization 预检
func TestHTTPCORSOptIn(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "a.jsonl"), `{"type":"session","id":"s"}`)
	sources := []SessionSource{newPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2),
		token: "secret", corsOrigin: "https://ops.example", maxConnections: 50,
	}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://ops.example" {
		t.Fatalf("ACAO = %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("预检要放行 Authorization，否则跨域根本用不了 token: %v", resp.Header)
	}
	if resp.Header.Get("Vary") != "Origin" {
		t.Fatalf("按具体 origin 放行时要带 Vary: Origin，得到 %q", resp.Header.Get("Vary"))
	}
}

// TestHTTPLimitClamped：?limit= 必须夹在上限内，不然一个请求就能把内存打爆
func TestHTTPLimitClamped(t *testing.T) {
	root := t.TempDir()
	lines := []string{`{"type":"session","id":"big"}`}
	for i := 0; i < 20; i++ {
		lines = append(lines, `{"type":"message","id":"m","message":{"role":"user","content":[{"type":"text","text":"x"}]}}`)
	}
	write(t, filepath.Join(root, "p", "big.jsonl"), lines...)
	sources := []SessionSource{newPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2),
		maxConnections: 50, maxLimit: 5,
	}))
	t.Cleanup(srv.Close)

	if _, body := get(t, srv.URL+"/sessions/big/messages?limit=99999999", ""); body["total"] != float64(5) {
		t.Fatalf("limit 未夹到上限 5: %v", body["total"])
	}
	// 负数与 0 仍然是「什么都不要」，与原行为一致
	if _, body := get(t, srv.URL+"/sessions/big/messages?limit=-1", ""); body["total"] != float64(0) {
		t.Fatalf("limit=-1 应返回 0 条: %v", body["total"])
	}
}

// TestHTTPSessionsETag：列表没变时轮询应当在 304 结束
func TestHTTPSessionsETag(t *testing.T) {
	srv, _ := newTestServer(t, "secret")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("/sessions 应当带 ETag")
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("If-None-Match", etag)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("同一个 ETag 应返回 304，得到 %d", resp.StatusCode)
	}

	// ETag 对不上就照常返回完整列表
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("If-None-Match", `W/"deadbeef"`)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ETag 不匹配应返回 200，得到 %d", resp.StatusCode)
	}
}

func TestBuildSourcesModes(t *testing.T) {
	// 未检测到任何数据源时回退 OpenClaw（all 与 auto 都一样）
	home := t.TempDir()
	setHome(t, home)
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

// TestAcceptQueueDepth：--accept-queue 显式指定就用它，0 表示按 max-connections 自动取
func TestAcceptQueueDepth(t *testing.T) {
	cases := []struct{ maxConns, queue, want int }{
		{50, 0, 100},  // 2 倍
		{2, 0, 32},    // 太小，用下限
		{200, 0, 400}, // 2 倍
		{2, 5, 5},     // 显式指定，不再兜底到下限
		{50, 1, 1},    // 显式压到最小
	}
	for _, c := range cases {
		if got := acceptQueueDepth(c.maxConns, c.queue); got != c.want {
			t.Errorf("acceptQueueDepth(%d, %d) = %d, want %d", c.maxConns, c.queue, got, c.want)
		}
	}
}

// TestMessageSink：取最早 N 条要能提前叫停，取最新 N 条要滚到末尾且内存只跟 limit 走
func TestMessageSink(t *testing.T) {
	feed := func(sink *messageSink, n int) int {
		fed := 0
		for i := 0; i < n; i++ {
			fed++
			if !sink.add(map[string]any{"id": i}) {
				break
			}
		}
		return fed
	}
	ids := func(items []map[string]any) []int {
		out := []int{}
		for _, m := range items {
			out = append(out, m["id"].(int))
		}
		return out
	}

	// 最早 3 条：喂到第 3 条就该收手
	head := newMessageSink(messageQuery{limit: 3})
	if fed := feed(head, 100); fed != 3 {
		t.Fatalf("取最早 N 条应在第 3 条停下，实际喂了 %d 条", fed)
	}
	if got := ids(head.result()); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("最早 3 条 = %v", got)
	}

	// 最新 3 条：要一路扫完，结果按时间先后排
	tail := newMessageSink(messageQuery{limit: 3, fromEnd: true})
	if fed := feed(tail, 100); fed != 100 {
		t.Fatalf("取最新 N 条必须扫完，实际只喂了 %d 条", fed)
	}
	if got := ids(tail.result()); !reflect.DeepEqual(got, []int{97, 98, 99}) {
		t.Fatalf("最新 3 条 = %v", got)
	}
	if len(tail.items) != 3 {
		t.Fatalf("环形缓冲应只留 3 条，实际 %d 条", len(tail.items))
	}

	// 条数不够 limit 时原样返回
	few := newMessageSink(messageQuery{limit: 10, fromEnd: true})
	feed(few, 2)
	if got := ids(few.result()); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("不足 limit 时 = %v", got)
	}
	// limit=0 一条都不收
	zero := newMessageSink(messageQuery{limit: 0})
	if fed := feed(zero, 5); fed != 1 || len(zero.result()) != 0 {
		t.Fatalf("limit=0 应立即停且为空: fed=%d len=%d", fed, len(zero.result()))
	}
}

// TestHTTPMessagesOrder：?order=desc 取最新的 N 条（会话最有价值的是结尾）
func TestHTTPMessagesOrder(t *testing.T) {
	root := t.TempDir()
	lines := []string{`{"type":"session","id":"long"}`}
	for i := 0; i < 10; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"type":"message","id":"m%d","message":{"role":"user","content":[{"type":"text","text":"第%d条"}]}}`, i, i))
	}
	write(t, filepath.Join(root, "p", "long.jsonl"), lines...)
	sources := []SessionSource{newPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)

	firstID := func(body map[string]any) string {
		msgs := body["messages"].([]any)
		return msgs[0].(map[string]any)["id"].(string)
	}
	lastID := func(body map[string]any) string {
		msgs := body["messages"].([]any)
		return msgs[len(msgs)-1].(map[string]any)["id"].(string)
	}

	_, asc := get(t, srv.URL+"/sessions/long/messages?limit=3", "")
	if asc["order"] != "asc" || firstID(asc) != "m0" || lastID(asc) != "m2" {
		t.Fatalf("默认应取最早 3 条: %v", asc)
	}
	_, desc := get(t, srv.URL+"/sessions/long/messages?limit=3&order=desc", "")
	if desc["order"] != "desc" || firstID(desc) != "m7" || lastID(desc) != "m9" {
		t.Fatalf("order=desc 应取最新 3 条、且仍按时间先后排: %v", desc)
	}
	// 条数不够时两头一样
	_, all := get(t, srv.URL+"/sessions/long/messages?limit=50&order=desc", "")
	if all["total"] != float64(10) || firstID(all) != "m0" {
		t.Fatalf("limit 大于总数时应给全部: %v", all)
	}
}

// TestHTTPHealthAuthRequired：页面据此决定要不要弹令牌框
func TestHTTPHealthAuthRequired(t *testing.T) {
	withToken, _ := newTestServer(t, "secret")
	if _, body := get(t, withToken.URL+"/health", ""); body["authRequired"] != true {
		t.Fatalf("配了 token 应报 authRequired=true: %v", body)
	}
	open, _ := newTestServer(t, "")
	if _, body := get(t, open.URL+"/health", ""); body["authRequired"] != false {
		t.Fatalf("没配 token 应报 authRequired=false: %v", body)
	}
}

// TestMatchRankPathSeparators：Windows 上 key 是 C:\...\abc.jsonl，
// 用户却习惯敲 proj/abc.jsonl。两种分隔符都得认，否则「后缀精确命中」会掉成「子串命中」，
// 跨源查询时可能被别的源的模糊命中抢先。
func TestMatchRankPathSeparators(t *testing.T) {
	winKey := `C:\Users\dev\.claude\projects\proj\abc-def.jsonl`
	rec := newRecord(map[string]any{"sessionId": "abc-def", "key": winKey}, "")

	cases := []struct {
		pattern string
		want    int
	}{
		{"abc-def", 0}, // 精确 sessionId
		{winKey, 1},    // 原样贴 Windows 路径
		{"c:/users/dev/.claude/projects/proj/abc-def.jsonl", 1}, // 正斜杠写法等价
		{"proj/abc-def.jsonl", 2},                               // 后缀命中，不该掉成 3
		{`proj\abc-def.jsonl`, 2},                               // 反斜杠写法同样命中
		{"projects", 3},                                         // 子串
		{"zzz", -1},
	}
	for _, c := range cases {
		// findSession 对 pattern 做的就是这一步
		if got := rec.matchRank(normalizeForMatch(c.pattern)); got != c.want {
			t.Errorf("matchRank(%q) = %d, want %d", c.pattern, got, c.want)
		}
	}
}

// TestSQLiteURIWindowsPath：反斜杠塞进 SQLite 的 file: URI 会有转义歧义
func TestSQLiteURIWindowsPath(t *testing.T) {
	got := sqliteURI(`C:\Users\dev\.hermes\state.db`)
	if runtime.GOOS == "windows" {
		if got != "file:C:/Users/dev/.hermes/state.db" {
			t.Fatalf("Windows 路径应换成正斜杠: %q", got)
		}
		return
	}
	// 非 Windows 上反斜杠是合法文件名字符，filepath.ToSlash 不该动它
	if got != `file:C:\Users\dev\.hermes\state.db` {
		t.Fatalf("非 Windows 不应改写路径: %q", got)
	}
}

// TestProjectsAndActive：项目聚合 + 活跃标记。
// 文件型数据源的 status 永远是 done，不按更新时间推断的话，
// 一个正在写的会话和三个月前的会话在列表里长得一模一样。
func TestProjectsAndActive(t *testing.T) {
	root := t.TempDir()
	fresh := filepath.Join(root, "p1", "fresh.jsonl")
	stale := filepath.Join(root, "p2", "stale.jsonl")
	write(t, fresh, `{"type":"session","id":"fresh","cwd":"/w/alpha"}`)
	write(t, stale, `{"type":"session","id":"stale","cwd":"/w/alpha"}`)
	other := filepath.Join(root, "p3", "other.jsonl")
	write(t, other, `{"type":"session","id":"other","cwd":"/w/beta"}`)

	old := time.Now().Add(-3 * time.Hour)
	for _, p := range []string{stale, other} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	api := newSessionQueryAPI([]SessionSource{newPiSource(root)}, 0)

	sessions, _ := api.listSessions()
	active := map[string]bool{}
	for _, s := range sessions {
		active[toStr(s["sessionId"])] = truthy(s["isActive"])
		if toStr(s["project"]) == "" {
			t.Fatalf("有 cwd 的会话应当带 project: %v", s)
		}
	}
	if !active["fresh"] {
		t.Fatal("刚写的会话应当是活跃的")
	}
	if active["stale"] || active["other"] {
		t.Fatalf("三小时前的会话不该算活跃: %v", active)
	}

	projects, ungrouped := api.listProjects()
	if len(projects) != 2 || ungrouped != 0 {
		t.Fatalf("应当归成 2 个项目: %v (ungrouped=%d)", projects, ungrouped)
	}
	// 最近动过的项目排前面
	if projects[0]["project"] != "/w/alpha" || projects[0]["sessions"] != 2 {
		t.Fatalf("第一个项目 = %v", projects[0])
	}
	if projects[0]["shortName"] != "alpha" || !truthy(projects[0]["isActive"]) {
		t.Fatalf("项目字段不对: %v", projects[0])
	}
	if projects[1]["project"] != "/w/beta" || truthy(projects[1]["isActive"]) {
		t.Fatalf("第二个项目 = %v", projects[1])
	}
}

// TestExportMarkdown：导出的 Markdown 要能直接贴进 issue
func TestExportMarkdown(t *testing.T) {
	srv, _ := newTestServer(t, "secret")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sessions/sess-1/export?limit=10", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("export = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	body := readBody(t, resp)
	for _, want := range []string{"# 2026-01-01T00-00-00_abc", "**数据源**：pi", "## 最终结果", "## 消息", "### user", "问题", "答案"} {
		if !strings.Contains(body, want) {
			t.Fatalf("导出里缺 %q:\n%s", want, body)
		}
	}
	// 找不到的会话
	if code, _ := get(t, srv.URL+"/sessions/nope/export", "secret"); code != 404 {
		t.Fatalf("不存在的会话导出应当 404，得到 %d", code)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		`a/b\c:d*e?f"g<h>i|j`: "a-b-c-d-e-f-g-h-i-j",
		"":                    "session",
		"...":                 "session",
		"normal-name":         "normal-name",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHTTPProjects：/projects 端点
func TestHTTPProjects(t *testing.T) {
	srv, _ := newTestServer(t, "secret")
	code, body := get(t, srv.URL+"/projects", "secret")
	if code != 200 {
		t.Fatalf("/projects = %d", code)
	}
	projects := body["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("projects = %v", body)
	}
	if projects[0].(map[string]any)["project"] != "/tmp" {
		t.Fatalf("project = %v", projects[0])
	}
	if code, _ := get(t, srv.URL+"/projects", ""); code != 401 {
		t.Fatal("/projects 应当要认证")
	}
}
