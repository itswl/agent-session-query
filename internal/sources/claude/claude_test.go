package claude

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

type M = map[string]any

func userRow(uuid, ts string, content any, extra ...M) M {
	row := M{"type": "user", "uuid": uuid, "timestamp": ts, "sessionId": "sess-1", "cwd": "/work/proj",
		"gitBranch": "main", "version": "2.1.300", "message": M{"role": "user", "content": content}}
	for _, e := range extra {
		for k, v := range e {
			row[k] = v
		}
	}
	return row
}

func assistantRow(uuid, ts, response string, usage M, content ...M) M {
	return M{"type": "assistant", "uuid": uuid, "timestamp": ts, "sessionId": "sess-1",
		"message": M{"id": response, "role": "assistant", "model": "claude-opus", "content": content, "usage": usage, "stop_reason": "end_turn"}}
}

// sessionRows is a session of one turn, among rows that are not conversation.
func sessionRows() []any {
	return []any{
		M{"type": "queue-operation", "operation": "enqueue"},
		M{"type": "attachment", "attachment": M{"type": "hook_success", "content": "hook says hi"}},
		userRow("u1", "2026-10-01T10:00:00Z", "fix the flaky test"),
		assistantRow("a1", "2026-10-01T10:00:05Z", "msg_1", M{"input_tokens": 10, "output_tokens": 3, "cache_read_input_tokens": 500, "cache_creation_input_tokens": 20},
			M{"type": "thinking", "thinking": "look at it"}),
		// one response written as several rows, a running output count on each
		assistantRow("a2", "2026-10-01T10:00:06Z", "msg_1", M{"input_tokens": 10, "output_tokens": 40, "cache_read_input_tokens": 500, "cache_creation_input_tokens": 20},
			M{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": M{"command": "go test ./..."}}),
		userRow("u2", "2026-10-01T10:00:09Z", []M{{"type": "tool_result", "tool_use_id": "toolu_1", "content": "FAIL", "is_error": true}},
			M{"toolUseResult": M{"durationMs": 2500}}),
		M{"type": "user", "isSidechain": true, "uuid": "side", "timestamp": "2026-10-01T10:00:10Z", "message": M{"role": "user", "content": "a subagent's row"}},
		assistantRow("a3", "2026-10-01T10:00:20Z", "msg_2", M{"input_tokens": 1, "output_tokens": 9},
			M{"type": "text", "text": "The test depended on map order; fixed."}),
		M{"type": "ai-title", "aiTitle": "Fix flaky test"},
		M{"type": "cost-state", "totalCostUSD": 0.42},
	}
}

func TestSession(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "-work-proj", "sess-1.jsonl"), sessionRows()...)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)

	if s.Session.ID != "sess-1" || s.Session.Cwd != "/work/proj" || s.Session.Branch != "main" || s.Session.Version != "2.1.300" {
		t.Errorf("session = %+v", s.Session)
	}
	if s.Session.Title != "Fix flaky test" {
		t.Errorf("title = %q, want the title Claude Code wrote", s.Session.Title)
	}
	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d: the hook row and the sidechain row are not conversation", len(s.Turns))
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt thinking tool reply" {
		t.Errorf("items = %s", got)
	}
	if turn.Ask != "fix the flaky test" || !strings.HasPrefix(turn.Answer, "The test depended") {
		t.Errorf("ask %q answer %q", turn.Ask, turn.Answer)
	}
	tool := sourcetest.Tools(turn)[0]
	if tool.Name != "Bash" || tool.Command != "go test ./..." || tool.Status != model.StatusError || tool.Output != "FAIL" || tool.DurMs != 2500 {
		t.Errorf("tool = %+v", tool)
	}
	// One call per response, at the response's final figures
	if len(turn.Calls) != 2 || turn.Calls[0].Usage.Output != 40 {
		t.Fatalf("calls = %+v", turn.Calls)
	}
	want := model.Usage{Input: 11, Output: 49, CacheRead: 500, CacheWrite: 20}
	if s.Stats.Usage != want {
		t.Errorf("usage = %+v, want %+v", s.Stats.Usage, want)
	}
	if s.Stats.CostUSD != 0.42 {
		t.Errorf("cost = %v, want the session's own total", s.Stats.CostUSD)
	}
}

// eventsRows is a session of what happens around the two sides' words: a prompt queued
// mid-turn, an interruption, a compaction, a failed hook, slash commands.
func eventsRows() []any {
	return []any{
		userRow("u1", "2026-10-01T10:00:00Z", "start the migration"),
		assistantRow("a1", "2026-10-01T10:00:01Z", "m1", nil, M{"type": "tool_use", "id": "t1", "name": "Bash", "input": M{"command": "sleep 100"}}),
		M{"type": "attachment", "uuid": "q1", "timestamp": "2026-10-01T10:00:30Z",
			"attachment": M{"type": "queued_command", "commandMode": "prompt", "prompt": "also back up first"}},
		M{"type": "attachment", "uuid": "q2", "timestamp": "2026-10-01T10:00:31Z",
			"attachment": M{"type": "queued_command", "commandMode": "task-notification", "prompt": "task done"}},
		userRow("u2", "2026-10-01T10:00:40Z", []M{{"type": "tool_result", "tool_use_id": "t1", "content": "[Request interrupted by user for tool use]"}}),
		userRow("u3", "2026-10-01T10:00:41Z", []M{{"type": "text", "text": "[Request interrupted by user]"}}),
		M{"type": "system", "subtype": "compact_boundary", "uuid": "c1", "timestamp": "2026-10-01T10:01:00Z", "content": "Conversation compacted"},
		userRow("u4", "2026-10-01T10:01:01Z", "This session is being continued from a previous conversation…", M{"isCompactSummary": true}),
		M{"type": "system", "subtype": "stop_hook_summary", "uuid": "h1", "timestamp": "2026-10-01T10:01:02Z", "hookErrors": []string{"lint failed"}},
		M{"type": "system", "subtype": "turn_duration", "uuid": "d1", "timestamp": "2026-10-01T10:01:03Z"},
		userRow("u5", "2026-10-01T10:02:00Z", "<command-name>/model</command-name>"),
		userRow("u6", "2026-10-01T10:02:01Z", "<local-command-stdout>Set model</local-command-stdout>"),
		userRow("u7", "2026-10-01T10:03:00Z", "<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>the diff</command-args>"),
		userRow("u8", "2026-10-01T10:03:01Z", "You are reviewing a diff. Follow these steps…", M{"isMeta": true}),
		assistantRow("a2", "2026-10-01T10:03:05Z", "m2", nil, M{"type": "text", "text": "Looks good."}),
	}
}

func TestEventsQueuedPromptsAndInjectedRows(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "s.jsonl"), eventsRows()...)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if len(s.Turns) != 2 {
		for _, turn := range s.Turns {
			t.Logf("turn %d ask %q: %s", turn.Index, turn.Ask, sourcetest.Kinds(turn))
		}
		t.Fatalf("turns = %d, want the migration and the /review", len(s.Turns))
	}
	one, two := s.Turn(t, 1), s.Turn(t, 2)
	if got := sourcetest.Kinds(one); got != "prompt tool prompt event event context event context context" {
		t.Errorf("turn 1 = %s", got)
	}
	if !one.Items[2].Queued || one.Items[2].Text != "also back up first" {
		t.Errorf("queued prompt = %+v", one.Items[2])
	}
	if tool := one.Items[1].Tool; tool.Status != model.StatusInterrupted {
		t.Errorf("a stopped call is interrupted, got %q", tool.Status)
	}
	if !one.Interrupted || one.Items[3].Event != model.EventInterrupted || one.Items[4].Event != model.EventCompaction || one.Items[6].Event != model.EventHookError {
		t.Errorf("events = %+v", one.Items)
	}
	// A slash command that sets a skill off is what the person asked
	if two.Ask != "/review the diff" || two.Answer != "Looks good." {
		t.Errorf("turn 2: ask %q answer %q", two.Ask, two.Answer)
	}
	if got := sourcetest.Kinds(two); got != "prompt context reply" {
		t.Errorf("turn 2 = %s", got)
	}
}

// The words Claude Code gives the model in place of a call's output when the call did not
// run as asked, as 2.1.112 and 2.1.327 write them; the last two only 2.1.327 writes.
const (
	declined         = "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed."
	declinedSaying   = "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). To tell you how to proceed, the user said:\n"
	notNow           = "The user doesn't want to take this action right now. STOP what you are doing and wait for the user to tell you how to proceed."
	agentDeclined    = "Permission for this tool use was denied. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). Try a different approach or report the limitation to complete your task."
	agentDeclinedSay = "Permission for this tool use was denied. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). The user said:\n"
	ruled            = "Permission to use Bash has been denied. IMPORTANT: You *may* attempt to accomplish this action using other tools that might naturally be used to accomplish this goal…"
	classified       = "Permission for this action has been denied. Reason: the command deletes files outside the project."
	noOneToAsk       = "Permission for this tool use was denied. It requires approval, and this session has no approval surface — nobody can answer a permission prompt here — so it was denied automatically."
	skipped          = "[Tool call skipped: the turn was stopped before this call ran, by the check whose denial is on another call in this batch. Nothing refused this call and it had no effects; re-run it if still needed.]"
)

// How a call ended is read from what Claude Code wrote of it: a failed command's exit code
// from the line its result opens with, and a call the person declined or stopped as
// interrupted — by the row's toolDenialKind where it has one, by Claude Code's own words
// where it has none. A failed call is what is left.
func TestHowACallEnded(t *testing.T) {
	code := func(n int) *int { return &n }
	cases := []struct {
		name   string
		result M // the tool_result beside its type and id
		row    M // the row's own fields
		status string
		exit   *int
	}{
		{"a command that failed", M{"content": "Exit code 1\n--- FAIL: TestOrder\nFAIL", "is_error": true},
			M{"toolUseResult": "Error: Exit code 1\n--- FAIL: TestOrder\nFAIL"}, model.StatusError, code(1)},
		{"one not found", M{"content": "Exit code 127\n/bin/sh: nope: command not found", "is_error": true}, nil, model.StatusError, code(127)},
		{"a command the person stopped", M{"content": "Exit code 137\n[Request interrupted by user for tool use]\nbuilding…", "is_error": true},
			nil, model.StatusInterrupted, code(137)},
		{"one the row says they stopped", M{"content": "building…\n<error>Command was aborted before completion</error>", "is_error": true},
			M{"toolUseResult": M{"stdout": "building…", "stderr": "", "interrupted": true}}, model.StatusInterrupted, nil},
		{"a command that passed printing the CLI's line", M{"content": "Exit code 3\nis what the docs say"}, nil, model.StatusOK, nil},
		{"an error that names a code in passing", M{"content": "Exit code 1 is what the docs say", "is_error": true}, nil, model.StatusError, nil},
		{"a tool's error", M{"content": "<tool_use_error>File does not exist.</tool_use_error>", "is_error": true}, nil, model.StatusError, nil},

		{"declined at the prompt", M{"content": declined, "is_error": true}, M{"toolUseResult": "Error: " + declined}, model.StatusInterrupted, nil},
		{"declined, saying why", M{"content": declinedSaying + "use the staging database", "is_error": true}, nil, model.StatusInterrupted, nil},
		{"stopped before it started", M{"content": notNow, "is_error": true}, M{"toolUseResult": notNow}, model.StatusInterrupted, nil},
		{"a subagent's call declined", M{"content": agentDeclined, "is_error": true}, nil, model.StatusInterrupted, nil},
		{"a subagent's call declined, saying why", M{"content": agentDeclinedSay + "not that file", "is_error": true}, nil, model.StatusInterrupted, nil},
		{"refused by a rule", M{"content": ruled, "is_error": true}, nil, model.StatusError, nil},
		{"refused with no one to ask", M{"content": noOneToAsk, "is_error": true}, nil, model.StatusError, nil},
		{"the words, printed by a call that passed", M{"content": declined}, nil, model.StatusOK, nil},

		{"declined, by the row", M{"content": declined, "is_error": true}, M{"toolDenialKind": "user-rejected"}, model.StatusInterrupted, nil},
		{"skipped when the turn stopped, by the row", M{"content": skipped, "is_error": true}, M{"toolDenialKind": "cancelled"}, model.StatusInterrupted, nil},
		{"stopped while it ran, by the row", M{"content": "Exit code 137\nbuilding…", "is_error": true}, M{"toolDenialKind": "interrupted"}, model.StatusInterrupted, code(137)},
		{"one that timed out", M{"content": "Exit code 143\nCommand timed out after 2m 0s", "is_error": true}, nil, model.StatusError, code(143)},
		{"refused by a rule in a decline's words, by the row", M{"content": agentDeclined, "is_error": true}, M{"toolDenialKind": "permission-rule"}, model.StatusError, nil},
		{"refused by auto mode, by the row", M{"content": classified, "is_error": true}, M{"toolDenialKind": "automode-blocked"}, model.StatusError, nil},
		{"a kind never heard of, read by its words", M{"content": declined, "is_error": true}, M{"toolDenialKind": "hologram"}, model.StatusInterrupted, nil},
		{"a kind never heard of, on the interruption notice", M{"content": "[Request interrupted by user for tool use]", "is_error": true}, M{"toolDenialKind": "teleport"}, model.StatusInterrupted, nil},
	}

	calls := make([]M, len(cases))
	rows := []any{userRow("u0", "2026-10-01T10:00:00Z", "clean up and build")}
	for i := range cases {
		calls[i] = M{"type": "tool_use", "id": fmt.Sprintf("t%d", i), "name": "Bash", "input": M{"command": fmt.Sprintf("step %d", i)}}
	}
	rows = append(rows, assistantRow("a1", "2026-10-01T10:00:01Z", "m1", nil, calls...))
	failed := 0
	for i, c := range cases {
		result := M{"type": "tool_result", "tool_use_id": fmt.Sprintf("t%d", i)}
		for k, v := range c.result {
			result[k] = v
		}
		rows = append(rows, userRow(fmt.Sprintf("r%d", i), "2026-10-01T10:00:02Z", []M{result}, c.row))
		if c.status != model.StatusOK {
			failed++
		}
	}
	rows = append(rows, assistantRow("a2", "2026-10-01T10:00:03Z", "m2", nil, M{"type": "text", "text": "Stopped, as asked."}))
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "proj", "sess-1.jsonl"), rows...)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)

	turn := s.Turn(t, 1)
	tools := sourcetest.Tools(turn)
	if len(tools) != len(cases) {
		t.Fatalf("%d tools, want %d", len(tools), len(cases))
	}
	for i, c := range cases {
		tool := tools[i]
		if tool.Status != c.status {
			t.Errorf("%s: status %q, want %q", c.name, tool.Status, c.status)
		}
		switch {
		case c.exit == nil && tool.Exit != nil:
			t.Errorf("%s: exit %d, want none", c.name, *tool.Exit)
		case c.exit != nil && (tool.Exit == nil || *tool.Exit != *c.exit):
			t.Errorf("%s: exit %v, want %d", c.name, tool.Exit, *c.exit)
		}
	}
	// Stopped or failed, a call that did not do what was asked counts among the turn's
	if turn.Failures != failed {
		t.Errorf("failures = %d, want %d", turn.Failures, failed)
	}
	if len(s.Unread) != 2 || s.Unread["toolDenialKind:hologram"] != 1 || s.Unread["toolDenialKind:teleport"] != 1 {
		t.Errorf("unread = %v, want the two kinds never heard of, the one on the interruption notice too", s.Unread)
	}
}

func TestSavedOutputIsReadInPlaceOfItsStub(t *testing.T) {
	root := t.TempDir()
	saved := filepath.Join(root, "proj", "sess-1", "tool-results", "toolu_9.txt")
	sourcetest.Write(t, saved, "line 1", "the last line says why it failed")
	stub := "<persisted-output>\nOutput too large (30.5KB). Full output saved to: /another/machine/.claude/projects/proj/sess-1/tool-results/toolu_9.txt\n\nPreview (first 2KB):\nline 1\n</persisted-output>"
	sourcetest.Write(t, filepath.Join(root, "proj", "sess-1.jsonl"),
		userRow("u1", "2026-10-01T10:00:00Z", "build it"),
		assistantRow("a1", "2026-10-01T10:00:01Z", "m1", nil,
			M{"type": "tool_use", "id": "t1", "name": "Bash", "input": M{"command": "make"}},
			M{"type": "tool_use", "id": "t2", "name": "Bash", "input": M{"command": "cat notes"}}),
		userRow("u2", "2026-10-01T10:00:02Z", []M{{"type": "tool_result", "tool_use_id": "t1", "content": stub}}),
		// an output that merely mentions a saved file is not a stub, and a path outside the
		// source's own root is never followed
		userRow("u3", "2026-10-01T10:00:03Z", []M{{"type": "tool_result", "tool_use_id": "t2", "content": "see: Full output saved to: /etc/passwd"}}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	tools := sourcetest.Tools(s.Turn(t, 1))
	if !strings.Contains(tools[0].Output, "the last line says why it failed") || strings.Contains(tools[0].Output, "persisted-output") {
		t.Errorf("output = %q, want the saved file", tools[0].Output)
	}
	if tools[1].Output != "see: Full output saved to: /etc/passwd" {
		t.Errorf("output = %q", tools[1].Output)
	}
}

func TestSavedOutputCap(t *testing.T) {
	defer func(l, h int64) { savedLimit, savedHead = l, h }(savedLimit, savedHead)
	savedLimit, savedHead = 64, 16
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 100)+strings.Repeat("z", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := readSaved(path)
	if !ok || !strings.HasPrefix(got, strings.Repeat("a", 16)) || !strings.HasSuffix(got, strings.Repeat("z", 48)) || !strings.Contains(got, "136 bytes of the middle left out") {
		t.Errorf("got %q", got)
	}
}

func TestSubagents(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "proj")
	sourcetest.Write(t, filepath.Join(base, "sess-1.jsonl"),
		userRow("u1", "2026-10-01T10:00:00Z", "survey the code"),
		assistantRow("a1", "2026-10-01T10:00:01Z", "m1", nil, M{"type": "tool_use", "id": "toolu_agent", "name": "Agent", "input": M{"prompt": "look around"}}),
		userRow("u2", "2026-10-01T10:05:00Z", []M{{"type": "tool_result", "tool_use_id": "toolu_agent", "content": "found things"}},
			M{"toolUseResult": M{"runId": "run-7"}}),
	)
	agent := func(id string) M {
		return M{"type": "user", "isSidechain": true, "agentId": id, "sessionId": "sess-1", "uuid": id + "-u", "cwd": "/work/proj",
			"timestamp": "2026-10-01T10:00:02Z", "message": M{"role": "user", "content": "look around the repository and report"}}
	}
	sourcetest.Write(t, filepath.Join(base, "sess-1", "subagents", "agent-aaa.jsonl"), agent("aaa"),
		M{"type": "assistant", "isSidechain": true, "agentId": "aaa", "sessionId": "sess-1", "uuid": "aaa-a", "timestamp": "2026-10-01T10:00:09Z",
			"message": M{"id": "r", "role": "assistant", "model": "claude-haiku", "content": []M{{"type": "text", "text": "report"}}}})
	sourcetest.Write(t, filepath.Join(base, "sess-1", "subagents", "agent-aaa.meta.json"),
		M{"agentType": "Explore", "description": "Survey the repository", "toolUseId": "toolu_agent"})
	sourcetest.Write(t, filepath.Join(base, "sess-1", "subagents", "workflows", "run-7", "agent-bbb.jsonl"), agent("bbb"))
	sourcetest.Write(t, filepath.Join(base, "sess-1", "subagents", "workflows", "run-7", "agent-bbb.meta.json"),
		M{"agentType": "general-purpose", "parentAgentId": "aaa"})

	all := sourcetest.All(t, Spec.Open(root))
	sourcetest.NothingUnread(t, all...)
	if len(all) != 3 {
		t.Fatalf("sessions = %d", len(all))
	}
	a, b, main := sourcetest.ByID(t, all, "aaa"), sourcetest.ByID(t, all, "bbb"), sourcetest.ByID(t, all, "sess-1")
	if a.Session.Parent == nil || a.Session.Parent.Session != "sess-1" || a.Session.Parent.Call != "toolu_agent" || a.Session.Parent.Type != "Explore" {
		t.Errorf("agent aaa parent = %+v", a.Session.Parent)
	}
	if a.Session.Title != "Survey the repository" || a.Stats.Turns != 1 || a.Turn(t, 1).Answer != "report" {
		t.Errorf("agent aaa = %+v / %+v", a.Session, a.Stats)
	}
	if b.Session.Parent == nil || b.Session.Parent.Run != "run-7" || b.Session.Parent.Agent != "aaa" {
		t.Errorf("agent bbb parent = %+v", b.Session.Parent)
	}
	if main.Session.Parent != nil || sourcetest.Tools(main.Turn(t, 1))[0].Run != "run-7" {
		t.Errorf("the session's Agent call should name the run it started: %+v", sourcetest.Tools(main.Turn(t, 1))[0])
	}
}

// forkRows is a fork's transcript: a copy of its parent's conversation up to the call
// that started it, then the row that hands it its directive, then its own work.
func forkRows() []any {
	side := func(row M) M { row["isSidechain"] = true; row["agentId"] = "fff"; return row }
	return []any{
		side(userRow("p1", "2026-10-01T09:00:00Z", "the parent's question")),
		side(assistantRow("p2", "2026-10-01T09:00:05Z", "pm", M{"input_tokens": 1000, "output_tokens": 1000},
			M{"type": "tool_use", "id": "toolu_fork", "name": "Agent", "input": M{}})),
		side(userRow("f1", "2026-10-01T09:00:06Z", []M{
			{"type": "tool_result", "tool_use_id": "toolu_fork", "content": "forked"},
			{"type": "text", "text": "<fork-boilerplate>You are a fork.</fork-boilerplate>\n\nCheck the tests"}})),
		side(assistantRow("f2", "2026-10-01T09:00:09Z", "fm", M{"input_tokens": 5, "output_tokens": 7}, M{"type": "text", "text": "They pass."})),
	}
}

func TestForkInheritsItsParentsRows(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proj", "sess-1", "subagents")
	sourcetest.Write(t, filepath.Join(dir, "agent-fff.jsonl"), forkRows()...)
	sourcetest.Write(t, filepath.Join(dir, "agent-fff.meta.json"), M{"agentType": "fork", "toolUseId": "toolu_fork"})
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if !s.Session.Parent.Fork {
		t.Error("the parent link should say it is a fork")
	}
	if s.Stats.Turns != 1 || s.Turn(t, 1).Ask != "Check the tests" {
		t.Fatalf("turns = %+v", s.Turns)
	}
	if s.Stats.Usage != (model.Usage{Input: 5, Output: 7}) {
		t.Errorf("usage = %+v: the copied rows are the parent's", s.Stats.Usage)
	}
	for _, it := range s.Turn(t, 0).Items {
		if !it.Inherited {
			t.Errorf("item %d of the copy is not inherited", it.Seq)
		}
	}
}

func TestLocate(t *testing.T) {
	if got := Spec.Locate(sourcetest.Env("/home/u")); got != filepath.Join("/home/u", ".claude", "projects") {
		t.Errorf("default = %s", got)
	}
	if got := Spec.Locate(sourcetest.Env("/home/u", "CLAUDE_CONFIG_DIR", "/cfg")); got != filepath.Join("/cfg", "projects") {
		t.Errorf("relocated = %s", got)
	}
}

// passedOverRows is a turn among the records Claude Code writes beside the conversation,
// each of a kind the adapter passes over knowing what it is.
func passedOverRows() []any {
	return []any{
		M{"type": "mode", "mode": "normal", "sessionId": "sess-1"},
		M{"type": "permission-mode", "permissionMode": "default", "sessionId": "sess-1"},
		M{"type": "session-owner", "sessionId": "sess-1"},
		M{"type": "file-history-snapshot", "messageId": "u1", "snapshot": M{}, "isSnapshotUpdate": false},
		M{"type": "attachment", "attachment": M{"type": "skill_listing", "content": "- review: reviews a diff"}},
		M{"type": "attachment", "attachment": M{"type": "deferred_tools_delta", "addedNames": []string{"WebFetch"}}},
		M{"type": "attachment", "attachment": M{"type": "queued_command", "commandMode": "task-notification", "prompt": "task done"}},
		M{"type": "attachment", "attachment": M{"type": "queued_command", "commandMode": "prompt", "isMeta": true, "prompt": "a peer's note"}},
		userRow("u1", "2026-10-01T10:00:00Z", []M{{"type": "text", "text": "what is in this picture"}, {"type": "image", "source": M{"type": "base64"}}}),
		M{"type": "api-request", "id": "req", "sessionId": "sess-1"},
		M{"type": "system", "subtype": "api_error", "uuid": "e1", "timestamp": "2026-10-01T10:00:02Z", "retryAttempt": 1},
		assistantRow("a1", "2026-10-01T10:00:04Z", "m1", M{"input_tokens": 3, "output_tokens": 4},
			M{"type": "redacted_thinking", "data": "c2VjcmV0"},
			M{"type": "tool_use", "id": "t1", "name": "ToolSearch", "input": M{"query": "select:Read"}}),
		userRow("u2", "2026-10-01T10:00:05Z", []M{{"type": "tool_result", "tool_use_id": "t1",
			"content": []M{{"type": "tool_reference", "tool_name": "Read"}, {"type": "text", "text": "loaded"}}}}),
		M{"type": "attachment", "attachment": M{"type": "total_tokens_reminder", "text": "tokens left"}},
		assistantRow("a2", "2026-10-01T10:00:06Z", "m2", nil, M{"type": "fallback", "from": M{"model": "claude-opus"}, "to": M{"model": "claude-sonnet"}}),
		assistantRow("a3", "2026-10-01T10:00:08Z", "m3", M{"input_tokens": 2, "output_tokens": 5}, M{"type": "text", "text": "A cat."}),
		M{"type": "system", "subtype": "turn_duration", "uuid": "d1", "timestamp": "2026-10-01T10:00:09Z", "durationMs": 9000},
		M{"type": "system", "subtype": "away_summary", "uuid": "s1", "timestamp": "2026-10-01T11:00:00Z", "content": "You asked about a picture"},
		M{"type": "last-prompt", "lastPrompt": "what is in this picture", "sessionId": "sess-1"},
		M{"type": "pr-link", "prNumber": 7, "sessionId": "sess-1"},
		M{"type": "tools-used", "tools": []string{"ToolSearch"}, "sessionId": "sess-1"},
		M{"type": "progress", "data": M{"type": "hook_progress"}},
		M{"type": "custom-title", "customTitle": "A picture"},
	}
}

func TestKindsPassedOverOnPurpose(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "proj", "sess-1.jsonl"), passedOverRows()...)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if len(s.Turns) != 1 || s.Session.Title != "A picture" {
		t.Fatalf("turns = %d, title %q", len(s.Turns), s.Session.Title)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt tool reply" {
		t.Errorf("items = %s: nothing passed over is an item", got)
	}
	if turn.Ask != "what is in this picture" || turn.Answer != "A cat." || sourcetest.Tools(turn)[0].Output != "loaded" {
		t.Errorf("ask %q answer %q tool %+v", turn.Ask, turn.Answer, sourcetest.Tools(turn)[0])
	}
}

// A record of a kind the adapter has never heard of is counted under what it is, and the
// session reads as it would without it.
func TestUnknownKindsAreCounted(t *testing.T) {
	session := func(unknown bool) sourcetest.Session {
		rows := []any{
			userRow("u1", "2026-10-01T10:00:00Z", "fix the flaky test"),
			assistantRow("a1", "2026-10-01T10:00:05Z", "msg_1", M{"input_tokens": 10, "output_tokens": 3},
				M{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": M{"command": "go test ./..."}}),
			userRow("u2", "2026-10-01T10:00:09Z", []M{{"type": "tool_result", "tool_use_id": "toolu_1", "content": []M{{"type": "text", "text": "FAIL"}}, "is_error": true}}),
			assistantRow("a2", "2026-10-01T10:00:20Z", "msg_2", M{"input_tokens": 1, "output_tokens": 9},
				M{"type": "text", "text": "Fixed."}),
		}
		if unknown {
			rows = []any{
				M{"type": "hologram", "sessionId": "sess-1"},
				M{"type": "attachment", "attachment": M{"type": "hologram"}},
				M{"type": "attachment", "uuid": "q", "timestamp": "2026-10-01T10:00:01Z",
					"attachment": M{"type": "queued_command", "commandMode": "hologram", "prompt": "beam me up"}},
				userRow("u1", "2026-10-01T10:00:00Z", []M{{"type": "text", "text": "fix the flaky test"}, {"type": "hologram_note"}}),
				assistantRow("a1", "2026-10-01T10:00:05Z", "msg_1", M{"input_tokens": 10, "output_tokens": 3},
					M{"type": "hologram_call", "name": "Bash"},
					M{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": M{"command": "go test ./..."}}),
				M{"type": "system", "subtype": "hologram", "uuid": "h", "timestamp": "2026-10-01T10:00:06Z", "content": "a hologram"},
				userRow("u2", "2026-10-01T10:00:09Z", []M{{"type": "tool_result", "tool_use_id": "toolu_1",
					"content": []M{{"type": "hologram_frame"}, {"type": "text", "text": "FAIL"}}, "is_error": true}}),
				assistantRow("a2", "2026-10-01T10:00:20Z", "msg_2", M{"input_tokens": 1, "output_tokens": 9},
					M{"type": "text", "text": "Fixed."}),
				M{"type": "hologram", "sessionId": "sess-1"},
			}
		}
		root := t.TempDir()
		sourcetest.Write(t, filepath.Join(root, "proj", "sess-1.jsonl"), rows...)
		return sourcetest.One(t, Spec.Open(root))
	}
	plain, odd := session(false), session(true)
	sourcetest.NothingUnread(t, plain)

	want := map[string]int{
		"row:hologram":            2,
		"attachment:hologram":     1,
		"queued_command:hologram": 1,
		"system:hologram":         1,
		"block:hologram_note":     1,
		"block:hologram_call":     1,
		"block:hologram_frame":    1,
	}
	if len(odd.Unread) != len(want) {
		t.Errorf("unread = %v, want %v", odd.Unread, want)
	}
	for kind, n := range want {
		if odd.Unread[kind] != n {
			t.Errorf("unread[%s] = %d, want %d (all: %v)", kind, odd.Unread[kind], n, odd.Unread)
		}
	}
	if odd.Stats != plain.Stats || len(odd.Turns) != len(plain.Turns) {
		t.Fatalf("stats = %+v, want %+v as without the unknown records", odd.Stats, plain.Stats)
	}
	for i := range plain.Turns {
		a, b := plain.Turns[i], odd.Turns[i]
		if sourcetest.Kinds(a) != sourcetest.Kinds(b) || a.Ask != b.Ask || a.Answer != b.Answer {
			t.Errorf("turn %d = %s %q %q, want %s %q %q", a.Index, sourcetest.Kinds(b), b.Ask, b.Answer, sourcetest.Kinds(a), a.Ask, a.Answer)
		}
		for j, tool := range sourcetest.Tools(a) {
			if got := sourcetest.Tools(b)[j]; got.Output != tool.Output || got.Status != tool.Status {
				t.Errorf("tool %d = %q %q, want %q %q", j, got.Output, got.Status, tool.Output, tool.Status)
			}
		}
	}
}

// A row can say anything of its counts. One below zero, or past what any response could
// count (model.MaxTokens), is no count and counts nothing, not the most a count can be:
// added up, the first takes from the session's total and the second swamps it. A duration
// no clock could measure is none either, and nor is one longer than any call runs: past
// 292 years a duration in milliseconds overflows a time.Duration, and wraps around
// wherever a call's duration is shown.
func TestCountsHeldToWhatACountCanBe(t *testing.T) {
	durations := []float64{1e300, 1e15, 1e13, 2e10, 86_400_000}
	calls := make([]M, len(durations))
	rows := []any{userRow("u1", "2026-10-01T10:00:00Z", "count")}
	for i := range durations {
		calls[i] = M{"type": "tool_use", "id": fmt.Sprintf("t%d", i), "name": "Bash", "input": M{"command": "true"}}
	}
	rows = append(rows, M{"type": "assistant", "uuid": "a1", "timestamp": "2026-10-01T10:00:01Z", "message": M{"id": "m1", "role": "assistant", "costUSD": -2,
		"content": calls,
		"usage":   M{"input_tokens": -5, "output_tokens": 3, "cache_read_input_tokens": int64(math.MaxInt64), "cache_creation_input_tokens": model.MaxTokens + 1}}})
	for i, ms := range durations {
		rows = append(rows, userRow(fmt.Sprintf("r%d", i), "2026-10-01T10:00:02Z", []M{{"type": "tool_result", "tool_use_id": fmt.Sprintf("t%d", i), "content": "ok"}},
			M{"toolUseResult": M{"durationMs": ms}}))
	}
	rows = append(rows, M{"type": "assistant", "uuid": "a2", "timestamp": "2026-10-01T10:00:03Z", "message": M{"id": "m2", "role": "assistant",
		"content": []M{{"type": "text", "text": "done"}},
		"usage":   M{"input_tokens": 4, "cache_read_input_tokens": int64(math.MaxInt64), "cache_creation_input_tokens": model.MaxTokens}}})
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "proj", "sess-1.jsonl"), rows...)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)
	sourcetest.NothingUnread(t, s)
	if want := (model.Usage{Input: 4, Output: 3, CacheWrite: model.MaxTokens}); s.Stats.Usage != want || s.Stats.CostUSD != 0 {
		t.Errorf("usage = %+v cost %v, want %+v and no cost", s.Stats.Usage, s.Stats.CostUSD, want)
	}
	tools := sourcetest.Tools(s.Turn(t, 1))
	if len(tools) != len(durations) {
		t.Fatalf("%d tools, want %d", len(tools), len(durations))
	}
	for i, tool := range tools[:len(tools)-1] {
		if tool.DurMs != 0 {
			t.Errorf("durationMs %g read as %d ms, want none", durations[i], tool.DurMs)
		}
	}
	if tool := tools[len(tools)-1]; tool.DurMs != 86_400_000 {
		t.Errorf("a call of a day read as %d ms", tool.DurMs)
	}
}

// A figure JSON holds that no int64 can — a count of 1e19, one past an int64, or one
// written 12.0 — or a price written as a string, is no figure, and costs nothing else of
// its row: the reply and its tool call are read. A row that does not decode at all is
// said not to have been understood.
func TestAFigureDoesNotCostItsRow(t *testing.T) {
	for _, count := range []string{"9223372036854775808", "1e19", "12.0", `"7"`} {
		raw := `{"type":"assistant","uuid":"a1","timestamp":"2026-10-01T10:00:01Z","message":{"id":"m1","role":"assistant","costUSD":"0.5",` +
			`"content":[{"type":"text","text":"running make"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"make"}}],` +
			`"usage":{"input_tokens":` + count + `,"output_tokens":7}}}`
		root := t.TempDir()
		var row M
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			t.Fatal(err)
		}
		sourcetest.Write(t, filepath.Join(root, "proj", "sess-1.jsonl"),
			userRow("u1", "2026-10-01T10:00:00Z", "build it"),
			json.RawMessage(raw),
			userRow("r1", "2026-10-01T10:00:02Z", []M{{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}}),
			json.RawMessage(`{"type":"assistant","uuid":"a2","timestamp":"2026-10-01T10:00:03Z","message":"not an object"}`))
		s := sourcetest.One(t, Spec.Open(root))
		turn := s.Turn(t, 1)
		if len(sourcetest.Tools(turn)) != 1 || s.Stats.Usage.Output != 7 || s.Stats.Calls != 1 {
			t.Errorf("input_tokens %s: tools %d, usage %+v, calls %d: the row was lost", count, len(sourcetest.Tools(turn)), s.Stats.Usage, s.Stats.Calls)
		}
		if want := map[string]int64{"12.0": 12}[count]; s.Stats.Usage.Input != want {
			t.Errorf("input_tokens %s read as %d, want %d", count, s.Stats.Usage.Input, want)
		}
		if s.Unread["undecodable:row:assistant"] != 1 {
			t.Errorf("a row that did not decode: unread %v", s.Unread)
		}
	}
}

// A cost is a row's word too. One past any response's, added to another, would make the
// session's +Inf, which no JSON encoder writes: neither the session's figures nor the
// turn's could be stored. A session total past any session's is no total either, and the
// session's cost is then what its responses cost.
func TestCostsHeldToWhatACostCanBe(t *testing.T) {
	root := t.TempDir()
	reply := func(uuid, response string, cost float64) M {
		return M{"type": "assistant", "uuid": uuid, "timestamp": "2026-10-01T10:00:01Z", "message": M{"id": response, "role": "assistant", "costUSD": cost,
			"content": []M{{"type": "text", "text": "spent"}}, "usage": M{"input_tokens": 1}}}
	}
	rows := func(id string) []any {
		return []any{
			userRow("u1", "2026-10-01T10:00:00Z", "spend", M{"sessionId": id}),
			reply("a1", "m1", 1.7e308),
			reply("a2", "m2", 1.7e308),
			reply("a3", "m3", 2e9),
			reply("a4", "m4", 0.25),
		}
	}
	sourcetest.Write(t, filepath.Join(root, "proj", "sess-1.jsonl"), rows("sess-1")...)
	sourcetest.Write(t, filepath.Join(root, "proj", "sess-2.jsonl"), append(rows("sess-2"), M{"type": "cost-state", "totalCostUSD": 1.7e308})...)
	all := sourcetest.All(t, Spec.Open(root))
	if len(all) != 2 {
		t.Fatalf("%d sessions, want 2", len(all))
	}
	for _, s := range all {
		sourcetest.Invariants(t, s)
		sourcetest.NothingUnread(t, s)
		if s.Session.Recorded != nil {
			t.Errorf("%s: recorded = %+v, want none", s.Session.ID, *s.Session.Recorded)
		}
		if s.Stats.CostUSD != 0.25 || s.Turn(t, 1).CostUSD != 0.25 {
			t.Errorf("%s: cost = %v, turn's %v, want 0.25", s.Session.ID, s.Stats.CostUSD, s.Turn(t, 1).CostUSD)
		}
		if _, err := json.Marshal(s.Stats); err != nil {
			t.Errorf("%s: stats: %v", s.Session.ID, err)
		}
		if _, err := json.Marshal(s.Turns); err != nil {
			t.Errorf("%s: turns: %v", s.Session.ID, err)
		}
	}
}
