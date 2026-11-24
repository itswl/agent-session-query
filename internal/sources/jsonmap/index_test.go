package jsonmap

import (
	"os"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/sources/kit"
)

// The index is written anew whole, and at its size when only a time in it moved; on a
// filesystem whose clock moves in coarse ticks (tmpfs) such a rewrite keeps the file's
// modification time too. Until the file has been still for longer than a tick can be, it
// is parsed again at every call; after that, what was parsed stands while its fingerprint
// does.
func TestAnIndexRewrittenAtItsSizeIsParsedAgainUntilItIsStill(t *testing.T) {
	x := At(t.TempDir())
	var at time.Time
	write := func(updated string) {
		t.Helper()
		if err := os.WriteFile(x.Path, []byte(`{"agent:main:one":{"session_id":"one","updated_at":"`+updated+`"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if at.IsZero() {
			info, err := os.Stat(x.Path)
			if err != nil {
				t.Fatal(err)
			}
			at = info.ModTime()
		}
		if err := os.Chtimes(x.Path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	updated := func() string {
		t.Helper()
		entries, err := x.Entries()
		if err != nil || len(entries) != 1 {
			t.Fatalf("entries = %+v, %v", entries, err)
		}
		return kit.Str(entries[0].Info["updated_at"])
	}
	t.Cleanup(func() { kit.Now = time.Now })

	write("2026-11-24T10:00:01Z")
	kit.Now = func() time.Time { return at.Add(time.Second) }
	if got := updated(); got != "2026-11-24T10:00:01Z" {
		t.Fatalf("updated = %q", got)
	}
	write("2026-11-24T10:00:02Z")
	if got := updated(); got != "2026-11-24T10:00:02Z" {
		t.Errorf("an index written a second ago, rewritten at its size and time: updated = %q, want what it says now", got)
	}

	// Still for an hour: what is parsed now stands for the fingerprint
	kit.Now = func() time.Time { return at.Add(time.Hour) }
	if got := updated(); got != "2026-11-24T10:00:02Z" {
		t.Fatalf("updated = %q", got)
	}
	write("2026-11-24T10:00:03Z")
	if got := updated(); got != "2026-11-24T10:00:02Z" {
		t.Errorf("an unmoved fingerprint of a still index: updated = %q, want what was parsed under it", got)
	}
}
