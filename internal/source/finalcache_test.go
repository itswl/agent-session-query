package source

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFinalCache: an unchanged file's final is served from memory; a rewrite invalidates
// it; a missing file is read but never banked.
func TestFinalCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	var cache finalCache
	reads := 0
	read := func() map[string]any {
		reads++
		return map[string]any{"n": reads}
	}

	first := cache.load([]string{path}, read)
	if first["n"] != 1 || reads != 1 {
		t.Fatalf("first load = %v after %d reads", first, reads)
	}
	second := cache.load([]string{path}, read)
	if second["n"] != 1 || reads != 1 {
		t.Fatalf("second load re-read an unchanged file (%d reads)", reads)
	}

	// A rewrite moves the fingerprint
	if err := os.WriteFile(path, []byte("one more"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cache.load([]string{path}, read); got["n"] != 2 || reads != 2 {
		t.Fatalf("a rewritten file was not re-read: %v after %d reads", got, reads)
	}

	// A missing file yields the read's answer but nothing cached
	gone := filepath.Join(dir, "gone.jsonl")
	if got := cache.load([]string{gone}, read); got["n"] != 3 || reads != 3 {
		t.Fatalf("a missing file was not read: %v after %d reads", got, reads)
	}
	if got := cache.load([]string{gone}, read); got["n"] != 4 || reads != 4 {
		t.Fatalf("a missing file's answer was cached: %v after %d reads", got, reads)
	}
}
