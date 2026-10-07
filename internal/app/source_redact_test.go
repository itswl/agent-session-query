package app

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Titles the CLIs write themselves are text like any other: a display name or session
// title that quoted a key must not ride out through the list, the page or a brief.
// (Titles this service synthesises already went through titleFromUserText.)

func TestHermesDisplayNameRedacted(t *testing.T) {
	home := t.TempDir()
	def := hermesDef(home)
	write(t, def.sessionsJSON,
		`{"k1": {"session_id": "s1", "display_name": "deploy `+packSecret+`", "updated_at": "2026-01-01T00:00:00"}}`)

	source := newJsonMapSource(def)
	recs := source.List()
	if len(recs) != 1 {
		t.Fatalf("fixture listed %d sessions", len(recs))
	}
	if got := recs[0].str("displayName"); strings.Contains(got, packSecret) || !strings.Contains(got, "[redacted]") {
		t.Fatalf("displayName = %q, want the key redacted", got)
	}
}

// TestOpenCodeTitleRedacted: the title column is opencode's own text and reaches the
// list, the page and the brief; it is cleaned like every assembled title.
func TestOpenCodeTitleRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{openCodeSchema,
		`INSERT INTO session VALUES ('ses_x', '/w/p', 'deploy ` + packSecret + `', 'ask', NULL, 1, 2, NULL, 0, 0, 0, 0, 0, 0)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("executing %q failed: %v", stmt, err)
		}
	}
	db.Close()

	source := newOpenCodeSource(path)
	recs := source.List()
	if len(recs) != 1 {
		t.Fatalf("fixture listed %d sessions", len(recs))
	}
	if got := recs[0].str("shortKey"); strings.Contains(got, packSecret) || !strings.Contains(got, "[redacted]") {
		t.Fatalf("shortKey = %q, want the key redacted", got)
	}
}
