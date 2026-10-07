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
	brief, err := api.sessionBrief("rrrr", "", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Session brief:", "Round 2 — interrupted", "ask two", "State at the end", "sessionId: rrrr"} {
		if !strings.Contains(brief, want) {
			t.Errorf("default brief lacks %q", want)
		}
	}
	// round=1 reaches the earlier round
	brief, err = api.sessionBrief("rrrr", "", 1, "", "")
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
	brief, err = api.sessionBrief("rrrr", "", 0, "2026-09-21T09:00:00Z", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, "Round 2") {
		t.Error("at=09:00 should brief round 2")
	}
	// errors
	if _, err := api.sessionBrief("rrrr", "", 5, "", ""); err == nil || !strings.Contains(err.Error(), "1..2") {
		t.Fatalf("round out of range = %v", err)
	}
	if _, err := api.sessionBrief("rrrr", "", 0, "2020-01-01T00:00:00Z", ""); err == nil {
		t.Fatal("an at outside every round should error")
	}
	if _, err := api.sessionBrief("zzzz", "", 0, "", ""); err == nil || err != errNoSession {
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
	brief, err := api.sessionBrief("rrrr", "", 0, "", "")
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
		brief, err := api.sessionBrief("cccc", source, 0, "", "")
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

// TestBriefReadsTheTailNotTheHead: the scan is capped, and the cap must read the
// session's tail — a handoff brief cares about where things ended. With distinct early
// and latest asks and a shrunk cap, the earliest ask falls outside the scan entirely
// and the default brief is the latest round the tail contains.
func TestBriefReadsTheTailNotTheHead(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "tail.jsonl"),
		`{"type":"user","sessionId":"tail","cwd":"/w","timestamp":"2026-09-21T07:00:00Z","message":{"role":"user","content":"first topic ask"}}`,
		`{"type":"assistant","timestamp":"2026-09-21T07:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"first topic done"}]}}`,
		`{"type":"user","sessionId":"tail","cwd":"/w","timestamp":"2026-09-21T08:00:00Z","message":{"role":"user","content":"second topic ask"}}`,
		`{"type":"assistant","timestamp":"2026-09-21T08:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"second topic done"}]}}`,
		`{"type":"user","sessionId":"tail","cwd":"/w","timestamp":"2026-09-21T09:00:00Z","message":{"role":"user","content":"third topic ask"}}`,
	)
	previous := briefScanCap
	briefScanCap = 4
	defer func() { briefScanCap = previous }()

	api := newSessionQueryAPI([]SessionSource{newClaudeSource(root)}, 0)
	out, err := api.sessionRounds("tail", "")
	if err != nil {
		t.Fatal(err)
	}
	if out["partial"] != true || out["messagesScanned"] != 4 || out["messagesTotal"] != 5 {
		t.Fatalf("scan bounds = %v / %v / %v", out["partial"], out["messagesScanned"], out["messagesTotal"])
	}

	brief, err := api.sessionBrief("tail", "", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// "first topic" survives exactly once — as the session's title, which is metadata
	// from the file's head and true under any window. Any other mention means the scan
	// read rounds from the head.
	if n := strings.Count(brief, "first topic"); n != 1 {
		t.Errorf("the earliest round should be outside the scan (title mention only), found %d", n)
	}
	if !strings.Contains(brief, "third topic ask") || !strings.Contains(brief, "Round 2 — interrupted") {
		t.Errorf("the tail was not read as the tail:\n%s", brief)
	}

	// An at older than the tail states the bounds instead of pretending
	_, err = api.sessionBrief("tail", "", 0, "2026-09-21T07:00:00Z", "")
	if err == nil || !strings.Contains(err.Error(), "latest") {
		t.Fatalf("at older than the tail = %v", err)
	}
}

// TestRoundFailuresAndChanges: a round says how many tool calls failed, and tells the
// files it changed from the files it merely read. Before this the two were one list,
// which read as "changed twelve files" after reading eleven and editing one, and a failed
// edit counted as a change.
func TestRoundFailuresAndChanges(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "ffff.jsonl"),
		`{"type":"user","sessionId":"ffff","cwd":"/w","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"fix the build"}}`,
		`{"type":"assistant","timestamp":"2026-10-01T10:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"/x/readme.md"}}]}}`,
		`{"type":"user","timestamp":"2026-10-01T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"r1","content":"text"}]}}`,
		`{"type":"assistant","timestamp":"2026-10-01T10:00:03Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/x/a.go","old_string":"a","new_string":"b"}}]}}`,
		`{"type":"user","timestamp":"2026-10-01T10:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"e1","is_error":true,"content":"String to replace not found"}]}}`,
		`{"type":"assistant","timestamp":"2026-10-01T10:00:05Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"w1","name":"Write","input":{"file_path":"/x/b.go","content":"package x"}}]}}`,
		`{"type":"user","timestamp":"2026-10-01T10:00:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"w1","content":"ok"}]}}`,
		`{"type":"assistant","timestamp":"2026-10-01T10:00:07Z","message":{"role":"assistant","content":[{"type":"text","text":"wrote b.go, a.go did not match"}]}}`,
	)
	api := newSessionQueryAPI([]SessionSource{newClaudeSource(root)}, 0)
	sr, err := api.roundsOf("ffff", "")
	if err != nil {
		t.Fatal(err)
	}
	r := sr.rounds[0]
	if r.failures != 1 || r.toolCalls != 3 {
		t.Errorf("failures = %d, toolCalls = %d", r.failures, r.toolCalls)
	}
	if strings.Join(r.files, ",") != "/x/readme.md,/x/a.go,/x/b.go" {
		t.Errorf("files touched = %v", r.files)
	}
	if strings.Join(r.changed, ",") != "/x/b.go" {
		t.Errorf("files changed = %v (the failed Edit is not a change, the Read never was)", r.changed)
	}

	out, _ := api.sessionRounds("ffff", "")
	row := out["rounds"].([]map[string]any)[0]
	if row["failures"] != 1 || strings.Join(row["filesChanged"].([]string), ",") != "/x/b.go" {
		t.Errorf("rounds row = %v", row)
	}

	brief, err := api.sessionBrief("ffff", "", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- Changed: /x/b.go",
		"- Files: /x/readme.md, /x/a.go, /x/b.go",
		"- Tools: read 1 · write 2 · failed 1",
		// the cheapest handoff: the command that reopens the session, in its directory
		"- resume: cd /w && claude --resume ffff",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
}

// TestSplitRoundsRespectsInjectedAndEvents: a user row the CLI assembled (injected) does
// not open a round, and a recorded interruption event marks the round interrupted even
// when the next ask has not come yet
func TestSplitRoundsRespectsInjectedAndEvents(t *testing.T) {
	text := func(s string) []map[string]any { return []map[string]any{textBlock(s)} }
	rounds := splitRounds([]map[string]any{
		{"role": "user", "injected": true, "timestamp": "2026-10-01T10:00:00Z", "content": text("# AGENTS.md instructions")},
		{"role": "user", "timestamp": "2026-10-01T10:00:01Z", "content": text("real ask")},
		// Claude Code hands a finished background task to the model as a user row; a
		// row that is wholly one XML element is injected whatever its tag
		{"role": "user", "timestamp": "2026-10-01T10:00:01Z", "content": text("<task-notification>\n<task-id>x</task-id>\n</task-notification>")},
		{"role": "user", "timestamp": "2026-10-01T10:00:01Z", "content": text("<teammate-message from=\"a\">done</teammate-message>")},
		{"role": "assistant", "timestamp": "2026-10-01T10:00:02Z", "content": []map[string]any{toolCallBlock("c1", "shell", map[string]any{"command": "sleep"})}},
		{"role": "system", "timestamp": "2026-10-01T10:00:03Z", "content": []map[string]any{eventBlock(eventInterrupted, "Turn aborted: interrupted")}},
	})
	if len(rounds) != 1 {
		t.Fatalf("rounds = %d, want 1 (the injected row starts nothing)", len(rounds))
	}
	if rounds[0].asked != "real ask" || !rounds[0].interrupted {
		t.Fatalf("round = %+v", rounds[0])
	}
	// The rounds index marks a failed round distinctly from an interrupted one
	failed := round{index: 1, failures: 2, asked: "x"}
	brief := renderBrief(record{SessionID: "s"}, []round{failed, {index: 2, interrupted: true, asked: "y"}}, []int{2}, 2, 2, "")
	if !strings.Contains(brief, "✗ x · 2 failed") || !strings.Contains(brief, "⚠ y") {
		t.Errorf("rounds index marks = \n%s", brief)
	}
}

// TestRoundEndIgnoresLateRows: a notification the CLI delivers hours after the agent
// stopped lands in whichever round was last. It must not become that round's end — a
// round of two commands reporting ninety-four hours is the bug this covers — but a search
// hit inside it still has to resolve to that round, which is what lastAt keeps.
func TestRoundEndIgnoresLateRows(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "late.jsonl"),
		`{"type":"user","sessionId":"late","cwd":"/w","timestamp":"2026-09-21T08:00:00Z","message":{"role":"user","content":"ask one"}}`,
		`{"type":"assistant","timestamp":"2026-09-21T08:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","timestamp":"2026-09-21T08:01:30Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"}]}]}}`,
		`{"type":"assistant","timestamp":"2026-09-21T08:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		// four days later, with nobody at the keyboard
		`{"type":"user","timestamp":"2026-09-25T06:00:00Z","message":{"role":"user","content":"<task-notification>a background task finished</task-notification>"}}`,
		`{"type":"user","timestamp":"2026-09-25T06:00:10Z","message":{"role":"user","content":"<system-reminder>a reminder</system-reminder>"}}`,
	)
	api := newSessionQueryAPI([]SessionSource{newClaudeSource(root)}, 0)
	sr, err := api.roundsOf("late", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sr.rounds) != 1 {
		t.Fatalf("rounds = %d, want 1 (neither tagged row is an ask)", len(sr.rounds))
	}
	r := sr.rounds[0]
	if r.endAt != "2026-09-21T08:02:00Z" {
		t.Errorf("endAt = %q, want the assistant's last word 2026-09-21T08:02:00Z", r.endAt)
	}
	if r.spanEnd() != "2026-09-25T06:00:10Z" {
		t.Errorf("spanEnd = %q, want the last row 2026-09-25T06:00:10Z", r.spanEnd())
	}
	// a hit in the late row still names this round
	if _, err := api.sessionBrief("late", "", 0, "2026-09-25T06:00:05Z", ""); err != nil {
		t.Errorf("brief at a late row: %v", err)
	}
	out, err := api.sessionRounds("late", "")
	if err != nil {
		t.Fatal(err)
	}
	row := out["rounds"].([]map[string]any)[0]
	if row["endAt"] != "2026-09-21T08:02:00Z" || row["lastAt"] != "2026-09-25T06:00:10Z" {
		t.Errorf("rounds row = %v", row)
	}
}

// TestBriefSince: the delta handoff. Everything that ran after a moment, with a summary
// of the stretch before the rounds themselves.
func TestBriefSince(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "delta.jsonl"),
		`{"type":"user","sessionId":"delta","cwd":"/w","timestamp":"2026-09-21T08:00:00Z","message":{"role":"user","content":"ask one"}}`,
		`{"type":"assistant","timestamp":"2026-09-21T08:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"w1","name":"Write","input":{"file_path":"/x/one.go"}}]}}`,
		`{"type":"user","timestamp":"2026-09-21T08:01:10Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"w1","content":[{"type":"text","text":"ok"}]}]}}`,
		`{"type":"assistant","timestamp":"2026-09-21T08:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"first done"}]}}`,
		`{"type":"user","sessionId":"delta","cwd":"/w","timestamp":"2026-09-21T10:00:00Z","message":{"role":"user","content":"ask two"}}`,
		`{"type":"assistant","timestamp":"2026-09-21T10:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"w2","name":"Write","input":{"file_path":"/x/two.go"}}]}}`,
		`{"type":"user","timestamp":"2026-09-21T10:01:10Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"w2","content":[{"type":"text","text":"ok"}]}]}}`,
		`{"type":"assistant","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"second done"}]}}`,
		`{"type":"user","sessionId":"delta","cwd":"/w","timestamp":"2026-09-21T11:00:00Z","message":{"role":"user","content":"ask three"}}`,
		`{"type":"assistant","timestamp":"2026-09-21T11:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"third done"}]}}`,
	)
	api := newSessionQueryAPI([]SessionSource{newClaudeSource(root)}, 0)

	brief, err := api.sessionBrief("delta", "", 0, "", "2026-09-21T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, "## Since 2026-09-21T09:00:00Z") {
		t.Errorf("no delta header:\n%s", brief)
	}
	if !strings.Contains(brief, "2 rounds: #2 → #3") {
		t.Errorf("delta covers the wrong rounds:\n%s", brief)
	}
	// the first round's file is before the cut; the second round's is not
	if strings.Contains(brief, "/x/one.go") || !strings.Contains(brief, "/x/two.go") {
		t.Errorf("delta files are not the delta's:\n%s", brief)
	}
	// both rounds after the cut are spelled out, the one before is not
	for _, want := range []string{"## Round 2", "## Round 3", "ask two", "ask three"} {
		if !strings.Contains(brief, want) {
			t.Errorf("missing %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "## Round 1") {
		t.Errorf("round 1 is before the cut:\n%s", brief)
	}
	// a relative form is the same question asked the way a caller usually has it
	if _, err := api.sessionBrief("delta", "", 0, "", "9999d"); err != nil {
		t.Errorf("relative since: %v", err)
	}
	if _, err := api.sessionBrief("delta", "", 0, "", "1m"); err == nil ||
		!strings.Contains(err.Error(), "no round") {
		t.Errorf("a cut after everything should say so, got %v", err)
	}
	if _, err := api.sessionBrief("delta", "", 0, "", "not-a-time"); err == nil {
		t.Error("an unreadable since should be an error")
	}
	// one round chosen by number still renders alone, with no delta header
	one, err := api.sessionBrief("delta", "", 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(one, "## Since") || strings.Contains(one, "## Round 3") {
		t.Errorf("round= should pick exactly one round:\n%s", one)
	}
}
