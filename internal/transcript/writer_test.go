package transcript

import (
	"strings"
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

// A response measured on a row of its own, written once it was done, and given the time the
// response began: the measure is the call's, which ends that long after, and so does the turn.
func TestAResponseMeasuredOnALaterRow(t *testing.T) {
	got := collect(t,
		user(0, "go"),
		Message{Role: Assistant, At: at(5), Response: "r1", Model: "m", Blocks: []Block{TextOf("done")}},
		Message{Role: Assistant, At: at(2), Response: "r1", Model: "m", Usage: &model.Usage{Input: 3, Output: 4}, DurMs: 4000, TTFTMs: 700},
	)
	turn := got.Turns[0]
	if len(turn.Calls) != 1 {
		t.Fatalf("calls = %+v", turn.Calls)
	}
	if c := turn.Calls[0]; c.DurMs != 4000 || c.TTFTMs != 700 || !c.At.Equal(at(5)) || !c.EndAt.Equal(at(6)) || c.Usage == nil {
		t.Errorf("call = %+v", c)
	}
	if !turn.EndedAt.Equal(at(6)) {
		t.Errorf("turn ended %v", turn.EndedAt)
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

// An event that ended the turn before the agent was done marks it interrupted, wherever
// the work had got to; one that did not, an error the work went on past, marks nothing.
// A fork's copy of either is the parent's.
func TestAnEventThatStopsTheTurn(t *testing.T) {
	stopped := EventOf("error", "[API Error: Quota exceeded. (Status: RESOURCE_EXHAUSTED)]")
	stopped.Stops = true
	got := collect(t,
		Message{Role: User, At: at(0), Inherited: true, Blocks: []Block{TextOf("the parent's ask")}},
		Message{Role: System, At: at(1), Inherited: true, Blocks: []Block{stopped}},
		user(2, "build it"),
		Message{Role: Assistant, At: at(3), Blocks: []Block{CallOf("c1", "run_shell_command", map[string]any{"command": "make"})}},
		Message{Role: ToolRole, At: at(4), Blocks: []Block{ResultOf("c1", "", "ok", Outcome{Status: model.StatusOK})}},
		Message{Role: System, At: at(5), Blocks: []Block{stopped}},
		user(6, "try again"),
		Message{Role: System, At: at(7), Blocks: []Block{EventOf("error", "a retry went through")}},
		Message{Role: Assistant, At: at(8), Blocks: []Block{TextOf("Built.")}},
	)
	if len(got.Turns) != 3 {
		t.Fatalf("turns = %d, want the copy and two asks", len(got.Turns))
	}
	if got.Turns[0].Interrupted {
		t.Error("the copy's stop is the parent's turn's, not this session's")
	}
	one, two := got.Turns[1], got.Turns[2]
	if !one.Interrupted || !one.EndedAt.Equal(at(5)) || one.Items[len(one.Items)-1].Event != "error" {
		t.Errorf("turn 1 interrupted=%v ended %v: the error ended it there", one.Interrupted, one.EndedAt)
	}
	if two.Interrupted || two.Answer != "Built." {
		t.Errorf("turn 2 interrupted=%v answer %q: an error that stopped nothing marks nothing", two.Interrupted, two.Answer)
	}
}

// A turn ends on an error when the error is the last of the agent's work: nothing it said,
// reasoned, called or had back came after. Events that are no work of the agent's — a
// routine step, an error the CLI says came after the work, a change of model, a compaction,
// the person's stop — neither end it nor go on past it, and a fork's copy of a row is the
// parent's.
func TestTheErrorATurnEndedOn(t *testing.T) {
	const refused = "402 Insufficient Balance (insufficient_balance)"
	failed := Message{Role: System, At: at(2), Blocks: []Block{EventOf(model.EventError, "  "+refused+"\n")}}
	routine := EventOf("step", "Step 1 · 3s")
	routine.Routine = true
	after := EventOf(model.EventError, "a command typed later failed")
	after.After = true
	stops := EventOf(model.EventError, refused)
	stops.Stops = true
	system := func(sec int, blocks ...Block) Message { return Message{Role: System, At: at(sec), Blocks: blocks} }
	assistant := func(sec int, blocks ...Block) Message { return Message{Role: Assistant, At: at(sec), Blocks: blocks} }

	cases := []struct {
		name        string
		msgs        []Message
		want        string
		interrupted bool
	}{
		{"the error is the last thing", []Message{failed}, refused, false},
		{"a routine step after it", []Message{failed, system(3, routine)}, refused, false},
		{"events that are no work after it", []Message{failed, system(3, after), system(4, EventOf(model.EventModelChange, "m2")),
			system(5, EventOf(model.EventCompaction, "summary"))}, refused, false},
		{"a word typed meanwhile", []Message{failed, {Role: User, At: at(3), Queued: true, Blocks: []Block{TextOf("and the logs")}}}, refused, false},
		{"a command the person ran beside it", []Message{failed, system(3, CallOf("p1", "shell", map[string]any{"command": "ls"}),
			ResultOf("p1", "shell", "a b", Outcome{Status: model.StatusOK}))}, refused, false},
		{"the CLI's words in the model's place", []Message{failed, {Role: Assistant, At: at(3), Model: SyntheticModel, Blocks: []Block{TextOf("API Error: 402")}}}, refused, false},
		{"replied, then the error", []Message{assistant(1, TextOf("Running the tests."), EventOf(model.EventError, refused))}, refused, false},
		{"words and reasoning that say nothing after it", []Message{failed, assistant(3, TextOf(" \n"), ThinkingOf("\t"))}, refused, false},
		{"a reply after it", []Message{failed, assistant(3, TextOf("It builds the image."))}, "", false},
		{"reasoning after it", []Message{failed, assistant(3, ThinkingOf("try another way"))}, "", false},
		{"a call after it", []Message{failed, assistant(3, CallOf("c1", "Bash", map[string]any{"command": "make"}))}, "", false},
		{"a result after it", []Message{assistant(1, CallOf("c1", "Bash", map[string]any{"command": "make"})), failed,
			{Role: ToolRole, At: at(3), Blocks: []Block{ResultOf("c1", "", "ok", Outcome{Status: model.StatusOK})}}}, "", false},
		{"an error the CLI says came after the work", []Message{assistant(1, TextOf("Done.")), system(2, after)}, "", false},
		{"a fork's copy of an error", []Message{assistant(1, TextOf("Done.")), {Role: System, At: at(2), Inherited: true, Blocks: []Block{EventOf(model.EventError, refused)}}}, "", false},
		{"an error that said nothing", []Message{system(2, EventOf(model.EventError, " "))}, noMessage, false},
		{"an error that cut the turn off", []Message{assistant(1, CallOf("c1", "Bash", nil)), system(2, stops)}, refused, true},
		{"the person's stop after it", []Message{failed, system(3, EventOf(model.EventInterrupted, "Interrupted"))}, refused, true},
		{"the person's stop alone", []Message{assistant(1, TextOf("Looking.")), system(3, EventOf(model.EventInterrupted, "Interrupted"))}, "", true},
	}
	for _, tc := range cases {
		got := collect(t, append([]Message{user(0, "deploy it")}, tc.msgs...)...)
		turn := got.Turns[0]
		if turn.Error != tc.want || turn.Interrupted != tc.interrupted {
			t.Errorf("%s: error %q, interrupted %v; want %q, %v", tc.name, turn.Error, turn.Interrupted, tc.want, tc.interrupted)
		}
	}

	// The answer stays the last thing the agent said, though an error came after it; and
	// what precedes the first ask ends on nothing
	got := collect(t, system(0, EventOf(model.EventError, "no credentials")), user(1, "deploy it"),
		assistant(2, TextOf("Running the tests."), EventOf(model.EventError, refused)), user(3, "again"), assistant(4, TextOf("Deployed.")))
	if zero, one, two := got.Turns[0], got.Turns[1], got.Turns[2]; zero.Error != "" || one.Error != refused || one.Answer != "Running the tests." || two.Error != "" {
		t.Errorf("turn 0 error %q; turn 1 error %q answer %q; turn 2 error %q", zero.Error, one.Error, one.Answer, two.Error)
	}
}

// An event that came once the work was done — the person taking the thread back an hour
// later — is the turn's last row, not the end of its work.
func TestAnEventAfterTheWork(t *testing.T) {
	after := EventOf("rollback", "Rolled back the last turn")
	after.After = true
	got := collect(t,
		user(0, "name it"),
		Message{Role: Assistant, At: at(2), Blocks: []Block{TextOf("relay")}},
		Message{Role: System, At: at(3600), Blocks: []Block{after}},
	)
	turn := got.Turns[0]
	if !turn.EndedAt.Equal(at(2)) || !turn.LastAt.Equal(at(3600)) || turn.Items[2].Event != "rollback" || turn.Interrupted {
		t.Errorf("ended %v, last %v, items %+v", turn.EndedAt, turn.LastAt, turn.Items)
	}
}

// An event the CLI writes as a matter of course is marked so on its item, for what draws
// and searches the record; one that is not stays unmarked. Being routine changes nothing
// of the turn: the step is work, and ends it where it came.
func TestARoutineEvent(t *testing.T) {
	routine := EventOf("step", "Step 1 · 2s")
	routine.Routine = true
	got := collect(t,
		user(0, "name it"),
		Message{Role: Assistant, At: at(1), Blocks: []Block{TextOf("relay")}},
		Message{Role: System, At: at(2), Blocks: []Block{routine}},
		Message{Role: System, At: at(3), Blocks: []Block{EventOf("error", "quota")}},
	)
	turn := got.Turns[0]
	if got := turn.Items[2]; got.Event != "step" || !got.Routine {
		t.Errorf("step = %+v, want routine", got)
	}
	if got := turn.Items[3]; got.Event != "error" || got.Routine {
		t.Errorf("error = %+v, want not routine", got)
	}
	if !turn.EndedAt.Equal(at(3)) || turn.Interrupted {
		t.Errorf("ended %v, interrupted %v", turn.EndedAt, turn.Interrupted)
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
		"local_shell": model.ToolExec, "TodoWrite": model.ToolWrite, "powershell": model.ToolExec, "read_powershell": model.ToolExec,
		// ripgrep by its command's name, as GitHub Copilot CLI names its search tool
		"rg": model.ToolSearch,
		// what is done to an agent is the agents' work: messaging one, reading what it said,
		// listing them (Copilot CLI's write_agent, read_agent, list_agents; Codex's
		// collaboration.list_agents; Claude Code's ListAgents), and an agent called as a tool
		// of its own name (Gemini CLI's browser_agent)
		"write_agent": model.ToolAgent, "read_agent": model.ToolAgent, "list_agents": model.ToolAgent,
		"collaboration.list_agents": model.ToolAgent, "ListAgents": model.ToolAgent, "browser_agent": model.ToolAgent,
		"spawn_agent": model.ToolAgent, "list_subagents": model.ToolAgent,
		// OpenClaw's subagents tool, and a browser run by a subagent
		"subagents": model.ToolAgent, "browser_subagent": model.ToolAgent,
		// a name that only begins with one is not, and TaskCreate's task is a to-do
		"TaskCreate": model.ToolWrite, "agent_write_file": model.ToolWrite,
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
	// Antigravity's run_command names it CommandLine
	if _, cmd := callFacts(model.ToolExec, inputJSON(`{"CommandLine":"ls /missing","Cwd":"/w"}`)); cmd != "ls /missing" {
		t.Errorf("CommandLine = %q", cmd)
	}
}

// A file a call names relative to the directory its message ran in is named by its whole
// path, written the way that directory is; a whole path, one from home, and any path of a
// message that names no whole directory are as the call named them.
func TestFilesRelativeToTheDirectory(t *testing.T) {
	for _, c := range []struct{ dir, file, want string }{
		{"/work/api", "internal/x.go", "/work/api/internal/x.go"},
		{"/work/api/", "./docs/../README.md", "/work/api/README.md"},
		{"/work/api", "/etc/hosts", "/etc/hosts"},
		{"/work/api", "~/notes.md", "~/notes.md"},
		{"/work/api", `C:\other\x.go`, `C:\other\x.go`},
		{`C:\work\api`, "internal/x.go", `C:\work\api\internal\x.go`},
		{`C:\work\api`, `internal\y.go`, `C:\work\api\internal\y.go`},
		{"C:/work/api", "x.go", `C:\work\api\x.go`},
		{`\\server\share\api`, "x.go", `\\server\share\api\x.go`},
		{`C:\work\api`, "/rooted.go", "/rooted.go"},
		{"work/api", "x.go", "x.go"},
		{"", "x.go", "x.go"},
	} {
		if got := within(c.dir, c.file); got != c.want {
			t.Errorf("within(%q, %q) = %q, want %q", c.dir, c.file, got, c.want)
		}
	}
	got := collect(t, user(0, "fix it"),
		Message{Role: Assistant, At: at(1), Dir: "/work/api", Blocks: []Block{CallOf("c1", "apply_patch", map[string]any{"input": "*** Begin Patch\n*** Update File: a.go\n*** End Patch"})}},
		Message{Role: ToolRole, At: at(2), Blocks: []Block{ResultOf("c1", "", "Success.", Outcome{Status: model.StatusOK})}},
		Message{Role: Assistant, At: at(3), Blocks: []Block{CallOf("c2", "Read", map[string]any{"path": "b.go"})}},
	)
	turn := got.Turns[0]
	if len(turn.Files) != 2 || turn.Files[0] != (model.File{Path: "/work/api/a.go", Changed: true}) || turn.Files[1].Path != "b.go" {
		t.Errorf("files = %+v", turn.Files)
	}
	if f := turn.Items[1].Tool.Files; len(f) != 1 || f[0] != "/work/api/a.go" {
		t.Errorf("the call names %v", f)
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

// What is kept to clean a kind once is kept small: a kind as long as a line is cleaned
// each time rather than held.
func TestUnreadKeepsNoLongKind(t *testing.T) {
	w := New(&Collector{})
	w.Unread("row:" + strings.Repeat("x", 1<<20))
	if len(w.kinds) != 0 {
		t.Errorf("%d raw kinds kept", len(w.kinds))
	}
}
