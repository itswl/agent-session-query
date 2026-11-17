package transcript

import (
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
)

func at(sec int) time.Time { return time.Date(2026, 10, 1, 12, 0, sec, 0, time.UTC) }

func collect(t *testing.T, msgs ...Message) Collected {
	t.Helper()
	c := &Collector{}
	w := New(c)
	for _, m := range msgs {
		w.Add(m)
	}
	stats, err := w.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return Collected{Session: w.S, Stats: stats, Turns: c.Turns}
}

func user(sec int, text string) Message {
	return Message{Role: User, At: at(sec), Blocks: []Block{TextOf(text)}}
}

func TestTurnsStartAtAPersonsWords(t *testing.T) {
	got := collect(t,
		user(0, "<environment_context>cwd</environment_context>"),
		user(1, "fix the login bug<runtime_info>T3</runtime_info>"),
		Message{Role: Assistant, At: at(2), Model: "m1", Blocks: []Block{ThinkingOf("hmm"), CallOf("c1", "Bash", map[string]any{"command": "go test ./..."})}},
		Message{Role: User, At: at(3), Blocks: []Block{ResultOf("c1", "", "FAIL", Outcome{Exit: ExitOf(1)})}},
		user(4, "<task-notification>done</task-notification>"),
		Message{Role: Assistant, At: at(5), Model: "m1", Blocks: []Block{TextOf("Fixed it.")}},
		user(3600, "<system-reminder>later</system-reminder>"),
		user(4000, "now add a test"),
	)
	if len(got.Turns) != 3 {
		t.Fatalf("turns = %d, want the preamble and two exchanges", len(got.Turns))
	}
	pre, one, two := got.Turns[0], got.Turns[1], got.Turns[2]
	if pre.Index != 0 || len(pre.Items) != 1 || pre.Items[0].Kind != model.Context {
		t.Errorf("preamble = %+v", pre)
	}
	if one.Ask != "fix the login bug" {
		t.Errorf("ask = %q, want the words without the harness block", one.Ask)
	}
	if one.Answer != "Fixed it." || one.Tools != 1 || one.Failures != 1 {
		t.Errorf("turn 1 = answer %q tools %d failures %d", one.Answer, one.Tools, one.Failures)
	}
	// The reminder an hour later is the turn's last row, not the end of its work
	if !one.EndedAt.Equal(at(5)) || !one.LastAt.Equal(at(3600)) {
		t.Errorf("turn 1 ended %v last %v", one.EndedAt, one.LastAt)
	}
	tool := one.Items[2].Tool
	if tool == nil || !tool.Done || tool.Status != model.StatusError || tool.Command != "go test ./..." || tool.Kind != model.ToolExec {
		t.Errorf("tool = %+v", tool)
	}
	if !two.Interrupted {
		t.Error("a turn nothing answered should read as interrupted")
	}
	if got.Session.Title != "fix the login bug" {
		t.Errorf("title = %q", got.Session.Title)
	}
	if got.Stats.Turns != 2 || got.Stats.Tools != 1 || got.Stats.Failures != 1 {
		t.Errorf("stats = %+v", got.Stats)
	}
	// Seq runs across turns
	for i, it := range got.Items() {
		if it.Seq != i {
			t.Fatalf("item %d has seq %d", i, it.Seq)
		}
	}
}

func TestAFailedEditIsNotAChange(t *testing.T) {
	got := collect(t,
		user(0, "edit two files"),
		Message{Role: Assistant, At: at(1), Blocks: []Block{
			CallOf("a", "Edit", map[string]any{"file_path": "/p/a.go"}),
			CallOf("b", "Edit", map[string]any{"file_path": "/p/b.go"}),
			CallOf("c", "Read", map[string]any{"file_path": "/p/c.go"}),
			CallOf("", "Write", map[string]any{"path": "/p/d.go"}),
		}},
		Message{Role: ToolRole, At: at(2), Blocks: []Block{
			ResultOf("a", "", "ok", Outcome{Status: model.StatusOK}),
			ResultOf("b", "", "no match", Outcome{Status: model.StatusError}),
			ResultOf("c", "", "…", Outcome{}),
		}},
	)
	want := map[string]bool{"/p/a.go": true, "/p/b.go": false, "/p/c.go": false, "/p/d.go": true}
	files := got.Turns[0].Files
	if len(files) != len(want) {
		t.Fatalf("files = %+v", files)
	}
	for _, f := range files {
		if want[f.Path] != f.Changed {
			t.Errorf("%s changed = %v", f.Path, f.Changed)
		}
	}
}

func TestOneCallPerResponse(t *testing.T) {
	got := collect(t,
		user(0, "go"),
		Message{Role: Assistant, At: at(1), Response: "r1", Model: "opus", Usage: &model.Usage{Input: 10, Output: 5, CacheRead: 100}, Blocks: []Block{ThinkingOf("t")}},
		Message{Role: Assistant, At: at(2), Response: "r1", Model: "opus", Usage: &model.Usage{Input: 10, Output: 40, CacheRead: 100}, Blocks: []Block{TextOf("hello")}},
		Message{Role: Assistant, At: at(3), Model: SyntheticModel, Blocks: []Block{TextOf("No response requested.")}},
		Message{Role: Assistant, At: at(9), Response: "r2", Model: "opus", Usage: &model.Usage{Input: 1, Output: 1}, Blocks: []Block{TextOf("bye")}},
	)
	turn := got.Turns[0]
	if len(turn.Calls) != 2 {
		t.Fatalf("calls = %d, want one per response and none for the synthetic row", len(turn.Calls))
	}
	if turn.Calls[0].Usage.Output != 40 || !turn.Calls[0].EndAt.Equal(at(2)) {
		t.Errorf("a split response takes its final figures: %+v", turn.Calls[0])
	}
	if turn.Usage != (model.Usage{Input: 11, Output: 41, CacheRead: 100}) {
		t.Errorf("usage = %+v", turn.Usage)
	}
	if turn.Items[1].Call != 1 || turn.Items[2].Call != 1 || turn.Items[3].Call != 0 || turn.Items[4].Call != 2 {
		t.Errorf("items name their calls wrongly: %+v", turn.Items)
	}
	if turn.Answer != "bye" {
		t.Errorf("answer = %q", turn.Answer)
	}
}

// What a CLI totalled for the session is the session's total where it can be: where it
// holds at least what the responses add up to.
func TestRecordedTotals(t *testing.T) {
	read := func(rec *model.Recorded, responses ...*model.Usage) model.Stats {
		t.Helper()
		c := &Collector{}
		w := New(c)
		w.S.Recorded = rec
		w.Add(user(0, "go"))
		for i, u := range responses {
			w.Add(Message{Role: Assistant, At: at(i + 1), Model: "m", Usage: u, CostUSD: 0.25, Blocks: []Block{TextOf("ok")}})
		}
		stats, err := w.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return stats
	}
	total := &model.Usage{Input: 100, Output: 30, CacheRead: 900, Reasoning: 4}

	// Hermes: no response says what it cost, the session does
	if got := read(&model.Recorded{Usage: total, CostUSD: 2}, nil, nil); got.Usage != *total || got.CostUSD != 2 || got.Calls != 2 {
		t.Errorf("no counters per response: %+v", got)
	}
	// OpenCode: the session's total holds a call no response accounts for
	if got := read(&model.Recorded{Usage: total, CostUSD: 2}, &model.Usage{Input: 60, Output: 20, CacheRead: 900, Reasoning: 4}); got.Usage != *total || got.CostUSD != 2 {
		t.Errorf("a total that covers the responses: %+v", got)
	}
	// A total that holds less than was read in any one counter is not the total of this
	sum := model.Usage{Input: 60, Output: 31, CacheRead: 900}
	if got := read(&model.Recorded{Usage: total, CostUSD: 2}, &model.Usage{Input: 30, Output: 20, CacheRead: 900}, &model.Usage{Input: 30, Output: 11}); got.Usage != sum || got.CostUSD != 0.5 {
		t.Errorf("a total that does not cover the responses replaced them: %+v", got)
	}
	// Claude Code: a cost and no counters; the responses' own are the usage
	if got := read(&model.Recorded{CostUSD: 2}, &model.Usage{Input: 60, Output: 20}); got.Usage != (model.Usage{Input: 60, Output: 20}) || got.CostUSD != 2 {
		t.Errorf("a recorded cost alone: %+v", got)
	}
	if got := read(nil, &model.Usage{Input: 60, Output: 20}); got.Usage != (model.Usage{Input: 60, Output: 20}) || got.CostUSD != 0.25 {
		t.Errorf("nothing recorded: %+v", got)
	}
}

func TestInheritedRowsBelongToTheParent(t *testing.T) {
	got := collect(t,
		Message{Role: User, At: at(0), Inherited: true, Blocks: []Block{TextOf("the parent's ask")}},
		Message{Role: Assistant, At: at(1), Inherited: true, Usage: &model.Usage{Output: 999}, Blocks: []Block{TextOf("the parent's reply"), CallOf("x", "Bash", nil)}},
		user(2, "<fork-boilerplate>rules</fork-boilerplate>\n\nreview the diff"),
		Message{Role: Assistant, At: at(3), Usage: &model.Usage{Output: 7}, Blocks: []Block{TextOf("done")}},
	)
	if len(got.Turns) != 2 || got.Turns[0].Index != 0 {
		t.Fatalf("turns = %+v", got.Turns)
	}
	for _, it := range got.Turns[0].Items {
		if !it.Inherited {
			t.Errorf("item %d of the copy is not marked inherited", it.Seq)
		}
	}
	if got.Turns[0].Items[0].Kind != model.Prompt {
		t.Error("the parent's ask is still a prompt in the copy")
	}
	if got.Turns[1].Ask != "review the diff" {
		t.Errorf("ask = %q", got.Turns[1].Ask)
	}
	if got.Stats.Usage.Output != 7 || got.Stats.Tools != 0 || got.Stats.Turns != 1 {
		t.Errorf("a fork's figures are its own: %+v", got.Stats)
	}
	if got.Session.Title != "review the diff" {
		t.Errorf("title = %q", got.Session.Title)
	}
}

func TestResultsWithoutIDsFindTheirCall(t *testing.T) {
	got := collect(t,
		user(0, "go"),
		Message{Role: Assistant, At: at(1), Blocks: []Block{CallOf("", "read_file", nil), CallOf("", "shell", map[string]any{"cmd": []any{"ls", "-l"}})}},
		Message{Role: ToolRole, At: at(2), Blocks: []Block{ResultOf("", "shell", "a b", Outcome{})}},
		Message{Role: ToolRole, At: at(3), Blocks: []Block{ResultOf("", "", "contents", Outcome{})}},
		Message{Role: ToolRole, At: at(4), Blocks: []Block{ResultOf("gone", "web_fetch", "late", Outcome{})}},
	)
	items := got.Turns[0].Items
	if items[1].Tool.Output != "contents" || items[2].Tool.Output != "a b" || items[2].Tool.Command != "ls -l" {
		t.Errorf("pairing: %+v / %+v", items[1].Tool, items[2].Tool)
	}
	if len(items) != 4 || items[3].Tool.Name != "web_fetch" || !items[3].Tool.Done {
		t.Errorf("a result with no call stands on its own: %+v", items)
	}
}

func TestQueuedPromptOpensNoTurn(t *testing.T) {
	got := collect(t,
		user(0, "first"),
		Message{Role: Assistant, At: at(1), Blocks: []Block{CallOf("c", "Bash", nil)}},
		Message{Role: User, At: at(2), Queued: true, Blocks: []Block{TextOf("also check the logs")}},
		Message{Role: Assistant, At: at(3), Blocks: []Block{TextOf("ok")}},
	)
	if len(got.Turns) != 1 {
		t.Fatalf("turns = %d", len(got.Turns))
	}
	q := got.Turns[0].Items[2]
	if q.Kind != model.Prompt || !q.Queued {
		t.Errorf("queued prompt = %+v", q)
	}
}

func TestAskOf(t *testing.T) {
	cases := []struct {
		in, ask string
		ok      bool
	}{
		{"hello", "hello", true},
		{"  ", "", false},
		{"<command-name>/model</command-name>", "", false},
		{"<local-command-stdout>x</local-command-stdout>", "", false},
		{"<foo>bar</foo>", "", false},
		{"<foo>bar</foo> and then some", "<foo>bar</foo> and then some", true},
		{`<teammate-message teammate_id="team-lead" summary="x">do the thing</teammate-message>`, "do the thing", true},
		{`<teammate-message teammate_id="team-lead">{"type":"shutdown_request"}</teammate-message>`, `{"type":"shutdown_request"}`, false},
		{`<teammate-message teammate_id="worker-1">news</teammate-message>`, "", false},
		{"Caveat: the messages below", "", false},
		{"ship it\n\n<runtime_info>x</runtime_info>\n<pull_request_linking>y</pull_request_linking>", "ship it", true},
		{"<runtime_info>only</runtime_info>", "", false},
	}
	for _, c := range cases {
		ask, ok := AskOf(c.in)
		if ask != c.ask || ok != c.ok {
			t.Errorf("AskOf(%q) = %q, %v; want %q, %v", c.in, ask, ok, c.ask, c.ok)
		}
	}
}

func TestToolKind(t *testing.T) {
	for name, want := range map[string]string{
		"Bash": model.ToolExec, "NotebookEdit": model.ToolWrite, "apply_patch": model.ToolWrite,
		"Read": model.ToolRead, "Grep": model.ToolSearch, "WebFetch": model.ToolNet, "Agent": model.ToolAgent,
		"publish": model.ToolOther, "runbook": model.ToolOther, "mcp__x__list_things": model.ToolSearch,
		"local_shell": model.ToolExec, "TodoWrite": model.ToolWrite,
	} {
		if got := ToolKind(name); got != want {
			t.Errorf("ToolKind(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCallFactsReadsPatches(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: a/b.go\n@@\n*** Add File: c.md\n*** End Patch"
	files, _ := callFacts(model.ToolWrite, inputJSON(map[string]any{"input": patch}))
	if len(files) != 2 || files[0] != "a/b.go" || files[1] != "c.md" {
		t.Errorf("files = %v", files)
	}
	files, _ = callFacts(model.ToolWrite, inputJSON(patch)) // a freeform tool's bare string
	if len(files) != 2 {
		t.Errorf("bare patch: files = %v", files)
	}
	_, cmd := callFacts(model.ToolExec, inputJSON(`{"command":"make test","timeout":5}`))
	if cmd != "make test" {
		t.Errorf("command = %q", cmd)
	}
}

// Passing over a record costs a map lookup, however many there are of it: a store that
// drifted holds thousands.
func BenchmarkUnread(b *testing.B) {
	w := New(&Collector{})
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		w.Unread("row:queue-operation")
		w.Unread("block:server_tool_use")
	}
}

func TestUnreadIsCheapOnceSeen(t *testing.T) {
	w := New(&Collector{})
	w.Unread("row:x")
	if allocs := testing.AllocsPerRun(100, func() { w.Unread("row:x") }); allocs > 0 {
		t.Errorf("%v allocations for a kind already seen", allocs)
	}
}
