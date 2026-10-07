package app

import "testing"

// TestListVersionIncludesMessageCounts: the background count landing must move the
// list's ETag, or a revalidating page keeps its 304 and the count-less body — the one
// that renders "counting…" — until something unrelated changes.
func TestListVersionIncludesMessageCounts(t *testing.T) {
	base := []record{newRecord(map[string]any{
		"source": "pi", "key": "k", "updatedAt": "2026-01-01T00:00:00",
	}, "")}
	without := listVersion(base, nil)

	counted := []record{newRecord(map[string]any{
		"source": "pi", "key": "k", "updatedAt": "2026-01-01T00:00:00", "messageCount": float64(12),
	}, "")}
	with := listVersion(counted, nil)

	if without == with {
		t.Fatal("a count arriving must change the tag")
	}
	if again := listVersion(counted, nil); again != with {
		t.Fatal("the tag must stay stable for unchanged records")
	}
}
