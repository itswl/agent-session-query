package app

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// Content search.
//
// No index. An index has to be written somewhere, maintained, and reasoned about when it
// goes stale, and that costs the "single binary, read-only, scp it anywhere" property.
// Measured locally, a bare scan over 470 MB / 173 sessions takes under a second, which is
// enough.
//
// The speed comes from ordering, not cleverness: run a case-insensitive Contains over the
// raw bytes first and only JSON-decode a line once it matches. That skips decoding for
// 99% of lines, and decoding is the expensive part of the scan.
const (
	defaultSearchLimit      = 30  // how many sessions to return at most
	defaultSearchPerSession = 3   // how many hits per session at most
	searchSnippetRadius     = 70  // characters kept on each side of a hit in a snippet
	maxSearchDepth          = 8   // recursion depth cap when hunting for body text in JSON
	cancelCheckLines        = 512 // scan this many lines between cancellation checks
)

// cancelled reports whether the caller has walked away. A search holds every core it
// can get, so an abandoned one has to stop rather than run to completion: the page
// fires a fresh search on every keystroke and only the last one is ever displayed.
func cancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// searchQuery is one content search.
type searchQuery struct {
	needle     string    // kept verbatim, echoed back to the caller
	lowered    []byte    // the needle lowercased (ASCII folding)
	limit      int       // how many sessions to return at most
	perSession int       // how many hits per session at most
	since      time.Time // only search sessions updated after this; zero means no limit
	until      time.Time // only search sessions updated before this; zero means no limit
}

// searchableSource lets a source implement content search itself.
// Anything that does not falls back to the generic path: scan the session file (nearly
// every source is one jsonl per session).
type searchableSource interface {
	Search(ctx context.Context, r record, q searchQuery) []map[string]any
}

// searchOutcome is one search's results and its scale.
// scanned and matched are reported separately: once results are truncated to limit, the
// caller still needs to know how much was left out.
type searchOutcome struct {
	results []map[string]any
	scanned int  // sessions actually scanned
	matched int  // sessions with a hit, possibly more than len(results)
	stopped bool // the search was cancelled part-way; results are incomplete
}

// search looks through every enabled source, newest session first.
func (a *SessionQueryAPI) search(ctx context.Context, q searchQuery) searchOutcome {
	type candidate struct {
		source SessionSource
		rec    record
	}

	all := []candidate{}
	for _, source := range a.sources {
		for _, rec := range a.recordsOf(source) {
			if !q.since.IsZero() && (rec.sortAt.IsZero() || rec.sortAt.Before(q.since)) {
				continue
			}
			if !q.until.IsZero() && (rec.sortAt.IsZero() || rec.sortAt.After(q.until)) {
				continue
			}
			all = append(all, candidate{source: source, rec: rec})
		}
	}
	// Newest first, so truncating at limit keeps the most recent
	sort.SliceStable(all, func(i, j int) bool { return all[i].rec.newerThan(all[j].rec) })

	hits := make([][]map[string]any, len(all))
	workers := runtime.GOMAXPROCS(0)
	if workers > len(all) {
		workers = len(all)
	}
	if workers > 0 {
		jobs := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					if cancelled(ctx) {
						continue // drain the channel without starting more work
					}
					c := all[i]
					hits[i] = safeParse(c.source.Mode(), "search results", func() []map[string]any {
						return searchOne(ctx, c.source, c.rec, q)
					})
				}
			}()
		}
		for i := range all {
			if cancelled(ctx) {
				break
			}
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	}

	out := searchOutcome{results: []map[string]any{}, scanned: len(all), stopped: cancelled(ctx)}
	for i, matches := range hits {
		if len(matches) == 0 {
			continue
		}
		out.matched++
		if len(out.results) >= q.limit {
			continue // keep counting matched so the caller learns how much was cut
		}
		item := all[i].rec.public()
		item["matches"] = matches
		item["matchCount"] = len(matches)
		out.results = append(out.results, item)
	}
	return out
}

// searchOne searches inside one session, using the source's own implementation when it
// has one and scanning the session file otherwise.
func searchOne(ctx context.Context, source SessionSource, rec record, q searchQuery) []map[string]any {
	if s, ok := source.(searchableSource); ok {
		return s.Search(ctx, rec, q)
	}
	return searchFile(ctx, rec.str("file"), q)
}

// searchFile scans one jsonl session file.
func searchFile(ctx context.Context, path string, q searchQuery) []map[string]any {
	if path == "" || len(q.lowered) == 0 {
		return nil
	}
	var hits []map[string]any
	var lower []byte // reused so each line costs no allocation
	lines := 0
	eachJSONLLine(path, func(line []byte) bool {
		// Sampled rather than checked every line: a channel receive per line would cost
		// more than the Contains that is the actual work here
		lines++
		if lines%cancelCheckLines == 0 && cancelled(ctx) {
			return false
		}
		lower = appendLowerASCII(lower[:0], line)
		if !bytes.Contains(lower, q.lowered) {
			return true
		}
		if hit := buildHit(line, q); hit != nil {
			hits = append(hits, hit)
		}
		return len(hits) < q.perSession
	})
	return hits
}

// buildHit turns a matching line into a result; it returns nil when the match landed
// only in a field name or an escape sequence.
func buildHit(line []byte, q searchQuery) map[string]any {
	var obj map[string]any
	if json.Unmarshal(line, &obj) != nil || obj == nil {
		return nil
	}
	text, ok := findMatchingText(obj, string(q.lowered), 0)
	if !ok {
		return nil
	}
	return map[string]any{
		"snippet":   snippetAround(text, string(q.lowered), searchSnippetRadius),
		"role":      hitRole(obj),
		"timestamp": strOr(obj["timestamp"], ""),
	}
}

// textFieldOrder lists the fields body text most likely lives in, searched in this order.
// Go map iteration is randomised, so without a fixed order the same query could return a
// different snippet on every run.
var textFieldOrder = []string{"text", "content", "thinking", "reasoning", "message", "payload"}

func findMatchingText(v any, needleLower string, depth int) (string, bool) {
	if depth > maxSearchDepth {
		return "", false
	}
	switch t := v.(type) {
	case string:
		if indexFold(t, needleLower) >= 0 {
			return t, true
		}
	case []any:
		for _, item := range t {
			if s, ok := findMatchingText(item, needleLower, depth+1); ok {
				return s, true
			}
		}
	case map[string]any:
		for _, key := range textFieldOrder {
			if inner, has := t[key]; has {
				if s, ok := findMatchingText(inner, needleLower, depth+1); ok {
					return s, true
				}
			}
		}
		keys := make([]string, 0, len(t))
		for key := range t {
			keys = append(keys, key)
		}
		sort.Strings(keys) // remaining fields go in key order so results stay stable
		for _, key := range keys {
			if s, ok := findMatchingText(t[key], needleLower, depth+1); ok {
				return s, true
			}
		}
	}
	return "", false
}

// hitRole does its best to recover the role of a hit (sources put it in different places)
func hitRole(obj map[string]any) string {
	if role := strOr(obj["role"], ""); role != "" {
		return role
	}
	for _, key := range []string{"message", "payload"} {
		if inner, ok := obj[key].(map[string]any); ok {
			if role := strOr(inner["role"], ""); role != "" {
				return role
			}
		}
	}
	return strOr(obj["type"], "")
}

// appendLowerASCII appends src to dst with A-Z folded to lowercase.
// It works byte by byte, so UTF-8 multi-byte sequences (lead byte >= 0x80) pass through
// untouched — scripts that have no case are unaffected.
func appendLowerASCII(dst, src []byte) []byte {
	for _, c := range src {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}

// indexFold finds needleLower case-insensitively (needleLower must already be lowercase)
// and returns -1 when absent. It does not copy all of s first, saving an allocation on
// long lines.
func indexFold(s, needleLower string) int {
	n := len(needleLower)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(s); i++ {
		hit := true
		for j := 0; j < n; j++ {
			c := s[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needleLower[j] {
				hit = false
				break
			}
		}
		if hit {
			return i
		}
	}
	return -1
}

// snippetAround cuts radius characters either side of the hit, marking each end it had
// to trim.
func snippetAround(text, needleLower string, radius int) string {
	idx := indexFold(text, needleLower)
	if idx < 0 {
		return truncate(text, radius*2, "…")
	}

	// Open a generous byte window first (UTF-8 is at most 4 bytes per character), then
	// narrow it down to a character count
	lo, hi := idx-radius*4, idx+len(needleLower)+radius*4
	if lo < 0 {
		lo = 0
	}
	if hi > len(text) {
		hi = len(text)
	}
	for lo > 0 && !utf8.RuneStart(text[lo]) { // align to a character boundary
		lo--
	}
	for hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi++
	}

	head := trimRunesFromLeft(text[lo:idx], radius)
	tail := trimRunesFromRight(text[idx:hi], radius+utf8.RuneCountInString(needleLower))
	out := head + tail
	if lo > 0 || len(head) < idx-lo {
		out = "…" + out
	}
	if hi < len(text) || len(tail) < hi-idx {
		out += "…"
	}
	return out
}

// trimRunesFromLeft keeps only the last n characters
func trimRunesFromLeft(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	drop := utf8.RuneCountInString(s) - n
	for i := range s {
		if drop == 0 {
			return s[i:]
		}
		drop--
	}
	return ""
}

// trimRunesFromRight keeps only the first n characters
func trimRunesFromRight(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
