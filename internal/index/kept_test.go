package index

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// An index an asq from before keeping made gains the column in place, and says of its
// transcripts which have not been copied since they were last read (see internal/keep).
func TestAnIndexFromBeforeKeepingGainsItsColumn(t *testing.T) {
	column := "  kept       TEXT NOT NULL DEFAULT '',     -- the stamp its files were last copied at (internal/keep)\n"
	if !strings.Contains(schema, column) {
		t.Fatalf("the column is not where the test looks for it: %q", column)
	}
	path := filepath.Join(t.TempDir(), "index.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(strings.Replace(schema, column, "", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO sessions (source, agent, key, stamp, id) VALUES ('claude', 'claude', 'k0', 's1', 'id0')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := t.Context()
	unkept, err := ix.Unkept(ctx, "claude", true, nil)
	if err != nil || len(unkept) != 1 || unkept[0].Key != "k0" || unkept[0].Stamp != "s1" {
		t.Fatalf("unkept = %+v, %v; want the one transcript", unkept, err)
	}
	if err := ix.Kept(ctx, unkept[0].SID, unkept[0].Stamp); err != nil {
		t.Fatal(err)
	}
	if unkept, err := ix.Unkept(ctx, "claude", false, []string{"id0"}); err != nil || len(unkept) != 0 {
		t.Errorf("once copied = %+v, %v; want none", unkept, err)
	}
}
