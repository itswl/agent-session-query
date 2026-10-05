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

// openCodeV2Schema is the 2.x layout reduced to the columns the source reads: a session
// row with its title, directory and times; one message row per message with the parts
// inline under data.
const openCodeV2Schema = `
CREATE TABLE session_v2 (
	id TEXT PRIMARY KEY, directory TEXT NOT NULL, title TEXT,
	time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
CREATE TABLE session_message (
	id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL, seq INTEGER NOT NULL,
	time_created INTEGER NOT NULL, data TEXT NOT NULL);`

func newOpenCodeV2Fixture(t *testing.T, withV1 bool) *OpenCodeSource {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{openCodeV2Schema,
		`INSERT INTO session_v2 VALUES ('ses_v2', '/w/new', 'New layout', 1790000000000, 1790000100000)`,
		`INSERT INTO session_message VALUES ('v2m1', 'ses_v2', 'user', 1, 1790000000000, '{"text":"hello v2"}')`,
		`INSERT INTO session_message VALUES ('v2m2', 'ses_v2', 'assistant', 2, 1790000050000,
			'{"role":"assistant","modelID":"gpt-x","providerID":"openai","finish":"stop","tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":1,"write":0}},"cost":0.01,"content":[{"type":"reasoning","text":"think"},{"type":"tool","name":"bash","id":"c9","state":{"status":"error","input":{"command":"false"},"error":"exit 1","metadata":{"exit":1},"time":{"start":1000,"end":1500}}},{"type":"text","text":"done v2"}]}')`,
	}
	if withV1 {
		statements = append(statements, openCodeSchema,
			`INSERT INTO session VALUES ('ses_old', '/w/old', 'Old layout', 'old', NULL, 1780000000000, 1780000100000, NULL, 0, 0, 0, 0, 0, 0)`,
			`INSERT INTO message VALUES ('msg_o1', 'ses_old', 1780000000000, 1780000000000, '{"role":"user","time":{"created":1780000000000}}')`,
			`INSERT INTO part VALUES ('po1', 'msg_o1', 'ses_old', 1, 1, '{"type":"text","text":"hello v1"}')`,
			// a migrated copy keeps its id: it lives in session_v2 now and must not list twice
			`INSERT INTO session VALUES ('ses_v2', '/w/new', 'New layout', 'new', NULL, 1790000000000, 1790000100000, NULL, 0, 0, 0, 0, 0, 0)`,
		)
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("executing %q failed: %v", stmt, err)
		}
	}
	return newOpenCodeSource(path)
}

// TestOpenCodeV2: 2.x moved sessions to session_v2 and messages to session_message and
// stopped writing the V1 tables, so a 2.x install listed nothing here and said nothing —
// an empty source and a moved one answer alike.
func TestOpenCodeV2(t *testing.T) {
	s := newOpenCodeV2Fixture(t, false)
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}
	rec := list[0]
	if rec.str("sessionId") != "ses_v2" || rec.str("cwd") != "/w/new" || rec.str("shortKey") != "New layout" {
		t.Fatalf("record = %v", rec.fields)
	}
	if rec.get("messageCount") != int64(2) {
		t.Errorf("messageCount = %v", rec.get("messageCount"))
	}

	msgs := s.Messages(rec, messageQuery{limit: 10})
	if len(msgs) != 2 || msgs[0]["role"] != "user" || msgs[0]["content"].([]map[string]any)[0]["content"] != "hello v2" {
		t.Fatalf("messages = %v", msgs)
	}
	blocks := msgs[1]["content"].([]map[string]any)
	kinds := []string{}
	for _, b := range blocks {
		kinds = append(kinds, toStr(b["type"]))
	}
	if strings.Join(kinds, ",") != "thinking,toolCall,toolResult,text" {
		t.Fatalf("assistant blocks = %v", kinds)
	}
	// a 2.x tool item names its tool as name and its call as id
	if blocks[1]["id"] != "c9" || blocks[1]["name"] != "bash" {
		t.Errorf("call = %v", blocks[1])
	}
	if r := blocks[2]; r["callId"] != "c9" || r["status"] != statusError || r["exitCode"] != 1 || r["durationMs"] != int64(500) || r["content"] != "error: exit 1" {
		t.Errorf("result = %v", r)
	}

	final := s.Final(rec)
	if final["text"] != "done v2" || final["isFinal"] != true || final["model"] != "openai/gpt-x" || final["messageCount"] != 2 {
		t.Fatalf("final = %v", final)
	}
	usage := final["usage"].(map[string]any)
	if usage["inputTokens"] != int64(10) || usage["cacheReadTokens"] != int64(1) || usage["estimatedCostUsd"] != 0.01 {
		t.Errorf("usage = %v", usage)
	}

	hits := s.Search(context.Background(), rec, searchQuery{needle: "done", lowered: []byte("done"), limit: 10, perSession: 3})
	if len(hits) != 1 || hits[0]["role"] != "assistant" || !strings.Contains(toStr(hits[0]["snippet"]), "done v2") {
		t.Fatalf("search = %v", hits)
	}
	none := s.Search(context.Background(), rec, searchQuery{needle: "c9", lowered: []byte("c9"), limit: 10, perSession: 3})
	if len(none) != 0 {
		t.Errorf("a hit in a call id is not a body hit: %v", none)
	}
}

// TestOpenCodeMixedSchemas: a database that saw 2.x and then 1.x again holds both layouts;
// the V1 sessions that never migrated are listed beside the V2 ones, a migrated one once.
func TestOpenCodeMixedSchemas(t *testing.T) {
	s := newOpenCodeV2Fixture(t, true)
	list := s.List()
	ids := map[string]record{}
	for _, rec := range list {
		ids[rec.str("sessionId")] = rec
	}
	if len(list) != 2 || ids["ses_v2"].fields == nil || ids["ses_old"].fields == nil {
		t.Fatalf("list = %v", list)
	}
	old := s.Messages(ids["ses_old"], messageQuery{limit: 10})
	if len(old) != 1 || old[0]["content"].([]map[string]any)[0]["content"] != "hello v1" {
		t.Fatalf("the V1 session must still read through the V1 tables: %v", old)
	}
	if s.Final(ids["ses_v2"])["text"] != "done v2" {
		t.Error("the migrated session reads through V2")
	}
}
