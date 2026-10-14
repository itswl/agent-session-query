package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClaudeSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj-x", "11111111-2222-3333-4444-555555555555.jsonl")
	write(t, path,
		`{"type":"user","uuid":"u1","sessionId":"cccc","cwd":"/home/imwl/proj","timestamp":"2026-09-14T07:14:06.596Z","message":{"role":"user","content":"write a greeting"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-09-14T07:14:07.000Z","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"subagent"}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-09-14T07:14:09.100Z","message":{"id":"msg_1","model":"claude-x","stop_reason":"end_turn","usage":{"input_tokens":5},"content":[{"type":"thinking","thinking":"thinking"},{"type":"text","text":"hello there"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-09-14T07:14:10.100Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"file1"}]}]}}`,
	)

	s := NewClaudeSource(root)
	list := s.List()
	if len(list) != 1 || list[0].SessionID != "cccc" || list[0].Cwd != "/home/imwl/proj" {
		t.Fatalf("list = %v", list)
	}

	msgs := s.Messages(list[0], MessageQuery{Limit: 50})
	if len(msgs) != 3 { // the sidechain one does not count
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["id"] != "u1" || msgs[0]["role"] != "user" {
		t.Fatalf("msg0 = %v", msgs[0])
	}
	toolMsg := msgs[2]["content"].([]map[string]any)[0]
	if toolMsg["type"] != "toolResult" || toolMsg["content"] != "file1" {
		t.Fatalf("toolResult = %v", toolMsg)
	}

	final := s.Final(list[0])
	if final["isFinal"] != true || final["text"] != "hello there" || final["thinking"] != "thinking" {
		t.Fatalf("final = %v", final)
	}
	if final["stopReason"] != "end_turn" || final["id"] != "msg_1" {
		t.Fatalf("final = %v", final)
	}
	calls := final["toolCalls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["name"] != "Bash" {
		t.Fatalf("toolCalls = %v", calls)
	}
}

// TestClaudeHeadMetadataBeyondFiveLines: cwd is not on the first line, and a run of
// non-conversation rows such as queue-operation can precede it. The original fixed 5-line
// window silently dropped any cwd past line 6 — project went empty in the list, project
// grouping broke, and nothing reported an error.
func TestClaudeHeadMetadataBeyondFiveLines(t *testing.T) {
	root := t.TempDir()
	lines := []string{}
	// The first 8 lines are preamble rows without cwd
	for i := 0; i < 8; i++ {
		lines = append(lines, `{"type":"queue-operation","operation":"enqueue","sessionId":"late-sid"}`)
	}
	lines = append(lines,
		`{"type":"user","uuid":"u1","sessionId":"late-sid","cwd":"/deep/in/the/file","timestamp":"t","message":{"role":"user","content":"hi"}}`)
	write(t, filepath.Join(root, "proj", "aaaa-bbbb.jsonl"), lines...)

	list := NewClaudeSource(root).List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	if got := list[0].Cwd; got != "/deep/in/the/file" {
		t.Fatalf("the cwd on line 9 was not picked up: %q", got)
	}
	if got := list[0].SessionID; got != "late-sid" {
		t.Fatalf("sessionId = %q", got)
	}
	// project is derived from cwd, so losing cwd drops the session into ungrouped
	if got := ToStr(list[0].Public()["project"]); got != "/deep/in/the/file" {
		t.Fatalf("project = %q", got)
	}
}

// TestClaudeHeadStopsEarly: stop as soon as sessionId and cwd are both in hand rather
// than reading the whole file
func TestClaudeHeadStopsEarly(t *testing.T) {
	root := t.TempDir()
	lines := []string{
		`{"type":"user","uuid":"u1","sessionId":"early","cwd":"/w","timestamp":"t","message":{"role":"user","content":"hi"}}`,
	}
	// Followed by one very long line: reading the whole file would be visibly slower here
	lines = append(lines, `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"`+strings.Repeat("x", 4_000_000)+`"}]}}`)
	write(t, filepath.Join(root, "proj", "cccc-dddd.jsonl"), lines...)

	list := NewClaudeSource(root).List()
	if len(list) != 1 || list[0].Cwd != "/w" || list[0].SessionID != "early" {
		t.Fatalf("list = %v", list)
	}
}

// TestClaudeToolStatusAndEvents: Claude records how every tool call ended — is_error on
// the result, interrupted on the row's toolUseResult, and the placeholder text a stopped
// call gets — and none of it was read, so a failed Bash and a successful one looked the
// same. The system rows worth a reader's attention (a compaction, a hook that failed)
// become events; the bookkeeping rows yield nothing and are not counted.
func TestClaudeToolStatusAndEvents(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "eeee.jsonl")
	write(t, path,
		`{"type":"user","uuid":"u1","sessionId":"eeee","cwd":"/w","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"build it"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-10-01T10:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"make"}}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-10-01T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"make: command not found"}]},"toolUseResult":{"stdout":"","stderr":"make: command not found","interrupted":false}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-10-01T10:00:03Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"sleep 100"}}]}}`,
		`{"type":"user","uuid":"u3","timestamp":"2026-10-01T10:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"[Request interrupted by user for tool use]"}]},"toolUseResult":{"interrupted":true}}`,
		`{"type":"user","uuid":"u4","timestamp":"2026-10-01T10:00:05Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`,
		`{"type":"system","subtype":"compact_boundary","uuid":"s1","timestamp":"2026-10-01T10:00:06Z","content":"Conversation compacted"}`,
		`{"type":"system","subtype":"stop_hook_summary","uuid":"s2","timestamp":"2026-10-01T10:00:07Z","hookErrors":["notify.sh exited with code 127"]}`,
		`{"type":"system","subtype":"stop_hook_summary","uuid":"s3","timestamp":"2026-10-01T10:00:08Z","hookErrors":[]}`,
		`{"type":"system","subtype":"turn_duration","uuid":"s4","timestamp":"2026-10-01T10:00:09Z","durationMs":1234}`,
		`{"type":"assistant","uuid":"a3","timestamp":"2026-10-01T10:00:10Z","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"tool_use","id":"t3","name":"Edit","input":{"file_path":"/w/a.go"}},{"type":"text","text":"done"}]}}`,
		`{"type":"user","uuid":"u5","timestamp":"2026-10-01T10:00:11Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","is_error":false,"content":[{"type":"text","text":"ok"}]}]}}`,
	)
	s := NewClaudeSource(root)
	rec := s.List()[0]
	msgs := s.Messages(rec, MessageQuery{Limit: 100})
	final := s.Final(rec)
	if n, _ := claudeCountMessages(path, 0); n != len(msgs) || final["messageCount"] != len(msgs) {
		t.Fatalf("counts disagree: counter %d, messages %d, final %v", n, len(msgs), final["messageCount"])
	}
	// 10 conversation and event rows: the empty hook summary and turn_duration are not
	// messages
	if len(msgs) != 10 {
		t.Fatalf("messages = %d", len(msgs))
	}

	results := map[string]map[string]any{}
	events := []string{}
	for _, m := range msgs {
		for _, block := range m["content"].([]map[string]any) {
			switch block["type"] {
			case "toolResult":
				results[ToStr(block["callId"])] = block
			case "event":
				events = append(events, ToStr(block["kind"]))
				if m["id"] == "u4" && block["content"] != "[Request interrupted by user]" {
					t.Errorf("interruption event = %v", block)
				}
			case "text":
				if m["id"] == "u4" {
					t.Error("the interruption notice must be an event, not the user's words")
				}
			}
		}
	}
	if r := results["t1"]; r["toolName"] != "Bash" || r["status"] != StatusError || r["content"] != "make: command not found" {
		t.Errorf("failed Bash = %v", r)
	}
	if r := results["t2"]; r["status"] != StatusInterrupted || r["toolName"] != "Bash" {
		t.Errorf("interrupted Bash = %v", r)
	}
	if r := results["t3"]; r["status"] != StatusOK || r["toolName"] != "Edit" {
		t.Errorf("successful Edit = %v", r)
	}
	if strings.Join(events, ",") != "interrupted,compaction,hook_error" {
		t.Errorf("events = %v", events)
	}
	// The final result's tool calls keep their ids too
	calls := final["toolCalls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "t3" {
		t.Errorf("final toolCalls = %v", calls)
	}
}

// TestClaudeFullRead: the ordinary read cuts tool output and thinking to a preview and says
// so; a full read keeps every byte
func TestClaudeFullRead(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("line of output\n", 100)
	write(t, filepath.Join(root, "proj", "ffff.jsonl"),
		`{"type":"user","uuid":"u1","sessionId":"ffff","cwd":"/w","timestamp":"t","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"t","message":{"role":"assistant","content":[{"type":"thinking","thinking":"`+strings.Repeat("думать ", 300)+`"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"t","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"`+strings.ReplaceAll(long, "\n", `\n`)+`"}]}}`,
	)
	s := NewClaudeSource(root)
	rec := s.List()[0]

	preview := s.Messages(rec, MessageQuery{Limit: 10})
	cut := preview[2]["content"].([]map[string]any)[0]
	if cut["truncated"] != true || !strings.HasSuffix(ToStr(cut["content"]), TruncationMark) {
		t.Fatalf("the preview must be cut and say so: %v", cut)
	}
	thinking := preview[1]["content"].([]map[string]any)[0]
	if thinking["truncated"] != true {
		t.Fatalf("thinking must be cut in a preview read: %v", thinking)
	}

	whole := s.Messages(rec, MessageQuery{Limit: 10, Full: true})
	full := whole[2]["content"].([]map[string]any)[0]
	if _, has := full["truncated"]; has || full["content"] != long {
		t.Fatalf("a full read must keep the output whole: %v", full)
	}
	if _, has := whole[1]["content"].([]map[string]any)[0]["truncated"]; has {
		t.Fatal("thinking must be whole in a full read")
	}
}

// TestClaudeSearchKeepsToTheRowsTheReaderDraws: a Claude Code transcript holds more
// bookkeeping than conversation, and hits in it were most of what a search returned —
// hook output, reminders and queue rows nobody can open. Only the rows Messages draws are
// searched, plus a prompt the user typed while the agent worked, which some versions record
// only as a queued-command attachment.
func TestClaudeSearchKeepsToTheRowsTheReaderDraws(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "s1.jsonl"),
		`{"type":"user","sessionId":"s1","cwd":"/w","uuid":"u1","timestamp":"2026-10-14T01:00:00Z","message":{"role":"user","content":"deploy the gateway"}}`,
		`{"type":"attachment","uuid":"a1","timestamp":"2026-10-14T01:00:01Z","attachment":{"type":"hook_success","content":"hook saw the gateway"}}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-14T01:00:02Z","content":"what about the gateway logs"}`,
		`{"type":"assistant","uuid":"s2","isSidechain":true,"timestamp":"2026-10-14T01:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"a subagent read the gateway"}]}}`,
		`{"type":"attachment","uuid":"a2","timestamp":"2026-10-14T01:00:04Z","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"what about the gateway logs"}}`,
		`{"type":"attachment","uuid":"a3","timestamp":"2026-10-14T01:00:05Z","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"background task gateway-check finished"}}`,
		`{"type":"assistant","uuid":"r1","timestamp":"2026-10-14T01:00:06Z","message":{"role":"assistant","content":[{"type":"text","text":"the gateway is up"}]}}`,
	)
	write(t, filepath.Join(root, "proj", "s2.jsonl"),
		`{"type":"user","sessionId":"s2","cwd":"/w","uuid":"v1","timestamp":"2026-10-14T02:00:00Z","message":{"role":"user","content":"something else"}}`,
		`{"type":"attachment","uuid":"b1","timestamp":"2026-10-14T02:00:01Z","attachment":{"type":"skill_listing","content":"a skill that manages a gateway"}}`,
	)
	s := NewClaudeSource(root)
	byID := map[string]Record{}
	for _, r := range s.List() {
		byID[r.SessionID] = r
	}
	q := SearchQuery{Needle: "gateway", Lowered: []byte("gateway"), Limit: 10, PerSession: 10}

	hits := s.Search(context.Background(), byID["s1"], q)
	got := []string{}
	for _, h := range hits {
		got = append(got, ToStr(h["role"])+": "+ToStr(h["snippet"]))
	}
	want := []string{"user: deploy the gateway", "user: what about the gateway logs", "assistant: the gateway is up"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("hits = %q, want %q", got, want)
	}
	// The queued prompt is the user's, so a role=user search finds it with the ask
	q.Role = "user"
	if n := len(s.Search(context.Background(), byID["s1"], q)); n != 2 {
		t.Fatalf("role=user found %d hits, want the ask and the queued prompt", n)
	}
	// A session whose only match is in a row nobody sees has no hit at all
	q.Role = ""
	if hits := s.Search(context.Background(), byID["s2"], q); len(hits) != 0 {
		t.Fatalf("a match only in a skill listing is not a hit: %v", hits)
	}
}

// TestClaudePersistedOutput: an output too large for the transcript is saved beside it and
// the transcript keeps a stub. A full read returns the saved output, found under the
// source's own root whatever directory the stub names; an ordinary read keeps the stub,
// marked truncated however short it is. Output that merely mentions a saved file is not a
// stub, and a stub cannot reach outside the projects directory.
func TestClaudePersistedOutput(t *testing.T) {
	root := t.TempDir()
	saved := strings.Repeat("build line\n", 60) + "FAIL: TestX\n"
	results := filepath.Join(root, "proj", "s1", "tool-results")
	if err := os.MkdirAll(results, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"toolu_big.txt": saved, "mcp-srv-tool-abc.txt": `{"items":[]}`} {
		if err := os.WriteFile(filepath.Join(results, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	call := func(id, name string) string {
		return `{"type":"assistant","timestamp":"2026-10-14T08:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":{}}]}}`
	}
	result := func(id, text string) string {
		return `{"type":"user","timestamp":"2026-10-14T08:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + text + `}]}}`
	}
	write(t, filepath.Join(root, "proj", "s1.jsonl"),
		`{"type":"user","sessionId":"s1","cwd":"/w","timestamp":"2026-10-14T07:59:00Z","message":{"role":"user","content":"run the build"}}`,
		// The path is the one on the machine that wrote it; only its last parts count
		call("toolu_big", "Bash"),
		result("toolu_big", `"<persisted-output>\nOutput too large (30.5KB). Full output saved to: /home/elsewhere/.claude/projects/proj/s1/tool-results/toolu_big.txt\n\nPreview (first 2KB):\nbuild line\n</persisted-output>"`),
		call("toolu_mcp", "mcp__srv__tool"),
		result("toolu_mcp", `"Error: result (56,846 characters) exceeds maximum allowed tokens. Output has been saved to /x/proj/s1/tool-results/mcp-srv-tool-abc.txt.\nFormat: JSON"`),
		// A command whose output mentions a saved file
		call("toolu_grep", "Bash"),
		result("toolu_grep", `"notes.md: Full output saved to: /x/proj/s1/tool-results/toolu_big.txt"`),
		// A stub naming a file outside the projects directory
		call("toolu_out", "Bash"),
		result("toolu_out", `"<persisted-output>\nOutput too large (1KB). Full output saved to: /x/../../tool-results/passwd\n</persisted-output>"`),
	)

	src := NewClaudeSource(root)
	list := src.List()
	if len(list) != 1 {
		t.Fatalf("list = %d records, want 1", len(list))
	}
	byCall := func(msgs []map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, m := range msgs {
			for _, b := range m["content"].([]map[string]any) {
				if b["type"] == "toolResult" {
					out[ToStr(b["callId"])] = b
				}
			}
		}
		return out
	}

	full := byCall(src.Messages(list[0], MessageQuery{Limit: 50, Full: true}))
	if full["toolu_big"]["content"] != saved {
		t.Errorf("a full read of the command's stub = %q, want the saved output", full["toolu_big"]["content"])
	}
	if full["toolu_mcp"]["content"] != `{"items":[]}` {
		t.Errorf("a full read of the MCP stub = %q, want the saved result", full["toolu_mcp"]["content"])
	}
	if !strings.HasPrefix(ToStr(full["toolu_grep"]["content"]), "notes.md:") {
		t.Errorf("output that mentions a saved file was replaced: %q", full["toolu_grep"]["content"])
	}
	if !strings.HasPrefix(ToStr(full["toolu_out"]["content"]), "<persisted-output>") {
		t.Errorf("a stub reached outside the projects directory: %q", full["toolu_out"]["content"])
	}

	preview := byCall(src.Messages(list[0], MessageQuery{Limit: 50}))
	if !strings.HasPrefix(ToStr(preview["toolu_big"]["content"]), "<persisted-output>") || preview["toolu_big"]["truncated"] != true {
		t.Errorf("an ordinary read = %v, want the stub, truncated", preview["toolu_big"])
	}
	// Shorter than any preview cut, and still not the output
	if preview["toolu_mcp"]["truncated"] != true {
		t.Errorf("a short stub is not marked truncated: %v", preview["toolu_mcp"])
	}
	if preview["toolu_grep"]["truncated"] != nil || preview["toolu_out"]["truncated"] != nil {
		t.Errorf("results that are not stubs of a file here are marked truncated: %v / %v", preview["toolu_grep"], preview["toolu_out"])
	}
}

// TestReadPersistedOutputCap: an output over the cap comes back as its start and its end
// with a line saying how much of the middle was left out, cut on whole characters.
func TestReadPersistedOutputCap(t *testing.T) {
	defer func(limit, head int64) { persistedOutputLimit, persistedOutputHead = limit, head }(persistedOutputLimit, persistedOutputHead)
	persistedOutputLimit, persistedOutputHead = 40, 10

	path := filepath.Join(t.TempDir(), "out.txt")
	// Three-byte characters, so both cuts land inside one
	body := "start" + strings.Repeat("中", 100) + "the end"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := readPersistedOutput(path)
	if !ok {
		t.Fatal("the output was not read")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("the cut split a character: %q", got)
	}
	if !strings.HasPrefix(got, "start") || !strings.HasSuffix(got, "the end") || !strings.Contains(got, "bytes of the middle left out; the whole output is out.txt") {
		t.Fatalf("capped output = %q", got)
	}

	small := filepath.Join(t.TempDir(), "small.txt")
	if err := os.WriteFile(small, []byte("whole"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := readPersistedOutput(small); !ok || got != "whole" {
		t.Fatalf("an output under the cap = %q, %v", got, ok)
	}
}

// writeClaudeSubagents lays out a session that handed work to subagents, the way Claude Code
// keeps them: an agent the main thread spawned with a tool call, one that agent spawned in
// turn, and a workflow's agent with no call behind it
func writeClaudeSubagents(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "proj")
	row := func(kind, ts, extra, content string) string {
		return `{"type":"` + kind + `","timestamp":"2026-10-14T` + ts + `Z","sessionId":"s1","cwd":"/w","gitBranch":"main"` + extra + `,"message":{"role":"` + kind + `","content":` + content + `}}`
	}
	agentRow := func(id, kind, ts, content string) string {
		return row(kind, ts, `,"isSidechain":true,"agentId":"`+id+`"`, content)
	}
	call := func(id string) string { return `[{"type":"tool_use","id":"` + id + `","name":"Agent","input":{}}]` }
	result := func(id string) string {
		return `[{"type":"tool_result","tool_use_id":"` + id + `","content":"launched"}]`
	}
	write(t, filepath.Join(dir, "s1.jsonl"),
		row("user", "10:00:00", "", `"fan out the review"`),
		row("assistant", "10:01:00", "", call("toolu_A")),
		row("user", "10:01:30", "", result("toolu_A")),
		row("assistant", "10:30:00", "", `[{"type":"text","text":"done"}]`),
	)
	agents := filepath.Join(dir, "s1", "subagents")
	write(t, filepath.Join(agents, "agent-aaa.jsonl"),
		agentRow("aaa", "user", "10:02:00", `"Review the diff for races, and hand the cache to an agent of its own"`),
		agentRow("aaa", "assistant", "10:03:00", call("toolu_B")),
		agentRow("aaa", "user", "10:03:30", result("toolu_B")),
		agentRow("aaa", "assistant", "10:10:00", `[{"type":"text","text":"found a race in the cache"}]`),
	)
	write(t, filepath.Join(agents, "agent-aaa.meta.json"),
		`{"agentType":"general-purpose","description":"Review the diff","toolUseId":"toolu_A","spawnDepth":1}`)
	write(t, filepath.Join(agents, "agent-bbb.jsonl"),
		agentRow("bbb", "user", "10:04:00", `"Check the cache"`),
		agentRow("bbb", "assistant", "10:05:00", `[{"type":"text","text":"the cache lock is held across the write"}]`),
	)
	write(t, filepath.Join(agents, "agent-bbb.meta.json"),
		`{"agentType":"general-purpose","description":"Check the cache","toolUseId":"toolu_B","parentAgentId":"aaa","spawnDepth":2}`)
	write(t, filepath.Join(agents, "workflows", "wf_1", "agent-ccc.jsonl"),
		agentRow("ccc", "user", "10:06:00", `"Scan the logs"`),
		agentRow("ccc", "assistant", "10:07:00", `[{"type":"text","text":"nothing unusual"}]`),
	)
	write(t, filepath.Join(agents, "workflows", "wf_1", "agent-ccc.meta.json"),
		`{"agentType":"workflow-subagent","description":"Scan the logs","spawnDepth":1}`)
}

// TestClaudeSubagents: a subagent's transcript is listed as a record of its own, tied to the
// session it ran in and to the agent that spawned it; its rows, every one marked
// isSidechain, are its thread — read, counted and searched — while the session's own
// transcript still leaves sidechain rows out.
func TestClaudeSubagents(t *testing.T) {
	root := t.TempDir()
	writeClaudeSubagents(t, root)
	src := NewClaudeSource(root)

	byID := map[string]Record{}
	for _, rec := range src.List() {
		byID[rec.SessionID] = rec
	}
	if len(byID) != 4 {
		t.Fatalf("records = %v, want the session and its three agents", byID)
	}
	session, a, b, c := byID["s1"], byID["aaa"], byID["bbb"], byID["ccc"]
	if session.IsSubagent() || session.Subagents != 3 || session.ResumeCommand() == "" {
		t.Errorf("session = %+v, want a session with 3 subagents", session)
	}
	if a.ParentSessionID != "s1" || a.AgentType != "general-purpose" || a.AgentDepth != 1 ||
		a.ParentToolUseID != "toolu_A" || a.ShortKey != "Review the diff" || a.ParentAgentID != "" {
		t.Errorf("agent aaa = %+v", a)
	}
	if !strings.HasPrefix(a.CreatedAt, "2026-10-14T10:02") || a.Cwd != "/w" || a.Branch != "main" {
		t.Errorf("agent aaa started %q in %q on %q", a.CreatedAt, a.Cwd, a.Branch)
	}
	// Not a session a CLI can reopen
	if a.ResumeCommand() != "" || a.Public()["resumeCommand"] != nil {
		t.Errorf("a subagent offers a resume command: %q", a.ResumeCommand())
	}
	if b.AgentDepth != 2 || b.ParentAgentID != "aaa" || b.ParentToolUseID != "toolu_B" {
		t.Errorf("agent bbb = %+v, want depth 2 under aaa", b)
	}
	if c.AgentDepth != 1 || c.AgentType != "workflow-subagent" || c.ParentSessionID != "s1" || c.ParentToolUseID != "" {
		t.Errorf("the workflow's agent = %+v", c)
	}

	if msgs := src.Messages(a, MessageQuery{Limit: 50}); len(msgs) != 4 {
		t.Errorf("agent aaa reads %d messages, want its 4 sidechain rows", len(msgs))
	}
	if msgs := src.Messages(session, MessageQuery{Limit: 50}); len(msgs) != 4 {
		t.Errorf("the session reads %d messages, want its own 4", len(msgs))
	}
	final := src.Final(b)
	if final["messageCount"] != 2 || final["text"] != "the cache lock is held across the write" {
		t.Errorf("agent bbb's final = %v", final)
	}
	if n, _ := claudeCountMessages(b.File, 0); n != 2 {
		t.Errorf("agent bbb counts %d messages, want 2", n)
	}
	q := SearchQuery{Needle: "lock is held", Lowered: []byte("lock is held"), PerSession: 3}
	if hits := src.Search(context.Background(), b, q); len(hits) != 1 || hits[0]["role"] != "assistant" {
		t.Errorf("searching agent bbb = %v, want its reply", hits)
	}
	if hits := src.Search(context.Background(), session, q); len(hits) != 0 {
		t.Errorf("the session's own transcript matched an agent's words: %v", hits)
	}
}
