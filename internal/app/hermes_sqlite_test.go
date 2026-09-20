package app

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
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
	final := source.Final(newRecord(map[string]any{
		"source": "hermes", "sessionId": "h-elsewhere", "status": "done",
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
	if r.str("sessionId") != "20260814_002606_1a7908" || r.str("key") != "20260814_002606_1a7908" {
		t.Fatalf("record = %v", r.fields)
	}
	if r.str("platform") != "desktop" || r.str("model") != "deepseek-v4-pro" || r.str("displayName") != "desktop session" {
		t.Fatalf("record = %v", r.fields)
	}
	if r.get("totalTokens") != float64(1720) || r.get("estimatedCostUsd") != 0.0123 {
		t.Fatalf("record = %v", r.fields)
	}
	if r.get("hasFile") != false || r.get("file") != nil {
		t.Fatalf("record = %v", r.fields)
	}
	// updatedAt takes the time of the last active message (epoch seconds to UTC ISO)
	if r.str("createdAt") != "2026-08-13T16:26:06" || r.str("updatedAt") != "2026-08-13T16:26:25" {
		t.Fatalf("createdAt/updatedAt = %v / %v", r.str("createdAt"), r.str("updatedAt"))
	}

	msgs := source.Messages(r, messageQuery{limit: 50})
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
	if n := records[0].get("messageCount"); n != int64(7) {
		t.Errorf("messageCount = %v, want 7 (sessions.message_count)", n)
	}
	live := records[0] // newest first
	if live.str("shortKey") != "排查接口 502" {
		t.Errorf("title must be the display name: %q", live.str("shortKey"))
	}
	if live.str("cwd") != "/w/proj" {
		t.Errorf("cwd = %q", live.str("cwd"))
	}
	named := records[1]
	if named.str("shortKey") != "older session" {
		t.Errorf("display_name is the fallback: %q", named.str("shortKey"))
	}

	msgs := hermesSQLiteMessages(dbPath, "h-live", messageQuery{limit: 10})
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
	if len(records) != 1 || records[0].str("sessionId") != "h-visible" {
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
	if len(records) != 1 || records[0].str("sessionId") != "h-one" {
		t.Fatalf("an unfiltered list must still return the session: %v", records)
	}
}

// TestHermesListFailureIsReported: a state.db this cannot read must not come back as a
// source with nothing in it. Both the session list and the health check have to say so,
// because otherwise the answer to "why is hermes empty" is indistinguishable from "you have
// no hermes sessions" — which is exactly how a schema that moved under this went unnoticed.
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

	source := newJsonMapSource(hermesDef(defaultHome()))
	sources := []SessionSource{source}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "hermes", sources: sources, api: newSessionQueryAPI(sources, 0), maxConnections: 10,
	}))
	t.Cleanup(srv.Close)

	// /health is unauthenticated, so it reports what the last scan hit rather than scanning
	// for itself: with nothing scanned yet it has nothing to report
	if _, body := get(t, srv.URL+"/health", ""); body["warnings"] != nil {
		t.Fatalf("/health must not scan the sources to answer: %v", body["warnings"])
	}
	if err := source.ListError(); err != nil {
		t.Fatalf("nothing has listed anything yet: %v", err)
	}

	if code, body := get(t, srv.URL+"/sessions", ""); code != 200 {
		t.Fatalf("/sessions = %d %v", code, body)
	} else if warnings, ok := body["warnings"].([]any); !ok || len(warnings) != 1 {
		t.Fatalf("/sessions must carry the source failure: %v", body)
	} else if first := warnings[0].(map[string]any); first["source"] != "hermes" ||
		!strings.Contains(toStr(first["error"]), "no such column") {
		t.Fatalf("warning = %v", first)
	}

	// The failure is on record now, so the source reports it even outside the response
	err := source.ListError()
	if err == nil {
		t.Fatal("the failure must be reported, not returned as an empty source")
	}
	if !strings.Contains(err.Error(), "no such column") {
		t.Fatalf("the underlying error should reach the caller: %v", err)
	}

	// And the health check, which cannot scan, reports it from what was recorded
	_, health := get(t, srv.URL+"/health", "")
	warnings, ok := health["warnings"].([]any)
	if !ok || len(warnings) != 1 || warnings[0].(map[string]any)["source"] != "hermes" {
		t.Fatalf("/health after a failed list = %v", health)
	}

	// Every scan replaces the last one's verdict: with the database it used to fail on gone,
	// the failure must go with it rather than sit there being reported forever
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	source.List()
	if err := source.ListError(); err != nil {
		t.Fatalf("a scan that no longer touches the database still reports its old failure: %v", err)
	}
}
