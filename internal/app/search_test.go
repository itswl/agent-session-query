package app

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexFold(t *testing.T) {
	cases := []struct {
		text, needle string
		want         int
	}{
		{"Hello World", "world", 6},
		{"HELLO", "hello", 0},
		{"nginx.conf", "NGINX", -1}, // the needle must already be lowercase; that is the caller's job
		{"配置 Nginx 反代", "nginx", 7}, // deliberately non-ASCII: "配置 " is 7 bytes, so this
		//                                pins the byte offset against multibyte text
		{"abc", "", 0},
		{"abc", "abcd", -1},
	}
	for _, c := range cases {
		if got := indexFold(c.text, c.needle); got != c.want {
			t.Errorf("indexFold(%q, %q) = %d, want %d", c.text, c.needle, got, c.want)
		}
	}
}

func TestSnippetAround(t *testing.T) {
	// The hit sits in the middle, so both ends need an ellipsis — and the snippet must not
	// slice a UTF-8 sequence in half. These fixtures are deliberately non-ASCII: that is
	// precisely what they test.
	long := strings.Repeat("一二三四五", 60) + "关键词" + strings.Repeat("六七八九十", 60)
	got := snippetAround(long, "关键词", 10)
	if !strings.Contains(got, "关键词") {
		t.Fatalf("the snippet does not contain the needle: %q", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("both ends were trimmed but carry no ellipsis: %q", got)
	}
	if !utf8Valid(got) {
		t.Fatalf("the snippet broke a UTF-8 sequence: %q", got)
	}
	if n := len([]rune(got)); n > 40 {
		t.Fatalf("the snippet is too long: %d characters", n)
	}

	// Short text is returned as-is, with no ellipsis
	if got := snippetAround("就这么短", "这么", 20); got != "就这么短" {
		t.Fatalf("short text = %q", got)
	}
	// The hit is at the start, so only the tail should carry an ellipsis
	head := snippetAround("开头命中"+strings.Repeat("填充", 100), "开头", 5)
	if strings.HasPrefix(head, "…") || !strings.HasSuffix(head, "…") {
		t.Fatalf("hit at the start = %q", head)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestFindMatchingTextPrefersBody(t *testing.T) {
	// When the needle appears in both a field name and the body, return the body
	obj := map[string]any{
		"snippetish": "unrelated",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "changed the nginx timeout"}},
		},
	}
	got, ok := findMatchingText(obj, "nginx", 0)
	if !ok || got != "changed the nginx timeout" {
		t.Fatalf("findMatchingText = %q %v", got, ok)
	}
	// Appearing only in a key name is not a match
	if _, ok := findMatchingText(map[string]any{"nginx": 1}, "nginx", 0); ok {
		t.Fatal("a key name must not count as a match")
	}
}

func newSearchServer(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_a.jsonl"),
		`{"type":"session","id":"s-hit","cwd":"/w/a"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"help me set up an Nginx reverse proxy"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","content":[{"type":"text","text":"nginx proxy_pass goes like this"}]}}`,
		`{"type":"message","id":"m3","message":{"role":"assistant","content":[{"type":"text","text":"a third NGINX, upper case"}]}}`,
	)
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-01_b.jsonl"),
		`{"type":"session","id":"s-miss","cwd":"/w/b"}`,
		`{"type":"message","id":"n1","message":{"role":"user","content":[{"type":"text","text":"something else entirely"}]}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPSearch(t *testing.T) {
	srv := newSearchServer(t)

	code, body := get(t, srv.URL+"/search?q=nginx", "")
	if code != 200 {
		t.Fatalf("/search = %d", code)
	}
	if body["total"] != float64(1) || body["scanned"] != float64(2) {
		t.Fatalf("should scan 2 sessions and match 1: %v", body)
	}
	hit := body["results"].([]any)[0].(map[string]any)
	if hit["sessionId"] != "s-hit" {
		t.Fatalf("the wrong session matched: %v", hit["sessionId"])
	}
	// Case-insensitive: all three should match (Nginx / nginx / NGINX)
	if hit["matchCount"] != float64(3) {
		t.Fatalf("matchCount = %v; matching should be case-insensitive", hit["matchCount"])
	}
	matches := hit["matches"].([]any)
	first := matches[0].(map[string]any)
	if !strings.Contains(first["snippet"].(string), "Nginx") || first["role"] != "user" {
		t.Fatalf("first hit = %v", first)
	}

	// per_session caps how many hits each session returns
	if _, body := get(t, srv.URL+"/search?q=nginx&per_session=1", ""); body["results"].([]any)[0].(map[string]any)["matchCount"] != float64(1) {
		t.Fatalf("per_session=1 had no effect: %v", body)
	}
	// limit caps how many sessions come back, but matched still reports the real total
	code, body = get(t, srv.URL+"/search?q=nginx&limit=0", "")
	if body["total"] != float64(0) || body["matched"] != float64(1) || body["truncated"] != true {
		t.Fatalf("matched should still be reported after limit truncates: %v", body)
	}
	// Nothing found
	if _, body := get(t, srv.URL+"/search?q=averyunlikelyneedle", ""); body["total"] != float64(0) || body["truncated"] != false {
		t.Fatalf("no match = %v", body)
	}
	// q is required
	if code, _ := get(t, srv.URL+"/search", ""); code != 400 {
		t.Fatalf("a missing q should be 400, got %d", code)
	}
}

func TestHTTPSearchNeedsAuth(t *testing.T) {
	// /search returns session bodies, so like /sessions it must require authentication
	srv, _ := newTestServer(t, "secret")
	if code, _ := get(t, srv.URL+"/search?q=x", ""); code != 401 {
		t.Fatalf("searching without a token should be 401, got %d", code)
	}
	if code, _ := get(t, srv.URL+"/search?q=question", "secret"); code != 200 {
		t.Fatalf("searching with a token = %d", code)
	}
}

func TestParseSince(t *testing.T) {
	for _, raw := range []string{"30d", "12h", "90m", "2026-09-01", "2026-09-01T10:00:00Z"} {
		if _, err := parseSince(raw); err != nil {
			t.Errorf("parseSince(%q) errored: %v", raw, err)
		}
	}
	for _, raw := range []string{"zzz", "", "-5x"} {
		if _, err := parseSince(raw); err == nil {
			t.Errorf("parseSince(%q) should have errored", raw)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	// Searching for "100%" must not turn into matching anything
	if got := escapeLike("100%"); got != `100\%` {
		t.Fatalf("escapeLike = %q", got)
	}
	if got := escapeLike("a_b"); got != `a\_b` {
		t.Fatalf("escapeLike = %q", got)
	}
}

func TestSearchStopsWhenCancelled(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_a.jsonl"),
		`{"type":"session","id":"s-hit","cwd":"/w/a"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"nginx"}]}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	api := newSessionQueryAPI(sources, 2)
	q := searchQuery{needle: "nginx", lowered: []byte("nginx"), limit: 10, perSession: 3}

	if live := api.search(context.Background(), q); live.matched != 1 || live.stopped {
		t.Fatalf("a live search should match and not report stopped: %+v", live)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := api.search(ctx, q)
	if !got.stopped {
		t.Fatal("a cancelled search should report stopped")
	}
	if len(got.results) != 0 {
		t.Fatalf("a cancelled search should produce no results, got %d", len(got.results))
	}
}

func TestSearchFileStopsMidFile(t *testing.T) {
	// Every line matches, so an uncancelled scan necessarily runs to the end. Cancellation
	// is sampled every cancelCheckLines rather than tested per line, so the scan stops
	// within one sample window instead of at an exact line.
	path := filepath.Join(t.TempDir(), "big.jsonl")
	total := 4 * cancelCheckLines
	lines := make([]string, 0, total)
	for i := 0; i < total; i++ {
		lines = append(lines, `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"nginx"}]}}`)
	}
	write(t, path, lines...)

	q := searchQuery{needle: "nginx", lowered: []byte("nginx"), perSession: total + 1}
	if full := searchFile(context.Background(), path, q); len(full) != total {
		t.Fatalf("an uncancelled scan should read the whole file: %d hits, want %d", len(full), total)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if stopped := searchFile(ctx, path, q); len(stopped) > cancelCheckLines {
		t.Fatalf("a cancelled scan should stop within one sample window, got %d hits", len(stopped))
	}
}

// TestSearchSkipsMetadataFields: a needle that lands only in an id or a timestamp is not
// a body hit — measured locally, "502" matched a timestamp's millisecond part and a
// uuid's tail often enough to dominate the first results page.
func TestSearchSkipsMetadataFields(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_meta.jsonl"),
		`{"type":"session","id":"s-meta","cwd":"/w"}`,
		// the hit is only in the uuid and the timestamp
		`{"type":"message","uuid":"abc-95020faa-xyz","timestamp":"2026-09-17T02:09:43.502Z","message":{"role":"user","content":[{"type":"text","text":"completely unrelated words"}]}}`,
		// the same needle in body text is a hit
		`{"type":"message","uuid":"u2","message":{"role":"assistant","content":[{"type":"text","text":"the server returned 502 Bad Gateway"}]}}`,
		// codex nests ids under payload
		`{"type":"response_item","payload":{"turn_id":"01a0b047-d043-7502-8","type":"message","role":"user","content":[{"type":"input_text","text":"another unrelated line"}]}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	api := newSessionQueryAPI(sources, 2)
	q := searchQuery{needle: "502", lowered: []byte("502"), limit: 10, perSession: 5}

	out := api.search(context.Background(), q)
	if len(out.results) != 1 {
		t.Fatalf("only the body hit should survive, got %d: %+v", len(out.results), out.results)
	}
	hit := out.results[0]["matches"].([]map[string]any)[0]
	if !strings.Contains(hit["snippet"].(string), "502 Bad Gateway") {
		t.Errorf("snippet = %q, want the body text", hit["snippet"])
	}
}
