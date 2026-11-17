package hermes

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// FuzzRead reads whatever it is given as a Hermes store would hold it: as a session's
// transcript file beside the index, and in the database's columns that hold JSON or text
// the adapter reads — an assistant's tool calls, its reasoning and words, a row's display
// metadata, a tool's output, a timestamp. Nothing it reads may panic or hand back a
// session that breaks what every session holds.
func FuzzRead(f *testing.F) {
	// The tests' fixtures, cut into short transcripts: an input the fuzzer finds new is
	// shrunk a byte at a time, each try a store built and read, and a long one takes the
	// whole of the time it is given for that
	for _, rows := range [][]string{
		// The older layout's transcript, as Hermes wrote it
		{
			`{"role":"session_meta","platform":"tui","model":"glm-5"}`,
			`{"role":"user","content":"from the older layout","timestamp":"2026-09-20T10:00:01"}`,
		},
		{
			`{"role":"assistant","content":"","reasoning":"look first","timestamp":"2026-09-20T10:00:02","tool_calls":[{"id":"c1","call_id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"ls\"}"}}],"finish_reason":"tool_calls"}`,
			`{"role":"tool","tool_call_id":"c1","content":"a.txt","timestamp":"2026-09-20T10:00:03"}`,
		},
		{`{"role":"assistant","content":"one file","timestamp":"2026-09-20T10:00:04","finish_reason":"stop","usage":{"input_tokens":10,"output_tokens":3}}`},
		// What the database's columns hold: tool calls, the mark on a row for the model
		// alone, a tool's output, a time
		{`[{"id":"call_1","call_id":"call_1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"nginx -t\"}"}}]`},
		{`"{\"model_only\": true}"`},
		{`{"output":"syntax is ok","exit_code":0}`},
		{`1790000004.5`},
		// The typed rows the same reader takes, which a transcript beside the index may hold too
		{
			`{"type":"session","id":"s","cwd":"/w/proj"}`,
			`{"type":"message","timestamp":"2026-09-18T04:03:44.021Z","message":{"role":"user","content":"set up a proxy"}}`,
		},
		{`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"look"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}],"stopReason":"toolUse","usage":{"input":4,"output":2,"cost":{"total":0.1}}}}`},
		{
			`{"type":"message","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","isError":true,"content":[{"type":"text","text":"no"}],"usage":{"input":1}}}`,
			`{"type":"compaction","summary":"folded"}`,
		},
	} {
		f.Add([]byte(strings.Join(rows, "\n")))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// A seed's directory is named after it, "seed#0": a URI must escape the #
		dir := t.TempDir()
		db, err := sql.Open("sqlite", kit.FileURI(filepath.Join(dir, "state.db")))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1) // the pragmas below hold for the connection they ran on
		// A fixture needs no journal and no sync, and one transaction makes it one write: a
		// store built a commit at a time spends the fuzzer's time on the disk
		if _, err := db.Exec(`PRAGMA journal_mode = OFF; PRAGMA synchronous = OFF`); err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, stmt := range []struct {
			sql  string
			args []any
		}{
			{schema, nil},
			{`INSERT INTO sessions (id, source, model, started_at, title) VALUES ('db', 'tui', 'glm-5', 1790000000, 'T')`, nil},
			{`INSERT INTO messages (session_id, role, content, display_metadata, timestamp) VALUES ('db', 'user', ?, ?, 1790000001)`, []any{text, text}},
			{`INSERT INTO messages (session_id, role, content, reasoning, tool_calls, timestamp) VALUES ('db', 'assistant', ?, ?, ?, 1790000002)`, []any{text, text, text}},
			{`INSERT INTO messages (session_id, role, content, tool_call_id, tool_name, timestamp) VALUES ('db', 'tool', ?, 'call_1', 'terminal', 1790000003)`, []any{text}},
			{`INSERT INTO messages (session_id, role, content, _compressed_summary, timestamp) VALUES ('db', 'assistant', ?, 1, ?)`, []any{text, text}},
		} {
			if _, err := tx.Exec(stmt.sql, stmt.args...); err != nil {
				t.Fatalf("%s: %v", stmt.sql, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		db.Close()
		sessions := filepath.Join(dir, "sessions")
		sourcetest.Write(t, filepath.Join(sessions, "sessions.json"), map[string]any{
			"agent:main:file": map[string]any{"session_id": "file", "display_name": "A file", "input_tokens": 5},
		})
		sourcetest.Write(t, filepath.Join(sessions, "file.jsonl"), text)
		// Every column the input lands in is read as whatever it holds, and every line of the
		// file as JSON or not, so no input is a reason for the store to be unlistable or a
		// session unreadable. Both sessions are listed and read, or the run fails: a harness
		// whose input stopped the scan would pass while reading nothing.
		all := sourcetest.All(t, Spec.Open(dir))
		if len(all) != 2 || all[0].Session.ID != "db" || all[1].Session.ID != "file" {
			t.Fatalf("read %d sessions, want the database's and the file's", len(all))
		}
		for _, s := range all {
			sourcetest.Invariants(t, s)
		}
	})
}
