package claude

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// A session's files are its transcript and the outputs saved beside it; a subagent's, its
// transcript and its meta file. Copied by those to another folder, the session reads there
// as it did, a saved output and all (see internal/keep).
func TestFilesAreWhatASessionReadsFrom(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "proj")
	sourcetest.Write(t, filepath.Join(base, "sess-1", "tool-results", "toolu_9.txt"), "the last line says why it failed")
	stub := "<persisted-output>\nOutput too large (30.5KB). Full output saved to: /x/.claude/projects/proj/sess-1/tool-results/toolu_9.txt\n</persisted-output>"
	sourcetest.Write(t, filepath.Join(base, "sess-1.jsonl"),
		userRow("u1", "2026-10-01T10:00:00Z", "build it"),
		assistantRow("a1", "2026-10-01T10:00:01Z", "m1", nil, M{"type": "tool_use", "id": "t1", "name": "Bash", "input": M{"command": "make"}}),
		userRow("u2", "2026-10-01T10:00:02Z", []M{{"type": "tool_result", "tool_use_id": "t1", "content": stub}}))
	agent := filepath.Join(base, "sess-1", "subagents", "agent-aaa.jsonl")
	sourcetest.Write(t, agent, M{"type": "user", "isSidechain": true, "agentId": "aaa", "sessionId": "sess-1", "uuid": "x", "timestamp": "2026-10-01T10:00:02Z",
		"message": M{"role": "user", "content": "look"}})
	sourcetest.Write(t, strings.TrimSuffix(agent, ".jsonl")+".meta.json", M{"agentType": "Explore"})

	src := Spec.Open(root)
	keeper := src.(sources.Keeper)
	copyRoot := t.TempDir()
	refs, err := src.Scan(t.Context())
	if err != nil || len(refs) != 2 {
		t.Fatalf("scan = %v, %v", refs, err)
	}
	var all []string
	for _, ref := range refs {
		for _, f := range keeper.Files(ref) {
			rel, _ := filepath.Rel(root, f)
			all = append(all, filepath.ToSlash(rel))
			if b, err := os.ReadFile(f); err == nil {
				sourcetest.Write(t, filepath.Join(copyRoot, rel), string(b))
			}
		}
	}
	sort.Strings(all)
	if want := "proj/sess-1.jsonl proj/sess-1/subagents/agent-aaa.jsonl proj/sess-1/subagents/agent-aaa.meta.json proj/sess-1/tool-results/toolu_9.txt"; strings.Join(all, " ") != want {
		t.Errorf("files = %s\nwant    %s", strings.Join(all, " "), want)
	}
	copied := sourcetest.All(t, Spec.Open(copyRoot))
	main := sourcetest.ByID(t, copied, "sess-1")
	if out := sourcetest.Tools(main.Turn(t, 1))[0].Output; !strings.Contains(out, "the last line says why it failed") {
		t.Errorf("the copy's saved output = %q", out)
	}
	if a := sourcetest.ByID(t, copied, "aaa"); a.Session.Parent == nil || a.Session.Parent.Type != "Explore" {
		t.Errorf("the copy's subagent = %+v", a.Session.Parent)
	}
}
