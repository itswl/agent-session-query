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
// Helpers
// ---------------------------------------------------------------------------

// setHome points the home directory at a temp directory.
// os.UserHomeDir() reads %USERPROFILE% on Windows rather than $HOME, so both have to be
// set for the test to be genuinely isolated there.
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
	// Deliberately non-ASCII: truncation must count code points, not bytes, and must never
	// slice a UTF-8 sequence in half
	got := truncate(strings.Repeat("好", 10), 3, "...[truncated]")
	if got != "好好好...[truncated]" {
		t.Fatalf("truncate = %q", got)
	}
	if truncate("abc", 3, "!") != "abc" {
		t.Fatal("an exact-length string should not be truncated")
	}
}

func TestContentText(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"abc", "abc"},
		{map[string]any{"text": "t"}, "t"},
		{map[string]any{"text": 1, "content": "c"}, "c"}, // a non-string text moves on to the next key
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
	// The lowercased forms are computed when the record is built (record.lowerSID / lowerKey)
	r := newRecord(map[string]any{"sessionId": "abc-def", "key": "/r/proj/abc-def.jsonl"}, "")
	cases := []struct {
		pattern string
		want    int
	}{
		{"abc-def", 0},               // exact sessionId
		{"/r/proj/abc-def.jsonl", 1}, // exact key
		{"proj/abc-def.jsonl", 2},    // key suffix
		{"proj", 3},                  // key substring
		{"c-d", 3},                   // also in key: a key substring beats a sessionId substring
		{"zzz", -1},
	}
	for _, c := range cases {
		if got := r.matchRank(c.pattern); got != c.want {
			t.Errorf("matchRank(%q) = %d, want %d", c.pattern, got, c.want)
		}
	}

	// Absent from key, present only in sessionId: rank 4
	r2 := newRecord(map[string]any{"sessionId": "uniq-sid-9", "key": "/r/other/file.jsonl"}, "")
	if got := r2.matchRank("sid-9"); got != 4 {
		t.Errorf("matchRank(sid-9) = %d, want 4", got)
	}

	// Case-insensitive: the pattern arrives already lowercased
	upper := newRecord(map[string]any{"sessionId": "ABC-DEF", "key": "/R/P.jsonl"}, "")
	if got := upper.matchRank("abc-def"); got != 0 {
		t.Errorf("case-insensitive match = %d, want 0", got)
	}
}

// TestRecordSortAcrossFormats: cross-source ordering goes by the parsed time, not by
// lexicographic order. Gemini writes RFC3339Nano, file sources use the shape derived from
// mtime, and OpenClaw uses epoch milliseconds — compare the strings and the one carrying a
// timezone offset lands in completely the wrong place.
func TestRecordSortAcrossFormats(t *testing.T) {
	records := []record{
		newRecord(map[string]any{"key": "mtime"}, "2026-09-14T03:16:50"),        // UTC
		newRecord(map[string]any{"key": "offset"}, "2026-09-14T11:20:00+08:00"), // = 03:20 UTC, the newest
		newRecord(map[string]any{"key": "nano"}, "2026-09-14T03:16:50.601Z"),    //
		newRecord(map[string]any{"key": "epochms"}, float64(1789197000000)),     // 2026-09-14T02:30 UTC
		newRecord(map[string]any{"key": "bad"}, "not a time at all"),            // unparseable, sorts last
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].newerThan(records[j]) })

	want := []string{"offset", "nano", "mtime", "epochms", "bad"}
	for i, key := range want {
		if got := records[i].str("key"); got != key {
			t.Fatalf("position %d = %q, want %q (full order %v)", i, got, key, keysOf(records))
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
// The query layer (each source has its own tests in source_*_test.go, SQLite in
// hermes_sqlite_test.go)
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

	// When both sources match the same sessionId exactly, the earlier source wins
	source, rec, ok := api.findSession("shared-id", "")
	if !ok || source.Mode() != "pi" || rec.str("sessionId") != "shared-id" {
		t.Fatalf("find = %v %v %v", source, rec.fields, ok)
	}

	// A fuzzy hit must not shadow an exact hit in another source
	write(t, filepath.Join(claudeRoot, "c", "session-y.jsonl"),
		`{"type":"user","uuid":"u","sessionId":"deadbeef-shared-id-x","timestamp":"t","message":{"role":"user","content":"hi"}}`,
	)
	source, rec, _ = api.findSession("shared-id", "")
	if source.Mode() != "pi" {
		t.Fatalf("the exact hit should win, got %s %v", source.Mode(), rec.fields)
	}

	// The "Session: " prefix is stripped
	if _, _, ok := api.findSession("Session: shared-id", ""); !ok {
		t.Fatal("the Session: prefix had no effect")
	}
	if _, _, ok := api.findSession("   ", ""); ok {
		t.Fatal("an empty pattern should match nothing")
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
		t.Fatal("there should be an ETag")
	}
	// A new file does not appear while the cache is valid; with TTL=0 it shows up at once
	write(t, filepath.Join(root, "p3", "third.jsonl"), `{"type":"session","id":"third"}`)
	if got, again := api.listSessions(); len(got) != 2 || again != etag {
		t.Fatalf("the cache should have hit, got %d entries etag=%s", len(got), again)
	}
	uncached := newSessionQueryAPI([]SessionSource{newPiSource(root)}, 0)
	got, newETag := uncached.listSessions()
	if len(got) != 3 {
		t.Fatalf("TTL=0 should show 3 entries immediately, got %d", len(got))
	}
	if newETag == etag {
		t.Fatal("the list changed, so the ETag should have too")
	}
}

// TestPublicIsACopy: public() has to hand back a copy. Records are held by the list cache,
// so one mutation of the map it returns leaves every later reader with dirty data.
func TestPublicIsACopy(t *testing.T) {
	r := newRecord(map[string]any{"sessionId": "s", "status": "done"}, "")
	out := r.public()
	out["status"] = "tampered"
	if r.str("status") != "done" {
		t.Fatalf("the internal field was changed to %q", r.str("status"))
	}
}

// TestFileRecordCacheReusesUnchanged: an unchanged file must not have its head reparsed.
func TestFileRecordCacheReusesUnchanged(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "p", "a.jsonl")
	write(t, path, `{"type":"session","id":"cached"}`)

	cache := newFileRecordCache(nil) // no counter: this test is about record reuse
	builds := 0
	build := func(p, modISO string) record {
		builds++
		return newRecord(map[string]any{"key": p, "sessionId": "cached"}, modISO)
	}

	if got := cache.records([]string{path}, build); len(got) != 1 || builds != 1 {
		t.Fatalf("first pass = %d entries / %d parses", len(got), builds)
	}
	if got := cache.records([]string{path}, build); len(got) != 1 || builds != 1 {
		t.Fatalf("the file did not change yet it was parsed again: %d times", builds)
	}

	// Changed content (and therefore size) must trigger a reparse
	write(t, path, `{"type":"session","id":"cached"}`, `{"type":"message"}`)
	if cache.records([]string{path}, build); builds != 2 {
		t.Fatalf("a changed file should be reparsed, got %d parses", builds)
	}

	// A deleted file drops out of the listing and its cache entry goes with it
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := cache.records([]string{path}, build); len(got) != 0 {
		t.Fatalf("the file is gone but %d entries were still listed", len(got))
	}
	if len(cache.entries) != 0 {
		t.Fatalf("the cache was not cleaned up: %v", cache.entries)
	}
}

// ---------------------------------------------------------------------------
// The HTTP layer
// ---------------------------------------------------------------------------

func newTestServerFixture(t *testing.T, token, sessionID string) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	sessionPath := filepath.Join(root, "p", "2026-01-01T00-00-00_abc.jsonl")
	write(t, sessionPath,
		`{"type":"session","id":"`+sessionID+`","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"question"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"answer"}]}}`,
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

	// Unauthenticated endpoints
	if code, body := get(t, srv.URL+"/health", ""); code != 200 || body["status"] != "ok" {
		t.Fatalf("/health = %d %v", code, body)
	}
	if code, _ := get(t, srv.URL+"/", ""); code != 200 {
		t.Fatalf("/ = %d", code)
	}
	// The /api prefix only applies to the /sessions family: /api/health falls through to the
	// 404 that comes after authentication (and hits 401 first when a token is set)
	if code, _ := get(t, srv.URL+"/api/health", ""); code != 401 {
		t.Fatalf("/api/health (token configured) = %d", code)
	}

	// Authentication
	if code, body := get(t, srv.URL+"/sessions", ""); code != 401 || body["error"] != "Unauthorized" {
		t.Fatalf("no token = %d %v", code, body)
	}
	if code, _ := get(t, srv.URL+"/sessions", "wrong"); code != 401 {
		t.Fatalf("wrong token = %d", code)
	}

	// Listing
	code, body := get(t, srv.URL+"/sessions", "secret")
	if code != 200 || body["total"] != float64(1) {
		t.Fatalf("/sessions = %d %v", code, body)
	}
	sessions := body["sessions"].([]any)
	first := sessions[0].(map[string]any)
	if first["sessionId"] != "sess-1" || first["source"] != "pi" {
		t.Fatalf("session = %v", first)
	}

	// The /api prefix is equivalent
	if code, body := get(t, srv.URL+"/api/sessions", "secret"); code != 200 || body["total"] != float64(1) {
		t.Fatalf("/api/sessions = %d %v", code, body)
	}

	// A single session (including the file field)
	if code, body := get(t, srv.URL+"/sessions/sess-1", "secret"); code != 200 || body["file"] != sessionPath {
		t.Fatalf("/sessions/sess-1 = %d %v", code, body)
	}

	// Messages plus limit
	code, body = get(t, srv.URL+"/sessions/sess-1/messages?limit=1", "secret")
	if code != 200 || body["total"] != float64(1) {
		t.Fatalf("messages = %d %v", code, body)
	}
	msgs := body["messages"].([]any)
	if msgs[0].(map[string]any)["id"] != "m1" { // limit takes the earliest N
		t.Fatalf("messages[0] = %v", msgs[0])
	}
	if _, body := get(t, srv.URL+"/sessions/sess-1/messages?limit=abc", "secret"); body["total"] != float64(2) {
		t.Fatalf("an invalid limit should fall back to 50: %v", body)
	}

	// The final result
	code, body = get(t, srv.URL+"/sessions/sess-1/final", "secret")
	if code != 200 || body["text"] != "answer" || body["isFinal"] != true {
		t.Fatalf("final = %d %v", code, body)
	}

	// Not found / unknown path
	if code, body := get(t, srv.URL+"/sessions/nope", "secret"); code != 404 || body["error"] != "Session not found" {
		t.Fatalf("404 = %d %v", code, body)
	}
	if code, body := get(t, srv.URL+"/whatever", "secret"); code != 404 || body["error"] != "Not found" {
		t.Fatalf("unknown path = %d %v", code, body)
	}

	// Without a token /api/health is a 404 (unauthenticated endpoints have no /api alias)
	openSrv, _ := newTestServer(t, "")
	if code, _ := get(t, openSrv.URL+"/api/health", ""); code != 404 {
		t.Fatalf("/api/health (no token) = %d", code)
	}
}

func TestHTTPEncodedPattern(t *testing.T) {
	// A pattern containing a colon must be URL encoded (%3A) and decoded before matching
	srv, _ := newTestServerFixture(t, "secret", "hook:alert:x")
	code, body := get(t, srv.URL+"/sessions/hook%3Aalert%3Ax", "secret")
	if code != 200 || body["sessionId"] != "hook:alert:x" {
		t.Fatalf("encoded pattern = %d %v", code, body)
	}
}

func TestHTTPOptionsAndMethods(t *testing.T) {
	srv, _ := newTestServer(t, "")

	// No CORS headers by default: with no token the service is unauthenticated, and a single
	// Access-Control-Allow-Origin: * would let any web page read this machine's sessions
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("there should be no CORS header by default: %d %v", resp.StatusCode, resp.Header)
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
		t.Fatalf("data endpoints should carry no CORS header by default, got %q", got)
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

// TestHTTPCORSOptIn: headers appear only once --cors-origin is set, and the preflight
// must allow Authorization
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
		t.Fatalf("the preflight must allow Authorization, or a cross-origin client cannot use a token at all: %v", resp.Header)
	}
	if resp.Header.Get("Vary") != "Origin" {
		t.Fatalf("allowing a specific origin requires Vary: Origin, got %q", resp.Header.Get("Vary"))
	}
}

// TestHTTPLimitClamped: ?limit= has to be clamped, or a single request can exhaust memory
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
		t.Fatalf("limit was not clamped to 5: %v", body["total"])
	}
	// Negative values and 0 still mean "nothing at all", matching the original behaviour
	if _, body := get(t, srv.URL+"/sessions/big/messages?limit=-1", ""); body["total"] != float64(0) {
		t.Fatalf("limit=-1 should return 0 entries: %v", body["total"])
	}
}

// TestHTTPSessionsETag: when the list has not changed, a poll should end at a 304
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
		t.Fatal("/sessions should carry an ETag")
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
		t.Fatalf("the same ETag should return 304, got %d", resp.StatusCode)
	}

	// A mismatched ETag returns the full list as usual
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("If-None-Match", `W/"deadbeef"`)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a mismatched ETag should return 200, got %d", resp.StatusCode)
	}
}

func TestBuildSourcesModes(t *testing.T) {
	// With no source detected, fall back to OpenClaw (the same for all and auto)
	home := t.TempDir()
	setHome(t, home)
	if sources, err := buildSources("auto"); err != nil || len(sources) != 1 || sources[0].Mode() != "openclaw" {
		t.Fatalf("auto fallback = %v %v", sources, err)
	}
	if sources, err := buildSources("all"); err != nil || len(sources) != 1 || sources[0].Mode() != "openclaw" {
		t.Fatalf("all fallback = %v %v", sources, err)
	}

	// Create the pi and claude sources: auto enables only those that exist, and so does all
	write(t, filepath.Join(home, ".pi", "agent", "sessions", "p", "x.jsonl"), `{"type":"session","id":"x"}`)
	write(t, filepath.Join(home, ".claude", "projects", "p", "y.jsonl"), `{"type":"user","uuid":"u","message":{"role":"user","content":"hi"}}`)
	if sources, err := buildSources("auto"); err != nil || len(sources) != 2 ||
		sources[0].Mode() != "pi" || sources[1].Mode() != "claude" {
		t.Fatalf("auto = %v %v", sources, err)
	}
	if sources, err := buildSources("all"); err != nil || len(sources) != 2 {
		t.Fatalf("all = %v %v", sources, err)
	}

	// A single named mode enables it whether or not the directory exists
	if sources, err := buildSources("pi"); err != nil || len(sources) != 1 || sources[0].Mode() != "pi" {
		t.Fatalf("pi = %v %v", sources, err)
	}
	if _, err := buildSources("nope"); err == nil {
		t.Fatal("an unknown mode should error")
	}
}

// TestAcceptQueueDepth: an explicit --accept-queue is used as given; 0 derives it from
// max-connections
func TestAcceptQueueDepth(t *testing.T) {
	cases := []struct{ maxConns, queue, want int }{
		{50, 0, 100},  // twice max-connections
		{2, 0, 32},    // too small, so the floor applies
		{200, 0, 400}, // twice max-connections
		{2, 5, 5},     // explicit, so the floor no longer applies
		{50, 1, 1},    // explicitly squeezed to the minimum
	}
	for _, c := range cases {
		if got := acceptQueueDepth(c.maxConns, c.queue); got != c.want {
			t.Errorf("acceptQueueDepth(%d, %d) = %d, want %d", c.maxConns, c.queue, got, c.want)
		}
	}
}

// TestMessageSink: the earliest N must stop early, and the latest N must run to the end
// while keeping memory tied to limit
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

	// Earliest 3: it should stop after the third
	head := newMessageSink(messageQuery{limit: 3})
	if fed := feed(head, 100); fed != 3 {
		t.Fatalf("the earliest N should stop at the third, but %d were fed", fed)
	}
	if got := ids(head.result()); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("earliest 3 = %v", got)
	}

	// Latest 3: it has to scan all the way, and the result comes back chronological
	tail := newMessageSink(messageQuery{limit: 3, fromEnd: true})
	if fed := feed(tail, 100); fed != 100 {
		t.Fatalf("the latest N must scan everything, but only %d were fed", fed)
	}
	if got := ids(tail.result()); !reflect.DeepEqual(got, []int{97, 98, 99}) {
		t.Fatalf("latest 3 = %v", got)
	}
	if len(tail.items) != 3 {
		t.Fatalf("the ring buffer should hold only 3, got %d", len(tail.items))
	}

	// Fewer than limit comes back as-is
	few := newMessageSink(messageQuery{limit: 10, fromEnd: true})
	feed(few, 2)
	if got := ids(few.result()); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("below limit = %v", got)
	}
	// limit=0 takes nothing at all
	zero := newMessageSink(messageQuery{limit: 0})
	if fed := feed(zero, 5); fed != 1 || len(zero.result()) != 0 {
		t.Fatalf("limit=0 should stop immediately and stay empty: fed=%d len=%d", fed, len(zero.result()))
	}
}

// TestHTTPMessagesOrder: ?order=desc returns the latest N (the end of a session is the
// interesting part)
func TestHTTPMessagesOrder(t *testing.T) {
	root := t.TempDir()
	lines := []string{`{"type":"session","id":"long"}`}
	for i := 0; i < 10; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"type":"message","id":"m%d","message":{"role":"user","content":[{"type":"text","text":"message %d"}]}}`, i, i))
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
		t.Fatalf("the default should take the earliest 3: %v", asc)
	}
	_, desc := get(t, srv.URL+"/sessions/long/messages?limit=3&order=desc", "")
	if desc["order"] != "desc" || firstID(desc) != "m7" || lastID(desc) != "m9" {
		t.Fatalf("order=desc should take the latest 3 and still be chronological: %v", desc)
	}
	// With fewer messages than the limit, both ends agree
	_, all := get(t, srv.URL+"/sessions/long/messages?limit=50&order=desc", "")
	if all["total"] != float64(10) || firstID(all) != "m0" {
		t.Fatalf("a limit above the total should return everything: %v", all)
	}
}

// TestHTTPHealthAuthRequired: the page uses this to decide whether to show the token prompt
func TestHTTPHealthAuthRequired(t *testing.T) {
	withToken, _ := newTestServer(t, "secret")
	if _, body := get(t, withToken.URL+"/health", ""); body["authRequired"] != true {
		t.Fatalf("with a token configured this should report authRequired=true: %v", body)
	}
	open, _ := newTestServer(t, "")
	if _, body := get(t, open.URL+"/health", ""); body["authRequired"] != false {
		t.Fatalf("without a token this should report authRequired=false: %v", body)
	}
}

// TestMatchRankPathSeparators: on Windows a key is C:\...\abc.jsonl while users habitually
// type proj/abc.jsonl. Both separators have to count, or an exact suffix hit degrades to a
// substring hit and another source's fuzzy match can win the cross-source lookup.
func TestMatchRankPathSeparators(t *testing.T) {
	winKey := `C:\Users\dev\.claude\projects\proj\abc-def.jsonl`
	rec := newRecord(map[string]any{"sessionId": "abc-def", "key": winKey}, "")

	cases := []struct {
		pattern string
		want    int
	}{
		{"abc-def", 0}, // exact sessionId
		{winKey, 1},    // the Windows path pasted verbatim
		{"c:/users/dev/.claude/projects/proj/abc-def.jsonl", 1}, // the forward-slash spelling is equivalent
		{"proj/abc-def.jsonl", 2},                               // a suffix hit, and must not degrade to 3
		{`proj\abc-def.jsonl`, 2},                               // the backslash spelling matches too
		{"projects", 3},                                         // substring
		{"zzz", -1},
	}
	for _, c := range cases {
		// this is exactly what findSession does to the pattern
		if got := rec.matchRank(normalizeForMatch(c.pattern)); got != c.want {
			t.Errorf("matchRank(%q) = %d, want %d", c.pattern, got, c.want)
		}
	}
}

// TestSQLiteURIWindowsPath: backslashes inside SQLite's file: URI are ambiguous with escapes
func TestSQLiteURIWindowsPath(t *testing.T) {
	got := sqliteURI(`C:\Users\dev\.hermes\state.db`)
	if runtime.GOOS == "windows" {
		if got != "file:C:/Users/dev/.hermes/state.db" {
			t.Fatalf("a Windows path should be converted to forward slashes: %q", got)
		}
		return
	}
	// Off Windows a backslash is a legal filename character and filepath.ToSlash leaves it be
	if got != `file:C:\Users\dev\.hermes\state.db` {
		t.Fatalf("paths should not be rewritten off Windows: %q", got)
	}
}

// TestProjectsAndActive: project grouping plus the live marker.
// File-backed sources always report status done, so without inferring from the update time
// a session being written right now looks identical to one from three months ago.
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
			t.Fatalf("a session with a cwd should carry a project: %v", s)
		}
	}
	if !active["fresh"] {
		t.Fatal("a just-written session should be active")
	}
	if active["stale"] || active["other"] {
		t.Fatalf("a session from three hours ago should not count as active: %v", active)
	}

	projects, ungrouped := api.listProjects()
	if len(projects) != 2 || ungrouped != 0 {
		t.Fatalf("this should group into 2 projects: %v (ungrouped=%d)", projects, ungrouped)
	}
	// The most recently touched project comes first
	if projects[0]["project"] != "/w/alpha" || projects[0]["sessions"] != 2 {
		t.Fatalf("first project = %v", projects[0])
	}
	if projects[0]["shortName"] != "alpha" || !truthy(projects[0]["isActive"]) {
		t.Fatalf("the project fields are wrong: %v", projects[0])
	}
	if projects[1]["project"] != "/w/beta" || truthy(projects[1]["isActive"]) {
		t.Fatalf("second project = %v", projects[1])
	}
}

// TestExportMarkdown: the exported Markdown should paste straight into an issue
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
	// The export heading is the display name, which since the title work is the first user
	// message ("question") rather than the file stem
	for _, want := range []string{"# question", "**Source**: pi", "## Final result", "## Messages", "### user", "question", "answer"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the export is missing %q:\n%s", want, body)
		}
	}
	// A session that does not exist
	if code, _ := get(t, srv.URL+"/sessions/nope/export", "secret"); code != 404 {
		t.Fatalf("exporting a nonexistent session should be 404, got %d", code)
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

// TestHTTPProjects: the /projects endpoint
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
		t.Fatal("/projects should require authentication")
	}
}

// TestLastRecordTime: read the last record's time from the tail of the file, accepting all
// three placements
func TestLastRecordTime(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, content, want string }{
		{"top-level timestamp", `{"type":"a","timestamp":"2026-09-01T01:00:00Z"}
{"type":"b","timestamp":"2026-09-02T02:00:00Z"}`, "2026-09-02T02:00:00Z"},
		{"inside message", `{"type":"message","message":{"timestamp":"2026-09-03T03:00:00Z"}}`, "2026-09-03T03:00:00Z"},
		{"a $set patch row", `{"sessionId":"g","lastUpdated":"2026-09-01T00:00:00Z"}
{"$set":{"lastUpdated":"2026-09-04T04:00:00Z"}}`, "2026-09-04T04:00:00Z"},
		{"trailing rows with no time", `{"type":"a","timestamp":"2026-09-05T05:00:00Z"}
{"type":"mode"}
{"type":"atis-latch"}`, "2026-09-05T05:00:00Z"},
		{"no time anywhere", `{"type":"mode"}
{"type":"atis-latch"}`, ""},
		{"empty file", "", ""},
	}
	for _, c := range cases {
		path := filepath.Join(dir, sanitizeFilename(c.name)+".jsonl")
		if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := lastRecordTime(path); got != c.want {
			t.Errorf("lastRecordTime(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestLastRecordTimeGrowsWindow: when the first tail window holds no complete record, the
// window has to grow
func TestLastRecordTimeGrowsWindow(t *testing.T) {
	saved := tailWindows
	tailWindows = []int64{64, 512, 4 << 20} // shrunk so the case is easy to construct
	defer func() { tailWindows = saved }()

	path := filepath.Join(t.TempDir(), "big.jsonl")
	// The last line is long: a 64-byte window cuts it in half, so it takes a larger one
	long := `{"type":"a","timestamp":"2026-09-06T06:00:00Z","pad":"` + strings.Repeat("x", 300) + `"}`
	if err := os.WriteFile(path, []byte("{\"type\":\"head\"}\n"+long+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lastRecordTime(path); got != "2026-09-06T06:00:00Z" {
		t.Fatalf("the window did not grow: %q", got)
	}
}

// TestUpdatedAtPrefersContentTime: the list time has to be when the conversation actually
// happened, not the file's mtime. Something rewrites session files without appending
// anything: measured over 174 real Claude sessions, 43 differed by more than an hour and
// the worst by 235.
func TestUpdatedAtPrefersContentTime(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "aaaa-bbbb.jsonl")
	write(t, path,
		`{"type":"user","uuid":"u1","sessionId":"sid","cwd":"/w","timestamp":"2026-09-11T11:25:29.029Z","message":{"role":"user","content":"hi"}}`,
	)
	// Set mtime to now: the file was touched, but its content is still six days old
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}

	r := newClaudeSource(root).List()[0]
	if got := r.str("updatedAt"); got != "2026-09-11T11:25:29.029Z" {
		t.Fatalf("updatedAt = %q; it should use the time in the content, not mtime", got)
	}
	// And a file that was merely touched must not be mistaken for one being written
	if truthy(r.public()["isActive"]) {
		t.Fatal("a six-day-old session must not count as active just because mtime is recent")
	}
}

// TestUpdatedAtFallsBackToMtime: with no usable time in the content, fall back to mtime
// rather than leaving it empty — empty sorts to the very end of the list, which is worse
// than using mtime.
func TestUpdatedAtFallsBackToMtime(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "cccc-dddd.jsonl")
	write(t, path, `{"type":"queue-operation","sessionId":"sid"}`, `{"type":"mode"}`)

	r := newClaudeSource(root).List()[0]
	updated := r.str("updatedAt")
	if updated == "" {
		t.Fatal("with no content time it should fall back to mtime, not stay empty")
	}
	if _, ok := parseTimestamp(updated); !ok {
		t.Fatalf("the mtime fallback does not parse: %q", updated)
	}
}

// TestFindSessionScopedBySource: the same pattern resolves differently per source, and an
// unknown source name is the caller's error to make (here, simply no match).
func TestFindSessionScopedBySource(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "w", "2026-01-01T00-00-00_s.jsonl"),
		`{"type":"session","id":"dup-id","cwd":"/w"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
	)
	api := newSessionQueryAPI([]SessionSource{newPiSource(root)}, 2)

	if _, _, ok := api.findSession("dup-id", "pi"); !ok {
		t.Fatal("the scoped lookup must find its own source")
	}
	if _, _, ok := api.findSession("dup-id", "claude"); ok {
		t.Fatal("a scoped lookup into another source must not fall through")
	}
}
