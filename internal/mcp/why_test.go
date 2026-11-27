package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/fixture"
)

// withCommit is the MCP server over the fixture's history and, beside it, a git repository
// that a session's call made a commit in: for why, which asks git of a file.
func withCommit(t *testing.T) (*Server, fixture.Commit) {
	t.Helper()
	a, c := fixture.OpenCommit(t)
	return New(a, io.Discard), c
}

// why says which session wrote one line: the commit git blame names, then the turns that
// made it, each with the step that ran git commit — which read opens by seq. A server
// started on stdio is asked as the command line is, a relative path included. Over HTTP
// the path is held to a rule — whole, and under a directory a session ran in — since the
// server would run git in the directory named to it. What git refuses is the caller's to
// set right, and comes back as the tool's answer.
func TestWhy(t *testing.T) {
	s, c := withCommit(t)
	ref := "claude/" + fixture.Committed + "#1"
	const made = "Made in 1 turn:\n"
	s.Caller = &Caller{Dirs: []string{c.Dir}} // started on stdio, in the repository
	t.Chdir(c.Dir)
	for _, path := range []string{c.File, "notes.md"} {
		text, isErr := call(t, s, "why", map[string]any{"path": path, "line": 1})
		if isErr {
			t.Errorf("on stdio, path %s: %s", path, text)
			continue
		}
		// The head names the file as it was given, as asq why prints it
		for _, want := range []string{
			path + ":1\n    " + c.Text + "\n", c.Short, " · trim the notes\n\n" + made,
			"\n1. " + ref + " ", "   ask    trim the notes\n",
			"\n   output Bash › [main (root-commit) " + c.Short + "] trim the notes", "notes.md  [1]\n", "   answer Committed.",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("on stdio, path %s: no %q in:\n%s", path, want, text)
			}
		}
		if strings.Contains(text, "Named in") {
			t.Errorf("on stdio, path %s: a turn is said to name the commit without having made it:\n%s", path, text)
		}
	}
	loose := filepath.Join(t.TempDir(), "loose.md")
	if err := os.WriteFile(loose, []byte("in no repository\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		line any
		want string
	}{
		{c.File, 99, "git blame: file notes.md has only 1 line"},
		{c.File, 0, "line 0: lines are counted from 1"},
		{c.File, "x", `line: want a whole number, got x`},
		{filepath.Join(c.Dir, "loose.md"), 1, "git blame: no such path 'loose.md' in HEAD"},
		{loose, 1, "git blame: not a git repository"},
	} {
		text, isErr := call(t, s, "why", map[string]any{"path": tc.path, "line": tc.line})
		if !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("on stdio, %s:%v: isError=%v %q, want an error saying %q", tc.path, tc.line, isErr, text, tc.want)
		}
	}

	// Over HTTP: a whole path under a directory a session ran in answers as on stdio. A
	// relative one names no directory the server can know; one under no session's directory
	// would have the server run git somewhere the history never was
	s.Caller = nil
	if text, isErr := call(t, s, "why", map[string]any{"path": c.File, "line": 1}); isErr || !strings.Contains(text, made) {
		t.Errorf("over HTTP, %s: isError=%v\n%s", c.File, isErr, text)
	}
	for _, tc := range []struct{ path, want string }{
		{"notes.md", `path "notes.md" is relative, and a request over HTTP does not say which directory you mean: give the whole path`},
		{loose, `is under no directory a session asq reads ran in, and over HTTP why answers only of those: projects lists them`},
	} {
		text, isErr := call(t, s, "why", map[string]any{"path": tc.path, "line": 1})
		if !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("over HTTP, %s: isError=%v %q, want an error saying %q", tc.path, isErr, text, tc.want)
		}
	}
	// and by the endpoint itself, not only by a server without a Caller
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"why","arguments":{"path":"notes.md","line":1}}}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
	var r reply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || rec.Code != http.StatusOK || !r.Result.IsError || len(r.Result.Content) != 1 ||
		!strings.Contains(r.Result.Content[0].Text, "is relative") {
		t.Errorf("POST /mcp why with a relative path = %d %s, want an answer that is an error saying the path is relative", rec.Code, rec.Body)
	}
}
