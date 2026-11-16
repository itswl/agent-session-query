package openclaw

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// FuzzRead reads whatever it is given as an agent database would hold it: each line one
// event of a session, as event_json, and the whole of it once more as one event stored
// compressed, the way OpenClaw stores a large one. Nothing it reads may panic or hand back
// a session that breaks what every session holds.
func FuzzRead(f *testing.F) {
	// The fixtures above, cut into short transcripts: an input the fuzzer finds new is
	// shrunk a byte at a time, each try a database built and read, and a long one takes the
	// whole of the time it is given for that
	for _, events := range [][]string{
		{
			`{"type":"session","id":"ses_cli","cwd":"/w/proj"}`,
			`{"type":"message","timestamp":"2026-09-18T04:03:44.021Z","message":{"role":"user","content":"set up an Nginx reverse proxy"}}`,
			`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"look first"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls -la"}}],"stopReason":"toolUse"}}`,
		},
		{
			`{"type":"message","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","isError":true,"content":[{"type":"text","text":"ls: cannot access"}],"usage":{"input":1000,"cost":{"total":0.002}}}}`,
			`{"type":"model_change","provider":"glm","modelId":"glm-5.3"}`,
			`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"proxy_pass"}],"stopReason":"stop","model":"glm-5.3","usage":{"input":481,"output":161,"cacheRead":37248,"reasoningTokens":40,"cost":{"total":0.0021}}}}`,
		},
		{
			`{"type":"custom","customType":"model-snapshot","data":{}}`,
			`{"type":"message","message":{"role":"custom","customType":"openclaw.nested-tool.v1","details":{"toolCallId":"n1","toolName":"exec","input":{"command":"ls /missing"},"result":{"content":[{"type":"text","text":"No such file"}],"details":{"exitCode":1,"durationMs":467}},"isError":false,"startedAt":1794568034704,"timestamp":1794568035399}}}`,
		},
		{
			`{"type":"custom_message","customType":"openclaw.runtime-context","content":"runtime context"}`,
			`{"type":"compaction","summary":"what came before"}`,
			`{"type":"message","message":{"role":"assistant","timestamp":1789704230000,"content":[],"stopReason":"error","errorMessage":"429 rate limit exceeded"}}`,
		},
		{
			// The older layout's shape: a result written as a block of the response
			`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"uptime"}},{"type":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"up 3 days"}]},{"type":"text","text":"fine"}],"stopReason":"stop"}}`,
		},
	} {
		f.Add([]byte(strings.Join(events, "\n")))
	}
	f.Fuzz(attempt)
}

// packer compresses an event the way OpenClaw stores a large one.
var packer, _ = zstd.NewWriter(nil)

// attempt lays data out as one session of an agent database and reads the store.
func attempt(t *testing.T, data []byte) {
	// A seed's directory is named after it, "seed#0": a URI must escape the #
	state := t.TempDir()
	path := filepath.Join(state, "agents", "main", "agent", "openclaw-agent.sqlite")
	sourcetest.Write(t, filepath.Join(filepath.Dir(path), ".keep"), "")
	db, err := sql.Open("sqlite", kit.FileURI(path))
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
	must := func(stmt string, args ...any) {
		if _, err := tx.Exec(stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	must(schema2611)
	must(`INSERT INTO session_windows VALUES ('ses', 'agent:main:main', 'running', 1789704223504, NULL, 1789704227713, 1789704227688, 'deepseek', 'deepseek-chat', '')`)
	seq := 0
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		seq++
		must(`INSERT INTO transcript_events (session_id, seq, event_json, created_at) VALUES ('ses', ?, ?, 0)`, seq, string(line))
	}
	must(`INSERT INTO transcript_events (session_id, seq, event_zstd, created_at) VALUES ('ses', ?, ?, 0)`, seq+1, packer.EncodeAll(data, nil))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	sourcetest.Attempt(t, Spec.Open(state))
}
