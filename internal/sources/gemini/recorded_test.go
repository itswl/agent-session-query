package gemini

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// recorded reads testdata/store: sessions Gemini CLI 0.68.0 wrote for these tests, trimmed
// (the thinking cut to its first pieces) and with the paths and the model renamed. A
// question asked with -p; a conversation resumed twice with --resume; a command that
// printed and one that exited 2; a file written and edited; a question about an @path; an
// interactive session rewound with /rewind and then cancelled with Esc while a command
// ran; a session that handed work to a subagent; a run whose command took 45 seconds; a
// run killed while its command ran; a read of a file that is not there.
func recorded(t *testing.T) []sourcetest.Session {
	t.Helper()
	return sourcetest.All(t, Spec.Open(filepath.Join("testdata", "store")))
}

func recordedSession(t *testing.T, id string) sourcetest.Session {
	t.Helper()
	return sourcetest.ByID(t, recorded(t), id)
}

// Every record the CLI wrote is of a kind the adapter knows — the patches --resume and
// /rewind write among them — and the sessions read as what happened: a turn for each
// question asked, the CLI's own row of context before the first.
func TestRecordedStoreReadsWhole(t *testing.T) {
	all := recorded(t)
	sourcetest.NothingUnread(t, all...)
	if len(all) != 11 {
		t.Errorf("sessions = %d, want 10 and the subagent", len(all))
	}
	turns := map[string]int{
		"db8f8bf0-65ea-496f-8ce6-7cce3871d7c6": 1, // -p
		"c53e59cc-04e9-4072-aeb2-7d83a3716e36": 3, // -p, then --resume twice
		"517cee15-68c6-49e7-b1d4-8a98eb65c081": 4, // one, two, /rewind, three, a cancelled command
	}
	for id, want := range turns {
		if s := sourcetest.ByID(t, all, id); s.Stats.Turns != want {
			t.Errorf("%s: turns = %d, want %d", id, s.Stats.Turns, want)
		}
	}
	// A -p run writes two user rows: the context the CLI opens every session with, then
	// what was asked. The first is no ask and no title
	qa := sourcetest.ByID(t, all, "db8f8bf0-65ea-496f-8ce6-7cce3871d7c6")
	if qa.Session.Title != "What is the capital of France? Answer in one short sentence." ||
		qa.Turn(t, 1).Ask != qa.Session.Title || qa.Turn(t, 1).Answer != "The capital of France is Paris." {
		t.Errorf("title %q, turn 1 = %+v", qa.Session.Title, qa.Turn(t, 1))
	}
	if ctx := qa.Turn(t, 0); len(ctx.Items) != 1 || ctx.Items[0].Kind != model.Context ||
		!strings.HasPrefix(ctx.Items[0].Text, "<session_context>") {
		t.Errorf("the session's context = %+v", ctx.Items)
	}
}

// --resume appends to the file it continues, and writes each model row of the history
// again as a $patch of parts — its thinking and its words a second time. The rows are read
// as first written: each answer once, each turn's thinking once.
func TestRecordedPatchesAreNotSaidAgain(t *testing.T) {
	s := recordedSession(t, "c53e59cc-04e9-4072-aeb2-7d83a3716e36")
	sourcetest.NothingUnread(t, s)
	for i, answer := range []string{"OK", "42", "43"} {
		turn := s.Turn(t, i+1)
		if turn.Answer != answer || sourcetest.Kinds(turn) != "prompt thinking reply" {
			t.Errorf("turn %d: answer %q, items %s", i+1, turn.Answer, sourcetest.Kinds(turn))
		}
	}
	// The CLI's own totals for the three runs: 8043+8063+8081 prompt, 7168+8000 of it
	// cached, 3+3+3 answered and 31+21+35 thought
	if want := (model.Usage{Input: 8043 + 895 + 81, CacheRead: 15168, Output: 9 + 87, Reasoning: 87}); s.Stats.Usage != want {
		t.Errorf("usage = %+v, want %+v", s.Stats.Usage, want)
	}
}

// The project directory is named after the directory a session ran in, and .project_root
// beside chats/ holds that directory's path: it is the session's working directory.
func TestRecordedCwdIsTheProjectsDirectory(t *testing.T) {
	for _, s := range recorded(t) {
		project := filepath.Base(projectDir(s.Session.Path))
		if want := "/home/user/work/" + project; s.Session.Cwd != want {
			t.Errorf("%s: cwd = %q, want %q", s.Session.ID, s.Session.Cwd, want)
		}
	}
}

// The model behind the recorded sessions streamed its thinking a token at a time, and the
// CLI wrote an entry for each, trimmed. They are one text.
func TestRecordedThinkingStreamedByTheToken(t *testing.T) {
	turn := recordedSession(t, "db8f8bf0-65ea-496f-8ce6-7cce3871d7c6").Turn(t, 1)
	if got, want := turn.Items[1].Text, `The user asks a simple factual question : " What is the`; got != want {
		t.Errorf("thinking = %q, want %q", got, want)
	}
}

// The CLI marks a command that ran a success whatever it exited with; the exit code is in
// what it told the model. ls of a missing path exited 2, and failed.
func TestRecordedCommandExitCodes(t *testing.T) {
	s := recordedSession(t, "66549caa-e84e-49c9-9a3c-4e8f5ad6f0ac")
	tools := sourcetest.Tools(s.Turn(t, 1))
	if len(tools) != 2 {
		t.Fatalf("tools = %d", len(tools))
	}
	echo, ls := tools[0], tools[1]
	if echo.Command != "echo hello-asq" || echo.Status != model.StatusOK || echo.Exit != nil || echo.Output != "hello-asq" {
		t.Errorf("echo = %+v", echo)
	}
	if ls.Command != "ls /nonexistent-asq-path" || ls.Status != model.StatusError || ls.Exit == nil || *ls.Exit != 2 ||
		ls.Output != "ls: cannot access '/nonexistent-asq-path': No such file or directory" {
		t.Errorf("ls = %+v", ls)
	}
	if s.Stats.Failures != 1 {
		t.Errorf("failures = %d", s.Stats.Failures)
	}
}

// Esc while a command ran: the CLI writes the call with its cancellation on the row that
// asked for it, a notice that the request was cancelled, and the call once more on a row
// of its own with neither tokens nor words. It is one call, one failure, and one model call
// fewer than the rows.
func TestRecordedCancelledCallIsOneCall(t *testing.T) {
	s := recordedSession(t, "517cee15-68c6-49e7-b1d4-8a98eb65c081")
	turn := s.Turn(t, 4)
	tools := sourcetest.Tools(turn)
	if len(tools) != 1 || tools[0].Status != model.StatusInterrupted || tools[0].Command != "sleep 40" ||
		tools[0].Output != "[Operation Cancelled] Reason: User cancelled operation" {
		t.Fatalf("tools = %+v", tools)
	}
	// The CLI's summary on exit: 4 requests, 1 tool call, 0 succeeded
	if s.Stats.Calls != 4 || s.Stats.Tools != 1 || s.Stats.Failures != 1 {
		t.Errorf("calls %d, tools %d, failures %d", s.Stats.Calls, s.Stats.Tools, s.Stats.Failures)
	}
}

// The notice the CLI writes when the person stops a turn is how the turn ended.
func TestRecordedCancelNoticeEndsTheTurn(t *testing.T) {
	turn := recordedSession(t, "517cee15-68c6-49e7-b1d4-8a98eb65c081").Turn(t, 4)
	if !turn.Interrupted {
		t.Error("the turn the person cancelled is not marked interrupted")
	}
	var events []string
	for _, it := range turn.Items {
		if it.Kind == model.Event {
			events = append(events, it.Event+": "+it.Text)
		}
	}
	if len(events) != 1 || events[0] != model.EventInterrupted+": Request cancelled." {
		t.Errorf("events = %v", events)
	}
}

// An @path in a question: the CLI sends the model the file's contents after the words, and
// keeps what was typed beside them. The ask is what was typed; the contents are context.
func TestRecordedAtPathAsk(t *testing.T) {
	s := recordedSession(t, "c402b5b5-5b7c-43e2-8345-ba3d9beceb14")
	turn := s.Turn(t, 1)
	if turn.Ask != "What word is in @notes.txt? Answer with the word only." || s.Session.Title != turn.Ask {
		t.Errorf("ask %q, title %q", turn.Ask, s.Session.Title)
	}
	if got := sourcetest.Kinds(turn); got != "prompt context thinking reply" {
		t.Fatalf("items = %s", got)
	}
	if got, want := turn.Items[1].Text, "--- Content from referenced files ---\nContent from @notes.txt:\nbeta\n--- End of content ---"; got != want {
		t.Errorf("context = %q, want %q", got, want)
	}
}

// A subagent writes a transcript of its own under its parent's id. It is a session of its
// own, tied to its parent, and what it spent is what the CLI counted for the subagent.
func TestRecordedSubagent(t *testing.T) {
	all := recorded(t)
	parent := sourcetest.ByID(t, all, "c853c54f-33c3-4c12-bfa1-c816ca7252d6")
	sub := sourcetest.ByID(t, all, "8295a9e5-e652-4112-bc3a-43b1ee994cdc")
	if parent.Session.Parent != nil {
		t.Errorf("the parent has a parent: %+v", parent.Session.Parent)
	}
	if sub.Session.Parent == nil || sub.Session.Parent.Session != parent.Session.ID {
		t.Fatalf("the subagent's parent = %+v", sub.Session.Parent)
	}
	if !strings.HasPrefix(sub.Session.Title, "Run the shell command `echo sub-asq`") || sub.Session.Cwd != "/home/user/work/sub" {
		t.Errorf("subagent title %q, cwd %q", sub.Session.Title, sub.Session.Cwd)
	}
	if tools := sourcetest.Tools(sub.Turn(t, 1)); len(tools) != 2 || tools[0].Command != "echo sub-asq" || tools[0].Output != "sub-asq" {
		t.Errorf("the subagent's tools = %+v", tools)
	}
	// The CLI's own count, by role: main 20854 prompt, 78 answered and 75 thought in 2
	// requests; subagent 20464 prompt of which 10176 cached, 80 and 74 in 2
	if want := (model.Usage{Input: 20854, Output: 78 + 75, Reasoning: 75}); parent.Stats.Usage != want || parent.Stats.Calls != 2 {
		t.Errorf("parent: %d calls, usage %+v, want 2 and %+v", parent.Stats.Calls, parent.Stats.Usage, want)
	}
	if want := (model.Usage{Input: 20464 - 10176, CacheRead: 10176, Output: 80 + 74, Reasoning: 74}); sub.Stats.Usage != want || sub.Stats.Calls != 2 {
		t.Errorf("subagent: %d calls, usage %+v, want 2 and %+v", sub.Stats.Calls, sub.Stats.Usage, want)
	}
}

// A call answered in place says when it finished. A command that slept 45 seconds ends 45
// seconds after the row that asked for it, not on it.
func TestRecordedToolEndsWhenItFinished(t *testing.T) {
	turn := recordedSession(t, "abd681d3-2466-43b3-815a-a45289d443a1").Turn(t, 1)
	tools := sourcetest.Tools(turn)
	if len(tools) != 1 || tools[0].Command != "sleep 45 && echo slept" {
		t.Fatalf("tools = %+v", tools)
	}
	if want := time.Date(2026, 11, 17, 4, 57, 31, 464e6, time.UTC); !tools[0].EndAt.Equal(want) {
		t.Errorf("the call ended at %v, want %v", tools[0].EndAt, want)
	}
}

// A run killed while its command ran leaves the row that asked for the command and nothing
// after: the CLI writes a call once it finishes. What is there reads.
func TestRecordedKilledRun(t *testing.T) {
	s := recordedSession(t, "3b03f458-4980-4d8b-abc3-8f57d3e4ce1a")
	turn := s.Turn(t, 1)
	if turn.Ask != "Run the shell command: sleep 40. Then tell me it finished." || turn.Answer != "" ||
		sourcetest.Kinds(turn) != "prompt thinking" || s.Stats.Calls != 1 {
		t.Errorf("turn = %+v", turn)
	}
}

// Where the latest turn of each recorded session stands. Gemini CLI marks no turn's end:
// the last row says it. A command it is running is not written until it has finished,
// so the run killed while its command ran was, by its file, at work.
func TestRecordedWhereTheLatestTurnStands(t *testing.T) {
	at := func(clock string) time.Time {
		t, _ := time.Parse(time.RFC3339Nano, "2026-11-17T"+clock+"Z")
		return t
	}
	for id, want := range map[string]model.Live{
		"66549caa-e84e-49c9-9a3c-4e8f5ad6f0ac": {State: model.LiveIdle, Since: at("04:53:48.404")},    // two commands, then the answer
		"3b03f458-4980-4d8b-abc3-8f57d3e4ce1a": {State: model.LiveWorking, Since: at("04:59:11.399")}, // killed while its command ran
		"517cee15-68c6-49e7-b1d4-8a98eb65c081": {State: model.LiveIdle, Since: at("05:05:42.05")},     // a command cancelled with Esc
	} {
		s := recordedSession(t, id)
		if got := s.Session.Live; got == nil || *got != want {
			t.Errorf("%s: %+v, want %+v", id, got, want)
		}
	}
}

// In the shape that writes a call while it runs, a call with no outcome yet is waited on.
func TestACallRunningIsWaitedOn(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "projC", "chats", "session-2026-10-02T10-00-00.jsonl"),
		`{"sessionId":"g-3","startTime":"2026-10-02T10:00:00Z","lastUpdated":"2026-10-02T10:05:00Z"}`,
		`{"type":"user","id":"u1","timestamp":"2026-10-02T10:00:01Z","content":[{"text":"build it"}]}`,
		`{"type":"gemini","id":"g1","timestamp":"2026-10-02T10:00:02Z","content":"","toolCalls":[{"id":"run-1","name":"run_shell_command","args":{"command":"make"},"status":"awaiting_approval"}]}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	want := model.Live{State: model.LiveWaiting, Since: time.Date(2026, 10, 2, 10, 0, 2, 0, time.UTC), Tool: "run_shell_command", Command: "make", Seq: 1}
	if got := s.Session.Live; got == nil || *got != want {
		t.Errorf("%+v, want %+v", got, want)
	}
}
