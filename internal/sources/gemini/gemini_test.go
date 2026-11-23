package gemini

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

func scan(t *testing.T, src sources.Source) []sources.Ref {
	t.Helper()
	refs, err := src.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestSession(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "projA", "chats", "session-2026-09-13T12-50-41.jsonl")
	sourcetest.Write(t, path,
		`{"sessionId":"g-1","startTime":"2026-09-13T12:50:41Z","lastUpdated":"2026-09-13T12:55:00Z"}`,
		`{"$set":{"messages":[{"type":"user","id":"gu1","timestamp":"t1","content":[{"text":"hey"}]}]}}`,
		`{"type":"gemini","id":"gg1","timestamp":"t2","model":"gemini-2.5","thoughts":"mulling","tokens":{"input":3},"content":"hello"}`,
		// A tool-call turn: content is the empty string, the substance is in toolCalls, and
		// thoughts is an array (its description is taken)
		`{"type":"gemini","id":"gg2","timestamp":"t3","model":"gemini-2.5","content":"","thoughts":[{"subject":"locate the file","description":"check package.json first"}],"toolCalls":[{"id":"c1","name":"read_file","args":{"file_path":"package.json"},"result":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"..."}}}]}]}`,
		// The user row carries the tool result back
		`{"type":"user","id":"gu2","timestamp":"t4","content":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"{\"name\":\"larkin\"}"}}}]}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)

	if s.Session.ID != "g-1" || s.Session.Path != path {
		t.Errorf("session = %+v", s.Session)
	}
	// No working directory is recorded, only the project the session is filed under
	if s.Session.Cwd != "projA" {
		t.Errorf("cwd = %q, want the project directory", s.Session.Cwd)
	}
	if s.Session.Title != "hey" {
		t.Errorf("title = %q: Gemini keeps none, the first ask stands in", s.Session.Title)
	}
	// None of the rows has a time that parses: the metadata's is what there is, never the
	// time of the first line's start
	if want := time.Date(2026, 9, 13, 12, 55, 0, 0, time.UTC); !s.Session.UpdatedAt.Equal(want) {
		t.Errorf("updated = %v, want the metadata's %v", s.Session.UpdatedAt, want)
	}
	if want := time.Date(2026, 9, 13, 12, 50, 41, 0, time.UTC); !s.Session.StartedAt.Equal(want) {
		t.Errorf("started = %v, want %v", s.Session.StartedAt, want)
	}
	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d", len(s.Turns))
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt thinking reply thinking tool" {
		t.Errorf("items = %s", got)
	}
	if turn.Ask != "hey" || turn.Answer != "hello" {
		t.Errorf("ask %q answer %q", turn.Ask, turn.Answer)
	}
	// a thoughts string and a thoughts array both become thinking, ahead of the row's words
	if turn.Items[1].Text != "mulling" || turn.Items[3].Text != "check package.json first" {
		t.Errorf("thinking = %q / %q", turn.Items[1].Text, turn.Items[3].Text)
	}
	tools := sourcetest.Tools(turn)
	if len(tools) != 1 {
		t.Fatalf("tools = %d: the user row's functionResponse answers the call, it is not a call", len(tools))
	}
	tool := tools[0]
	if tool.Name != "read_file" || tool.CallID != "c1" || !tool.Done || tool.Output != `{"name":"larkin"}` || tool.Status != model.StatusOK {
		t.Errorf("tool = %+v", tool)
	}
	if len(tool.Files) != 1 || tool.Files[0] != "package.json" {
		t.Errorf("args were lost: files = %v", tool.Files)
	}
	if len(turn.Calls) != 2 || turn.Calls[0].Model != "gemini-2.5" || turn.Calls[1].Model != "gemini-2.5" {
		t.Errorf("calls = %+v", turn.Calls)
	}
	if s.Stats.Usage != (model.Usage{Input: 3}) {
		t.Errorf("usage = %+v", s.Stats.Usage)
	}
	if s.Session.Model != "gemini-2.5" {
		t.Errorf("model = %q", s.Session.Model)
	}
}

// One conversation can span several files: Gemini CLI continues a session in a new file
// with the same sessionId (measured: two files one minute apart). They are one session —
// one Ref, messages read from both files in filename order — not two rows that lead to the
// same place.
func TestContinuationFilesAreOneConversation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "chats")
	first := filepath.Join(dir, "session-2026-09-14T03-16-80e8c90b.jsonl")
	sourcetest.Write(t, first,
		`{"sessionId":"sid-80e8c90b","startTime":"2026-09-14T03:16:50Z"}`,
		`{"type":"user","content":[{"text":"帮我连一下vpn"}],"timestamp":"2026-09-14T03:16:52Z"}`,
	)
	// written later, but listed by the glob first: the order of a conversation is its names'
	sourcetest.Write(t, filepath.Join(dir, "session-2026-09-14T03-17-80e8c90b.jsonl"),
		`{"sessionId":"sid-80e8c90b","startTime":"2026-09-14T03:17:49Z"}`,
		`{"type":"gemini","content":"vpn 连好了","timestamp":"2026-09-14T03:18:01Z"}`,
	)
	src := Spec.Open(root)

	refs := scan(t, src)
	if len(refs) != 1 {
		t.Fatalf("two continuation files must be one session, got %d refs", len(refs))
	}
	if refs[0].Key != first {
		t.Errorf("key = %q, want the earliest file", refs[0].Key)
	}
	s := sourcetest.Read(t, src, refs[0])
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "sid-80e8c90b" {
		t.Errorf("id = %q", s.Session.ID)
	}
	if s.Stats.Turns != 1 || s.Turn(t, 1).Ask != "帮我连一下vpn" || s.Turn(t, 1).Answer != "vpn 连好了" {
		t.Errorf("the session must be read through both files: %+v", s.Turns)
	}
	// The session began in the first file and was last written in the second
	if want := time.Date(2026, 9, 14, 3, 16, 50, 0, time.UTC); !s.Session.StartedAt.Equal(want) {
		t.Errorf("started = %v, want the first file's start", s.Session.StartedAt)
	}
	if want := time.Date(2026, 9, 14, 3, 18, 1, 0, time.UTC); !s.Session.UpdatedAt.Equal(want) {
		t.Errorf("updated = %v, want the last row's time", s.Session.UpdatedAt)
	}
}

// A Read that was not preceded by a Scan of this Source finds the other files of its
// conversation for itself.
func TestReadWithoutAScan(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "chats")
	sourcetest.Write(t, filepath.Join(dir, "session-2026-09-14T03-16-aaaa.jsonl"),
		`{"sessionId":"s1"}`, `{"type":"user","content":[{"text":"one"}]}`)
	sourcetest.Write(t, filepath.Join(dir, "session-2026-09-14T03-17-aaaa.jsonl"),
		`{"sessionId":"s1"}`, `{"type":"gemini","content":"two"}`)
	refs := scan(t, Spec.Open(root))

	s := sourcetest.Read(t, Spec.Open(root), refs[0])
	sourcetest.NothingUnread(t, s)
	if s.Turn(t, 1).Ask != "one" || s.Turn(t, 1).Answer != "two" {
		t.Errorf("turns = %+v", s.Turns)
	}
}

// Newer Gemini CLI answers a call on the toolCalls entry itself — a status, and the output
// as resultDisplay or under result — while older files answer on the user row that follows
// (and echo the response under result without the output, which is why a result field
// alone is not an answer, see TestSession). A file carrying both shows each result once,
// with the status the entry recorded.
func TestInlineToolStatus(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "projB", "chats", "session-2026-10-01T10-00-00.jsonl"),
		`{"sessionId":"g-2","startTime":"2026-10-01T10:00:00Z","lastUpdated":"2026-10-01T10:05:00Z"}`,
		`{"type":"user","id":"u1","timestamp":"t1","content":[{"text":"build it"}]}`,
		`{"type":"gemini","id":"g1","timestamp":"t2","content":"","toolCalls":[`+
			`{"id":"run-1","name":"run_shell_command","args":{"command":"npm run build"},"status":"error","resultDisplay":"npm ERR! missing script"},`+
			`{"id":"rep-1","name":"replace","args":{"file_path":"config.ts","old_string":"a","new_string":"b"},"status":"success","resultDisplay":{"fileName":"config.ts","fileDiff":"--- a\n+++ b\n-a\n+b\n"}},`+
			`{"id":"cancel-1","name":"run_shell_command","args":{"command":"sleep 9"},"status":"cancelled","result":[{"functionResponse":{"id":"cancel-1","name":"run_shell_command","response":{"error":"Command was cancelled by the user."}}}]},`+
			`{"id":"late-1","name":"read_file","args":{"file_path":"x"},"status":"success"},`+
			`{"id":"live-1","name":"run_shell_command","args":{"command":"tail -f log"},"status":"executing"}]}`,
		// The user row repeats run-1 (already answered: skipped) and brings late-1's
		// output (the status came earlier without output: applied here)
		`{"type":"user","id":"u2","timestamp":"t3","content":[{"functionResponse":{"id":"run-1","name":"run_shell_command","response":{"output":"npm ERR! missing script"}}},{"functionResponse":{"id":"late-1","name":"read_file","response":{"output":"the file"}}}]}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt tool tool tool tool tool" {
		t.Fatalf("items = %s: each call once, and the user row's copy of run-1 adds nothing", got)
	}
	tools := map[string]*model.Tool{}
	for _, tool := range sourcetest.Tools(turn) {
		tools[tool.CallID] = tool
	}
	if r := tools["run-1"]; r.Status != model.StatusError || r.Output != "npm ERR! missing script" || r.Name != "run_shell_command" || r.Command != "npm run build" {
		t.Errorf("failed command = %+v", r)
	}
	if r := tools["rep-1"]; r.Status != model.StatusOK || !strings.Contains(r.Output, "+b") {
		t.Errorf("replace = %+v", r)
	}
	if r := tools["cancel-1"]; r.Status != model.StatusInterrupted || r.Output != "Command was cancelled by the user." {
		t.Errorf("cancelled = %+v", r)
	}
	if r := tools["late-1"]; r.Status != model.StatusOK || r.Output != "the file" {
		t.Errorf("the user row must bring late-1's output with the status the call recorded: %+v", r)
	}
	// Still running when the file was written: no result, and no verdict
	if r := tools["live-1"]; r.Done || r.Status != "" {
		t.Errorf("a call still executing = %+v", r)
	}
	if turn.Tools != 5 || turn.Failures != 2 {
		t.Errorf("tools %d failures %d: an error and a cancellation fail, nothing else", turn.Tools, turn.Failures)
	}
	// An edit that succeeded changed its file; a read did not
	files := map[string]bool{}
	for _, f := range turn.Files {
		files[f.Path] = f.Changed
	}
	if changed, ok := files["config.ts"]; !ok || !changed || files["x"] {
		t.Errorf("files = %+v", turn.Files)
	}
}

// The counters overlap in Gemini's usage metadata: input holds the cached tokens, output
// leaves the thoughts out. The canonical shape adds up to the total the row records.
func TestUsageIsConvertedToTheCanonicalShape(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "chats", "session-2026-10-01T10-00-00.jsonl"),
		`{"sessionId":"g-3"}`,
		`{"type":"user","timestamp":"2026-10-01T10:00:00Z","content":[{"text":"think about it"}]}`,
		// measured: nothing cached, and 396 tokens of thinking beside 133 of answer
		`{"type":"gemini","id":"a","timestamp":"2026-10-01T10:00:01Z","model":"gemini-2.5-pro","content":"one","tokens":{"input":15597,"output":133,"cached":0,"thoughts":396,"tool":0,"total":16126}}`,
		// a cache hit, and a search result that was added to the prompt
		`{"type":"gemini","id":"b","timestamp":"2026-10-01T10:00:02Z","model":"gemini-2.5-pro","content":"two","tokens":{"input":1000,"output":50,"cached":700,"thoughts":200,"tool":30,"total":1280}}`,
		// only counters, nothing said: still a model call
		`{"type":"gemini","id":"c","timestamp":"2026-10-01T10:00:03Z","model":"gemini-2.5-pro","content":"","tokens":{"input":10,"output":1,"cached":40,"thoughts":0,"tool":0,"total":11}}`,
		// no counters at all
		`{"type":"gemini","id":"d","timestamp":"2026-10-01T10:00:04Z","content":"three"}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	want := []model.Usage{
		{Input: 15597, Output: 133 + 396, Reasoning: 396},
		{Input: 300 + 30, Output: 250, CacheRead: 700, Reasoning: 200},
		{Input: 0, Output: 1, CacheRead: 40}, // cached over input is not a negative prompt
	}
	if len(turn.Calls) != 4 {
		t.Fatalf("calls = %d, want one per gemini row", len(turn.Calls))
	}
	for i, w := range want {
		if got := turn.Calls[i].Usage; got == nil || *got != w {
			t.Errorf("call %d usage = %+v, want %+v", i, got, w)
		}
	}
	if turn.Calls[3].Usage != nil {
		t.Errorf("a row without counters has no usage: %+v", turn.Calls[3].Usage)
	}
	// Each row's own total is the sum of the canonical counters
	if got := turn.Calls[0].Usage.Total(); got != 16126 {
		t.Errorf("total = %d, the row records 16126", got)
	}
	if got := turn.Calls[1].Usage.Total(); got != 1280 {
		t.Errorf("total = %d, the row records 1280", got)
	}
	// The model comes from the row, and carries to the one that names none
	if turn.Calls[3].Model != "gemini-2.5-pro" {
		t.Errorf("model = %q", turn.Calls[3].Model)
	}
}

// A counter below zero or past any model's reach is no count: as it stands, it puts a
// figure no session could reach in the totals, and two of them, added together as output
// and thoughts are, wrap round to below zero. It counts 0 (model.TokensOf) — not the bound,
// which would add a trillion tokens — and the row's other counters stand.
func TestTokenCountsPastAnyModelCountNothing(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "chats", "session-2026-10-01T10-00-00.jsonl"),
		`{"sessionId":"g-8"}`,
		`{"type":"user","id":"u","content":[{"text":"q"}]}`,
		`{"type":"gemini","id":"a","content":"x","tokens":{"input":9e18,"output":9e18,"thoughts":9e18,"tool":9e18}}`,
		`{"type":"gemini","id":"b","content":"y","tokens":{"input":1e30,"output":-1e30,"cached":5}}`,
		// the most a count can be, and one past it
		`{"type":"gemini","id":"c","content":"z","tokens":{"input":1099511627776,"output":1099511627777,"thoughts":-1,"cached":0}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)
	calls := s.Turn(t, 1).Calls
	if len(calls) != 3 {
		t.Fatalf("calls = %+v, want one per gemini row", calls)
	}
	// Nothing left of the first row's counters: the call is still a call, with no usage
	if calls[0].Usage != nil {
		t.Errorf("usage = %+v, want none", *calls[0].Usage)
	}
	for i, want := range []model.Usage{{CacheRead: 5}, {Input: model.MaxTokens}} {
		if got := calls[i+1].Usage; got == nil || *got != want {
			t.Errorf("call %d usage = %+v, want %+v", i+1, got, want)
		}
	}
	if got, want := s.Stats.Usage, (model.Usage{Input: model.MaxTokens, CacheRead: 5}); got != want {
		t.Errorf("session usage = %+v, want %+v", got, want)
	}
}

// The first line's lastUpdated is its value when the file was opened; patch rows move it
// on. The session's update time is the time on the last row that has one.
func TestUpdateTimeIsReadFromTheLastRow(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "chats", "session-2026-10-01T10-00-00.jsonl"),
		`{"sessionId":"g-4","startTime":"2026-10-01T10:00:00.000Z","lastUpdated":"2026-10-01T10:00:00.000Z"}`,
		`{"type":"user","id":"u","timestamp":"2026-10-01T10:00:05.000Z","content":[{"text":"hi"}]}`,
		`{"$set":{"lastUpdated":"2026-10-01T10:00:09.000Z"}}`,
		`{"type":"gemini","id":"g","timestamp":"2026-10-01T10:00:10.000Z","content":"hello"}`,
		`{"$set":{"lastUpdated":"2026-10-01T10:30:00.000Z"}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if want := time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC); !s.Session.UpdatedAt.Equal(want) {
		t.Errorf("updated = %v, want the last patch's %v", s.Session.UpdatedAt, want)
	}
}

func TestRowsThatAreNotTheConversation(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "chats", "session-2026-10-01T10-00-00.jsonl"),
		`{"sessionId":"g-5","startTime":"2026-10-01T10:00:00Z"}`,
		`{"type":"info","id":"i","timestamp":"2026-10-01T10:00:01Z","content":"update available"}`,
		`not json at all`,
		``,
		`{"$set":{"summary":"nothing to see"}}`,
		`{"type":"user","id":"u","timestamp":"2026-10-01T10:00:02Z","content":"a plain string content"}`,
		`{"type":"error","id":"e","timestamp":"2026-10-01T10:00:03Z","content":"quota"}`,
		`{"type":"gemini","id":"g","timestamp":"2026-10-01T10:00:04Z","content":"ok"}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	// Info and error rows are known for what they are, and a line that is not JSON is no
	// record at all: nothing here is of a kind the adapter does not know
	sourcetest.NothingUnread(t, s)
	if s.Stats.Turns != 1 || s.Turn(t, 1).Ask != "a plain string content" || s.Turn(t, 1).Answer != "ok" {
		t.Errorf("turns = %+v", s.Turns)
	}
	// An info row is the CLI's own; an error is an event, and one the model answered after
	// ended nothing
	if got := sourcetest.Kinds(s.Turn(t, 1)); got != "prompt event reply" || s.Turn(t, 1).Interrupted {
		t.Errorf("items = %s, interrupted %v", got, s.Turn(t, 1).Interrupted)
	}
}

// A file whose first line is a message has no metadata to name its session by; it is a
// session of its own, under its file's name, and does not join another.
func TestAFileWithoutMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "chats", "session-2026-10-01T10-00-00-ab12.jsonl")
	sourcetest.Write(t, path, `{"type":"user","timestamp":"2026-10-01T10:00:00Z","content":[{"text":"hello"}]}`)
	sourcetest.Write(t, filepath.Join(root, "proj", "chats", "session-2026-10-01T11-00-00-cd34.jsonl"),
		`{"sessionId":"real"}`, `{"type":"user","content":[{"text":"other"}]}`)
	all := sourcetest.All(t, Spec.Open(root))
	if len(all) != 2 {
		t.Fatalf("sessions = %d", len(all))
	}
	sourcetest.NothingUnread(t, all...)
	s := sourcetest.ByID(t, all, "session-2026-10-01T10-00-00-ab12")
	if s.Turn(t, 1).Ask != "hello" || s.Session.Cwd != "proj" {
		t.Errorf("session = %+v", s.Session)
	}
	if want := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC); !s.Session.UpdatedAt.Equal(want) {
		t.Errorf("updated = %v", s.Session.UpdatedAt)
	}
}

// Scan reads no transcript, and a stamp moves when — and only when — its own conversation
// does: a line appended to a continuation file counts, another session's does not.
func TestScanStamps(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "chats")
	a1 := filepath.Join(dir, "session-2026-10-01T10-00-aaaa.jsonl")
	a2 := filepath.Join(dir, "session-2026-10-01T11-00-aaaa.jsonl")
	b := filepath.Join(dir, "session-2026-10-01T12-00-bbbb.jsonl")
	sourcetest.Write(t, a1, `{"sessionId":"a"}`, `{"type":"user","content":[{"text":"q"}]}`)
	sourcetest.Write(t, a2, `{"sessionId":"a"}`, `{"type":"gemini","content":"r"}`)
	sourcetest.Write(t, b, `{"sessionId":"b"}`, `{"type":"user","content":[{"text":"q"}]}`)
	src := Spec.Open(root)

	stamps := func() map[string]string {
		out := map[string]string{}
		for _, ref := range scan(t, src) {
			out[ref.Key] = ref.Stamp
		}
		return out
	}
	before := stamps()
	if len(before) != 2 {
		t.Fatalf("refs = %v", before)
	}
	if again := stamps(); again[a1] != before[a1] || again[b] != before[b] {
		t.Errorf("an untouched store changed its stamps: %v → %v", before, again)
	}

	appendTo(t, a2, `{"type":"gemini","content":"more"}`+"\n")
	after := stamps()
	if after[a1] == before[a1] {
		t.Error("a line appended to a continuation file left its conversation's stamp where it was")
	}
	if after[b] != before[b] {
		t.Error("another session's stamp moved")
	}

	// A new continuation file joins the conversation: its stamp moves, and its key does not
	sourcetest.Write(t, filepath.Join(dir, "session-2026-10-01T13-00-aaaa.jsonl"), `{"sessionId":"a"}`)
	joined := stamps()
	if len(joined) != 2 || joined[a1] == after[a1] {
		t.Errorf("after a third file: %v", joined)
	}
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// A file's first line is read when its fingerprint is new and not again: which files
// belong together is worked out on every scan, and an untouched store must not be opened.
func TestFirstLinesAreReadOncePerFingerprint(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "chats")
	one := filepath.Join(dir, "session-2026-10-01T10-00-0001.jsonl")
	two := filepath.Join(dir, "session-2026-10-01T11-00-0002.jsonl")
	sourcetest.Write(t, one, `{"sessionId":"aaaa-1"}`, `{"type":"user","content":[{"text":"q"}]}`)
	sourcetest.Write(t, two, `{"sessionId":"bbbb-1"}`, `{"type":"user","content":[{"text":"q"}]}`)
	src := Spec.Open(root)
	if refs := scan(t, src); len(refs) != 2 {
		t.Fatalf("refs = %d", len(refs))
	}

	// Give the first file the other's session id, at the size and time it had: nothing
	// says it changed, so what was read stands
	info, err := os.Stat(one)
	if err != nil {
		t.Fatal(err)
	}
	sourcetest.Write(t, one, `{"sessionId":"bbbb-1"}`, `{"type":"user","content":[{"text":"q"}]}`)
	if err := os.Chtimes(one, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if refs := scan(t, src); len(refs) != 2 {
		t.Fatalf("refs = %d: the first line of an unchanged file was read again", len(refs))
	}
	// A new fingerprint is read
	appendTo(t, one, `{"type":"gemini","content":"a"}`+"\n")
	if refs := scan(t, src); len(refs) != 1 {
		t.Fatalf("refs = %d: a changed file's first line must be read again", len(refs))
	}
}

func TestSpec(t *testing.T) {
	if got := Spec.Locate(sourcetest.Env("/home/u")); got != filepath.Join("/home/u", ".gemini", "tmp") {
		t.Errorf("default = %s", got)
	}
	// no variable of Gemini CLI's moves it
	if got := Spec.Locate(sourcetest.Env("/home/u", "GEMINI_HOME", "/x")); got != filepath.Join("/home/u", ".gemini", "tmp") {
		t.Errorf("with a variable = %s", got)
	}
	// --resume takes a session's id from the session's own directory (measured, 0.68)
	if Spec.Name != "gemini" || Spec.Title != "Gemini CLI" || !Spec.Movable || Spec.Resume != "gemini --resume" {
		t.Errorf("spec = %+v", Spec)
	}
	missing := Spec.Open(filepath.Join(t.TempDir(), "absent"))
	if missing.Exists() || len(scan(t, missing)) != 0 {
		t.Error("a store that is not there lists nothing")
	}
}

// A file that continues a conversation can open with a patch and name its session on the
// line after. The scan has to find that id, or the two files of one conversation list as
// two sessions that both read as it.
func TestSessionIDNotOnTheFirstLine(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projA", "chats")
	sourcetest.Write(t, filepath.Join(dir, "session-2026-09-13T12-50-41.jsonl"),
		`{"sessionId":"g-1","startTime":"2026-09-13T12:50:41Z","lastUpdated":"2026-09-13T12:55:00Z"}`,
		`{"type":"user","id":"gu1","timestamp":"2026-09-13T12:50:42Z","content":[{"text":"first question"}]}`,
		`{"type":"gemini","id":"gg1","timestamp":"2026-09-13T12:50:50Z","model":"gemini-2.5","content":"first answer"}`)
	sourcetest.Write(t, filepath.Join(dir, "session-2026-09-13T13-10-00.jsonl"),
		`{"$set":{"lastUpdated":"2026-09-13T13:10:00Z"}}`,
		`{"sessionId":"g-1","startTime":"2026-09-13T12:50:41Z","lastUpdated":"2026-09-13T13:12:00Z"}`,
		`{"type":"user","id":"gu2","timestamp":"2026-09-13T13:10:01Z","content":[{"text":"second question"}]}`,
		`{"type":"gemini","id":"gg2","timestamp":"2026-09-13T13:10:09Z","model":"gemini-2.5","content":"second answer"}`)
	src := Spec.Open(root)
	if refs := scan(t, src); len(refs) != 1 {
		t.Fatalf("refs = %d: both files name session g-1, and are one conversation", len(refs))
	}
	s := sourcetest.One(t, src)
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "g-1" || len(s.Turns) != 2 || s.Turn(t, 2).Ask != "second question" {
		t.Errorf("session %q with %d turns", s.Session.ID, len(s.Turns))
	}
}

// A message written twice is one message: what its id last said, where it first stood.
// Read as two, a response's tokens are spent twice and its call is made twice.
func TestAMessageWrittenAgainIsStillOneMessage(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "projA", "chats", "session-2026-09-13T12-50-41.jsonl"),
		`{"sessionId":"g-2","startTime":"2026-09-13T12:50:41Z","lastUpdated":"2026-09-13T12:50:41Z"}`,
		`{"type":"user","id":"gu1","timestamp":"2026-09-13T12:50:42Z","content":[{"text":"read the config"}]}`,
		// the call as first written: no status yet
		`{"type":"gemini","id":"gg1","timestamp":"2026-09-13T12:50:45Z","model":"gemini-2.5","content":"","tokens":{"input":100,"output":20},"toolCalls":[{"id":"c1","name":"read_file","args":{"file_path":"config.json"}}]}`,
		// … and again once it finished, with its outcome
		`{"type":"gemini","id":"gg1","timestamp":"2026-09-13T12:50:45Z","model":"gemini-2.5","content":"","tokens":{"input":100,"output":20},"toolCalls":[{"id":"c1","name":"read_file","args":{"file_path":"config.json"},"status":"success","resultDisplay":"{\"port\":8080}"}]}`,
		`{"type":"gemini","id":"gg2","timestamp":"2026-09-13T12:50:50Z","model":"gemini-2.5","content":"The port is 8080.","tokens":{"input":150,"output":10}}`,
		// a patch that sets the messages again repeats what the rows already said
		`{"$set":{"lastUpdated":"2026-09-13T12:51:00Z","messages":[{"type":"user","id":"gu1","timestamp":"2026-09-13T12:50:42Z","content":[{"text":"read the config"}]},{"type":"gemini","id":"gg2","timestamp":"2026-09-13T12:50:50Z","model":"gemini-2.5","content":"The port is 8080.","tokens":{"input":150,"output":10}}]}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d: the ask written again is the same ask", len(s.Turns))
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt tool reply" {
		t.Errorf("items = %s", got)
	}
	if tool := sourcetest.Tools(turn)[0]; !tool.Done || tool.Status != model.StatusOK || tool.Output != `{"port":8080}` {
		t.Errorf("the call is what its row last said: %+v", tool)
	}
	if s.Stats.Calls != 2 || s.Stats.Usage.Input != 250 || s.Stats.Usage.Output != 30 {
		t.Errorf("usage = %+v in %d calls, want 250 in and 30 out in 2", s.Stats.Usage, s.Stats.Calls)
	}
	if want := time.Date(2026, 9, 13, 12, 51, 0, 0, time.UTC); !s.Session.UpdatedAt.Equal(want) {
		t.Errorf("updated = %v, want the patch's own time %v", s.Session.UpdatedAt, want)
	}
}

// Where a call's result comes back on the user row — no status on the call — the files
// after its functionResponse go with that result; a file the person sent goes with the
// ask, and one the model made with its words.
func TestFilesGoWithWhatCarriedThem(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "9f2c", "chats", "session-2026-11-02T09-00-1c6f2b8d.jsonl"),
		`{"sessionId":"1c6f2b8d","projectHash":"9f2c","startTime":"2026-11-02T09:00:00.000Z","lastUpdated":"2026-11-02T09:00:00.000Z","kind":"main"}`,
		`{"id":"u1","timestamp":"2026-11-02T09:00:01.000Z","type":"user","content":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}},{"fileData":{"mimeType":"application/pdf","fileUri":"/work/spec.pdf"}}]}`,
		`{"id":"g1","timestamp":"2026-11-02T09:00:02.000Z","type":"gemini","content":"","model":"gemini-3-pro-preview","toolCalls":[{"id":"shot-1","name":"take_screenshot","args":{}}],"tokens":{"input":10,"output":2,"total":12}}`,
		`{"id":"u2","timestamp":"2026-11-02T09:00:03.000Z","type":"user","content":[{"functionResponse":{"id":"shot-1","name":"take_screenshot","response":{"output":"Took it."}}},{"inlineData":{"mimeType":"image/jpeg","data":"/9j/4AAQ"}}]}`,
		// a call and its response that carry no id
		`{"id":"g1b","timestamp":"2026-11-02T09:00:03.500Z","type":"gemini","content":"","model":"gemini-3-pro-preview","toolCalls":[{"name":"read_file","args":{"file_path":"b.png"}}],"tokens":{"input":10,"output":2,"total":12}}`,
		`{"id":"u2b","timestamp":"2026-11-02T09:00:03.700Z","type":"user","content":[{"functionResponse":{"name":"read_file","response":{"output":"Read it."}}},{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}]}`,
		`{"id":"g2","timestamp":"2026-11-02T09:00:04.000Z","type":"gemini","content":[{"text":"Here it is, redrawn."},{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}],"model":"gemini-3-pro-image","tokens":{"input":20,"output":5,"total":25}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if turn.Ask != "[image · png · 8 bytes]\n[document · pdf · spec.pdf]" {
		t.Errorf("a picture and a document sent alone ask %q", turn.Ask)
	}
	if tools := sourcetest.Tools(turn); len(tools) != 2 || tools[0].Output != "Took it.\n[image · jpeg · 6 bytes]" || tools[1].Output != "Read it.\n[image · png · 8 bytes]" {
		t.Errorf("tools = %+v", tools)
	}
	if turn.Answer != "Here it is, redrawn.\n[image · png · 8 bytes]" {
		t.Errorf("answer %q", turn.Answer)
	}
}

// The rows as Gemini CLI writes them now: the metadata names its project and kind, a patch
// sets the workspace's directories, an attached image travels as an inlineData part, a
// call's result comes back on a user row of its own, an image a tool read is acknowledged
// by a synthetic thought and kept on an info row, a row rewritten from the model's history
// holds its thinking and its calls as parts, and /rewind takes the conversation back. All
// of it is a kind the adapter knows.
func TestTheFormatAsGeminiCLIWritesIt(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "9f2c", "chats", "session-2026-11-01T09-00-0b5e1a7c.jsonl"),
		`{"sessionId":"0b5e1a7c-3c1d","projectHash":"9f2c","startTime":"2026-11-01T09:00:00.000Z","lastUpdated":"2026-11-01T09:00:00.000Z","kind":"main"}`,
		`{"$set":{"directories":["/work/assets"]}}`,
		`{"id":"u1","timestamp":"2026-11-01T09:00:01.000Z","type":"user","content":[{"text":"what is in shot.png?"},{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}],"displayContent":[{"text":"what is in @shot.png?"}]}`,
		`{"$set":{"lastUpdated":"2026-11-01T09:00:01.000Z"}}`,
		// rewritten from the model's history: the thought and the call are parts beside the
		// thoughts and toolCalls fields that say the same
		`{"id":"g1","timestamp":"2026-11-01T09:00:04.000Z","type":"gemini","content":[{"text":"**Reading the notes**\n\nThey may say what the image is for.","thought":true},{"functionCall":{"id":"read-1","name":"read_file","args":{"file_path":"diagram.png"}},"thoughtSignature":"c2ln"}],`+
			`"thoughts":[{"subject":"Reading the notes","description":"They may say what the image is for.","timestamp":"2026-11-01T09:00:03.000Z"}],"model":"gemini-3-pro-preview","tokens":{"input":2000,"output":20,"cached":0,"thoughts":50,"tool":0,"total":2070},`+
			`"toolCalls":[{"id":"read-1","name":"read_file","args":{"file_path":"diagram.png"},"result":[{"functionResponse":{"id":"read-1","name":"read_file","response":{"output":"Binary content provided (1 item(s))."}}}],"status":"success","timestamp":"2026-11-01T09:00:04.000Z","resultDisplay":"Read image file: diagram.png","displayName":"ReadFile","description":"diagram.png","renderOutputAsMarkdown":true}]}`,
		`{"id":"u2","timestamp":"2026-11-01T09:00:04.100Z","type":"user","content":[{"functionResponse":{"id":"read-1","name":"read_file","response":{"output":"Binary content provided (1 item(s))."}}},{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}]}`,
		`{"id":"g2","timestamp":"2026-11-01T09:00:04.200Z","type":"gemini","content":[{"text":"Binary content received. Proceeding with analysis.","thought":true,"thoughtSignature":"skip_thought_signature_validator"}],"thoughts":[],"tokens":null}`,
		`{"id":"i1","timestamp":"2026-11-01T09:00:04.300Z","type":"info","content":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}]}`,
		`{"id":"g3","timestamp":"2026-11-01T09:00:09.000Z","type":"gemini","content":"It is a login form.","thoughts":[{"subject":"Describing it","description":"Two fields and a button.","timestamp":"2026-11-01T09:00:08.000Z"}],"model":"gemini-3-pro-preview","tokens":{"input":2400,"output":8,"cached":2000,"thoughts":30,"tool":0,"total":2438}}`,
		`{"id":"u3","timestamp":"2026-11-01T09:01:00.000Z","type":"user","content":[{"text":"make the button blue"}]}`,
		`{"id":"g4","timestamp":"2026-11-01T09:01:05.000Z","type":"gemini","content":"Done: the button is blue.","thoughts":[],"model":"gemini-3-pro-preview","tokens":{"input":500,"output":9,"cached":2000,"thoughts":0,"tool":0,"total":2509}}`,
		`{"$rewindTo":"u3"}`,
		`{"id":"w1","timestamp":"2026-11-01T09:01:30.000Z","type":"warning","content":"Rewound the conversation."}`,
		`{"id":"u4","timestamp":"2026-11-01T09:02:00.000Z","type":"user","content":[{"text":"make the button green"}]}`,
		`{"id":"g5","timestamp":"2026-11-01T09:02:04.000Z","type":"gemini","content":"Done: the button is green.","thoughts":[],"model":"gemini-3-pro-preview","tokens":{"input":510,"output":9,"cached":2000,"thoughts":0,"tool":0,"total":2519}}`,
		`{"id":"e1","timestamp":"2026-11-01T09:02:10.000Z","type":"error","content":"Quota exceeded."}`,
		`{"$set":{"lastUpdated":"2026-11-01T09:02:10.000Z","summary":"Recolour the login button"}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)

	// What was rewound was still said, done and paid for, and is read
	if len(s.Turns) != 3 || s.Turn(t, 2).Answer != "Done: the button is blue." || s.Turn(t, 3).Ask != "make the button green" {
		t.Fatalf("turns = %+v", s.Turns)
	}
	turn := s.Turn(t, 1)
	// The ask is what was typed, and the picture it named; what the CLI sent in its place,
	// the model read too
	if turn.Ask != "what is in @shot.png?\n[image · png · 8 bytes]" || turn.Answer != "It is a login form." {
		t.Errorf("ask %q answer %q", turn.Ask, turn.Answer)
	}
	// The thought part of a row with thoughts of its own is not read twice, the call part
	// is the toolCalls entry, and the synthetic thought of a row without thoughts is
	// thinking, not an answer
	if got := sourcetest.Kinds(turn); got != "prompt context thinking tool thinking thinking reply" {
		t.Errorf("items = %s", got)
	}
	if got := turn.Items[1].Text; got != "what is in shot.png?" {
		t.Errorf("what was sent = %q", got)
	}
	if got := turn.Items[4].Text; got != "Binary content received. Proceeding with analysis." {
		t.Errorf("the synthetic thought = %q", got)
	}
	tools := sourcetest.Tools(turn)
	if len(tools) != 1 || tools[0].CallID != "read-1" || tools[0].Status != model.StatusOK || !tools[0].Done {
		t.Errorf("tools = %+v", tools)
	}
	// The picture read_file read came back on the row after the call was answered in place:
	// it is the call's, not a picture the person sent, and the info row's copy is not read
	if tools[0].Output != "Read image file: diagram.png\n[image · png · 8 bytes]" || len(turn.Items[3].Attachments) != 1 {
		t.Errorf("the image read = %q, %+v", tools[0].Output, turn.Items[3].Attachments)
	}
	if s.Stats.Calls != 5 || s.Stats.Usage.CacheRead != 6000 {
		t.Errorf("%d calls, usage %+v", s.Stats.Calls, s.Stats.Usage)
	}
	// The summary the session list shows is the title. The error came after the turn was
	// answered: it is shown, and stopped nothing
	if s.Session.Title != "Recolour the login button" {
		t.Errorf("title = %q", s.Session.Title)
	}
	if three := s.Turn(t, 3); sourcetest.Kinds(three) != "prompt reply event" || three.Interrupted || three.Items[2].Text != "Quota exceeded." {
		t.Errorf("turn 3 = %s interrupted %v", sourcetest.Kinds(three), three.Interrupted)
	}
}

// An error Gemini CLI showed the person is an event where it happened. One that came while
// the model owed an answer — after the ask, or after the results of the calls it made — and
// after which it wrote nothing is how the turn ended: the turn is marked stopped by it, at
// its time. One after the answer, such as a command typed between turns that failed, ends
// nothing. The rows are built from packages/core/src/services/chatRecordingTypes.ts, the
// error's words from packages/core/src/utils/errorParsing.ts.
//
// A turn cut off after a tool's result is closed, as the next prompt is sent, with a model
// row Gemini CLI makes of its own — no model, no tokens, only the words that say the
// response was interrupted (closeUnansweredToolResponseTurn in
// packages/core/src/core/geminiChat.ts, written to the file as it is made since
// 0.69.0-preview.0). It is not the model going on, nor its answer, nor a model call: the
// turn still ended at the error.
func TestAnErrorThatEndedTheTurn(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "as 0.68 writes it", true: "closed as 0.69 writes it"}[closed], func(t *testing.T) {
			anErrorThatEndedTheTurn(t, closed)
		})
	}
}

func anErrorThatEndedTheTurn(t *testing.T, closed bool) {
	root := t.TempDir()
	quota := `[API Error: Quota exceeded for quota metric 'Generate Content API requests per minute' (Status: RESOURCE_EXHAUSTED)]\nPlease wait and try again later. To increase your limits, request a quota increase through AI Studio, or switch to another /auth method`
	placeholder := ``
	if closed {
		placeholder = `{"id":"p1","timestamp":"2026-11-03T09:04:59.990Z","type":"gemini","content":[{"text":"[The previous response was interrupted before it completed.]"}],"thoughts":[],"tokens":null}`
	}
	sourcetest.Write(t, filepath.Join(root, "9f2c", "chats", "session-2026-11-03T09-00-1c2d3e4f.jsonl"),
		`{"sessionId":"1c2d3e4f-5a6b","projectHash":"9f2c","startTime":"2026-11-03T09:00:00.000Z","lastUpdated":"2026-11-03T09:00:00.000Z","kind":"main"}`,
		`{"id":"u1","timestamp":"2026-11-03T09:00:01.000Z","type":"user","content":[{"text":"run the tests and fix what fails"}]}`,
		`{"id":"g1","timestamp":"2026-11-03T09:00:04.000Z","type":"gemini","content":"","thoughts":[],"model":"gemini-3-pro-preview","tokens":{"input":1500,"output":12,"cached":0,"thoughts":0,"tool":0,"total":1512},`+
			`"toolCalls":[{"id":"run-1","name":"run_shell_command","args":{"command":"npm test"},"result":[{"functionResponse":{"id":"run-1","name":"run_shell_command","response":{"output":"Output: 1 failing\nExit Code: 1"}}}],"status":"success","timestamp":"2026-11-03T09:00:09.000Z","resultDisplay":"1 failing","displayName":"Shell","description":"npm test","renderOutputAsMarkdown":false}]}`,
		`{"id":"u2","timestamp":"2026-11-03T09:00:09.100Z","type":"user","content":[{"functionResponse":{"id":"run-1","name":"run_shell_command","response":{"output":"Output: 1 failing\nExit Code: 1"}}}]}`,
		`{"id":"e1","timestamp":"2026-11-03T09:00:10.000Z","type":"error","content":"`+quota+`"}`,
		`{"$set":{"lastUpdated":"2026-11-03T09:00:10.000Z"}}`,
		placeholder,
		`{"id":"u3","timestamp":"2026-11-03T09:05:00.000Z","type":"user","content":[{"text":"try again"}]}`,
		`{"id":"e2","timestamp":"2026-11-03T09:05:01.000Z","type":"error","content":"`+quota+`"}`,
		`{"id":"u4","timestamp":"2026-11-03T10:00:00.000Z","type":"user","content":[{"text":"now?"}]}`,
		`{"id":"g2","timestamp":"2026-11-03T10:00:03.000Z","type":"gemini","content":"The test is fixed.","thoughts":[],"model":"gemini-3-pro-preview","tokens":{"input":1600,"output":6,"cached":1500,"thoughts":0,"tool":0,"total":1606}}`,
		`{"id":"e3","timestamp":"2026-11-03T10:00:20.000Z","type":"error","content":"Unknown command: /deploy"}`,
		`{"id":"w1","timestamp":"2026-11-03T10:00:21.000Z","type":"warning","content":"Some settings could not be saved."}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Stats.Turns != 3 {
		t.Fatalf("turns = %d", s.Stats.Turns)
	}
	one, two, three := s.Turn(t, 1), s.Turn(t, 2), s.Turn(t, 3)
	if got := sourcetest.Kinds(one); got != "prompt tool event" {
		t.Errorf("turn 1 = %s", got)
	}
	last := one.Items[len(one.Items)-1]
	if !one.Interrupted || last.Event != "error" || !strings.HasPrefix(last.Text, "[API Error: Quota exceeded") ||
		!one.EndedAt.Equal(time.Date(2026, 11, 3, 9, 0, 10, 0, time.UTC)) || one.Answer != "" || len(one.Calls) != 1 {
		t.Errorf("turn 1 interrupted %v, ended %v, answer %q, %d calls, last %+v: the error cut off the work",
			one.Interrupted, one.EndedAt, one.Answer, len(one.Calls), last)
	}
	if got := sourcetest.Kinds(two); got != "prompt event" || !two.Interrupted {
		t.Errorf("turn 2 = %s, interrupted %v: refused before any answer", got, two.Interrupted)
	}
	if got := sourcetest.Kinds(three); got != "prompt reply event" || three.Interrupted || three.Answer != "The test is fixed." {
		t.Errorf("turn 3 = %s, interrupted %v: the error after the answer ended nothing", got, three.Interrupted)
	}
	// The turn ended with its answer: an error shown after it does not stretch it
	if !three.EndedAt.Equal(time.Date(2026, 11, 3, 10, 0, 3, 0, time.UTC)) || !three.LastAt.Equal(time.Date(2026, 11, 3, 10, 0, 20, 0, time.UTC)) {
		t.Errorf("turn 3 ended %v, last %v: the answer is its end, the error its last row", three.EndedAt, three.LastAt)
	}
	// Without a summary the first ask is the title
	if s.Session.Title != "run the tests and fix what fails" {
		t.Errorf("title = %q", s.Session.Title)
	}
}

// The summary Gemini CLI writes of a session is what its session list shows: it is the
// session's title, the last one written over any before it, whether on a metadata line or
// in a patch. A subagent's summary is the start of what it concluded, and its title stays
// the task it was given.
func TestTheSummaryIsTheTitle(t *testing.T) {
	root := t.TempDir()
	chats := filepath.Join(root, "9f2c", "chats")
	sourcetest.Write(t, filepath.Join(chats, "session-2026-11-04T09-00-aaaa1111.jsonl"),
		`{"sessionId":"aaaa1111","projectHash":"9f2c","startTime":"2026-11-04T09:00:00.000Z","lastUpdated":"2026-11-04T09:00:00.000Z","kind":"main"}`,
		`{"id":"u1","timestamp":"2026-11-04T09:00:01.000Z","type":"user","content":[{"text":"why does the login page flash white?"}]}`,
		`{"id":"g1","timestamp":"2026-11-04T09:00:05.000Z","type":"gemini","content":"The theme loads after the first paint.","model":"gemini-3-pro-preview","tokens":{"input":900,"output":9}}`,
		`{"$set":{"summary":"Investigate login page flash","memoryScratchpad":{"version":1,"toolSequence":[]}}}`,
	)
	// --resume opens with the metadata again, the summary in it, and a later one replaces it
	sourcetest.Write(t, filepath.Join(chats, "session-2026-11-04T10-00-bbbb2222.jsonl"),
		`{"sessionId":"bbbb2222","projectHash":"9f2c","startTime":"2026-11-04T10:00:00.000Z","lastUpdated":"2026-11-04T10:00:00.000Z","summary":"Rename the config keys","kind":"main"}`,
		`{"id":"u1","timestamp":"2026-11-04T10:00:01.000Z","type":"user","content":[{"text":"and the env vars too"}]}`,
		`{"$set":{"summary":"Rename config keys and environment variables"}}`,
	)
	sourcetest.Write(t, filepath.Join(chats, "aaaa1111", "agent-1.jsonl"),
		`{"sessionId":"agent-1","projectHash":"9f2c","startTime":"2026-11-04T09:00:02.000Z","kind":"subagent"}`,
		`{"id":"u1","timestamp":"2026-11-04T09:00:02.000Z","type":"user","content":[{"text":"Find where the theme is loaded"}]}`,
		`{"id":"g1","timestamp":"2026-11-04T09:00:04.000Z","type":"gemini","content":"In app.tsx, after mount.","model":"gemini-3-flash-preview"}`,
		`{"$set":{"summary":"In app.tsx, after mount."}}`,
	)
	all := sourcetest.All(t, Spec.Open(root))
	sourcetest.NothingUnread(t, all...)
	for id, want := range map[string]string{
		"aaaa1111": "Investigate login page flash",
		"bbbb2222": "Rename config keys and environment variables",
		"agent-1":  "Find where the theme is loaded",
	} {
		if got := sourcetest.ByID(t, all, id).Session.Title; got != want {
			t.Errorf("%s: title %q, want %q", id, got, want)
		}
	}
}

// A record of a kind Gemini CLI had not written when the adapter was made — a row type, a
// patch operator, a part — is counted under its name, and the rest of the session reads as
// it would without it.
func TestUnknownKindsAreCounted(t *testing.T) {
	root := t.TempDir()
	lines := []any{
		`{"sessionId":"g-7","projectHash":"9f2c","startTime":"2026-11-02T10:00:00Z"}`,
		`{"id":"u1","timestamp":"2026-11-02T10:00:01Z","type":"user","content":[{"text":"run the tests"}]}`,
		`{"id":"g1","timestamp":"2026-11-02T10:00:05Z","type":"gemini","model":"gemini-3-pro-preview","content":[{"text":"all green"}],"tokens":{"input":10,"output":2}}`,
	}
	known := filepath.Join(root, "p", "chats", "session-2026-11-02T10-00-known.jsonl")
	sourcetest.Write(t, known, lines...)
	want := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, want)
	if err := os.Remove(known); err != nil {
		t.Fatal(err)
	}

	sourcetest.Write(t, filepath.Join(root, "p", "chats", "session-2026-11-02T10-00-new.jsonl"),
		lines[0], lines[1],
		`{"id":"c1","timestamp":"2026-11-02T10:00:02Z","type":"checkpoint","content":"saved"}`,
		`{"$truncate":{"after":"u1"}}`,
		`{"id":"x1","timestamp":"2026-11-02T10:00:03Z","content":"a message that does not say what it is"}`,
		`{"$set":{"messages":[{"id":"x2","content":"nor does this one"}]}}`,
		`{"id":"g1","timestamp":"2026-11-02T10:00:05Z","type":"gemini","model":"gemini-3-pro-preview","content":[{"text":"all green"},{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"1"}}],"tokens":{"input":10,"output":2}}`,
		// a known kind whose fields say nothing is not an unknown kind
		`{"$set":null}`,
		`{}`,
	)
	got := sourcetest.One(t, Spec.Open(root))
	wantUnread := map[string]int{"row:checkpoint": 1, "row:$truncate": 1, "row:(untyped)": 2, "part:codeExecutionResult": 1}
	if len(got.Unread) != len(wantUnread) {
		t.Errorf("unread = %v, want %v", got.Unread, wantUnread)
	}
	for kind, n := range wantUnread {
		if got.Unread[kind] != n {
			t.Errorf("unread[%s] = %d, want %d (all: %v)", kind, got.Unread[kind], n, got.Unread)
		}
	}
	if len(got.Turns) != len(want.Turns) || sourcetest.Kinds(got.Turn(t, 1)) != sourcetest.Kinds(want.Turn(t, 1)) ||
		got.Turn(t, 1).Ask != "run the tests" || got.Turn(t, 1).Answer != "all green" || got.Stats.Usage != want.Stats.Usage {
		t.Errorf("the session read differently beside what it did not know: %+v, want %+v", got.Turns, want.Turns)
	}
}

// Gemini CLI reads a row's content leniently: a lone part stands for a list of one, and a
// string among the parts is a text part. A row written so — by an earlier version, or
// converted from the older .json files — reads as the same row with its parts in a list.
func TestContentAsGeminiCLIReadsIt(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "chats", "session-2026-11-03T10-00-a.jsonl"),
		`{"sessionId":"g-10"}`,
		`{"id":"u1","timestamp":"2026-11-03T10:00:01Z","type":"user","content":{"text":"what is in a.go?"}}`,
		`{"id":"g1","timestamp":"2026-11-03T10:00:02Z","type":"gemini","content":"","toolCalls":[{"id":"r-1","name":"read_file","args":{"file_path":"a.go"}}]}`,
		`{"id":"u2","timestamp":"2026-11-03T10:00:03Z","type":"user","content":{"functionResponse":{"id":"r-1","name":"read_file","response":{"output":"package a"}}}}`,
		`{"id":"g2","timestamp":"2026-11-03T10:00:04Z","type":"gemini","content":["It declares", {"text":"package a."}]}`,
		`{"id":"u3","timestamp":"2026-11-03T10:00:05Z","type":"user","content":["and b.go?"]}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if len(s.Turns) != 2 {
		t.Fatalf("turns = %+v", s.Turns)
	}
	turn := s.Turn(t, 1)
	if turn.Ask != "what is in a.go?" || turn.Answer != "It declares\npackage a." {
		t.Errorf("ask %q answer %q", turn.Ask, turn.Answer)
	}
	tools := sourcetest.Tools(turn)
	if len(tools) != 1 || tools[0].CallID != "r-1" || !tools[0].Done || tools[0].Output != "package a" {
		t.Errorf("tools = %+v", tools)
	}
	if got := s.Turn(t, 2).Ask; got != "and b.go?" {
		t.Errorf("second ask = %q", got)
	}
}

// A gemini row Gemini CLI made from the model's history alone has its calls as
// functionCall parts and no toolCalls. Those parts are the calls, answered by the user row
// that follows; a part that a toolCalls entry has — on its own row by id or, without one,
// by name, or on an earlier row by id — is that entry, and is not a second call.
func TestCallsOnlyInTheHistoryAreRead(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "chats", "session-2026-11-04T10-00-a.jsonl"),
		`{"sessionId":"g-11"}`,
		`{"id":"u1","timestamp":"2026-11-04T10:00:01Z","type":"user","content":[{"text":"look at a.go and b.go"}]}`,
		// an ordinary row: the call part and the toolCalls entry are one call, matched by
		// id, and by name where the part or the entry has no id
		`{"id":"g1","timestamp":"2026-11-04T10:00:02Z","type":"gemini","content":[{"functionCall":{"id":"t-1","name":"read_file","args":{"file_path":"a.go"}}},{"functionCall":{"name":"glob","args":{"pattern":"*.go"}}},{"functionCall":{"id":"l-1","name":"list_directory","args":{"dir_path":"."}}}],`+
			`"toolCalls":[{"id":"t-1","name":"read_file","args":{"file_path":"a.go"},"status":"success","resultDisplay":"package a"},{"id":"t-2","name":"glob","args":{"pattern":"*.go"},"status":"success","resultDisplay":"a.go b.go"},{"name":"list_directory","args":{"dir_path":"."},"status":"success","resultDisplay":"a.go b.go"}]}`,
		// made from the history: one call already issued above, one new, one never answered
		`{"id":"h1","timestamp":"2026-11-04T10:00:03Z","type":"gemini","content":[{"text":"Now b.go."},{"functionCall":{"id":"t-1","name":"read_file","args":{"file_path":"a.go"}}},{"functionCall":{"id":"h-1","name":"read_file","args":{"file_path":"b.go"}}},{"functionCall":{"id":"h-2","name":"read_file","args":{"file_path":"c.go"}}}]}`,
		`{"id":"u2","timestamp":"2026-11-04T10:00:04Z","type":"user","content":[{"functionResponse":{"id":"h-1","name":"read_file","response":{"output":"package b"}}}]}`,
		`{"id":"g2","timestamp":"2026-11-04T10:00:05Z","type":"gemini","content":"Both are one-line packages."}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt tool tool tool reply tool tool reply" {
		t.Errorf("items = %s", got)
	}
	var calls []string
	for _, tool := range sourcetest.Tools(turn) {
		calls = append(calls, tool.Name+":"+tool.CallID)
	}
	if got := strings.Join(calls, " "); got != "read_file:t-1 glob:t-2 list_directory: read_file:h-1 read_file:h-2" {
		t.Fatalf("calls = %s, want each once", got)
	}
	tools := sourcetest.Tools(turn)
	if b := tools[3]; !b.Done || b.Output != "package b" || b.Status != model.StatusOK || !strings.Contains(string(b.Input), "b.go") || turn.Items[5].Call == 0 {
		t.Errorf("the call made in the history = %+v (call %d)", b, turn.Items[5].Call)
	}
	if c := tools[4]; c.Done || !strings.Contains(string(c.Input), "c.go") {
		t.Errorf("the call never answered = %+v", c)
	}
	if s.Stats.Tools != 5 {
		t.Errorf("tools = %d", s.Stats.Tools)
	}
}

// A row's call parts are looked up among its toolCalls entries, not compared with each of
// them: read so, a row grows the read with its size and not with its square. This one is a
// megabyte of little else, which took twenty seconds part by part and takes a fraction of
// one; the bound leaves room for a slow or busy machine and none for the square.
func TestARowOfManyCallsReadsInTimeWithItsSize(t *testing.T) {
	const parts, entries = 16000, 160000
	var row strings.Builder
	row.WriteString(`{"id":"g1","timestamp":"2026-11-04T10:00:02Z","type":"gemini","content":[`)
	for i := range parts {
		if i > 0 {
			row.WriteByte(',')
		}
		row.WriteString(`{"functionCall":{"name":"a"}}`)
	}
	row.WriteString(`],"toolCalls":[`)
	for i := range entries {
		if i > 0 {
			row.WriteByte(',')
		}
		row.WriteString(`{}`)
	}
	row.WriteString(`]}`)
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "chats", "session-2026-11-04T10-00-a.jsonl"),
		`{"sessionId":"g-12"}`,
		`{"id":"u1","timestamp":"2026-11-04T10:00:01Z","type":"user","content":[{"text":"go"}]}`,
		row.String(),
	)
	start := time.Now()
	s := sourcetest.One(t, Spec.Open(root))
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("reading took %s", took)
	}
	if s.Stats.Tools != parts+entries {
		t.Errorf("tools = %d, want %d", s.Stats.Tools, parts+entries)
	}
}

// Thinking comes as one entry per streamed piece. A piece with a bold subject — whole
// thought summaries come so, one piece each — starts a line; a piece without one goes on
// with the thought before it.
func TestThoughtsJoinAsTheyStreamed(t *testing.T) {
	got := thoughtText([]any{
		map[string]any{"subject": "Reading the config", "description": "The port is set twice."},
		map[string]any{"subject": "", "description": "The second"},
		map[string]any{"subject": "", "description": "wins."},
		map[string]any{"subject": "Answering", "description": ""},
		map[string]any{"subject": "", "description": ""},
		"not an entry",
	})
	if want := "The port is set twice. The second wins.\nAnswering"; got != want {
		t.Errorf("thinking = %q, want %q", got, want)
	}
}

// Without a .project_root, or with something in it that is not a path, the project's own
// directory name is all a session says of where it ran.
func TestCwdWithoutAProjectRoot(t *testing.T) {
	root := t.TempDir()
	for project, marker := range map[string]string{
		"bare":     "",
		"relative": "work/thing",
		"twoLines": "/a\n/b",
		"windows":  `C:\Users\u\code`,
		"posix":    "  /srv/app\n",
	} {
		sourcetest.Write(t, filepath.Join(root, project, "chats", "session-2026-11-17T04-00-"+project+".jsonl"),
			`{"sessionId":"`+project+`"}`, `{"type":"user","id":"u","content":[{"text":"hi"}]}`)
		if marker != "" {
			if err := os.WriteFile(filepath.Join(root, project, ".project_root"), []byte(marker), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := map[string]string{"bare": "bare", "relative": "relative", "twoLines": "twoLines", "windows": `C:\Users\u\code`, "posix": "/srv/app"}
	for _, s := range sourcetest.All(t, Spec.Open(root)) {
		if s.Session.Cwd != want[s.Session.ID] {
			t.Errorf("%s: cwd = %q, want %q", s.Session.ID, s.Session.Cwd, want[s.Session.ID])
		}
	}
}

// The exit code is read where the shell tool wrote one, and nowhere else: not from a
// command cancelled, sent to the background or ended by a signal, not from the words of
// what it printed, and not from another tool.
func TestShellExitCodes(t *testing.T) {
	for _, c := range []struct {
		output, display   string
		code              int
		exited, signalled bool
	}{
		{"<untrusted_context>\nOutput: x\nExit Code: 1\nProcess Group PGID: 7\n</untrusted_context>", "", 1, true, false},
		{"Output: (empty)\nExit Code: 127\nBackground PIDs: 9\nProcess Group PGID: 7", "", 127, true, false},
		{"<untrusted_context>\nOutput: ok\nProcess Group PGID: 7\n</untrusted_context>", "ok", 0, false, false},
		{"Output: Exit Code: 3", "", 0, false, false},
		{"Output: done\nSignal: 15\nProcess Group PGID: 7", "done", 0, false, true},
		{"Output: \nError: spawn sh ENOENT\nProcess Group PGID: 7", "", 0, false, false},
		{"Command was cancelled by user before it could complete. There was no output before it was cancelled.", "", 0, false, false},
		{"Command moved to background (PID: 9). Output hidden. Press Ctrl+B to view.", "", 0, false, false},
		{"Output: x\nExit Code: (none)", "", 0, false, false},
		// What the command printed ends in lines like the tool's: the tool's are the last,
		// each once — and with what the user was shown, only what follows that
		{"Output: make: ok\nExit Code: 0\nExit Code: 1\nProcess Group PGID: 7", "", 1, true, false},
		{"Output: tests\nSignal: 9\nExit Code: 0\nExit Code: 2\nProcess Group PGID: 7", "", 2, true, false},
		{"Output: done\nExit Code: 5\nProcess Group PGID: 7", "done\nExit Code: 5", 0, false, false},
		{"Output: done\nExit Code: 5\nExit Code: 1\nProcess Group PGID: 7", "done\nExit Code: 5", 1, true, false},
		{"Output: done\nExit Code: 1\nProcess Group PGID: 7", "something else", 1, true, false},
		// Before 0.68: every line, "(none)" where there was nothing to say
		{"Command: ls /missing\nDirectory: (root)\nOutput: ls: /missing: No such file\nError: (none)\nExit Code: 2\nSignal: (none)\nBackground PIDs: (none)\nProcess Group PGID: 7", "", 2, true, false},
		{"Command: true\nDirectory: (root)\nOutput: (empty)\nError: (none)\nExit Code: 0\nSignal: (none)\nBackground PIDs: (none)\nProcess Group PGID: 7", "", 0, true, false},
	} {
		code, exited, signalled := shellEnd(c.output, c.display)
		if code != c.code || exited != c.exited || signalled != c.signalled {
			t.Errorf("shellEnd(%q, %q) = %d, %v, %v; want %d, %v, %v", c.output, c.display, code, exited, signalled, c.code, c.exited, c.signalled)
		}
	}
	if killed := result("r0", shellTool, map[string]any{"output": "Output: done\nSignal: 15\nProcess Group PGID: 7"}, "", "success"); killed.Status != model.StatusInterrupted {
		t.Errorf("a command a signal ended = %+v", killed.Outcome)
	}
	if passed := result("r0", shellTool, map[string]any{"output": "Output: done\nExit Code: 5\nProcess Group PGID: 7"}, "done\nExit Code: 5", "success"); passed.Status != model.StatusOK || passed.Exit != nil {
		t.Errorf("a command that passed, whose output ends in an exit code of its own = %+v", passed.Outcome)
	}
	other := result("r1", "read_file", map[string]any{"output": "Output: x\nExit Code: 2"}, "", "success")
	if other.Exit != nil || other.Status != model.StatusOK {
		t.Errorf("another tool's output read as a command's: %+v", other.Outcome)
	}
	cancelled := result("r2", shellTool, map[string]any{"output": "Output: x\nExit Code: 130"}, "", "cancelled")
	if cancelled.Exit == nil || *cancelled.Exit != 130 || cancelled.Status != model.StatusInterrupted {
		t.Errorf("a cancelled command with an exit code = %+v", cancelled.Outcome)
	}
}

// A call written while it ran and again once it was done — on a later row of the same
// response, with its outcome — is one call, answered once.
func TestACallWrittenAgainWhenDoneIsOneCall(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "projC", "chats", "session-2026-10-02T10-00-00.jsonl"),
		`{"sessionId":"g-3","startTime":"2026-10-02T10:00:00Z","lastUpdated":"2026-10-02T10:05:00Z"}`,
		`{"type":"user","id":"u1","timestamp":"2026-10-02T10:00:01Z","content":[{"text":"build it"}]}`,
		`{"type":"gemini","id":"g1","timestamp":"2026-10-02T10:00:02Z","content":"","toolCalls":[{"id":"run-1","name":"run_shell_command","args":{"command":"make"},"status":"executing"}]}`,
		`{"type":"gemini","id":"g2","timestamp":"2026-10-02T10:00:09Z","content":"","toolCalls":[{"id":"run-1","name":"run_shell_command","args":{"command":"make"},"status":"success","resultDisplay":"built"}]}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	tools := sourcetest.Tools(s.Turn(t, 1))
	if len(tools) != 1 || tools[0].Output != "built" || tools[0].Status != model.StatusOK {
		t.Fatalf("tools = %d: %+v", len(tools), tools)
	}
}
