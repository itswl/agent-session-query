package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// openCodeSchema is the real schema reduced to the columns the source reads.
const openCodeSchema = `
CREATE TABLE session (
	id TEXT PRIMARY KEY, directory TEXT NOT NULL, title TEXT NOT NULL,
	slug TEXT NOT NULL, model TEXT, time_created INTEGER NOT NULL,
	time_updated INTEGER NOT NULL, time_archived INTEGER,
	cost REAL NOT NULL DEFAULT 0,
	tokens_input INTEGER NOT NULL DEFAULT 0, tokens_output INTEGER NOT NULL DEFAULT 0,
	tokens_reasoning INTEGER NOT NULL DEFAULT 0,
	tokens_cache_read INTEGER NOT NULL DEFAULT 0, tokens_cache_write INTEGER NOT NULL DEFAULT 0);
CREATE TABLE message (
	id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
	time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE part (
	id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL,
	time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);`

// newOpenCodeFixture builds a database with rows copied from the real thing — ids,
// JSON payloads and the unix-millisecond times included.
func newOpenCodeFixture(t *testing.T) *OpenCodeSource {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	statements := append([]string{openCodeSchema},
		`INSERT INTO session VALUES ('ses_live', '/w/proj', '询问模型身份', 'ask-model',
			'{"id":"deepseek-flash","providerID":"deepseek"}',
			1789697128684, 1789697262019, NULL, 0.0031, 16111, 533, 449, 31616, 0)`,
		`INSERT INTO session VALUES ('ses_archived', '/w/old', 'old one', 'old',
			NULL, 1789000000000, 1789000000000, 1789000100000, 0, 0, 0, 0, 0, 0)`,

		`INSERT INTO message VALUES ('msg_u1', 'ses_live', 1789697128704, 1789697128704,
			'{"role":"user","time":{"created":1789697128704}}')`,
		`INSERT INTO message VALUES ('msg_a1', 'ses_live', 1789697128725, 1789697131296,
			'{"role":"assistant","modelID":"deepseek-flash","providerID":"deepseek","finish":"stop","time":{"created":1789697128725,"completed":1789697131296}}')`,
		// an aborted assistant turn: no finish, so it is not the final result
		`INSERT INTO message VALUES ('msg_a2', 'ses_live', 1789697130000, 1789697130000,
			'{"role":"assistant","time":{"created":1789697130000}}')`,

		`INSERT INTO part VALUES ('p1', 'msg_u1', 'ses_live', 1, 1,
			'{"type":"text","text":"set up an Nginx reverse proxy"}')`,
		`INSERT INTO part VALUES ('p2', 'msg_a1', 'ses_live', 2, 2,
			'{"type":"reasoning","text":"nginx needs the proxy_pass directive"}')`,
		`INSERT INTO part VALUES ('p3', 'msg_a1', 'ses_live', 3, 3,
			'{"type":"text","text":"proxy_pass goes like this"}')`,
		`INSERT INTO part VALUES ('p4', 'msg_a1', 'ses_live', 4, 4,
			'{"type":"step-start","snapshot":"abc"}')`,
		`INSERT INTO part VALUES ('p5', 'msg_a1', 'ses_live', 5, 5,
			'{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"ls -la"},"output":"note.txt"}}')`,
		`INSERT INTO part VALUES ('p6', 'msg_a1', 'ses_live', 6, 6,
			'{"type":"tool","tool":"grep","callID":"c2","state":{"status":"error","error":"no such file"}}')`,
		`INSERT INTO part VALUES ('p7', 'msg_a1', 'ses_live', 7, 7,
			'{"type":"step-finish","reason":"stop","snapshot":"abc"}')`,
	)
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("executing %q failed: %v", stmt, err)
		}
	}
	return newOpenCodeSource(path)
}

func TestOpenCodeList(t *testing.T) {
	s := newOpenCodeFixture(t)
	records := s.List()
	if len(records) != 1 {
		t.Fatalf("the archived session must be skipped: %d records", len(records))
	}
	rec := records[0]
	for field, want := range map[string]string{
		"sessionId": "ses_live",
		"shortKey":  "询问模型身份", // the title, which is what opencode itself shows
		"cwd":       "/w/proj",
		"model":     "deepseek/deepseek-flash",
		"updatedAt": "2026-09-18T02:07:42",
	} {
		if got := rec.str(field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	if rec.truthy("hasFile") {
		t.Error("an all-SQLite source has no session file")
	}
}

func TestOpenCodeMessages(t *testing.T) {
	s := newOpenCodeFixture(t)
	msgs := s.Messages(s.List()[0], messageQuery{limit: 100})
	if len(msgs) != 3 {
		t.Fatalf("3 messages expected, got %d", len(msgs))
	}

	blocks := msgs[1]["content"].([]map[string]any)
	kinds := []string{}
	for _, b := range blocks {
		kinds = append(kinds, b["type"].(string))
	}
	// step boundaries carry no content and must not appear; a tool part becomes the
	// call followed by its result; the errored tool's result carries the error
	want := []string{"thinking", "text", "toolCall", "toolResult", "toolCall", "toolResult"}
	if len(kinds) != len(want) {
		t.Fatalf("block kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("block kinds = %v, want %v", kinds, want)
		}
	}
	call := blocks[2]
	if call["name"] != "bash" {
		t.Errorf("tool name = %v", call["name"])
	}
	if args := call["arguments"].(map[string]any); args["command"] != "ls -la" {
		t.Errorf("tool arguments = %v", args)
	}
	if res := blocks[3]["content"].(string); res != "note.txt" {
		t.Errorf("tool output = %q", res)
	}
	if res := blocks[5]["content"].(string); res != "error: no such file" {
		t.Errorf("an errored tool must surface its error, got %q", res)
	}
}

func TestOpenCodeFinal(t *testing.T) {
	s := newOpenCodeFixture(t)
	final := s.Final(s.List()[0])
	if final == nil {
		t.Fatal("no final result")
	}
	// msg_a2 is newer but has no finish; the final result is msg_a1
	if final["id"] != "msg_a1" {
		t.Errorf("final id = %v, want msg_a1", final["id"])
	}
	if final["stopReason"] != "stop" || final["isFinal"] != true {
		t.Errorf("stopReason/isFinal = %v/%v", final["stopReason"], final["isFinal"])
	}
	if final["model"] != "deepseek/deepseek-flash" {
		t.Errorf("model = %v", final["model"])
	}
	if final["text"] != "proxy_pass goes like this" {
		t.Errorf("text = %v", final["text"])
	}
	if final["thinking"] != "nginx needs the proxy_pass directive" {
		t.Errorf("thinking = %v", final["thinking"])
	}
	usage := final["usage"].(map[string]any)
	if usage["inputTokens"] != int64(16111) || usage["reasoningTokens"] != int64(449) {
		t.Errorf("usage = %v", usage)
	}
	if final["messageCount"] != 3 {
		t.Errorf("messageCount = %v", final["messageCount"])
	}
}

func TestOpenCodeSearch(t *testing.T) {
	s := newOpenCodeFixture(t)
	rec := s.List()[0]
	q := searchQuery{needle: "proxy_pass", lowered: []byte("proxy_pass"), limit: 10, perSession: 5}

	hits := s.Search(context.Background(), rec, q)
	if len(hits) != 2 { // the reasoning and the text, not the field names
		t.Fatalf("2 hits expected (thinking + text), got %d: %v", len(hits), hits)
	}
	for _, hit := range hits {
		if sn := hit["snippet"].(string); !strings.Contains(sn, "proxy_pass") {
			t.Errorf("snippet does not contain the needle: %q", sn)
		}
	}

	// A needle that only appears inside a tool's arguments is not a body hit
	q = searchQuery{needle: "ls -la", lowered: []byte("ls -la"), limit: 10, perSession: 5}
	if hits := s.Search(context.Background(), rec, q); len(hits) != 0 {
		t.Fatalf("a tool argument must not count as a body hit: %v", hits)
	}
}
