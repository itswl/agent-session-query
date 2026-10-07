package app

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// countingProbe stands in for a source's message counter: it counts lines (a trailing
// line without a newline counts, as a Scanner yields it) from the requested offset, and
// records the offsets it was asked for.
type countingProbe struct {
	mu    sync.Mutex
	calls []int64
}

func (p *countingProbe) count(path string, from int64) (int, bool) {
	p.mu.Lock()
	p.calls = append(p.calls, from)
	p.mu.Unlock()
	return countLines(path, from)
}

// countLines counts lines from a byte offset; a final line without a newline counts, as
// the Scanner-based counters yield it.
func countLines(path string, from int64) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, true
	}
	if from > 0 && from <= int64(len(data)) {
		data = data[from:]
	}
	n := 0
	for i, b := range data {
		if b == '\n' || i == len(data)-1 {
			n++
		}
	}
	return n, true
}

func (p *countingProbe) lastOffset() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[len(p.calls)-1]
}

// fullCount runs a source's counter over the whole file — the shape the older tests used
// before the counters learned to resume from an offset.
func fullCount(counter func(string, int64) (int, bool), path string) int {
	n, _ := counter(path, 0)
	return n
}

// waitForCount enqueues this version of the file and waits until its own count is
// stored. It cannot poll through countFor: that deliberately answers with the previous
// version's stale count while a rescan runs (the page shows a stale number rather than
// "counting…" forever), so the stored entry is checked directly.
func waitForCount(t *testing.T, cache *fileRecordCache, path string, mod time.Time, size int64) int {
	t.Helper()
	cache.countFor(path, mod, size) // enqueue if this version is not counted yet
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cache.countMu.Lock()
		hit, ok := cache.counts[path]
		cache.countMu.Unlock()
		if ok && hit.size == size && hit.mod.Equal(mod) {
			return hit.n
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the count for %s never landed", path)
	return 0
}

// TestFileCacheIncrementalCount: appending to a session is counted from the previous end,
// not from the top — the fix for the quadratic cost of an actively written file — and the
// resume is refused whenever the offset is not provably on a line boundary.
func TestFileCacheIncrementalCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := &countingProbe{}
	cache := newFileRecordCache(probe.count)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := waitForCount(t, cache, path, st.ModTime(), st.Size()); n != 2 {
		t.Fatalf("first count = %d, want 2", n)
	}

	appendTo := func(s string) (time.Time, int64) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.ModTime(), st.Size()
	}

	// An append resumes at the old size
	mod, size := appendTo("c\nd\n")
	if n := waitForCount(t, cache, path, mod, size); n != 4 {
		t.Fatalf("count after append = %d, want 4", n)
	}
	if off := probe.lastOffset(); off != int64(len("a\nb\n")) {
		t.Fatalf("resume offset = %d, want %d", off, len("a\nb\n"))
	}

	// A line written without its newline yet counts once as a partial line...
	mod, size = appendTo("e")
	if n := waitForCount(t, cache, path, mod, size); n != 5 {
		t.Fatalf("count with a partial line = %d, want 5", n)
	}
	// ...and completing it cannot resume from just after the partial line: that byte is
	// inside a line, and a tail scan would count the whole line a second time
	mod, size = appendTo("\n")
	if n := waitForCount(t, cache, path, mod, size); n != 5 {
		t.Fatalf("count after completing the line = %d, want 5", n)
	}
	if off := probe.lastOffset(); off != 0 {
		t.Fatalf("completing a partial line must force a full recount, got offset %d", off)
	}

	// An ordinary append after that resumes again
	mod, size = appendTo("f\n")
	if n := waitForCount(t, cache, path, mod, size); n != 6 {
		t.Fatalf("count after another append = %d, want 6", n)
	}
	if off := probe.lastOffset(); off == 0 {
		t.Fatal("a plain append should resume, not recount")
	}

	// A file that shrank (rewritten, truncated) is recounted from the top
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := waitForCount(t, cache, path, st.ModTime(), st.Size()); n != 1 {
		t.Fatalf("count after truncation = %d, want 1", n)
	}
	if off := probe.lastOffset(); off != 0 {
		t.Fatalf("a shrunken file must be recounted from the top, got offset %d", off)
	}
}

// writingProbe writes one line into the file from inside its first count call — the shape
// of a session that grows while its count is being read.
type writingProbe struct {
	path  string
	once  sync.Once
	mu    sync.Mutex
	calls []int64
}

func (p *writingProbe) count(path string, from int64) (int, bool) {
	p.mu.Lock()
	p.calls = append(p.calls, from)
	p.mu.Unlock()
	p.once.Do(func() {
		f, err := os.OpenFile(p.path, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, _ = f.WriteString("c\n")
			_ = f.Close()
		}
	})
	return countLines(path, from)
}

// TestFileCacheCountDropsUnstableVersion: a write landing while the scan runs must not be
// banked under the requested version — the banked number would describe a longer file
// than its size claims, and every later resume would re-count the delta. The dropped scan
// is retried; the number that finally lands equals a full recount.
func TestFileCacheCountDropsUnstableVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := &writingProbe{path: path}
	cache := newFileRecordCache(probe.count)

	st0, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cache.countFor(path, st0.ModTime(), st0.Size()) // the probe writes during this scan

	// Wait until the probe's line has landed, then ask for that version
	deadline := time.Now().Add(3 * time.Second)
	var mod time.Time
	var size int64
	for {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() != st0.Size() {
			mod, size = st.ModTime(), st.Size()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe never wrote")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if n := waitForCount(t, cache, path, mod, size); n != 3 {
		t.Fatalf("count = %d, want 3 — the scan that raced the write must not be banked", n)
	}
}

// TestFileCacheReplacementForcesRecount: a longer file whose byte at the old size happens
// to be a newline satisfies the size and line-boundary checks; only the tail fingerprint
// can tell it is not an append, and resuming into it would add a stale base to foreign
// content.
func TestFileCacheReplacementForcesRecount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("aaa\nbbbbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := &countingProbe{}
	cache := newFileRecordCache(probe.count)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := waitForCount(t, cache, path, st.ModTime(), st.Size()); n != 2 {
		t.Fatalf("first count = %d, want 2", n)
	}

	replacement := "q\nw\ne\nr\nt\nu\nv\n" // 14 bytes; byte 9 is a newline
	if len(replacement) != 14 || replacement[9] != '\n' {
		t.Fatal("fixture drifted")
	}
	if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := waitForCount(t, cache, path, st2.ModTime(), st2.Size()); n != 7 {
		t.Fatalf("count after replacement = %d, want 7 (a resume here would add the stale base)", n)
	}
}
