package source

import (
	"path/filepath"
	"strings"
	"testing"
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
