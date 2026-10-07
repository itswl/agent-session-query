package app

import (
	"encoding/json"
	"fmt"
	"github.com/itswl/agent-session-query/internal/source"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	got := source.Truncate(strings.Repeat("好", 10), 3, "...[truncated]")
	if got != "好好好...[truncated]" {
		t.Fatalf("truncate = %q", got)
	}
	if source.Truncate("abc", 3, "!") != "abc" {
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
		if got := source.ContentText(c.in); got != c.want {
			t.Errorf("source.ContentText(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatchRank(t *testing.T) {
	// The lowercased forms are computed when the record is built (record.lowerSID / lowerKey)
	r := source.NewRecord(source.Record{SessionID: "abc-def", Key: "/r/proj/abc-def.jsonl"}, "")
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
		if got := r.MatchRank(c.pattern); got != c.want {
			t.Errorf("matchRank(%q) = %d, want %d", c.pattern, got, c.want)
		}
	}

	// Absent from key, present only in sessionId: rank 4
	r2 := source.NewRecord(source.Record{SessionID: "uniq-sid-9", Key: "/r/other/file.jsonl"}, "")
	if got := r2.MatchRank("sid-9"); got != 4 {
		t.Errorf("matchRank(sid-9) = %d, want 4", got)
	}

	// Case-insensitive: the pattern arrives already lowercased
	upper := source.NewRecord(source.Record{SessionID: "ABC-DEF", Key: "/R/P.jsonl"}, "")
	if got := upper.MatchRank("abc-def"); got != 0 {
		t.Errorf("case-insensitive match = %d, want 0", got)
	}
}

// TestRecordSortAcrossFormats: cross-source ordering goes by the parsed time, not by
// lexicographic order. Gemini writes RFC3339Nano, file sources use the shape derived from
// mtime, and OpenClaw uses epoch milliseconds — compare the strings and the one carrying a
// timezone offset lands in completely the wrong place.
func TestRecordSortAcrossFormats(t *testing.T) {
	records := []source.Record{
		source.NewRecord(source.Record{Key: "mtime"}, "2026-09-14T03:16:50"),        // UTC
		source.NewRecord(source.Record{Key: "offset"}, "2026-09-14T11:20:00+08:00"), // = 03:20 UTC, the newest
		source.NewRecord(source.Record{Key: "nano"}, "2026-09-14T03:16:50.601Z"),    //
		source.NewRecord(source.Record{Key: "epochms"}, float64(1789197000000)),     // 2026-09-14T02:30 UTC
		source.NewRecord(source.Record{Key: "bad"}, "not a time at all"),            // unparseable, sorts last
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].NewerThan(records[j]) })

	want := []string{"offset", "nano", "mtime", "epochms", "bad"}
	for i, key := range want {
		if got := records[i].Key; got != key {
			t.Fatalf("position %d = %q, want %q (full order %v)", i, got, key, keysOf(records))
		}
	}
}

func keysOf(records []source.Record) []string {
	out := []string{}
	for _, r := range records {
		out = append(out, r.Key)
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

	api := newSessionQueryAPI([]source.SessionSource{
		source.NewPiSource(piRoot),
		source.NewClaudeSource(claudeRoot),
	}, 2)

	// When both sources match the same sessionId exactly, the earlier source wins
	src, rec, ok := api.findSession("shared-id", "")
	if !ok || src.Mode() != "pi" || rec.SessionID != "shared-id" {
		t.Fatalf("find = %v %v %v", src, rec, ok)
	}

	// A fuzzy hit must not shadow an exact hit in another source
	write(t, filepath.Join(claudeRoot, "c", "session-y.jsonl"),
		`{"type":"user","uuid":"u","sessionId":"deadbeef-shared-id-x","timestamp":"t","message":{"role":"user","content":"hi"}}`,
	)
	src, rec, _ = api.findSession("shared-id", "")
	if src.Mode() != "pi" {
		t.Fatalf("the exact hit should win, got %s %v", src.Mode(), rec)
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

	api := newSessionQueryAPI([]source.SessionSource{source.NewPiSource(root)}, 60)
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
	uncached := newSessionQueryAPI([]source.SessionSource{source.NewPiSource(root)}, 0)
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
	r := source.NewRecord(source.Record{SessionID: "s", Status: "done"}, "")
	out := r.Public()
	out["status"] = "tampered"
	if r.Status != "done" {
		t.Fatalf("the internal field was changed to %q", r.Status)
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
	sources := []source.SessionSource{source.NewPiSource(root)}
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
	} else {
		// A client that upgrades separately from the server asks what this build can do
		// rather than mapping a version onto a feature list
		caps, ok := body["capabilities"].([]any)
		if !ok || len(caps) == 0 {
			t.Fatalf("/health capabilities = %v", body["capabilities"])
		}
		found := map[string]bool{}
		for _, c := range caps {
			found[source.ToStr(c)] = true
		}
		for _, want := range []string{"brief.since", "messages.full", "rounds.lastAt"} {
			if !found[want] {
				t.Errorf("/health capabilities missing %q: %v", want, caps)
			}
		}
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
	// The body names the server's version; the page reloads itself when it changes
	if body["version"] != buildVersion {
		t.Fatalf("version = %v, want %q", body["version"], buildVersion)
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
	sources := []source.SessionSource{source.NewPiSource(root)}
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
	sources := []source.SessionSource{source.NewPiSource(root)}
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

	// The tag moves once when a background message count lands (that is how the page
	// learns its numbers), so settle it first: take tags until two reads in a row agree.
	// Holding an unsettled tag against itself would race a genuine change.
	var etag string
	for attempt := 0; attempt < 100; attempt++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sessions", nil)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		got := resp.Header.Get("ETag")
		if got != "" && got == etag {
			break
		}
		etag = got
		time.Sleep(10 * time.Millisecond)
	}
	if etag == "" {
		t.Fatal("/sessions should carry an ETag")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
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
	if sources, err := source.BuildSources("auto", nil); err != nil || len(sources) != 1 || sources[0].Mode() != "openclaw" {
		t.Fatalf("auto fallback = %v %v", sources, err)
	}
	if sources, err := source.BuildSources("all", nil); err != nil || len(sources) != 1 || sources[0].Mode() != "openclaw" {
		t.Fatalf("all fallback = %v %v", sources, err)
	}

	// Create the pi and claude sources: auto enables only those that exist, and so does all
	write(t, filepath.Join(home, ".pi", "agent", "sessions", "p", "x.jsonl"), `{"type":"session","id":"x"}`)
	write(t, filepath.Join(home, ".claude", "projects", "p", "y.jsonl"), `{"type":"user","uuid":"u","message":{"role":"user","content":"hi"}}`)
	if sources, err := source.BuildSources("auto", nil); err != nil || len(sources) != 2 ||
		sources[0].Mode() != "pi" || sources[1].Mode() != "claude" {
		t.Fatalf("auto = %v %v", sources, err)
	}
	if sources, err := source.BuildSources("all", nil); err != nil || len(sources) != 2 {
		t.Fatalf("all = %v %v", sources, err)
	}

	// A single named mode enables it whether or not the directory exists
	if sources, err := source.BuildSources("pi", nil); err != nil || len(sources) != 1 || sources[0].Mode() != "pi" {
		t.Fatalf("pi = %v %v", sources, err)
	}
	if _, err := source.BuildSources("nope", nil); err == nil {
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
	sources := []source.SessionSource{source.NewPiSource(root)}
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
	_, older := get(t, srv.URL+"/sessions/long/messages?limit=3&order=desc&offset=3", "")
	if firstID(older) != "m4" || lastID(older) != "m6" {
		t.Fatalf("descending offset should page to the older 3: %v", older)
	}
	_, newer := get(t, srv.URL+"/sessions/long/messages?limit=3&order=asc&offset=3", "")
	if firstID(newer) != "m3" || lastID(newer) != "m5" {
		t.Fatalf("ascending offset should page to the newer 3: %v", newer)
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
	rec := source.NewRecord(source.Record{SessionID: "abc-def", Key: winKey}, "")

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
		if got := rec.MatchRank(source.NormalizeForMatch(c.pattern)); got != c.want {
			t.Errorf("matchRank(%q) = %d, want %d", c.pattern, got, c.want)
		}
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

	api := newSessionQueryAPI([]source.SessionSource{source.NewPiSource(root)}, 0)

	sessions, _ := api.listSessions()
	active := map[string]bool{}
	for _, s := range sessions {
		active[source.ToStr(s["sessionId"])] = source.Truthy(s["isActive"])
		if source.ToStr(s["project"]) == "" {
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
	if projects[0]["shortName"] != "alpha" || !source.Truthy(projects[0]["isActive"]) {
		t.Fatalf("the project fields are wrong: %v", projects[0])
	}
	if projects[1]["project"] != "/w/beta" || source.Truthy(projects[1]["isActive"]) {
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

	r := source.NewClaudeSource(root).List()[0]
	if got := r.UpdatedAt; got != "2026-09-11T11:25:29.029Z" {
		t.Fatalf("updatedAt = %q; it should use the time in the content, not mtime", got)
	}
	// And a file that was merely touched must not be mistaken for one being written
	if source.Truthy(r.Public()["isActive"]) {
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

	r := source.NewClaudeSource(root).List()[0]
	updated := r.UpdatedAt
	if updated == "" {
		t.Fatal("with no content time it should fall back to mtime, not stay empty")
	}
	if _, ok := source.ParseTimestamp(updated); !ok {
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
	api := newSessionQueryAPI([]source.SessionSource{source.NewPiSource(root)}, 2)

	if _, _, ok := api.findSession("dup-id", "pi"); !ok {
		t.Fatal("the scoped lookup must find its own source")
	}
	if _, _, ok := api.findSession("dup-id", "claude"); ok {
		t.Fatal("a scoped lookup into another source must not fall through")
	}
}

// TestExportCoversTheWholeSession: Export means the document, not a page of it. It used to
// inherit the message stream's 200-message page size, so exporting a 16 000-message session
// produced its last 200 with nothing in the file to say so — it read exactly like an export
// of the whole thing.
func TestExportCoversTheWholeSession(t *testing.T) {
	root := t.TempDir()
	lines := []string{
		`{"type":"session","id":"s-export","cwd":"/w"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"first"}]}}`,
	}
	for i := 2; i <= 250; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"type":"message","id":"m%d","message":{"role":"assistant","content":[{"type":"text","text":"line %d"}]}}`,
			i, i))
	}
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_export.jsonl"), lines...)

	sources := []source.SessionSource{source.NewPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 0), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)

	fetch := func(query string) string {
		t.Helper()
		resp, err := http.Get(srv.URL + "/sessions/s-export/export" + query)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("export%s = %d", query, resp.StatusCode)
		}
		return readBody(t, resp)
	}

	// No Limit: the whole session, and the file says so
	full := fetch("")
	if n := strings.Count(full, "\n### "); n != 250 {
		t.Fatalf("an unqualified export holds %d messages, want all 250", n)
	}
	if !strings.Contains(full, "**Messages**: all 250 messages") {
		t.Errorf("the export does not state its coverage:\n%s", firstLines(full, 12))
	}

	// A limit is honoured — and announced, which is the point
	head := fetch("?limit=10")
	if n := strings.Count(head, "\n### "); n != 10 {
		t.Fatalf("limit=10 produced %d messages", n)
	}
	if !strings.Contains(head, "10 of 250 messages") || !strings.Contains(head, "not in this file") {
		t.Errorf("a truncated export does not say it is truncated:\n%s", firstLines(head, 12))
	}
}

// firstLines is the head of an export, for a failure message that can be read
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestExportJSONL: the same session as data rather than as a document. One JSON object per
// line, each tagged, so a query is a filter — `select(.type=="message" and .role=="user")`
// — rather than a parse. This is the form something that intends to analyse a session with
// code wants, and the form the HTTP export offers alongside the Markdown one.
func TestExportJSONL(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_jsonl.jsonl"),
		`{"type":"session","id":"s-jsonl","cwd":"/w"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"first"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"second"}]}}`,
	)
	sources := []source.SessionSource{source.NewPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 0), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)

	fetch := func(query string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/sessions/s-jsonl/export" + query)
		if err != nil {
			t.Fatal(err)
		}
		return resp, readBody(t, resp)
	}

	resp, body := fetch("?format=jsonl")
	if resp.StatusCode != 200 {
		t.Fatalf("jsonl export = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, ".jsonl") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	// Every line has to stand alone: the point of the format is that a reader can take
	// one line at a time without parsing the file
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 4 { // session + 2 messages + final
		t.Fatalf("got %d lines, want 4:\n%s", len(lines), body)
	}
	kinds := []string{}
	for i, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %d is not JSON: %v (%q)", i+1, err, line)
		}
		kind, _ := record["type"].(string)
		kinds = append(kinds, kind)
		if i == 0 {
			// The header says what the file holds, the same promise the Markdown makes
			if record["coverage"] != "all 2 messages" || record["complete"] != true {
				t.Errorf("header = %v", record)
			}
			if record["sessionId"] != "s-jsonl" {
				t.Errorf("header sessionId = %v", record["sessionId"])
			}
		}
		if i == 1 && record["role"] != "user" {
			t.Errorf("first message record = %v", record)
		}
	}
	if kinds[0] != "session" || kinds[len(kinds)-1] != "final" {
		t.Errorf("record types = %v", kinds)
	}

	// A truncated export says so in the header, where a program will look
	_, body = fetch("?format=jsonl&limit=1")
	var header map[string]any
	if err := json.Unmarshal([]byte(strings.Split(body, "\n")[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header["complete"] != false || !strings.Contains(header["coverage"].(string), "1 of 2") {
		t.Errorf("a truncated jsonl export does not announce itself: %v", header)
	}

	// Markdown stays the default, and an unknown format is refused rather than guessed
	resp, _ = fetch("")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("the default export is not Markdown: %q", ct)
	}
	if resp, _ := fetch("?format=xml"); resp.StatusCode != 400 {
		t.Errorf("format=xml = %d, want 400", resp.StatusCode)
	}
}

// TestExportEveryFormat: the same session as a document, as data, and as a page. Each is a
// different answer to "give me this session", and each has to arrive with a type and a
// filename a client can act on.
func TestExportEveryFormat(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_all.jsonl"),
		`{"type":"session","id":"s-all","cwd":"/w"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"a <script>alert(1)</script> question"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"an answer"},{"type":"thinking","thinking":"a thought"},{"type":"toolCall","name":"bash","arguments":{"command":"ls"}}]}}`,
	)
	sources := []source.SessionSource{source.NewPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 0), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)

	wantType := map[string]string{
		"md":    "text/markdown",
		"jsonl": "application/x-ndjson",
		"json":  "application/json",
		"html":  "text/html",
	}
	for format, contentType := range wantType {
		resp, body := func() (*http.Response, string) {
			r, err := http.Get(srv.URL + "/sessions/s-all/export?format=" + format)
			if err != nil {
				t.Fatal(err)
			}
			return r, readBody(t, r)
		}()
		if resp.StatusCode != 200 {
			t.Fatalf("format=%s = %d", format, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, contentType) {
			t.Errorf("format=%s Content-Type = %q, want %q", format, ct, contentType)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "."+format) {
			t.Errorf("format=%s filename does not carry the extension: %q", format, cd)
		}
		if !strings.Contains(body, "an answer") {
			t.Errorf("format=%s does not contain the session", format)
		}
		// Whatever the format, a session's own text must not become markup. A session file
		// holds whatever its writer put there, and the HTML export is opened from disk.
		if format == "html" && strings.Contains(body, "<script>alert(1)</script>") {
			t.Errorf("the html export did not escape session content")
		}
	}

	// The json document carries the same header the jsonl does
	_, body := func() (*http.Response, string) {
		r, err := http.Get(srv.URL + "/sessions/s-all/export?format=json")
		if err != nil {
			t.Fatal(err)
		}
		return r, readBody(t, r)
	}()
	var doc struct {
		Session  map[string]any   `json:"session"`
		Messages []map[string]any `json:"messages"`
		Final    map[string]any   `json:"final"`
		Complete bool             `json:"complete"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("the json export does not parse: %v", err)
	}
	if len(doc.Messages) != 2 || doc.Session["sessionId"] != "s-all" || doc.Final == nil {
		t.Errorf("json export = %d messages, session %v", len(doc.Messages), doc.Session["sessionId"])
	}
	if doc.Session["coverage"] != "all 2 messages" {
		t.Errorf("json header coverage = %v", doc.Session["coverage"])
	}
}

// TestIsActiveIsRecencyNotLiveness pins what the dot is allowed to claim: the session's
// newest message sits inside activeWindow, and nothing about whether a process exists.
// Both directions are the point — a session that ended moments ago still reports true,
// and one whose agent has been working for longer than the window reports false — which
// is why the docs and the page describe recency rather than asserting the session is
// being written.
func TestIsActiveIsRecencyNotLiveness(t *testing.T) {
	activeAgo := func(d time.Duration) bool {
		ts := time.Now().Add(-d).UTC().Format(time.RFC3339Nano)
		r := source.NewRecord(source.Record{SessionID: "s", UpdatedAt: ts}, ts)
		return source.Truthy(r.Public()["isActive"])
	}
	if !activeAgo(5 * time.Second) {
		t.Error("a newest message seconds old should report active")
	}
	if !activeAgo(source.ActiveWindow - 15*time.Second) {
		t.Error("just inside the window should report active")
	}
	if activeAgo(source.ActiveWindow + 15*time.Second) {
		t.Error("just outside the window must not report active: the window is the whole claim")
	}
	// No usable timestamp is not activity — it is the absence of evidence
	if source.Truthy(source.NewRecord(source.Record{SessionID: "s"}, "").Public()["isActive"]) {
		t.Error("a record with no parseable time must not report active")
	}
}

// A hit tells you where a session is; resumeCommand tells you how to get back into it.
// The commands were read off each CLI's own help, and the two sources without one are the
// point of the test: a wrong command is worse than none.
func TestResumeCommand(t *testing.T) {
	for _, c := range []struct{ source, sid, want string }{
		{"claude", "1e2057c3-4ec4", "claude --resume 1e2057c3-4ec4"},
		{"codex", "0199f0a1", "codex resume 0199f0a1"},
		{"grok", "01a0cbfb", "grok -r 01a0cbfb"},
		{"hermes", "session_42", "hermes --resume session_42"},
		{"opencode", "ses_abc", "opencode --session ses_abc"},
		{"pi", "9f8e7d", "pi --session 9f8e7d"},
		// Gemini resumes by index into its own recent list, not by id
		{"gemini", "g-1", ""},
		// OpenClaw's "session key" could not be confirmed to be this id
		{"openclaw", "oc-1", ""},
		// A labeled instance is still the same CLI
		{"claude:box2", "abc", "claude --resume abc"},
		// Nothing to name
		{"claude", "", ""},
		{"nosuchsource", "abc", ""},
	} {
		rec := source.NewRecord(source.Record{Source: c.source, SessionID: c.sid}, "")
		if got := rec.ResumeCommand(); got != c.want {
			t.Errorf("%s/%q: got %q, want %q", c.source, c.sid, got, c.want)
		}
	}
}

// An id is normally a uuid and passes through untouched. The quoting exists so that an id
// carrying a space or a quote cannot turn a pasted command into two commands.
func TestResumeCommandQuotesUnsafeIds(t *testing.T) {
	rec := source.NewRecord(source.Record{Source: "hermes", SessionID: "a b; rm -rf /"}, "")
	if got := rec.ResumeCommand(); got != `hermes --resume 'a b; rm -rf /'` {
		t.Errorf("unsafe id = %q", got)
	}
	rec = source.NewRecord(source.Record{Source: "hermes", SessionID: "it's"}, "")
	if got := rec.ResumeCommand(); got != `hermes --resume 'it'\''s'` {
		t.Errorf("quoted id = %q", got)
	}
}

// Sources without a resume command omit the key rather than sending an empty one: a field
// that is sometimes a command and sometimes "" reads as a command that failed to build.
func TestPublicOmitsMissingResumeCommand(t *testing.T) {
	with := source.NewRecord(source.Record{Source: "claude", SessionID: "abc"}, "").Public()
	if with["resumeCommand"] != "claude --resume abc" {
		t.Errorf("resumeCommand = %v", with["resumeCommand"])
	}
	without := source.NewRecord(source.Record{Source: "gemini", SessionID: "g-1"}, "").Public()
	if _, present := without["resumeCommand"]; present {
		t.Errorf("gemini must not carry the key at all: %v", without)
	}
}

// TestHTTPMessagesFull: the ordinary read cuts tool output to a preview and marks it; ?full=1
// returns it whole, and the export always does — a document that cut every output at five
// hundred characters was a preview of the session, not the session
func TestHTTPMessagesFull(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("0123456789", 80)
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_full.jsonl"),
		`{"type":"session","id":"full-1","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"run"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"seq"}}],"stopReason":"toolUse"}}`,
		`{"type":"message","id":"m3","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"`+long+`"}],"isError":false}}`,
		`{"type":"message","id":"m4","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"done"}]}}`,
	)
	sources := []source.SessionSource{source.NewPiSource(root)}
	api := newSessionQueryAPI(sources, 0)
	srv := httptest.NewServer(newAPIServer(serverOptions{mode: "auto", sources: sources, api: api, maxConnections: 50}))
	defer srv.Close()

	_, body := get(t, srv.URL+"/sessions/full-1/messages", "")
	result := body["messages"].([]any)[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["truncated"] != true || len(result["content"].(string)) >= len(long) {
		t.Fatalf("the default read must cut and say so: %v", result)
	}
	_, body = get(t, srv.URL+"/sessions/full-1/messages?full=1", "")
	result = body["messages"].([]any)[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if _, has := result["truncated"]; has || result["content"] != long {
		t.Fatalf("?full=1 must return the whole output: %v", result)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sessions/full-1/export", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), long) {
		t.Fatal("the export must carry the whole output")
	}
}

// TestExportRendersOutcomes: a document says how each tool call ended and what happened
// between the turns, in words a reader scans for
func TestExportRendersOutcomes(t *testing.T) {
	blocks := []map[string]any{
		source.ToolCallBlock("c1", "Bash", map[string]any{"command": "make"}),
		source.ToolOutcome{Status: source.StatusError, ExitCode: 2, HasExit: true, DurationMs: 2300}.Apply(source.ToolResultBlock("c1", "Bash", "no rule", false)),
		source.EventBlock(source.EventCompaction, "Conversation compacted"),
		source.ToolOutcome{Status: source.StatusInterrupted}.Apply(source.ToolResultBlock("c2", "Bash", "", false)),
	}
	var md strings.Builder
	writeBlocks(&md, blocks)
	for _, want := range []string{"↳ Bash — failed · exit 2 · 2.3s", "> context compacted: Conversation compacted", "↳ Bash — interrupted"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown lacks %q:\n%s", want, md.String())
		}
	}
	var page strings.Builder
	writeHTMLBlocks(&page, blocks)
	if !strings.Contains(page.String(), `class="result failed"`) || !strings.Contains(page.String(), `class="event"`) {
		t.Errorf("html lacks the outcome classes:\n%s", page.String())
	}
	for ms, want := range map[int64]string{300: "300ms", 2300: "2.3s", 45_000: "45s", 134_000: "2m14s", 7_200_000: "2h"} {
		if got := formatDurationMs(ms); got != want {
			t.Errorf("formatDurationMs(%d) = %q, want %q", ms, got, want)
		}
	}
}

// TestListVersionFollowsBuild: the sessions ETag changes with the server version, so a page
// served by the previous version gets a full answer — and the new version — on its next poll.
func TestListVersionFollowsBuild(t *testing.T) {
	saved := buildVersion
	defer func() { buildVersion = saved }()
	buildVersion = "v1.0.0"
	before := listVersion(nil, nil)
	buildVersion = "v1.0.1"
	after := listVersion(nil, nil)
	if before == after {
		t.Fatalf("ETag %s did not change with the version", before)
	}
	if again := listVersion(nil, nil); again != after {
		t.Fatalf("ETag not stable: %s then %s", after, again)
	}
}

// TestServerCapabilitiesAreSortedAndUnique: the list is read by clients older than the
// build serving it, so it is append-only and stable. A duplicate or an unsorted entry is
// a merge that went wrong.
func TestServerCapabilitiesAreSortedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i, name := range serverCapabilities {
		if name == "" || strings.TrimSpace(name) != name {
			t.Errorf("capability %d = %q", i, name)
		}
		if seen[name] {
			t.Errorf("duplicate capability %q", name)
		}
		seen[name] = true
		if i > 0 && serverCapabilities[i-1] > name {
			t.Errorf("capabilities out of order: %q before %q", serverCapabilities[i-1], name)
		}
	}
}
