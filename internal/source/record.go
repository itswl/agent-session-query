package source

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

// Record is the one session shape every source converges on.
//
// Typed fields rather than a field map: the sources disagree about everything — a jsonl
// file, an SQLite database, a sessions.json entry — and a mistyped key in a map read as
// an empty string (a session with no id, a search that finds nothing) with nothing on the
// line to say why. A struct makes that contract the compiler's business.
//
// The base fields are always in the public shape; file is null when the session has none.
// The optional ones are omitted rather than empty when the source does not record them:
// an absent key says that, "" does not (see public). messageCount rides with hasCount for
// the same reason — a source that does not count and a session with zero messages are
// different facts.
//
// Everything else is derived, internal, and never surfaces: sortAt / sortKey drive list
// ordering, lowerSID / lowerKey are the lowercased forms used for matching — all computed
// once when the record is built, so a lookup never has to ToLower every record again.
type Record struct {
	Source    string
	Key       string
	ShortKey  string
	SessionID string
	File      string // empty means none; the public shape renders that as null
	HasFile   bool
	Status    string
	UpdatedAt string

	CreatedAt   string
	Cwd         string
	Model       string
	Branch      string
	CliVersion  string
	Project     string // gemini's own grouping field, used when cwd is not there
	DisplayName string
	Platform    string

	TotalTokens      float64
	EstimatedCostUsd float64
	RuntimeMs        float64

	MessageCount int
	HasCount     bool
	// The numeric extras follow HasCount's rule: a source that recorded a zero keeps the
	// key, and only a source that does not report the figure omits it
	HasTotalTokens   bool
	HasEstimatedCost bool
	HasRuntime       bool
	Archived         bool
	Files            []string // the files one session spans when it spans several (gemini)

	sortAt   time.Time // parsed update time; zero means this record's time would not parse
	sortKey  string    // fallback when the time will not parse: the raw string
	lowerSID string
	lowerKey string
}

// NewRecord finalizes a record: the raw "update time" value is parsed, and the derived
// sort and match keys are computed once here.
//
// Sources spell time differently (2006-01-02T15:04:05 from mtime, RFC3339Nano from
// Gemini, whatever sessions.json happens to hold for Hermes, epoch numbers, ...). They
// are all parsed into a time.Time here before sorting: compare the strings
// lexicographically instead and a single offset-bearing timestamp misorders the whole
// cross-source list.
func NewRecord(r Record, updatedAt any) Record {
	r.sortAt, _ = parseTimestampValue(updatedAt)
	r.sortKey = ToStr(updatedAt)
	r.lowerSID = NormalizeForMatch(r.SessionID)
	r.lowerKey = NormalizeForMatch(r.Key)
	return r
}

// NormalizeForMatch folds a string to "lowercase + forward slashes" for matching.
//
// A file-backed source's key is a full path, and the separator follows the OS — on
// Windows that is `\`. Without folding, the same pattern drops from an exact suffix hit
// to a mere substring hit there, and a user typing the habitual `proj/abc.jsonl` never
// matches `...\proj\abc.jsonl` at all.
func NormalizeForMatch(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "\\", "/"))
}

// ActiveWindow bounds what IsActive reports: the session's newest message carries a
// timestamp inside this window.
//
// That is recency, not liveness. Nothing here checks whether a process exists: a session
// whose agent exited a minute ago still reports true until the window passes, and one
// that has been thinking or running a command for longer than the window reports false
// while very much alive.
//
// Reading real liveness was measured across all seven sources and is not available
// uniformly: Claude Code registers only its background sessions (~/.claude/jobs), Hermes
// has lease and heartbeat tables that sit empty, Codex takes a writer lock only for the
// instant of a write, and the rest expose nothing. The processes do not hold their own
// transcript open either — every CLI appends and closes — so there is no cross-source
// signal to read. A per-source answer would make one green dot mean "a process is
// running" for one source and "written recently" for the other six in the same list,
// which is worse than an approximation that is at least consistent. So the approximation
// stays, and the wording says what it measures.
const ActiveWindow = 2 * time.Minute

// public assembles the JSON shape: a fresh map per call, because a record is shared by
// the list cache and handing the caller anything the record still owns invites a write
// through it.
//
// The optional fields are omitted rather than empty when the source has nothing to say:
// an absent key says "this source does not record it", "" does not — a rule the branch
// and resumeCommand fields always followed, now applied to all of them.
//
// isActive is computed here rather than at build time because it depends on what time
// it is now. Freeze it at build time and a record scanned ten minutes ago keeps
// reporting recent activity.
func (r Record) Public() map[string]any {
	out := map[string]any{
		"source":    r.Source,
		"key":       r.Key,
		"shortKey":  r.ShortKey,
		"sessionId": r.SessionID,
		"file":      nil, // a string when the session has one; null says it is a database row
		"hasFile":   r.HasFile,
		"status":    r.Status,
		"updatedAt": r.UpdatedAt,
	}
	if r.File != "" {
		out["file"] = r.File
	}
	if r.CreatedAt != "" {
		out["createdAt"] = r.CreatedAt
	}
	if r.Cwd != "" {
		out["cwd"] = r.Cwd
	}
	if r.Model != "" {
		out["model"] = r.Model
	}
	if r.Branch != "" {
		out["branch"] = r.Branch
	}
	if r.CliVersion != "" {
		out["cliVersion"] = r.CliVersion
	}
	if r.DisplayName != "" {
		out["displayName"] = r.DisplayName
	}
	if r.Platform != "" {
		out["platform"] = r.Platform
	}
	if r.HasTotalTokens {
		out["totalTokens"] = r.TotalTokens
	}
	if r.HasEstimatedCost {
		out["estimatedCostUsd"] = r.EstimatedCostUsd
	}
	if r.HasRuntime {
		out["runtimeMs"] = r.RuntimeMs
	}
	if r.HasCount {
		out["messageCount"] = r.MessageCount
	}
	if r.Archived {
		out["archived"] = true
	}
	if len(r.Files) > 0 {
		out["files"] = append([]string(nil), r.Files...) // copied: the record's slice lives in the cache
	}
	out["isActive"] = r.IsActive()
	out["project"] = r.ProjectName()
	if resume := r.ResumeCommand(); resume != "" {
		// Omitted rather than empty for the sources that have none: a key that is
		// sometimes a command and sometimes "" reads as a command that failed to build
		out["resumeCommand"] = resume
	}
	return out
}

// IsActive reports whether the session's newest message falls inside ActiveWindow —
// recency, not liveness (see the ActiveWindow comment).
func (r Record) IsActive() bool {
	return !r.sortAt.IsZero() && time.Since(r.sortAt) < ActiveWindow
}

// SortAt is the parsed update time, zero when it would not parse. Ordering and the
// since/until bounds work off this rather than the string.
func (r Record) SortAt() time.Time { return r.sortAt }

// project is the session's owning project: cwd first (every source but Hermes's jsonl
// era carries one), then gemini's own project field.
func (r Record) ProjectName() string {
	if r.Cwd != "" {
		return r.Cwd
	}
	return r.Project
}

// resumeCommands is the command that reopens one of a source's sessions by id, verified
// against each CLI's own help on a machine that has all eight installed.
//
// Two sources are deliberately absent, for different reasons.
//
// Gemini CLI addresses sessions by position rather than identity: --resume takes "latest"
// or an index, --list-sessions numbers them, and --delete-session takes that same index.
// A session id cannot be turned into a command at all.
//
// OpenClaw can be resumed by session id, which its help does not say: the query is matched
// against a list built from derivedTitle, displayName, label, subject, sessionId and key,
// so a full uuid substring-matches exactly one session. What rules it out is the candidate
// set. That list is the last 50 sessions still inside the recent-activity window, fetched
// from a running Gateway, while this list covers every session on disk. The command would
// work for the newest handful and fail with "no recent session matched" for the rest, and
// a button that is present and sometimes wrong is worse than one that is absent.
//
// The flags differ more than they look: claude and hermes take --resume, codex and
// openclaw take resume as a subcommand, grok takes -r, and pi and opencode resume by id
// through --session while their own --resume is an interactive picker.
var resumeCommands = map[string]string{
	"claude":   "claude --resume",
	"codex":    "codex resume",
	"grok":     "grok -r",
	"hermes":   "hermes --resume",
	"opencode": "opencode --session",
	"pi":       "pi --session",
}

// ResumeCommand is how to reopen this session in the CLI that wrote it, or empty when
// that source has no by-id resume.
//
// It names the session only, with no cd in front. The session is found from anywhere:
// measured on Claude Code and Grok, both resolve a session id across working directories,
// and Grok even reports which directory the session came from. What the working directory
// decides is where the resumed agent then works, since it inherits the one it was launched
// in — so a caller that means to carry on with the same files wants the record's cwd,
// which the record already carries. The page prefixes the cd for that reason.
func (r Record) ResumeCommand() string {
	mode := r.Source
	if i := strings.IndexByte(mode, ':'); i >= 0 {
		mode = mode[:i] // a labeled instance (claude:box2) resumes with its base CLI
	}
	command, ok := resumeCommands[mode]
	if !ok {
		return ""
	}
	if r.SessionID == "" {
		return ""
	}
	return command + " " + ShellArg(r.SessionID)
}

// shellArg quotes an id that is not plainly safe to paste into a shell. Ids are normally
// uuids or filename stems and pass through untouched; the quoting is there so that an id
// carrying a space or a quote cannot turn a copied command into two.
func ShellArg(s string) string {
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/', r == '=', r == '@', r == '+':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe && s != "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// NewerThan drives list ordering: later update time comes first.
// Records whose time would not parse (zero sortAt) always sort after those that have
// one, and among themselves fall back to reverse string order.
func (r Record) NewerThan(other Record) bool {
	if !r.sortAt.IsZero() || !other.sortAt.IsZero() {
		if r.sortAt.Equal(other.sortAt) {
			return false // equal times keep source order (paired with sort.SliceStable)
		}
		return r.sortAt.After(other.sortAt)
	}
	return r.sortKey > other.sortKey
}

// MatchRank: see the free function of the same name. This uses the lowercased forms
// computed when the record was built.
func (r Record) MatchRank(patternLower string) int {
	return MatchRank(patternLower, r.lowerSID, r.lowerKey)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// maxLineBytes caps a single line (Claude Code tool output can get large). The buffer
// grows on demand, so a normal file never needs more than the initial 64 KB. If a line
// really does exceed the cap, Scanner aborts — note that means "nothing after this line
// is read", not "this line is skipped", which is why the error below must be surfaced.
const maxLineBytes = 256 * 1024 * 1024

// EachJSONLLine walks raw bytes line by line. The slice handed to the callback is only
// valid for that call (the buffer is reused); copy it to keep it. Scanner rather than
// ReadBytes: Scanner.Bytes() is a view into the internal buffer, so no allocation per
// line — measured ~40% faster than line-wise ReadBytes over a 3000-line session.
func EachJSONLLine(path string, fn func(line []byte) bool) {
	eachJSONLLineFrom(path, 0, fn)
}

// eachJSONLLineFrom is EachJSONLLine starting at a byte offset. The offset must sit on a
// line boundary — the caller (the count cache) checks that the byte before it is a
// newline — and it exists so a counter resuming after an append reads only the appended
// tail instead of the whole file again.
func eachJSONLLineFrom(path string, from int64, fn func(line []byte) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return
		}
	}

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
	eachJSONLFrom(path, 0, fn)
}

// eachJSONLFrom is eachJSONL starting at a byte offset (see eachJSONLLineFrom)
func eachJSONLFrom(path string, from int64, fn func(obj map[string]any) bool) {
	eachJSONLLineFrom(path, from, func(line []byte) bool {
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

// ContentText flattens the several shapes of content into plain text
// (string / array / single object).
func ContentText(value any) string {
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
				return ContentText(t)
			}
		}
		return ""
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := ContentText(item); text != "" {
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
// top of the list and has isActive report recent activity on it.
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
	if _, ok := ParseTimestamp(ts); !ok {
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

// MatchRank scores how well a pattern matches a session: 0 is the most exact, -1 means
// no match. pattern / sid / key must already have gone through NormalizeForMatch
// (lowercase + forward slashes).
func MatchRank(patternLower, sid, key string) int {
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

// Truncate cuts by character (Unicode code point), appending a marker when it cuts.
func Truncate(text string, limit int, mark string) string {
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

// Truthy: nil, false, 0, the empty string and empty containers are falsy.
func Truthy(v any) bool {
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

// ToStr converts to string: strings pass through, bools render as True/False, numbers
// avoid exponent notation.
func ToStr(v any) string {
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

func ToFloat(v any) (float64, bool) {
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

// StrOr takes the string value, falling back to def when empty.
func StrOr(v any, def string) string {
	if Truthy(v) {
		return ToStr(v)
	}
	return def
}

// floatOrZero reads a numeric field out of a json blob, folding anything else (a string,
// a null, a nested object) to zero
func floatOrZero(v any) float64 {
	n, _ := ToFloat(v)
	return n
}

// GetOr uses the stored value when the key exists (even if null), otherwise def.
func GetOr(m map[string]any, key string, def any) any {
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
		return ParseTimestamp(t)
	case float64, int, int64:
		sec, _ := ToFloat(v)
		return epochToTime(sec)
	}
	return time.Time{}, false
}

func ParseTimestamp(s string) (time.Time, bool) {
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
