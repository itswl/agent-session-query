package app

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Content search.
//
// No index. An index has to be written somewhere, maintained, and reasoned about when it
// goes stale, and that costs the "single binary, read-only, scp it anywhere" property.
// Measured locally, a bare scan over 562 MB / 143 sessions costs 1.9 s cold and 43-112 ms
// warm, and the warm number is the one an interactive page pays.
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
	// pattern limits the search to the one session it names, found the same way
	// get_session finds it (a full id, a fragment, a path fragment). Without it a search
	// spans every session; with it, "where in this session did we discuss X" is one call
	// instead of paging a 16 000-message session fifty at a time.
	pattern string
	// role keeps only hits from messages with that role — the human's words rather than
	// the answer that repeats them. Applied while collecting, not after, so per_session
	// counts hits that match rather than hits that happen to come first.
	role string
}

// probeLimit is what a source actually fetches per session: one hit more than the caller
// asked for.
//
// That extra hit is how "there were more" is known. Without it a source that returns
// exactly per_session hits is indistinguishable from one that ran out of file at exactly
// that point, and the alternative — every source reporting a flag of its own — would mean
// widening searchableSource and touching all five places that cap. The length says it
// instead, and search() trims before anything leaves.
func (q searchQuery) probeLimit() int {
	if q.perSession <= 0 {
		return 0
	}
	return q.perSession + 1
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
	// Why results are short, kept apart rather than as one flag. A caller that sees fewer
	// hits than it asked for needs to know which knob to turn, and the two answers are
	// different knobs: sessionsCut is limit, hitsCut is per_session. One boolean covering
	// both says only "something was cut" and leaves the caller guessing.
	sessionsCut bool // more sessions matched than limit returned
	hitsCut     bool // some session had more hits than per_session returned
}

// search looks through every enabled source, newest session first.
func (a *SessionQueryAPI) search(ctx context.Context, q searchQuery) searchOutcome {
	type candidate struct {
		source SessionSource
		rec    record
	}

	all := []candidate{}
	if q.pattern != "" {
		// Scoped: the pattern names one session, and only that one is searched
		if source, rec, ok := a.findSession(q.pattern, ""); ok {
			all = append(all, candidate{source: source, rec: rec})
		}
	} else {
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
		// The extra hit probeLimit asked for is the evidence, and it is dropped here so
		// no caller ever sees more than it asked for
		if len(matches) > q.perSession {
			matches = matches[:q.perSession]
			out.hitsCut = true
		}
		if len(out.results) >= q.limit {
			continue // keep counting matched so the caller learns how much was cut
		}
		item := all[i].rec.public()
		item["matches"] = matches
		item["matchCount"] = len(matches)
		out.results = append(out.results, item)
	}
	out.sessionsCut = out.matched > len(out.results)
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
			// The role filter lives here rather than after the fact: filtering a
			// per_session-sized slice would keep whichever hits came first in the file
			// and could miss the one being asked for entirely.
			if q.role == "" || strOr(hit["role"], "") == q.role {
				hits = append(hits, hit)
			}
		}
		return len(hits) < q.probeLimit()
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

// metadataFieldSkip names fields whose values look like text when you squint but never
// are: timestamps, ordinals, versions. A short or numeric needle ("502", "12") matches
// inside them constantly — measured locally, searching "502" surfaced snippets that were
// a timestamp's millisecond part (02:09:43.502Z) and a uuid's tail (...b7502fe).
//
// Anything id-shaped is handled by rule rather than by enumeration: a key that ends in
// "id" after folding (sessionId / session_id / turnId / root_turn_id / callID ...) names
// an identifier, never body text. The enumerated list covers the non-id shapes.
//
// Skipping these fields also means a match that landed nowhere else produces no hit at
// all, which is exactly the wanted outcome: the row matched, but it had nothing to say.
var metadataFieldSkip = map[string]bool{
	"timestamp": true, "time": true, "createdat": true, "updatedat": true,
	"starttime": true, "endtime": true, "lastupdated": true,
	"timecreated": true, "timeupdated": true,
	"ordinal": true, "seq": true, "version": true,
}

// isMetadataField folds camelCase and snake_case to the same form (sessionId and
// session_id both become "sessionid") before applying the rule and the list.
func isMetadataField(key string) bool {
	folded := strings.ReplaceAll(strings.ToLower(key), "_", "")
	return metadataFieldSkip[folded] || strings.HasSuffix(folded, "id")
}

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
			if isMetadataField(key) {
				continue
			}
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
			if isMetadataField(key) {
				continue // ids and timestamps are not body text
			}
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

// stripTerminalControls removes ANSI escape sequences and other control characters.
//
// A transcript records what a tool printed, and tool output is full of colour codes.
// Measured on this machine: 24 of 157 Claude sessions carry escape sequences, all of them
// on the rows that hold tool results, and searching a word inside that output returned 5
// snippets in 111 with a raw ESC still in them.
//
// Two things go wrong when that reaches a caller. The mild one is display: the snippet
// renders as mojibake anywhere that is not a terminal. The other is that a snippet is
// content the caller never chose to run. An OSC 52 in a tool log drives the clipboard of
// whoever prints it, and a cursor sequence moves their cursor.
//
// Only snippets are cleaned. A message body is returned exactly as stored, because that is
// the data and what to do with it is the caller's policy; a snippet is ours, cut for
// display, so it is ours to make safe.
func stripTerminalControls(s string) string {
	if strings.IndexFunc(s, isTerminalControl) < 0 {
		return s // the overwhelmingly common case, and it allocates nothing
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == 0x1b:
			i += escapeSequenceLen(s[i:])
		case c < utf8.RuneSelf && isTerminalControl(rune(c)):
			i++
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			out = append(out, s[i:i+size]...)
			i += size
		}
	}
	return string(out)
}

// isTerminalControl reports whether a rune is a control character worth removing.
// Tab and newline stay: they are layout inside the text, not instructions to a terminal.
// Carriage return goes, because on a terminal it rewrites the line already printed.
func isTerminalControl(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f
}

// escapeSequenceLen is the length of the escape sequence starting at s[0], which the
// caller has already established is ESC.
//
// An unterminated sequence consumes the rest of the string. That is deliberate: leaving
// the tail of a half-written CSI behind would put the bytes it is made of back into the
// output, which is the thing being prevented.
func escapeSequenceLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[': // CSI: parameters, intermediates, then one final byte in @ to ~
		for i := 2; i < len(s); i++ {
			if c := s[i]; c >= 0x40 && c <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case ']', 'P', 'X', '^', '_': // OSC and friends: run to BEL or to ST (ESC backslash)
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	default: // a two-byte escape
		return 2
	}
}

func snippetAround(text, needleLower string, radius int) string {
	// Cleaned before the window is cut, not after: an escape sequence counted toward the
	// radius would spend the snippet's budget on bytes nobody sees, and cutting inside one
	// would leave its tail behind.
	text = stripTerminalControls(text)
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
