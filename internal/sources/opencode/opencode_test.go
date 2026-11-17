package opencode

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// The real schemas, reduced to the columns that are read.
const schemaV1 = `
CREATE TABLE session (
	id TEXT PRIMARY KEY, directory TEXT NOT NULL, title TEXT NOT NULL, slug TEXT NOT NULL, model TEXT,
	time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, time_archived INTEGER);
CREATE TABLE message (
	id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
	time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE part (
	id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL,
	time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX message_session_time_created_id_idx ON message (session_id, time_created, id);
CREATE INDEX part_message_id_id_idx ON part (message_id, id);`

const schemaV2 = `
CREATE TABLE session_v2 (
	id TEXT PRIMARY KEY, parent_id TEXT, slug TEXT NOT NULL, directory TEXT NOT NULL, title TEXT,
	version TEXT NOT NULL, agent TEXT, model TEXT,
	time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, time_archived INTEGER);
CREATE TABLE session_message (
	id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL, seq INTEGER NOT NULL,
	time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);`

func build(t testing.TB, statements ...string) (sources.Source, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	exec(t, path, statements...)
	return Spec.Open(path), path
}

func exec(t testing.TB, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", kit.FileURI(path))
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

// Rows copied from a real 1.x database: ids, payloads and unix-millisecond times.
var rowsV1 = []string{
	`INSERT INTO session VALUES ('ses_live', '/w/proj', '询问模型身份', 'ask-model',
		'{"id":"deepseek-flash","providerID":"deepseek"}', 1789697128684, 1789697262019, NULL)`,
	`INSERT INTO session VALUES ('ses_archived', '/w/old', '', 'old-slug', NULL, 1789000000000, 1789000000000, 1789000100000)`,
	`INSERT INTO message VALUES ('msg_u1', 'ses_live', 1789697128704, 1789697128704, '{"role":"user","time":{"created":1789697128704}}')`,
	`INSERT INTO message VALUES ('msg_a1', 'ses_live', 1789697128725, 1789697131296,
		'{"role":"assistant","modelID":"deepseek-flash","providerID":"deepseek","finish":"stop","cost":0.0031,
		  "tokens":{"input":100,"output":20,"reasoning":30,"cache":{"read":500,"write":7}},
		  "time":{"created":1789697128725,"completed":1789697131296}}')`,
	`INSERT INTO part VALUES ('p1', 'msg_u1', 'ses_live', 1, 1, '{"type":"text","text":"set up an Nginx reverse proxy"}')`,
	`INSERT INTO part VALUES ('p2', 'msg_a1', 'ses_live', 2, 2, '{"type":"reasoning","text":"nginx needs the proxy_pass directive"}')`,
	`INSERT INTO part VALUES ('p3', 'msg_a1', 'ses_live', 3, 3, '{"type":"step-start","snapshot":"abc"}')`,
	`INSERT INTO part VALUES ('p4', 'msg_a1', 'ses_live', 4, 4,
		'{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"ls -la"},"output":"note.txt","metadata":{"exit":0},"time":{"start":1000,"end":1250}}}')`,
	`INSERT INTO part VALUES ('p5', 'msg_a1', 'ses_live', 5, 5, '{"type":"tool","tool":"grep","callID":"c2","state":{"status":"error","error":"no such file"}}')`,
	`INSERT INTO part VALUES ('p6', 'msg_a1', 'ses_live', 6, 6, '{"type":"tool","tool":"bash","callID":"c3","state":{"status":"error","error":"The operation was aborted"}}')`,
	`INSERT INTO part VALUES ('p7', 'msg_a1', 'ses_live', 7, 7, '{"type":"tool","tool":"bash","callID":"c4","state":{"status":"running","input":{"command":"sleep 9"}}}')`,
	`INSERT INTO part VALUES ('p8', 'msg_a1', 'ses_live', 8, 8, '{"type":"text","text":"proxy_pass goes like this"}')`,
	`INSERT INTO part VALUES ('p9', 'msg_a1', 'ses_live', 9, 9, '{"type":"step-finish","reason":"stop"}')`,
}

func TestV1(t *testing.T) {
	src, path := build(t, append([]string{schemaV1}, rowsV1...)...)
	all := sourcetest.All(t, src)
	if len(all) != 2 {
		t.Fatalf("sessions = %d", len(all))
	}
	sourcetest.NothingUnread(t, all...)
	old, live := sourcetest.ByID(t, all, "ses_archived"), sourcetest.ByID(t, all, "ses_live")
	if !old.Session.Archived || old.Session.Title != "old-slug" {
		t.Errorf("archived session = %+v", old.Session)
	}
	s := live.Session
	if s.Title != "询问模型身份" || s.Cwd != "/w/proj" || s.Model != "deepseek/deepseek-flash" || s.Path != path || s.Archived {
		t.Errorf("session = %+v", s)
	}
	if s.StartedAt.UnixMilli() != 1789697128684 || s.UpdatedAt.UnixMilli() != 1789697262019 {
		t.Errorf("times = %v / %v", s.StartedAt, s.UpdatedAt)
	}
	turn := live.Turn(t, 1)
	// step-start and step-finish are boundaries, not content
	if got := sourcetest.Kinds(turn); got != "prompt thinking tool tool tool tool reply" {
		t.Errorf("items = %s", got)
	}
	if turn.Ask != "set up an Nginx reverse proxy" || turn.Answer != "proxy_pass goes like this" {
		t.Errorf("ask %q answer %q", turn.Ask, turn.Answer)
	}
	tools := sourcetest.Tools(turn)
	if ls := tools[0]; ls.Name != "bash" || ls.Command != "ls -la" || ls.Output != "note.txt" || ls.Status != model.StatusOK || ls.Exit == nil || *ls.Exit != 0 || ls.DurMs != 250 {
		t.Errorf("completed tool = %+v", ls)
	}
	if grep := tools[1]; grep.Status != model.StatusError || grep.Output != "error: no such file" {
		t.Errorf("failed tool = %+v", grep)
	}
	if stopped := tools[2]; stopped.Status != model.StatusInterrupted {
		t.Errorf("a tool the user stopped is interrupted: %+v", stopped)
	}
	if running := tools[3]; running.Done {
		t.Errorf("a running tool has no result yet: %+v", running)
	}
	// The reasoning is reported beside the output and is added into it
	call := turn.Calls[0]
	want := model.Usage{Input: 100, Output: 50, CacheRead: 500, CacheWrite: 7, Reasoning: 30}
	if len(turn.Calls) != 1 || *call.Usage != want || call.Model != "deepseek/deepseek-flash" || call.CostUSD != 0.0031 || call.DurMs != 2571 || call.Stop != "stop" {
		t.Errorf("call = %+v usage %+v", call, call.Usage)
	}
}

// Rows in the shapes a real 2.x database holds: a tool's output is a list of content
// items, its error an object, its times on the part.
var rowsV2 = []string{
	`INSERT INTO session_v2 VALUES ('ses_2', NULL, 'slug', '/w/two', 'Ship the fix', '2.3.1', 'build',
		'{"id":"glm-5.3","providerID":"glm","variant":"high"}', 1790000000000, 1790000000500, NULL)`,
	`INSERT INTO session_message VALUES ('m1', 'ses_2', 'system', 1, 1790000000001, 1790000000001,
		'{"description":"Instructions updated: core/date","text":"Today is Thursday.","metadata":{}}')`,
	`INSERT INTO session_message VALUES ('m2', 'ses_2', 'user', 2, 1790000001000, 1790000001000, '{"text":"run the tests","files":[],"time":{"created":1790000001000}}')`,
	`INSERT INTO session_message VALUES ('m3', 'ses_2', 'assistant', 3, 1790000002000, 1790000009000,
		'{"agent":"build","finish":"tool-calls","cost":0.01,"model":{"id":"glm-5.3","providerID":"glm","variant":"high"},
		  "time":{"created":1790000002000,"streamed":1790000002400,"completed":1790000009000},
		  "tokens":{"input":10,"output":4,"reasoning":6,"cache":{"read":90,"write":0}},
		  "content":[
		    {"type":"reasoning","text":"start with the suite","time":{"created":1790000002500,"completed":1790000003000}},
		    {"type":"tool","id":"call_1","name":"shell","executed":false,"time":{"created":1790000003000,"ran":1790000003100,"completed":1790000005100},
		     "state":{"status":"completed","input":{"command":"go test ./..."},"content":[{"type":"text","text":"FAIL pkg"},{"type":"file","name":"report.xml","mime":"text/xml","uri":"file:///r"}],"metadata":{"exit":1,"truncated":false}}},
		    {"type":"tool","id":"call_2","name":"read","executed":false,"time":{"created":1790000005200,"ran":1790000005200,"completed":1790000005300},
		     "state":{"status":"error","input":{"path":"/w/two/a.go"},"error":{"type":"aborted","message":"Aborted by the user"}}},
		    {"type":"text","text":"One package fails."}]}')`,
	`INSERT INTO session_message VALUES ('m4', 'ses_2', 'shell', 4, 1790000010000, 1790000010000,
		'{"command":"git status","exit":0,"status":"exited","shellID":"s","output":{"output":"clean","truncated":false,"size":5,"cursor":5},"time":{"created":1790000010000,"completed":1790000010300}}')`,
	`INSERT INTO session_message VALUES ('m5', 'ses_2', 'model-switched', 5, 1790000011000, 1790000011000, '{"model":{"id":"deepseek-flash","providerID":"deepseek"},"time":{"created":1790000011000}}')`,
	`INSERT INTO session_message VALUES ('m6', 'ses_2', 'compaction', 6, 1790000012000, 1790000012000, '{"status":"completed","reason":"auto","summary":"## Where we are\nOne package fails."}')`,
	`INSERT INTO session_message VALUES ('m7', 'ses_2', 'compaction', 7, 1790000013000, 1790000013000, '{"status":"failed","error":{"data":{"message":"context too long"}}}')`,
	`INSERT INTO session_message VALUES ('m8', 'ses_2', 'assistant', 8, 1790000014000, 1790000014000,
		'{"finish":"error","model":{"id":"deepseek-flash","providerID":"deepseek"},"error":{"type":"aborted","message":"Aborted"},"content":[],"time":{"created":1790000014000}}')`,
	`INSERT INTO session_message VALUES ('m9', 'ses_2', 'idle', 9, 1790000015000, 1790000015000, '{"outcome":"interrupted","time":{"created":1790000015000}}')`,
	`INSERT INTO session_message VALUES ('m10', 'ses_2', 'user', 10, 1790000016000, 1790000016000, '{"text":"try again"}')`,
	`INSERT INTO session_message VALUES ('m11', 'ses_2', 'idle', 11, 1790000017000, 1790000017000, '{"outcome":"failed","error":{"type":"provider.free-limit","message":"free limit reached"}}')`,
	`INSERT INTO session_message VALUES ('m12', 'ses_2', 'user', 12, 1790000018000, 1790000018000, '{not json')`,
	// a session a task tool spawned
	`INSERT INTO session_v2 VALUES ('ses_child', 'ses_2', 'c', '/w/two', 'Look into pkg', '2.3.1', 'explore', NULL, 1790000003000, 1790000004000, NULL)`,
}

func TestV2(t *testing.T) {
	src, _ := build(t, append([]string{schemaV2}, rowsV2...)...)
	all := sourcetest.All(t, src)
	// The row that will not parse is not counted: it is not of a kind, it is broken
	sourcetest.NothingUnread(t, all...)
	s := sourcetest.ByID(t, all, "ses_2")
	if s.Session.Title != "Ship the fix" || s.Session.Cwd != "/w/two" || s.Session.Version != "2.3.1" {
		t.Errorf("session = %+v", s.Session)
	}
	if got := sourcetest.Kinds(s.Turn(t, 0)); got != "context" {
		t.Errorf("preamble = %s", got)
	}
	one := s.Turn(t, 1)
	if got := sourcetest.Kinds(one); got != "prompt thinking tool tool reply tool event event event event" {
		t.Errorf("turn 1 = %s", got)
	}
	tools := sourcetest.Tools(one)
	if sh := tools[0]; sh.Command != "go test ./..." || sh.Output != "FAIL pkg\n[file: report.xml]" || sh.Status != model.StatusError || *sh.Exit != 1 || sh.DurMs != 2000 {
		t.Errorf("shell = %+v", sh)
	}
	if rd := tools[1]; rd.Status != model.StatusInterrupted || rd.Output != "error: Aborted by the user" || len(rd.Files) != 1 {
		t.Errorf("aborted read = %+v", rd)
	}
	// A command the person ran themselves is a step of the turn, not a model's call
	if own := tools[2]; own.Name != "shell" || own.Command != "git status" || own.Output != "clean" || own.Status != model.StatusOK || own.DurMs != 300 || one.Items[5].Call != 0 {
		t.Errorf("the person's shell command = %+v", own)
	}
	events := one.Items[6:]
	if events[0].Event != model.EventModelChange || events[0].Text != "deepseek/deepseek-flash" {
		t.Errorf("model switch = %+v", events[0])
	}
	if events[1].Event != model.EventCompaction || !strings.HasPrefix(events[1].Text, "## Where we are") || events[2].Text != "context too long" {
		t.Errorf("compactions = %+v / %+v", events[1], events[2])
	}
	// The response that was stopped says so once: the idle row after it adds nothing
	if events[3].Event != model.EventInterrupted || !one.Interrupted {
		t.Errorf("interruption = %+v", events[3])
	}
	call := one.Calls[0]
	if *call.Usage != (model.Usage{Input: 10, Output: 10, CacheRead: 90, Reasoning: 6}) || call.Model != "glm/glm-5.3" || call.DurMs != 7000 || call.TTFTMs != 400 || call.CostUSD != 0.01 {
		t.Errorf("call = %+v usage %+v", call, call.Usage)
	}
	two := s.Turn(t, 2)
	if two.Ask != "try again" || sourcetest.Kinds(two) != "prompt event" || two.Items[1].Text != "free limit reached" {
		t.Errorf("turn 2 = %s %+v", sourcetest.Kinds(two), two.Items)
	}
	child := sourcetest.ByID(t, all, "ses_child")
	if child.Session.Parent == nil || child.Session.Parent.Session != "ses_2" || child.Session.Parent.Type != "explore" {
		t.Errorf("child parent = %+v", child.Session.Parent)
	}
}

// A database that saw 2.x and then 1.x again holds both layouts: the 1.x sessions never
// migrated are listed beside the 2.x ones, and a migrated one — it keeps its id — once.
func TestMixedLayouts(t *testing.T) {
	stmts := append([]string{schemaV1, schemaV2}, rowsV1...)
	stmts = append(stmts, rowsV2...)
	stmts = append(stmts,
		`INSERT INTO session_v2 VALUES ('ses_live', NULL, 's', '/w/migrated', 'Migrated', '2.0.0', NULL, NULL, 1789697128684, 1789697262019, NULL)`,
		`INSERT INTO session_message VALUES ('mm', 'ses_live', 'user', 1, 1789697128704, 1789697128704, '{"text":"asked after the migration"}')`,
	)
	src, _ := build(t, stmts...)
	all := sourcetest.All(t, src)
	if len(all) != 4 {
		t.Fatalf("sessions = %d, want ses_2, ses_child, ses_live once, ses_archived", len(all))
	}
	sourcetest.NothingUnread(t, all...)
	if live := sourcetest.ByID(t, all, "ses_live"); live.Session.Cwd != "/w/migrated" || live.Turn(t, 1).Ask != "asked after the migration" {
		t.Errorf("a migrated session reads through the layout it lives in: %+v", live.Session)
	}
	if old := sourcetest.ByID(t, all, "ses_archived"); !old.Session.Archived {
		t.Errorf("the unmigrated 1.x session = %+v", old.Session)
	}
}

func TestStampsMoveWithTheirOwnSession(t *testing.T) {
	src, path := build(t, append([]string{schemaV2}, rowsV2...)...)
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
	exec(t, path, `INSERT INTO session_message VALUES ('new', 'ses_child', 'user', 1, 1790000020000, 1790000020000, '{"text":"more"}')`)
	after := stamps()
	if before[path+"#ses_2"] != after[path+"#ses_2"] {
		t.Error("a message in another session moved this session's stamp")
	}
	if before[path+"#ses_child"] == after[path+"#ses_child"] {
		t.Error("a new message did not move its session's stamp")
	}
}

func TestUnreadableLayoutIsAnErrorNotAnEmptyStore(t *testing.T) {
	src, _ := build(t, `CREATE TABLE something_else (id TEXT)`)
	if _, err := src.Scan(context.Background()); err == nil {
		t.Error("a database with none of the session tables listed nothing and said nothing")
	}
	// One of the two 2.x tables alone is a migration caught halfway: 1.x is still the truth
	src, _ = build(t, append([]string{schemaV1, `CREATE TABLE session_v2 (id TEXT PRIMARY KEY)`}, rowsV1...)...)
	if all := sourcetest.All(t, src); len(all) != 2 {
		t.Errorf("sessions = %d, want the two 1.x ones", len(all))
	}
}

func TestLocateAndOpen(t *testing.T) {
	if got := Spec.Locate(sourcetest.Env("/home/u", "XDG_DATA_HOME", "/data")); filepath.ToSlash(got) != "/data/opencode/opencode.db" && !strings.Contains(got, "AppData") {
		t.Errorf("relocated = %s", got)
	}
	// A --path may name the data directory or the database itself
	dir := t.TempDir()
	if a, b := Spec.Open(dir).Location(), Spec.Open(filepath.Join(dir, "opencode.db")).Location(); a != b || filepath.Base(a) != "opencode.db" {
		t.Errorf("open: %s / %s", a, b)
	}
}

// What 2.3 added to a session row: the session a fork was made from, and OpenCode's own
// totals for the session.
const schemaV23 = `
ALTER TABLE session_v2 ADD COLUMN fork_session_id TEXT;
ALTER TABLE session_v2 ADD COLUMN fork_boundary TEXT;
ALTER TABLE session_v2 ADD COLUMN cost REAL DEFAULT 0 NOT NULL;
ALTER TABLE session_v2 ADD COLUMN tokens_input INTEGER DEFAULT 0 NOT NULL;
ALTER TABLE session_v2 ADD COLUMN tokens_output INTEGER DEFAULT 0 NOT NULL;
ALTER TABLE session_v2 ADD COLUMN tokens_reasoning INTEGER DEFAULT 0 NOT NULL;
ALTER TABLE session_v2 ADD COLUMN tokens_cache_read INTEGER DEFAULT 0 NOT NULL;
ALTER TABLE session_v2 ADD COLUMN tokens_cache_write INTEGER DEFAULT 0 NOT NULL;
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message (session_id, seq);
CREATE INDEX session_message_time_created_idx ON session_message (time_created);`

// A session and a fork of it, as OpenCode 2.3 writes them: the fork holds the parent's rows
// under new ids with their seq, their time and their tokens as they were, then a notice,
// then what was asked of the fork. Each session row counts that session's own spending —
// the parent's with the call that named it, which left no message.
var rowsFork = []string{
	`INSERT INTO session_v2 (id, slug, directory, title, version, model, time_created, time_updated, cost, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read)
		VALUES ('ses_parent', 'p', '/w/notes', 'Read the notes', '2.3.0', '{"id":"deepseek-flash","providerID":"deepseek"}', 1794794145110, 1794794149413, 0.004, 350, 31, 6, 3000)`,
	`INSERT INTO session_message VALUES ('p_u1', 'ses_parent', 'user', 4, 1794794145200, 1794794145200, '{"text":"read notes.txt","files":[]}')`,
	`INSERT INTO session_message VALUES ('p_a1', 'ses_parent', 'assistant', 5, 1794794145214, 1794794146066,
		'{"agent":"build","finish":"stop","cost":0.001,"model":{"id":"deepseek-flash","providerID":"deepseek"},
		  "time":{"created":1794794145214,"completed":1794794146066},
		  "tokens":{"input":100,"output":10,"reasoning":0,"cache":{"read":1000,"write":0}},
		  "content":[{"type":"text","text":"It says the deploy runs at noon."}]}')`,
	`INSERT INTO session_message VALUES ('p_i1', 'ses_parent', 'idle', 21, 1794794146900, 1794794146900, '{"outcome":"succeeded"}')`,
	`INSERT INTO session_message VALUES ('p_u2', 'ses_parent', 'user', 24, 1794794147384, 1794794147384, '{"text":"now list /nowhere","files":[]}')`,
	`INSERT INTO session_message VALUES ('p_a2', 'ses_parent', 'assistant', 25, 1794794147389, 1794794148547,
		'{"agent":"build","finish":"stop","cost":0.002,"model":{"id":"deepseek-flash","providerID":"deepseek"},
		  "time":{"created":1794794147389,"completed":1794794148547},
		  "tokens":{"input":200,"output":14,"reasoning":6,"cache":{"read":2000,"write":0}},
		  "content":[
		    {"type":"tool","id":"call_ls","name":"shell","time":{"created":1794794147900,"ran":1794794147900,"completed":1794794148100},
		     "state":{"status":"completed","input":{"command":"ls /nowhere"},"content":[{"type":"text","text":"ls: /nowhere: No such file or directory"}],"metadata":{"exit":1}}},
		    {"type":"text","text":"There is no such directory."}]}')`,
	`INSERT INTO session_message VALUES ('p_i2', 'ses_parent', 'idle', 39, 1794794149416, 1794794149416, '{"outcome":"succeeded"}')`,

	`INSERT INTO session_v2 (id, fork_session_id, fork_boundary, slug, directory, title, version, model, time_created, time_updated, cost, tokens_input, tokens_output, tokens_cache_read)
		VALUES ('ses_fork', 'ses_parent', '{"type":"through","messageID":"p_i2"}', 'f', '/w/notes', 'Read the notes (fork #1)', '2.3.0',
		        '{"id":"deepseek-flash","providerID":"deepseek"}', 1794794149645, 1794794150752, 0.0005, 40, 3, 500)`,
	`INSERT INTO session_message SELECT 'copy_' || seq, 'ses_fork', type, seq, time_created, time_updated, data FROM session_message WHERE session_id = 'ses_parent'`,
	`INSERT INTO session_message VALUES ('msg_fork_notice_ses_fork', 'ses_fork', 'synthetic', 43, 1794794149913, 1794794149913,
		'{"text":"<fork-notice>\nThis conversation is a fork of another conversation.\n</fork-notice>"}')`,
	`INSERT INTO session_message VALUES ('f_u1', 'ses_fork', 'user', 44, 1794794149913, 1794794149913, '{"text":"say only: forked","files":[]}')`,
	`INSERT INTO session_message VALUES ('f_a1', 'ses_fork', 'assistant', 45, 1794794149919, 1794794150752,
		'{"agent":"build","finish":"stop","cost":0.0005,"model":{"id":"deepseek-flash","providerID":"deepseek"},
		  "time":{"created":1794794149919,"completed":1794794150752},
		  "tokens":{"input":40,"output":3,"reasoning":0,"cache":{"read":500,"write":0}},
		  "content":[{"type":"text","text":"forked"}]}')`,
	`INSERT INTO session_message VALUES ('f_i1', 'ses_fork', 'idle', 51, 1794794150755, 1794794150755, '{"outcome":"succeeded"}')`,
}

func forkStore(t *testing.T, more ...string) (sources.Source, string) {
	t.Helper()
	return build(t, append(append([]string{schemaV2, schemaV23}, rowsFork...), more...)...)
}

func stampOf(t *testing.T, src sources.Source, id string) string {
	t.Helper()
	refs, err := src.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if strings.HasSuffix(ref.Key, "#"+id) {
			return ref.Stamp
		}
	}
	t.Fatalf("no session %s among %d", id, len(refs))
	return ""
}

// The rows a fork copied are its parent's: they are shown, and counted nowhere — not as
// turns of the fork, not as its failures, not as its spending.
func TestAForkCountsOnlyWhatItDidItself(t *testing.T) {
	src, _ := forkStore(t)
	all := sourcetest.All(t, src)
	sourcetest.NothingUnread(t, all...)
	fork, parent := sourcetest.ByID(t, all, "ses_fork"), sourcetest.ByID(t, all, "ses_parent")

	if fork.Stats.Turns != 1 || fork.Stats.Calls != 1 || fork.Stats.Tools != 0 || fork.Stats.Failures != 0 {
		t.Errorf("the fork's own work is one turn of one call: %+v", fork.Stats)
	}
	if want := (model.Usage{Input: 40, Output: 3, CacheRead: 500}); fork.Stats.Usage != want || fork.Stats.CostUSD != 0.0005 {
		t.Errorf("the fork spent %+v ($%v), want %+v", fork.Stats.Usage, fork.Stats.CostUSD, want)
	}
	if one := fork.Turn(t, 1); one.Ask != "say only: forked" || one.Answer != "forked" {
		t.Errorf("the fork's turn = %q → %q", one.Ask, one.Answer)
	}
	// What it copied is there to read, before its first ask, marked as the parent's
	before := fork.Turn(t, 0)
	copies, own := 0, 0
	for _, it := range before.Items {
		if it.Inherited {
			copies++
		} else {
			own++
		}
	}
	if copies != 5 || own != 1 || !strings.Contains(before.Items[len(before.Items)-1].Text, "fork-notice") {
		t.Errorf("before the fork's first ask: %d copied items and %d of its own (%s)", copies, own, sourcetest.Kinds(before))
	}
	if fork.Session.Title != "Read the notes (fork #1)" {
		t.Errorf("title = %q", fork.Session.Title)
	}

	// The parent is untouched by having been forked, and its totals are OpenCode's own:
	// the two responses and the call that named the session
	if parent.Stats.Turns != 2 || parent.Stats.Calls != 2 || parent.Stats.Failures != 1 {
		t.Errorf("parent = %+v", parent.Stats)
	}
	if want := (model.Usage{Input: 350, Output: 37, CacheRead: 3000, Reasoning: 6}); parent.Stats.Usage != want || parent.Stats.CostUSD != 0.004 {
		t.Errorf("the parent spent %+v ($%v), want %+v", parent.Stats.Usage, parent.Stats.CostUSD, want)
	}
	// Across the store every response is counted once
	total := parent.Stats.Usage
	total.Add(fork.Stats.Usage)
	if want := (model.Usage{Input: 390, Output: 40, CacheRead: 3500, Reasoning: 6}); total != want {
		t.Errorf("both sessions spent %+v, want %+v", total, want)
	}
}

// A copy is somebody else's only while somebody else holds the row. Undone in the parent,
// or with the parent deleted — which OpenCode does without deleting its forks — the
// fork's row is the only record left of what was spent, and the fork is read again.
func TestACopyWhoseOriginalIsGoneIsTheForksOwn(t *testing.T) {
	src, path := forkStore(t)
	whole := stampOf(t, src, "ses_fork")
	if plain := stampOf(t, src, "ses_parent"); strings.Contains(plain, "copied") || !strings.Contains(whole, "copied") {
		t.Fatalf("stamps: fork %q, parent %q", whole, plain)
	}

	// The parent is undone to before its second ask
	exec(t, path, `DELETE FROM session_message WHERE session_id = 'ses_parent' AND seq >= 24`)
	undone := stampOf(t, src, "ses_fork")
	if undone == whole {
		t.Fatal("the fork's stamp did not move when its parent lost the rows it copied")
	}
	fork := sourcetest.ByID(t, sourcetest.All(t, src), "ses_fork")
	if fork.Stats.Turns != 2 || fork.Stats.Calls != 2 || fork.Stats.Failures != 1 || fork.Turn(t, 1).Ask != "now list /nowhere" || fork.Turn(t, 2).Ask != "say only: forked" {
		t.Errorf("with the second exchange undone in the parent, the fork = %+v", fork.Stats)
	}
	// OpenCode's total for the fork no longer covers what the fork is the record of
	if want := (model.Usage{Input: 240, Output: 23, CacheRead: 2500, Reasoning: 6}); fork.Stats.Usage != want {
		t.Errorf("the fork spent %+v, want %+v", fork.Stats.Usage, want)
	}

	// The parent is deleted
	exec(t, path, `DELETE FROM session_message WHERE session_id = 'ses_parent'`, `DELETE FROM session_v2 WHERE id = 'ses_parent'`)
	if gone := stampOf(t, src, "ses_fork"); gone == undone || strings.Contains(gone, "copied") {
		t.Fatalf("the fork's stamp with its parent deleted = %q", gone)
	}
	fork = sourcetest.One(t, src)
	if fork.Stats.Turns != 3 || fork.Stats.Calls != 3 || fork.Turn(t, 1).Ask != "read notes.txt" {
		t.Errorf("with the parent deleted, the fork = %+v", fork.Stats)
	}
	if want := (model.Usage{Input: 340, Output: 33, CacheRead: 3500, Reasoning: 6}); fork.Stats.Usage != want {
		t.Errorf("the fork spent %+v, want %+v", fork.Stats.Usage, want)
	}
	for _, it := range fork.Turn(t, 1).Items {
		if it.Inherited {
			t.Errorf("a row nobody else holds is still marked as copied: %+v", it)
		}
	}
}

// A fork of a fork copies the copies. Each response is still counted once — by the oldest
// session that holds it — whichever of the three is deleted.
func TestAForkOfAForkAndWhatIsLeftOfItsFamily(t *testing.T) {
	second := []string{
		`INSERT INTO session_v2 (id, fork_session_id, slug, directory, title, version, time_created, time_updated, tokens_input, tokens_output)
			VALUES ('ses_fork2', 'ses_fork', 'g', '/w/notes', 'Read the notes (fork #2)', '2.3.0', 1794794160000, 1794794161000, 7, 1)`,
		`INSERT INTO session_message SELECT 'again_' || seq, 'ses_fork2', type, seq, time_created, time_updated, data FROM session_message WHERE session_id = 'ses_fork'`,
		`INSERT INTO session_message VALUES ('g_u1', 'ses_fork2', 'user', 60, 1794794160100, 1794794160100, '{"text":"and again","files":[]}')`,
		`INSERT INTO session_message VALUES ('g_a1', 'ses_fork2', 'assistant', 61, 1794794160200, 1794794160900,
			'{"finish":"stop","model":{"id":"deepseek-flash","providerID":"deepseek"},"tokens":{"input":7,"output":1,"reasoning":0,"cache":{"read":0,"write":0}},
			  "content":[{"type":"text","text":"again"}]}')`,
	}
	spent := func(t *testing.T, src sources.Source) (map[string]int64, int64) {
		t.Helper()
		by, total := map[string]int64{}, int64(0)
		for _, s := range sourcetest.All(t, src) {
			by[s.Session.ID] = s.Stats.Usage.Input
			total += s.Stats.Usage.Input
		}
		return by, total
	}
	// Every response's input, once: 100 + 200 in the parent, the 50 of the call that named
	// it, 40 in the first fork, 7 in the second
	const responses = 100 + 200 + 40 + 7

	src, path := forkStore(t, second...)
	if by, total := spent(t, src); by["ses_parent"] != 350 || by["ses_fork"] != 40 || by["ses_fork2"] != 7 || total != responses+50 {
		t.Errorf("all three present: %v", by)
	}
	// The middle one deleted: its own response is now the second fork's to account for,
	// and the parent's rows are still the parent's
	exec(t, path, `DELETE FROM session_message WHERE session_id = 'ses_fork'`, `DELETE FROM session_v2 WHERE id = 'ses_fork'`)
	if by, total := spent(t, src); by["ses_parent"] != 350 || by["ses_fork2"] != 47 || total != responses+50 {
		t.Errorf("the first fork deleted: %v", by)
	}

	// The parent deleted instead: the first fork is the oldest holder of its rows, and the
	// second still copies them
	src, path = forkStore(t, second...)
	exec(t, path, `DELETE FROM session_message WHERE session_id = 'ses_parent'`, `DELETE FROM session_v2 WHERE id = 'ses_parent'`)
	if by, total := spent(t, src); by["ses_fork"] != 340 || by["ses_fork2"] != 7 || total != responses {
		t.Errorf("the parent deleted: %v", by)
	}
}

// OpenCode's own totals for a session are read from its row; a session whose row holds
// none — an older 2.x, a session that never called a model — has none recorded.
func TestTheSessionRowsOwnTotals(t *testing.T) {
	src, _ := forkStore(t,
		`INSERT INTO session_v2 (id, slug, directory, version, time_created, time_updated) VALUES ('ses_quiet', 'q', '/w/notes', '2.3.0', 1794794170000, 1794794170000)`)
	all := sourcetest.All(t, src)
	rec := sourcetest.ByID(t, all, "ses_parent").Session.Recorded
	if rec == nil || rec.Usage == nil || *rec.Usage != (model.Usage{Input: 350, Output: 37, CacheRead: 3000, Reasoning: 6}) || rec.CostUSD != 0.004 {
		t.Errorf("recorded = %+v", rec)
	}
	if rec := sourcetest.ByID(t, all, "ses_quiet").Session.Recorded; rec != nil {
		t.Errorf("a session that spent nothing has totals recorded: %+v", rec)
	}
	// Before 2.3 the row has no such columns
	old, _ := build(t, append([]string{schemaV2}, rowsV2...)...)
	if rec := sourcetest.ByID(t, sourcetest.All(t, old), "ses_2").Session.Recorded; rec != nil {
		t.Errorf("recorded on a layout that keeps none: %+v", rec)
	}
}

// What 2.x writes beside the conversation: a skill's instructions once it was activated are
// context the model went on from, like the instructions OpenCode updates; a session moving
// to another directory and a turn that ended as it should add nothing. All of it is known.
func TestWhatV2WritesBesideTheConversation(t *testing.T) {
	src, _ := build(t, schemaV2,
		`INSERT INTO session_v2 VALUES ('ses_s', NULL, 's', '/w/s', 'Skills', '2.3.0', 'build', NULL, 1790000000000, 1790000000500, NULL)`,
		`INSERT INTO session_message VALUES ('s1', 'ses_s', 'user', 1, 1790000001000, 1790000001000, '{"text":"use the release skill","files":[],"agents":[]}')`,
		`INSERT INTO session_message VALUES ('s2', 'ses_s', 'skill', 2, 1790000001100, 1790000001100,
			'{"skill":"release","name":"release","text":"Tag, then push the tag.","time":{"created":1790000001100}}')`,
		`INSERT INTO session_message VALUES ('s3', 'ses_s', 'location-switched', 3, 1790000001200, 1790000001200,
			'{"location":{"directory":"/w/s-worktree"},"previous":{"location":{"directory":"/w/s"}},"time":{"created":1790000001200}}')`,
		`INSERT INTO session_message VALUES ('s4', 'ses_s', 'assistant', 4, 1790000001300, 1790000001900,
			'{"finish":"stop","model":{"id":"m","providerID":"p"},"content":[{"type":"text","text":"Tagged."}],"time":{"created":1790000001300,"completed":1790000001900}}')`,
		`INSERT INTO session_message VALUES ('s5', 'ses_s', 'idle', 5, 1790000002000, 1790000002000, '{"outcome":"succeeded"}')`,
	)
	s := sourcetest.One(t, src)
	sourcetest.NothingUnread(t, s)
	one := s.Turn(t, 1)
	if got := sourcetest.Kinds(one); got != "prompt context reply" || one.Items[1].Text != "Tag, then push the tag." || one.Answer != "Tagged." {
		t.Errorf("turn 1 = %s %+v", got, one.Items)
	}
}

// A record of a kind OpenCode had not written when the adapter was made — a row type, a
// part type, a tool's output item, a 1.x role — is passed over and counted under its kind,
// once however many parts it carries, and the rest of the session reads as it did. What
// 1.x wrote beside the content and is left aside on purpose is not counted.
func TestWhatIsNotUnderstoodIsCounted(t *testing.T) {
	same := func(t *testing.T, before, after sourcetest.Session) {
		t.Helper()
		if !reflect.DeepEqual(before.Turns, after.Turns) || before.Stats != after.Stats {
			t.Errorf("the session read differently with records it does not know in it:\n%+v\n%+v", before.Stats, after.Stats)
		}
	}
	t.Run("2.x", func(t *testing.T) {
		src, path := build(t, append([]string{schemaV2}, rowsV2...)...)
		before := sourcetest.ByID(t, sourcetest.All(t, src), "ses_2")
		exec(t, path,
			`INSERT INTO session_message VALUES ('x1', 'ses_2', 'hologram', 13, 1790000019000, 1790000019000, '{"text":"from a later OpenCode"}')`,
			`UPDATE session_message SET data = json_insert(data, '$.content[#]', json('{"type":"hologram","text":"?"}'), '$.content[#]', json('{"text":"no type"}')) WHERE id = 'm3'`,
			`UPDATE session_message SET data = json_insert(data, '$.content[1].state.content[#]', json('{"type":"hologram"}')) WHERE id = 'm3'`,
			// What 1.x wrote beside the content is 1.x's: 2.x writes none of it in a
			// response, and one that does is a 2.x that has moved on
			`UPDATE session_message SET data = json_insert(data, '$.content[#]', json('{"type":"file","mime":"image/png","url":"file:///w/two/plot.png"}'),
				'$.content[#]', json('{"type":"patch","hash":"abc","files":["/w/two/a.go"]}'), '$.content[#]', json('{"type":"step-finish"}')) WHERE id = 'm3'`,
		)
		after := sourcetest.ByID(t, sourcetest.All(t, src), "ses_2")
		want := map[string]int{"row:hologram": 1, "part:hologram": 1, "part:(untyped)": 1, "output:hologram": 1,
			"part:file": 1, "part:patch": 1, "part:step-finish": 1}
		if !reflect.DeepEqual(after.Unread, want) {
			t.Errorf("unread = %v, want %v", after.Unread, want)
		}
		same(t, before, after)
	})
	t.Run("1.x", func(t *testing.T) {
		src, path := build(t, append([]string{schemaV1}, rowsV1...)...)
		before := sourcetest.ByID(t, sourcetest.All(t, src), "ses_live")
		exec(t, path,
			`INSERT INTO message VALUES ('msg_x', 'ses_live', 1789697128710, 1789697128710, '{"role":"moderator","time":{"created":1789697128710}}')`,
			`INSERT INTO part VALUES ('px1', 'msg_x', 'ses_live', 1, 1, '{"type":"text","text":"a word from the moderator"}')`,
			`INSERT INTO part VALUES ('px2', 'msg_x', 'ses_live', 2, 2, '{"type":"text","text":"and another"}')`,
			// A message with no role at all, carrying tool parts: whose calls they are is not
			// known, so none of them is a tool run or a failure of this session's
			`INSERT INTO message VALUES ('msg_y', 'ses_live', 1789697128715, 1789697128715, '{"time":{"created":1789697128715}}')`,
			`INSERT INTO part VALUES ('py1', 'msg_y', 'ses_live', 1, 1, '{"type":"tool","tool":"bash","callID":"cy1","state":{"status":"completed","input":{"command":"make"},"output":"","metadata":{"exit":2}}}')`,
			`INSERT INTO part VALUES ('py2', 'msg_y', 'ses_live', 2, 2, '{"type":"tool","tool":"read","callID":"cy2","state":{"status":"error","error":"no such file"}}')`,
			`INSERT INTO part VALUES ('p10', 'msg_a1', 'ses_live', 10, 10, '{"type":"hologram"}')`,
			// What 1.x wrote beside the content
			`INSERT INTO part VALUES ('pk1', 'msg_u1', 'ses_live', 11, 11, '{"type":"file","mime":"text/plain","filename":"notes.txt","url":"file:///w/proj/notes.txt"}')`,
			`INSERT INTO part VALUES ('pk2', 'msg_u1', 'ses_live', 12, 12, '{"type":"agent","name":"explore"}')`,
			`INSERT INTO part VALUES ('pk3', 'msg_u1', 'ses_live', 13, 13, '{"type":"subtask","prompt":"look","description":"look","agent":"explore"}')`,
			`INSERT INTO part VALUES ('pk4', 'msg_u1', 'ses_live', 14, 14, '{"type":"compaction","auto":true}')`,
			`INSERT INTO part VALUES ('pk5', 'msg_a1', 'ses_live', 15, 15, '{"type":"snapshot","snapshot":"abc"}')`,
			`INSERT INTO part VALUES ('pk6', 'msg_a1', 'ses_live', 16, 16, '{"type":"patch","hash":"abc","files":["/w/proj/a"]}')`,
			`INSERT INTO part VALUES ('pk7', 'msg_a1', 'ses_live', 17, 17, '{"type":"retry","attempt":1,"error":{"name":"APIError"}}')`,
		)
		after := sourcetest.ByID(t, sourcetest.All(t, src), "ses_live")
		want := map[string]int{"role:moderator": 1, "role:(untyped)": 1, "part:hologram": 1}
		if !reflect.DeepEqual(after.Unread, want) {
			t.Errorf("unread = %v, want %v", after.Unread, want)
		}
		same(t, before, after)
	})
}

// A counter OpenCode could not have written — below zero, or past model.MaxTokens — reads
// as none, in a message and in the session row alike, and so does a cost below zero: the
// session's totals are sums, and one such value would carry them below zero. A count at
// the bound itself is a count.
func TestACounterNoCallCouldHaveReadsAsNone(t *testing.T) {
	src, _ := build(t, schemaV2, schemaV23,
		`INSERT INTO session_v2 (id, slug, directory, version, time_created, time_updated, cost, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write)
			VALUES ('ses_n', 'n', '/w', '2.3.0', 1790000000000, 1790000000000, -1, -5, 7, 9000000000000000000, 1099511627776, 1099511627777)`,
		`INSERT INTO session_message VALUES ('n1', 'ses_n', 'user', 1, 1790000001000, 1790000001000, '{"text":"count"}')`,
		`INSERT INTO session_message VALUES ('n2', 'ses_n', 'assistant', 2, 1790000002000, 1790000002000,
			'{"cost":-0.5,"tokens":{"input":-5,"output":3,"reasoning":9e18,"cache":{"read":1e300,"write":-1e300}},"content":[{"type":"text","text":"done"}]}')`,
	)
	s := sourcetest.One(t, src)
	sourcetest.Invariants(t, s)
	if call := s.Turn(t, 1).Calls[0]; call.Usage == nil || *call.Usage != (model.Usage{Output: 3}) || call.CostUSD != 0 {
		t.Errorf("call = %+v usage %+v", call, call.Usage)
	}
	if rec := s.Session.Recorded; rec == nil || *rec.Usage != (model.Usage{Output: 7, CacheRead: model.MaxTokens}) || rec.CostUSD != 0 {
		t.Errorf("recorded = %+v", rec)
	}
}

// A cost no response could have run up — past a billion dollars, or one the session row
// holds as infinite — reads as none too: the session's cost is a sum, and two such values
// carry it to +Inf. A cost of the size calls have is read as it is.
func TestACostNoCallCouldHaveReadsAsNone(t *testing.T) {
	src, _ := build(t, schemaV2, schemaV23,
		`INSERT INTO session_v2 (id, slug, directory, version, time_created, time_updated, cost, tokens_input)
			VALUES ('ses_c', 'c', '/w', '2.3.0', 1790000000000, 1790000000000, 9e999, 10)`,
		`INSERT INTO session_message VALUES ('c1', 'ses_c', 'user', 1, 1790000001000, 1790000001000, '{"text":"spend"}')`,
		`INSERT INTO session_message VALUES ('c2', 'ses_c', 'assistant', 2, 1790000002000, 1790000002000, '{"cost":1.7e308,"content":[{"type":"text","text":"a"}]}')`,
		`INSERT INTO session_message VALUES ('c3', 'ses_c', 'assistant', 3, 1790000003000, 1790000003000, '{"cost":1.7e308,"content":[{"type":"text","text":"b"}]}')`,
		`INSERT INTO session_message VALUES ('c4', 'ses_c', 'assistant', 4, 1790000004000, 1790000004000, '{"cost":2e9,"content":[{"type":"text","text":"c"}]}')`,
		`INSERT INTO session_message VALUES ('c5', 'ses_c', 'assistant', 5, 1790000005000, 1790000005000, '{"cost":0.25,"content":[{"type":"text","text":"d"}]}')`,
	)
	s := sourcetest.One(t, src)
	sourcetest.Invariants(t, s)
	var costs []float64
	for _, call := range s.Turn(t, 1).Calls {
		costs = append(costs, call.CostUSD)
	}
	if !reflect.DeepEqual(costs, []float64{0, 0, 0, 0.25}) {
		t.Errorf("costs = %v", costs)
	}
	if rec := s.Session.Recorded; rec == nil || rec.CostUSD != 0 {
		t.Errorf("recorded = %+v", rec)
	}
	if s.Stats.CostUSD != 0.25 {
		t.Errorf("the session cost $%v, want $0.25", s.Stats.CostUSD)
	}
}

// A response's time is its row's word, and a row can say anything: one that ends 9e18 ms
// after it began would end before it began, once a time.Duration wrapped. A duration no
// call could take is none (model.DurMs).
func TestADurationNoCallCouldTakeIsNone(t *testing.T) {
	src, _ := build(t, schemaV2, schemaV23,
		`INSERT INTO session_v2 (id, slug, directory, version, time_created, time_updated) VALUES ('ses_d', 'd', '/w', '2.3.0', 1790000000000, 1790000000000)`,
		`INSERT INTO session_message VALUES ('d1', 'ses_d', 'user', 1, 1790000001000, 1790000001000, '{"text":"time it"}')`,
		`INSERT INTO session_message VALUES ('d2', 'ses_d', 'assistant', 2, 1790000002000, 1790000002000,
			'{"time":{"created":1790000002000,"completed":9e18},"content":[{"type":"text","text":"a"}]}')`,
		`INSERT INTO session_message VALUES ('d3', 'ses_d', 'assistant', 3, 1790000003000, 1790000003000,
			'{"time":{"created":1790000003000,"completed":1790000004500},"content":[{"type":"text","text":"b"}]}')`,
	)
	s := sourcetest.One(t, src)
	sourcetest.Invariants(t, s)
	calls := s.Turn(t, 1).Calls
	if len(calls) != 2 || calls[0].DurMs != 0 || calls[1].DurMs != 1500 {
		t.Fatalf("calls = %+v", calls)
	}
	if !calls[0].EndAt.IsZero() && calls[0].EndAt.Before(calls[0].At) {
		t.Errorf("a call ends at %v, before it began at %v", calls[0].EndAt, calls[0].At)
	}
}
