package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// record is the one session shape every source converges on.
//
// fields holds the public fields (source / key / sessionId / file / hasFile / status /
// updatedAt, plus whatever each source adds). Everything else is derived, internal, and
// never surfaces: sortAt / sortKey drive list ordering, lowerSID / lowerKey are the
// lowercased forms used for matching — all computed once when the record is built, so a
// lookup never has to ToLower every record again.
type record struct {
	fields map[string]any

	sortAt   time.Time // parsed update time; zero means this record's time would not parse
	sortKey  string    // fallback when the time will not parse: the raw string
	lowerSID string
	lowerKey string
}

// newRecord builds a record from its field map and the raw "update time" value.
//
// Sources spell time differently (2006-01-02T15:04:05 from mtime, RFC3339Nano from
// Gemini, whatever sessions.json happens to hold for Hermes, epoch numbers, ...). They
// are all parsed into a time.Time here before sorting: compare the strings
// lexicographically instead and a single offset-bearing timestamp misorders the whole
// cross-source list.
func newRecord(fields map[string]any, updatedAt any) record {
	at, _ := parseTimestampValue(updatedAt)
	return record{
		fields:   fields,
		sortAt:   at,
		sortKey:  toStr(updatedAt),
		lowerSID: normalizeForMatch(toStr(fields["sessionId"])),
		lowerKey: normalizeForMatch(toStr(fields["key"])),
	}
}

// normalizeForMatch folds a string to "lowercase + forward slashes" for matching.
//
// A file-backed source's key is a full path, and the separator follows the OS — on
// Windows that is `\`. Without folding, the same pattern drops from an exact suffix hit
// to a mere substring hit there, and a user typing the habitual `proj/abc.jsonl` never
// matches `...\proj\abc.jsonl` at all.
func normalizeForMatch(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "\\", "/"))
}

func (r record) get(k string) any     { return r.fields[k] }
func (r record) str(k string) string  { return toStr(r.fields[k]) }
func (r record) truthy(k string) bool { return truthy(r.fields[k]) }

// activeWindow: an update time inside this window counts as "currently running"
const activeWindow = 2 * time.Minute

// public returns a copy of the public fields. Records are held by the list cache and
// shared across goroutines; hand the internal map out directly and one careless
// assignment by a caller poisons every later reader.
//
// isActive is computed here rather than at build time because it depends on what time
// it is now. Freeze it at build time and a record scanned ten minutes ago keeps
// claiming to be live.
func (r record) public() map[string]any {
	out := make(map[string]any, len(r.fields)+2)
	for k, v := range r.fields {
		out[k] = v
	}
	out["isActive"] = !r.sortAt.IsZero() && time.Since(r.sortAt) < activeWindow
	out["project"] = r.project()
	return out
}

// project is the session's owning project: cwd first (every source but Hermes's jsonl
// era carries one), then gemini's own project field.
func (r record) project() string {
	if cwd := r.str("cwd"); cwd != "" {
		return cwd
	}
	return r.str("project")
}

// newerThan drives list ordering: later update time comes first.
// Records whose time would not parse (zero sortAt) always sort after those that have
// one, and among themselves fall back to reverse string order.
func (r record) newerThan(other record) bool {
	if !r.sortAt.IsZero() || !other.sortAt.IsZero() {
		if r.sortAt.Equal(other.sortAt) {
			return false // equal times keep source order (paired with sort.SliceStable)
		}
		return r.sortAt.After(other.sortAt)
	}
	return r.sortKey > other.sortKey
}

// matchRank: see the free function of the same name. This uses the lowercased forms
// computed when the record was built.
func (r record) matchRank(patternLower string) int {
	return matchRank(patternLower, r.lowerSID, r.lowerKey)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// maxLineBytes caps a single line (Claude Code tool output can get large). The buffer
// grows on demand, so a normal file never needs more than the initial 64 KB. If a line
// really does exceed the cap, Scanner aborts — note that means "nothing after this line
// is read", not "this line is skipped", which is why the error below must be surfaced.
const maxLineBytes = 256 * 1024 * 1024

// eachJSONLLine walks raw bytes line by line. The slice handed to the callback is only
// valid for that call (the buffer is reused); copy it to keep it. Scanner rather than
// ReadBytes: Scanner.Bytes() is a view into the internal buffer, so no allocation per
// line — measured ~40% faster than line-wise ReadBytes over a 3000-line session.
func eachJSONLLine(path string, fn func(line []byte) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if !fn(line) {
			return
		}
	}
	// An over-long line or a read failure makes the rest of the file unreachable;
	// truncating silently is far harder to diagnose than saying so
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] reading %s stopped; the rest of the file was not parsed: %v\n", path, err)
	}
}

// eachJSONL decodes one object per line (bad lines and non-objects are skipped).
// Returning false from fn stops early.
func eachJSONL(path string, fn func(obj map[string]any) bool) {
	eachJSONLLine(path, func(line []byte) bool {
		var obj map[string]any
		if json.Unmarshal(line, &obj) == nil && obj != nil {
			return fn(obj)
		}
		return true
	})
}

// readJSONL reads a jsonl file, taking at most limit valid records (limit <= 0 = all).
func readJSONL(path string, limit int) []map[string]any {
	out := []map[string]any{}
	eachJSONL(path, func(obj map[string]any) bool {
		if limit > 0 && len(out) >= limit {
			return false
		}
		out = append(out, obj)
		return true
	})
	return out
}

// contentText flattens the several shapes of content into plain text
// (string / array / single object).
func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		for _, key := range []string{"text", "content", "thinking"} {
			inner, ok := v[key]
			if !ok {
				continue
			}
			switch t := inner.(type) {
			case string:
				return t
			case []any:
				return contentText(t)
			}
		}
		return ""
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := contentText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// tailWindows are the window sizes tried in turn when looking backwards from the end of
// a file for its last record. Measured over 174 real Claude sessions, 172 of them yield
// a complete record within the final 64 KB.
var tailWindows = []int64{64 << 10, 512 << 10, 4 << 20}

// lastRecordTime reads the tail of a session file and returns the time carried by its
// last record.
//
// Why not just use the file's mtime: mtime is when the file was written, not when the
// conversation happened. Measured over 174 real Claude sessions, 43 of them (25%) differ
// by more than an hour, the worst by 235 hours — something rewrites session files
// without appending anything, which floats a conversation that ended six days ago to the
// top of the list and has isActive report it as "currently being written".
//
// It does not read the whole file: seeking a small window from the end is enough (the
// largest session here is 103 MB). If the window holds no complete record the next size
// up is tried; if none do, it returns "" and the caller falls back to mtime.
func lastRecordTime(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	if size == 0 {
		return ""
	}

	for _, window := range tailWindows {
		if window > size {
			window = size
		}
		start := size - window
		buf := make([]byte, window)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return ""
		}
		// When the window does not start at the beginning of the file, its first line
		// is probably cut in half — drop it
		if start > 0 {
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			} else {
				buf = nil
			}
		}
		if ts := lastTimestampIn(buf); ts != "" {
			return ts
		}
		if window >= size {
			break // already read the whole file; no point growing further
		}
	}
	return ""
}

// lastTimestampIn scans a byte range backwards for the first record carrying a time.
// All three placements count: a top-level timestamp (Claude / Codex / Pi),
// message.timestamp (some Pi lines), and $set.lastUpdated (Gemini's patch lines).
func lastTimestampIn(buf []byte) string {
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Timestamp string `json:"timestamp"`
			Message   struct {
				Timestamp string `json:"timestamp"`
			} `json:"message"`
			Set struct {
				LastUpdated string `json:"lastUpdated"`
			} `json:"$set"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		for _, ts := range []string{probe.Timestamp, probe.Message.Timestamp, probe.Set.LastUpdated} {
			if ts != "" {
				return ts
			}
		}
	}
	return ""
}

// updatedAtOf is a session's last-activity time: the time on the last record in the
// file if there is one, otherwise the file's mtime. A value that will not parse also
// falls back — an unparseable time sorts to the very end of the list, which is worse
// than using mtime.
func updatedAtOf(path, modISO string) string {
	ts := lastRecordTime(path)
	if ts == "" {
		return modISO
	}
	if _, ok := parseTimestamp(ts); !ok {
		return modISO
	}
	return ts
}

// mtimeISO is a file's modification time (UTC, second precision, e.g. 2026-09-14T07:41:48).
func mtimeISO(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return st.ModTime().UTC().Format("2006-01-02T15:04:05")
}

// matchRank scores how well a pattern matches a session: 0 is the most exact, -1 means
// no match. pattern / sid / key must already have gone through normalizeForMatch
// (lowercase + forward slashes).
func matchRank(patternLower, sid, key string) int {
	if sid != "" && patternLower == sid {
		return 0
	}
	if key == patternLower {
		return 1
	}
	if strings.HasSuffix(key, ":"+patternLower) || strings.HasSuffix(key, "/"+patternLower) {
		return 2
	}
	if strings.Contains(key, patternLower) {
		return 3
	}
	if sid != "" && strings.Contains(sid, patternLower) {
		return 4
	}
	return -1
}

// truncate cuts by character (Unicode code point), appending a marker when it cuts.
func truncate(text string, limit int, mark string) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	count := 0
	for i := range text {
		if count == limit {
			return text[:i] + mark
		}
		count++
	}
	return text + mark
}

// truthy: nil, false, 0, the empty string and empty containers are falsy.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// toStr converts to string: strings pass through, bools render as True/False, numbers
// avoid exponent notation.
func toStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// strOr takes the string value, falling back to def when empty.
func strOr(v any, def string) string {
	if truthy(v) {
		return toStr(v)
	}
	return def
}

// getOr uses the stored value when the key exists (even if null), otherwise def.
func getOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// getMap reads a map field (missing or wrongly typed yields an empty map).
func getMap(m map[string]any, key string) map[string]any {
	if inner, ok := m[key].(map[string]any); ok && inner != nil {
		return inner
	}
	return map[string]any{}
}

// getSlice reads an array field (missing or wrongly typed yields nil).
func getSlice(m map[string]any, key string) []any {
	if inner, ok := m[key].([]any); ok {
		return inner
	}
	return nil
}

// strField reads a string field (anything else yields "").
func strField(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// timeLayouts are the time shapes seen across sources, ordered by how often they turn
// up (parsing tries each in turn). Layouts without a zone parse as UTC — every source
// writes UTC.
var timeLayouts = []string{
	time.RFC3339Nano,                // 2026-09-14T03:16:50.601Z, and +08:00 offsets too
	"2006-01-02T15:04:05",           // the shape derived from mtime
	"2006-01-02 15:04:05.999999999", // space separated
	"2006-01-02 15:04:05",
}

// parseTimestampValue parses a raw "update time" value into a UTC time.
// It accepts strings (the layouts above, plus a bare numeric epoch) and numbers
// (epoch seconds or milliseconds).
func parseTimestampValue(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		return parseTimestamp(t)
	case float64, int, int64:
		sec, _ := toFloat(v)
		return epochToTime(sec)
	}
	return time.Time{}, false
}

func parseTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range timeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			return parsed.UTC(), true
		}
	}
	// A bare numeric epoch (some writers put the timestamp into JSON as a string)
	if sec, err := strconv.ParseFloat(s, 64); err == nil {
		return epochToTime(sec)
	}
	return time.Time{}, false
}

// epochToTime converts epoch seconds or milliseconds to UTC. Anything past 1e11 is
// treated as milliseconds (1e11 seconds is the year 5138, 1e11 milliseconds is 1973 —
// the boundary cannot be mistaken).
func epochToTime(v float64) (time.Time, bool) {
	if v == 0 {
		return time.Time{}, false
	}
	if v > 1e11 || v < -1e11 {
		v /= 1000
	}
	if v < -62135596800 || v > 253402300799 {
		return time.Time{}, false
	}
	sec := int64(v)
	nsec := int64((v - float64(sec)) * float64(time.Second))
	return time.Unix(sec, nsec).UTC(), true
}

// utcFromSeconds formats an epoch-seconds timestamp into the two UTC shapes;
// out-of-range input returns ok=false.
func utcFromSeconds(sec float64) (dashed string, iso string, ok bool) {
	whole := int64(sec)
	if sec < 0 && float64(whole) != sec {
		whole-- // match time.Unix rounding direction
	}
	if whole < -62135596800 || whole > 253402300799 {
		return "", "", false
	}
	t := time.Unix(whole, 0).UTC()
	return t.Format("2006-01-02 15:04:05"), t.Format("2006-01-02T15:04:05"), true
}
