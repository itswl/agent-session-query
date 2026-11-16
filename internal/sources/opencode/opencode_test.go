package opencode

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
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

func build(t *testing.T, statements ...string) (sources.Source, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	exec(t, path, statements...)
	return Spec.Open(path), path
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
