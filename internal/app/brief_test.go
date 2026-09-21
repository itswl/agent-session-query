package app

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// briefFixture writes a deliberately mixed session: a command-plumbing row before
// anything, one full round (ask, Write tool call, tool result, assistant conclusion),
// and a trailing ask with no reply — an interrupted round.
func briefFixture(t *testing.T) *SessionQueryAPI {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "rrrr.jsonl"),
		`{"type":"user","sessionId":"rrrr","cwd":"/w","timestamp":"2026-09-21T07:59:00Z","message":{"role":"user","content":"<command-name>/model</command-name><command-message>model</command-message>"}}`,
		`{"type":"user","sessionId":"rrrr","cwd":"/w","timestamp":"2026-09-21T08:00:00Z","message":{"role":"user","content":"ask one"}}`,
		`{"type":"assistant","timestamp":"2026-09-21T08:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"/x/a.css"}}]}}`,
		`{"type":"user","timestamp":"2026-09-21T08:02:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"}]}]}}`,
		`{"type":"assistant","timestamp":"2026-09-21T08:03:00Z","message":{"role":"assistant","content":[{"type":"text","text":"round one finished"}]}}`,
		`{"type":"user","sessionId":"rrrr","cwd":"/w","timestamp":"2026-09-21T09:00:00Z","message":{"role":"user","content":"ask two"}}`,
	)
	api := newSessionQueryAPI([]SessionSource{newClaudeSource(root)}, 0)
	return api
}

func TestSplitRounds(t *testing.T) {
	api := briefFixture(t)
	sr, err := api.roundsOf("rrrr", "")
	if err != nil {
		t.Fatal(err)
	}
	rounds := sr.rounds
	if len(rounds) != 2 {
		t.Fatalf("rounds = %d, want 2 (the plumbing row starts nothing, the trailing ask is its own round)", len(rounds))
	}
	if rounds[0].asked != "ask one" || rounds[0].outcome != "round one finished" {
		t.Fatalf("round 1 = %+v", rounds[0])
	}
	if len(rounds[0].files) != 1 || rounds[0].files[0] != "/x/a.css" {
		t.Fatalf("round 1 files = %v", rounds[0].files)
	}
	if rounds[0].toolCalls != 1 || rounds[0].kinds["write"] != 1 {
		t.Fatalf("round 1 tools = %d %v", rounds[0].toolCalls, rounds[0].kinds)
	}
	if rounds[0].interrupted {
		t.Fatal("round 1 ended with an assistant reply, not interrupted")
	}
	if !rounds[1].interrupted {
		t.Fatal("round 2 ends on an unanswered ask: interrupted")
	}
}

func TestSessionBriefSelection(t *testing.T) {
	api := briefFixture(t)
	// default: the latest round, which is the interrupted one
	brief, err := api.sessionBrief("rrrr", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Session brief:", "Round 2 — interrupted", "ask two", "State at the end", "sessionId: rrrr"} {
		if !strings.Contains(brief, want) {
			t.Errorf("default brief lacks %q", want)
		}
	}
	// round=1 reaches the earlier round
	brief, err = api.sessionBrief("rrrr", "", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Round 1", "ask one", "/x/a.css", "round one finished"} {
		if !strings.Contains(brief, want) {
			t.Errorf("round 1 brief lacks %q", want)
		}
	}
	if strings.Contains(brief, "- Asked: ask two") {
		t.Error("round 1 brief leaked round 2's ask into the detail section")
	}
	// at= picks the round a timestamp falls in
	brief, err = api.sessionBrief("rrrr", "", 0, "2026-09-21T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, "Round 2") {
		t.Error("at=09:00 should brief round 2")
	}
	// errors
	if _, err := api.sessionBrief("rrrr", "", 5, ""); err == nil || !strings.Contains(err.Error(), "1..2") {
		t.Fatalf("round out of range = %v", err)
	}
	if _, err := api.sessionBrief("rrrr", "", 0, "2020-01-01T00:00:00Z"); err == nil {
		t.Fatal("an at outside every round should error")
	}
	if _, err := api.sessionBrief("zzzz", "", 0, ""); err == nil || err != errNoSession {
		t.Fatalf("no-session = %v", err)
	}
}

func TestSessionRoundsIndex(t *testing.T) {
	api := briefFixture(t)
	out, err := api.sessionRounds("rrrr", "")
	if err != nil {
		t.Fatal(err)
	}
	if out["total"] != 2 {
		t.Fatalf("total = %v", out["total"])
	}
	rounds := out["rounds"].([]map[string]any)
	if rounds[0]["asked"] != "ask one" || rounds[1]["interrupted"] != true {
		t.Fatalf("rounds = %v", rounds)
	}
}

func TestBriefHTTPRoutes(t *testing.T) {
	srv, _ := newTestServer(t, "secret")
	// The fixture session is one round: ask + reply
	code, body := get(t, srv.URL+"/sessions/sess-1/rounds", "secret")
	if code != 200 {
		t.Fatalf("rounds = %d %v", code, body)
	}
	if body["total"] != float64(1) {
		t.Fatalf("total = %v", body["total"])
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sessions/sess-1/brief", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("brief = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(raw), "# Session brief:") {
		t.Fatalf("brief body = %q", string(raw)[:min(200, len(raw))])
	}
	// An out-of-range round is a 400 with the reason
	code, body = get(t, srv.URL+"/sessions/sess-1/brief?round=9", "secret")
	if code != 400 {
		t.Fatalf("brief round=9 = %d %v", code, body)
	}
}

func TestCapTextIsRuneSafe(t *testing.T) {
	cjk := capText("帮我把配置文件抽出来重新组织一下结构", 5)
	if !utf8.ValidString(cjk) {
		t.Fatalf("capText produced invalid UTF-8: %q", cjk)
	}
	if cjk != "帮我把配置 …" {
		t.Fatalf("cjk cut = %q", cjk)
	}
	if short := capText("one two three four", 7); short != "one two …" {
		t.Fatalf("ascii cut = %q", short)
	}
	if short := capText("short", 50); short != "short" {
		t.Fatalf("short string changed: %q", short)
	}
}

func TestToolKindOf(t *testing.T) {
	cases := map[string]string{
		"Bash":             "exec",
		"run_terminal_cmd": "exec",
		"sh":               "exec",
		"Write":            "write",
		"NotebookEdit":     "write",
		"Edit":             "write",
		"Read":             "read",
		"open_file":        "read",
		"Grep":             "search",
		"glob_list":        "search",
		"WebFetch":         "net",
		"curl_page":        "net",
		"dispatch_agent":   "agent",
		"Task":             "agent",
		"publish":          "other",
		"runbook":          "other",
		"flash":            "other",
	}
	for name, want := range cases {
		if got := toolKindOf(name); got != want {
			t.Errorf("toolKindOf(%q) = %q, want %q", name, got, want)
		}
	}
}

// inflatedCountSource lies about the session's length the way a real final result does
// when the session ran past the scan cap.
type inflatedCountSource struct {
	SessionSource
}

func (s inflatedCountSource) Final(r record) map[string]any {
	out := s.SessionSource.Final(r)
	out["messageCount"] = 999999
	return out
}

func TestSessionRoundsSurfacesPartialScan(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "rrrr.jsonl"),
		`{"type":"user","sessionId":"rrrr","cwd":"/w","message":{"role":"user","content":"ask"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	)
	api := newSessionQueryAPI([]SessionSource{inflatedCountSource{newClaudeSource(root)}}, 0)
	out, err := api.sessionRounds("rrrr", "")
	if err != nil {
		t.Fatal(err)
	}
	if out["partial"] != true || out["messagesTotal"] != 999999 {
		t.Fatalf("partial = %v, total = %v", out["partial"], out["messagesTotal"])
	}
	brief, err := api.sessionBrief("rrrr", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, "scanned the latest 2 of 999999 messages (partial)") {
		t.Error("the brief header does not disclose the partial scan")
	}
}

func TestSessionBriefSourceDisambiguation(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeClaudeFixture(t, dirA, "proj", "cccc", "/w")
	writeClaudeFixture(t, dirB, "proj", "cccc", "/w")
	api := newSessionQueryAPI([]SessionSource{
		labeledSource{SessionSource: newClaudeSource(dirA), mode: "claude:a"},
		labeledSource{SessionSource: newClaudeSource(dirB), mode: "claude:b"},
	}, 0)
	for _, source := range []string{"claude:a", "claude:b"} {
		brief, err := api.sessionBrief("cccc", source, 0, "")
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		if !strings.Contains(brief, "source "+source) {
			t.Errorf("%s brief names the wrong source:\n%s", source, brief)
		}
		if !strings.Contains(brief, strings.TrimPrefix(source, "claude:")+":/w") {
			t.Errorf("%s brief lost the label prefix on the project:\n%s", source, brief)
		}
	}
}
