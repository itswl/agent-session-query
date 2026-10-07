package source

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// TestFileRecordCacheReusesUnchanged: an unchanged file must not have its head reparsed.
func TestFileRecordCacheReusesUnchanged(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "p", "a.jsonl")
	write(t, path, `{"type":"session","id":"cached"}`)

	cache := newFileRecordCache(nil) // no counter: this test is about record reuse
	builds := 0
	build := func(p, modISO string) Record {
		builds++
		return NewRecord(Record{Key: p, SessionID: "cached"}, modISO)
	}

	if got := cache.records([]string{path}, build); len(got) != 1 || builds != 1 {
		t.Fatalf("first pass = %d entries / %d parses", len(got), builds)
	}
	if got := cache.records([]string{path}, build); len(got) != 1 || builds != 1 {
		t.Fatalf("the file did not change yet it was parsed again: %d times", builds)
	}

	// Changed content (and therefore size) must trigger a reparse
	write(t, path, `{"type":"session","id":"cached"}`, `{"type":"message"}`)
	if cache.records([]string{path}, build); builds != 2 {
		t.Fatalf("a changed file should be reparsed, got %d parses", builds)
	}

	// A deleted file drops out of the listing and its cache entry goes with it
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := cache.records([]string{path}, build); len(got) != 0 {
		t.Fatalf("the file is gone but %d entries were still listed", len(got))
	}
	if len(cache.entries) != 0 {
		t.Fatalf("the cache was not cleaned up: %v", cache.entries)
	}
}

// TestMessageSink: the earliest N must stop early, and the latest N must run to the end
// while keeping memory tied to limit
func TestMessageSink(t *testing.T) {
	feed := func(sink *messageSink, n int) int {
		fed := 0
		for i := 0; i < n; i++ {
			fed++
			if !sink.add(map[string]any{"id": i}) {
				break
			}
		}
		return fed
	}
	ids := func(items []map[string]any) []int {
		out := []int{}
		for _, m := range items {
			out = append(out, m["id"].(int))
		}
		return out
	}

	// Earliest 3: it should stop after the third
	head := newMessageSink(MessageQuery{Limit: 3})
	if fed := feed(head, 100); fed != 3 {
		t.Fatalf("the earliest N should stop at the third, but %d were fed", fed)
	}
	if got := ids(head.result()); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("earliest 3 = %v", got)
	}

	// Latest 3: it has to scan all the way, and the result comes back chronological
	tail := newMessageSink(MessageQuery{Limit: 3, FromEnd: true})
	if fed := feed(tail, 100); fed != 100 {
		t.Fatalf("the latest N must scan everything, but only %d were fed", fed)
	}
	if got := ids(tail.result()); !reflect.DeepEqual(got, []int{97, 98, 99}) {
		t.Fatalf("latest 3 = %v", got)
	}
	if len(tail.items) != 3 {
		t.Fatalf("the ring buffer should hold only 3, got %d", len(tail.items))
	}

	// Fewer than limit comes back as-is
	few := newMessageSink(MessageQuery{Limit: 10, FromEnd: true})
	feed(few, 2)
	if got := ids(few.result()); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("below limit = %v", got)
	}
	// limit=0 takes nothing at all
	zero := newMessageSink(MessageQuery{Limit: 0})
	if fed := feed(zero, 5); fed != 1 || len(zero.result()) != 0 {
		t.Fatalf("limit=0 should stop immediately and stay empty: fed=%d len=%d", fed, len(zero.result()))
	}
}

// TestSQLiteURIWindowsPath: backslashes inside SQLite's file: URI are ambiguous with escapes
func TestSQLiteURIWindowsPath(t *testing.T) {
	got := sqliteURI(`C:\Users\dev\.hermes\state.db`)
	if runtime.GOOS == "windows" {
		if got != "file:C:/Users/dev/.hermes/state.db" {
			t.Fatalf("a Windows path should be converted to forward slashes: %q", got)
		}
		return
	}
	// Off Windows a backslash is a legal filename character and filepath.ToSlash leaves it be
	if got != `file:C:\Users\dev\.hermes\state.db` {
		t.Fatalf("paths should not be rewritten off Windows: %q", got)
	}
}

// TestLastRecordTime: read the last record's time from the tail of the file, accepting all
// three placements
func TestLastRecordTime(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, content, want string }{
		{"top-level timestamp", `{"type":"a","timestamp":"2026-09-01T01:00:00Z"}
{"type":"b","timestamp":"2026-09-02T02:00:00Z"}`, "2026-09-02T02:00:00Z"},
		{"inside message", `{"type":"message","message":{"timestamp":"2026-09-03T03:00:00Z"}}`, "2026-09-03T03:00:00Z"},
		{"a $set patch row", `{"sessionId":"g","lastUpdated":"2026-09-01T00:00:00Z"}
{"$set":{"lastUpdated":"2026-09-04T04:00:00Z"}}`, "2026-09-04T04:00:00Z"},
		{"trailing rows with no time", `{"type":"a","timestamp":"2026-09-05T05:00:00Z"}
{"type":"mode"}
{"type":"atis-latch"}`, "2026-09-05T05:00:00Z"},
		{"no time anywhere", `{"type":"mode"}
{"type":"atis-latch"}`, ""},
		{"empty file", "", ""},
	}
	for _, c := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "_")+".jsonl")
		if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := lastRecordTime(path); got != c.want {
			t.Errorf("lastRecordTime(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestLastRecordTimeGrowsWindow: when the first tail window holds no complete record, the
// window has to grow
func TestLastRecordTimeGrowsWindow(t *testing.T) {
	saved := tailWindows
	tailWindows = []int64{64, 512, 4 << 20} // shrunk so the case is easy to construct
	defer func() { tailWindows = saved }()

	path := filepath.Join(t.TempDir(), "big.jsonl")
	// The last line is long: a 64-byte window cuts it in half, so it takes a larger one
	long := `{"type":"a","timestamp":"2026-09-06T06:00:00Z","pad":"` + strings.Repeat("x", 300) + `"}`
	if err := os.WriteFile(path, []byte("{\"type\":\"head\"}\n"+long+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lastRecordTime(path); got != "2026-09-06T06:00:00Z" {
		t.Fatalf("the window did not grow: %q", got)
	}
}
