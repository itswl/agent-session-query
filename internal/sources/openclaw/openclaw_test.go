package openclaw

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

const schema = `
CREATE TABLE session_windows (
	session_id TEXT PRIMARY KEY, session_key TEXT NOT NULL,
	status TEXT, started_at INTEGER, ended_at INTEGER,
	updated_at INTEGER NOT NULL, transcript_updated_at INTEGER,
	model_provider TEXT, model TEXT, display_name TEXT);
CREATE TABLE transcript_events (
	session_id TEXT NOT NULL, seq INTEGER NOT NULL,
	event_json TEXT NOT NULL, created_at INTEGER NOT NULL,
	PRIMARY KEY (session_id, seq));`

// Rows copied from a real 2026.9 install: the event shapes, the call and its result as
// two messages, the unix-millisecond times.
var rows = []string{
	`INSERT INTO session_windows VALUES ('ses_chat', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '部署排障记录')`,
	`INSERT INTO session_windows VALUES ('ses_cli', 'agent:main:main', 'running', 1789704223504, NULL, 1789704227713, 1789704227688, 'deepseek', 'deepseek-chat', '')`,
	`INSERT INTO transcript_events VALUES ('ses_cli', 1, '{"type":"session","id":"ses_cli","cwd":"/w/proj"}', 1789704223504)`,
	`INSERT INTO transcript_events VALUES ('ses_cli', 2, '{"type":"message","timestamp":"2026-09-18T04:03:44.021Z","message":{"role":"user","content":"set up an Nginx reverse proxy"}}', 1789704224021)`,
	`INSERT INTO transcript_events VALUES ('ses_cli', 3, '{"type":"message","timestamp":"2026-09-18T04:03:45.831Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"look first"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls -la"}}],"stopReason":"toolUse"}}', 1789704225831)`,
	`INSERT INTO transcript_events VALUES ('ses_cli', 4, '{"type":"message","timestamp":"2026-09-18T04:03:45.867Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","isError":true,"content":[{"type":"text","text":"ls: cannot access"}]}}', 1789704225867)`,
	`INSERT INTO transcript_events VALUES ('ses_cli', 5, '{"type":"model_change","timestamp":"2026-09-18T04:03:46.000Z","provider":"glm","modelId":"glm-5.3"}', 1789704226000)`,
	`INSERT INTO transcript_events VALUES ('ses_cli', 6, '{"type":"message","timestamp":"2026-09-18T04:03:47.000Z","message":{"role":"assistant","content":[{"type":"text","text":"proxy_pass goes like this"}],"stopReason":"stop","model":"glm-5.3","usage":{"input":481,"output":161,"cacheRead":37248,"cacheWrite":0,"totalTokens":37890,"cost":{"total":0.0021}}}}', 1789704227000)`,
	`INSERT INTO transcript_events VALUES ('ses_cli', 7, 'not json at all', 1789704228000)`,
}

func agent(t *testing.T, state, name string, statements ...string) string {
	t.Helper()
	path := filepath.Join(state, "agents", name, "agent", "openclaw-agent.sqlite")
	sourcetest.Write(t, filepath.Join(filepath.Dir(path), ".keep"), "")
	exec(t, path, statements...)
	return path
}

func exec(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func TestAgentDatabase(t *testing.T) {
	state := t.TempDir()
	path := agent(t, state, "main", append([]string{schema}, rows...)...)
	all := sourcetest.All(t, Spec.Open(state))
	if len(all) != 2 {
		t.Fatalf("sessions = %d", len(all))
	}
	sourcetest.NothingUnread(t, all...)
	chat := sourcetest.ByID(t, all, "ses_chat")
	if chat.Session.Title != "部署排障记录" || chat.Session.Cwd != "" || chat.Session.Running {
		t.Errorf("chat session = %+v", chat.Session)
	}
	cli := sourcetest.ByID(t, all, "ses_cli")
	s := cli.Session
	// With no display_name the first ask is the title; the cwd is the session row's
	if s.Title != "set up an Nginx reverse proxy" || s.Cwd != "/w/proj" || s.Model != "deepseek-chat" || !s.Running || s.Path != path {
		t.Errorf("cli session = %+v", s)
	}
	if s.StartedAt.UnixMilli() != 1789704223504 || s.UpdatedAt.UnixMilli() != 1789704227688 {
		t.Errorf("times = %v / %v", s.StartedAt, s.UpdatedAt)
	}
	turn := cli.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt thinking tool event reply" {
		t.Errorf("items = %s", got)
	}
	tool := sourcetest.Tools(turn)[0]
	if tool.Name != "bash" || tool.Command != "ls -la" || tool.Status != model.StatusError || tool.Output != "ls: cannot access" {
		t.Errorf("tool = %+v", tool)
	}
	if turn.Items[3].Event != model.EventModelChange || turn.Items[3].Text != "glm/glm-5.3" {
		t.Errorf("model change = %+v", turn.Items[3])
	}
	// The pi-ai shape: the cache is beside the input already
	last := turn.Calls[len(turn.Calls)-1]
	if *last.Usage != (model.Usage{Input: 481, Output: 161, CacheRead: 37248}) || last.CostUSD != 0.0021 || last.Model != "glm-5.3" || last.Stop != "stop" {
		t.Errorf("call = %+v usage %+v", last, last.Usage)
	}
}

func TestEveryAgentIsListedAndStampsAreItsOwn(t *testing.T) {
	state := t.TempDir()
	main := agent(t, state, "main", append([]string{schema}, rows...)...)
	other := agent(t, state, "ops", schema,
		`INSERT INTO session_windows VALUES ('ses_ops', 'k', 'done', 1, 2, 2, 2, NULL, 'm', 'Ops')`,
		`INSERT INTO transcript_events VALUES ('ses_ops', 1, '{"type":"message","message":{"role":"user","content":"hello"}}', 1)`)
	src := Spec.Open(state)
	stamps := func() map[string]string {
		refs, err := src.Scan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, ref := range refs {
			out[ref.Key] = ref.Stamp
		}
		return out
	}
	before := stamps()
	if len(before) != 3 || before[other+"#ses_ops"] == "" {
		t.Fatalf("refs = %v", before)
	}
	exec(t, main, `INSERT INTO transcript_events VALUES ('ses_chat', 1, '{"type":"message","message":{"role":"user","content":"more"}}', 9)`)
	after := stamps()
	if before[main+"#ses_cli"] != after[main+"#ses_cli"] || before[other+"#ses_ops"] != after[other+"#ses_ops"] {
		t.Error("an event in one session moved another session's stamp")
	}
	if before[main+"#ses_chat"] == after[main+"#ses_chat"] {
		t.Error("a new event did not move its session's stamp")
	}
}

func TestUnreadableDatabaseIsAnError(t *testing.T) {
	state := t.TempDir()
	agent(t, state, "main", `CREATE TABLE something_else (id TEXT)`)
	if _, err := Spec.Open(state).Scan(context.Background()); err == nil {
		t.Error("a database with neither table listed nothing and said nothing")
	}
}

// Before 2026.9: an index and a transcript per session, read when no agent database is there.
func TestOlderLayout(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "agents", "default", "sessions")
	sourcetest.Write(t, filepath.Join(dir, "sessions.json"), map[string]any{
		"agent:default:hook:alert": map[string]any{"sessionId": "old-1", "status": "done", "model": "deepseek-chat", "updatedAt": 1789704262019},
		"agent:default:none":       map[string]any{"sessionId": "no-file"},
	})
	sourcetest.Write(t, filepath.Join(dir, "old-1.jsonl"),
		map[string]any{"type": "session", "id": "old-1", "cwd": "/w/old"},
		map[string]any{"type": "message", "timestamp": "2026-09-18T04:03:44.021Z", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "check the alert"}}}},
		map[string]any{"type": "message", "timestamp": "2026-09-18T04:03:45.000Z", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "toolCall", "id": "c1", "name": "bash", "arguments": map[string]any{"command": "uptime"}},
			map[string]any{"type": "toolResult", "toolCallId": "c1", "toolName": "bash", "content": []any{map[string]any{"type": "text", "text": "up 3 days"}}},
			map[string]any{"type": "text", "text": "The host is fine."}}, "stopReason": "stop"}})
	var src sources.Source = Spec.Open(state)
	if !src.Exists() {
		t.Fatal("the older layout is a store too")
	}
	s := sourcetest.One(t, src)
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "old-1" || s.Session.Cwd != "/w/old" || s.Session.Model != "deepseek-chat" || s.Session.UpdatedAt.UnixMilli() != 1789704262019 {
		t.Errorf("session = %+v", s.Session)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt tool reply" || sourcetest.Tools(turn)[0].Output != "up 3 days" || turn.Answer != "The host is fine." {
		t.Errorf("turn = %s", got)
	}
	// A result written as a block says nothing of how the call ended
	if tool := sourcetest.Tools(turn)[0]; tool.Status != "" {
		t.Errorf("status = %q, want not recorded", tool.Status)
	}
}

func TestLocate(t *testing.T) {
	if got := Spec.Locate(sourcetest.Env("/home/u")); got != filepath.Join("/home/u", ".openclaw") {
		t.Errorf("default = %s", got)
	}
	if got := Spec.Locate(sourcetest.Env("/home/u", "OPENCLAW_STATE_DIR", "/data/oc")); got != "/data/oc" {
		t.Errorf("relocated = %s", got)
	}
	if Spec.Open(t.TempDir()).Exists() || Spec.Resume != "" {
		t.Error("an empty state directory is no store, and OpenClaw has no by-id resume worth offering")
	}
}

// The 2026.11 layout, as a real install wrote it: an event past a few kilobytes is stored
// compressed (event_json NULL, event_zstd the frame), and the tools an agent's `exec`
// program runs are recorded as messages of their own kind — the only place the transcript
// says what was really read and run.
const schema2611 = `
CREATE TABLE session_windows (
	session_id TEXT PRIMARY KEY, session_key TEXT NOT NULL,
	status TEXT, started_at INTEGER, ended_at INTEGER,
	updated_at INTEGER NOT NULL, transcript_updated_at INTEGER,
	model_provider TEXT, model TEXT, display_name TEXT);
CREATE TABLE transcript_events (
	session_id TEXT NOT NULL, seq INTEGER NOT NULL,
	event_json TEXT, created_at INTEGER NOT NULL,
	event_zstd BLOB, event_utf8_bytes INTEGER,
	PRIMARY KEY (session_id, seq));`

func TestCompressedEventsAndNestedTools(t *testing.T) {
	state := t.TempDir()
	path := agent(t, state, "main", schema2611,
		`INSERT INTO session_windows VALUES ('ses_new', 'agent:main:explicit:ses_new', NULL, NULL, NULL, 1794568037232, NULL, 'deepseek', 'deepseek-v4-pro', NULL)`)
	enc, _ := zstd.NewWriter(nil)
	defer enc.Close()
	events := []struct {
		text   string
		packed bool
	}{
		{`{"type":"session","id":"ses_new","cwd":"/w/proj"}`, false},
		{`{"type":"custom","customType":"model-snapshot","data":{}}`, false},
		{`{"type":"message","timestamp":"2026-11-13T11:06:51.000Z","message":{"role":"user","content":[{"type":"text","text":"read the notes, then list the missing directory"}]}}`, false},
		{`{"type":"custom_message","customType":"openclaw.runtime-context","content":"runtime context the person never saw"}`, false},
		{`{"type":"message","timestamp":"2026-11-13T11:07:07.000Z","message":{"role":"assistant","model":"deepseek-v4-pro","content":[{"type":"toolCall","id":"call_outer","name":"exec","arguments":{"code":"return await read({path:'/w/proj/notes.txt'})"}}],"stopReason":"toolUse","usage":{"input":1000,"output":200,"cacheRead":4000,"cacheWrite":0,"reasoningTokens":150,"totalTokens":5200,"cost":{"total":0.004}}}}`, false},
		{`{"type":"message","timestamp":"2026-11-13T11:07:08.108Z","message":{"role":"custom","customType":"openclaw.nested-tool.v1","display":true,"excludeFromContext":true,"content":"","details":{"parentToolCallId":"call_outer","toolCallId":"nested_1","toolName":"read","input":{"path":"/w/proj/notes.txt"},"result":{"content":[{"type":"text","text":"the deploy runs at noon"}],"details":{"kind":"text"}},"isError":false,"startedAt":1794568028080,"timestamp":1794568028106}}}`, false},
		// The outer call's result is long, and so it is the one that arrives compressed
		{`{"type":"message","timestamp":"2026-11-13T11:07:08.200Z","message":{"role":"toolResult","toolCallId":"call_outer","toolName":"exec","isError":false,"content":[{"type":"text","text":"` + strings.Repeat("the deploy runs at noon. ", 400) + `"}]}}`, true},
		{`{"type":"message","timestamp":"2026-11-13T11:07:15.399Z","message":{"role":"custom","customType":"openclaw.nested-tool.v1","content":"","details":{"toolCallId":"nested_2","toolName":"exec","input":{"command":"ls /missing"},"result":{"content":[{"type":"text","text":"ls: /missing: No such file or directory"}],"details":{"status":"completed","exitCode":1,"durationMs":467}},"isError":false,"startedAt":1794568034704,"timestamp":1794568035399}}}`, false},
		{`{"type":"message","timestamp":"2026-11-13T11:07:17.000Z","message":{"role":"assistant","model":"deepseek-v4-pro","content":[{"type":"text","text":"The notes say the deploy runs at noon; the directory is not there."}],"stopReason":"stop","usage":{"input":300,"output":40,"cacheRead":5000,"totalTokens":5340,"cost":{"total":0.001}}}}`, false},
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range events {
		if e.packed {
			_, err = db.Exec(`INSERT INTO transcript_events (session_id, seq, event_zstd, event_utf8_bytes, created_at) VALUES ('ses_new', ?, ?, ?, 0)`, i, enc.EncodeAll([]byte(e.text), nil), len(e.text))
		} else {
			_, err = db.Exec(`INSERT INTO transcript_events (session_id, seq, event_json, created_at) VALUES ('ses_new', ?, ?, 0)`, i, e.text)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s := sourcetest.One(t, Spec.Open(state))
	sourcetest.NothingUnread(t, s)
	if s.Session.Cwd != "/w/proj" || s.Session.Model != "deepseek-v4-pro" || len(s.Turns) != 1 {
		t.Fatalf("session = %+v, %d turns", s.Session, len(s.Turns))
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt tool tool tool reply" {
		t.Errorf("items = %s: the runtime context and the snapshot are the CLI's own, and each nested run is a call", got)
	}
	tools := sourcetest.Tools(turn)
	outer, read, ls := tools[0], tools[1], tools[2]
	if outer.Name != "exec" || !outer.Done || outer.Status != model.StatusOK || !strings.HasPrefix(outer.Output, "the deploy runs at noon. ") || len(outer.Output) < 9000 {
		t.Errorf("the outer call's result was stored compressed, and is read all the same: done=%v status=%q, %d bytes", outer.Done, outer.Status, len(outer.Output))
	}
	if read.Name != "read" || read.Status != model.StatusOK || read.Output != "the deploy runs at noon" || read.DurMs != 26 || len(read.Files) != 1 || read.Files[0] != "/w/proj/notes.txt" {
		t.Errorf("nested read = %+v", read)
	}
	// Nothing flagged it — isError is false — but a command that exits 1 failed
	if ls.Name != "exec" || ls.Command != "ls /missing" || ls.Status != model.StatusError || ls.Exit == nil || *ls.Exit != 1 || ls.DurMs != 467 {
		t.Errorf("nested command = %+v", ls)
	}
	if turn.Tools != 3 || turn.Failures != 1 || !strings.HasPrefix(turn.Answer, "The notes say") {
		t.Errorf("turn = %d tools, %d failed, answer %q", turn.Tools, turn.Failures, turn.Answer)
	}
	// A nested run is not a model response: two responses, their tokens as recorded
	want := model.Usage{Input: 1300, Output: 240, CacheRead: 9000, Reasoning: 150}
	if s.Stats.Calls != 2 || s.Stats.Usage != want || s.Stats.CostUSD != 0.005 {
		t.Errorf("usage = %+v in %d calls, cost %v; want %+v in 2", s.Stats.Usage, s.Stats.Calls, s.Stats.CostUSD, want)
	}
}

// One agent's database that cannot be read is not every agent's.
func TestOneUnreadableAgentDatabase(t *testing.T) {
	state := t.TempDir()
	agent(t, state, "main", append([]string{schema}, rows...)...)
	agent(t, state, "zzz", `CREATE TABLE something_else (x)`) // a database, but not one this reads
	refs, err := Spec.Open(state).Scan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "zzz") {
		t.Errorf("the agent that could not be read went unnamed: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("refs = %d, want the two sessions of the agent that can be read", len(refs))
	}
}

// What the row shape says beyond words and calls: a response that failed at the provider,
// what a tool's own model call cost, and which of a row's two times is when it happened.
func TestErrorStopsToolSpendAndMessageTime(t *testing.T) {
	state := t.TempDir()
	agent(t, state, "main", schema,
		`INSERT INTO session_windows VALUES ('ses_x', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`INSERT INTO transcript_events VALUES ('ses_x', 1, '{"type":"message","timestamp":"2026-09-18T04:09:00.000Z","message":{"role":"user","timestamp":1789704224000,"content":"summarise the logs"}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_x', 2, '{"type":"message","timestamp":"2026-09-18T04:09:00.000Z","message":{"role":"assistant","timestamp":1789704225000,"content":[{"type":"toolCall","id":"c1","name":"summarize","arguments":{}}],"stopReason":"toolUse","usage":{"input":10,"output":5,"totalTokens":15}}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_x', 3, '{"type":"message","timestamp":"2026-09-18T04:09:00.000Z","message":{"role":"toolResult","timestamp":1789704229000,"toolCallId":"c1","toolName":"summarize","isError":false,"content":[{"type":"text","text":"a summary"}],"usage":{"input":1000,"output":200,"totalTokens":1200,"cost":{"total":0.002}}}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_x', 4, '{"type":"message","timestamp":"2026-09-18T04:09:00.000Z","message":{"role":"assistant","timestamp":1789704230000,"content":[],"stopReason":"error","errorMessage":"429 rate limit exceeded"}}', 0)`,
	)
	s := sourcetest.One(t, Spec.Open(state))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if want := (model.Usage{Input: 1010, Output: 205}); s.Stats.Usage != want || s.Stats.CostUSD != 0.002 {
		t.Errorf("usage = %+v, cost %v; want %+v: what a tool's own model call spent is on its result", s.Stats.Usage, s.Stats.CostUSD, want)
	}
	last := turn.Items[len(turn.Items)-1]
	if last.Kind != model.Event || last.Event != "error" || last.Text != "429 rate limit exceeded" {
		t.Errorf("a response that failed at the provider left %+v", last)
	}
	// All four rows were appended in the same instant; the messages say when each happened
	if got := turn.StartedAt.UnixMilli(); got != 1789704224000 {
		t.Errorf("the turn started at %d: the row's time is when it was flushed, the message's when it happened", got)
	}
	if tool := sourcetest.Tools(turn)[0]; tool.EndAt.UnixMilli() != 1789704229000 {
		t.Errorf("the tool ended at %d", tool.EndAt.UnixMilli())
	}
}

// Every counter a response carries is read, each as what it is. The first usage object is
// one OpenClaw 2026.11 wrote, whole: its output already holds the reasoning (319 = 3
// shown + 316 reasoning, and input + output = totalTokens), so reasoningTokens is the
// part of the output that was thinking and is not added to it; and what contextUsage
// says is how full the context was, which is not something spent. The second writes to
// the cache as well.
func TestEveryCounterOfAResponse(t *testing.T) {
	state := t.TempDir()
	agent(t, state, "main", schema,
		`INSERT INTO session_windows VALUES ('ses_u', 'agent:main:main', 'done', 1794000000000, 1794000009000, 1794000009000, 1794000009000, 'deepseek', 'deepseek-reasoner', '')`,
		`INSERT INTO transcript_events VALUES ('ses_u', 1, '{"type":"message","timestamp":"2026-11-07T04:00:00.000Z","message":{"role":"user","content":"what is in notes.txt"}}', 1794000000000)`,
		`INSERT INTO transcript_events VALUES ('ses_u', 2, '{"type":"message","timestamp":"2026-11-07T04:00:03.000Z","message":{"role":"assistant","model":"deepseek-reasoner","content":[{"type":"toolCall","id":"c1","name":"read","arguments":{"path":"notes.txt"}}],"stopReason":"toolUse",
			"usage":{"input":15254,"output":319,"cacheRead":0,"cacheWrite":0,"reasoningTokens":316,"contextUsage":{"promptTokens":15254,"totalTokens":15573},"totalTokens":15573,"cost":{"input":0.0021,"output":0.0001,"total":0.0022},"usageReport":{"promptTokens":15254}}}}', 1794000003000)`,
		`INSERT INTO transcript_events VALUES ('ses_u', 3, '{"type":"message","timestamp":"2026-11-07T04:00:04.000Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"read","isError":false,"content":[{"type":"text","text":"the deploy runs at noon"}]}}', 1794000004000)`,
		`INSERT INTO transcript_events VALUES ('ses_u', 4, '{"type":"message","timestamp":"2026-11-07T04:00:08.000Z","message":{"role":"assistant","model":"deepseek-reasoner","content":[{"type":"text","text":"It says the deploy runs at noon."}],"stopReason":"stop",
			"usage":{"input":200,"output":90,"cacheRead":14000,"cacheWrite":1200,"reasoningTokens":40,"totalTokens":15490,"cost":{"total":0.0005}}}}', 1794000008000)`,
	)
	s := sourcetest.One(t, Spec.Open(state))
	sourcetest.NothingUnread(t, s)
	calls := s.Turn(t, 1).Calls
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if want := (model.Usage{Input: 15254, Output: 319, Reasoning: 316}); *calls[0].Usage != want || calls[0].CostUSD != 0.0022 {
		t.Errorf("the first response = %+v ($%v), want %+v", *calls[0].Usage, calls[0].CostUSD, want)
	}
	if want := (model.Usage{Input: 200, Output: 90, CacheRead: 14000, CacheWrite: 1200, Reasoning: 40}); *calls[1].Usage != want {
		t.Errorf("the second response = %+v, want %+v", *calls[1].Usage, want)
	}
	// And they add up as they are: nothing dropped on the way to the session's total
	want := model.Usage{Input: 15454, Output: 409, CacheRead: 14000, CacheWrite: 1200, Reasoning: 356}
	if s.Stats.Usage != want || s.Stats.Usage.Total() != 15573+15490 {
		t.Errorf("the session = %+v (total %d), want %+v: what OpenClaw's own totalTokens add up to", s.Stats.Usage, s.Stats.Usage.Total(), want)
	}
}

// The rows pi's format defines beside messages, as OpenClaw may write them, are the CLI's
// own and are passed over knowingly. A row, a role or a block of a kind OpenClaw had not
// written when this was made is counted under its name, and the rest of the session reads
// as it would without it: a block's text, and a role's words, are still read.
func TestUnknownKindsAreCounted(t *testing.T) {
	state := t.TempDir()
	agent(t, state, "main", schema,
		`INSERT INTO session_windows VALUES ('ses_k', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`INSERT INTO transcript_events VALUES ('ses_k', 1, '{"type":"session","id":"ses_k","cwd":"/w/proj"}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_k', 2, '{"type":"thinking_level_change","thinkingLevel":"high"}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_k', 3, '{"type":"label","targetId":"x","label":"start"}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_k', 4, '{"type":"session_info","name":"a name"}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_k', 5, '{"type":"context_edit","targetId":"x","replacement":null}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_k', 6, '{"type":"message","timestamp":"2026-09-18T04:03:44.021Z","message":{"role":"user","content":[{"type":"text","text":"what is in notes.txt"},{"type":"image","data":"","mimeType":"image/png"}]}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_k', 7, '{"type":"checkpoint","id":"k1"}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_k', 8, '{"type":"message","timestamp":"2026-09-18T04:03:45.000Z","message":{"role":"moderator","content":"a role from a later OpenClaw"}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_k', 9, '{"type":"message","timestamp":"2026-09-18T04:03:46.000Z","message":{"role":"assistant","content":[{"type":"citation","text":"from notes.txt"},{"type":"text","text":"It says the deploy runs at noon."}],"stopReason":"stop"}}', 0)`,
	)
	s := sourcetest.One(t, Spec.Open(state))
	want := map[string]int{"row:checkpoint": 1, "role:moderator": 1, "block:citation": 1}
	if len(s.Unread) != len(want) {
		t.Errorf("unread = %v, want %v", s.Unread, want)
	}
	for kind, n := range want {
		if s.Unread[kind] != n {
			t.Errorf("unread = %v, want %v", s.Unread, want)
			break
		}
	}
	turn := s.Turn(t, 1)
	// The picture is marked as one, though the event left its bytes out
	if got := sourcetest.Kinds(turn); got != "prompt context reply reply" || turn.Ask != "what is in notes.txt\n[image · png]" {
		t.Errorf("items = %s, ask %q", got, turn.Ask)
	}
	if turn.Items[1].Text != "a role from a later OpenClaw" || turn.Items[2].Text != "from notes.txt" || !strings.HasSuffix(turn.Answer, "It says the deploy runs at noon.") {
		t.Errorf("context %q, the unknown block %q, answer %q", turn.Items[1].Text, turn.Items[2].Text, turn.Answer)
	}
}

// A counter is a count by model.TokensOf's rule: one below zero, or past model.MaxTokens,
// would take a session's totals below zero or, added to another as large, past what an
// int64 holds, and is read as none — not as the bound, which is a count itself. A price
// below zero, or past any response's, is none too.
func TestCountsThatAreNotCounts(t *testing.T) {
	state := t.TempDir()
	agent(t, state, "main", schema,
		`INSERT INTO session_windows VALUES ('ses_n', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`INSERT INTO transcript_events VALUES ('ses_n', 1, '{"type":"message","message":{"role":"user","content":"summarise the logs"}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_n', 2, '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"summarize","arguments":{}}],"stopReason":"toolUse","usage":{"input":-5,"output":7,"cacheRead":9e18,"cost":{"total":-0.5}}}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_n', 3, '{"type":"message","message":{"role":"toolResult","toolCallId":"c1","toolName":"summarize","content":"a summary","usage":{"input":10,"cacheRead":9e18,"cost":-1}}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_n', 4, '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"c2","name":"summarize","arguments":{}}],"stopReason":"toolUse","usage":{"input":`+strconv.Itoa(model.MaxTokens)+`,"cacheWrite":`+strconv.Itoa(model.MaxTokens+1)+`,"cost":{"total":1e300}}}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_n', 5, '{"type":"message","message":{"role":"toolResult","toolCallId":"c2","toolName":"summarize","content":"another"}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_n', 6, '{"type":"message","message":{"role":"assistant","content":"done","stopReason":"stop","usage":{"input":3,"output":1,"cost":{"total":0.25}}}}', 0)`,
	)
	s := sourcetest.One(t, Spec.Open(state))
	sourcetest.Invariants(t, s)
	if want := (model.Usage{Input: 13 + model.MaxTokens, Output: 8}); s.Stats.Usage != want || s.Stats.CostUSD != 0.25 {
		t.Errorf("usage = %+v, cost %v; want %+v and 0.25", s.Stats.Usage, s.Stats.CostUSD, want)
	}
}

// A message row that holds its message beside its type rather than under it — the flat
// shape — is read as that shape is, and one that holds no message at all is counted as a
// row of no role rather than dropped unseen.
func TestAMessageRowWithoutItsMessageObject(t *testing.T) {
	state := t.TempDir()
	agent(t, state, "main", schema,
		`INSERT INTO session_windows VALUES ('ses_f', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`INSERT INTO transcript_events VALUES ('ses_f', 1, '{"type":"message","timestamp":"2026-09-18T04:03:44.021Z","role":"user","content":"the proxy returns 502"}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_f', 2, '{"type":"message","id":"m2","timestamp":"2026-09-18T04:03:45.000Z"}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_f', 3, '{"type":"message","timestamp":"2026-09-18T04:03:46.000Z","message":{"role":"assistant","content":[{"type":"text","text":"The upstream block pointed at the wrong port."}],"stopReason":"stop"}}', 0)`,
	)
	s := sourcetest.One(t, Spec.Open(state))
	if len(s.Unread) != 1 || s.Unread["role:(untyped)"] != 1 {
		t.Errorf("unread = %v, want the one row with no message", s.Unread)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt reply" || turn.Ask != "the proxy returns 502" {
		t.Errorf("items = %s, ask %q", got, turn.Ask)
	}
}

// A branch_summary row is what a branch of the conversation that was left came to: an
// event, read as pi reads it, and not a row of a kind unknown.
func TestBranchSummary(t *testing.T) {
	state := t.TempDir()
	agent(t, state, "main", schema,
		`INSERT INTO session_windows VALUES ('ses_b', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`INSERT INTO transcript_events VALUES ('ses_b', 1, '{"type":"message","timestamp":"2026-09-18T04:03:44.021Z","message":{"role":"user","content":"the proxy returns 502"}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_b', 2, '{"type":"branch_summary","id":"b1","fromId":"m1","timestamp":"2026-09-18T04:03:45.000Z","summary":"tried port 8080 first; it was closed"}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_b', 3, '{"type":"message","timestamp":"2026-09-18T04:03:46.000Z","message":{"role":"assistant","content":[{"type":"text","text":"The upstream block pointed at the wrong port."}],"stopReason":"stop"}}', 0)`,
	)
	s := sourcetest.One(t, Spec.Open(state))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt event reply" || turn.Items[1].Event != "branch_summary" || turn.Items[1].Text != "tried port 8080 first; it was closed" {
		t.Errorf("items = %s %+v", got, turn.Items)
	}
}

// What a command's result says of how it ended is read where it is an exit code and a
// duration, and passed over where no exit code or duration can be: a figure far past any
// would read as one code on one platform and another on the next. The duration then
// comes from the run's own times. A Windows crash's code runs past 2^31, and is one.
func TestFiguresNoExitCodeOrDurationCanBe(t *testing.T) {
	state := t.TempDir()
	nested := func(seq int, id, details string) string {
		return `INSERT INTO transcript_events VALUES ('ses_e', ` + strconv.Itoa(seq) + `, '{"type":"message","message":{"role":"custom","customType":"openclaw.nested-tool.v1","content":"","details":{"toolCallId":"` + id +
			`","toolName":"exec","input":{"command":"run"},"result":{"content":[{"type":"text","text":"out"}],"details":` + details + `},"isError":false,"startedAt":1794568034704,"timestamp":1794568035399}}}', 0)`
	}
	agent(t, state, "main", schema,
		`INSERT INTO session_windows VALUES ('ses_e', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`INSERT INTO transcript_events VALUES ('ses_e', 1, '{"type":"message","message":{"role":"user","content":"run them"}}', 0)`,
		nested(2, "n1", `{"exitCode":1e300,"durationMs":1e300}`),
		nested(3, "n2", `{"exitCode":-1e300,"durationMs":-5}`),
		nested(4, "n3", `{"exitCode":3221225477,"durationMs":467}`),
	)
	s := sourcetest.One(t, Spec.Open(state))
	sourcetest.NothingUnread(t, s)
	tools := sourcetest.Tools(s.Turn(t, 1))
	if len(tools) != 3 {
		t.Fatalf("tools = %d", len(tools))
	}
	for _, tool := range tools[:2] {
		if tool.Exit != nil || tool.DurMs != 695 || tool.Status != model.StatusOK {
			exit := "none"
			if tool.Exit != nil {
				exit = strconv.Itoa(*tool.Exit)
			}
			t.Errorf("%s: exit %s, %d ms, status %q; want no exit code and the run's own 695 ms", tool.CallID, exit, tool.DurMs, tool.Status)
		}
	}
	if crash := tools[2]; crash.Exit == nil || *crash.Exit != 3221225477 || crash.DurMs != 467 || crash.Status != model.StatusError {
		t.Errorf("a crash on Windows = %+v", crash)
	}
}

// A run's own times are a row's word too: one that began at the start of the calendar
// and ended at its end ran for no duration a call can have, and none is read — a
// duration past what a time.Duration holds would end the call before it began.
func TestTimesNoRunCouldSpan(t *testing.T) {
	state := t.TempDir()
	agent(t, state, "main", schema,
		`INSERT INTO session_windows VALUES ('ses_t', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`INSERT INTO transcript_events VALUES ('ses_t', 1, '{"type":"message","message":{"role":"user","content":"run it"}}', 0)`,
		`INSERT INTO transcript_events VALUES ('ses_t', 2, '{"type":"message","message":{"role":"custom","customType":"openclaw.nested-tool.v1","content":"","details":{"toolCallId":"n1","toolName":"exec","input":{"command":"run"},"result":{"content":[{"type":"text","text":"out"}]},"isError":false,"startedAt":"0001-01-01T00:00:01Z","timestamp":"9999-12-31T23:59:59Z"}}}', 0)`,
	)
	s := sourcetest.One(t, Spec.Open(state))
	sourcetest.Invariants(t, s)
	if tools := sourcetest.Tools(s.Turn(t, 1)); len(tools) != 1 || tools[0].DurMs != 0 {
		t.Errorf("tools = %+v", tools)
	}
}

// A call that has not ended when its result is written says so in the result's status:
// "running" for a command given background or past its yield window (as a real one wrote
// it, in testdata), "waiting" for a Code Mode program suspended with tools still out, and
// "approval-pending" for a command whose approval is followed up later. It has no status,
// and nothing reading the record takes it for one that succeeded. Every other status is
// of a call that ended: "yielded" is sessions_yield having handed the turn back.
func TestAnExecStillRunningHasNoStatus(t *testing.T) {
	state := t.TempDir()
	nested := func(seq int, id, details string) string {
		return `INSERT INTO transcript_events VALUES ('ses_r', ` + strconv.Itoa(seq) + `, '{"type":"message","message":{"role":"custom","customType":"openclaw.nested-tool.v1","content":"","details":{"toolCallId":"` + id +
			`","toolName":"exec","input":{"command":"make test"},"result":{"content":[{"type":"text","text":"Command still running (session s1)"}],"details":` + details + `},"isError":false,"startedAt":1794568034704,"timestamp":1794568035399}}}', 0)`
	}
	agent(t, state, "main", schema,
		`INSERT INTO session_windows VALUES ('ses_r', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`INSERT INTO transcript_events VALUES ('ses_r', 1, '{"type":"message","message":{"role":"user","content":"test it"}}', 0)`,
		nested(2, "n1", `{"status":"running","sessionId":"briny-atlas","pid":2470537}`),
		nested(3, "n2", `{"status":"completed","exitCode":0}`),
		nested(4, "n3", `{"status":"approval-pending","approvalId":"a1"}`), // not run yet: waiting to be allowed
		nested(5, "n4", `{"status":"waiting","runId":"r1","reason":"pending_tools"}`),
		`INSERT INTO transcript_events VALUES ('ses_r', 6, '{"type":"message","message":{"role":"toolResult","toolCallId":"y1","toolName":"sessions_yield","content":[{"type":"text","text":"{\"status\":\"yielded\"}"}],"details":{"status":"yielded"},"isError":false}}', 0)`,
	)
	s := sourcetest.One(t, Spec.Open(state))
	var got []string
	for _, tool := range sourcetest.Tools(s.Turn(t, 1)) {
		got = append(got, tool.Status)
	}
	if want := []string{"", model.StatusOK, "", "", model.StatusOK}; !slices.Equal(got, want) {
		t.Errorf("statuses = %q, want %q", got, want)
	}
}

// One row's figure of another type does not cost the agent's database; and a session whose
// events cold storage took away says that they are missing, rather than reading as a
// session that said nothing.
func TestAFigureOfTheWrongTypeAndAColdArchive(t *testing.T) {
	state := t.TempDir()
	agent(t, state, "main", schema,
		`INSERT INTO session_windows VALUES ('ses_odd', 'agent:main:main', 'done', 'soon', 1789704262019, 'later', 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`INSERT INTO transcript_events VALUES ('ses_odd', 1, '{"type":"message","message":{"role":"user","content":"still listed"}}', 0)`,
		`INSERT INTO session_windows VALUES ('ses_cold', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`CREATE TABLE session_transcript_cold_archives (session_id TEXT PRIMARY KEY, storage TEXT, event_count INTEGER, last_seq INTEGER)`,
		`INSERT INTO session_transcript_cold_archives VALUES ('ses_cold', 'file', 40, 40)`,
		`INSERT INTO session_windows VALUES ('ses_warm', 'agent:main:main', 'done', 1789704223504, 1789704262019, 1789704262019, 1789704262019, 'deepseek', 'deepseek-chat', '')`,
		`INSERT INTO transcript_events VALUES ('ses_warm', 41, '{"type":"message","timestamp":"2026-09-18T04:30:00Z","message":{"role":"user","content":"and then?"}}', 0)`,
		`INSERT INTO session_transcript_cold_archives VALUES ('ses_warm', 'file', 'forty', 40)`,
	)
	all := sourcetest.All(t, Spec.Open(state))
	if odd := sourcetest.ByID(t, all, "ses_odd"); odd.Turn(t, 1).Ask != "still listed" {
		t.Errorf("a session whose start is a word = %+v", odd.Session)
	}
	cold := sourcetest.ByID(t, all, "ses_cold")
	if len(cold.Unread) != 0 {
		t.Errorf("a session in cold storage: unread %v — nothing was passed over not knowing it", cold.Unread)
	}
	// and says so itself, where it begins
	said := ""
	for _, turn := range cold.Turns {
		for _, it := range turn.Items {
			if it.Kind == model.Event && it.Event == model.EventColdStorage {
				said = it.Text
			}
		}
	}
	if !strings.HasPrefix(said, "40 of this session's events were moved to OpenClaw's cold storage") {
		t.Errorf("a session in cold storage says %q; turns %+v", said, cold.Turns)
	}
	// Some of it still there: the notice comes before it, and the ask is the ask
	warm := sourcetest.ByID(t, all, "ses_warm")
	first := warm.Turns[0]
	if len(first.Items) == 0 || first.Items[0].Event != model.EventColdStorage || !strings.HasPrefix(first.Items[0].Text, "Some of this session's events") {
		t.Errorf("a session partly in cold storage begins with %+v", first.Items)
	}
	if warm.Turn(t, 1).Ask != "and then?" {
		t.Errorf("the ask after the notice = %q", warm.Turn(t, 1).Ask)
	}
}
