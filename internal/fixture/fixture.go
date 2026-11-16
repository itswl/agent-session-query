// Package fixture is a small history for tests: a Claude Code store on disk, read
// through the same adapter, writer and index as a real one. The tests of everything
// above the adapters — the index, the command line, the HTTP API, the MCP server, the
// exporter — ask their questions of it, so what they check is the whole path and not a
// stand-in for it.
//
//	alpha   "Fix flaky test", in /work/alpha on main, an hour old
//	          #1  fix the flaky test          Bash fails (exit 1), then the fix
//	          #2  现在给数据库加索引            Edit db/schema.sql, an Agent call, the answer
//	          └─ agent-x1 (Explore)           the subagent that call started
//	beta    in /work/beta on release, three days old
//	          #1  rotate the deploy key       a long output, and a reply holding a key
package fixture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/index"
)

// The ids and the secret the fixture plants.
const (
	Alpha    = "aaaa1111-0000-4000-8000-000000000001"
	Beta     = "bbbb2222-0000-4000-8000-000000000002"
	Agent    = "x1"
	Secret   = "sk-" + "Zx9Qw7Lm3Pv5Tr8Kd2Nf6Hb4" // in two pieces: whole, it is a key to every scanner that reads this file
	LongLine = "line of build output that goes on and on"
)

type m = map[string]any

// History is the fixture on disk.
type History struct {
	Dir string    // the Claude store
	Now time.Time // the moment the sessions are dated back from
}

// Write puts the history under a temporary directory.
func Write(t testing.TB) History {
	t.Helper()
	h := History{Dir: filepath.Join(t.TempDir(), "claude"), Now: time.Now().UTC().Truncate(time.Second)}
	at := func(ago time.Duration) string { return h.Now.Add(-ago).Format(time.RFC3339Nano) }

	user := func(session, cwd, branch, uuid, ts string, content any, extra ...m) m {
		row := m{"type": "user", "uuid": uuid, "timestamp": ts, "sessionId": session, "cwd": cwd, "gitBranch": branch,
			"version": "2.1.300", "message": m{"role": "user", "content": content}}
		for _, e := range extra {
			for k, v := range e {
				row[k] = v
			}
		}
		return row
	}
	assistant := func(session, uuid, ts, response string, usage m, content ...m) m {
		return m{"type": "assistant", "uuid": uuid, "timestamp": ts, "sessionId": session,
			"message": m{"id": response, "role": "assistant", "model": "claude-opus-5", "content": content, "usage": usage, "stop_reason": "end_turn"}}
	}

	const hour, minute, second = time.Hour, time.Minute, time.Second
	alpha := filepath.Join(h.Dir, "-work-alpha")
	write(t, filepath.Join(alpha, Alpha+".jsonl"),
		user(Alpha, "/work/alpha", "main", "a-u1", at(hour), "fix the flaky test"),
		assistant(Alpha, "a-a1", at(hour-5*second), "msg_a1", m{"input_tokens": 100, "output_tokens": 40, "cache_read_input_tokens": 900, "cache_creation_input_tokens": 50},
			m{"type": "thinking", "thinking": "the order of a map is not stable"},
			m{"type": "tool_use", "id": "toolu_test", "name": "Bash", "input": m{"command": "go test ./..."}}),
		user(Alpha, "/work/alpha", "main", "a-u2", at(hour-9*second),
			[]m{{"type": "tool_result", "tool_use_id": "toolu_test", "content": "--- FAIL: TestOrder\ndial tcp: ECONNREFUSED 127.0.0.1:5432\nexit status 1", "is_error": true}},
			m{"toolUseResult": m{"durationMs": 2500}}),
		assistant(Alpha, "a-a2", at(hour-20*second), "msg_a2", m{"input_tokens": 20, "output_tokens": 30, "cache_read_input_tokens": 1000},
			m{"type": "text", "text": "The test depended on map order; fixed by sorting the keys."}),

		user(Alpha, "/work/alpha", "main", "a-u3", at(30*minute), "现在给数据库加索引"),
		assistant(Alpha, "a-a3", at(30*minute-4*second), "msg_a3", m{"input_tokens": 30, "output_tokens": 60, "cache_read_input_tokens": 1100},
			m{"type": "tool_use", "id": "toolu_edit", "name": "Edit", "input": m{"file_path": "/work/alpha/db/schema.sql", "old_string": "x", "new_string": "CREATE INDEX idx_users_email ON users (email);"}}),
		user(Alpha, "/work/alpha", "main", "a-u4", at(30*minute-5*second),
			[]m{{"type": "tool_result", "tool_use_id": "toolu_edit", "content": "The file has been updated."}}),
		assistant(Alpha, "a-a4", at(30*minute-6*second), "msg_a4", m{"input_tokens": 10, "output_tokens": 20, "cache_read_input_tokens": 1200},
			m{"type": "tool_use", "id": "toolu_agent", "name": "Agent", "input": m{"prompt": "check the other tables for missing indexes"}}),
		user(Alpha, "/work/alpha", "main", "a-u5", at(29*minute),
			[]m{{"type": "tool_result", "tool_use_id": "toolu_agent", "content": "orders has no index on user_id"}}),
		assistant(Alpha, "a-a5", at(29*minute-5*second), "msg_a5", m{"input_tokens": 10, "output_tokens": 25, "cache_read_input_tokens": 1300},
			m{"type": "text", "text": "索引已加上，另外 orders 表也缺一个。"}),
		m{"type": "ai-title", "aiTitle": "Fix flaky test"},
	)
	agent := filepath.Join(alpha, Alpha, "subagents", "agent-"+Agent)
	write(t, agent+".jsonl",
		m{"type": "user", "isSidechain": true, "agentId": Agent, "sessionId": Alpha, "uuid": "x-u1", "cwd": "/work/alpha",
			"timestamp": at(30*minute - 7*second), "message": m{"role": "user", "content": "check the other tables for missing indexes"}},
		m{"type": "assistant", "isSidechain": true, "agentId": Agent, "sessionId": Alpha, "uuid": "x-a1", "timestamp": at(30*minute - 20*second),
			"message": m{"id": "msg_x1", "role": "assistant", "model": "claude-haiku-5", "usage": m{"input_tokens": 5, "output_tokens": 7},
				"content": []m{{"type": "text", "text": "orders has no index on user_id"}}}},
	)
	write(t, agent+".meta.json", m{"agentType": "Explore", "description": "Survey the tables", "toolUseId": "toolu_agent"})

	const day = 24 * time.Hour
	write(t, filepath.Join(h.Dir, "-work-beta", Beta+".jsonl"),
		user(Beta, "/work/beta", "release", "b-u1", at(3*day), "rotate the deploy key"),
		assistant(Beta, "b-a1", at(3*day-3*second), "msg_b1", m{"input_tokens": 50, "output_tokens": 10},
			m{"type": "tool_use", "id": "toolu_build", "name": "Bash", "input": m{"command": "make build"}}),
		user(Beta, "/work/beta", "release", "b-u2", at(3*day-30*second),
			[]m{{"type": "tool_result", "tool_use_id": "toolu_build", "content": strings.Repeat(LongLine+"\n", 400) + "build finished"}}),
		assistant(Beta, "b-a2", at(3*day-40*second), "msg_b2", m{"input_tokens": 60, "output_tokens": 15},
			m{"type": "text", "text": "Rotated. The new key is " + Secret + " and the old one is revoked."}),
	)
	return h
}

func write(t testing.TB, path string, rows ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
}

// Config reads the history and nothing else, into an index of its own.
func (h History) Config(t testing.TB) app.Config {
	t.Helper()
	return app.Config{
		IndexPath: filepath.Join(t.TempDir(), "index.db"),
		Stores:    catalog.Config{Only: []string{"claude"}, Paths: []catalog.Path{{Agent: "claude", Dir: h.Dir}}},
	}
}

// Open writes the history, opens the program over it and brings the index up to date.
func Open(t testing.TB) (*app.App, History) {
	t.Helper()
	h := Write(t)
	a, err := app.Open(h.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if _, err := a.Sync(t.Context(), index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	return a, h
}
