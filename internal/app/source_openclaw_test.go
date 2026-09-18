package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const openClawSchema = `
CREATE TABLE session_windows (
	session_id TEXT PRIMARY KEY, session_key TEXT NOT NULL,
	status TEXT, started_at INTEGER, ended_at INTEGER,
	updated_at INTEGER NOT NULL, transcript_updated_at INTEGER,
	model_provider TEXT, model TEXT, display_name TEXT);
CREATE TABLE transcript_events (
	session_id TEXT NOT NULL, seq INTEGER NOT NULL,
	event_json TEXT NOT NULL, created_at INTEGER NOT NULL,
	PRIMARY KEY (session_id, seq));`

// newOpenClawFixture builds an agent database with rows copied from a real 2026.9
// install: the event JSON shapes, the toolCall/toolResult message split and the unix
// millisecond times included.
func newOpenClawFixture(t *testing.T) *OpenClawSource {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openclaw-agent.sqlite")
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	statements := append([]string{openClawSchema},
		// display_name set: a chat-channel session
		`INSERT INTO session_windows VALUES ('ses_chat', 'agent:main:main', 'done',
			1789704223504, 1789704262019, 1789704262019, 1789704262019,
			'deepseek', 'deepseek-chat', '部署排障记录')`,
		// display_name empty: a CLI run, the first user message is the title
		`INSERT INTO session_windows VALUES ('ses_cli', 'agent:main:main', 'running',
			1789704223504, NULL, 1789704227713, 1789704227688,
			'deepseek', 'deepseek-chat', '')`,

		`INSERT INTO transcript_events VALUES ('ses_cli', 1,
			'{"type":"session","id":"ses_cli","cwd":"/w/proj"}', 1789704223504)`,
		`INSERT INTO transcript_events VALUES ('ses_cli', 2,
			'{"type":"message","timestamp":"2026-09-18T04:03:44.021Z","message":{"role":"user","content":"set up an Nginx reverse proxy"}}', 1789704224021)`,
		`INSERT INTO transcript_events VALUES ('ses_cli', 3,
			'{"type":"message","timestamp":"2026-09-18T04:03:45.831Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls -la"}}],"stopReason":"toolUse"}}', 1789704225831)`,
		`INSERT INTO transcript_events VALUES ('ses_cli', 4,
			'{"type":"message","timestamp":"2026-09-18T04:03:45.867Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"note.txt"}]}}', 1789704225867)`,
		`INSERT INTO transcript_events VALUES ('ses_cli', 5,
			'{"type":"message","timestamp":"2026-09-18T04:03:47.000Z","message":{"role":"assistant","content":[{"type":"text","text":"proxy_pass goes like this"}],"stopReason":"stop","model":"deepseek-chat","usage":{"input":481,"output":161,"cacheRead":37248,"cacheWrite":0,"cost":{"total":0.0021}}}}', 1789704227000)`,
		// a later assistant row that only stopped to run a tool: not the final result
		`INSERT INTO transcript_events VALUES ('ses_cli', 6,
			'{"type":"message","timestamp":"2026-09-18T04:04:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"let me check one more thing"}],"stopReason":"toolUse"}}', 1789704241000)`,
	)
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("executing %q failed: %v", stmt, err)
		}
	}
	return newOpenClawSource([]string{path})
}

func recordOf(t *testing.T, s *OpenClawSource, id string) record {
	t.Helper()
	for _, rec := range s.List() {
		if rec.str("sessionId") == id {
			return rec
		}
	}
	t.Fatalf("no record with id %s", id)
	return record{}
}

func TestOpenClawList(t *testing.T) {
	s := newOpenClawFixture(t)
	records := s.List()
	if len(records) != 2 {
		t.Fatalf("2 sessions expected, got %d", len(records))
	}

	chat := recordOf(t, s, "ses_chat")
	if chat.str("shortKey") != "部署排障记录" {
		t.Errorf("display_name must be the title, got %q", chat.str("shortKey"))
	}
	if chat.str("cwd") != "" {
		t.Errorf("a session without a type=session event has no cwd, got %q", chat.str("cwd"))
	}

	cli := recordOf(t, s, "ses_cli")
	if cli.str("shortKey") != "set up an Nginx reverse proxy" {
		t.Errorf("without display_name the first user message is the title, got %q", cli.str("shortKey"))
	}
	if cli.str("cwd") != "/w/proj" {
		t.Errorf("cwd from the type=session event = %q", cli.str("cwd"))
	}
	if cli.str("model") != "deepseek/deepseek-chat" {
		t.Errorf("model = %q", cli.str("model"))
	}
	if cli.str("status") != "running" {
		t.Errorf("the table's own status must pass through, got %q", cli.str("status"))
	}
}

func TestOpenClawMessages(t *testing.T) {
	s := newOpenClawFixture(t)
	msgs := s.Messages(recordOf(t, s, "ses_cli"), messageQuery{limit: 100})
	if len(msgs) != 5 {
		t.Fatalf("5 messages expected, got %d", len(msgs))
	}

	if msgs[0]["role"] != "user" {
		t.Errorf("first message role = %v", msgs[0]["role"])
	}
	call := msgs[1]["content"].([]map[string]any)[0]
	if call["type"] != "toolCall" || call["name"] != "bash" {
		t.Errorf("toolCall = %v", call)
	}
	if args := call["arguments"].(map[string]any); args["command"] != "ls -la" {
		t.Errorf("tool arguments = %v", args)
	}

	// A tool result is its own message with toolName riding along
	res := msgs[2]
	if res["role"] != "toolResult" {
		t.Errorf("third message role = %v", res["role"])
	}
	block := res["content"].([]map[string]any)[0]
	if block["type"] != "toolResult" || block["toolName"] != "bash" || block["content"] != "note.txt" {
		t.Errorf("toolResult block = %v", block)
	}

	if msgs[3]["role"] != "assistant" {
		t.Errorf("last message role = %v", msgs[3]["role"])
	}
	if ts := msgs[0]["timestamp"]; ts != "2026-09-18T04:03:44.021Z" {
		t.Errorf("timestamp must be the event-level ISO string, got %v", ts)
	}
}

func TestOpenClawFinal(t *testing.T) {
	s := newOpenClawFixture(t)
	final := s.Final(recordOf(t, s, "ses_cli"))
	if final == nil {
		t.Fatal("no final result")
	}
	// seq 6 is newer but stopped to run a tool; the result is seq 5
	if final["stopReason"] != "stop" || final["isFinal"] != true {
		t.Errorf("stopReason/isFinal = %v/%v", final["stopReason"], final["isFinal"])
	}
	if final["text"] != "proxy_pass goes like this" {
		t.Errorf("text = %v", final["text"])
	}
	usage := final["usage"].(map[string]any)
	if usage["inputTokens"] != int64(481) || usage["cacheReadTokens"] != int64(37248) {
		t.Errorf("usage = %v", usage)
	}
	if usage["estimatedCostUsd"] != 0.0021 {
		t.Errorf("cost = %v", usage["estimatedCostUsd"])
	}
}

func TestOpenClawSearch(t *testing.T) {
	s := newOpenClawFixture(t)
	rec := recordOf(t, s, "ses_cli")
	q := searchQuery{needle: "proxy_pass", lowered: []byte("proxy_pass"), limit: 10, perSession: 5}

	hits := s.Search(context.Background(), rec, q)
	if len(hits) == 0 {
		t.Fatal("the assistant text must match")
	}
	for _, hit := range hits {
		if sn := hit["snippet"].(string); !strings.Contains(sn, "proxy_pass") {
			t.Errorf("snippet does not contain the needle: %q", sn)
		}
	}
}
