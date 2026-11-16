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
	turn := sourcetest.One(t, Spec.Open(root)).Turn(t, 1)
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
	if s.Stats.Turns != 1 || s.Turn(t, 1).Ask != "a plain string content" || s.Turn(t, 1).Answer != "ok" {
		t.Errorf("turns = %+v", s.Turns)
	}
	if got := sourcetest.Kinds(s.Turn(t, 1)); got != "prompt reply" {
		t.Errorf("items = %s: info and error rows are the CLI's own", got)
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
	if Spec.Name != "gemini" || Spec.Title != "Gemini CLI" || !Spec.Movable || Spec.Resume != "" {
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
