package app

import (
	"crypto/sha256"
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
	// them. Keyed by (mtime, size) like the records are; a file that only grew is counted
	// from its previous end (see countStable), so a session being written costs its tail
	// per poll, not its whole length again.
	countMu  sync.Mutex
	counts   map[string]fileCountEntry
	counting map[string]bool // queued or in flight
	queue    []countRequest
	working  bool

	// countFile is the source's own rule for what counts as a message (claude skips
	// sidechains, codex skips the developer row, ...), so the list and the session's final
	// result agree on the number. It is asked for the messages in [from, EOF) and says
	// whether it could honor that offset: a counter whose rule is line-local resumes from
	// an append; one whose grouping spans lines (grok joins several updates into one
	// message) reports resumed=false and returns the whole file's count instead, because
	// a tail scan could split a group across the seam and miscount it.
	countFile func(path string, from int64) (count int, resumed bool)
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
	// tail fingerprints the last bytes of the counted version (see tailPrint). It is what
	// lets a resume prove the bytes before the offset are still the ones that produced n.
	tail [32]byte
}

type countRequest struct {
	path string
}

type fileRecordEntry struct {
	mod  time.Time
	size int64
	rec  record
}

func newFileRecordCache(countFile func(path string, from int64) (int, bool)) *fileRecordCache {
	return &fileRecordCache{
		entries:   map[string]fileRecordEntry{},
		counts:    map[string]fileCountEntry{},
		counting:  map[string]bool{},
		countFile: countFile,
	}
}

// countFor returns a cached count for exactly this version of the file, or queues the
// file to be counted. When a file is actively being written, return the previous count
// as a stale-but-useful value while the new scan runs; otherwise the UI stays on
// "counting…" forever for the session that is currently open.
func (c *fileRecordCache) countFor(path string, mod time.Time, size int64) (int, bool) {
	if c.countFile == nil {
		return 0, false
	}
	c.countMu.Lock()
	hit, have := c.counts[path]
	if have && hit.size == size && hit.mod.Equal(mod) {
		c.countMu.Unlock()
		return hit.n, true
	}
	if !c.counting[path] {
		// The version to bank is decided by the worker from its own stat (see
		// countStable): the requester's stat is already stale by the time the queue drains,
		// and banking a count under the requester's version was how the number drifted —
		// the next resume then started inside bytes the base already contained.
		c.counting[path] = true
		c.queue = append(c.queue, countRequest{path: path})
	}
	start := !c.working
	c.working = true
	c.countMu.Unlock()

	if start {
		go c.countWorker()
	}
	if have {
		return hit.n, true
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

		entry, ok := c.countStable(req.path)
		c.countMu.Lock()
		delete(c.counting, req.path) // a dropped scan is retried by the next poll
		if ok {
			c.counts[req.path] = entry
		}
		c.countMu.Unlock()
	}
}

// countStable counts one path and returns the entry to bank, or ok=false when the file
// changed while it was scanned — such a count describes no single version, and banking it
// under the requested (size, mtime) made the next resume start inside bytes already
// counted, inflating the number until the file happened to shrink.
func (c *fileRecordCache) countStable(path string) (fileCountEntry, bool) {
	pre, err := os.Stat(path)
	if err != nil {
		return fileCountEntry{}, false
	}
	c.countMu.Lock()
	prev, have := c.counts[path]
	c.countMu.Unlock()

	// Resume from the previous end only when it is provably still there: the size grew,
	// the offset sits just after a newline, and the bytes before it fingerprint the same.
	from, base := int64(0), 0
	if have && pre.Size() > prev.size && newlineBefore(path, prev.size) &&
		tailPrint(path, prev.size) == prev.tail {
		from, base = prev.size, prev.n
	}
	n, resumed := c.countFile(path, from)
	if from > 0 && resumed {
		n += base
	}

	post, err := os.Stat(path)
	if err != nil || post.Size() != pre.Size() || !post.ModTime().Equal(pre.ModTime()) {
		return fileCountEntry{}, false
	}
	return fileCountEntry{mod: pre.ModTime(), size: pre.Size(), n: n, tail: tailPrint(path, pre.Size())}, true
}

// tailPrint fingerprints up to the last 256 bytes before off. At resume time the counted
// bytes must be the ones the count was made from; a grown size with a newline at the
// boundary is not proof of that (a rewritten file can satisfy both by coincidence), so
// the tail is compared as well. It samples instead of hashing the whole prefix — a
// full-prefix hash would defeat the resume — and that is enough for real writers: a
// rewrite differs before the boundary long before its last 256 bytes, and an append only
// extends after it.
func tailPrint(path string, off int64) [32]byte {
	if off <= 0 {
		return [32]byte{}
	}
	const window = 256
	start := off - window
	if start < 0 {
		start = 0
	}
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}
	}
	defer f.Close()
	buf := make([]byte, off-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(buf)
}

// newlineBefore reports whether the byte just before off is a newline — the proof that
// off is a line boundary, so a scan may resume there.
func newlineBefore(path string, off int64) bool {
	if off <= 0 {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off-1); err != nil {
		return false
	}
	return b[0] == '\n'
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
