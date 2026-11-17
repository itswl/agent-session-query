package antigravity

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

func scan(t *testing.T, src sources.Source) map[string]sources.Ref {
	t.Helper()
	refs, err := src.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]sources.Ref{}
	for _, ref := range refs {
		out[filepath.Base(ref.Key)] = ref
	}
	if len(out) != len(refs) {
		t.Fatalf("two refs share an id: %v", refs)
	}
	return out
}

func writeSteps(t *testing.T, root, brain, id string, lines ...any) string {
	t.Helper()
	dir := filepath.Join(root, brain, "brain", id)
	sourcetest.Write(t, filepath.Join(dir, ".system_generated", "logs", "transcript_full.jsonl"), lines...)
	return dir
}

// execIndex runs statements against the conversation index of a brain.
func execIndex(t *testing.T, root, brain string, stmts ...string) {
	t.Helper()
	path := filepath.Join(root, brain, "conversation_summaries.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

const createIndex = `CREATE TABLE conversation_summaries(
	conversation_id TEXT, title TEXT, workspace_uris TEXT, preview TEXT, last_modified_time TEXT)`

// newFixture builds the layout agy writes: two brain roots under one gemini dir, a
// transcript per conversation, and the index beside the CLI's root.
func newFixture(t *testing.T, index bool) string {
	t.Helper()
	root := t.TempDir()

	// One conversation the index knows — with a rewritten step, which the CLI does while
	// the turn is still finishing — and one, the IDE's, it does not
	writeSteps(t, root, "antigravity-cli", "sess-1",
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-12T10:00:00Z","content":"<USER_REQUEST>fix the build</USER_REQUEST><ADDITIONAL_METADATA>noise</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:01Z","content":"on it","input_tokens":80,"cache_read_tokens":20,"output_tokens":12}`,
		`{"step_index":2,"source":"MODEL","type":"RUN_COMMAND","status":"DONE","created_at":"2026-10-12T10:00:02Z","content":""}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:03Z","content":"done now","model":"gemini-3-pro","input_tokens":100,"cache_read_tokens":20,"output_tokens":12}`,
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:04Z","content":"all set","input_tokens":5,"output_tokens":1}`,
	)
	writeSteps(t, root, "antigravity", "sess-2",
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-10-13T09:00:00Z","content":"explain the test harness"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-13T09:00:05Z","content":"it runs fixtures"}`,
	)
	if index {
		execIndex(t, root, "antigravity-cli", createIndex,
			`INSERT INTO conversation_summaries VALUES ('sess-1', 'Build fix', '["file:///work/repo"]', 'preview', '2026-10-12T10:00:09Z')`)
	}
	return root
}

// The index answers title, workspace and time; a conversation it does not know is titled by
// its own opening ask and timed by its steps.
func TestScanAndSession(t *testing.T) {
	root := newFixture(t, true)
	src := Spec.Open(root)
	if !src.Exists() || src.Location() != root {
		t.Fatalf("exists %v location %q", src.Exists(), src.Location())
	}
	refs := scan(t, src)
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2 (the CLI's brain and the IDE's)", len(refs))
	}
	if want := filepath.Join(root, "antigravity", "brain", "sess-2"); refs["sess-2"].Key != want {
		t.Errorf("key = %q, want the brain directory %q", refs["sess-2"].Key, want)
	}

	all := sourcetest.All(t, src)
	sourcetest.NothingUnread(t, all...)
	one := sourcetest.ByID(t, all, "sess-1")
	if one.Session.Title != "Build fix" || one.Session.Path != refs["sess-1"].Key {
		t.Errorf("indexed session = %+v", one.Session)
	}
	// workspace_uris is a file:// URI: the path it yields follows the OS
	if one.Session.Cwd != filepath.FromSlash("/work/repo") {
		t.Errorf("cwd = %q, want the index's workspace", one.Session.Cwd)
	}
	indexed := time.Date(2026, 10, 12, 10, 0, 9, 0, time.UTC)
	if !one.Session.UpdatedAt.Equal(indexed) || !refs["sess-1"].ModTime.Equal(indexed) {
		t.Errorf("updated = %v, mod = %v: the index's time is the conversation's", one.Session.UpdatedAt, refs["sess-1"].ModTime)
	}

	two := sourcetest.ByID(t, all, "sess-2")
	if two.Session.Title != "explain the test harness" || two.Session.Cwd != "" {
		t.Errorf("unindexed session = %+v, want the first ask as its title", two.Session)
	}
	if want := time.Date(2026, 10, 13, 9, 0, 5, 0, time.UTC); !two.Session.UpdatedAt.Equal(want) {
		t.Errorf("unindexed updated = %v, want the last step's", two.Session.UpdatedAt)
	}
	if want := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC); !two.Session.StartedAt.Equal(want) {
		t.Errorf("unindexed started = %v, want the first step's", two.Session.StartedAt)
	}
}

func TestScanWithoutIndex(t *testing.T) {
	all := sourcetest.All(t, Spec.Open(newFixture(t, false)))
	if len(all) != 2 {
		t.Fatalf("sessions = %d, want 2", len(all))
	}
	sourcetest.NothingUnread(t, all...)
	for _, s := range all {
		if s.Session.Title == "" {
			t.Errorf("session %s has no title", s.Session.ID)
		}
	}
	if got := sourcetest.ByID(t, all, "sess-1").Session.Title; got != "fix the build" {
		t.Errorf("title = %q", got)
	}
}

// Steps arrive once, in step order, at their latest line; the person's prompt loses agy's
// own context blocks; an unrecognised step stays visible; and agy's counters become the
// canonical usage, a rewritten step counted once at its corrected numbers.
func TestSteps(t *testing.T) {
	root := newFixture(t, true)
	s := sourcetest.ByID(t, sourcetest.All(t, Spec.Open(root)), "sess-1")
	sourcetest.NothingUnread(t, s)

	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d", len(s.Turns))
	}
	turn := s.Turn(t, 1)
	if turn.Ask != "fix the build" {
		t.Errorf("ask = %q, want the user's own words", turn.Ask)
	}
	if got := sourcetest.Kinds(turn); got != "prompt reply event reply" {
		t.Fatalf("items = %s, want steps 0..3 with step 1 once", got)
	}
	if turn.Items[1].Text != "done now" {
		t.Errorf("rewritten step = %q, want the latest line", turn.Items[1].Text)
	}
	if ev := turn.Items[2]; ev.Event != "RUN_COMMAND" || ev.ID != "2" {
		t.Errorf("unknown step = %+v, want an event naming RUN_COMMAND", ev)
	}
	if turn.Answer != "all set" {
		t.Errorf("answer = %q", turn.Answer)
	}
	if len(turn.Calls) != 2 || turn.Calls[0].Model != "gemini-3-pro" {
		t.Fatalf("calls = %+v", turn.Calls)
	}
	// agy's input_tokens excludes the cache and its output_tokens includes the thinking:
	// both are the canonical meaning, so nothing is subtracted or added
	want := model.Usage{Input: 105, Output: 13, CacheRead: 20}
	if s.Stats.Usage != want {
		t.Errorf("usage = %+v, want %+v", s.Stats.Usage, want)
	}
	if *turn.Calls[0].Usage != (model.Usage{Input: 100, Output: 12, CacheRead: 20}) {
		t.Errorf("the rewritten step counts at its corrected counters: %+v", turn.Calls[0].Usage)
	}
}

// A model step that is not done carries provisional counters; only the finished ones are
// counted. A cache write is its own counter.
func TestOnlyFinishedStepsAreCounted(t *testing.T) {
	root := t.TempDir()
	writeSteps(t, root, "antigravity-cli", "sess-3",
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-10-12T10:00:00Z","content":"<USER_REQUEST>go</USER_REQUEST>"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:01Z","content":"one","input_tokens":10,"cache_read_tokens":4,"cache_write_tokens":6,"output_tokens":3}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"GENERATING","created_at":"2026-10-12T10:00:02Z","content":"par","input_tokens":900,"output_tokens":900}`,
		// a finished step that recorded no counters at all
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:03Z","content":"two"}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if want := (model.Usage{Input: 10, Output: 3, CacheRead: 4, CacheWrite: 6}); s.Stats.Usage != want {
		t.Errorf("usage = %+v, want %+v", s.Stats.Usage, want)
	}
	turn := s.Turn(t, 1)
	if len(turn.Calls) != 3 || turn.Calls[1].Usage != nil || turn.Calls[2].Usage != nil {
		t.Errorf("calls = %+v: every model step is a call, only the finished ones with counters have usage", turn.Calls)
	}
	if got := sourcetest.Kinds(turn); got != "prompt reply reply reply" {
		t.Errorf("items = %s", got)
	}
}

func TestContextIsNotAnAsk(t *testing.T) {
	root := t.TempDir()
	writeSteps(t, root, "antigravity-cli", "sess-4",
		// agy's own context in the user's seat, before anything was asked
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-10-12T10:00:00Z","content":"<ADDITIONAL_METADATA>the date is today</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"type":"USER_INPUT","created_at":"2026-10-12T10:00:01Z","content":"<USER_REQUEST>\n  what changed?\n</USER_REQUEST>\n<ADDITIONAL_METADATA>x</ADDITIONAL_METADATA>"}`,
		// an empty wrapper falls back to the whole content, and nothing at all says nothing
		`{"step_index":2,"type":"USER_INPUT","created_at":"2026-10-12T10:00:02Z","content":""}`,
		`{"step_index":3,"type":"CHECKPOINT","created_at":"2026-10-12T10:00:03Z","content":"saved"}`,
		`{"step_index":"four","type":"USER_INPUT","content":"a step with no index is not a step"}`,
		`not json`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	// A checkpoint is a step agy writes; a line that is not a step is not a record of an
	// unknown kind, it is no record at all
	sourcetest.NothingUnread(t, s)
	if got := sourcetest.Kinds(s.Turn(t, 0)); got != "context" {
		t.Errorf("turn 0 = %s: agy's own context is not what the person asked", got)
	}
	turn := s.Turn(t, 1)
	if turn.Ask != "what changed?" || sourcetest.Kinds(turn) != "prompt event" || turn.Items[1].Event != "CHECKPOINT" || turn.Items[1].Text != "saved" {
		t.Errorf("turn 1 = %+v", turn)
	}
	if len(s.Turns) != 2 {
		t.Errorf("turns = %d", len(s.Turns))
	}
}

// A stamp moves when the transcript does or the conversation's own row of the index does,
// and holds when another conversation changes — a write to the index must not have every
// transcript read again.
func TestStamps(t *testing.T) {
	root := newFixture(t, true)
	src := Spec.Open(root)
	before := scan(t, src)
	again := scan(t, src)
	for id := range before {
		if again[id].Stamp != before[id].Stamp {
			t.Errorf("%s: an untouched store changed its stamp", id)
		}
	}

	// the CLI appends a step to sess-1
	path := filepath.Join(root, "antigravity-cli", "brain", "sess-1", ".system_generated", "logs", "transcript_full.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"step_index":4,"type":"PLANNER_RESPONSE","status":"DONE","content":"more"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	after := scan(t, src)
	if after["sess-1"].Stamp == before["sess-1"].Stamp || after["sess-2"].Stamp != before["sess-2"].Stamp {
		t.Errorf("a step appended to sess-1: stamps %q → %q, sess-2 %q → %q",
			before["sess-1"].Stamp, after["sess-1"].Stamp, before["sess-2"].Stamp, after["sess-2"].Stamp)
	}

	// the index learns sess-2, and renames nothing else
	execIndex(t, root, "antigravity-cli",
		`INSERT INTO conversation_summaries VALUES ('sess-2', 'Harness', '[]', '', '2026-10-13T09:10:00Z')`)
	learned := scan(t, src)
	if learned["sess-2"].Stamp == after["sess-2"].Stamp || learned["sess-1"].Stamp != after["sess-1"].Stamp {
		t.Errorf("a row for sess-2 appeared: sess-2 %q → %q, sess-1 %q → %q",
			after["sess-2"].Stamp, learned["sess-2"].Stamp, after["sess-1"].Stamp, learned["sess-1"].Stamp)
	}
	named := sourcetest.Read(t, src, learned["sess-2"])
	sourcetest.NothingUnread(t, named)
	if named.Session.Title != "Harness" {
		t.Errorf("title = %q after the index named it", named.Session.Title)
	}

	// and renames sess-1, which moves its stamp only
	execIndex(t, root, "antigravity-cli", `UPDATE conversation_summaries SET title = 'Fixed' WHERE conversation_id = 'sess-1'`)
	renamed := scan(t, src)
	if renamed["sess-1"].Stamp == learned["sess-1"].Stamp || renamed["sess-2"].Stamp != learned["sess-2"].Stamp {
		t.Error("a rename of sess-1 must move its stamp and not sess-2's")
	}
}

// The index is read once per version of the database, not once per conversation or per
// scan: a database whose fingerprint has not moved is not opened.
func TestIndexIsReadOncePerFingerprint(t *testing.T) {
	root := newFixture(t, true)
	src := Spec.Open(root)
	if got := sourcetest.ByID(t, sourcetest.All(t, src), "sess-1").Session.Title; got != "Build fix" {
		t.Fatalf("title = %q", got)
	}

	// Rewrite the title at the same length and put the modification time back: nothing
	// about the file says it changed, so what was read stands
	db := filepath.Join(root, "antigravity-cli", "conversation_summaries.db")
	info, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	execIndex(t, root, "antigravity-cli", `UPDATE conversation_summaries SET title = 'Build fax' WHERE conversation_id = 'sess-1'`)
	if err := os.Chtimes(db, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(db); after.Size() != info.Size() {
		t.Skip("the database grew, the fingerprint moved with it")
	}
	if got := sourcetest.ByID(t, sourcetest.All(t, src), "sess-1").Session.Title; got != "Build fix" {
		t.Errorf("title = %q: an unchanged database was queried again", got)
	}
	// A fresh source has nothing remembered and reads what is there
	if got := sourcetest.ByID(t, sourcetest.All(t, Spec.Open(root)), "sess-1").Session.Title; got != "Build fax" {
		t.Errorf("title = %q", got)
	}
}

// The index is the CLI's answer to what a conversation is called, not the store: without a
// usable one the conversations are still there.
func TestAnIndexThatCannotBeReadDoesNotHideTheConversations(t *testing.T) {
	for name, setup := range map[string]func(root string){
		"not a database": func(root string) {
			sourcetest.Write(t, filepath.Join(root, "antigravity-cli", "conversation_summaries.db"), "this is not sqlite")
		},
		"no table yet": func(root string) {
			execIndex(t, root, "antigravity-cli", `CREATE TABLE other(x)`)
		},
		"a schema that moved": func(root string) {
			execIndex(t, root, "antigravity-cli", `CREATE TABLE conversation_summaries(id TEXT, name TEXT)`,
				`INSERT INTO conversation_summaries VALUES ('sess-1', 'x')`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := newFixture(t, false)
			setup(root)
			all := sourcetest.All(t, Spec.Open(root))
			if len(all) != 2 || sourcetest.ByID(t, all, "sess-1").Session.Title != "fix the build" {
				t.Errorf("sessions = %+v", all)
			}
		})
	}
}

// An index missing the optional columns still names its conversations; the time may be an
// ISO string or the naive form some builds write, which is UTC.
func TestIndexColumnsAndTimeFormats(t *testing.T) {
	root := t.TempDir()
	writeSteps(t, root, "antigravity-cli", "a1", `{"step_index":0,"type":"USER_INPUT","content":"hello"}`)
	writeSteps(t, root, "antigravity-cli", "a2", `{"step_index":0,"type":"USER_INPUT","content":"hello"}`)
	writeSteps(t, root, "antigravity-cli", "a3", `{"step_index":0,"type":"USER_INPUT","content":"hello"}`)
	execIndex(t, root, "antigravity-cli",
		`CREATE TABLE conversation_summaries(conversation_id TEXT, title TEXT, last_modified_time TEXT)`,
		`INSERT INTO conversation_summaries VALUES ('a1', 'one', '2026-10-12 10:00:09.250')`,
		`INSERT INTO conversation_summaries VALUES ('a2', 'two', '2026-10-12T10:00:09+02:00')`,
		`INSERT INTO conversation_summaries VALUES ('a3', NULL, NULL)`,
		`INSERT INTO conversation_summaries VALUES ('../escape', 'bad', NULL)`)
	all := sourcetest.All(t, Spec.Open(root))
	if len(all) != 3 {
		t.Fatalf("sessions = %d", len(all))
	}
	sourcetest.NothingUnread(t, all...)
	if got, want := sourcetest.ByID(t, all, "a1").Session.UpdatedAt, time.Date(2026, 10, 12, 10, 0, 9, 250_000_000, time.UTC); !got.Equal(want) {
		t.Errorf("naive time = %v, want %v", got, want)
	}
	if got, want := sourcetest.ByID(t, all, "a2").Session.UpdatedAt, time.Date(2026, 10, 12, 8, 0, 9, 0, time.UTC); !got.Equal(want) {
		t.Errorf("zoned time = %v, want %v", got, want)
	}
	if s := sourcetest.ByID(t, all, "a3"); s.Session.Title != "hello" || s.Session.Cwd != "" {
		t.Errorf("a row of nulls names nothing: %+v", s.Session)
	}
}

func TestOnlyConversationsAreListed(t *testing.T) {
	root := t.TempDir()
	writeSteps(t, root, "antigravity-cli", "good-1", `{"step_index":0,"type":"USER_INPUT","content":"hi"}`)
	writeSteps(t, root, "antigravity-cli", "has space", `{"step_index":0,"type":"USER_INPUT","content":"hi"}`)
	writeSteps(t, root, "antigravity-cli", "..hidden", `{"step_index":0,"type":"USER_INPUT","content":"hi"}`)
	// a conversation with only the binary store: nothing to read
	if err := os.MkdirAll(filepath.Join(root, "antigravity-cli", "brain", "binary-only", "implicit"), 0o755); err != nil {
		t.Fatal(err)
	}
	// an id in both brains is the CLI's
	writeSteps(t, root, "antigravity", "good-1", `{"step_index":0,"type":"USER_INPUT","content":"the IDE's"}`)
	if err := os.WriteFile(filepath.Join(root, "antigravity-cli", "brain", "stray-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "good-1" || s.Turn(t, 1).Ask != "hi" {
		t.Errorf("session = %+v", s.Session)
	}
}

func TestWorkspace(t *testing.T) {
	for uris, want := range map[string]string{
		`["file:///work/repo"]`:               filepath.FromSlash("/work/repo"),
		`["file:///work/my%20repo","x"]`:      filepath.FromSlash("/work/my repo"),
		`["vscode-remote://x","file:///a/b"]`: filepath.FromSlash("/a/b"), // the first file:// URI
		`["file:///C:/work/app"]`:             filepath.FromSlash("C:/work/app"),
		`["https://example.com"]`:             "",
		`[]`:                                  "",
		``:                                    "",
		`not json`:                            "",
	} {
		if got := workspace(uris); got != want {
			t.Errorf("workspace(%s) = %q, want %q", uris, got, want)
		}
	}
}

func TestMissingBrains(t *testing.T) {
	src := Spec.Open(t.TempDir())
	if src.Exists() {
		t.Error("no brain, Exists must say so")
	}
	if refs := scan(t, src); len(refs) != 0 {
		t.Errorf("an empty gemini dir listed %d sessions", len(refs))
	}
	// the IDE alone is enough
	root := t.TempDir()
	writeSteps(t, root, "antigravity", "ide-1", `{"step_index":0,"type":"USER_INPUT","content":"hi"}`)
	if src := Spec.Open(root); !src.Exists() || len(scan(t, src)) != 1 {
		t.Error("the IDE's brain alone is a store")
	}
}

func TestSpec(t *testing.T) {
	if got := Spec.Locate(sourcetest.Env("/home/u")); got != filepath.Join("/home/u", ".gemini") {
		t.Errorf("default = %s", got)
	}
	if Spec.Name != "antigravity" || Spec.Title != "Antigravity" || !Spec.Movable || Spec.Resume != "agy --conversation" {
		t.Errorf("spec = %+v", Spec)
	}
}

// agy changes what it writes. A field of a type this did not expect costs that field, not
// the step it is on — its words and its tokens are still there.
func TestAStepWithAnOddlyTypedField(t *testing.T) {
	root := t.TempDir()
	writeSteps(t, root, "antigravity-cli", "sess-odd",
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-10-12T10:00:00Z","content":"<USER_REQUEST>what changed?</USER_REQUEST>"}`,
		// created_at as a number, model as an object: neither is what the others write
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":1791799201,"model":{"id":"gemini-3"},"content":"Two files changed.","input_tokens":120,"output_tokens":30}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if turn.Answer != "Two files changed." || s.Stats.Usage.Input != 120 || s.Stats.Usage.Output != 30 {
		t.Errorf("answer %q, usage %+v: the step was dropped for one field's sake", turn.Answer, s.Stats.Usage)
	}
	if turn.Items[len(turn.Items)-1].At.IsZero() {
		t.Error("a time written as a number is still a time")
	}
}

// A step of a type agy had not written when this reader was made is shown under its type and
// counted as unread, once however often it was rewritten; the steps around it, and a tool
// step of a type this reader knows, read as they always have.
func TestAStepOfAnUnknownTypeIsCounted(t *testing.T) {
	root := t.TempDir()
	writeSteps(t, root, "antigravity-cli", "sess-new",
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-12T10:00:00Z","content":"<USER_REQUEST>tidy the repo</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:01Z","content":"looking","input_tokens":40,"output_tokens":4}`,
		`{"step_index":2,"source":"MODEL","type":"HOLOGRAM_PROJECTION","status":"RUNNING","created_at":"2026-10-12T10:00:02Z","content":"warming up"}`,
		`{"step_index":3,"source":"MODEL","type":"VIEW_FILE","status":"DONE","created_at":"2026-10-12T10:00:03Z","content":"package main"}`,
		`{"step_index":2,"source":"MODEL","type":"HOLOGRAM_PROJECTION","status":"DONE","created_at":"2026-10-12T10:00:04Z","content":"projected"}`,
		`{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:05Z","content":"tidied","input_tokens":50,"output_tokens":6}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	if len(s.Unread) != 1 || s.Unread["step:HOLOGRAM_PROJECTION"] != 1 {
		t.Errorf("unread = %v, want step:HOLOGRAM_PROJECTION once", s.Unread)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt reply event event reply" {
		t.Fatalf("items = %s", got)
	}
	if ev := turn.Items[2]; ev.Event != "HOLOGRAM_PROJECTION" || ev.Text != "projected" {
		t.Errorf("unknown step = %+v, want an event naming it, at its latest line", ev)
	}
	if ev := turn.Items[3]; ev.Event != "VIEW_FILE" || ev.Text != "package main" {
		t.Errorf("tool step = %+v", ev)
	}
	if turn.Ask != "tidy the repo" || turn.Answer != "tidied" || s.Stats.Usage != (model.Usage{Input: 90, Output: 10}) {
		t.Errorf("ask %q, answer %q, usage %+v", turn.Ask, turn.Answer, s.Stats.Usage)
	}
}

// A counter no model call could have spent is not a count, and counts 0 — not the most a
// count can be: taken as it is, two of them sum past the largest total and wrap it below
// zero, and one below zero takes a total down. A counter at model.MaxTokens is still one.
// The rest of the step is read as it is.
func TestACounterPastAnyCountIsNotCounted(t *testing.T) {
	root := t.TempDir()
	writeSteps(t, root, "antigravity-cli", "sess-huge",
		`{"step_index":0,"type":"USER_INPUT","content":"<USER_REQUEST>go</USER_REQUEST>"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","content":"one","input_tokens":9e18,"output_tokens":7}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"two","input_tokens":1e300,"output_tokens":5,"cache_read_tokens":3}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","content":"three","input_tokens":-40,"output_tokens":1099511627777,"cache_write_tokens":1099511627776}`,
		`{"step_index":4,"type":"PLANNER_RESPONSE","status":"DONE","content":"four","input_tokens":-1e300,"output_tokens":-2}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)
	sourcetest.NothingUnread(t, s)
	if want := (model.Usage{Output: 12, CacheRead: 3, CacheWrite: model.MaxTokens}); s.Stats.Usage != want {
		t.Errorf("usage = %+v, want %+v", s.Stats.Usage, want)
	}
	turn := s.Turn(t, 1)
	if turn.Answer != "four" || len(turn.Calls) != 4 {
		t.Fatalf("answer %q, calls %d: the steps are still there", turn.Answer, len(turn.Calls))
	}
	if turn.Calls[3].Usage != nil {
		t.Errorf("a step with nothing it could have spent has no usage: %+v", turn.Calls[3].Usage)
	}
}

// NOTIFY_USER and ERROR_MESSAGE are, by their names, agy's word to the person and a failure
// of its own, and neither has been seen in a transcript. Shown as an event like a tool's
// step, either would be misread with nothing to say so; each is shown under its type and
// counted as unread until what it holds is known. Neither is passed off as the reply.
func TestNoticesAndErrorsAreCounted(t *testing.T) {
	root := t.TempDir()
	writeSteps(t, root, "antigravity-cli", "sess-task",
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-12T10:00:00Z","content":"<USER_REQUEST>ship it</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:01Z","content":"working","input_tokens":30,"output_tokens":3}`,
		`{"step_index":2,"source":"MODEL","type":"ERROR_MESSAGE","status":"DONE","created_at":"2026-10-12T10:00:02Z","content":"quota exceeded"}`,
		`{"step_index":3,"source":"MODEL","type":"NOTIFY_USER","status":"GENERATING","created_at":"2026-10-12T10:00:03Z","content":"Shipped"}`,
		`{"step_index":3,"source":"MODEL","type":"NOTIFY_USER","status":"DONE","created_at":"2026-10-12T10:00:04Z","content":"Shipped; the plan is ready for review."}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)
	if len(s.Unread) != 2 || s.Unread["step:NOTIFY_USER"] != 1 || s.Unread["step:ERROR_MESSAGE"] != 1 {
		t.Errorf("unread = %v, want step:NOTIFY_USER and step:ERROR_MESSAGE once each", s.Unread)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt reply event event" {
		t.Fatalf("items = %s", got)
	}
	if ev := turn.Items[2]; ev.Event != "ERROR_MESSAGE" || ev.Text != "quota exceeded" {
		t.Errorf("error step = %+v", ev)
	}
	if ev := turn.Items[3]; ev.Event != "NOTIFY_USER" || ev.Text != "Shipped; the plan is ready for review." {
		t.Errorf("notice step = %+v, want it at its latest line", ev)
	}
	if turn.Answer != "working" {
		t.Errorf("answer %q", turn.Answer)
	}
}
