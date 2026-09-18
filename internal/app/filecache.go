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

	// Message counts, filled in the background.
	//
	// The list is fast because it only globs and reads file heads; counting a session's
	// messages means reading it whole, and the corpus here is 231 files / 453 MB —
	// measured at 2.3 s for the lot. That is a fine price once per file version and an
	// unacceptable one to pay inside a request the page repeats every ten seconds, so a
	// list returns whatever counts it already has and queues the rest. The next poll has
	// them. Keyed by (mtime, size) like the records are.
	countMu  sync.Mutex
	counts   map[string]fileCountEntry
	counting map[string]bool // queued or in flight
	queue    []countRequest
	working  bool

	// countFile is the source's own rule for what counts as a message (claude skips
	// sidechains, codex skips the developer row, ...), so the list and the session's final
	// result agree on the number.
	countFile func(path string) int
}

// hasCount reports whether the record already carries this count
func (e fileRecordEntry) hasCount(n int) bool {
	got, ok := toFloat(e.rec.fields["messageCount"])
	return ok && int(got) == n
}

type fileCountEntry struct {
	mod  time.Time
	size int64
	n    int
}

type countRequest struct {
	path string
	mod  time.Time
	size int64
}

type fileRecordEntry struct {
	mod  time.Time
	size int64
	rec  record
}

func newFileRecordCache(countFile func(path string) int) *fileRecordCache {
	return &fileRecordCache{
		entries:   map[string]fileRecordEntry{},
		counts:    map[string]fileCountEntry{},
		counting:  map[string]bool{},
		countFile: countFile,
	}
}

// countFor returns a cached count for exactly this version of the file, or queues the
// file to be counted and reports false.
func (c *fileRecordCache) countFor(path string, mod time.Time, size int64) (int, bool) {
	if c.countFile == nil {
		return 0, false
	}
	c.countMu.Lock()
	if hit, ok := c.counts[path]; ok && hit.size == size && hit.mod.Equal(mod) {
		c.countMu.Unlock()
		return hit.n, true
	}
	if !c.counting[path] {
		c.counting[path] = true
		c.queue = append(c.queue, countRequest{path: path, mod: mod, size: size})
	}
	start := !c.working
	c.working = true
	c.countMu.Unlock()

	if start {
		go c.countWorker()
	}
	return 0, false
}

// countWorker drains the queue one file at a time. A single worker is deliberate: this is
// background work competing with the requests that are the point of the service.
func (c *fileRecordCache) countWorker() {
	for {
		c.countMu.Lock()
		if len(c.queue) == 0 {
			c.working = false
			c.countMu.Unlock()
			return
		}
		req := c.queue[0]
		c.queue = c.queue[1:]
		c.countMu.Unlock()

		n := c.countFile(req.path)

		c.countMu.Lock()
		delete(c.counting, req.path)
		if n >= 0 {
			c.counts[req.path] = fileCountEntry{mod: req.mod, size: req.size, n: n}
		}
		c.countMu.Unlock()
	}
}

// keepCounts drops counts for files this round did not see, so deleted sessions do not
// pin memory. Called with the same path set the records are built from.
func (c *fileRecordCache) keepCounts(paths []string) {
	if c.countFile == nil {
		return
	}
	live := make(map[string]bool, len(paths))
	for _, p := range paths {
		live[p] = true
	}
	c.countMu.Lock()
	for path := range c.counts {
		if !live[path] {
			delete(c.counts, path)
		}
	}
	c.countMu.Unlock()
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
			// A count that arrived since this record was cached is attached by replacing
			// the field map rather than writing into it: the cached record is read by
			// other requests, and mutating its map under them would be a data race.
			if n, counted := c.countFor(path, mod, size); counted && !hit.hasCount(n) {
				fields := make(map[string]any, len(hit.rec.fields)+1)
				for k, v := range hit.rec.fields {
					fields[k] = v
				}
				fields["messageCount"] = n
				hit.rec.fields = fields
			}
			fresh[path] = hit
			out = append(out, hit.rec)
			continue
		}

		rec := build(path, mod.UTC().Format("2006-01-02T15:04:05"))
		if n, ok := c.countFor(path, mod, size); ok {
			rec.fields["messageCount"] = n // freshly built, not shared yet
		}
		fresh[path] = fileRecordEntry{mod: mod, size: size, rec: rec}
		out = append(out, rec)
	}

	c.mu.Lock()
	c.entries = fresh
	c.mu.Unlock()
	c.keepCounts(paths)
	return out
}
