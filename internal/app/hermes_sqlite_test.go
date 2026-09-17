package app

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
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
	started_at REAL, ended_at REAL
);
CREATE TABLE messages (
	id TEXT, session_id TEXT, role TEXT, content TEXT,
	finish_reason TEXT, reasoning TEXT, timestamp REAL,
	active INTEGER DEFAULT 1
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
