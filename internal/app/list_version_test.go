package app

import "testing"

// TestListVersionIncludesMessageCounts: the background count landing must move the
// list's ETag, or a revalidating page keeps its 304 and the count-less body — the one
// that renders "counting…" — until something unrelated changes.
func TestListVersionIncludesMessageCounts(t *testing.T) {
	base := []record{newRecord(record{
		Source: "pi", Key: "k", UpdatedAt: "2026-01-01T00:00:00",
	}, "")}
	without := listVersion(base, nil)

	counted := []record{newRecord(record{
		Source: "pi", Key: "k", UpdatedAt: "2026-01-01T00:00:00",
		MessageCount: 12, HasCount: true,
	}, "")}
	with := listVersion(counted, nil)

	if without == with {
		t.Fatal("a count arriving must change the tag")
	}
	if again := listVersion(counted, nil); again != with {
		t.Fatal("the tag must stay stable for unchanged records")
	}
}
