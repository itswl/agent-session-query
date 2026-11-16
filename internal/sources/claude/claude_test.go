package claude

import (
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
// count, is no count: added up, the first takes from the session's and the second wraps
// it around to below zero. A duration no clock could measure is none either.
func TestCountsHeldToWhatACountCanBe(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "proj", "sess-1.jsonl"),
		userRow("u1", "2026-10-01T10:00:00Z", "count"),
		M{"type": "assistant", "uuid": "a1", "timestamp": "2026-10-01T10:00:01Z", "message": M{"id": "m1", "role": "assistant", "costUSD": -2,
			"content": []M{{"type": "tool_use", "id": "t1", "name": "Bash", "input": M{"command": "true"}}},
			"usage":   M{"input_tokens": -5, "output_tokens": 3, "cache_read_input_tokens": int64(9223372036854775807)}}},
		userRow("u2", "2026-10-01T10:00:02Z", []M{{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}}, M{"toolUseResult": M{"durationMs": 1e300}}),
		M{"type": "assistant", "uuid": "a2", "timestamp": "2026-10-01T10:00:03Z", "message": M{"id": "m2", "role": "assistant",
			"content": []M{{"type": "text", "text": "done"}},
			"usage":   M{"input_tokens": 4, "cache_read_input_tokens": int64(9223372036854775807)}}},
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)
	if want := (model.Usage{Input: 4, Output: 3}); s.Stats.Usage != want || s.Stats.CostUSD != 0 {
		t.Errorf("usage = %+v cost %v, want %+v and no cost", s.Stats.Usage, s.Stats.CostUSD, want)
	}
	if tool := sourcetest.Tools(s.Turn(t, 1))[0]; tool.DurMs != 0 {
		t.Errorf("duration = %d ms, want none", tool.DurMs)
	}
}
