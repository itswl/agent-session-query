package source

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// A session's final result changes only when its file does. /usage walks every session's
// final on the file-shaped sources, and /sessions/<id>/final asks for the same one again
// and again; without a memory that means re-reading the whole transcript every time. This
// cache answers from the files' own fingerprints — (mtime, size) for each file the final
// was built from — so a steady poll reads nothing.
//
// The cached map is shared with its readers and must not be modified: the same discipline
// as records() (see fileRecordCache). It is JSON-encoded or read, never written.
type finalCache struct {
	mu      sync.Mutex
	entries map[string]map[string]any // fingerprint → final
}

// maxCachedFinals bounds the memory a long-lived history can pin. When it is reached the
// whole map is dropped rather than evicting entries one by one: the next pass re-reads
// what it needs, and sessions a CLI deleted stop being remembered at all.
const maxCachedFinals = 4096

// finalStamp fingerprints every file a final is built from, or ok=false when one of them
// is missing — an answer that depends on a file that is not there is not cached.
func finalStamp(paths []string) (string, bool) {
	var b strings.Builder
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return "", false
		}
		fmt.Fprintf(&b, "%s\x00%d\x00%d\x00", p, info.ModTime().UnixNano(), info.Size())
	}
	return b.String(), true
}

// load returns the cached final while its files are unchanged, else reads it once and
// banks it under the fingerprint taken before the read.
func (c *finalCache) load(paths []string, read func() map[string]any) map[string]any {
	stamp, ok := finalStamp(paths)
	if ok {
		c.mu.Lock()
		final, hit := c.entries[stamp]
		c.mu.Unlock()
		if hit {
			return final
		}
	}
	final := read()
	if !ok || final == nil {
		return final
	}
	c.mu.Lock()
	if c.entries == nil || len(c.entries) >= maxCachedFinals {
		c.entries = make(map[string]map[string]any)
	}
	c.entries[stamp] = final
	c.mu.Unlock()
	return final
}
