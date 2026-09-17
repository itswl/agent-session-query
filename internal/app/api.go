package app

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SessionQueryAPI dispatches requests to the individual source adapters.
//
// Session lists are cached per source with a short TTL (2 seconds by default, tunable
// with --cache-ttl, 0 disables it). Both lookup and listing go through it, so a single
// request never has to rescan every session file. Messages and final results always read
// from disk; the cache only covers the "which sessions exist" layer of metadata.
type SessionQueryAPI struct {
	sources  []SessionSource
	cacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]*cachedRecords
}

// cachedRecords is one source's list cache.
//
// scan guarantees at most one scan per source at a time. When the cache has just expired
// and several requests arrive together, having each rescan the directory is pure waste
// (a cache stampede) — now one scans and the rest wait for its result.
type cachedRecords struct {
	scan    sync.Mutex
	at      time.Time
	records []record
}

func newSessionQueryAPI(sources []SessionSource, cacheTTLSeconds float64) *SessionQueryAPI {
	if cacheTTLSeconds < 0 {
		cacheTTLSeconds = 0
	}
	return &SessionQueryAPI{
		sources:  sources,
		cacheTTL: time.Duration(cacheTTLSeconds * float64(time.Second)),
		cache:    map[string]*cachedRecords{},
	}
}

// recordsOf returns one source's session list (TTL cached)
func (a *SessionQueryAPI) recordsOf(source SessionSource) []record {
	if a.cacheTTL <= 0 {
		return safeList(source)
	}

	a.mu.Lock()
	entry, ok := a.cache[source.Mode()]
	if !ok {
		entry = &cachedRecords{}
		a.cache[source.Mode()] = entry
	}
	a.mu.Unlock()

	entry.scan.Lock()
	defer entry.scan.Unlock()
	if !entry.at.IsZero() && time.Since(entry.at) < a.cacheTTL {
		return entry.records
	}
	entry.records = safeList(source)
	entry.at = time.Now()
	return entry.records
}

// safeList keeps one failing source from taking down the whole list
func safeList(source SessionSource) (out []record) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[WARN] %s failed to list sessions: %v\n", source.Mode(), rec)
			out = nil
		}
	}()
	return source.List()
}

// listSessions returns the merged session list plus a weak validator (used as the ETag).
//
// The web page polls every 10 seconds and the list has almost always not changed. With an
// ETag those polls end at a 304, instead of serialising and transferring the entire list
// again every time.
func (a *SessionQueryAPI) listSessions() ([]map[string]any, string) {
	all := []record{}
	for _, source := range a.sources {
		all = append(all, a.recordsOf(source)...)
	}
	// Newest update time first; equal times keep source order (stable sort)
	sort.SliceStable(all, func(i, j int) bool { return all[i].newerThan(all[j]) })

	out := make([]map[string]any, 0, len(all))
	for _, item := range all {
		out = append(out, item.public())
	}
	return out, listVersion(all)
}

// listVersion is the list's weak validator: membership, update time or status changing
// all change it.
func listVersion(records []record) string {
	h := fnv.New64a()
	for _, r := range records {
		for _, field := range []string{"source", "key", "updatedAt", "status"} {
			_, _ = h.Write([]byte(r.str(field)))
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte{0x1e})
	}
	return `W/"` + strconv.FormatUint(h.Sum64(), 16) + `"`
}

// findSession picks the best match across every enabled source (exact hits win, and no
// source shadows another)
func (a *SessionQueryAPI) findSession(pattern string) (SessionSource, record, bool) {
	pattern = strings.TrimSpace(pattern)
	if strings.HasPrefix(pattern, "Run: ") {
		pattern = pattern[len("Run: "):]
	}
	if strings.HasPrefix(pattern, "Session: ") {
		pattern = pattern[len("Session: "):]
	}
	if pattern == "" {
		return nil, record{}, false
	}
	patternLower := normalizeForMatch(pattern)

	var bestSource SessionSource
	var bestRecord record
	bestRank := -1
	for _, source := range a.sources {
		for _, item := range a.recordsOf(source) {
			rank := item.matchRank(patternLower)
			if rank == -1 {
				continue
			}
			if bestRank == -1 || rank < bestRank {
				bestSource, bestRecord, bestRank = source, item, rank
			}
		}
	}
	if bestSource == nil {
		return nil, record{}, false
	}
	return bestSource, bestRecord, true
}

func (a *SessionQueryAPI) getSession(pattern string) (map[string]any, bool) {
	_, item, ok := a.findSession(pattern)
	if !ok {
		return nil, false
	}
	return item.public(), true
}

// safeParse keeps one session's parse blowing up from turning the request into a 500:
// log it and treat the result as "nothing parsed". Session files are written by other
// programs and their format can change at any time — messages and final both go through
// this.
func safeParse[T any](mode, what string, parse func() T) (out T) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[WARN] %s failed to parse %s: %v\n", mode, what, rec)
			var zero T
			out = zero
		}
	}()
	return parse()
}

func (a *SessionQueryAPI) getMessages(pattern string, q messageQuery) ([]map[string]any, bool) {
	source, item, ok := a.findSession(pattern)
	if !ok {
		return nil, false
	}
	messages := safeParse(source.Mode(), "messages", func() []map[string]any {
		return source.Messages(item, q)
	})
	if messages == nil {
		messages = []map[string]any{}
	}
	return messages, true
}

func (a *SessionQueryAPI) getFinalMessage(pattern string) (map[string]any, bool) {
	source, item, ok := a.findSession(pattern)
	if !ok {
		return nil, false
	}

	result := safeParse(source.Mode(), "the final result", func() map[string]any {
		return source.Final(item)
	})

	if result == nil {
		status := item.str("status")
		if status == "" {
			status = "unknown"
		}
		result = map[string]any{
			"status":       status,
			"isFinal":      false,
			"isProcessing": status == "running",
			"messageCount": 0,
			"source":       item.get("source"),
			"error":        "Session file not available yet (session may be still initializing)",
		}
	}
	return result, true
}

// listProjects groups sessions by project.
//
// This is the one thing this tool can do that the others cannot: what you did on a given
// repository with Claude Code, Codex and Gemini, all in a single view.
// Note that hermes / openclaw have neither cwd nor project and land in ungrouped.
func (a *SessionQueryAPI) listProjects() ([]map[string]any, int) {
	type bucket struct {
		sessions int
		sources  map[string]int
		latest   record
	}
	order := []string{}
	buckets := map[string]*bucket{}
	ungrouped := 0

	for _, source := range a.sources {
		for _, rec := range a.recordsOf(source) {
			name := rec.project()
			if name == "" {
				ungrouped++
				continue
			}
			b, ok := buckets[name]
			if !ok {
				b = &bucket{sources: map[string]int{}}
				buckets[name] = b
				order = append(order, name)
			}
			b.sessions++
			b.sources[rec.str("source")]++
			if b.latest.fields == nil || rec.newerThan(b.latest) {
				b.latest = rec
			}
		}
	}

	out := make([]map[string]any, 0, len(order))
	for _, name := range order {
		b := buckets[name]
		sources := make([]string, 0, len(b.sources))
		for mode := range b.sources {
			sources = append(sources, mode)
		}
		sort.Strings(sources)
		out = append(out, map[string]any{
			"project":       name,
			"shortName":     filepath.Base(name),
			"sessions":      b.sessions,
			"sources":       sources,
			"sourceCounts":  b.sources,
			"updatedAt":     b.latest.str("updatedAt"),
			"latestSession": b.latest.str("sessionId"),
			"isActive":      !b.latest.sortAt.IsZero() && time.Since(b.latest.sortAt) < activeWindow,
		})
	}
	// Most recently touched projects first
	sort.SliceStable(out, func(i, j int) bool {
		return toStr(out[i]["updatedAt"]) > toStr(out[j]["updatedAt"])
	})
	return out, ungrouped
}

func sourceModes(sources []SessionSource) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.Mode())
	}
	return out
}
