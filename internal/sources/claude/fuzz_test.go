package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// jsonl renders rows as the lines of a transcript, the way sourcetest.Write writes them.
func jsonl(t testing.TB, rows ...any) []byte {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if line, ok := row.(string); ok {
			b.WriteString(line + "\n")
			continue
		}
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

// FuzzRead reads whatever a transcript may hold, both as a session's own and as a fork's,
// beside a saved output a stub can name: an error is an answer, but a panic is not, and
// neither is a session that breaks what every session must hold.
func FuzzRead(f *testing.F) {
	stub := "<persisted-output>\nOutput too large (1KB). Full output saved to: /elsewhere/.claude/projects/proj/sess-1/tool-results/t1.txt\n\nPreview (first 2KB):\nline 1\n</persisted-output>"
	for _, rows := range [][]any{
		sessionRows(),
		eventsRows(),
		forkRows(),
		passedOverRows(),
		{
			userRow("u1", "2026-10-01T10:00:00Z", "build it"),
			assistantRow("a1", "2026-10-01T10:00:01Z", "m1", nil, M{"type": "tool_use", "id": "t1", "name": "Bash", "input": M{"command": "make"}}),
			userRow("u2", "2026-10-01T10:00:02Z", []M{{"type": "tool_result", "tool_use_id": "t1", "content": stub}},
				M{"toolUseResult": M{"persistedOutputPath": "/elsewhere/.claude/projects/proj/sess-1/tool-results/t1.txt", "durationMs": 1200}}),
		},
	} {
		f.Add(jsonl(f, rows...))
	}
	f.Add([]byte("{}\nnull\n[]\n\"text\"\n{\"type\":\"user\"}\n{\"type\":\"assistant\",\"message\":{\"content\":\"hi\"}}\n"))

	// One store for every input, each replacing the transcripts of the one before: a fuzz
	// worker runs its inputs one at a time, and a directory made and removed for each
	// would be most of what it spends
	root := f.TempDir()
	session := filepath.Join(root, "proj", "sess-1")
	write := func(t testing.TB, path string, b []byte) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(f, filepath.Join(session, "tool-results", "t1.txt"), []byte("line 1\nthe line the preview dropped\n"))
	write(f, filepath.Join(session, "subagents", "agent-fff.meta.json"), []byte(`{"agentType":"fork","toolUseId":"toolu_fork"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		write(t, session+".jsonl", data)
		write(t, filepath.Join(session, "subagents", "agent-fff.jsonl"), data)
		sourcetest.Attempt(t, Spec.Open(root))
	})
}
