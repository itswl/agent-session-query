package app

import (
	"context"
	"fmt"
	"github.com/itswl/agent-session-query/internal/source"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

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
	sources := []source.SessionSource{source.NewPiSource(root)}
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

	// per_session caps how many hits each session returns, and says so under its own
	// reason: the caller that wants the rest turns per_session, not limit
	_, body = get(t, srv.URL+"/search?q=nginx&per_session=1", "")
	if body["results"].([]any)[0].(map[string]any)["matchCount"] != float64(1) {
		t.Fatalf("per_session=1 had no effect: %v", body)
	}
	if cut := body["truncated"].(map[string]any); cut["hits"] != true || cut["sessions"] != false {
		t.Fatalf("per_session=1 cut hits, not sessions: %v", cut)
	}
	// limit caps how many sessions come back, but matched still reports the real total
	code, body = get(t, srv.URL+"/search?q=nginx&limit=0", "")
	if body["total"] != float64(0) || body["matched"] != float64(1) {
		t.Fatalf("matched should still be reported after limit truncates: %v", body)
	}
	if cut := body["truncated"].(map[string]any); cut["sessions"] != true || cut["hits"] != false || cut["scan"] != false {
		t.Fatalf("limit=0 is the count-only request; it cuts at the page but scans everything: %v", cut)
	}
	// Nothing found: neither reason is set
	_, body = get(t, srv.URL+"/search?q=averyunlikelyneedle", "")
	if body["total"] != float64(0) {
		t.Fatalf("no match = %v", body)
	}
	if cut := body["truncated"].(map[string]any); cut["sessions"] != false || cut["hits"] != false || cut["scan"] != false {
		t.Fatalf("nothing matched, so nothing was cut: %v", cut)
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

func TestSearchStopsWhenCancelled(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_a.jsonl"),
		`{"type":"session","id":"s-hit","cwd":"/w/a"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"nginx"}]}}`,
	)
	sources := []source.SessionSource{source.NewPiSource(root)}
	api := newSessionQueryAPI(sources, 2)
	q := source.SearchQuery{Needle: "nginx", Lowered: []byte("nginx"), Limit: 10, PerSession: 3}

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
	sources := []source.SessionSource{source.NewPiSource(root)}
	api := newSessionQueryAPI(sources, 2)
	q := source.SearchQuery{Needle: "502", Lowered: []byte("502"), Limit: 10, PerSession: 5}

	out := api.search(context.Background(), q)
	if len(out.results) != 1 {
		t.Fatalf("only the body hit should survive, got %d: %+v", len(out.results), out.results)
	}
	hit := out.results[0]["matches"].([]map[string]any)[0]
	if !strings.Contains(hit["snippet"].(string), "502 Bad Gateway") {
		t.Errorf("snippet = %q, want the body text", hit["snippet"])
	}
}

// TestMessagesAnchoredAt: ?at= positions the window at a point in time instead of at an
// end, which is how a search hit in the middle of a long session becomes reachable — the
// stream otherwise only ever loads one end.
func TestMessagesAnchoredAt(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_anchor.jsonl"),
		`{"type":"session","id":"s-anchor","cwd":"/w"}`,
		`{"type":"message","id":"a1","timestamp":"2026-09-01T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"oldest"}]}}`,
		`{"type":"message","id":"a2","timestamp":"2026-09-02T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"second"}]}}`,
		`{"type":"message","id":"a3","timestamp":"2026-09-03T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"middle"}]}}`,
		`{"type":"message","id":"a4","timestamp":"2026-09-04T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"fourth"}]}}`,
		`{"type":"message","id":"a5","timestamp":"2026-09-05T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"newest"}]}}`,
	)
	sources := []source.SessionSource{source.NewPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 0), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)

	texts := func(body map[string]any) []string {
		out := []string{}
		for _, raw := range body["messages"].([]any) {
			m := raw.(map[string]any)
			for _, b := range m["content"].([]any) {
				if blk := b.(map[string]any); blk["type"] == "text" {
					out = append(out, blk["content"].(string))
				}
			}
		}
		return out
	}

	// Ascending from the anchor: it starts there and runs forward
	_, body := get(t, srv.URL+"/sessions/s-anchor/messages?limit=2&order=asc&at=2026-09-03T00:00:00Z", "")
	if got := texts(body); len(got) != 2 || got[0] != "middle" || got[1] != "fourth" {
		t.Fatalf("asc from the anchor = %v, want [middle fourth]", got)
	}

	// Descending: the window ends at the anchor
	_, body = get(t, srv.URL+"/sessions/s-anchor/messages?limit=2&order=desc&at=2026-09-03T00:00:00Z", "")
	if got := texts(body); len(got) != 2 || got[0] != "second" || got[1] != "middle" {
		t.Fatalf("desc to the anchor = %v, want [second middle]", got)
	}

	// An anchor before everything: ascending gets the head
	_, body = get(t, srv.URL+"/sessions/s-anchor/messages?limit=2&order=asc&at=2020-01-01T00:00:00Z", "")
	if got := texts(body); len(got) != 2 || got[0] != "oldest" {
		t.Fatalf("asc from before the session = %v, want the head", got)
	}

	// An anchor after everything: descending gets the tail
	_, body = get(t, srv.URL+"/sessions/s-anchor/messages?limit=2&order=desc&at=2030-01-01T00:00:00Z", "")
	if got := texts(body); len(got) != 2 || got[1] != "newest" {
		t.Fatalf("desc from after the session = %v, want the tail", got)
	}

	// An epoch is accepted too, and a bad value is a 400 rather than a silent ignore
	if code, _ := get(t, srv.URL+"/sessions/s-anchor/messages?at=1789000000", ""); code != 200 {
		t.Fatalf("an epoch anchor = %d", code)
	}
	if code, _ := get(t, srv.URL+"/sessions/s-anchor/messages?at=nonsense", ""); code != 400 {
		t.Fatalf("a bad anchor should be 400, got %d", code)
	}
}

// TestSearchScopedAndByRole: two things a search client could not ask for. Scoping to one
// session turns "where in this session did we discuss X" from 332 paged requests into
// one; the role filter is applied while collecting rather than after, so per_session
// counts hits that match instead of hits that merely came first.
func TestSearchScopedAndByRole(t *testing.T) {
	root := t.TempDir()
	// The target session: three assistant hits, then a user hit. With per_session=1 and
	// role=user, a filter applied after collection would find nothing.
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_target.jsonl"),
		`{"type":"session","id":"s-target","cwd":"/w/target"}`,
		`{"type":"message","id":"a1","timestamp":"2026-09-01T10:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"nginx one"}]}}`,
		`{"type":"message","id":"a2","timestamp":"2026-09-01T10:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"nginx two"}]}}`,
		`{"type":"message","id":"a3","timestamp":"2026-09-01T10:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"nginx three"}]}}`,
		`{"type":"message","id":"u1","timestamp":"2026-09-01T10:03:00Z","message":{"role":"user","content":[{"type":"text","text":"did you set up the nginx proxy yet"}]}}`,
	)
	// Another session with the same term, which must not appear in a scoped search
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-01_other.jsonl"),
		`{"type":"session","id":"s-other","cwd":"/w/other"}`,
		`{"type":"message","id":"b1","timestamp":"2026-09-02T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"nginx elsewhere"}]}}`,
	)

	sources := []source.SessionSource{source.NewPiSource(root)}
	api := newSessionQueryAPI(sources, 0)
	base := source.SearchQuery{Needle: "nginx", Lowered: []byte("nginx"), Limit: 10, PerSession: 10}

	// Global: both sessions
	if out := api.search(context.Background(), base); out.matched != 2 {
		t.Fatalf("unscoped search matched %d sessions, want 2", out.matched)
	}

	// Scoped: one session, and only it is scanned
	scoped := base
	scoped.Pattern = "s-target"
	out := api.search(context.Background(), scoped)
	if out.matched != 1 || out.scanned != 1 {
		t.Fatalf("scoped search matched %d / scanned %d, want 1/1", out.matched, out.scanned)
	}
	if got := out.results[0]["sessionId"]; got != "s-target" {
		t.Fatalf("scoped search returned %v", got)
	}

	// A pattern that names nothing searches nothing, rather than falling back to everything
	missing := base
	missing.Pattern = "no-such-session"
	if out := api.search(context.Background(), missing); out.matched != 0 || out.scanned != 0 {
		t.Fatalf("an unmatched pattern scanned %d and matched %d", out.scanned, out.matched)
	}

	// Role, with a per_session small enough that filtering afterwards would miss the
	// user hit behind three assistant ones
	byRole := base
	byRole.Pattern = "s-target"
	byRole.PerSession = 1
	byRole.Role = "assistant"
	if out := api.search(context.Background(), byRole); len(out.results) != 1 {
		t.Fatalf("role=assistant found %d sessions", len(out.results))
	} else if hit := out.results[0]["matches"].([]map[string]any)[0]; hit["role"] != "assistant" {
		t.Fatalf("role=assistant returned a %v hit", hit["role"])
	}

	byRole.Role = "user"
	out = api.search(context.Background(), byRole)
	if len(out.results) != 1 {
		t.Fatalf("role=user found %d sessions; the filter is cutting after per_session", len(out.results))
	}
	hit := out.results[0]["matches"].([]map[string]any)[0]
	if hit["role"] != "user" || !strings.Contains(hit["snippet"].(string), "nginx proxy") {
		t.Fatalf("role=user returned %v", hit)
	}
}

// truncated.hits says per_session cut something the caller can see. A session that limit
// dropped from the results entirely is the other reason — sessions — and must not raise
// hits as well, or the caller raises per_session and nothing changes, because every
// session it can actually see was complete.
func TestSearchHitsTruncationOnlyCountsReturnedSessions(t *testing.T) {
	root := t.TempDir()
	// Newest, and it matches exactly once
	write(t, filepath.Join(root, "p", "2026-02-02T00-00-00_new.jsonl"),
		`{"type":"session","id":"s-new","cwd":"/w/new"}`,
		`{"type":"message","id":"a1","timestamp":"2026-02-02T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"one kafka here"}]}}`,
	)
	// Older, and it matches three times
	write(t, filepath.Join(root, "p", "2026-02-01T00-00-00_old.jsonl"),
		`{"type":"session","id":"s-old","cwd":"/w/old"}`,
		`{"type":"message","id":"b1","timestamp":"2026-02-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"kafka one"}]}}`,
		`{"type":"message","id":"b2","timestamp":"2026-02-01T00:00:02Z","message":{"role":"user","content":[{"type":"text","text":"kafka two"}]}}`,
		`{"type":"message","id":"b3","timestamp":"2026-02-01T00:00:03Z","message":{"role":"user","content":[{"type":"text","text":"kafka three"}]}}`,
	)
	sources := []source.SessionSource{source.NewPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)

	// limit=1 keeps only the newest session, which has one hit and so is complete;
	// per_session=2 is never reached by anything the caller receives
	_, body := get(t, srv.URL+"/search?q=kafka&limit=1&per_session=2", "")
	results := body["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["sessionId"] != "s-new" {
		t.Fatalf("expected only the newest session back: %v", body)
	}
	if results[0].(map[string]any)["matchCount"] != float64(1) {
		t.Fatalf("the returned session has one hit, so nothing about it was cut: %v", results[0])
	}
	cut := body["truncated"].(map[string]any)
	if cut["sessions"] != true {
		t.Errorf("a session was dropped by limit, so sessions must be true: %v", cut)
	}
	if cut["hits"] != false {
		t.Errorf("no returned session lost a hit, so hits must be false: %v", cut)
	}
}

// TestSearchStopsOnceLimitIsReached: the scan is bounded by the page, not by the corpus.
// Candidates are newest-first and jobs are handed out in that order, so once limit sessions
// have matched, nothing still unscanned could have reached the results — the scan stops,
// truncated.scan says so, and matched becomes "at least this many".
func TestSearchStopsOnceLimitIsReached(t *testing.T) {
	// The stop is a scheduling decision: with a worker per candidate the dispatcher can
	// hand every session out before any scan completes and never observe the limit being
	// reached. Pinning a small pool makes the stop deterministic, which is what this test
	// is about; the results themselves never depend on it.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	root := t.TempDir()
	// Far more sessions than any worker pool hands out before the first two matches land
	const sessions = 300
	for i := 0; i < sessions; i++ {
		write(t, filepath.Join(root, "p", fmt.Sprintf("s%03d.jsonl", i)),
			`{"type":"session","id":"sid-`+strconv.Itoa(i)+`","cwd":"/w"}`,
			`{"type":"message","id":"m1","timestamp":"`+strconv.Itoa(1700000000+i)+`","message":{"role":"user","content":[{"type":"text","text":"the kafka question"}]}}`,
		)
	}
	sources := []source.SessionSource{source.NewPiSource(root)}
	api := newSessionQueryAPI(sources, 0)
	q := source.SearchQuery{Needle: "kafka", Lowered: []byte("kafka"), Limit: 2, PerSession: 3}

	out := api.search(context.Background(), q)
	if len(out.results) != 2 {
		t.Fatalf("limit=2 returned %d sessions", len(out.results))
	}
	if !out.scanStopped || !out.sessionsCut || out.stopped {
		t.Fatalf("a filled page must report the scan stop: %+v", out)
	}
	if out.scanned < 2 || out.scanned >= sessions {
		t.Fatalf("scanned %d of %d; the scan is not bounded by the page", out.scanned, sessions)
	}
	if out.matched < 2 {
		t.Fatalf("matched %d, want at least the two returned", out.matched)
	}
	// Newest first still holds under the early stop: the page is the two newest sessions
	if got := out.results[0]["sessionId"]; got != "sid-299" {
		t.Fatalf("the page does not start at the newest session: %v", got)
	}
	if got := out.results[1]["sessionId"]; got != "sid-298" {
		t.Fatalf("the second row is not the second newest: %v", got)
	}

	// limit=0 is the count-only request: it must keep scanning to the end
	countOnly := q
	countOnly.Limit = 0
	all := api.search(context.Background(), countOnly)
	if all.scanStopped || all.matched != sessions || all.scanned != sessions {
		t.Fatalf("limit=0 must count the whole corpus: matched %d scanned %d stopped %v",
			all.matched, all.scanned, all.scanStopped)
	}

	// ... and the HTTP shape carries the reason: truncated.scan
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: api, maxConnections: 50,
	}))
	t.Cleanup(srv.Close)
	_, body := get(t, srv.URL+"/search?q=kafka&limit=2", "")
	cut := body["truncated"].(map[string]any)
	if cut["scan"] != true || cut["sessions"] != true {
		t.Fatalf("a stopped scan must be reported: %v", cut)
	}
	if results := body["results"].([]any); len(results) != 2 {
		t.Fatalf("limit=2 returned %d results over HTTP", len(results))
	}
}
