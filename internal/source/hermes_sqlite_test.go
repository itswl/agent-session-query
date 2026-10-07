package source

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// makeHermesDB builds a state.db shaped like Hermes's
func makeHermesDB(t *testing.T, dbPath string, schema string, statements []string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteURI(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range append([]string{schema}, statements...) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("executing %q failed: %v", stmt, err)
		}
	}
}

const hermesSchema = `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, message_count INTEGER,
	input_tokens INTEGER, output_tokens INTEGER,
	cache_read_tokens INTEGER, cache_write_tokens INTEGER,
	reasoning_tokens INTEGER, estimated_cost_usd REAL,
	session_key TEXT, display_name TEXT, source TEXT, model TEXT,
	started_at REAL, ended_at REAL,
	title TEXT, cwd TEXT, archived INTEGER NOT NULL DEFAULT 0,
	pinned INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE messages (
	id TEXT, session_id TEXT, role TEXT, content TEXT,
	finish_reason TEXT, reasoning TEXT, timestamp REAL,
	active INTEGER DEFAULT 1,
	tool_calls TEXT, tool_name TEXT
);`

func newHermesFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	setHome(t, home)
	dir := filepath.Join(home, ".hermes")
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "state.db")
}

func TestHermesSQLiteFinal(t *testing.T) {
	dbPath := newHermesFixture(t)
	makeHermesDB(t, dbPath, hermesSchema, []string{
		`INSERT INTO sessions (id, message_count, input_tokens, output_tokens, cache_read_tokens,
			cache_write_tokens, reasoning_tokens, estimated_cost_usd)
		 VALUES ('h-webhook', 12, 1500, 220, 30, 40, 7, 0.0123)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('m1', 'h-webhook', 'assistant', 'the earlier one', 'stop', 'reasoning one', '2026-09-13T10:00:00Z', 1)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('m2', 'h-webhook', 'assistant', 'the final answer', 'stop', 'reasoning two', '2026-09-13T10:05:00Z', 1)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('m3', 'h-webhook', 'assistant', 'soft deleted', 'stop', 'reasoning three', '2026-09-13T10:09:00Z', 0)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('m4', 'h-webhook', 'user', 'a user message', NULL, NULL, '2026-09-13T09:59:00Z', 1)`,
	})

	result := hermesSQLiteFinal(dbPath, "hermes", "h-webhook", "done")
	if result == nil {
		t.Fatal("the final message should have come from state.db")
	}
	if result["text"] != "the final answer" || result["thinking"] != "reasoning two" {
		t.Fatalf("this is not the last active=1 assistant message: %v", result)
	}
	// The timestamp column is a REAL epoch; it used to reach the client as a stringified
	// float ("1.789705937728841e+09") rather than a date
	if ts, _ := result["timestamp"].(string); !strings.HasPrefix(ts, "2026-") {
		t.Errorf("timestamp = %v, want a formatted date", result["timestamp"])
	}
	if result["isFinal"] != true || result["isProcessing"] != false || result["messageCount"] != int64(12) {
		t.Fatalf("the terminal-state fields are wrong: %v", result)
	}
	if result["stopReason"] != "stop" || result["source"] != "hermes" || result["timestamp"] != "2026-09-13T10:05:00Z" {
		t.Fatalf("wrong fields: %v", result)
	}
	usage := result["usage"].(map[string]any)
	if usage["inputTokens"] != int64(1500) || usage["estimatedCostUsd"] != 0.0123 || usage["reasoningTokens"] != int64(7) {
		t.Fatalf("usage is wrong: %v", usage)
	}

	// Not registered in the sessions table: usage is an empty object and message_count falls
	// back to the real count
	makeHermesDB(t, dbPath, "", []string{
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('n1', 'h-count', 'assistant', 'counted instead', 'stop', NULL, '2026-09-14T10:00:00Z', 1)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('n2', 'h-count', 'user', 'a question', NULL, NULL, '2026-09-14T09:59:00Z', 1)`,
	})
	second := hermesSQLiteFinal(dbPath, "hermes", "h-count", "done")
	if second == nil || second["messageCount"] != int64(2) {
		t.Fatalf("message_count should fall back to 2: %v", second)
	}
	if len(second["usage"].(map[string]any)) != 0 {
		t.Fatalf("with no sessions row, usage should be empty: %v", second["usage"])
	}

	// No assistant message with finish_reason=stop yields nil, leaving the fallback to the
	// layer above
	if got := hermesSQLiteFinal(dbPath, "hermes", "h-empty", "done"); got != nil {
		t.Fatalf("a miss should return nil: %v", got)
	}
	// A non-hermes source, an empty session id, and a missing database all skip this path
	if got := hermesSQLiteFinal(dbPath, "openclaw", "h-webhook", "done"); got != nil {
		t.Fatalf("openclaw must not go through sqlite: %v", got)
	}
	if got := hermesSQLiteFinal(filepath.Join(t.TempDir(), "nope.db"), "hermes", "h-webhook", "done"); got != nil {
		t.Fatalf("a missing database should return nil: %v", got)
	}
	if got := hermesSQLiteFinal("", "hermes", "h-webhook", "done"); got != nil {
		t.Fatalf("no configured state.db path should return nil: %v", got)
	}
}

// TestFinalUsesConfiguredDBPath: Final has to use the stateDB the source was configured
// with rather than re-deriving one from $HOME — when the two disagree it would silently
// query the wrong database.
func TestFinalUsesConfiguredDBPath(t *testing.T) {
	elsewhere := t.TempDir()
	dbPath := filepath.Join(elsewhere, "state.db")
	makeHermesDB(t, dbPath, hermesSchema, []string{
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('x1', 'h-elsewhere', 'assistant', 'the answer in the other database', 'stop', NULL, '2026-09-13T10:09:00Z', 1)`,
	})

	// There is nothing under home, so only genuinely using def.stateDB can find it
	setHome(t, t.TempDir())
	def := hermesDef(defaultHome())
	def.stateDB = dbPath

	source := newJsonMapSource(def)
	final := source.Final(NewRecord(Record{
		Source: "hermes", SessionID: "h-elsewhere", Status: "done",
	}, ""))
	if final["text"] != "the answer in the other database" {
		t.Fatalf("the configured state.db was not used: %v", final)
	}
}

func TestJsonMapUsesSQLiteWhenFileMissing(t *testing.T) {
	dbPath := newHermesFixture(t)
	dir := filepath.Dir(dbPath)
	write(t, filepath.Join(dir, "sessions", "sessions.json"),
		`{"hook:webhook:only":{"session_id":"h-talk","updated_at":"2026-09-13T10:10:00Z"}}`,
	)
	makeHermesDB(t, dbPath, hermesSchema, []string{
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('w1', 'h-talk', 'assistant', 'an answer that exists only in state.db', 'stop', NULL, '2026-09-13T10:09:00Z', 1)`,
	})

	source := newJsonMapSource(hermesDef(defaultHome()))
	list := source.List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	final := source.Final(list[0])
	if final["isFinal"] != true || final["text"] != "an answer that exists only in state.db" {
		raw, _ := json.Marshal(final)
		t.Fatalf("final should come from state.db: %s", raw)
	}
	if final["error"] != nil {
		t.Fatalf("this should not hit the error fallback: %v", final)
	}
}

// The newer Hermes shape: no sessions.json and no jsonl, every session lives in state.db
func TestHermesSQLiteOnlySource(t *testing.T) {
	dbPath := newHermesFixture(t)
	makeHermesDB(t, dbPath, hermesSchema, []string{
		`INSERT INTO sessions (id, session_key, display_name, source, model,
			input_tokens, output_tokens, estimated_cost_usd, started_at, ended_at)
		 VALUES ('20260814_002606_1a7908', NULL, 'desktop session', 'desktop', 'deepseek-v4-pro',
			1500, 220, 0.0123, 1786638366.44, NULL)`,
		`INSERT INTO messages (id, session_id, role, content, reasoning, timestamp, active)
		 VALUES (1, '20260814_002606_1a7908', 'user', 'which model are you?', NULL, 1786638379.3163, 1)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES (2, '20260814_002606_1a7908', 'assistant', 'I am Hermes', 'stop', 'let me think', 1786638385.27674, 1)`,
		`INSERT INTO messages (id, session_id, role, content, reasoning, timestamp, active)
		 VALUES (3, '20260814_002606_1a7908', 'user', 'soft deleted', NULL, 1786713684.97493, 0)`,
	})

	source := newJsonMapSource(hermesDef(defaultHome()))
	if !source.Exists() {
		t.Fatal("the hermes source should be enabled when state.db exists")
	}
	if source.Location() != dbPath {
		t.Fatalf("location should point at state.db: %v", source.Location())
	}

	list := source.List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	if err := source.ListError(); err != nil {
		t.Fatalf("a list that worked must not leave a failure behind: %v", err)
	}
	r := list[0]
	if r.SessionID != "20260814_002606_1a7908" || r.Key != "20260814_002606_1a7908" {
		t.Fatalf("record = %v", r)
	}
	if r.Platform != "desktop" || r.Model != "deepseek-v4-pro" || r.DisplayName != "desktop session" {
		t.Fatalf("record = %v", r)
	}
	if r.TotalTokens != 1720 || r.EstimatedCostUsd != 0.0123 {
		t.Fatalf("record = %v", r)
	}
	if r.HasFile || r.File != "" {
		t.Fatalf("record = %v", r)
	}
	// updatedAt takes the time of the last active message (epoch seconds to UTC ISO)
	if r.CreatedAt != "2026-08-13T16:26:06" || r.UpdatedAt != "2026-08-13T16:26:25" {
		t.Fatalf("createdAt/updatedAt = %v / %v", r.CreatedAt, r.UpdatedAt)
	}

	msgs := source.Messages(r, MessageQuery{Limit: 50})
	if len(msgs) != 2 { // the active=0 one does not count
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["role"] != "user" || msgs[0]["timestamp"] != "2026-08-13T16:26:19" {
		t.Fatalf("msgs[0] = %v", msgs[0])
	}
	blocks := msgs[1]["content"].([]map[string]any)
	if len(blocks) != 2 || blocks[0]["type"] != "thinking" || blocks[0]["content"] != "let me think" ||
		blocks[1]["type"] != "text" || blocks[1]["content"] != "I am Hermes" {
		t.Fatalf("blocks = %v", blocks)
	}

	final := source.Final(r)
	if final["isFinal"] != true || final["text"] != "I am Hermes" || final["thinking"] != "let me think" {
		t.Fatalf("final = %v", final)
	}
}

func TestHermesSQLiteListAndToolMessages(t *testing.T) {
	dbPath := newHermesFixture(t)
	makeHermesDB(t, dbPath, hermesSchema, []string{
		// title wins over display_name; cwd is the new column
		`INSERT INTO sessions (id, session_key, title, display_name, cwd, model,
		                       started_at, ended_at, estimated_cost_usd, message_count)
		 VALUES ('h-live', '', '排查接口 502', '', '/w/proj', 'deepseek-chat',
		         1789705755.0, 1789705770.0, 0.0012, 7)`,
		// archived: sessions Hermes hides from its own list must not be listed here either
		`INSERT INTO sessions (id, session_key, display_name, archived, started_at, ended_at)
		 VALUES ('h-archived', 'k-archived', 'bot session', 1, 1789705755.0, 1789705770.0)`,
		// display_name only (the older field, pre-title Hermes)
		`INSERT INTO sessions (id, session_key, display_name, started_at, ended_at)
		 VALUES ('h-named', 'k-named', 'older session', 1789600000.0, 1789600100.0)`,

		`INSERT INTO messages (id, session_id, role, content, timestamp)
		 VALUES ('n1', 'h-live', 'user', 'why is the API returning 502', 1789705756.0)`,
		// an assistant turn that stops to run tools: reasoning + tool_calls
		`INSERT INTO messages (id, session_id, role, reasoning, tool_calls, finish_reason, timestamp)
		 VALUES ('n2', 'h-live', 'assistant', 'check the gateway logs first',
		         '[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"kubectl logs\",\"timeout\":10}"}}]',
		         'tool_calls', 1789705757.0)`,
		// the tool result: role=tool with tool_name and a structured content
		`INSERT INTO messages (id, session_id, role, content, tool_name, timestamp)
		 VALUES ('n3', 'h-live', 'tool', '{"output": "upstream timeout x3"}', 'terminal', 1789705758.0)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, timestamp)
		 VALUES ('n4', 'h-live', 'assistant', 'the upstream is timing out', 'stop', 1789705759.0)`,
	})

	records, err := hermesSQLiteList(dbPath, "hermes", nil)
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	if len(records) != 2 {
		// A column/scan mismatch returns no records, so this count also guards the SELECT
		// list staying in step with the Scan
		t.Fatalf("the archived session must be skipped: %d records", len(records))
	}
	if n := records[0].MessageCount; !records[0].HasCount || n != 7 {
		t.Errorf("messageCount = %v, want 7 (sessions.message_count)", n)
	}
	live := records[0] // newest first
	if live.ShortKey != "排查接口 502" {
		t.Errorf("title must be the display name: %q", live.ShortKey)
	}
	if live.Cwd != "/w/proj" {
		t.Errorf("cwd = %q", live.Cwd)
	}
	named := records[1]
	if named.ShortKey != "older session" {
		t.Errorf("display_name is the fallback: %q", named.ShortKey)
	}

	msgs := hermesSQLiteMessages(dbPath, "h-live", MessageQuery{Limit: 10})
	if len(msgs) != 4 {
		t.Fatalf("4 messages expected, got %d", len(msgs))
	}
	// n2: reasoning becomes thinking, tool_calls becomes a toolCall block with the
	// arguments string parsed into an object
	n2 := msgs[1]["content"].([]map[string]any)
	if n2[0]["type"] != "thinking" {
		t.Errorf("first block of n2 = %v", n2[0])
	}
	call := n2[1]
	if call["type"] != "toolCall" || call["name"] != "terminal" {
		t.Errorf("toolCall = %v", call)
	}
	if args := call["arguments"].(map[string]any); args["command"] != "kubectl logs" {
		t.Errorf("arguments = %v (must be the parsed JSON string)", args)
	}
	// n3: a tool row is the result, tool_name rides along
	n3 := msgs[2]
	if n3["role"] != "tool" {
		t.Errorf("third message role = %v", n3["role"])
	}
	block := n3["content"].([]map[string]any)[0]
	if block["type"] != "toolResult" || block["toolName"] != "terminal" {
		t.Errorf("toolResult = %v", block)
	}
	if !strings.Contains(block["content"].(string), "upstream timeout") {
		t.Errorf("tool output = %v", block["content"])
	}
}

// hermesSessionsSchema builds a state.db whose sessions table carries the columns the list
// query reads plus the given visibility column ("" for a Hermes whose table has none).
// Which column marks the sessions Hermes hides is the one thing that differs between
// versions, so each variant is named in the test that needs it.
func hermesSessionsSchema(visibility string) string {
	tail := ""
	if visibility != "" {
		tail = ",\n\t" + visibility + " INTEGER NOT NULL DEFAULT 0"
	}
	return `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, session_key TEXT, title TEXT, display_name TEXT,
	source TEXT, model TEXT, cwd TEXT,
	input_tokens INTEGER, output_tokens INTEGER, reasoning_tokens INTEGER,
	estimated_cost_usd REAL, started_at REAL, ended_at REAL, message_count INTEGER` + tail + `
);
CREATE TABLE messages (
	id TEXT, session_id TEXT, role TEXT, content TEXT, timestamp REAL,
	active INTEGER DEFAULT 1
);`
}

// TestHermesSQLiteListLegacyHiddenColumn: Hermes before the rename marked the sessions it
// hides in `hidden`. The filter is chosen from the schema, so those installs keep hiding
// them — and, more to the point, an install whose table has no `hidden` no longer fails its
// entire list over a column that is not there.
func TestHermesSQLiteListLegacyHiddenColumn(t *testing.T) {
	dbPath := newHermesFixture(t)
	makeHermesDB(t, dbPath, hermesSessionsSchema("hidden"), []string{
		`INSERT INTO sessions (id, session_key, display_name, started_at, ended_at)
		 VALUES ('h-visible', 'k-visible', 'a normal session', 1789705755.0, 1789705770.0)`,
		`INSERT INTO sessions (id, session_key, display_name, hidden, started_at, ended_at)
		 VALUES ('h-hidden', 'k-hidden', 'bot session', 1, 1789705755.0, 1789705770.0)`,
	})

	records, err := hermesSQLiteList(dbPath, "hermes", nil)
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	if len(records) != 1 || records[0].SessionID != "h-visible" {
		t.Fatalf("the hidden session must be skipped: %v", records)
	}
}

// TestHermesSQLiteListWithoutVisibilityColumn: with no visibility column at all the list
// runs unfiltered. That is the safer failure of the two — showing a session Hermes would
// have hidden beats answering a database full of sessions with an empty list.
func TestHermesSQLiteListWithoutVisibilityColumn(t *testing.T) {
	dbPath := newHermesFixture(t)
	makeHermesDB(t, dbPath, hermesSessionsSchema(""), []string{
		`INSERT INTO sessions (id, session_key, display_name, started_at, ended_at)
		 VALUES ('h-one', 'k-one', 'the only session', 1789705755.0, 1789705770.0)`,
	})

	records, err := hermesSQLiteList(dbPath, "hermes", nil)
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	if len(records) != 1 || records[0].SessionID != "h-one" {
		t.Fatalf("an unfiltered list must still return the session: %v", records)
	}
}

// TestHermesListFailureIsReported: a state.db this cannot read must not come back as a
// source with nothing in it — "why is hermes empty" and "you have no hermes sessions" must
// not read the same, which is exactly how a schema that moved under this went unnoticed.
// (What a caller sees — /sessions' warnings, /health — is tested from the app side.)
func TestHermesListFailureIsReported(t *testing.T) {
	dbPath := newHermesFixture(t)
	// A sessions table missing the columns the list reads (title here): the statement fails
	// outright, the way a renamed or dropped column does
	makeHermesDB(t, dbPath, `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, session_key TEXT, display_name TEXT, archived INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE messages (
	id TEXT, session_id TEXT, role TEXT, content TEXT, timestamp REAL,
	active INTEGER DEFAULT 1
);`, nil)

	src := newJsonMapSource(hermesDef(defaultHome()))
	if err := src.ListError(); err != nil {
		t.Fatalf("nothing has listed anything yet: %v", err)
	}
	if recs := src.List(); len(recs) != 0 {
		t.Fatalf("a database that cannot be read listed %d records", len(recs))
	}
	err := src.ListError()
	if err == nil {
		t.Fatal("the failure must be reported, not returned as an empty source")
	}
	if !strings.Contains(err.Error(), "no such column") {
		t.Fatalf("the underlying error should reach the caller: %v", err)
	}

	// Every scan replaces the last one's verdict: with the database it used to fail on
	// gone, the failure must go with it rather than sit there being reported forever
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	src.List()
	if err := src.ListError(); err != nil {
		t.Fatalf("a scan that no longer touches the database still reports its old failure: %v", err)
	}
}

// TestHermesCompactedRowsStayVisible: Hermes shows live rows and the rows its context
// compression archived (active = 0, compacted = 1), and hides only what undo, rewind and
// regenerate removed (active = 0, compacted = 0). Filtering on active alone, as this did,
// hid the older half of every compressed session. Compression also re-inserts protected
// rows as live copies of archived originals, so a row is one message whether the database
// holds it once or twice; and rows the model reads but nobody typed (model_only) stay out.
// A tool row pairs with its call by tool_call_id.
func TestHermesCompactedRowsStayVisible(t *testing.T) {
	dbPath := newHermesFixture(t)
	schema := `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, message_count INTEGER,
	input_tokens INTEGER, output_tokens INTEGER,
	cache_read_tokens INTEGER, cache_write_tokens INTEGER,
	reasoning_tokens INTEGER, estimated_cost_usd REAL,
	session_key TEXT, display_name TEXT, source TEXT, model TEXT,
	started_at REAL, ended_at REAL, title TEXT, cwd TEXT, archived INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE messages (
	id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT,
	finish_reason TEXT, reasoning TEXT, timestamp REAL,
	active INTEGER DEFAULT 1, compacted INTEGER DEFAULT 0,
	tool_calls TEXT, tool_name TEXT, tool_call_id TEXT, display_metadata TEXT
);`
	makeHermesDB(t, dbPath, schema, []string{
		`INSERT INTO sessions (id, session_key, title, cwd) VALUES ('h-comp', 'cli', 'compressed one', '/w')`,
		// the original of a protected head row, archived by compression
		`INSERT INTO messages (id, session_id, role, content, timestamp, active, compacted) VALUES (1, 'h-comp', 'user', 'first ask', 100, 0, 1)`,
		// an answer compression folded into a summary: archived, still shown
		`INSERT INTO messages (id, session_id, role, content, finish_reason, timestamp, active, compacted) VALUES (2, 'h-comp', 'assistant', 'old answer', 'stop', 200, 0, 1)`,
		// rewound away: hidden
		`INSERT INTO messages (id, session_id, role, content, timestamp, active, compacted) VALUES (3, 'h-comp', 'assistant', 'rewound away', 300, 0, 0)`,
		// the live copy of row 1 compression re-inserted, same words and time
		`INSERT INTO messages (id, session_id, role, content, timestamp, active, compacted) VALUES (4, 'h-comp', 'user', 'first ask', 100, 1, 0)`,
		// a call and its result, paired by tool_call_id
		`INSERT INTO messages (id, session_id, role, content, timestamp, active, tool_calls) VALUES (5, 'h-comp', 'assistant', '', 400, 1, '[{"id":"call_1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"ls\"}"}}]')`,
		`INSERT INTO messages (id, session_id, role, content, timestamp, active, tool_name, tool_call_id) VALUES (6, 'h-comp', 'tool', 'a.txt', 401, 1, 'terminal', 'call_1')`,
		// what only the model sees
		`INSERT INTO messages (id, session_id, role, content, timestamp, active, display_metadata) VALUES (7, 'h-comp', 'user', 'merged context', 402, 1, '{"model_only": true}')`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, timestamp, active) VALUES (8, 'h-comp', 'assistant', 'the end', 'stop', 500, 1)`,
	})

	msgs := hermesSQLiteMessages(dbPath, "h-comp", MessageQuery{Limit: 50})
	texts := []string{}
	for _, m := range msgs {
		for _, block := range m["content"].([]map[string]any) {
			switch block["type"] {
			case "text", "toolResult":
				texts = append(texts, ToStr(block["content"]))
			case "toolCall":
				if block["id"] != "call_1" || block["name"] != "terminal" {
					t.Errorf("tool call = %v", block)
				}
			}
			if block["type"] == "toolResult" && (block["callId"] != "call_1" || block["toolName"] != "terminal") {
				t.Errorf("tool result = %v", block)
			}
		}
	}
	want := "first ask|old answer|a.txt|the end"
	if got := strings.Join(texts, "|"); got != want {
		t.Fatalf("shown rows = %q, want %q", got, want)
	}
	// The kept copy of the duplicated row is the live one
	if msgs[0]["id"] != "4" {
		t.Errorf("the live copy (id 4) must be the one shown, got id %v", msgs[0]["id"])
	}

	// The window is cut after the projection: the latest two are the pair's result and
	// the closing answer, not whatever two rows the table held last
	tail := hermesSQLiteMessages(dbPath, "h-comp", MessageQuery{Limit: 2, FromEnd: true})
	if len(tail) != 2 || tail[0]["id"] != "6" || tail[1]["id"] != "8" {
		t.Errorf("latest 2 = %v", tail)
	}

	// Final follows the same projection: the newest shown stop answer
	final := hermesSQLiteFinal(dbPath, "hermes", "h-comp", "done")
	if final == nil || final["text"] != "the end" {
		t.Fatalf("final = %v", final)
	}
}

// TestHermesSQLiteMessagesStreamWinners: the read runs in two passes — identity and the
// winning copy first (light references only), then the winners in chunks of 200 — so a
// session larger than one chunk must come back in first-occurrence order under the same
// dedupe the single-pass version had: a live copy replaces its archived original and keeps
// the original's position, a later archived copy does not displace a live one, and the
// window is still cut after the projection, not before it.
func TestHermesSQLiteMessagesStreamWinners(t *testing.T) {
	dbPath := newHermesFixture(t)
	schema := `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, message_count INTEGER,
	input_tokens INTEGER, output_tokens INTEGER,
	cache_read_tokens INTEGER, cache_write_tokens INTEGER,
	reasoning_tokens INTEGER, estimated_cost_usd REAL,
	session_key TEXT, display_name TEXT, source TEXT, model TEXT,
	started_at REAL, ended_at REAL, title TEXT, cwd TEXT, archived INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE messages (
	id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT,
	finish_reason TEXT, reasoning TEXT, timestamp REAL,
	active INTEGER DEFAULT 1, compacted INTEGER DEFAULT 0,
	tool_calls TEXT, tool_name TEXT, tool_call_id TEXT, display_metadata TEXT
);`
	statements := []string{
		`INSERT INTO sessions (id, session_key, title, cwd) VALUES ('h-big', 'cli', 'big one', '/w')`,
	}
	for i := 1; i <= 250; i++ {
		active, compacted := 1, 0
		if i == 1 {
			active, compacted = 0, 1 // the archived original the live copy replaces
		}
		statements = append(statements, fmt.Sprintf(
			`INSERT INTO messages (id, session_id, role, content, timestamp, active, compacted) VALUES (%d, 'h-big', 'user', 'note %d', %d, %d, %d)`,
			i, i, 1000+i, active, compacted))
	}
	statements = append(statements,
		// an archived re-insert of note 5 while note 5 itself is live: the live copy stays
		`INSERT INTO messages (id, session_id, role, content, timestamp, active, compacted) VALUES (251, 'h-big', 'user', 'note 5', 1005, 0, 1)`,
		// the live copy of note 1 replaces the archived original, at the original's position
		`INSERT INTO messages (id, session_id, role, content, timestamp, active, compacted) VALUES (260, 'h-big', 'user', 'note 1', 1001, 1, 0)`,
	)
	makeHermesDB(t, dbPath, schema, statements)

	msgs := hermesSQLiteMessages(dbPath, "h-big", MessageQuery{Limit: 1000})
	if len(msgs) != 250 {
		t.Fatalf("250 shown messages expected, got %d", len(msgs))
	}
	if msgs[0]["id"] != "260" {
		t.Errorf("the live copy of note 1 must be shown at the first position, got id %v", msgs[0]["id"])
	}
	if text := msgs[0]["content"].([]map[string]any)[0]["content"]; text != "note 1" {
		t.Errorf("first message text = %v", text)
	}
	// A message beyond the first fetch chunk (200) still comes back, in place
	if msgs[205]["id"] != "206" {
		t.Errorf("message 205 = %v, want id 206", msgs[205]["id"])
	}
	// The later archived copy of note 5 does not displace the live one
	if msgs[4]["id"] != "5" {
		t.Errorf("note 5 must keep the live row (id 5), got id %v", msgs[4]["id"])
	}
	// The window is still cut after the projection: the latest two are notes 249 and 250
	tail := hermesSQLiteMessages(dbPath, "h-big", MessageQuery{Limit: 2, FromEnd: true})
	if len(tail) != 2 || tail[0]["id"] != "249" || tail[1]["id"] != "250" {
		t.Errorf("latest two = %v", tail)
	}
}

// TestHermesSQLiteMessagesSameIDDistinctRows: nothing forces the id column to be unique in
// every Hermes schema, so the second pass matches winners on id plus their identity hash —
// two rows sharing an id but not their words must both come back.
func TestHermesSQLiteMessagesSameIDDistinctRows(t *testing.T) {
	dbPath := newHermesFixture(t)
	schema := `
CREATE TABLE sessions (id TEXT PRIMARY KEY, session_key TEXT, title TEXT, cwd TEXT);
CREATE TABLE messages (
	id TEXT, session_id TEXT, role TEXT, content TEXT, timestamp REAL,
	active INTEGER DEFAULT 1, compacted INTEGER DEFAULT 0
);`
	makeHermesDB(t, dbPath, schema, []string{
		`INSERT INTO sessions (id, session_key, title) VALUES ('h-dup', 'cli', 'dup')`,
		`INSERT INTO messages (id, session_id, role, content, timestamp) VALUES ('dup', 'h-dup', 'user', 'first body', 10)`,
		`INSERT INTO messages (id, session_id, role, content, timestamp) VALUES ('dup', 'h-dup', 'assistant', 'second body', 20)`,
	})
	msgs := hermesSQLiteMessages(dbPath, "h-dup", MessageQuery{Limit: 10})
	if len(msgs) != 2 {
		t.Fatalf("two rows sharing an id must both come back, got %d", len(msgs))
	}
	texts := map[string]bool{}
	for _, m := range msgs {
		texts[ToStr(m["content"].([]map[string]any)[0]["content"])] = true
	}
	if !texts["first body"] || !texts["second body"] {
		t.Fatalf("shown texts = %v", texts)
	}
}
