package source

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
	return NewOpenClawSource([]string{path})
}

func recordOf(t *testing.T, s *OpenClawSource, id string) Record {
	t.Helper()
	for _, rec := range s.List() {
		if rec.SessionID == id {
			return rec
		}
	}
	t.Fatalf("no record with id %s", id)
	return Record{}
}

func TestOpenClawList(t *testing.T) {
	s := newOpenClawFixture(t)
	records := s.List()
	if len(records) != 2 {
		t.Fatalf("2 sessions expected, got %d", len(records))
	}

	chat := recordOf(t, s, "ses_chat")
	if chat.ShortKey != "部署排障记录" {
		t.Errorf("display_name must be the title, got %q", chat.ShortKey)
	}
	if chat.Cwd != "" {
		t.Errorf("a session without a type=session event has no cwd, got %q", chat.Cwd)
	}

	cli := recordOf(t, s, "ses_cli")
	if cli.ShortKey != "set up an Nginx reverse proxy" {
		t.Errorf("without display_name the first user message is the title, got %q", cli.ShortKey)
	}
	if cli.Cwd != "/w/proj" {
		t.Errorf("cwd from the type=session event = %q", cli.Cwd)
	}
	if cli.Model != "deepseek/deepseek-chat" {
		t.Errorf("model = %q", cli.Model)
	}
	if cli.Status != "running" {
		t.Errorf("the table's own status must pass through, got %q", cli.Status)
	}
}

func TestOpenClawMessages(t *testing.T) {
	s := newOpenClawFixture(t)
	msgs := s.Messages(recordOf(t, s, "ses_cli"), MessageQuery{Limit: 100})
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
	q := SearchQuery{Needle: "proxy_pass", Lowered: []byte("proxy_pass"), Limit: 10, PerSession: 5}

	hits := s.Search(context.Background(), rec, q)
	if len(hits) == 0 {
		t.Fatal("the assistant text must match")
	}
	for _, hit := range hits {
		if sn := hit["snippet"].(string); !strings.Contains(sn, "proxy_pass") {
			t.Errorf("snippet does not contain the Needle: %q", sn)
		}
	}
}

// TestOpenClawToolResultStatus: the result message pairs with its call by toolCallId and
// carries isError, which is the whole of what OpenClaw records about how a tool went
func TestOpenClawToolResultStatus(t *testing.T) {
	s := newOpenClawFixture(t)
	msgs := s.Messages(recordOf(t, s, "ses_cli"), MessageQuery{Limit: 20})
	call := msgs[1]["content"].([]map[string]any)[0]
	if call["id"] != "c1" {
		t.Fatalf("the call must carry its id: %v", call)
	}
	res := msgs[2]["content"].([]map[string]any)[0]
	if res["callId"] != "c1" || res["status"] != StatusOK {
		t.Fatalf("the result must pair with the call and say it went fine: %v", res)
	}

	path := filepath.Join(t.TempDir(), "openclaw-agent.sqlite")
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{openClawSchema,
		`INSERT INTO session_windows VALUES ('ses_err', 'agent:main:main', 'done', 1, 2, 2, 2, 'p', 'm', '')`,
		`INSERT INTO transcript_events VALUES ('ses_err', 1, '{"type":"message","timestamp":"2026-10-01T10:00:00Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"c2","name":"bash","arguments":{"command":"false"}}]}}', 1)`,
		`INSERT INTO transcript_events VALUES ('ses_err', 2, '{"type":"message","timestamp":"2026-10-01T10:00:01Z","message":{"role":"toolResult","toolCallId":"c2","toolName":"bash","isError":true,"content":[{"type":"text","text":"exit 1"}]}}', 2)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()
	errSource := NewOpenClawSource([]string{path})
	msgs = errSource.Messages(recordOf(t, errSource, "ses_err"), MessageQuery{Limit: 20})
	res = msgs[1]["content"].([]map[string]any)[0]
	if res["callId"] != "c2" || res["status"] != StatusError || res["content"] != "exit 1" {
		t.Fatalf("a failed tool = %v", res)
	}
}
