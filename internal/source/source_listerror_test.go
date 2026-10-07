package source

import (
	"os"
	"path/filepath"
	"testing"
)

// A source that cannot be read must say so (see ListErrorReporter): showing it as an
// empty source is the exact failure the mechanism exists to prevent, and for seven of
// the eight sources a moved schema or a corrupt index used to read as "no sessions".

func TestJsonMapListErrorOnCorruptSessionsJSON(t *testing.T) {
	home := t.TempDir()
	def := openclawDef(home)
	if err := os.MkdirAll(filepath.Dir(def.sessionsJSON), 0o755); err != nil {
		t.Fatal(err)
	}
	// Truncated mid-write, as another process's half-finished write leaves it
	if err := os.WriteFile(def.sessionsJSON, []byte(`{"a": {"sessionId": "x"`), 0o644); err != nil {
		t.Fatal(err)
	}
	source := newJsonMapSource(def)
	if recs := source.List(); len(recs) != 0 {
		t.Fatalf("a corrupt index listed %d records", len(recs))
	}
	if err := source.ListError(); err == nil {
		t.Fatal("a corrupt sessions.json must be reported as a read failure, not an empty source")
	}

	// A valid but empty index is an empty source, not a failure
	if err := os.WriteFile(def.sessionsJSON, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	source.List()
	if err := source.ListError(); err != nil {
		t.Fatalf("an empty index is not an error: %v", err)
	}

	// A missing file is not an error either: the source is simply empty
	if err := os.Remove(def.sessionsJSON); err != nil {
		t.Fatal(err)
	}
	source.List()
	if err := source.ListError(); err != nil {
		t.Fatalf("a missing sessions.json is empty, not broken: %v", err)
	}
}

func TestOpenCodeListErrorOnUnreadableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(path, []byte("this is not a SQLite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := NewOpenCodeSource(path)
	if recs := source.List(); len(recs) != 0 {
		t.Fatalf("a database that cannot be read listed %d records", len(recs))
	}
	if err := source.ListError(); err == nil {
		t.Fatal("a database that cannot be read must be reported, not shown as empty")
	}
}

func TestOpenClawListErrorOnUnreadableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openclaw-agent.sqlite")
	if err := os.WriteFile(path, []byte("this is not a SQLite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := NewOpenClawSource([]string{path})
	if recs := source.List(); len(recs) != 0 {
		t.Fatalf("a database that cannot be read listed %d records", len(recs))
	}
	if err := source.ListError(); err == nil {
		t.Fatal("a database that cannot be read must be reported, not shown as empty")
	}
}
