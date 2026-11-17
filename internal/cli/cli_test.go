package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/render"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// asq runs one command against the fixture and returns what it printed and how it ended.
func asq(t *testing.T, h fixture.History, indexPath string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	// Flags may stand anywhere among the arguments; here they follow the command
	full := append([]string{args[0]}, "--index", indexPath, "--only", "claude", "--path", "claude="+h.Dir)
	full = append(full, args[1:]...)
	var out, errs bytes.Buffer
	code = Run(full, strings.NewReader(""), &out, &errs)
	return out.String(), errs.String(), code
}

func setup(t *testing.T) (fixture.History, string) {
	t.Helper()
	t.Setenv("ASQ_ONLY", "")
	t.Setenv("ASQ_PATH", "")
	t.Setenv("ASQ_INDEX", "")
	t.Setenv("ASQ_PROXY_HEADER", "")
	t.Setenv("ASQ_STATE", t.TempDir()) // nothing a test does is kept beside the real thing
	h := fixture.Write(t)
	return h, h.Config(t).IndexPath
}

func TestCommands(t *testing.T) {
	h, index := setup(t)
	alpha := "claude/aaaa1111"
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"ls"}, []string{alpha, "claude/bbbb2222", "Fix flaky test", "2 turns", "1 failed", "1 agent", "/work/alpha", "main"}},
		{[]string{"ls", "--project", "beta"}, []string{"claude/bbbb2222", "rotate the deploy key"}},
		{[]string{"ls", "flaky"}, []string{alpha}},
		{[]string{"ls", "--agents"}, []string{"claude/x1"}},
		{[]string{"search", "数据库"}, []string{"1 turn in 1 session", alpha, "#2", "现在给数据库加索引"}},
		{[]string{"search", "--since", "7d", "ECONNREFUSED"}, []string{"output", "Bash", "ECONNREFUSED"}},
		{[]string{"s", `"map order"`}, []string{"#1", "reply"}},
		{[]string{"show", alpha}, []string{"Fix flaky test", "#1", "#2", "fix the flaky test", "/work/alpha && claude --resume", "Subagents", "Explore"}},
		{[]string{"show", alpha + "#1"}, []string{"go test ./...", "[2]", "The test depended on map order"}},
		{[]string{"show", alpha, "--turns", "last"}, []string{"schema.sql", "索引已加上"}},
		{[]string{"show", alpha, "--seq", "2"}, []string{"step 2", "ECONNREFUSED", "exit status 1"}},
		{[]string{"show", "claude/x1"}, []string{"Survey the tables", "subagent (Explore) of claude/" + fixture.Alpha}},
		{[]string{"brief", alpha}, []string{"# Session brief: Fix flaky test", "## Turn 2", "## State at the end", "schema.sql"}},
		{[]string{"brief", alpha, "--turn", "1"}, []string{"## Turn 1", "Failed: Bash `go test ./...`"}},
		{[]string{"brief", alpha, "--since", "45m"}, []string{"## Since", "## Turn 2"}},
		{[]string{"pack", "--since", "7d"}, []string{"# Session pack", "Fix flaky test", "rotate the deploy key"}},
		{[]string{"pack", "--turns", "--project", "alpha"}, []string{"fix the flaky test", "现在给数据库加索引"}},
		{[]string{"export", alpha}, []string{"fix the flaky test", "go test ./...", "ECONNREFUSED", "索引已加上"}},
		{[]string{"export", alpha, "--format", "html"}, []string{"<!doctype html>", "fix the flaky test"}},
		{[]string{"usage"}, []string{"Rolling windows", "24h", "By model", "claude-opus-5", "By source", "By project", "/work/alpha"}},
		{[]string{"usage", "--by", "day"}, []string{"By day", "total"}},
		{[]string{"failures"}, []string{"1 failure", "Bash", "go test ./...", "exit status 1"}},
		{[]string{"digest"}, []string{"Total", "/work/alpha", "/work/beta", "changed"}},
		{[]string{"files", "schema.sql", "--changed"}, []string{"changed", "/work/alpha/db/schema.sql", "#2"}},
		{[]string{"projects"}, []string{"/work/alpha", "/work/beta"}},
		{[]string{"resume", alpha}, []string{"/work/alpha && claude --resume " + fixture.Alpha}},
		{[]string{"doctor"}, []string{"asq " + app.Version + "\n", "✓ claude", "3 transcripts", "1 turn, 3 steps"}},
		{[]string{"index"}, []string{"2 sessions", "1 subagent transcript", "3 turns", "✓ claude"}},
		{[]string{"index", "path"}, []string{index}},
		{[]string{"otel", "--dry-run", "--content", "asks"}, []string{`"resourceSpans"`, "fix the flaky test", "invoke_agent"}},
	} {
		out, errs, code := asq(t, h, index, tc.args...)
		if code != 0 {
			t.Errorf("asq %s: exit %d: %s", strings.Join(tc.args, " "), code, errs)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("asq %s: no %q in:\n%s", strings.Join(tc.args, " "), want, out)
			}
		}
		if strings.Contains(out, "\x1b[") {
			t.Errorf("asq %s: colour codes in output that is not a terminal", strings.Join(tc.args, " "))
		}
	}
}

func TestJSON(t *testing.T) {
	h, index := setup(t)
	for _, tc := range []struct {
		args []string
		keys []string
	}{
		{[]string{"ls"}, []string{"sessions", "total"}},
		{[]string{"search", "flaky"}, []string{"hits", "total", "sessions"}},
		{[]string{"show", "aaaa1111"}, []string{"session", "turns", "agents"}},
		{[]string{"show", "aaaa1111#1"}, []string{"session", "turns"}},
		{[]string{"show", "aaaa1111", "--seq", "2"}, []string{"session", "turn", "item"}},
		{[]string{"brief", "aaaa1111"}, []string{"session", "brief"}},
		{[]string{"pack"}, []string{"pack", "sessions"}},
		{[]string{"export", "aaaa1111"}, []string{"session", "turns"}},
		{[]string{"usage"}, []string{"total", "windows", "byModel", "byDay", "bySource", "byProject"}},
		{[]string{"usage", "--by", "model"}, []string{"by", "rows", "total"}},
		{[]string{"failures"}, []string{"failures", "total", "byTool"}},
		{[]string{"digest"}, []string{"projects", "total"}},
		{[]string{"files", "schema"}, []string{"files", "total"}},
		{[]string{"projects"}, []string{"projects"}},
		{[]string{"doctor"}, []string{"version", "stores", "ok"}},
		{[]string{"index"}, []string{"version", "index", "totals", "sync", "stores", "warnings"}},
	} {
		out, errs, code := asq(t, h, index, append(tc.args, "--json")...)
		var got map[string]json.RawMessage
		if code != 0 || json.Unmarshal([]byte(out), &got) != nil {
			t.Errorf("asq %s --json: exit %d, stdout is not one JSON object: %s%s", strings.Join(tc.args, " "), code, errs, out[:min(200, len(out))])
			continue
		}
		for _, key := range tc.keys {
			if _, ok := got[key]; !ok {
				t.Errorf("asq %s --json: no %q", strings.Join(tc.args, " "), key)
			}
		}
	}
}

func TestWhatGoesWrong(t *testing.T) {
	h, index := setup(t)
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"show", "zzzz"}, 1, "asq ls prints refs"},
		{[]string{"show"}, 2, "which session?"},
		{[]string{"show", "aaaa1111#9"}, 1, "no such turn (it has 2)"},
		{[]string{"show", "aaaa1111", "--seq", "99"}, 1, "no step 99"},
		{[]string{"show", "aaaa1111", "--turns", "seven"}, 2, "turns: want"},
		{[]string{"search"}, 2, "what should it look for?"},
		{[]string{"ls", "--since", "whenever"}, 2, "unreadable time"},
		{[]string{"ls", "--source", "emacs"}, 2, "unknown source"},
		{[]string{"ls", "--nope"}, 2, "flag provided but not defined"},
		{[]string{"usage", "--by", "colour"}, 2, "grouped by model"},
		{[]string{"export", "aaaa1111", "--format", "docx"}, 2, "docx"},
		{[]string{"brief", "aaaa1111", "--turn", "9"}, 1, "turn must be 1..2"},
		{[]string{"resume", "claude/x1"}, 1, "resume the session it ran in"},
		{[]string{"files"}, 2, "which file?"},
		{[]string{"index", "sideways"}, 2, "asq index [path | rebuild | rm]"},
		{[]string{"otel"}, 2, "where to?"},
		{[]string{"otel", "--dry-run", "--content", "everything"}, 2, "none, asks or all"},
		{[]string{"serve", "--host", "0.0.0.0"}, 2, "set a token"},
		{[]string{"serve", "--proxy-header", "X-Real-IP: 10.0.0.1"}, 2, "takes the name of a header"},
	} {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		t.Setenv("LANGFUSE_PUBLIC_KEY", "")
		t.Setenv("ASQ_TOKEN", "")
		out, errs, code := asq(t, h, index, tc.args...)
		if code != tc.code || !strings.Contains(errs, tc.want) {
			t.Errorf("asq %s: exit %d, stderr %q; want exit %d saying %q", strings.Join(tc.args, " "), code, errs, tc.code, tc.want)
		}
		if out != "" {
			t.Errorf("asq %s: a failed command printed to stdout: %q", strings.Join(tc.args, " "), out)
		}
	}

	var out, errs bytes.Buffer
	if code := Run([]string{"frobnicate"}, nil, &out, &errs); code != 2 || !strings.Contains(errs.String(), "unknown command") {
		t.Errorf("an unknown command: exit %d %q", code, errs.String())
	}
	out.Reset()
	if code := Run(nil, nil, &out, &errs); code != 0 || !strings.Contains(out.String(), "asq search <words>") {
		t.Errorf("no command prints the usage: exit %d", code)
	}
	out.Reset()
	if code := Run([]string{"ls", "-h"}, nil, &out, &errs); code != 0 {
		t.Errorf("-h is not a failure: exit %d", code)
	}
}

func TestIndexRebuildAndRemove(t *testing.T) {
	h, index := setup(t)
	if out, errs, code := asq(t, h, index, "index", "rebuild"); code != 0 || !strings.Contains(out, "3 turns") {
		t.Fatalf("rebuild: exit %d %s%s", code, errs, out)
	}
	if out, _, code := asq(t, h, index, "index", "rm"); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("rm: exit %d %s", code, out)
	}
	if out, _, code := asq(t, h, index, "ls"); code != 0 || !strings.Contains(out, "claude/aaaa1111") {
		t.Errorf("after the index is removed the next command rebuilds it: exit %d\n%s", code, out)
	}
}

func TestCollector(t *testing.T) {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY", "LANGFUSE_HOST", "LANGFUSE_BASE_URL"} {
		t.Setenv(name, "")
	}
	if got, _, err := collector("", nil); err != nil || got != "" {
		t.Errorf("nothing configured = %q, %v", got, err)
	}
	got, headers, err := collector("https://c.example:4318", []string{"X-Team=infra", "Authorization: Bearer abc=="})
	if err != nil || got != "https://c.example:4318" || headers["X-Team"] != "infra" || headers["Authorization"] != "Bearer abc==" {
		t.Errorf("flags = %q %v %v", got, headers, err)
	}
	if _, _, err := collector("collector:4318", nil); err == nil {
		t.Error("an address with no scheme is accepted")
	}
	if _, _, err := collector("https://c.example", []string{"novalue"}); err == nil {
		t.Error("a header with no value is accepted")
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-key=a%20b, x-other=c")
	got, headers, _ = collector("", nil)
	if got != "http://localhost:4318" || headers["x-key"] != "a b" || headers["x-other"] != "c" {
		t.Errorf("OpenTelemetry's own variables = %q %v", got, headers)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-lf-1")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-lf-2")
	got, headers, _ = collector("", nil)
	if got != "https://cloud.langfuse.com/api/public/otel" || headers["Authorization"] != "Basic cGstbGYtMTpzay1sZi0y" {
		t.Errorf("Langfuse's variables = %q %v", got, headers)
	}
	t.Setenv("LANGFUSE_HOST", "https://lf.internal/")
	if got, _, _ = collector("", nil); got != "https://lf.internal/api/public/otel" {
		t.Errorf("a self-hosted Langfuse = %q", got)
	}
}

// syncBuffer is a buffer two goroutines can share: the follower writes, the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestFollowPrintsWhatASessionGains(t *testing.T) {
	a, h := fixture.Open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, _, err := a.Resolve(ctx, fixture.Beta)
	if err != nil {
		t.Fatal(err)
	}
	last, err := a.Index.Turns(ctx, &s, index.TurnRange{First: 1, Last: -1, Tail: 1})
	if err != nil || len(last) != 1 {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- followSession(ctx, a, &s, last[0], false, render.Style{}, &streams{out: out, err: io.Discard})
	}()

	path := filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl")
	add := func(rows ...string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for _, row := range rows {
			f.WriteString(row + "\n")
		}
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !strings.Contains(out.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("following never printed %q; it printed:\n%s", want, out.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	now := h.Now.Format(time.RFC3339)
	row := func(kind, uuid, message string) string {
		return `{"type":"` + kind + `","uuid":"` + uuid + `","timestamp":"` + now + `","sessionId":"` + fixture.Beta + `","cwd":"/work/beta","message":` + message + `}`
	}

	// A new ask opens a turn; the call it makes is printed before its result is in
	add(row("user", "f-u1", `{"role":"user","content":"and tell the team"}`),
		row("assistant", "f-a1", `{"id":"msg_f1","role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"toolu_f1","name":"Bash","input":{"command":"notify --team"}}]}`))
	waitFor("and tell the team")
	waitFor("notify --team")
	if !strings.Contains(out.String(), "#2") || !strings.Contains(out.String(), "(no result)") {
		t.Errorf("a new turn opens with its line, and a call still running says so:\n%s", out.String())
	}
	if strings.Contains(out.String(), "rotate the deploy key") {
		t.Error("following printed again what was already on screen")
	}

	// The result arrives: the call is printed again, with how it ended
	add(row("user", "f-u2", `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_f1","content":"notify: no such channel","is_error":true}]}`),
		row("assistant", "f-a2", `{"id":"msg_f2","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"The channel does not exist."}]}`))
	waitFor("✗ failed")
	waitFor("The channel does not exist.")
	if strings.Count(out.String(), "notify --team") != 2 || !strings.Contains(out.String(), "no such channel") {
		t.Errorf("the call is shown once when made and once when answered:\n%s", out.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("following ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("following did not stop when interrupted")
	}
}

// A store that could not be read is said with the answer, whichever command asked: the
// sessions already known of it are still listed, and the reader is told the list may be
// short.
func TestAnUnreadableStoreIsSaidWithEveryAnswer(t *testing.T) {
	h, indexPath := setup(t)
	if _, errs, code := asq(t, h, indexPath, "ls"); code != 0 || errs != "" {
		t.Fatalf("a store that reads fine: exit %d, stderr %q", code, errs)
	}
	sourcetest.Unreadable(t, filepath.Join(h.Dir, "-work-beta"))
	for _, args := range [][]string{{"ls"}, {"search", "rotate"}, {"usage"}, {"show", "bbbb2222"}} {
		out, errs, code := asq(t, h, indexPath, args...)
		if code != 0 || !strings.Contains(errs, "claude could not be read") {
			t.Errorf("asq %s: exit %d, stderr %q", strings.Join(args, " "), code, errs)
		}
		if strings.Contains(errs, "\x1b") || strings.Contains(errs, "\r") {
			t.Errorf("asq %s: control sequences on a stderr that is not a terminal: %q", strings.Join(args, " "), errs)
		}
		if args[0] != "usage" && !strings.Contains(out, "bbbb2222") && !strings.Contains(out, "rotate the deploy key") {
			t.Errorf("asq %s: the session in the unreadable directory is gone from the answer:\n%s", strings.Join(args, " "), out)
		}
	}
}

// A store can open with any number of sessions in which nothing was said. They prove
// nothing either way, and doctor reads on to one that does.
func TestDoctorLooksPastEmptySessions(t *testing.T) {
	h, indexPath := setup(t)
	for i := range 60 {
		path := filepath.Join(h.Dir, "-work-cron", fmt.Sprintf("0000%04d-0000-4000-8000-000000000000.jsonl", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		// opened and closed: bookkeeping, and not a word
		if err := os.WriteFile(path, []byte(`{"type":"queue-operation","operation":"enqueue"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Duration(i+1) * time.Minute)
		os.Chtimes(path, future, future)
	}
	out, errs, code := asq(t, h, indexPath, "doctor")
	if code != 0 || !strings.Contains(out, "✓ claude") || !strings.Contains(out, "63 transcripts") {
		t.Errorf("doctor: exit %d\n%s%s", code, out, errs)
	}
}

// Which asq this is is said wherever someone would look for it: the usage, --version, and
// doctor, whose report depends on it.
func TestTheVersionIsSaid(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"--version"}, {"version"}} {
		var out, errs bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, &errs); code != 0 || !strings.HasPrefix(out.String(), "asq "+app.Version) {
			t.Errorf("asq %q: exit %d, first line %q", args, code, strings.SplitN(out.String(), "\n", 2)[0])
		}
	}
}
