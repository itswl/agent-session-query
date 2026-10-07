package source

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// The per-file content scan: how a source without its own search implementation answers a
// SearchQuery, and the primitives the SQLite ones reuse (snippet building, hit roles).

const (
	maxSearchDepth   = 8   // recursion depth cap when hunting for body text in JSON
	CancelCheckLines = 512 // scan this many lines between cancellation checks
)

// cancelled reports whether the caller has walked away. A search holds every core it
// can get, so an abandoned one has to stop rather than run to completion: the page
// fires a fresh search on every keystroke and only the last one is ever displayed.
func Cancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// searchFile scans one jsonl session file.
func SearchFile(ctx context.Context, path string, q SearchQuery) []map[string]any {
	if path == "" || len(q.Lowered) == 0 {
		return nil
	}
	var hits []map[string]any
	var lower []byte // reused so each line costs no allocation
	lines := 0
	EachJSONLLine(path, func(line []byte) bool {
		// Sampled rather than checked every line: a channel receive per line would cost
		// more than the Contains that is the actual work here
		lines++
		if lines%CancelCheckLines == 0 && Cancelled(ctx) {
			return false
		}
		lower = AppendLowerASCII(lower[:0], line)
		if !bytes.Contains(lower, q.Lowered) {
			return true
		}
		if hit := buildHit(line, q); hit != nil {
			// The role filter lives here rather than after the fact: filtering a
			// per_session-sized slice would keep whichever hits came first in the file
			// and could miss the one being asked for entirely.
			if q.Role == "" || StrOr(hit["role"], "") == q.Role {
				hits = append(hits, hit)
			}
		}
		return len(hits) < q.ProbeLimit()
	})
	return hits
}

// buildHit turns a matching line into a result; it returns nil when the match landed
// only in a field name or an escape sequence.
func buildHit(line []byte, q SearchQuery) map[string]any {
	var obj map[string]any
	if json.Unmarshal(line, &obj) != nil || obj == nil {
		return nil
	}
	text, ok := findMatchingText(obj, string(q.Lowered), 0)
	if !ok {
		return nil
	}
	return map[string]any{
		"snippet":   SnippetAround(text, string(q.Lowered), SearchSnippetRadius),
		"role":      hitRole(obj),
		"timestamp": hitTimestamp(obj["timestamp"]),
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
		// Cleaned before the test, so a needle that exists only inside an escape sequence
		// is not a match — it is not in the text a reader would see. Searching "38;2"
		// otherwise hit every line coloured with a 24-bit sequence and returned a snippet
		// with no occurrence of the needle in it, inflating matched and matchCount.
		// Returning the cleaned string also means snippetAround has nothing left to strip.
		if clean := StripTerminalControls(t); IndexFold(clean, needleLower) >= 0 {
			return clean, true
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
	if role := StrOr(obj["role"], ""); role != "" {
		return role
	}
	for _, key := range []string{"message", "payload"} {
		if inner, ok := obj[key].(map[string]any); ok {
			if role := StrOr(inner["role"], ""); role != "" {
				return role
			}
		}
	}
	if role := GrokHitRole(obj); role != "" {
		return role
	}
	if role := CodexHitRole(obj); role != "" {
		return role
	}
	return StrOr(obj["type"], "")
}

// hitTimestamp renders a hit's time the way a message spells one. Most sources write an
// ISO string and it passes straight through; Grok writes epoch seconds, and a bare integer
// in the field every other source fills with text is not something a caller should have to
// special-case.
func hitTimestamp(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if iso := GrokTimestamp(v); iso != "" {
		return iso
	}
	return ToStr(v)
}

// appendLowerASCII appends src to dst with A-Z folded to lowercase.
// It works byte by byte, so UTF-8 multi-byte sequences (lead byte >= 0x80) pass through
// untouched — scripts that have no case are unaffected.
func AppendLowerASCII(dst, src []byte) []byte {
	for _, c := range src {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}
