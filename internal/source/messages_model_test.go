package source

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assistantModels lists the model each assistant message names, in order
func assistantModels(msgs []map[string]any) []string {
	var out []string
	for _, m := range msgs {
		if m["role"] == "assistant" {
			out = append(out, ToStr(m["model"]))
		}
	}
	return out
}

// TestMessagesCarryModel: every source whose transcript says which model answered puts it
// on the assistant message. The export names a generation by it and a backend prices it
// by it; the session's last model is only a fallback when a row does not say.
func TestMessagesCarryModel(t *testing.T) {
	t.Run("codex: the model of each turn's turn_context", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "2026", "10", "01", "rollout-2026-10-01T10-00-00-abc.jsonl"),
			`{"type":"session_meta","payload":{"session_id":"codex-m","cwd":"/p","cli_version":"0.154.0"}}`,
			`{"type":"turn_context","payload":{"turn_id":"t1","model":"gpt-a","cwd":"/p"}}`,
			`{"type":"response_item","timestamp":"2026-10-01T10:00:01Z","payload":{"type":"message","role":"user","id":"u1","content":[{"text":"one"}]}}`,
			`{"type":"response_item","timestamp":"2026-10-01T10:00:02Z","payload":{"type":"message","role":"assistant","id":"a1","content":[{"text":"first"}]}}`,
			`{"type":"token_usage_record","payload":{"usage":{"input_tokens":7,"output_tokens":2}}}`,
			`{"type":"turn_context","payload":{"turn_id":"t2","model":"gpt-b","cwd":"/p"}}`,
			`{"type":"response_item","timestamp":"2026-10-01T10:01:01Z","payload":{"type":"message","role":"user","id":"u2","content":[{"text":"two"}]}}`,
			`{"type":"response_item","timestamp":"2026-10-01T10:01:02Z","payload":{"type":"message","role":"assistant","id":"a2","content":[{"text":"second"}]}}`,
		)
		s := NewCodexSource(root)
		msgs := s.Messages(s.List()[0], MessageQuery{Limit: 50})
		if got := strings.Join(assistantModels(msgs), ","); got != "gpt-a,gpt-b" {
			t.Fatalf("models = %q, want gpt-a,gpt-b (the model switched between turns)", got)
		}
	})

	t.Run("gemini: the entry's model", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "projA", "chats", "session-2026-09-13T12-50-41.jsonl"),
			`{"sessionId":"g-m","startTime":"2026-09-13T12:50:41Z","lastUpdated":"2026-09-13T12:55:00Z"}`,
			`{"$set":{"messages":[{"type":"user","id":"gu1","timestamp":"t1","content":[{"text":"hey"}]}]}}`,
			`{"type":"gemini","id":"gg1","timestamp":"t2","model":"gemini-2.5","tokens":{"input":3},"content":"hello"}`,
		)
		s := NewGeminiSource(root)
		if got := assistantModels(s.Messages(s.List()[0], MessageQuery{Limit: 50})); len(got) != 1 || got[0] != "gemini-2.5" {
			t.Fatalf("models = %v", got)
		}
	})

	t.Run("pi: the message's model", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "proj", "2026-09-13T13-04-12-594Z_aaaa.jsonl"),
			`{"type":"session","id":"pi-m","cwd":"/p"}`,
			`{"type":"message","id":"m1","timestamp":"2026-09-13T13:05:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
			`{"type":"message","id":"m2","timestamp":"2026-09-13T13:05:05Z","message":{"role":"assistant","model":"glm-x","usage":{"input_tokens":10},"content":[{"type":"text","text":"hi"}]}}`,
		)
		s := NewPiSource(root)
		if got := assistantModels(s.Messages(s.List()[0], MessageQuery{Limit: 50})); len(got) != 1 || got[0] != "glm-x" {
			t.Fatalf("models = %v", got)
		}
	})

	t.Run("opencode: provider/model of the message", func(t *testing.T) {
		s := newOpenCodeFixture(t)
		// msg_a1 names deepseek/deepseek-flash; msg_a2 names nothing and claims nothing
		got := assistantModels(s.Messages(s.List()[0], MessageQuery{Limit: 100}))
		if len(got) != 2 || got[0] != "deepseek/deepseek-flash" || got[1] != "" {
			t.Fatalf("models = %q, want [deepseek/deepseek-flash, \"\"]", got)
		}
	})

	t.Run("openclaw: the message's model", func(t *testing.T) {
		s := newOpenClawFixture(t)
		found := false
		for _, model := range assistantModels(s.Messages(recordOf(t, s, "ses_cli"), MessageQuery{Limit: 100})) {
			if model == "deepseek-chat" {
				found = true
			}
		}
		if !found {
			t.Fatal("no assistant message names the fixture's deepseek-chat")
		}
	})

	t.Run("grok history: the model_id of the assistant lines", func(t *testing.T) {
		root, summary := grokFixture(t)
		dir := filepath.Dir(summary)
		if err := os.Remove(filepath.Join(dir, "updates.jsonl")); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "chat_history.jsonl"),
			`{"type":"user","content":[{"type":"text","text":"list the files"}]}`,
			`{"type":"assistant","content":"one file.","model_id":"gemini-3.8-flash"}`,
		)
		s := NewGrokSource(root)
		if got := assistantModels(s.Messages(s.List()[0], MessageQuery{Limit: 50})); len(got) != 1 || got[0] != "gemini-3.8-flash" {
			t.Fatalf("models = %v", got)
		}
	})
}

// TestGrokTurnUsageOnMessage: the ACP stream's turn_completed carries what the turn cost;
// the turn's assistant message carries it, so a round can say what it cost.
func TestGrokTurnUsageOnMessage(t *testing.T) {
	root, _ := grokFixture(t)
	s := NewGrokSource(root)
	var usage map[string]any
	for _, m := range s.Messages(s.List()[0], MessageQuery{Limit: 50}) {
		if m["role"] == "assistant" && m["usage"] != nil {
			if usage != nil {
				t.Fatal("the turn's usage landed on more than one message")
			}
			usage = m["usage"].(map[string]any)
		}
	}
	if usage == nil || ToStr(usage["inputTokens"]) != "15667" || ToStr(usage["outputTokens"]) != "53" {
		t.Fatalf("assistant usage = %v, want the turn's 15667 in / 53 out", usage)
	}
}

// TestHermesListedSessionTakesStateDBModel: a session sessions.json lists names no model
// there; its state.db row does, and the record carries it — Hermes keeps no model per
// message, so it is the only name the export can give the session's generations.
func TestHermesListedSessionTakesStateDBModel(t *testing.T) {
	dbPath := newHermesFixture(t)
	write(t, filepath.Join(filepath.Dir(dbPath), "sessions", "sessions.json"),
		`{"hook:webhook:only":{"session_id":"h-model","updated_at":"2026-09-13T10:10:00Z"}}`,
	)
	makeHermesDB(t, dbPath, hermesSchema, []string{
		`INSERT INTO sessions (id, model) VALUES ('h-model', 'glm-x')`,
	})
	list := newJsonMapSource(hermesDef(defaultHome())).List()
	if len(list) != 1 || list[0].SessionID != "h-model" || list[0].Model != "glm-x" {
		t.Fatalf("list = %+v, want h-model with state.db's model glm-x", list)
	}
}

// TestOpenCodeTiming: OpenCode measures a response — time.created when the call was made,
// time.completed when it ended, and a 2.x part's time.created when it began to arrive — and
// the assistant message carries them as durationMs and firstTokenMs.
func TestOpenCodeTiming(t *testing.T) {
	v1 := newOpenCodeFixture(t) // msg_a1: created 1789697128725, completed 1789697131296
	for _, m := range v1.Messages(v1.List()[0], MessageQuery{Limit: 100}) {
		if m["role"] == "assistant" && m["model"] != nil {
			if d, _ := ToFloat(m["durationMs"]); d != 2571 {
				t.Fatalf("1.x durationMs = %v, want 2571", m["durationMs"])
			}
		}
	}

	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{openCodeV2Schema,
		`INSERT INTO session_v2 VALUES ('ses_t', '/w/t', 'Timed', 1790000000000, 1790000100000)`,
		`INSERT INTO session_message VALUES ('t1', 'ses_t', 'user', 1, 1790000000000, '{"text":"go"}')`,
		`INSERT INTO session_message VALUES ('t2', 'ses_t', 'assistant', 2, 1790000010000,
			'{"model":{"id":"deepseek-flash","providerID":"personal"},"time":{"created":1790000010000,"completed":1790000014800},
			  "content":[{"type":"reasoning","text":"hm","time":{"created":1790000011080,"completed":1790000014790}},{"type":"text","text":"done"}]}')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%q: %v", stmt, err)
		}
	}
	db.Close()
	s := NewOpenCodeSource(path)
	for _, m := range s.Messages(s.List()[0], MessageQuery{Limit: 10}) {
		if m["role"] != "assistant" {
			continue
		}
		d, _ := ToFloat(m["durationMs"])
		first, _ := ToFloat(m["firstTokenMs"])
		if m["model"] != "personal/deepseek-flash" || d != 4800 || first != 1080 {
			t.Fatalf("2.x assistant = model %v, durationMs %v, firstTokenMs %v; want personal/deepseek-flash, 4800, 1080", m["model"], d, first)
		}
		return
	}
	t.Fatal("no assistant message")
}
