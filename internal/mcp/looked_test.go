package mcp

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/catalog"
)

// An agent that finds no sessions is told where asq looked, when nothing was found anywhere;
// one whose history holds sessions is not.
func TestNothingFoundSaysWhereItLooked(t *testing.T) {
	if text, _ := call(t, server(t), "search", map[string]any{"query": "nothinglikethis"}); strings.Contains(text, "looked in") {
		t.Errorf("a history with sessions says where it looked:\n%s", text)
	}

	nowhere := filepath.Join(t.TempDir(), "no", "such", "store")
	a, err := app.Open(app.Config{IndexPath: filepath.Join(t.TempDir(), "index.db"),
		Stores: catalog.Config{Only: []string{"claude"}, Paths: []catalog.Path{{Agent: "claude", Dir: nowhere}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s := New(a, io.Discard)
	looked := "No CLI's sessions were found on the machine asq runs on. It looked in: claude " + nowhere + " (not there)."
	text, failed := call(t, s, "sessions", map[string]any{})
	if failed || !strings.Contains(text, looked) || !strings.Contains(text, "asq doctor there says more") {
		t.Errorf("sessions with no store:\n%s", text)
	}

	// health says where it looked in its own body, with what to do about it, and is not told
	// so again beneath — nor that the index is catching up, which it says too
	text, failed = call(t, s, "health", map[string]any{})
	lookedLine := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "  claude ") && strings.Contains(line, "not there") && strings.HasSuffix(line, nowhere) {
			lookedLine = true
		}
	}
	if failed || !strings.Contains(text, "\nNo CLI's sessions were found on the machine asq runs on. It looked in:\n") || !lookedLine ||
		!strings.Contains(text, "named with ASQ_PATH") || !strings.Contains(text, "asq doctor there says more") || strings.Contains(text, "\n[") {
		t.Errorf("health with no store:\n%s", text)
	}

	// A person who asks for a handoff or a catch-up is told the same, with why there was none
	s.Caller = &Caller{Dirs: []string{"/work/alpha"}}
	for _, name := range []string{"handoff", "catchup"} {
		if _, rpcErr := getPrompt(t, s, name, nil); rpcErr == nil || !strings.Contains(rpcErr.Message, "no session ran in /work/alpha") || !strings.Contains(rpcErr.Message, looked) {
			t.Errorf("%s with no store = %+v", name, rpcErr)
		}
	}
}
