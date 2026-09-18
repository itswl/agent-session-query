package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newPackServer(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	// Three sessions on three days, in the order they happened
	write(t, filepath.Join(root, "proj", "2026-01-01T00-00-00_a.jsonl"),
		`{"type":"session","id":"pack-a","cwd":"/w/proj"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"set up the proxy"}],"timestamp":"2026-09-01T10:00:00Z"}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"proxy is up"}],"timestamp":"2026-09-01T10:01:00Z"}}`,
	)
	write(t, filepath.Join(root, "proj", "2026-01-02T00-00-00_b.jsonl"),
		`{"type":"session","id":"pack-b","cwd":"/w/proj"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"the proxy is down again"}],"timestamp":"2026-09-02T10:00:00Z"}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"restarted it"}],"timestamp":"2026-09-02T10:01:00Z"}}`,
	)
	write(t, filepath.Join(root, "other", "2026-01-03T00-00-00_c.jsonl"),
		`{"type":"session","id":"pack-c","cwd":"/w/other"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"unrelated"}],"timestamp":"2026-09-03T10:00:00Z"}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 0), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPackOrdersByTimeAndCarriesThePair: a pack answers "how did this get here", so it runs
// oldest first — the opposite of the session list, which answers "what was I doing".
func TestPackOrdersByTimeAndCarriesThePair(t *testing.T) {
	srv := newPackServer(t)

	resp, err := http.Get(srv.URL + "/export")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("pack = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := readBody(t, resp)

	if !strings.Contains(body, "**Sessions**: 3 (every session)") {
		t.Errorf("the pack does not state its scope:\n%s", firstLines(body, 12))
	}
	if !strings.Contains(body, "**Range**: 2026-09-01 → 2026-09-03") {
		t.Errorf("the pack does not state its range:\n%s", firstLines(body, 12))
	}
	// The notice is the part that stops a reader taking the last entry as the current truth
	if !strings.Contains(body, "not a summary") || !strings.Contains(body, "currently") {
		t.Errorf("the pack does not say what it is:\n%s", firstLines(body, 14))
	}

	// Each entry carries the ask and the outcome, and the two sessions in /w/proj come
	// before the unrelated one that happened later
	first := strings.Index(body, "set up the proxy")
	second := strings.Index(body, "the proxy is down again")
	third := strings.Index(body, "unrelated")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("an entry is missing its ask:\n%s", body)
	}
	if !(first < second && second < third) {
		t.Errorf("entries are not in time order: %d %d %d", first, second, third)
	}
	if !strings.Contains(body, "proxy is up") || !strings.Contains(body, "restarted it") {
		t.Errorf("an entry is missing its outcome:\n%s", body)
	}
}

func TestPackFiltersAndLimits(t *testing.T) {
	srv := newPackServer(t)

	// A project filter narrows to the two in /w/proj
	resp, _ := http.Get(srv.URL + "/export?project=proj")
	body := readBody(t, resp)
	if !strings.Contains(body, "**Sessions**: 2 ") || strings.Contains(body, "unrelated") {
		t.Errorf("project filter did not narrow:\n%s", firstLines(body, 8))
	}

	// A limit keeps the most recent and says how many were left out
	resp, _ = http.Get(srv.URL + "/export?project=proj&limit=1")
	body = readBody(t, resp)
	if !strings.Contains(body, "the 1 most recent of 2 matching") {
		t.Errorf("a limited pack does not say what it dropped:\n%s", firstLines(body, 8))
	}
	if !strings.Contains(body, "the proxy is down again") || strings.Contains(body, "set up the proxy") {
		t.Errorf("a limit should keep the most recent, not the oldest")
	}

	// A selection that matches nothing is a 404, not an empty document
	if resp, _ := http.Get(srv.URL + "/export?project=nothing-matches"); resp.StatusCode != 404 {
		t.Errorf("an empty selection = %d, want 404", resp.StatusCode)
	}
	// A bad mode or source is refused rather than guessed
	if resp, _ := http.Get(srv.URL + "/export?mode=x"); resp.StatusCode != 400 {
		t.Errorf("mode=x = %d, want 400", resp.StatusCode)
	}
	if resp, _ := http.Get(srv.URL + "/export?source=nope"); resp.StatusCode != 400 {
		t.Errorf("source=nope = %d, want 400", resp.StatusCode)
	}
}

// TestPackJSONL: the index as data — a header record, then one per session, each carrying
// the id a program follows to the transcript rather than the transcript itself.
func TestPackJSONL(t *testing.T) {
	srv := newPackServer(t)
	resp, err := http.Get(srv.URL + "/export?format=jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Errorf("Content-Type = %q", ct)
	}
	lines := strings.Split(strings.TrimSpace(readBody(t, resp)), "\n")
	if len(lines) != 4 { // pack + 3 sessions
		t.Fatalf("got %d records, want 4:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	var header, session map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &session); err != nil {
		t.Fatal(err)
	}
	if header["type"] != "pack" || header["sessions"] != float64(3) || header["mode"] != "index" {
		t.Errorf("header = %v", header)
	}
	if session["type"] != "session" || session["asked"] != "set up the proxy" {
		t.Errorf("first session record = %v", session)
	}
	if session["concluded"] != "proxy is up" {
		t.Errorf("the record does not carry the outcome: %v", session["concluded"])
	}
	if !strings.HasPrefix(session["transcript"].(string), "/sessions/pack-a/export") {
		t.Errorf("the record does not point at its transcript: %v", session["transcript"])
	}
}

// TestPackExtendsAShortOpening: an opening message is not always a statement of intent —
// "评审一下" says nothing about what is being reviewed, because it was said into a
// conversation that already had context. The next user turns usually say what it was
// about, so a short opening is extended with them; a long one is left alone.
func TestPackExtendsAShortOpening(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_short.jsonl"),
		`{"type":"session","id":"s-short","cwd":"/w"}`,
		`{"type":"message","id":"m1","timestamp":"2026-09-01T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"评审一下"}]}}`,
		`{"type":"message","id":"m2","timestamp":"2026-09-01T10:01:00Z","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"which repository?"}]}}`,
		// A machine-assembled row: role=user but nobody said it
		`{"type":"message","id":"m3","timestamp":"2026-09-01T10:02:00Z","message":{"role":"user","content":[{"type":"text","text":"<local-command-caveat>Caveat: generated while running local commands"}]}}`,
		`{"type":"message","id":"m4","timestamp":"2026-09-01T10:03:00Z","message":{"role":"user","content":[{"type":"text","text":"就是 hookstack 那个仓库，看有没有设计问题"}]}}`,
	)
	// A terse opening whose own message carries on: the sentence that identifies the work
	// is the second paragraph
	write(t, filepath.Join(root, "p", "2026-01-03T00-00-00_terse.jsonl"),
		`{"type":"session","id":"s-terse","cwd":"/w"}`,
		`{"type":"message","id":"m1","timestamp":"2026-09-03T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"参考评审一下\n\n✦ larkin 的未来方向不是走向企业级大而全，而是纵向做深"}]}}`,
		`{"type":"message","id":"m2","timestamp":"2026-09-03T10:01:00Z","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"ok"}]}}`,
	)
	// A session whose opening already says what it wants
	write(t, filepath.Join(root, "p", "2026-01-02T00-00-00_long.jsonl"),
		`{"type":"session","id":"s-long","cwd":"/w"}`,
		`{"type":"message","id":"m1","timestamp":"2026-09-02T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"把 auth 模块的重试逻辑改成指数退避，最大 30 秒"}]}}`,
		`{"type":"message","id":"m2","timestamp":"2026-09-02T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"另外记得补测试"}]}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 0), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)

	resp, _ := http.Get(srv.URL + "/export")
	body := readBody(t, resp)

	asks := []string{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "- **Asked**: ") {
			asks = append(asks, strings.TrimPrefix(line, "- **Asked**: "))
		}
	}
	if len(asks) != 3 {
		t.Fatalf("got %d Asked lines, want 3:\n%s", len(asks), firstLines(body, 30))
	}
	// Oldest first: 09-01 short, 09-02 already complete, 09-03 terse-but-continues
	short, long, terse := asks[0], asks[1], asks[2]

	if !strings.Contains(short, "评审一下") || !strings.Contains(short, "就是 hookstack 那个仓库") {
		t.Errorf("a short opening was not extended with what followed: %q", short)
	}
	if strings.Contains(short, "local-command-caveat") {
		t.Errorf("a machine-assembled row was taken for a user turn: %q", short)
	}
	if !strings.HasPrefix(long, "把 auth 模块的重试逻辑") {
		t.Errorf("the long opening is not first on its line: %q", long)
	}
	if strings.Contains(long, "另外记得补测试") {
		t.Errorf("an opening that already says what it wants was extended anyway: %q", long)
	}
	if !strings.Contains(terse, "参考评审一下") || !strings.Contains(terse, "larkin 的未来方向") {
		t.Errorf("a terse first line was not followed into its own message: %q", terse)
	}
	if strings.Count(terse, "参考评审一下") != 1 {
		t.Errorf("the opening is repeated on its own line: %q", terse)
	}
}
