package app

import (
	"os"
	"sync"
	"time"
)

// fileRecordCache memoizes "the list record parsed out of a session file's head",
// keyed by (mtime, size).
//
// The four file-backed sources (Claude / Codex / Pi / Gemini) all have to open every
// session file and read its first few lines to get metadata like sessionId and cwd.
// That content only changes when the file itself does, so a stat is enough to tell
// whether last time's result still holds: in steady state List() degrades to a single
// round of stat calls and never re-parses an unchanged file.
//
// Measured over 173 real Claude sessions: 52 ms to parse them all, 2.4 ms for
// glob + stat alone. Nine tenths of the cost was re-parsing unchanged heads — and the
// web page polls /sessions every 10 seconds, hitting it every single time.
type fileRecordCache struct {
	mu      sync.Mutex
	entries map[string]fileRecordEntry
}

type fileRecordEntry struct {
	mod  time.Time
	size int64
	rec  record
}

func newFileRecordCache() *fileRecordCache {
	return &fileRecordCache{entries: map[string]fileRecordEntry{}}
}

// records resolves one record per path: unchanged mtime/size reuses the cached
// value, anything changed or unseen goes through build. build receives modISO, the
// file's mtime in ISO form — the stat already happened, so sources need not repeat it.
//
// Paths absent from this round are dropped, so deleted sessions do not pin memory.
func (c *fileRecordCache) records(paths []string, build func(path, modISO string) record) []record {
	out := make([]record, 0, len(paths))
	fresh := make(map[string]fileRecordEntry, len(paths))

	c.mu.Lock()
	known := c.entries
	c.mu.Unlock()

	for _, path := range paths {
		st, err := os.Stat(path)
		if err != nil {
			continue // just deleted; leave it out of this round
		}
		mod, size := st.ModTime(), st.Size()

		if hit, ok := known[path]; ok && hit.size == size && hit.mod.Equal(mod) {
			fresh[path] = hit
			out = append(out, hit.rec)
			continue
		}

		rec := build(path, mod.UTC().Format("2006-01-02T15:04:05"))
		fresh[path] = fileRecordEntry{mod: mod, size: size, rec: rec}
		out = append(out, rec)
	}

	c.mu.Lock()
	c.entries = fresh
	c.mu.Unlock()
	return out
}
