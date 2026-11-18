package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/render"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
	"github.com/itswl/agent-session-query/internal/transcript"
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
		{[]string{"s", `"map order"`}, []string{"#1", "✗1 failed", "thinking the order of a map", "answer The test depended on map order"}},
		{[]string{"search", "rotating"}, []string{"claude/bbbb2222", "#1", "✓", "answer Rotated."}},
		{[]string{"search", "title:flaky"}, []string{"1 turn in 1 session", "#1", "title  Fix flaky test"}},
		{[]string{"search", "title:deploy"}, []string{"1 turn in 1 session", "claude/bbbb2222", "title  rotate the deploy key"}},
		{[]string{"search", "ECONNREFUSED", "数据库"}, []string{"No turn holds all 2 words.", "Partial matches, closest first: 2 turns in 1 session, the closest holding 1 of the 2.", "lacks  数据库", "lacks  ECONNREFUSED"}},
		{[]string{"search", "flaky", "nothinglikethis"}, []string{"No turn holds all 2 words; none holds “nothinglikethis”.", "lacks  nothinglikethis"}},
		{[]string{"search", "nothinglikethis", "norethis"}, []string{"No turn holds “nothinglikethis” or “norethis”."}},
		{[]string{"show", alpha}, []string{"Fix flaky test", "#1", "#2", "fix the flaky test", "/work/alpha && claude --resume", "Subagents", "Explore"}},
		{[]string{"show", alpha + "#1"}, []string{"go test ./...", "[2]", "The test depended on map order"}},
		{[]string{"show", alpha, "--turns", "last"}, []string{"schema.sql", "索引已加上"}},
		{[]string{"show", alpha, "--seq", "2"}, []string{"step 2", "ECONNREFUSED", "exit status 1"}},
		{[]string{"show", "claude/x1"}, []string{"Survey the tables", "subagent (Explore) of claude/" + fixture.Alpha}},
		{[]string{"brief", alpha}, []string{"# Session brief: Fix flaky test", "## Turn 2", "## State at the end", "schema.sql"}},
		{[]string{"brief", alpha, "--turn", "1"}, []string{"## Turn 1", "Failed: Bash `go test ./...`"}},
		{[]string{"brief", alpha, "--since", "45m"}, []string{"## Since", "## Turn 2"}},
		{[]string{"brief", alpha, "--since", "all"}, []string{"## The whole session", "## Turn 1\n", "## Turn 2\n", "  - db/schema.sql (#2)"}},
		{[]string{"brief", "--from", "2", alpha}, []string{"## From turn 2", "## Turn 2\n"}},
		{[]string{"pack", "--since", "7d"}, []string{"# Session pack", "Fix flaky test", "rotate the deploy key"}},
		{[]string{"pack", "--turns", "--project", "alpha"}, []string{"fix the flaky test", "现在给数据库加索引"}},
		{[]string{"export", alpha}, []string{"fix the flaky test", "go test ./...", "ECONNREFUSED", "索引已加上"}},
		{[]string{"export", alpha, "--format", "html"}, []string{"<!doctype html>", "fix the flaky test"}},
		{[]string{"usage"}, []string{"Rolling windows", "24h", "By model", "claude-opus-5", "By source", "By project", "/work/alpha"}},
		{[]string{"usage", "--by", "day"}, []string{"By day", "total"}},
		{[]string{"failures"}, []string{"1 failure", "Bash", "go test ./...", "exit status 1"}},
		{[]string{"failures", "econnrefused", "5432"}, []string{"1 failure", "go test ./..."}},
		{[]string{"failures", "postgres"}, []string{"No failed calls hold those words. In this window: Bash 1"}},
		{[]string{"digest"}, []string{"Total", "/work/alpha", "/work/beta", "changed"}},
		{[]string{"files", "schema.sql", "--changed"}, []string{"changed", "/work/alpha/db/schema.sql", "#2"}},
		{[]string{"projects"}, []string{"/work/alpha", "/work/beta"}},
		{[]string{"resume", alpha}, []string{"/work/alpha && claude --resume " + fixture.Alpha}},
		// the newest is alpha's, whose last record is the history's latest: not the file
		// written last, which a coarse filesystem clock (tmpfs) cannot tell apart (#55)
		{[]string{"doctor"}, []string{"asq " + app.Version + "\n", "✓ claude", "3 transcripts", "2 turns, 8 steps"}},
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
		{[]string{"show", "aaaa1111", "bbbb2222"}, 2, "which session?"},
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
		{[]string{"brief", "aaaa1111", "--from", "3"}, 1, "from must be 1..2"},
		{[]string{"brief", "aaaa1111", "--since", "whenever"}, 2, "since: unreadable time"},
		{[]string{"resume", "claude/x1"}, 1, "resume the session it ran in"},
		{[]string{"files"}, 2, "which file?"},
		{[]string{"index", "sideways"}, 2, "asq index [path | rebuild | rm]"},
		{[]string{"otel"}, 2, "where to?"},
		{[]string{"otel", "--dry-run", "--content", "everything"}, 2, "none, asks or all"},
		{[]string{"serve", "--host", "0.0.0.0"}, 2, "set a token"},
		{[]string{"serve", "--proxy-header", "X-Real-IP: 10.0.0.1"}, 2, "takes the name of a header"},
		{[]string{"serve", "--proxy-header", "X-Real-IP"}, 2, "a server without a token is not to be put behind one"},
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

// A refusal of the proxy header names it the way it was given: whoever set
// ASQ_PROXY_HEADER in a service's environment has no --proxy-header to take out.
func TestServeNamesTheProxyHeaderAsItWasGiven(t *testing.T) {
	h, index := setup(t)
	t.Setenv("ASQ_TOKEN", "")
	const behind = " is for a server behind a proxy, and a server without a token is not to be put behind one: set a token (--token, or ASQ_TOKEN), or remove "
	for _, tc := range []struct {
		env  string // ASQ_PROXY_HEADER
		args []string
		want string
	}{
		{"X-Real-IP", nil, "ASQ_PROXY_HEADER" + behind + "ASQ_PROXY_HEADER\n"},
		{"", []string{"--proxy-header", "X-Real-IP"}, "--proxy-header" + behind + "--proxy-header\n"},
		{"CF-Connecting-IP", []string{"--proxy-header", "X-Real-IP"}, "--proxy-header" + behind + "--proxy-header\n"},
		{"X-Real-IP: 10.0.0.1", nil, `ASQ_PROXY_HEADER takes the name of a header (CF-Connecting-IP, X-Real-IP, X-Forwarded-For), not "X-Real-IP: 10.0.0.1"` + "\n"},
		{"", []string{"--proxy-header", "X-Real-IP: 10.0.0.1"}, `--proxy-header takes the name of a header (CF-Connecting-IP, X-Real-IP, X-Forwarded-For), not "X-Real-IP: 10.0.0.1"` + "\n"},
	} {
		t.Setenv("ASQ_PROXY_HEADER", tc.env)
		_, errs, code := asq(t, h, index, append([]string{"serve"}, tc.args...)...)
		if code != 2 || !strings.Contains(errs, "asq serve: "+tc.want) {
			t.Errorf("ASQ_PROXY_HEADER=%q asq serve %s: exit %d, stderr %q; want exit 2 saying %q", tc.env, strings.Join(tc.args, " "), code, errs, tc.want)
		}
	}
}

// "." on the command line is the directory asq runs in. --project . keeps the sessions
// that ran in it or below it, by the path the shell gives it or the one its symbolic
// links lead to, as a CLI may record either; show and brief with no ref take the newest
// of them, and say which. Where there is nothing, the directory looked in is said.
func TestHereIsTheCurrentDirectory(t *testing.T) {
	setup(t)
	root := t.TempDir()
	real, err := filepath.EvalSymlinks(root) // what a CLI that asks the system records
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"proj/web", "proj-old", "empty"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := filepath.Join(t.TempDir(), "claude")
	now := time.Now().UTC().Truncate(time.Second)
	session := func(id, dir, ask string, ago time.Duration) {
		ts := now.Add(-ago).Format(time.RFC3339)
		cwd := filepath.Join(real, filepath.FromSlash(dir))
		path := filepath.Join(store, "-"+strings.ReplaceAll(dir, "/", "-"), id+".jsonl")
		sourcetest.Write(t, path,
			map[string]any{"type": "user", "uuid": id + "-u", "timestamp": ts, "sessionId": id, "cwd": cwd, "message": map[string]any{"role": "user", "content": ask}},
			map[string]any{"type": "assistant", "uuid": id + "-a", "timestamp": ts, "sessionId": id,
				"message": map[string]any{"id": "msg-" + id, "role": "assistant", "model": "claude-opus-5", "content": []any{map[string]any{"type": "text", "text": "done: " + ask}}}})
		if err := os.Chtimes(path, now.Add(-ago), now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	const (
		top   = "cccc0001-0000-4000-8000-000000000001"
		web   = "cccc0002-0000-4000-8000-000000000002"
		older = "cccc0003-0000-4000-8000-000000000003"
	)
	session(top, "proj", "plan the release", 3*time.Hour)
	session(web, "proj/web", "fix the web build", 2*time.Hour)
	session(older, "proj-old", "the old one, written last", time.Hour)
	h, indexPath := fixture.History{Dir: store}, filepath.Join(t.TempDir(), "index.db")
	in := func(dir string) {
		t.Helper()
		t.Chdir(filepath.Join(root, filepath.FromSlash(dir)))
	}

	in("proj")
	for _, tc := range []struct {
		args       []string
		want, not  []string
		says, from string
		tells      string // on stderr, whole
	}{
		{args: []string{"ls", "--project", "."}, want: []string{"plan the release", "fix the web build"}, not: []string{"written last"}},
		{args: []string{"ls", "--project", "./web"}, want: []string{"fix the web build"}, not: []string{"plan the release"}},
		{args: []string{"ls", "--project", ".."}, want: []string{"plan the release", "fix the web build", "written last"}},
		{args: []string{"projects", "--project", "."}, want: []string{filepath.Join(real, "proj", "web") + "\n", filepath.Join(real, "proj") + "\n"}, not: []string{"proj-old"}},
		{args: []string{"show"}, want: []string{"fix the web build"}, not: []string{"plan the release"}, says: "claude/" + web + ", the newest session in ", from: "proj"},
		{args: []string{"brief"}, want: []string{"# Session brief", "fix the web build"}, says: "claude/" + web + ", the newest session in ", from: "proj"},
		{args: []string{"show", "--json"}, want: []string{`"id": "` + web + `"`}, says: "the newest session in ", from: "proj"},
		// Every command that takes the flags that select sessions keeps to the directory
		{args: []string{"search", "the", "--project", "."}, want: []string{"plan the release", "fix the web build"}, not: []string{"written last"}},
		{args: []string{"pack", "--project", "."}, want: []string{"plan the release", "fix the web build"}, not: []string{"written last"}},
		{args: []string{"usage", "--by", "project", "--project", ".", "--json"}, want: []string{`"key": "` + filepath.Join(real, "proj", "web") + `"`}, not: []string{"proj-old"}},
		{args: []string{"digest", "--project", "."}, want: []string{filepath.Join(real, "proj", "web")}, not: []string{"proj-old"}},
		{args: []string{"otel", "--dry-run", "--content", "asks", "--project", "."}, want: []string{"plan the release", "fix the web build"}, not: []string{"written last"},
			tells: "would send 2 turns of 2 transcripts as "},
	} {
		out, errs, code := asq(t, h, indexPath, tc.args...)
		if code != 0 {
			t.Errorf("asq %s: exit %d: %s", strings.Join(tc.args, " "), code, errs)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("asq %s: no %q in:\n%s", strings.Join(tc.args, " "), want, out)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(out, not) {
				t.Errorf("asq %s: %q is not there or below it:\n%s", strings.Join(tc.args, " "), not, out)
			}
		}
		if tc.says != "" && !strings.Contains(errs, tc.says+filepath.Join(root, tc.from)) {
			t.Errorf("asq %s: stderr %q does not say %q", strings.Join(tc.args, " "), errs, tc.says+filepath.Join(root, tc.from))
		}
		if !strings.Contains(errs, tc.tells) {
			t.Errorf("asq %s: stderr %q does not say %q", strings.Join(tc.args, " "), errs, tc.tells)
		}
	}

	// A link to the directory is the directory, whichever path the CLI recorded
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "proj"), link); err != nil {
		t.Logf("no symbolic link here (%v): the rest is not tried", err)
	} else {
		t.Chdir(link)
		out, errs, code := asq(t, h, indexPath, "ls", "--project", ".")
		if code != 0 || !strings.Contains(out, "plan the release") || !strings.Contains(out, "fix the web build") {
			t.Errorf("through a link to the directory: exit %d, %s%s", code, out, errs)
		}
	}

	in("empty")
	out, errs, code := asq(t, h, indexPath, "ls", "--project", ".")
	if code != 0 || !strings.Contains(out, "No sessions.") || !strings.Contains(errs, "looked in "+filepath.Join(root, "empty")+" and the directories below it") {
		t.Errorf("ls --project . where nothing ran: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	out, errs, code = asq(t, h, indexPath, "failures", "--project", ".")
	if code != 0 || !strings.Contains(errs, "looked in "+filepath.Join(root, "empty")) {
		t.Errorf("failures --project . where nothing ran: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	out, errs, code = asq(t, h, indexPath, "otel", "--dry-run", "--project", ".")
	if code != 0 || out != "" || !strings.Contains(errs, "would send 0 turns") || !strings.Contains(errs, "looked in "+filepath.Join(root, "empty")) {
		t.Errorf("otel --project . where nothing ran: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	for _, args := range [][]string{{"show"}, {"show", "-f"}, {"brief"}} {
		out, errs, code := asq(t, h, indexPath, args...)
		if code != 1 || out != "" || !strings.Contains(errs, "no session ran in "+filepath.Join(root, "empty")+" or below it") {
			t.Errorf("asq %s where nothing ran: exit %d, stdout %q, stderr %q", strings.Join(args, " "), code, out, errs)
		}
	}
	// A part of a path is still a part of a path
	if out, errs, code := asq(t, h, indexPath, "ls", "--project", "proj"); code != 0 || !strings.Contains(out, "written last") || errs != "" {
		t.Errorf("ls --project proj: exit %d, %s%s", code, out, errs)
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

// told is a store of sessions, newest first: each a word said, an event alone, or a
// session moved to cold storage whole ("cold") or in part ("warm").
type told []string

func (told) Location() string { return "test" }
func (told) Exists() bool     { return true }
func (t told) Scan(context.Context) ([]sources.Ref, error) {
	refs := make([]sources.Ref, len(t))
	for i := range t {
		refs[i] = sources.Ref{Key: fmt.Sprint(i), Stamp: "1", ModTime: time.Unix(int64(1790000000-i), 0)}
	}
	return refs, nil
}
func (t told) Read(_ context.Context, ref sources.Ref, w *transcript.Writer) error {
	i, _ := strconv.Atoi(ref.Key)
	w.S.ID = "s" + ref.Key
	at := time.Unix(1790000000, 0)
	event := func(kind string) {
		w.Add(transcript.Message{Role: transcript.System, At: at, Blocks: []transcript.Block{transcript.EventOf(kind, "said of the session")}})
	}
	ask := func() {
		w.Add(transcript.Message{Role: transcript.User, At: at, Blocks: []transcript.Block{{Type: transcript.Text, Text: "hello"}}})
	}
	switch t[i] {
	case "said":
		ask()
	case "event":
		event(model.EventCompaction)
	case "cold":
		event(model.EventColdStorage)
	case "warm":
		event(model.EventColdStorage)
		ask()
	case "drifted":
		// moved in part, and what is left is not understood
		event(model.EventColdStorage)
		w.Unread("row:message_v3")
	}
	return nil
}

// A session that holds nothing but events is not a conversation doctor can judge a store
// by. One moved whole to where it is not read says so, and that is why it is empty: a store
// of those is not one whose format drifted, though a store of other empty sessions may be.
func TestDoctorIsNotToldByEventsAlone(t *testing.T) {
	ctx := context.Background()
	look := func(t told) (look, error) { return deepRead(ctx, index.Store{Name: "openclaw", Source: t}) }
	if lk, err := look(told{"event", "said"}); err != nil || lk.newest != "s1" {
		t.Errorf("events alone before a conversation: newest %q, %v; want s1", lk.newest, err)
	}
	if lk, err := look(told{"event", "event", "event"}); err == nil {
		t.Errorf("sessions of events alone passed as conversations: newest %q", lk.newest)
	}
	if lk, err := look(told{"cold", "cold", "cold"}); err != nil || lk.newest != "" || lk.archived != 3 {
		t.Errorf("a store whose newest are in cold storage: newest %q, archived %d, %v; want none, 3, no error", lk.newest, lk.archived, err)
	}
	if _, err := look(told{"cold", "event", "cold"}); err == nil {
		t.Error("sessions in cold storage hid others that read as empty")
	}
	if lk, err := look(told{"cold", "warm", "said"}); err != nil || lk.newest != "s1" || lk.archived != 2 {
		t.Errorf("a conversation moved in part: newest %q, archived %d, %v; want s1, 2", lk.newest, lk.archived, err)
	}
	if _, err := look(told{"drifted", "drifted", "cold"}); err == nil {
		t.Error("sessions moved in part, the rest not understood, passed as moved whole")
	}

	// Stopped before it read anything: not a pass
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := deepRead(stopped, index.Store{Name: "openclaw", Source: told{"said"}}); err == nil {
		t.Error("a look stopped before it read a session passed")
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

// asq index rm takes what earlier versions of asq left beside the index, and only where
// they left it: an index at a path of its own is the only one there is.
func TestIndexRmTakesWhatEarlierVersionsLeft(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "local"))
	t.Setenv("ASQ_INDEX", "")
	dir := filepath.Dir(index.DefaultPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	put := func(names ...string) {
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	gone := func(names ...string) {
		t.Helper()
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Errorf("%s is still there", name)
			}
		}
	}
	current := filepath.Base(index.DefaultPath())
	put("index-v1.db", "index-v1.db-wal", current)
	var out, errs bytes.Buffer
	if code := Run([]string{"index", "rm"}, strings.NewReader(""), &out, &errs); code != 0 {
		t.Fatalf("asq index rm: exit %d\n%s", code, &errs)
	}
	gone("index-v1.db", "index-v1.db-wal", current)
	if !strings.Contains(out.String(), "index-v1.db — an earlier asq's") {
		t.Errorf("asq index rm said:\n%s", &out)
	}

	put("index-v1.db")
	out.Reset()
	if code := Run([]string{"index", "--index", filepath.Join(home, "own.db"), "rm"}, strings.NewReader(""), &out, &errs); code != 0 {
		t.Fatalf("asq index --index … rm: exit %d\n%s", code, &errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "index-v1.db")); err != nil {
		t.Errorf("an index at a path of its own took the earlier asq's with it: %v", err)
	}
}

// A doctor stopped part of the way does not pass, and says it was stopped, in words and in
// JSON: a failure with nothing said of it reads as a store that failed unseen.
func TestAStoppedDoctorSaysSo(t *testing.T) {
	h, indexPath := setup(t)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	for _, json := range []bool{false, true} {
		args := []string{"--index", indexPath, "--only", "claude", "--path", "claude=" + h.Dir}
		if json {
			args = append(args, "--json")
		}
		var out, errs bytes.Buffer
		err := cmdDoctor(stopped, args, &streams{in: strings.NewReader(""), out: &out, err: &errs})
		said := out.String()
		if err == nil || !(json && strings.Contains(said, `"stopped": true`) || !json && strings.Contains(said, "stopped before every store")) {
			t.Errorf("a stopped doctor (json %v): %v\n%s", json, err, said)
		}
	}
}
