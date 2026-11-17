package index

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources"
)

// A session whose files have not moved is not read again — unless the adapter now reads
// its CLI's files differently (sources.Spec.Reads): then every session of the CLI is read
// again at the next sync, once, and an index does not keep what an older reading made.
func TestANewReadingIsReadAgain(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	sync := func(reads int) int {
		t.Helper()
		res, err := ix.Sync(ctx, []Store{{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: reads}, Source: many{n: 4}}}, SyncOptions{Force: true})
		if err != nil {
			t.Fatal(err)
		}
		return res.Read
	}
	// An index made before readings were counted holds the files' own stamps
	if n := sync(0); n != 4 {
		t.Fatalf("first read: %d", n)
	}
	if n := sync(0); n != 0 {
		t.Errorf("nothing moved, and %d were read again", n)
	}
	for _, step := range []struct {
		reads, want int
		why         string
	}{
		{1, 4, "the first counted reading: every session read again"},
		{1, 0, "the same reading: nothing"},
		{2, 4, "a new reading: every session again"},
		{2, 0, "and once only"},
	} {
		if n := sync(step.reads); n != step.want {
			t.Errorf("reading %d: %d read, want %d (%s)", step.reads, n, step.want, step.why)
		}
	}
}
