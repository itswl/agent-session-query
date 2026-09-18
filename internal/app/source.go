package app

import (
	"fmt"
	"os"
	"path/filepath"
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

// buildSources wires up data sources according to the run mode.
//
//   - a single named mode: enable only that one
//   - all: enable all six (missing ones warn)
//   - auto: enable whichever exist (the two json-map sources look for sessions.json,
//     the rest for their directory)
func buildSources(mode string) ([]SessionSource, error) {
	home := defaultHome()

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
		"pi":       func() SessionSource { return newPiSource(filepath.Join(home, ".pi", "agent", "sessions")) },
		"claude":   func() SessionSource { return newClaudeSource(filepath.Join(home, ".claude", "projects")) },
		"codex":    func() SessionSource { return newCodexSource(filepath.Join(home, ".codex", "sessions")) },
		"gemini":   func() SessionSource { return newGeminiSource(filepath.Join(home, ".gemini", "tmp")) },
		"opencode": func() SessionSource { return newOpenCodeSource(filepath.Join(openCodeDataDir(home), "opencode.db")) },
	}

	if mode != "auto" && mode != "all" {
		factory, ok := factories[mode]
		if !ok {
			return nil, fmt.Errorf("unknown mode %q (choose from: auto, all, %s)", mode, joinModes())
		}
		return []SessionSource{factory()}, nil
	}

	enabled := []SessionSource{}
	for _, name := range knownModes {
		source := factories[name]()
		if source.Exists() {
			enabled = append(enabled, source)
		} else if mode == "all" {
			fmt.Fprintf(os.Stderr, "[WARN] data source not found, skipped: %s (%s)\n", name, source.Location())
		}
	}
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
