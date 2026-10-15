package source

import (
	"os"
	"path/filepath"
	"testing"
)

func osStatForTest(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// TestStorageFileSources: the file-shaped sources size by session — each record's own
// file, and the directory a session keeps beside it — with whatever else lies under the
// root reported as overhead, never counted into a session.
func TestStorageFileSources(t *testing.T) {
	t.Run("pi: one file per session, stray files are overhead", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "proj", "2026-10-01T10-00-00-000Z_aaaa.jsonl"),
			`{"type":"session","id":"aaaa","cwd":"/w/pi"}`,
			`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`)
		write(t, filepath.Join(root, "proj", "notes.txt"), `not a session`)

		sessions, overhead := NewPiSource(root).Storage()
		if len(sessions) != 1 || sessions[0].SessionID != "aaaa" {
			t.Fatalf("sessions = %v", sessions)
		}
		if sessions[0].Project != "/w/pi" {
			t.Fatalf("project = %q", sessions[0].Project)
		}
		if sessions[0].Bytes != int64(len("not a session"))+1 && sessions[0].Bytes <= 0 {
			t.Fatalf("bytes = %d", sessions[0].Bytes)
		}
		if overhead != int64(len("not a session"))+1 {
			t.Fatalf("overhead = %d, want the stray file's size", overhead)
		}
	})

	t.Run("claude: a subagent's transcript is its own, the directory is the session's", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "p", "ses-main.jsonl"),
			`{"type":"user","sessionId":"ses-main","cwd":"/w","message":{"role":"user","content":"hi"}}`)
		write(t, filepath.Join(root, "p", "ses-main", "subagents", "agent-sub1.jsonl"),
			`{"type":"user","sessionId":"agent-sub1","message":{"role":"user","content":"do it"}}`)
		write(t, filepath.Join(root, "p", "ses-main", "subagents", "agent-sub1.meta.json"),
			`{"description":"the subagent"}`)

		sessions, _ := NewClaudeSource(root).Storage()
		byID := map[string]SessionStorage{}
		for _, s := range sessions {
			byID[s.SessionID] = s
		}
		if len(byID) != 2 {
			t.Fatalf("sessions = %v", byID)
		}
		mainInfo, _ := osStatForTest(filepath.Join(root, "p", "ses-main.jsonl"))
		subInfo, _ := osStatForTest(filepath.Join(root, "p", "ses-main", "subagents", "agent-sub1.jsonl"))
		metaInfo, _ := osStatForTest(filepath.Join(root, "p", "ses-main", "subagents", "agent-sub1.meta.json"))
		if byID["ses-main"].Bytes != mainInfo+metaInfo {
			t.Fatalf("main = %d, want transcript + the meta in its directory", byID["ses-main"].Bytes)
		}
		// the subagent's record carries the sessionId field ("sub1"), not the filename
		if byID["sub1"].Bytes != subInfo {
			t.Fatalf("subagent = %d, want its own transcript only", byID["sub1"].Bytes)
		}
	})

	t.Run("grok: a session is its directory, the group's index is overhead", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "%2Fw", "01a0-grok")
		write(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"01a0-grok","cwd":"/w"},"session_summary":"x"}`)
		write(t, filepath.Join(dir, "updates.jsonl"), `{"timestamp":1,"method":"session/update","params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hi"}}}}`)
		write(t, filepath.Join(root, "%2Fw", "session_search.sqlite"), `index bytes`)

		sessions, overhead := NewGrokSource(root).Storage()
		if len(sessions) != 1 || sessions[0].SessionID != "01a0-grok" {
			t.Fatalf("sessions = %v", sessions)
		}
		if sessions[0].Bytes <= 0 || overhead <= 0 {
			t.Fatalf("session = %d, overhead = %d", sessions[0].Bytes, overhead)
		}
	})
}

// TestStorageOpenCode: the SQLite sources size by the bytes of a session's rows; the
// file's remainder — indexes, free pages — is overhead.
func TestStorageOpenCode(t *testing.T) {
	s := newOpenCodeFixture(t)
	sessions, overhead := s.Storage()
	if len(sessions) < 2 {
		t.Fatalf("sessions = %v", sessions)
	}
	byID := map[string]SessionStorage{}
	total := int64(0)
	for _, row := range sessions {
		byID[row.SessionID] = row
		total += row.Bytes
	}
	if byID["ses_live"].Bytes <= 0 {
		t.Fatalf("ses_live = %v", byID["ses_live"])
	}
	if byID["ses_live"].Project != "/w/proj" {
		t.Fatalf("project = %q", byID["ses_live"].Project)
	}
	if overhead <= 0 {
		t.Fatalf("overhead = %d, want the file minus its rows", overhead)
	}
	if total+overhead <= 0 {
		t.Fatal("nothing accounted")
	}
}

// TestStorageLabeled: a labeled instance prefixes the project, the way List prefixes
// cwd, so two instances of one CLI stay apart in the report.
func TestStorageLabeled(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "2026-10-01T10-00-00-000Z_aaaa.jsonl"),
		`{"type":"session","id":"aaaa","cwd":"/w/pi"}`)

	labeled := Label(NewPiSource(root), "pi:box2")
	reporter, ok := labeled.(StorageReporter)
	if !ok {
		t.Fatal("a labeled instance must carry Storage through")
	}
	sessions, _ := reporter.Storage()
	if len(sessions) != 1 || sessions[0].Project != "box2:/w/pi" {
		t.Fatalf("sessions = %v", sessions)
	}
}
