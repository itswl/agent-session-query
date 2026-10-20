package source

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestDBSnapshotCache: an unchanged database is answered from the cache without another
// read; a -wal appearing counts as a change; a failed read is never banked.
func TestDBSnapshotCache(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	if err := os.WriteFile(db, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}

	var cache dbSnapshotCache[[]string]
	reads := 0
	read := func() ([]string, error) {
		reads++
		return []string{"row"}, nil
	}

	if _, err := cache.load(db, read); err != nil || reads != 1 {
		t.Fatalf("first load = %v after %d reads, want one read", err, reads)
	}
	if _, err := cache.load(db, read); err != nil || reads != 1 {
		t.Fatalf("second load re-read an unchanged database (%d reads)", reads)
	}

	// A write lands in the write-ahead log first: the db file's mtime stays put, and the
	// -wal appearing is the only sign anything moved
	if err := os.WriteFile(db+"-wal", []byte("w"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.load(db, read); err != nil || reads != 2 {
		t.Fatalf("a new -wal was not seen as a change (%d reads)", reads)
	}

	// A failed read is retried, not cached
	if err := os.WriteFile(db+"-wal", []byte("ww"), 0o600); err != nil {
		t.Fatal(err)
	}
	fail := func() ([]string, error) { return nil, errors.New("locked") }
	if _, err := cache.load(db, fail); err == nil {
		t.Fatal("a failed read came back without its error")
	}
	if _, err := cache.load(db, read); err != nil || reads != 3 {
		t.Fatalf("after a failure the next call did not read again (%d reads)", reads)
	}

	// A missing database is a stamp too, and the read is attempted (the source reports
	// the failure the way it always did)
	gone := filepath.Join(dir, "gone.db")
	if _, err := cache.load(gone, read); err != nil || reads != 4 {
		t.Fatalf("a missing database was not read (%d reads)", reads)
	}
}
