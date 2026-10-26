package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/source"
)

// writeActivitySession lays down one Claude session: an ask, a tool call that fails, and
// an answer — with usage on the assistant rows, the way the CLI writes it.
func writeActivitySession(t *testing.T, root, id, project string) {
	t.Helper()
	at := func(minutesAgo int) string {
		return time.Now().UTC().Add(-time.Duration(minutesAgo) * time.Minute).Format("2006-01-02T15:04:05Z")
	}
	write(t, filepath.Join(root, project, id+".jsonl"),
		`{"type":"user","sessionId":"`+id+`","cwd":"/w/`+project+`","timestamp":"`+at(4)+`","message":{"role":"user","content":"run the tests"}}`,
		`{"type":"assistant","timestamp":"`+at(3)+`","message":{"role":"assistant","usage":{"input_tokens":100,"output_tokens":10},"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}`,
		`{"type":"user","timestamp":"`+at(2)+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"build failed\nline two"}]}}`,
		`{"type":"assistant","timestamp":"`+at(1)+`","message":{"role":"assistant","usage":{"input_tokens":50,"output_tokens":5},"content":[{"type":"text","text":"the build is broken"}]}}`,
	)
}

// TestRoundUsage: a round reports what its own messages cost, in the rounds JSON, the
// brief and the fold line's material
func TestRoundUsage(t *testing.T) {
	root := t.TempDir()
	writeActivitySession(t, root, "usage-1", "proj")
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)

	out, err := api.sessionRounds("usage-1", "")
	if err != nil {
		t.Fatal(err)
	}
	row := out["rounds"].([]map[string]any)[0]
	usage, ok := row["usage"].(map[string]any)
	if !ok {
		t.Fatalf("rounds row has no usage: %v", row)
	}
	if usage["inputTokens"] != int64(150) || usage["outputTokens"] != int64(15) {
		t.Fatalf("usage = %v, want 150 in / 15 out", usage)
	}

	brief, err := api.sessionBrief("usage-1", "", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, "- Tokens: 150 in · 15 out") {
		t.Fatalf("brief lacks the token line:\n%s", brief)
	}
}

// TestFailuresReport: failed tool results come back with the tool, the command, a preview
// of the output and the round they belong to
func TestFailuresReport(t *testing.T) {
	root := t.TempDir()
	writeActivitySession(t, root, "fail-1", "proj")
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)

	report := api.failuresReport(time.Now().Add(-24*time.Hour), time.Time{}, "", "", "", 100)
	failures := report["failures"].([]map[string]any)
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want one", failures)
	}
	one := failures[0]
	if one["tool"] != "Bash" || one["command"] != "go test ./..." ||
		!strings.Contains(source.ToStr(one["output"]), "build failed") {
		t.Fatalf("failure = %v", one)
	}
	if one["sessionId"] != "fail-1" || one["round"] != 1 {
		t.Fatalf("failure identity = %v", one)
	}

	// A tool filter that does not match finds nothing
	if got := api.failuresReport(time.Now().Add(-24*time.Hour), time.Time{}, "", "", "Read", 100)["failures"].([]map[string]any); len(got) != 0 {
		t.Fatalf("tool filter = %v, want none", got)
	}
	// A window that ends before the session has nothing either
	if got := api.failuresReport(time.Time{}, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), "", "", "", 100)["failures"].([]map[string]any); len(got) != 0 {
		t.Fatalf("window filter = %v, want none", got)
	}
}

// TestDigestReport: a project's stretch adds up sessions, rounds, failures, tokens, the
// kinds of files changed, and the models and CLIs that did the work
func TestDigestReport(t *testing.T) {
	root := t.TempDir()
	writeActivitySession(t, root, "dig-1", "proj")
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)

	report := api.digestReport(time.Now().Add(-24*time.Hour), time.Time{}, "", "")
	projects := report["projects"].([]map[string]any)
	if len(projects) != 1 {
		t.Fatalf("projects = %v", projects)
	}
	one := projects[0]
	if one["sessions"] != 1 || one["rounds"] != 1 || one["failures"] != 1 {
		t.Fatalf("project row = %v", one)
	}
	tokens := one["tokens"].(map[string]any)
	if tokens["inputTokens"] != int64(150) || tokens["outputTokens"] != int64(15) {
		t.Fatalf("tokens = %v", tokens)
	}
	if sources := one["sources"].([]string); len(sources) != 1 || sources[0] != "claude" {
		t.Fatalf("sources = %v", sources)
	}
	if !strings.Contains(source.ToStr(one["project"]), "proj") {
		t.Fatalf("project = %v", one["project"])
	}
	totals := report["totals"].(map[string]any)
	if totals["sessions"] != 1 || totals["failures"] != 1 {
		t.Fatalf("totals = %v", totals)
	}
}
