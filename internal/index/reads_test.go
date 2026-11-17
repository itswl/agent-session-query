package index

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources"
)

// A session whose files have not moved is not read again — unless the adapter now reads
// its CLI's files differently (sources.Spec.Reads): then every session of the CLI read
// with an older reading is read again at the next sync, once. A session a newer reading
// stored is left to it, so that two binaries sharing an index do not undo each other.
func TestANewReadingIsReadAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ix.Close() }()
	ctx := context.Background()
	sync := func(reads int) int {
		t.Helper()
		res, err := ix.Sync(ctx, []Store{{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: reads}, Source: many{n: 4}}}, SyncOptions{Force: true})
		if err != nil {
			t.Fatal(err)
		}
		return res.Read
	}
	type step struct {
		reads, want int
		why         string
	}
	steps := func(when string, steps []step) {
		t.Helper()
		for _, step := range steps {
			if n := sync(step.reads); n != step.want {
				t.Errorf("%s, reading %d: %d read, want %d (%s)", when, step.reads, n, step.want, step.why)
			}
		}
	}
	steps("a new index", []step{
		{0, 4, "the first read"},
		{0, 0, "nothing moved"},
		{1, 4, "the first counted reading: every session read again"},
		{1, 0, "the same reading: nothing"},
		{2, 4, "a new reading: every session again"},
		{2, 0, "and once only"},
		{1, 0, "an older binary leaves what a newer reading stored"},
		{2, 0, "and the newer one finds it as it left it"},
	})

	// An index from before readings were kept has none: it gains the column, every
	// session at reading 0, and keeps its sessions
	ix.Close()
	{
		raw, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.rw.Exec(`ALTER TABLE sessions DROP COLUMN reads`); err != nil {
			t.Fatal(err)
		}
		raw.Close()
	}
	if ix, err = Open(path); err != nil {
		t.Fatalf("an index without readings: %v", err)
	}
	var sessions, unread int
	if err := ix.rw.QueryRow(`SELECT count(*), count(*) FILTER (WHERE reads = 0) FROM sessions`).Scan(&sessions, &unread); err != nil {
		t.Fatal(err)
	}
	if sessions != 4 || unread != 4 {
		t.Fatalf("after the column came: %d sessions, %d at reading 0; want 4 and 4", sessions, unread)
	}
	steps("an index from before readings", []step{
		{2, 4, "read with no counted reading: every session again"},
		{2, 0, "and once only"},
	})
}
