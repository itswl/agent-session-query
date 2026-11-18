package copilot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// FuzzRead reads whatever a session's log might hold, laid out where Scan finds it. An
// error is an answer; a panic, or a session that breaks what every session must hold, is
// not.
func FuzzRead(f *testing.F) {
	// The test store's logs, each of them whole
	logs, err := filepath.Glob(filepath.Join("testdata", "store", "*", "events*.jsonl"))
	if err != nil || len(logs) == 0 {
		f.Fatalf("no seeds: %v", err)
	}
	for _, path := range logs {
		seed, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(seed)
	}
	// A rotated log whose link points back at itself, and calls stopped and settled twice
	for _, lines := range [][]any{
		{
			ev("e1", "session.journal_link", "2026-11-17T09:00:00.000Z", M{"version": 1, "sessionId": "s", "previousFile": "events.jsonl"}),
			M{"id": "e2", "timestamp": "2026-11-17T09:00:01.000Z", "type": "user.message", "checkpoint": true, "data": M{"content": "copied"}},
		},
		{
			ev("e1", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "go"}),
			ev("e2", "assistant.message", "2026-11-17T09:00:02.000Z", M{"messageId": "m", "content": "", "apiCallId": "x",
				"toolRequests": []M{{"toolCallId": "a", "name": "bash", "arguments": "ls"}, {"toolCallId": "a", "name": "view"}}}),
			sub("agent", ev("e3", "abort", "2026-11-17T09:00:03.000Z", M{"reason": "user_abort"})),
			ev("e4", "abort", "2026-11-17T09:00:04.000Z", M{"reason": "user_initiated"}),
			ev("e5", "tool.execution_complete", "2026-11-17T09:00:05.000Z", M{"toolCallId": "a", "success": false, "error": "a string"}),
			ev("e6", "session.usage_record", "2026-11-17T09:00:06.000Z", M{"usage": M{"apiCallId": "x", "inputTokens": 9e18, "duration": -1}}),
		},
	} {
		path := filepath.Join(f.TempDir(), "seed.jsonl")
		sourcetest.Write(f, path, lines...)
		seed, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(seed)
	}
	// One store for every input: a worker runs its inputs one at a time, and a directory
	// made and removed for each would be most of what an input costs
	root := f.TempDir()
	path := filepath.Join(root, "c0ffee00-0000-4000-8000-00000000f022", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		sourcetest.Attempt(t, Spec.Open(root))
	})
}

// FuzzWorkspace reads whatever a workspace.yaml might hold: any text is a document, of
// scalars or of none.
func FuzzWorkspace(f *testing.F) {
	for _, seed := range []string{
		"name: plain\n",
		"name: \"a\\u00e9\\\n  b\"\nsummary: 'it''s'\n",
		"name: |-\n  two\n  lines\n",
		"name: >\n\n   \n",
		"name: \"\\",
		"name: '",
		"name:\n  - a\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		yamlScalars(doc)
	})
}
