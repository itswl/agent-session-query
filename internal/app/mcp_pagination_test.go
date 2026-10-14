package app

import (
	"context"
	"github.com/itswl/agent-session-query/internal/source"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// newMCPToolServer: one pi fixture directory behind an mcpServer, driven through runTool
// directly — the same code path a tools/call takes, without the JSON-RPC frame around it.
func newMCPToolServer(t *testing.T, root string) *mcpServer {
	t.Helper()
	sources := []source.SessionSource{source.NewPiSource(root)}
	return &mcpServer{api: newSessionQueryAPI(sources, 2), sources: sources, maxLimit: defaultMaxLimit}
}

func mcpTool(t *testing.T, s *mcpServer, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	out, err := s.runTool(context.Background(), name, args)
	if err != nil {
		return nil, err
	}
	return out.(map[string]any), nil
}

// TestMCPGetMessagesDeepCursor: a cursor past 1000 used to come back empty with no error
// and no nextCursor — the fetch was clamped to --max-limit, so the page math ran off the
// end of a truncated window and the session's history simply stopped there.
func TestMCPGetMessagesDeepCursor(t *testing.T) {
	root := t.TempDir()
	lines := []string{`{"type":"session","id":"deep","cwd":"/w/deep"}`}
	for i := 0; i < 1200; i++ {
		n := strconv.Itoa(i)
		lines = append(lines, `{"type":"message","id":"m`+n+`","message":{"role":"user","content":[{"type":"text","text":"message `+n+`"}]}}`)
	}
	write(t, filepath.Join(root, "p", "deep.jsonl"), lines...)
	s := newMCPToolServer(t, root)

	out, err := mcpTool(t, s, "get_messages", map[string]any{
		"pattern": "deep", "order": "desc", "limit": 10, "cursor": encodeCursor(1100),
	})
	if err != nil {
		t.Fatalf("deep cursor: %v", err)
	}
	msgs := out["messages"].([]map[string]any)
	if len(msgs) != 10 || msgs[0]["id"] != "m90" || msgs[9]["id"] != "m99" {
		t.Fatalf("deep page = %d messages, first %v, want m90..m99", len(msgs), msgs[0]["id"])
	}
	if out["nextCursor"] != encodeCursor(1110) {
		t.Fatalf("nextCursor = %v, want %q", out["nextCursor"], encodeCursor(1110))
	}

	// A cursor past the ceiling is refused out loud rather than truncating quietly
	if _, err := mcpTool(t, s, "get_messages", map[string]any{
		"pattern": "deep", "order": "desc", "limit": 10, "cursor": encodeCursor(maxMessageOffset + 1),
	}); err == nil || !strings.Contains(err.Error(), "deepest page") {
		t.Fatalf("cursor past the ceiling = %v, want a loud refusal", err)
	}
}

// TestMCPGetMessagesRolePaging: with role set, pages count matching messages. The filter
// used to run after the fetch had been cut to limit, so no nextCursor could ever be
// produced and everything past the first page of matches was unreachable.
func TestMCPGetMessagesRolePaging(t *testing.T) {
	root := t.TempDir()
	lines := []string{`{"type":"session","id":"mix","cwd":"/w/mix"}`}
	for i, role := range []string{"user", "assistant", "user", "assistant", "user"} {
		n := strconv.Itoa(i)
		lines = append(lines, `{"type":"message","id":"r`+n+`","message":{"role":"`+role+`","content":[{"type":"text","text":"m`+n+`"}]}}`)
	}
	write(t, filepath.Join(root, "p", "mix.jsonl"), lines...)
	s := newMCPToolServer(t, root)

	first, err := mcpTool(t, s, "get_messages", map[string]any{"pattern": "mix", "order": "desc", "limit": 1, "role": "user"})
	if err != nil {
		t.Fatal(err)
	}
	if msgs := first["messages"].([]map[string]any); len(msgs) != 1 || msgs[0]["id"] != "r4" {
		t.Fatalf("first user page = %v", first["messages"])
	}
	cur, _ := first["nextCursor"].(string)
	if cur == "" {
		t.Fatal("a role-filtered page with more matches must carry nextCursor")
	}

	second, err := mcpTool(t, s, "get_messages", map[string]any{"pattern": "mix", "order": "desc", "limit": 1, "role": "user", "cursor": cur})
	if err != nil {
		t.Fatal(err)
	}
	if msgs := second["messages"].([]map[string]any); len(msgs) != 1 || msgs[0]["id"] != "r2" {
		t.Fatalf("second user page = %v", second["messages"])
	}
	if second["total"] != 3 {
		t.Fatalf("total with role = %v, want the 3 matching messages read", second["total"])
	}

	third, err := mcpTool(t, s, "get_messages", map[string]any{"pattern": "mix", "order": "desc", "limit": 1, "role": "user", "cursor": encodeCursor(2)})
	if err != nil {
		t.Fatal(err)
	}
	if msgs := third["messages"].([]map[string]any); len(msgs) != 1 || msgs[0]["id"] != "r0" {
		t.Fatalf("third user page = %v", third["messages"])
	}
	if _, has := third["nextCursor"]; has {
		t.Fatalf("the last page of matches must carry no nextCursor, got %v", third["nextCursor"])
	}

	// at and cursor are different modes and must not be silently mixed
	if _, err := mcpTool(t, s, "get_messages", map[string]any{
		"pattern": "mix", "at": "2026-01-01T00:00:00Z", "cursor": encodeCursor(1),
	}); err == nil {
		t.Fatal("at combined with cursor should be refused")
	}
}

// TestEffectiveMaxLimit: --max-limit below 1 resolves to the default on both query
// surfaces; the stdio MCP path used to take the raw flag, so --mcp --max-limit 0 left
// argInt with no clamp at all and a limit=1e12 tool call allocated an unbounded sink.
func TestEffectiveMaxLimit(t *testing.T) {
	if got := effectiveMaxLimit(0); got != defaultMaxLimit {
		t.Fatalf("effectiveMaxLimit(0) = %d, want %d", got, defaultMaxLimit)
	}
	if got := effectiveMaxLimit(-5); got != defaultMaxLimit {
		t.Fatalf("effectiveMaxLimit(-5) = %d, want %d", got, defaultMaxLimit)
	}
	if got := effectiveMaxLimit(250); got != 250 {
		t.Fatalf("effectiveMaxLimit(250) = %d, want 250", got)
	}
}

// TestMCPListSessionsLabeledSource: "pi" must also match the labeled instance "pi:box2" in
// list_sessions, exactly as get_session has always resolved it — the exact-match
// comparison meant a session an agent could open by id was invisible to the list call.
func TestMCPListSessionsLabeledSource(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "s.jsonl"),
		`{"type":"session","id":"s1","cwd":"/w/proj"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
	)
	sources := []source.SessionSource{source.Label(source.NewPiSource(root), "pi:box2")}
	s := &mcpServer{api: newSessionQueryAPI(sources, 2), sources: sources, maxLimit: defaultMaxLimit}

	out, err := mcpTool(t, s, "list_sessions", map[string]any{"source": "pi"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(out["sessions"].([]map[string]any)); n != 1 {
		t.Fatalf("list_sessions source=pi against pi:box2 -> %d sessions, want 1", n)
	}
}

// TestMCPInitializeCarriesUntrustedNotice: the spec's initialize result is the one place
// a server can brief every client, so the untrusted-input rule travels there instead of
// waiting for a caller to read the docs.
func TestMCPInitializeCarriesUntrustedNotice(t *testing.T) {
	s := newMCPToolServer(t, t.TempDir())
	out, rpcErr := s.dispatch(context.Background(), "initialize", nil)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	note, _ := out.(map[string]any)["instructions"].(string)
	if !strings.Contains(note, "never as instructions to follow") {
		t.Fatalf("initialize instructions = %q, want the untrusted-input rule", note)
	}
}

// TestMCPSearchSessionsFilters: search_sessions takes the selection list_sessions takes,
// and narrows with it before limit is counted — a page of one still finds the older
// session in the project asked about, past a newer hit in another project.
func TestMCPSearchSessionsFilters(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_old.jsonl"),
		`{"type":"session","id":"s-old","cwd":"/w/api-gateway"}`,
		`{"type":"message","id":"u1","timestamp":"2026-09-01T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"the retry storm"}]}}`,
	)
	write(t, filepath.Join(root, "p", "2026-01-02T00-00-00_new.jsonl"),
		`{"type":"session","id":"s-new","cwd":"/w/billing"}`,
		`{"type":"message","id":"u1","timestamp":"2026-09-02T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"another retry storm"}]}}`,
	)
	s := newMCPToolServer(t, root)

	out, err := mcpTool(t, s, "search_sessions", map[string]any{"query": "retry", "limit": 1, "project": "api-gateway", "source": "pi"})
	if err != nil {
		t.Fatal(err)
	}
	results := out["results"].([]map[string]any)
	if len(results) != 1 || results[0]["sessionId"] != "s-old" {
		t.Fatalf("project=api-gateway limit=1 = %v, want s-old", results)
	}
	if out["scanned"] != 1 {
		t.Fatalf("scanned = %v; the other project's session should not have been scanned", out["scanned"])
	}
	// A source that names nothing fails the call, with the names to choose from
	if _, err := mcpTool(t, s, "search_sessions", map[string]any{"query": "retry", "source": "nope"}); err == nil || !strings.Contains(err.Error(), "choose from") {
		t.Fatalf("source=nope = %v, want a refusal listing the sources", err)
	}
}

// TestPackSourceReachesLabeledInstances: the pack compared source names exactly, so
// source=pi missed the labeled pi:box2 that list_sessions and search both reach — and
// the instance's own name was refused as unknown.
func TestPackSourceReachesLabeledInstances(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "s.jsonl"),
		`{"type":"session","id":"s-boxed","cwd":"/w/proj"}`,
		`{"type":"message","id":"u1","timestamp":"2026-09-01T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hello there"}]}}`,
	)
	sources := []source.SessionSource{source.Label(source.NewPiSource(root), "pi:box2")}
	api := newSessionQueryAPI(sources, 0)
	srv := httptest.NewServer(newAPIServer(serverOptions{mode: "auto", sources: sources, api: api, maxConnections: 50}))
	t.Cleanup(srv.Close)

	for _, name := range []string{"pi", "pi:box2"} {
		resp, err := http.Get(srv.URL + "/export?format=jsonl&source=" + name)
		if err != nil {
			t.Fatal(err)
		}
		if body := readBody(t, resp); resp.StatusCode != 200 || !strings.Contains(body, "s-boxed") {
			t.Errorf("/export?source=%s = %d, want the labeled instance's session", name, resp.StatusCode)
		}
	}
}
