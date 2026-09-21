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
}

// messageSink collects messages according to a messageQuery.
//
// Earliest N: stop as soon as there are enough (add returns false) and end the scan early.
// Latest N: the scan has to run to the end of the file, so a ring buffer of capacity
// limit holds the tail — memory tracks limit, not session length (a 16444-message
// session still keeps only the last N).
type messageSink struct {
	q     messageQuery
	items []map[string]any
	start int // ring buffer write position (only used when fromEnd)
}

func newMessageSink(q messageQuery) *messageSink {
	if q.limit < 0 {
		q.limit = 0
	}
	capacity := q.limit
	if capacity > 512 {
		capacity = 512 // do not reserve a whole block up front for a limit=1000 request
	}
	return &messageSink{q: q, items: make([]map[string]any, 0, capacity)}
}

// add takes one more message; false means there are enough and the caller may stop.
func (s *messageSink) add(m map[string]any) bool {
	if s.q.limit == 0 {
		return false
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
		s.items = append(s.items, m)
		return len(s.items) < s.q.limit
	}
	if len(s.items) < s.q.limit {
		s.items = append(s.items, m)
		return true
	}
	s.items[s.start] = m
	s.start = (s.start + 1) % s.q.limit
	return true
}

// result returns the collected messages in chronological order
func (s *messageSink) result() []map[string]any {
	if len(s.items) == 0 {
		return []map[string]any{}
	}
	if !s.q.fromEnd || s.start == 0 {
		return s.items
	}
	out := make([]map[string]any, 0, len(s.items))
	out = append(out, s.items[s.start:]...)
	out = append(out, s.items[:s.start]...)
	return out
}

// Supported sources (the values --mode accepts); auto enables whichever exist
var knownModes = []string{"hermes", "openclaw", "pi", "claude", "codex", "gemini", "opencode"}

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
	if _, ok := map[string]bool{"pi": true, "claude": true, "codex": true, "gemini": true}[mode]; !ok {
		return fmt.Errorf("--path: %s cannot be relocated this way (supported: pi, claude, codex, gemini)", mode)
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
		// The wrapped source caches its records across scans, so they are shared; prefix
		// a copy instead of writing through to what the cache hands out next time
		fields := make(map[string]any, len(recs[i].fields)+1)
		for k, v := range recs[i].fields {
			fields[k] = v
		}
		fields["source"] = s.mode
		if cwd := toStr(fields["cwd"]); cwd != "" {
			fields["cwd"] = label + cwd
		}
		if project := toStr(fields["project"]); project != "" {
			fields["project"] = label + project
		}
		recs[i].fields = fields
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

	// The four file-backed sources scan one directory, so that directory can move; the
	// json-map and SQLite ones keep their layout across several files and stay at home
	movable := map[string]func(dir string) SessionSource{
		"pi":     func(dir string) SessionSource { return newPiSource(dir) },
		"claude": func(dir string) SessionSource { return newClaudeSource(dir) },
		"codex":  func(dir string) SessionSource { return newCodexSource(dir) },
		"gemini": func(dir string) SessionSource { return newGeminiSource(dir) },
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
