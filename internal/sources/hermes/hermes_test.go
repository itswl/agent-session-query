package hermes

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// The current schema, reduced to the columns that are read.
const schema = `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, source TEXT NOT NULL, display_name TEXT, model TEXT,
	started_at REAL NOT NULL, ended_at REAL, message_count INTEGER DEFAULT 0,
	input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
	cache_read_tokens INTEGER DEFAULT 0, cache_write_tokens INTEGER DEFAULT 0, reasoning_tokens INTEGER DEFAULT 0,
	cwd TEXT, git_branch TEXT, estimated_cost_usd REAL, actual_cost_usd REAL, title TEXT, last_activity_at REAL,
	archived INTEGER NOT NULL DEFAULT 0, hidden INTEGER NOT NULL DEFAULT 0);
CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT,
	tool_call_id TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL NOT NULL, finish_reason TEXT,
	reasoning TEXT, _compressed_summary INTEGER NOT NULL DEFAULT 0,
	active INTEGER NOT NULL DEFAULT 1, compacted INTEGER NOT NULL DEFAULT 0,
	display_kind TEXT, display_metadata TEXT);`

func home(t *testing.T, statements ...string) (sources.Source, string) {
	t.Helper()
	dir := t.TempDir()
	if len(statements) > 0 {
		exec(t, filepath.Join(dir, "state.db"), statements...)
	}
	return Spec.Open(dir), dir
}

func exec(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", kit.FileURI(path)) // a home's path may hold a #
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// msg is an INSERT for one message row; extra is "columns=>values" for the rest.
func msg(session, role, content string, ts float64, extra string) string {
	cols, vals := "", ""
	if extra != "" {
		parts := strings.SplitN(extra, "=>", 2)
		cols, vals = ", "+parts[0], ", "+parts[1]
	}
	return `INSERT INTO messages (session_id, role, content, timestamp` + cols + `) VALUES ('` + session + `', '` + role + `', '` +
		strings.ReplaceAll(content, "'", "''") + `', ` + strconv.FormatFloat(ts, 'f', -1, 64) + vals + `)`
}

func TestDatabaseSession(t *testing.T) {
	src, dir := home(t, schema,
		`INSERT INTO sessions (id, source, display_name, model, started_at, ended_at, message_count, input_tokens, output_tokens,
			cache_read_tokens, reasoning_tokens, cwd, git_branch, estimated_cost_usd, title)
		 VALUES ('s1', 'tui', 'older name', 'deepseek-chat', 1790000000, 1790000100, 6, 1200, 300, 9000, 120, '/w/proj', 'main', 0.02, 'Fix the nginx config')`,
		msg("s1", "user", "the proxy returns 502", 1790000001, ""),
		msg("s1", "assistant", "", 1790000002, `reasoning, tool_calls, finish_reason=>'check the upstream', '[{"id":"call_1","call_id":"call_1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"nginx -t\"}"}}]', 'tool_calls'`),
		msg("s1", "tool", `{"output":"syntax is ok","exit_code":0}`, 1790000003, `tool_call_id, tool_name=>'call_1', 'terminal'`),
		msg("s1", "assistant", "The upstream block pointed at the wrong port.", 1790000004, `finish_reason=>'stop'`),
		// what Hermes does not show: a regenerated reply, a row for the model alone, a hidden one
		msg("s1", "assistant", "an answer that was regenerated away", 1790000005, `active=>0`),
		msg("s1", "user", "context only the model reads", 1790000006, `display_metadata=>'{"model_only": true}'`),
		msg("s1", "assistant", "kept out of the display", 1790000007, `display_kind=>'hidden'`),
		msg("s1", "session_meta", "{}", 1790000008, ""),
	)
	s := sourcetest.One(t, src)
	sourcetest.NothingUnread(t, s)
	sess := s.Session
	if sess.ID != "s1" || sess.Title != "Fix the nginx config" || sess.Cwd != "/w/proj" || sess.Branch != "main" ||
		sess.Platform != "tui" || sess.Model != "deepseek-chat" || sess.Path != filepath.Join(dir, "state.db") {
		t.Errorf("session = %+v", sess)
	}
	if sess.StartedAt.Unix() != 1790000000 || sess.UpdatedAt.Unix() != 1790000008 {
		t.Errorf("times = %v / %v", sess.StartedAt, sess.UpdatedAt)
	}
	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d", len(s.Turns))
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt thinking tool reply" {
		t.Errorf("items = %s", got)
	}
	tool := sourcetest.Tools(turn)[0]
	// Hermes does not record how a call ended: no status is not the same as success
	if tool.Name != "terminal" || tool.Command != "nginx -t" || !tool.Done || tool.Status != "" || !strings.Contains(tool.Output, "syntax is ok") {
		t.Errorf("tool = %+v", tool)
	}
	if turn.Answer != "The upstream block pointed at the wrong port." || turn.Calls[0].Model != "deepseek-chat" {
		t.Errorf("answer %q calls %+v", turn.Answer, turn.Calls)
	}
	// The counters are the session's own: no response carries any
	want := model.Usage{Input: 1200, Output: 300, CacheRead: 9000, Reasoning: 120}
	if s.Stats.Usage != want || s.Stats.CostUSD != 0.02 {
		t.Errorf("usage = %+v cost %v", s.Stats.Usage, s.Stats.CostUSD)
	}
}

// Hermes shows live rows and the rows its compression folded away; compression also
// re-inserts the head and tail of the conversation as live copies of archived originals,
// with their original times, so timestamps regress and a message can be held twice.
func TestCompactedRowsStayAndCopiesCollapse(t *testing.T) {
	src, _ := home(t, schema,
		`INSERT INTO sessions (id, source, started_at) VALUES ('s1', 'cron', 1790000000)`,
		msg("s1", "user", "first question", 1790000001, `active, compacted=>0, 1`),
		msg("s1", "assistant", "first answer", 1790000002, `active, compacted=>0, 1`),
		msg("s1", "user", "second question", 1790000003, `active, compacted=>0, 1`),
		msg("s1", "assistant", "the summary of what came before", 1790000010, `_compressed_summary=>1`),
		// the live copy of an archived original: same words, same time, inserted later
		msg("s1", "user", "second question", 1790000003, ""),
		msg("s1", "assistant", "second answer", 1790000011, ""),
	)
	s := sourcetest.One(t, src)
	sourcetest.NothingUnread(t, s)
	if s.Stats.Turns != 2 {
		for _, turn := range s.Turns {
			t.Logf("turn %d %q: %s", turn.Index, turn.Ask, sourcetest.Kinds(turn))
		}
		t.Fatalf("turns = %d, want the two questions once each", s.Stats.Turns)
	}
	if one := s.Turn(t, 1); one.Ask != "first question" || one.Answer != "first answer" {
		t.Errorf("the compacted half is still the conversation: %+v", one)
	}
	two := s.Turn(t, 2)
	if got := sourcetest.Kinds(two); got != "prompt event reply" || two.Items[1].Event != model.EventCompaction || two.Answer != "second answer" {
		t.Errorf("turn 2 = %s %+v", got, two.Items)
	}
}

func TestArchivedByEitherColumnOrNeither(t *testing.T) {
	for name, c := range map[string]struct {
		column string
		want   bool
	}{"archived": {"archived INTEGER NOT NULL DEFAULT 1", true}, "hidden": {"hidden INTEGER NOT NULL DEFAULT 1", true}, "neither": {"note TEXT", false}} {
		src, _ := home(t,
			`CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT, started_at REAL, `+c.column+`)`,
			`CREATE TABLE messages (id TEXT, session_id TEXT, role TEXT, content TEXT, timestamp REAL)`,
			`INSERT INTO sessions (id, source, started_at) VALUES ('s', 'tui', 1790000000)`,
			`INSERT INTO messages VALUES ('m1', 's', 'user', 'hello', 1790000001)`,
			`INSERT INTO messages VALUES ('m2', 's', 'assistant', 'hi', 1790000002)`,
		)
		s := sourcetest.One(t, src)
		sourcetest.NothingUnread(t, s)
		if s.Session.Archived != c.want {
			t.Errorf("%s: archived = %v", name, s.Session.Archived)
		}
		// An older schema — text ids, none of the newer columns — still reads
		if s.Stats.Turns != 1 || s.Turn(t, 1).Answer != "hi" {
			t.Errorf("%s: turns = %+v", name, s.Turns)
		}
	}
}

func TestScan(t *testing.T) {
	src, dir := home(t, schema,
		`INSERT INTO sessions (id, source, started_at, title) VALUES ('a', 'tui', 1790000000, 'A'), ('b', 'tui', 1790000050, 'B')`,
		msg("a", "user", "one", 1790000001, ""),
		msg("b", "user", "two", 1790000051, ""),
	)
	db := filepath.Join(dir, "state.db")
	stamps := func() map[string]string {
		refs, err := src.Scan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, ref := range refs {
			out[ref.Key] = ref.Stamp
		}
		return out
	}
	before := stamps()
	if len(before) != 2 || before[db+"#a"] == "" {
		t.Fatalf("refs = %v", before)
	}
	exec(t, db, msg("b", "assistant", "an answer", 1790000052, ""))
	after := stamps()
	if before[db+"#a"] != after[db+"#a"] {
		t.Error("a message in another session moved this session's stamp")
	}
	if before[db+"#b"] == after[db+"#b"] {
		t.Error("a new message did not move its session's stamp")
	}
	exec(t, db, `UPDATE sessions SET title = 'A, renamed' WHERE id = 'a'`)
	if stamps()[db+"#a"] == after[db+"#a"] {
		t.Error("a new title did not move the session's stamp")
	}
}

// A state.db that cannot be read must not come back as a Hermes with no sessions in it.
func TestUnreadableDatabaseIsAnError(t *testing.T) {
	src, _ := home(t, `CREATE TABLE sessions (name TEXT)`, `CREATE TABLE messages (x TEXT)`)
	if _, err := src.Scan(context.Background()); err == nil {
		t.Error("a schema that moved listed nothing and said nothing")
	}
}

// The older layout: sessions.json and a transcript per session. A session held both ways
// is listed once and read from its transcript, with what the database says about it.
func TestTranscriptFilesAndTheIndex(t *testing.T) {
	src, dir := home(t, schema,
		`INSERT INTO sessions (id, source, started_at, title, cwd) VALUES ('both', 'tui', 1790000000, 'Held both ways', '/w/both')`,
		msg("both", "user", "the database's copy", 1790000001, ""),
	)
	sessions := filepath.Join(dir, "sessions")
	sourcetest.Write(t, filepath.Join(sessions, "sessions.json"), map[string]any{
		"_README": "session index",
		"agent:main:both": map[string]any{"session_id": "both", "display_name": "index name", "platform": "feishu",
			"created_at": "2026-09-22T10:00:00", "updated_at": "2026-09-22T10:05:00", "input_tokens": 50, "output_tokens": 5},
		"agent:main:file": map[string]any{"session_id": "file-only", "display_name": "Only a file", "platform": "tui", "model": "glm-5",
			"created_at": "2026-09-20T10:00:00", "updated_at": "2026-09-20T10:05:00", "input_tokens": 700, "output_tokens": 70, "estimated_cost_usd": 0.5},
		"agent:main:none": map[string]any{"session_id": "nothing-to-read", "display_name": "No transcript anywhere"},
	})
	sourcetest.Write(t, filepath.Join(sessions, "both.jsonl"),
		map[string]any{"role": "user", "content": "the transcript's copy", "timestamp": "2026-09-22T10:00:01"},
		map[string]any{"role": "assistant", "content": "answered", "reasoning": "thinking", "timestamp": "2026-09-22T10:00:05"})
	sourcetest.Write(t, filepath.Join(sessions, "file-only.jsonl"),
		map[string]any{"role": "session_meta", "platform": "tui", "model": "glm-5"},
		map[string]any{"role": "user", "content": "from the older layout", "timestamp": "2026-09-20T10:00:01"},
		map[string]any{"role": "assistant", "content": "", "timestamp": "2026-09-20T10:00:02",
			"tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "terminal", "arguments": `{"command":"ls"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "a.txt", "timestamp": "2026-09-20T10:00:03"},
		map[string]any{"role": "assistant", "content": "one file", "timestamp": "2026-09-20T10:00:04"})

	all := sourcetest.All(t, src)
	if len(all) != 2 {
		t.Fatalf("sessions = %d, want each session once and none for the record with nothing to read", len(all))
	}
	sourcetest.NothingUnread(t, all...)
	both := sourcetest.ByID(t, all, "both")
	if both.Turn(t, 1).Ask != "the transcript's copy" || both.Session.Title != "Held both ways" || both.Session.Cwd != "/w/both" {
		t.Errorf("both = %+v ask %q", both.Session, both.Turn(t, 1).Ask)
	}
	file := sourcetest.ByID(t, all, "file-only")
	if file.Session.Title != "Only a file" || file.Session.Model != "glm-5" || file.Session.Platform != "tui" {
		t.Errorf("file-only session = %+v", file.Session)
	}
	turn := file.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt tool reply" || sourcetest.Tools(turn)[0].Output != "a.txt" || sourcetest.Tools(turn)[0].Command != "ls" {
		t.Errorf("file-only turn = %s", got)
	}
	if file.Stats.Usage != (model.Usage{Input: 700, Output: 70}) || file.Stats.CostUSD != 0.5 {
		t.Errorf("file-only totals = %+v", file.Stats)
	}
}

func TestLocate(t *testing.T) {
	if got := Spec.Locate(sourcetest.Env("/home/u")); got != filepath.Join("/home/u", ".hermes") {
		t.Errorf("default = %s", got)
	}
	if got := Spec.Locate(sourcetest.Env("/home/u", "HERMES_HOME", "/data/hermes")); got != "/data/hermes" {
		t.Errorf("relocated = %s", got)
	}
	if src, _ := home(t); src.Exists() {
		t.Error("an empty home is no store")
	}
}

// A row of a kind Hermes had not written when this was made is counted under its name, in
// the database and in a transcript file alike, and the rest of the session reads as it
// would without it.
func TestUnknownKindsAreCounted(t *testing.T) {
	src, dir := home(t, schema,
		`INSERT INTO sessions (id, source, started_at) VALUES ('db', 'tui', 1790000000)`,
		msg("db", "user", "the proxy returns 502", 1790000001, ""),
		msg("db", "moderator", "a role from a later Hermes", 1790000002, ""),
		msg("db", "assistant", "The upstream block pointed at the wrong port.", 1790000003, `finish_reason=>'stop'`),
	)
	sessions := filepath.Join(dir, "sessions")
	sourcetest.Write(t, filepath.Join(sessions, "sessions.json"), map[string]any{
		"agent:main:file": map[string]any{"session_id": "file", "display_name": "A file"},
	})
	sourcetest.Write(t, filepath.Join(sessions, "file.jsonl"),
		map[string]any{"role": "session_meta", "platform": "tui", "model": "glm-5"},
		map[string]any{"role": "system", "content": "the system prompt"},
		map[string]any{"role": "user", "content": "list the files", "timestamp": "2026-09-20T10:00:01"},
		map[string]any{"role": "moderator", "content": "a role from a later Hermes", "timestamp": "2026-09-20T10:00:02"},
		map[string]any{"type": "checkpoint", "id": "k1", "timestamp": "2026-09-20T10:00:03"},
		map[string]any{"content": "a row that says nothing of what it is"},
		map[string]any{"role": "assistant", "content": "one file", "timestamp": "2026-09-20T10:00:04"})

	all := sourcetest.All(t, src)
	db, file := sourcetest.ByID(t, all, "db"), sourcetest.ByID(t, all, "file")
	if len(db.Unread) != 1 || db.Unread["role:moderator"] != 1 {
		t.Errorf("the database's unread = %v, want the one row of an unknown role", db.Unread)
	}
	want := map[string]int{"role:moderator": 1, "row:checkpoint": 1, "role:(untyped)": 1}
	if len(file.Unread) != len(want) {
		t.Errorf("the file's unread = %v, want %v", file.Unread, want)
	}
	for kind, n := range want {
		if file.Unread[kind] != n {
			t.Errorf("the file's unread = %v, want %v", file.Unread, want)
			break
		}
	}
	for _, s := range []sourcetest.Session{db, file} {
		if s.Stats.Turns != 1 {
			t.Fatalf("%s: turns = %d", s.Session.ID, s.Stats.Turns)
		}
		if turn := s.Turn(t, 1); sourcetest.Kinds(turn) != "prompt reply" {
			t.Errorf("%s: items = %s, want the ask and the answer alone", s.Session.ID, sourcetest.Kinds(turn))
		}
	}
}

// The totals Hermes keeps for a session are read as counts, by model.Tokens's rule: one
// below zero, or past model.MaxTokens, is none — not the bound — while the bound itself is
// a count. A price below zero, past any session's, or infinite is none too — in the
// database's session row and in the older index alike. Where what was billed is not a
// price, what was estimated stands.
func TestTotalsThatAreNotCounts(t *testing.T) {
	src, dir := home(t, schema,
		`INSERT INTO sessions (id, source, started_at, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, actual_cost_usd, estimated_cost_usd)
		 VALUES ('db', 'tui', 1790000000, -1200, 300, 9e18, `+strconv.Itoa(model.MaxTokens+1)+`, `+strconv.Itoa(model.MaxTokens)+`, -0.5, -0.25)`,
		`INSERT INTO sessions (id, source, started_at, output_tokens, actual_cost_usd, estimated_cost_usd) VALUES ('inf', 'tui', 1790000000, 1, 1e999, 2e9)`,
		`INSERT INTO sessions (id, source, started_at, output_tokens, actual_cost_usd, estimated_cost_usd) VALUES ('est', 'tui', 1790000000, 1, 5e300, 0.75)`,
		msg("db", "user", "hello", 1790000001, ""),
		msg("db", "assistant", "hi", 1790000002, ""),
		msg("inf", "user", "hello", 1790000001, ""),
		msg("est", "user", "hello", 1790000001, ""),
	)
	sessions := filepath.Join(dir, "sessions")
	sourcetest.Write(t, filepath.Join(sessions, "sessions.json"), map[string]any{
		"agent:main:file": map[string]any{"session_id": "file", "input_tokens": -700, "output_tokens": 70, "cache_read_tokens": 9e18, "estimated_cost_usd": -0.5},
		"agent:main:big":  map[string]any{"session_id": "big", "output_tokens": 70, "cache_write_tokens": model.MaxTokens + 1, "estimated_cost_usd": 1e300},
	})
	sourcetest.Write(t, filepath.Join(sessions, "file.jsonl"),
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi", "usage": map[string]any{"input_tokens": -3, "output_tokens": 2}})
	sourcetest.Write(t, filepath.Join(sessions, "big.jsonl"), map[string]any{"role": "user", "content": "hello"})
	all := sourcetest.All(t, src)
	for _, s := range all {
		sourcetest.Invariants(t, s)
	}
	if db := sourcetest.ByID(t, all, "db"); db.Stats.Usage != (model.Usage{Output: 300, Reasoning: model.MaxTokens}) || db.Stats.CostUSD != 0 {
		t.Errorf("db: usage = %+v, cost %v", db.Stats.Usage, db.Stats.CostUSD)
	}
	if inf := sourcetest.ByID(t, all, "inf"); inf.Stats.CostUSD != 0 {
		t.Errorf("inf: cost %v, want none for a price that is infinite or past any session's", inf.Stats.CostUSD)
	}
	if est := sourcetest.ByID(t, all, "est"); est.Stats.CostUSD != 0.75 {
		t.Errorf("est: cost %v, want what was estimated where what was billed is not a price", est.Stats.CostUSD)
	}
	if file := sourcetest.ByID(t, all, "file"); file.Stats.Usage != (model.Usage{Output: 70}) || file.Stats.CostUSD != 0 {
		t.Errorf("file: usage = %+v, cost %v", file.Stats.Usage, file.Stats.CostUSD)
	}
	if big := sourcetest.ByID(t, all, "big"); big.Stats.Usage != (model.Usage{Output: 70}) || big.Stats.CostUSD != 0 {
		t.Errorf("big: usage = %+v, cost %v", big.Stats.Usage, big.Stats.CostUSD)
	}
}

// A home whose path holds a # is read like any other: the database's sessions are named by
// the database's path and the id after it, and that path is not cut at its own #.
func TestAHomeWhosePathHoldsAHash(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "with#hash")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exec(t, filepath.Join(dir, "state.db"), schema,
		`INSERT INTO sessions (id, source, started_at) VALUES ('a#1', 'tui', 1790000000)`,
		msg("a#1", "user", "the proxy returns 502", 1790000001, ""),
		msg("a#1", "assistant", "The upstream block pointed at the wrong port.", 1790000002, `finish_reason=>'stop'`))
	s := sourcetest.One(t, Spec.Open(dir))
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "a#1" || s.Session.Path != filepath.Join(dir, "state.db") || s.Stats.Turns != 1 ||
		s.Turn(t, 1).Answer != "The upstream block pointed at the wrong port." {
		t.Errorf("session = %+v, %d turns", s.Session, s.Stats.Turns)
	}
}

// A column holds a value of any type in SQLite. One row's figure of a type it should not
// be — a message's timestamp written as text, which MAX hands back over every number; a
// count with a fraction; a time that is a word — is read as none: it does not make the
// whole store look unreadable, and every session is still listed and read.
func TestAFigureOfTheWrongTypeDoesNotHideTheStore(t *testing.T) {
	src, _ := home(t, schema,
		`INSERT INTO sessions (id, source, started_at, ended_at, message_count, last_activity_at) VALUES ('a', 'tui', 1790000000, 'later', 2.5, x'00')`,
		`INSERT INTO sessions (id, source, started_at) VALUES ('b', 'tui', 1790000100)`,
		msg("a", "user", "the proxy returns 502", 1790000001, ""),
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('a', 'assistant', 'The upstream block pointed at the wrong port.', 'yesterday')`,
		msg("b", "user", "another session", 1790000101, ""),
	)
	all := sourcetest.All(t, src)
	if len(all) != 2 {
		t.Fatalf("sessions = %d, want both", len(all))
	}
	sourcetest.NothingUnread(t, all...)
	a := sourcetest.ByID(t, all, "a")
	if a.Stats.Turns != 1 || a.Turn(t, 1).Answer != "The upstream block pointed at the wrong port." || a.Session.StartedAt.Unix() != 1790000000 {
		t.Errorf("a = %+v, %d turns", a.Session, a.Stats.Turns)
	}
	if b := sourcetest.ByID(t, all, "b"); b.Stats.Turns != 1 || b.Turn(t, 1).Ask != "another session" {
		t.Errorf("b = %+v", b.Session)
	}
}

// Whether a message is the summary compression left is read as whatever the column
// holds: a word or a blob in one row is that row's flag unknown, not the session
// unreadable. (Whether a row is live, Hermes itself decides in its query, and a row whose
// flag is not 1 is not shown, by Hermes or here.)
func TestAFlagOfTheWrongTypeDoesNotHideTheSession(t *testing.T) {
	src, _ := home(t, schema,
		`INSERT INTO sessions (id, source, started_at) VALUES ('f', 'tui', 1790000000)`,
		msg("f", "user", "check the queue", 1790000001, ""),
		`INSERT INTO messages (session_id, role, content, timestamp, active, _compressed_summary) VALUES ('f', 'assistant', 'The queue is drained.', 1790000002, 1, 'no')`,
		`INSERT INTO messages (session_id, role, content, timestamp, active, _compressed_summary) VALUES ('f', 'user', 'and the dead letters?', 1790000003, 1, x'00')`,
		`INSERT INTO messages (session_id, role, content, timestamp, active, _compressed_summary) VALUES ('f', 'assistant', 'What compression kept.', 1790000004, 1, '1')`,
	)
	s := sourcetest.One(t, src)
	if s.Stats.Turns != 2 || s.Turn(t, 1).Answer != "The queue is drained." || s.Turn(t, 2).Ask != "and the dead letters?" {
		t.Fatalf("turns = %d: answer %q, ask %q", s.Stats.Turns, s.Turn(t, 1).Answer, s.Turn(t, 2).Ask)
	}
	if got := sourcetest.Kinds(s.Turn(t, 2)); got != "prompt event" {
		t.Errorf("a summary flagged '1' = %s, want the compaction event", got)
	}
}
