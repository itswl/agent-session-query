package gemini

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// A conversation's files are every file it is written in and the .project_root of its
// project, which says the directory it ran in; a subagent's, its file and the same. Copied
// by those to another folder, a conversation reads there as it did (see internal/keep).
func TestFilesAreWhatAConversationReadsFrom(t *testing.T) {
	root := t.TempDir()
	chats := filepath.Join(root, "app", "chats")
	sourcetest.Write(t, filepath.Join(chats, "session-2026-09-13T12-50-41-aaaa.jsonl"),
		`{"sessionId":"g-1","startTime":"2026-09-13T12:50:41Z"}`,
		`{"type":"user","id":"u1","timestamp":"2026-09-13T12:50:42Z","content":[{"text":"first"}]}`)
	sourcetest.Write(t, filepath.Join(chats, "session-2026-09-13T12-51-41-bbbb.jsonl"),
		`{"sessionId":"g-1","startTime":"2026-09-13T12:51:41Z"}`,
		`{"type":"user","id":"u2","timestamp":"2026-09-13T12:51:42Z","content":[{"text":"second"}]}`)
	sourcetest.Write(t, filepath.Join(chats, "g-1", "agent-1.jsonl"),
		`{"sessionId":"agent-1","kind":"subagent"}`,
		`{"type":"user","id":"s1","timestamp":"2026-09-13T12:52:00Z","content":[{"text":"look"}]}`)
	if err := os.WriteFile(filepath.Join(root, "app", ".project_root"), []byte("/work/app"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourcetest.Write(t, filepath.Join(root, "app", "logs.json"), `[]`) // Gemini CLI's own, and no session's

	src := Spec.Open(root)
	keeper := src.(sources.Keeper)
	copyRoot := t.TempDir()
	var all []string
	for _, ref := range scan(t, src) {
		for _, f := range keeper.Files(ref) {
			rel, _ := filepath.Rel(root, f)
			all = append(all, filepath.ToSlash(rel))
			if b, err := os.ReadFile(f); err == nil {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(copyRoot, rel)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(copyRoot, rel), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	sort.Strings(all)
	want := "app/.project_root app/.project_root app/chats/g-1/agent-1.jsonl app/chats/session-2026-09-13T12-50-41-aaaa.jsonl app/chats/session-2026-09-13T12-51-41-bbbb.jsonl"
	if strings.Join(all, " ") != want {
		t.Errorf("files = %s\nwant    %s", strings.Join(all, " "), want)
	}
	for _, s := range sourcetest.All(t, Spec.Open(copyRoot)) {
		if s.Session.Cwd != "/work/app" {
			t.Errorf("%s in the copy ran in %q, want /work/app", s.Session.ID, s.Session.Cwd)
		}
		if s.Session.ID == "g-1" && s.Stats.Turns != 2 {
			t.Errorf("the copy of g-1 has %d turns, want both files' 2", s.Stats.Turns)
		}
	}
}
