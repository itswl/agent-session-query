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

	// A person who asks for a handoff or a catch-up is told the same, with why there was none
	s.Caller = &Caller{Dirs: []string{"/work/alpha"}}
	for _, name := range []string{"handoff", "catchup"} {
		if _, rpcErr := getPrompt(t, s, name, nil); rpcErr == nil || !strings.Contains(rpcErr.Message, "no session ran in /work/alpha") || !strings.Contains(rpcErr.Message, looked) {
			t.Errorf("%s with no store = %+v", name, rpcErr)
		}
	}
}
