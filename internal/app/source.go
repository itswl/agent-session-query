package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SessionSource is the data source adapter: one shape for list / messages / final.
type SessionSource interface {
	Mode() string
	Location() string // printed at startup and when a source is missing
	Exists() bool
	List() []record
	// Messages returns a session's messages; messageQuery picks which slice
	Messages(r record, q messageQuery) []map[string]any
	// Final returns a session's final result; nil means the session file does not
	// exist yet, and the layer above turns that into an error response
	Final(r record) map[string]any
}

// listErrorReporter lets a source say that its last List() failed rather than came back
// empty.
//
// That distinction is the whole point of it: a source with nothing to show and a source
// whose data could not be read at all both answer with zero records, so without this a
// schema that moved under us reads as "you have no sessions". Only the sources that can
// fail this way implement it, and the API reports what they last hit (see listWarnings).
type listErrorReporter interface {
	ListError() error
}

// messageQuery describes one message query: how many, and from which end.
//
// fromEnd corresponds to ?order=desc. The interesting part of a session is usually its
// end, and with only "the earliest N" available, a session with tens of thousands of
// messages would forever show nothing but its opening.
type messageQuery struct {
	limit   int
	fromEnd bool
	// at positions the window at a point in time instead of at one end of the session:
	// ascending, the first limit messages at or after it; descending, the last limit
	// messages at or before it. It is how a search hit becomes a place you can land on —
	// a match in the middle of a 16 000-message session is in neither end's window.
	// Zero means "no anchoring", the usual case.
	at time.Time
	// offset pages from an end without relying on timestamps. For a descending
	// query it skips this many messages from the newest end; for ascending it
	// skips this many from the oldest end. It is deliberately independent of at:
	// timestamps are not unique enough to be a reliable paging cursor.
	offset int
	// full asks for tool output and thinking whole rather than cut to a preview (see
	// blocks.go). The ordinary read cuts them, because a window of two hundred messages
	// carrying every build log in full is megabytes; the export and a caller that wants
	// the end of the output the preview dropped ask for this.
	full bool
	// role keeps only messages with this role ("user" / "assistant"). The filter runs
	// inside the sink, before the window and the offset are counted, so limit, offset and
	// the page a cursor lands on all speak of matching messages rather than of whichever
	// rows happened to come first. Empty is the ordinary case: no filtering.
	role string
}

// messageSink collects messages according to a messageQuery.
//
// Earliest N: stop as soon as there are enough (add returns false) and end the scan early.
// Latest N: the scan has to run to the end of the file, so a ring buffer holds the
// requested page plus any descending offset — exactly limit+offset messages, never a
// smaller cap. The offset is bounded by the HTTP layer; it is the price of a stable
// cursor for sources whose timestamps are not unique.
type messageSink struct {
	q        messageQuery
	items    []map[string]any
	start    int // ring buffer write position (only used when fromEnd)
	skipped  int // messages skipped from the oldest end (ascending offset)
	capacity int // ring capacity, including a descending offset
}

func newMessageSink(q messageQuery) *messageSink {
	if q.limit < 0 {
		q.limit = 0
	}
	capacity := q.limit
	if q.fromEnd && q.at.IsZero() {
		capacity += q.offset
	}
	// No separate cap here. The ring has to hold exactly limit+offset: result() slices
	// the ring by the offset, and a clamp below that cut the page short — or emptied it —
	// with no error and no flag, at request sizes the HTTP layer explicitly accepts
	// (offset up to maxMessageOffset, limit up to maxLimit). Both are clamped where they
	// enter, so there is nothing to defend against here. The allocation is a slice of map
	// pointers: even the deepest page the API accepts is a few hundred kilobytes.
	return &messageSink{q: q, items: make([]map[string]any, 0, capacity), capacity: capacity}
}

// add takes one more message; false means there are enough and the caller may stop.
func (s *messageSink) add(m map[string]any) bool {
	if s.q.limit == 0 {
		return false
	}
	// A role filter is a filter, not a stop: the scan keeps going so the offset and the
	// "another page follows" probe below count matching messages only.
	if s.q.role != "" && toStr(m["role"]) != s.q.role {
		return true
	}
	if !s.q.at.IsZero() {
		// A message with no readable timestamp is kept rather than dropped: silently
		// losing rows is worse than one extra row in an anchored window.
		if ts, ok := parseTimestamp(toStr(m["timestamp"])); ok {
			if s.q.fromEnd && ts.After(s.q.at) {
				return true // newer than the anchor; the window ends before it
			}
			if !s.q.fromEnd && ts.Before(s.q.at) {
				return true // older than the anchor; the window starts at it
			}
		}
	}
	if !s.q.fromEnd {
		if s.q.at.IsZero() && s.skipped < s.q.offset {
			s.skipped++
			return true
		}
		if len(s.items) >= s.q.limit {
			return false
		}
		s.items = append(s.items, m)
		return len(s.items) < s.q.limit
	}
	if len(s.items) < s.capacity {
		s.items = append(s.items, m)
		return true
	}
	s.items[s.start] = m
	s.start = (s.start + 1) % s.capacity
	return true
}

// result returns the collected messages in chronological order. For a descending
// offset, the ring contains [older page ... newer offset]; return only the older page.
func (s *messageSink) result() []map[string]any {
	if len(s.items) == 0 {
		return []map[string]any{}
	}
	ordered := s.items
	if s.q.fromEnd && s.start != 0 {
		ordered = make([]map[string]any, 0, len(s.items))
		ordered = append(ordered, s.items[s.start:]...)
		ordered = append(ordered, s.items[:s.start]...)
	}
	if s.q.fromEnd && s.q.at.IsZero() && s.q.offset > 0 {
		end := len(ordered) - s.q.offset
		if end <= 0 {
			return []map[string]any{}
		}
		start := end - s.q.limit
		if start < 0 {
			start = 0
		}
		return ordered[start:end]
	}
	return ordered
}

// Supported sources (the values --mode accepts); auto enables whichever exist
var knownModes = []string{"hermes", "openclaw", "pi", "claude", "codex", "gemini", "opencode", "grok"}

// fileExists reports whether a path exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// defaultHome resolves ~ by reading the user's home directory
func defaultHome() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return "/root"
}

// sourcePath is one --path value: a file-backed source whose directory moved, with an
// optional instance label.
type sourcePath struct {
	mode  string
	label string // empty: relocate the source; set: add a labeled instance beside it
	dir   string
}

// pathFlag collects repeatable --path flags.
type pathFlag []sourcePath

func (p *pathFlag) String() string {
	parts := make([]string, 0, len(*p))
	for _, sp := range *p {
		if sp.label == "" {
			parts = append(parts, sp.mode+"="+sp.dir)
		} else {
			parts = append(parts, sp.mode+":"+sp.label+"="+sp.dir)
		}
	}
	return strings.Join(parts, " ")
}

func (p *pathFlag) Set(value string) error {
	spec := value
	dir := ""
	if i := strings.IndexByte(spec, '='); i >= 0 {
		dir, spec = spec[i+1:], spec[:i]
	} else {
		return fmt.Errorf("--path %q: want mode[:label]=directory", value)
	}
	mode, label := spec, ""
	hasColon := false
	if i := strings.IndexByte(spec, ':'); i >= 0 {
		mode, label, hasColon = spec[:i], spec[i+1:], true
	}
	if dir == "" {
		return fmt.Errorf("--path %q: the directory is empty", value)
	}
	// A colon promises a label, so an empty one is a typo; no colon at all is the plain
	// relocation form and carries no label to validate
	if hasColon && !validSourceLabel(label) {
		return fmt.Errorf("--path %q: the label may be lowercase letters, digits and dashes only", value)
	}
	if !validMode(mode) || mode == "auto" || mode == "all" {
		return fmt.Errorf("--path %q: unknown source %q (choose from: %s)", value, mode, joinModes())
	}
	// The directory-shaped sources can point anywhere; the json-map and SQLite ones keep
	// their layout across several files, which one directory cannot stand in for
	if _, ok := map[string]bool{"pi": true, "claude": true, "codex": true, "gemini": true, "grok": true}[mode]; !ok {
		return fmt.Errorf("--path: %s cannot be relocated this way (supported: pi, claude, codex, gemini, grok)", mode)
	}
	*p = append(*p, sourcePath{mode: mode, label: label, dir: dir})
	return nil
}

// validSourceLabel: what may follow the colon in --path mode:label
func validSourceLabel(label string) bool {
	if label == "" {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// labeledSource renames one instance of a file-backed source so several of the same CLI
// can be told apart: Mode() returns "claude:probe-watch", the records it lists carry that
// name in their source field, and the label is prefixed onto the project and cwd so
// grouping and the page's filters do not melt the instances into one. Everything else —
// messages, the final result, searching — is the wrapped source's own.
type labeledSource struct {
	SessionSource
	mode string
}

func (s labeledSource) Mode() string { return s.mode }

func (s labeledSource) List() []record {
	recs := s.SessionSource.List()
	label := s.mode[strings.IndexByte(s.mode, ':')+1:] + ":"
	for i := range recs {
		// List returns record values in a fresh slice, so writing to recs[i] cannot reach
		// what the wrapped source's cache hands out next time
		recs[i].Source = s.mode
		if recs[i].Cwd != "" {
			recs[i].Cwd = label + recs[i].Cwd
		}
		if recs[i].Project != "" {
			recs[i].Project = label + recs[i].Project
		}
	}
	return recs
}

func (s labeledSource) Final(r record) map[string]any {
	out := s.SessionSource.Final(r)
	if out != nil {
		out["source"] = s.mode
	}
	return out
}

// buildSources wires up data sources according to the run mode and any --path overrides.
//
//   - a single named mode: enable only that one
//   - all: enable everything (missing ones warn)
//   - auto: enable whichever exist (the two json-map sources look for sessions.json,
//     the rest for their directory)
//
// A --path without a label relocates that source: the default instance scans the given
// directory instead of the one under home, keeping the usual exists-or-skip behaviour.
// A --path with a label — claude:probe-watch=… — adds an instance beside whatever else is
// enabled, in every mode: asking for it by name is the explicit act, so it is enabled
// even when the directory does not exist (with a warning) and even when --mode names
// another source.
func buildSources(mode string, paths []sourcePath) ([]SessionSource, error) {
	home := defaultHome()

	// The five file-backed sources scan one directory, so that directory can move; the
	// json-map and SQLite ones keep their layout across several files and stay at home
	movable := map[string]func(dir string) SessionSource{
		"pi":     func(dir string) SessionSource { return newPiSource(dir) },
		"claude": func(dir string) SessionSource { return newClaudeSource(dir) },
		"codex":  func(dir string) SessionSource { return newCodexSource(dir) },
		"gemini": func(dir string) SessionSource { return newGeminiSource(dir) },
		"grok":   func(dir string) SessionSource { return newGrokSource(dir) },
	}
	factories := map[string]func() SessionSource{
		"hermes": func() SessionSource { return newJsonMapSource(hermesDef(home)) },
		"openclaw": func() SessionSource {
			// 2026.9 OpenClaw is SQLite; the pre-SQLite sessions.json layout stays as the
			// fallback for older installs
			if dbs := openClawAgentDBs(home); len(dbs) > 0 {
				return newOpenClawSource(dbs)
			}
			return newJsonMapSource(openclawDef(home))
		},
		"opencode": func() SessionSource { return newOpenCodeSource(filepath.Join(openCodeDataDir(home), "opencode.db")) },
		"pi":       func() SessionSource { return newPiSource(filepath.Join(home, ".pi", "agent", "sessions")) },
		"claude":   func() SessionSource { return newClaudeSource(filepath.Join(home, ".claude", "projects")) },
		"codex":    func() SessionSource { return newCodexSource(filepath.Join(home, ".codex", "sessions")) },
		"gemini":   func() SessionSource { return newGeminiSource(filepath.Join(home, ".gemini", "tmp")) },
		"grok":     func() SessionSource { return newGrokSource(filepath.Join(home, ".grok", "sessions")) },
	}

	replacements := map[string]string{}
	extra := make([]SessionSource, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		if p.label == "" {
			replacements[p.mode] = p.dir
			continue
		}
		if seen[p.mode+":"+p.label] {
			return nil, fmt.Errorf("--path: %s:%s given twice", p.mode, p.label)
		}
		seen[p.mode+":"+p.label] = true
		source := movable[p.mode](p.dir)
		if !source.Exists() {
			fmt.Fprintf(os.Stderr, "[WARN] --path directory not found, enabled anyway: %s:%s (%s)\n", p.mode, p.label, p.dir)
		}
		extra = append(extra, labeledSource{SessionSource: source, mode: p.mode + ":" + p.label})
	}
	// A source's directory, relocated when --path said so
	at := func(name string) SessionSource {
		if repl, moved := replacements[name]; moved {
			return movable[name](repl)
		}
		return factories[name]()
	}

	if mode != "auto" && mode != "all" {
		if _, ok := factories[mode]; !ok {
			return nil, fmt.Errorf("unknown mode %q (choose from: auto, all, %s)", mode, joinModes())
		}
		return append([]SessionSource{at(mode)}, extra...), nil
	}

	enabled := []SessionSource{}
	for _, name := range knownModes {
		// knownModes and factories are two lists of the same thing; a mode in one and not
		// the other is a wiring bug, and it surfaces as an error rather than a nil call
		if factories[name] == nil {
			return nil, fmt.Errorf("knownModes names %q but no factory exists for it", name)
		}
		source := at(name)
		if source.Exists() {
			enabled = append(enabled, source)
		} else if mode == "all" {
			fmt.Fprintf(os.Stderr, "[WARN] data source not found, skipped: %s (%s)\n", name, source.Location())
		}
	}
	enabled = append(enabled, extra...)
	if len(enabled) > 0 {
		return enabled, nil
	}

	fmt.Fprintln(os.Stderr, "[WARN] no data source detected; defaulting to OpenClaw")
	return []SessionSource{factories["openclaw"]()}, nil
}

func joinModes() string {
	out := ""
	for i, m := range knownModes {
		if i > 0 {
			out += "/"
		}
		out += m
	}
	return out
}

// jsonMapDef describes a source shaped as "one sessions.json index plus one jsonl per
// session". When stateDB is non-empty (hermes only), sessions can still be listed from
// SQLite even without sessions.json.
type jsonMapDef struct {
	mode            string
	sessionsJSON    string
	sessionsDir     string
	sessionIDField  string
	stopReasonField string
	stateDB         string
}

func hermesDef(home string) jsonMapDef {
	return jsonMapDef{
		mode:            "hermes",
		sessionsJSON:    filepath.Join(home, ".hermes", "sessions", "sessions.json"),
		sessionsDir:     filepath.Join(home, ".hermes", "sessions"),
		sessionIDField:  "session_id",
		stopReasonField: "finish_reason",
		stateDB:         filepath.Join(home, ".hermes", "state.db"),
	}
}

func openclawDef(home string) jsonMapDef {
	return jsonMapDef{
		mode:            "openclaw",
		sessionsJSON:    filepath.Join(home, ".openclaw", "agents", "default", "sessions", "sessions.json"),
		sessionsDir:     filepath.Join(home, ".openclaw", "agents", "default", "sessions"),
		sessionIDField:  "sessionId",
		stopReasonField: "stopReason",
	}
}
