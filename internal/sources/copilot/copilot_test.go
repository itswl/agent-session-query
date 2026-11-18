package copilot

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// The store in testdata/store was written to the session-events schema Copilot CLI 1.0.114
// ships (schemas/session-events.schema.json in the CLI's bundle, the source of the SDK's
// generated session-events.d.ts), and every event in it validates against that schema; its
// workspace.yaml files are what the yaml package's stringify — the one the CLI's
// JavaScript writes workspace.yaml with — makes of their fields. It is not a recorded
// session: no Copilot CLI was run to write it.
//
//	…0001  a session: a failing command, an edit, a file the person would not have
//	       created, words sent while the agent worked and then Ctrl+C, a subagent, a
//	       compaction, a model change, a notification, a failed hook, an error, a
//	       command the person ran themselves, and a last line cut off mid-write
//	…0002  /fork of …0001 after its first turn
//	…0003  a log the CLI rotated: the part moved aside, and the log started again
//	…0004  the detached run …0001 started on its way out
const (
	whole    = "c0ffee00-0000-4000-8000-000000000001"
	forked   = "c0ffee00-0000-4000-8000-000000000002"
	rotated  = "c0ffee00-0000-4000-8000-000000000003"
	detached = "c0ffee00-0000-4000-8000-000000000004"
)

func store(t *testing.T) []sourcetest.Session {
	t.Helper()
	all := sourcetest.All(t, Spec.Open(filepath.Join("testdata", "store")))
	sourcetest.NothingUnread(t, all...)
	if len(all) != 4 {
		t.Fatalf("sessions = %d, want 4", len(all))
	}
	return all
}

func TestSession(t *testing.T) {
	s := sourcetest.ByID(t, store(t), whole)
	if s.Session.Title != "Fix the flaky order test" || s.Session.Cwd != "/work/alpha" || s.Session.Branch != "main" ||
		s.Session.Version != "1.0.114" || s.Session.Parent != nil {
		t.Errorf("session = %+v", s.Session)
	}
	// The model that answered last; the change to another came after it, and a subagent's
	// model is not the session's
	if s.Session.Model != "claude-sonnet-4.6" {
		t.Errorf("model = %q", s.Session.Model)
	}
	if got := s.Session.StartedAt.Format("15:04:05"); got != "09:00:00" {
		t.Errorf("started %s", got)
	}
	// The last line, cut off mid-write, is not read; the one before it is the last word
	if got := s.Session.UpdatedAt.Format("15:04:05"); got != "09:06:00" {
		t.Errorf("updated %s", got)
	}
	if s.Stats.Turns != 4 || s.Stats.Calls != 10 || s.Stats.Tools != 7 || s.Stats.Failures != 4 {
		t.Errorf("stats = %+v", s.Stats)
	}

	turn := s.Turn(t, 1)
	// The reasoning came as an event of its own and again on the response: it is one item
	if got := sourcetest.Kinds(turn); got != "prompt thinking tool tool reply" {
		t.Errorf("items = %s", got)
	}
	if turn.Ask != "fix the flaky order test" || turn.Answer != "The test depended on map order; it sorts the keys now." {
		t.Errorf("ask %q, answer %q", turn.Ask, turn.Answer)
	}
	tools := sourcetest.Tools(turn)
	test, edit := tools[0], tools[1]
	// A command that ran succeeded as a tool call; its exit code is how it went
	if test.Name != "bash" || test.Kind != model.ToolExec || test.Command != "go test ./..." ||
		test.Status != model.StatusError || test.Exit == nil || *test.Exit != 1 || test.DurMs != 2000 ||
		!strings.Contains(test.Output, "--- FAIL: TestOrder") {
		t.Errorf("command = %+v", test)
	}
	// What the CLI shows of an edit is its diff, which is what is read
	if edit.Name != "edit" || edit.Status != model.StatusOK || !strings.HasPrefix(edit.Output, "diff --git") {
		t.Errorf("edit = %+v", edit)
	}
	if len(turn.Files) != 1 || turn.Files[0] != (model.File{Path: "/work/alpha/order.go", Changed: true}) {
		t.Errorf("files = %+v", turn.Files)
	}
	if turn.Failures != 1 || turn.Interrupted {
		t.Errorf("failures %d, interrupted %v", turn.Failures, turn.Interrupted)
	}

	// One call per response, its parts joined by the provider's id for it. Copilot counts
	// the cache inside the input: 12000 sent, of which 9000 read from the cache and 2000
	// written to it. Its cost is in AI credits: 3.15 of them, at $0.01 each
	if len(turn.Calls) != 3 {
		t.Fatalf("calls = %+v", turn.Calls)
	}
	first := turn.Calls[0]
	want := model.Usage{Input: 1000, Output: 180, CacheRead: 9000, CacheWrite: 2000, Reasoning: 40}
	if first.ID != "chatcmpl-1" || first.Model != "claude-sonnet-4.6" || first.Usage == nil || *first.Usage != want ||
		first.Stop != "tool_calls" || !near(first.CostUSD, 0.0315) {
		t.Errorf("first call = %+v %+v", first, first.Usage)
	}
	if turn.Items[1].Call != 1 || turn.Items[4].Call != 3 {
		t.Errorf("the thinking and the reply are not their calls': %+v", turn.Items)
	}
}

func near(a, b float64) bool { return a-b < 1e-12 && b-a < 1e-12 }

// A call the person would not have run did not fail: it was stopped, and changed nothing.
func TestDeclinedCall(t *testing.T) {
	turn := sourcetest.ByID(t, store(t), whole).Turn(t, 2)
	tools := sourcetest.Tools(turn)
	if len(tools) != 1 || tools[0].Name != "create" || tools[0].Status != model.StatusInterrupted ||
		!strings.HasPrefix(tools[0].Output, "The user rejected this tool call") {
		t.Fatalf("tools = %+v", tools)
	}
	if len(turn.Files) != 1 || turn.Files[0].Changed {
		t.Errorf("files = %+v", turn.Files)
	}
	if turn.Answer != "Understood: no changelog entry." {
		t.Errorf("answer = %q", turn.Answer)
	}
}

// Words sent while the agent works go into the turn under way, and Ctrl+C stops the turn
// and the command it was running, which never reports back.
func TestSteeringAndAbort(t *testing.T) {
	turn := sourcetest.ByID(t, store(t), whole).Turn(t, 3)
	if got := sourcetest.Kinds(turn); got != "prompt tool prompt event" {
		t.Errorf("items = %s", got)
	}
	if !turn.Items[2].Queued || turn.Items[2].Text != "with -race too" {
		t.Errorf("steering = %+v", turn.Items[2])
	}
	tools := sourcetest.Tools(turn)
	if len(tools) != 1 || !tools[0].Done || tools[0].Status != model.StatusInterrupted || tools[0].EndAt.Format("15:04:05") != "09:02:30" {
		t.Errorf("command = %+v", tools)
	}
	if !turn.Interrupted || turn.Items[3].Event != model.EventInterrupted || turn.Items[3].Text != "user_initiated" {
		t.Errorf("turn = %+v", turn)
	}
}

// A subagent writes into the session's log. Its calls and what they cost are the turn's;
// its words are said to the agent that started it, and the turn's answer stays that agent's.
func TestSubagent(t *testing.T) {
	turn := sourcetest.ByID(t, store(t), whole).Turn(t, 4)
	if !strings.HasPrefix(sourcetest.Kinds(turn), "prompt tool tool thinking reply") {
		t.Errorf("items = %s", sourcetest.Kinds(turn))
	}
	tools := sourcetest.Tools(turn)
	task, grep := tools[0], tools[1]
	if task.Name != "task" || task.Kind != model.ToolAgent || task.Output != "Found one in report.go, line 12." {
		t.Errorf("task = %+v", task)
	}
	if grep.Name != "grep" || grep.Status != model.StatusOK || turn.Items[3].Text != "Found one in report.go, line 12." {
		t.Errorf("the subagent's work = %+v, %+v", grep, turn.Items[3])
	}
	if turn.Answer != "There is one more, in report.go." {
		t.Errorf("answer = %q", turn.Answer)
	}
	models := []string{}
	for _, c := range turn.Calls {
		models = append(models, c.Model)
	}
	if strings.Join(models, " ") != "claude-sonnet-4.6 claude-haiku-4.5 claude-haiku-4.5 claude-sonnet-4.6" {
		t.Errorf("calls = %v", models)
	}
}

// What happened around the conversation: the events among the items, and a command the
// person ran themselves, which exited 1.
func TestAroundTheConversation(t *testing.T) {
	turn := sourcetest.ByID(t, store(t), whole).Turn(t, 4)
	var events []string
	var context string
	for _, it := range turn.Items {
		switch it.Kind {
		case model.Event:
			events = append(events, it.Event+": "+it.Text)
		case model.Context:
			context = it.Text
		}
	}
	want := []string{
		"compaction: Fixed TestOrder by sorting keys; report.go has the same bug.",
		"model_change: gpt-5.6",
		"hook_error: lint hook exited 2",
		"error: You have hit the rate limit.",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Errorf("events = %q", events)
	}
	if !strings.HasPrefix(context, "<system_notification>Shell 3 completed") {
		t.Errorf("context = %q", context)
	}
	tools := sourcetest.Tools(turn)
	own := tools[len(tools)-1]
	if own.Name != "local_shell" || own.Command != "ls /missing" || own.Exit == nil || *own.Exit != 1 || own.Status != model.StatusError {
		t.Errorf("the person's command = %+v", own)
	}
}

// /fork copies the session's events as they were into a new log, behind a session.start
// rewritten to the moment of the fork: the copies are the parent's, and only what came
// after is the fork's own.
func TestFork(t *testing.T) {
	s := sourcetest.ByID(t, store(t), forked)
	if s.Stats.Turns != 1 || s.Stats.Calls != 1 || s.Stats.Tools != 0 || s.Stats.Failures != 0 {
		t.Errorf("stats = %+v", s.Stats)
	}
	if want := (model.Usage{Input: 1000, Output: 25, CacheRead: 13000}); s.Stats.Usage != want || !near(s.Stats.CostUSD, 0.005) {
		t.Errorf("usage = %+v, cost %v", s.Stats.Usage, s.Stats.CostUSD)
	}
	copied := s.Turn(t, 0)
	for _, it := range copied.Items {
		if !it.Inherited {
			t.Errorf("a copy that is not inherited: %+v", it)
		}
	}
	if len(copied.Items) != 5 || s.Turn(t, 1).Ask != "try a stable sort instead" {
		t.Errorf("turns = %+v", s.Turns)
	}
	// A name too long for a line is folded onto the next by the yaml package
	if s.Session.Title != "Fix the flaky order test: try a stable sort instead of sorting the keys, and kee…" {
		t.Errorf("title = %q", s.Session.Title)
	}
	if got := s.Session.StartedAt.Format("15:04:05"); got != "10:00:00" {
		t.Errorf("started %s", got)
	}
}

// A rotated log is read from its first part on, and the copies the log was started again
// with are not read twice.
func TestRotatedLog(t *testing.T) {
	s := sourcetest.ByID(t, store(t), rotated)
	if s.Stats.Turns != 2 || s.Turn(t, 1).Ask != "rotate the deploy key" || s.Turn(t, 2).Ask != "and the staging key" {
		t.Errorf("turns = %+v", s.Turns)
	}
	// It has no workspace.yaml: the title is the first thing asked
	if s.Session.Title != "rotate the deploy key" || s.Session.Cwd != "/work/beta" || s.Stats.Calls != 2 {
		t.Errorf("session = %+v, %+v", s.Session, s.Stats)
	}
	if got := s.Session.StartedAt.Format("15:04:05"); got != "11:00:00" {
		t.Errorf("started %s", got)
	}
}

// When the part a log links to is gone, the copies at its head are all there is of what
// came before, and they are read.
func TestRotatedLogWhoseEarlierPartIsGone(t *testing.T) {
	root := t.TempDir()
	head, err := os.ReadFile(filepath.Join("testdata", "store", rotated, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sourcetest.Write(t, filepath.Join(root, rotated, "events.jsonl"), strings.TrimSuffix(string(head), "\n"))
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Stats.Turns != 2 || s.Turn(t, 1).Ask != "rotate the deploy key" || s.Turn(t, 1).Answer != "" ||
		s.Turn(t, 2).Answer != "Staging rotated too." || s.Session.Cwd != "/work/beta" {
		t.Errorf("turns = %+v", s.Turns)
	}
}

// A link out of the session's directory, or one that loops, is not followed.
func TestLinksThatAreNotFollowed(t *testing.T) {
	for _, prev := range []string{"../elsewhere/events.jsonl", "/etc/events.jsonl", `..\events.jsonl`, "events.jsonl", ".."} {
		root := t.TempDir()
		sourcetest.Write(t, filepath.Join(root, "elsewhere", "events.jsonl"),
			ev("e1", "session.start", "2026-11-17T08:00:00.000Z", M{"sessionId": "elsewhere"}),
			ev("e2", "user.message", "2026-11-17T08:00:01.000Z", M{"content": "not this session's"}))
		sourcetest.Write(t, filepath.Join(root, "s1", "events.jsonl"),
			ev("e3", "session.journal_link", "2026-11-17T09:00:00.000Z", M{"version": 1, "sessionId": "s1", "previousFile": prev}),
			ev("e4", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "this session's"}))
		all := sourcetest.All(t, Spec.Open(root))
		s := sourcetest.ByID(t, all, "s1")
		sourcetest.NothingUnread(t, s)
		if s.Stats.Turns != 1 || s.Turn(t, 1).Ask != "this session's" {
			t.Errorf("%s: turns = %+v", prev, s.Turns)
		}
	}
}

// The run a session starts detached to keep what it learned hangs beneath it.
func TestDetachedRun(t *testing.T) {
	s := sourcetest.ByID(t, store(t), detached)
	if s.Session.Parent == nil || s.Session.Parent.Session != whole || s.Session.Parent.Depth != 1 {
		t.Errorf("parent = %+v", s.Session.Parent)
	}
	// Its only message is the CLI's, and it has no name but the summary
	if s.Session.Title != "Consolidate session memory" || s.Stats.Turns != 0 || s.Stats.Tools != 1 {
		t.Errorf("session = %+v, %+v", s.Session, s.Stats)
	}
}

type M = map[string]any

// ev is an event as the log writes it.
func ev(id, typ, at string, data M) M {
	return M{"id": id, "timestamp": at, "parentId": nil, "type": typ, "data": data}
}

// sub is a subagent's event.
func sub(agent string, e M) M {
	e["agentId"] = agent
	return e
}

func TestUserMessageSources(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "s1", "events.jsonl"),
		ev("e1", "session.start", "2026-11-17T09:00:00.000Z", M{"sessionId": "s1", "context": M{"cwd": "/w"}}),
		ev("e2", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "typed", "source": "user"}),
		// a skill's hidden prompt, another agent's message, autopilot carrying on
		ev("e3", "user.message", "2026-11-17T09:00:02.000Z", M{"content": "the skill's instructions", "source": "skill-pdf"}),
		ev("e4", "user.message", "2026-11-17T09:00:03.000Z", M{"content": "from a peer", "source": "agent-3"}),
		ev("e5", "user.message", "2026-11-17T09:00:04.000Z", M{"content": "Continue the task.", "source": "autopilot-continuation", "isAutopilotContinuation": true}),
		// what a command the person typed sends, and a prompt they scheduled
		ev("e6", "user.message", "2026-11-17T09:00:05.000Z", M{"content": "Review the diff", "source": "command-review"}),
		ev("e7", "user.message", "2026-11-17T09:00:06.000Z", M{"content": "check the build", "source": "schedule-1"}),
		// sent while busy, to wait its turn: it is a run of its own when it is written
		ev("e8", "user.message", "2026-11-17T09:00:07.000Z", M{"content": "then deploy", "delivery": "queued"}),
		// no source at all, as an older CLI writes it
		ev("e9", "user.message", "2026-11-17T09:00:08.000Z", M{"content": "plain"}),
		// /autopilot <objective>: the person's words, sent by the command; then the run it
		// sets off, which autopilot carries on by itself
		ev("e10", "user.message", "2026-11-17T09:00:09.000Z", M{"content": "make every test pass", "source": "autopilot-objective"}),
		ev("e11", "assistant.message", "2026-11-17T09:00:10.000Z", M{"messageId": "m1", "content": "Two fail; fixing the first."}),
		ev("e12", "user.message", "2026-11-17T09:00:11.000Z", M{"content": "", "source": "autopilot", "isAutopilotContinuation": true}),
		ev("e13", "user.message", "2026-11-17T09:00:12.000Z", M{"content": "Keep going.", "source": "autopilot", "isAutopilotContinuation": true}),
		ev("e14", "assistant.message", "2026-11-17T09:00:13.000Z", M{"messageId": "m2", "content": "Both pass."}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	var asks []string
	for _, turn := range s.Turns {
		if turn.Index > 0 {
			asks = append(asks, turn.Ask)
		}
	}
	if got := strings.Join(asks, " | "); got != "typed | Review the diff | check the build | then deploy | plain | make every test pass" {
		t.Errorf("asks = %s", got)
	}
	if got := sourcetest.Kinds(s.Turn(t, 1)); got != "prompt context context context" {
		t.Errorf("turn 1 = %s", got)
	}
	if run := s.Turn(t, 6); sourcetest.Kinds(run) != "prompt reply context reply" || run.Answer != "Both pass." {
		t.Errorf("the objective's run = %s, answer %q", sourcetest.Kinds(run), run.Answer)
	}
}

// Before every event carried agentId, a subagent's events said whose they were in their
// data — agentId there, or the call that started the agent — and the CLI tells them apart
// by either still.
func TestSubagentMarkedInItsData(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "s1", "events.jsonl"),
		ev("e1", "session.start", "2026-11-17T09:00:00.000Z", M{"sessionId": "s1"}),
		ev("e2", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "go"}),
		ev("e3", "user.message", "2026-11-17T09:00:02.000Z", M{"content": "the subagent's task", "parentToolCallId": "t1"}),
		ev("e4", "assistant.message", "2026-11-17T09:00:03.000Z", M{"messageId": "m1", "content": "one", "parentToolCallId": "t1", "model": "small"}),
		sub("agent-2", ev("e5", "assistant.message", "2026-11-17T09:00:04.000Z", M{"messageId": "m2", "content": "two", "model": "small"})),
		ev("e6", "assistant.message", "2026-11-17T09:00:05.000Z", M{"messageId": "m3", "content": "three", "agentId": "agent-2", "model": "small"}),
		ev("e7", "assistant.message", "2026-11-17T09:00:06.000Z", M{"messageId": "m4", "content": "done", "model": "big"}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt thinking thinking thinking reply" || turn.Answer != "done" {
		t.Errorf("items = %s, answer %q", got, turn.Answer)
	}
	if s.Session.Model != "big" {
		t.Errorf("model = %q", s.Session.Model)
	}
}

// str_replace_editor reads as well as writes, by its command: a view changes nothing.
func TestStrReplaceEditor(t *testing.T) {
	root := t.TempDir()
	call := func(id, command string) M {
		return M{"toolCallId": id, "name": "str_replace_editor", "arguments": M{"command": command, "path": "/w/" + id + ".go"}}
	}
	sourcetest.Write(t, filepath.Join(root, "s1", "events.jsonl"),
		ev("e1", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "go"}),
		ev("e2", "assistant.message", "2026-11-17T09:00:02.000Z", M{"messageId": "m1", "content": "", "toolRequests": []M{call("a", "view"), call("b", "str_replace")}}),
		ev("e3", "tool.execution_complete", "2026-11-17T09:00:03.000Z", M{"toolCallId": "a", "success": true, "result": M{"content": "1 package w"}}),
		ev("e4", "tool.execution_complete", "2026-11-17T09:00:04.000Z", M{"toolCallId": "b", "success": true, "result": M{"content": "File /w/b.go updated."}}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	tools := sourcetest.Tools(turn)
	if tools[0].Name != "view" || tools[0].Kind != model.ToolRead || tools[1].Name != "str_replace_editor" || tools[1].Kind != model.ToolWrite {
		t.Errorf("tools = %+v, %+v", tools[0], tools[1])
	}
	if len(turn.Files) != 2 || turn.Files[0].Changed || !turn.Files[1].Changed {
		t.Errorf("files = %+v", turn.Files)
	}
}

// apply_patch is a freeform tool: its arguments are the patch itself, a string, and not an
// object around it. The files it names are what it changed.
func TestFreeformArguments(t *testing.T) {
	root := t.TempDir()
	patch := func(file string) string {
		return "*** Begin Patch\n*** Update File: /w/" + file + "\n@@\n-\tsort.Strings(keys)\n+\tslices.Sort(keys)\n*** End Patch\n"
	}
	sourcetest.Write(t, filepath.Join(root, "s1", "events.jsonl"),
		ev("e1", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "go"}),
		ev("e2", "assistant.message", "2026-11-17T09:00:02.000Z", M{"messageId": "m1", "content": "", "toolRequests": []M{
			{"toolCallId": "a", "name": "apply_patch", "type": "custom", "arguments": patch("order.go")},
		}}),
		ev("e3", "tool.execution_start", "2026-11-17T09:00:02.100Z", M{"toolCallId": "a", "toolName": "apply_patch", "arguments": patch("order.go")}),
		ev("e4", "tool.execution_complete", "2026-11-17T09:00:02.200Z", M{"toolCallId": "a", "success": true, "result": M{"content": "Done"}}),
		// a call no response asked for, which its start is the first to name
		ev("e5", "tool.execution_start", "2026-11-17T09:00:03.000Z", M{"toolCallId": "b", "toolName": "apply_patch", "arguments": patch("report.go")}),
		ev("e6", "tool.execution_complete", "2026-11-17T09:00:03.100Z", M{"toolCallId": "b", "success": true, "result": M{"content": "Done"}}),
		// the object the CLI takes them in too
		ev("e7", "assistant.message", "2026-11-17T09:00:04.000Z", M{"messageId": "m2", "content": "", "toolRequests": []M{
			{"toolCallId": "c", "name": "apply_patch", "arguments": M{"input": patch("main.go")}},
		}}),
		ev("e8", "tool.execution_complete", "2026-11-17T09:00:04.100Z", M{"toolCallId": "c", "success": true, "result": M{"content": "Done"}}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	tools := sourcetest.Tools(turn)
	if len(tools) != 3 {
		t.Fatalf("tools = %+v", tools)
	}
	for i, file := range []string{"order.go", "report.go", "main.go"} {
		if got := tools[i]; got.Kind != model.ToolWrite || len(got.Files) != 1 || got.Files[0] != "/w/"+file ||
			string(got.Input) != `{"input":`+strconv.Quote(patch(file))+`}` {
			t.Errorf("%s: %+v, input %s", file, got, got.Input)
		}
	}
	if len(turn.Files) != 3 || !turn.Files[0].Changed || !turn.Files[1].Changed || !turn.Files[2].Changed {
		t.Errorf("files = %+v", turn.Files)
	}
}

// A command's exit code is on its result's shellExecution, and before that only at the end
// of what the shell tool printed. Only a shell tool's output is read for it.
func TestExitCodes(t *testing.T) {
	root := t.TempDir()
	request := func(id, name string) M {
		return M{"toolCallId": id, "name": name, "arguments": M{"command": "make " + id, "path": "/w/f"}}
	}
	result := func(id, at, content string, extra M) M {
		data := M{"toolCallId": id, "success": true, "result": M{"content": content}}
		for k, v := range extra {
			data[k] = v
		}
		return ev("r-"+id, "tool.execution_complete", at, data)
	}
	sourcetest.Write(t, filepath.Join(root, "s1", "events.jsonl"),
		ev("e1", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "go"}),
		ev("e2", "assistant.message", "2026-11-17T09:00:02.000Z", M{"messageId": "m1", "content": "", "toolRequests": []M{
			request("passes", "bash"), request("fails", "bash"), request("both", "powershell"), request("later", "read_bash"),
			request("view", "view"), request("signal", "bash"), request("detached", "read_bash"),
		}}),
		result("passes", "2026-11-17T09:00:03.000Z", "ok\n<exited with exit code 0>", nil),
		result("fails", "2026-11-17T09:00:03.000Z", "no\n<exited with exit code 2>\n", nil),
		// shellExecution is the CLI's own figure and is taken over the text
		result("both", "2026-11-17T09:00:03.000Z", "<exited with exit code 0>", M{"shellExecution": M{"exitCode": 5}}),
		result("later", "2026-11-17T09:00:03.000Z", "done\n<command with id: 3 exited with exit code 1>", nil),
		// a file that says it exited is a file that says so
		result("view", "2026-11-17T09:00:03.000Z", "1 <exited with exit code 9>", nil),
		result("signal", "2026-11-17T09:00:03.000Z", "killed\n<exited with exit code -1>", nil),
		result("detached", "2026-11-17T09:00:03.000Z", "built\n<shellId: 4 completed with exit code 3>", nil),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	want := map[string]string{"passes": "0 ok", "fails": "2 error", "both": "5 error", "later": "1 error", "view": "- ok", "signal": "-1 error",
		"detached": "3 error"}
	for _, tool := range sourcetest.Tools(s.Turn(t, 1)) {
		got := "-"
		if tool.Exit != nil {
			got = strconv.Itoa(*tool.Exit)
		}
		if got+" "+tool.Status != want[tool.CallID] {
			t.Errorf("%s: %s %s, want %s", tool.CallID, got, tool.Status, want[tool.CallID])
		}
		// powershell, Windows' shell tool, runs commands as bash does
		if tool.CallID == "both" && (tool.Kind != model.ToolExec || tool.Command != "make both") {
			t.Errorf("powershell: %+v", tool)
		}
	}
}

// A result that comes after Ctrl+C is how the call ended, and it ended interrupted; a call
// whose permission was cancelled never ran.
func TestResultsAfterAnAbort(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "s1", "events.jsonl"),
		ev("e1", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "go"}),
		ev("e2", "assistant.message", "2026-11-17T09:00:02.000Z", M{"messageId": "m1", "content": "", "toolRequests": []M{
			{"toolCallId": "a", "name": "bash", "arguments": M{"command": "sleep 100"}},
			{"toolCallId": "b", "name": "edit", "arguments": M{"path": "/w/x"}},
		}}),
		ev("e3", "permission.completed", "2026-11-17T09:00:03.000Z", M{"requestId": "p", "toolCallId": "b", "result": M{"kind": "cancelled"}}),
		ev("e4", "tool.execution_complete", "2026-11-17T09:00:03.500Z", M{"toolCallId": "b", "success": false, "error": M{"message": "Tool call was cancelled"}}),
		ev("e5", "abort", "2026-11-17T09:00:04.000Z", M{"reason": "user_initiated"}),
		ev("e6", "tool.execution_complete", "2026-11-17T09:00:05.000Z", M{"toolCallId": "a", "success": false, "error": M{"message": "Command was terminated"}}),
		// and a second result for a call that has one is not a second call
		ev("e7", "tool.execution_complete", "2026-11-17T09:00:06.000Z", M{"toolCallId": "a", "success": true, "result": M{"content": "late"}}),
		ev("e8", "user.message", "2026-11-17T09:01:00.000Z", M{"content": "again"}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	tools := sourcetest.Tools(s.Turn(t, 1))
	if len(tools) != 2 || tools[0].Status != model.StatusInterrupted || tools[0].Output != "Command was terminated" ||
		tools[1].Status != model.StatusInterrupted || tools[0].EndAt.Format("15:04:05") != "09:00:05" {
		t.Errorf("tools = %+v, %+v", tools[0], tools[1])
	}
	if len(s.Turn(t, 2).Items) != 1 {
		t.Errorf("turn 2 = %+v", s.Turn(t, 2).Items)
	}
}

// How a refused call ended is known from the permission request's answer, or failing that
// from the error the call ended with; either says whether the person said no.
func TestWhoRefusedACall(t *testing.T) {
	root := t.TempDir()
	request := func(id string) M {
		return M{"toolCallId": id, "name": "bash", "arguments": M{"command": "rm -rf build"}}
	}
	failed := func(id, at string, err M) M {
		return ev("r-"+id, "tool.execution_complete", at, M{"toolCallId": id, "success": false, "error": err})
	}
	answered := func(id, at, kind string) M {
		return ev("p-"+id, "permission.completed", at, M{"requestId": "q-" + id, "toolCallId": id, "result": M{"kind": kind}})
	}
	sourcetest.Write(t, filepath.Join(root, "s1", "events.jsonl"),
		ev("e1", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "clean up"}),
		ev("e2", "assistant.message", "2026-11-17T09:00:02.000Z", M{"messageId": "m1", "content": "", "toolRequests": []M{
			request("asked"), request("coded"), request("worded"), request("ruled"), request("hooked"), request("broke"),
		}}),
		answered("asked", "2026-11-17T09:00:03.000Z", "denied-interactively-by-user"),
		failed("asked", "2026-11-17T09:00:03.100Z", M{"message": "no"}),
		failed("coded", "2026-11-17T09:00:03.200Z", M{"message": "no", "code": "rejected"}),
		failed("worded", "2026-11-17T09:00:03.300Z", M{"message": "The user rejected this tool call"}),
		answered("ruled", "2026-11-17T09:00:03.400Z", "denied-by-rules"),
		failed("ruled", "2026-11-17T09:00:03.500Z", M{"message": "Permission to run this tool was denied by a deny rule.", "code": "denied"}),
		answered("hooked", "2026-11-17T09:00:03.600Z", "denied-by-permission-request-hook"),
		failed("hooked", "2026-11-17T09:00:03.700Z", M{"message": "Permission denied by hook."}),
		failed("broke", "2026-11-17T09:00:03.800Z", M{"message": "spawn failed"}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	want := map[string]string{"asked": model.StatusInterrupted, "coded": model.StatusInterrupted, "worded": model.StatusInterrupted,
		"ruled": model.StatusError, "hooked": model.StatusError, "broke": model.StatusError}
	for _, tool := range sourcetest.Tools(s.Turn(t, 1)) {
		if tool.Status != want[tool.CallID] {
			t.Errorf("%s: %s, want %s", tool.CallID, tool.Status, want[tool.CallID])
		}
	}
}

func TestUsage(t *testing.T) {
	for _, c := range []struct {
		name string
		u    usage
		want model.Usage
		cost float64
	}{
		{"the cache inside the input", usage{InputTokens: 100, CacheReadTokens: 60, CacheWriteTokens: 30, OutputTokens: 9, ReasoningTokens: 4},
			model.Usage{Input: 10, CacheRead: 60, CacheWrite: 30, Output: 9, Reasoning: 4}, 0},
		{"no cache", usage{InputTokens: 100, OutputTokens: 9}, model.Usage{Input: 100, Output: 9}, 0},
		// an input smaller than its cache cannot hold it, and stands
		{"a cache the input cannot hold", usage{InputTokens: 10, CacheReadTokens: 60}, model.Usage{Input: 10, CacheRead: 60}, 0},
		{"counts that cannot be", usage{InputTokens: -5, OutputTokens: 1e300, CacheReadTokens: -1}, model.Usage{}, 0},
	} {
		if got := *c.u.canonical(); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	for nano, want := range map[float64]float64{1e9: 0.01, 0: 0, -5: 0, 1e40: 0} {
		u := usage{}
		u.CopilotUsage = &struct {
			TotalNanoAiu float64 `json:"totalNanoAiu"`
		}{nano}
		if got := u.costUSD(); !near(got, want) {
			t.Errorf("%g nano AIU: $%g, want $%g", nano, got, want)
		}
	}
}

func TestLocate(t *testing.T) {
	home := "/home/u"
	if got, want := Spec.Locate(sourcetest.Env(home)), filepath.Join(home, ".copilot", "session-state"); got != want {
		t.Errorf("default: %s, want %s", got, want)
	}
	if got, want := Spec.Locate(sourcetest.Env(home, "COPILOT_HOME", "/moved")), filepath.Join("/moved", "session-state"); got != want {
		t.Errorf("COPILOT_HOME: %s, want %s", got, want)
	}
	// The XDG directories no longer move it
	if got := Spec.Locate(sourcetest.Env(home, "XDG_STATE_HOME", "/x", "XDG_CONFIG_HOME", "/y")); got != filepath.Join(home, ".copilot", "session-state") {
		t.Errorf("XDG: %s", got)
	}
}

// A session is a directory with a log in it; renaming it moves its stamp, as the name is
// in workspace.yaml beside the log.
func TestScan(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", "events.jsonl")
	sourcetest.Write(t, a, ev("e1", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "one"}))
	sourcetest.Write(t, filepath.Join(root, "b", "events.jsonl"), ev("e1", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "two"}))
	// not sessions: a directory with no log, a log's earlier part alone, a stray file
	sourcetest.Write(t, filepath.Join(root, "c", "workspace.yaml"), "id: c")
	sourcetest.Write(t, filepath.Join(root, "d", "events.1.jsonl"), ev("e1", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "x"}))
	sourcetest.Write(t, filepath.Join(root, "events.jsonl"), "")
	src := Spec.Open(root)
	stamps := func() map[string]string {
		refs, err := src.Scan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range refs {
			out[r.Key] = r.Stamp
		}
		return out
	}
	before := stamps()
	if len(before) != 2 || before[a] == "" {
		t.Fatalf("refs = %v", before)
	}
	sourcetest.Write(t, filepath.Join(root, "a", "workspace.yaml"), "summary: the CLI's summary\nname: renamed")
	if after := stamps(); after[a] == before[a] {
		t.Error("a name given does not move the stamp")
	}
	s := sourcetest.ByID(t, sourcetest.All(t, src), "a")
	if s.Session.Title != "renamed" {
		t.Errorf("title = %q", s.Session.Title)
	}
}

func TestScanOfAMissingStore(t *testing.T) {
	src := Spec.Open(filepath.Join(t.TempDir(), "never-made"))
	if src.Exists() {
		t.Error("a store that is not there exists")
	}
	if refs, err := src.Scan(context.Background()); err != nil || len(refs) != 0 {
		t.Errorf("refs %v, err %v", refs, err)
	}
}

// Lines that are not events, events of a kind not known, and fields of the wrong type.
func TestWhatIsNotAnEvent(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "s1", "events.jsonl"),
		`not json`, `null`, `[1]`, `"x"`,
		ev("e1", "session.start", "2026-11-17T09:00:00.000Z", M{"sessionId": 7, "context": "nowhere", "copilotVersion": "1.0.200"}),
		ev("e2", "user.message", "2026-11-17T09:00:01.000Z", M{"content": "after the damage", "delivery": 3}),
		ev("e3", "assistant.message", "2026-11-17T09:00:02.000Z", M{"messageId": "m1", "content": "still here", "toolRequests": "none"}),
		ev("e4", "session.usage_record", "2026-11-17T09:00:03.000Z", M{"usage": M{"inputTokens": "many", "outputTokens": 5}}),
		ev("e5", "session.brand_new", "2026-11-17T09:00:04.000Z", M{}),
		M{"id": "e6", "timestamp": "2026-11-17T09:00:05.000Z"},
		// an ephemeral event does not reach the log; one that does is not read
		M{"id": "e7", "timestamp": "2026-11-17T09:00:06.000Z", "type": "assistant.message", "ephemeral": true, "data": M{"content": "never"}},
	)
	s := sourcetest.One(t, Spec.Open(root))
	if s.Unread["event:session.brand_new"] != 1 || s.Unread["event:(untyped)"] != 1 || len(s.Unread) != 2 {
		t.Errorf("unread = %v", s.Unread)
	}
	if s.Session.ID != "s1" || s.Session.Version != "1.0.200" || s.Turn(t, 1).Ask != "after the damage" || s.Turn(t, 1).Answer != "still here" {
		t.Errorf("session = %+v, turn = %+v", s.Session, s.Turn(t, 1))
	}
	if s.Stats.Usage != (model.Usage{Output: 5}) {
		t.Errorf("usage = %+v", s.Stats.Usage)
	}
}
