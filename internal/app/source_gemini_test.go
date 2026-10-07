package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeminiSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "projA", "chats", "session-2026-09-13T12-50-41.jsonl")
	write(t, path,
		`{"sessionId":"g-1","startTime":"2026-09-13T12:50:41Z","lastUpdated":"2026-09-13T12:55:00Z"}`,
		`{"$set":{"messages":[{"type":"user","id":"gu1","timestamp":"t1","content":[{"text":"hey"}]}]}}`,
		`{"type":"gemini","id":"gg1","timestamp":"t2","model":"gemini-2.5","thoughts":"mulling","tokens":{"input":3},"content":"hello"}`,
		// A tool-call turn: content is the empty string, the substance is in toolCalls, and
		// thoughts is an array (its description is taken)
		`{"type":"gemini","id":"gg2","timestamp":"t3","model":"gemini-2.5","content":"","thoughts":[{"subject":"locate the file","description":"check package.json first"}],"toolCalls":[{"id":"c1","name":"read_file","args":{"file_path":"package.json"},"result":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"..."}}}]}]}`,
		// The user row carries the tool result back
		`{"type":"user","id":"gu2","timestamp":"t4","content":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"{\"name\":\"larkin\"}"}}}]}`,
	)

	s := newGeminiSource(root)
	list := s.List()
	if len(list) != 1 || list[0].SessionID != "g-1" || list[0].Project != "projA" {
		t.Fatalf("list = %v", list[0])
	}
	if list[0].UpdatedAt != "2026-09-13T12:55:00Z" { // uses the metadata time, never scans the whole file
		t.Fatalf("updatedAt = %v", list[0].UpdatedAt)
	}

	msgs := s.Messages(list[0], messageQuery{limit: 50})
	if len(msgs) != 4 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[1]["role"] != "assistant" {
		t.Fatalf("msg1 = %v", msgs[1])
	}
	// a thoughts string becomes a leading thinking block
	parts := msgs[1]["content"].([]map[string]any)
	if parts[0]["type"] != "thinking" || parts[0]["content"] != "mulling" {
		t.Fatalf("parts = %v", parts)
	}

	// Tool-call turn: thinking (from the thoughts array) + toolCall, with no result
	// (the next user row carries that)
	parts2 := msgs[2]["content"].([]map[string]any)
	if len(parts2) != 2 || parts2[0]["type"] != "thinking" || parts2[0]["content"] != "check package.json first" {
		t.Fatalf("parts2 = %v", parts2)
	}
	if parts2[1]["type"] != "toolCall" || parts2[1]["name"] != "read_file" {
		t.Fatalf("parts2 = %v", parts2)
	}
	if args := parts2[1]["arguments"].(map[string]any); args["file_path"] != "package.json" {
		t.Fatalf("toolCall args = %v", args)
	}

	// Tool result coming back: functionResponse becomes toolResult
	parts3 := msgs[3]["content"].([]map[string]any)
	if len(parts3) != 1 || parts3[0]["type"] != "toolResult" || parts3[0]["toolName"] != "read_file" {
		t.Fatalf("parts3 = %v", parts3)
	}
	if parts3[0]["content"] != `{"name":"larkin"}` {
		t.Fatalf("toolResult content = %v", parts3[0]["content"])
	}

	final := s.Final(list[0])
	if final["stopReason"] != "stop" || final["isFinal"] != true {
		t.Fatalf("final = %v", final)
	}
	if final["thinking"] != "check package.json first" {
		t.Fatalf("final thinking = %v", final["thinking"])
	}
	tcs := final["toolCalls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("final toolCalls = %v", tcs)
	}
	tc := tcs[0].(map[string]any)
	if tc["name"] != "read_file" || tc["arguments"].(map[string]any)["file_path"] != "package.json" {
		t.Fatalf("final toolCall = %v", tc)
	}
}

// TestGeminiMergesContinuationFiles: Gemini CLI continues a session in a new file with
// the same sessionId (measured locally: two files one minute apart). They are one
// conversation — one list row, messages read from both files in filename order, and a
// search that only knows the record finds the continuation file too. Before the merge,
// the second row's click did nothing (the UI keys rows by sessionId) and the second
// file's messages were unreachable.
func TestGeminiMergesContinuationFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "chats")
	write(t, filepath.Join(dir, "session-2026-09-14T03-16-80e8c90b.jsonl"),
		`{"sessionId":"sid-80e8c90b","startTime":"2026-09-14T03:16:50Z"}`,
		`{"type":"user","content":[{"text":"帮我连一下vpn"}],"timestamp":"2026-09-14T03:16:52Z"}`,
	)
	write(t, filepath.Join(dir, "session-2026-09-14T03-17-80e8c90b.jsonl"),
		`{"sessionId":"sid-80e8c90b","startTime":"2026-09-14T03:17:49Z"}`,
		`{"type":"gemini","content":"vpn 连好了","timestamp":"2026-09-14T03:18:01Z"}`,
	)
	s := newGeminiSource(root)

	records := s.List()
	if len(records) != 1 {
		t.Fatalf("two continuation files must be one session, got %d records", len(records))
	}
	rec := records[0]
	if rec.SessionID != "sid-80e8c90b" {
		t.Errorf("sessionId = %q", rec.SessionID)
	}
	if !strings.Contains(rec.File, "03-16") {
		t.Errorf("file must point at the earliest file, got %q", rec.File)
	}

	msgs := s.Messages(rec, messageQuery{limit: 100})
	if len(msgs) != 2 {
		t.Fatalf("messages from both files expected, got %d", len(msgs))
	}
	if msgs[0]["role"] != "user" || msgs[1]["role"] != "assistant" {
		t.Errorf("roles = %v / %v", msgs[0]["role"], msgs[1]["role"])
	}

	final := s.Final(rec)
	if final["text"] != "vpn 连好了" {
		t.Errorf("final text = %q (must come from the continuation file)", final["text"])
	}
	if final["messageCount"] != 2 {
		t.Errorf("messageCount = %v (must count both files)", final["messageCount"])
	}

	// The searchableSource path must reach the continuation file as well
	q := searchQuery{needle: "连好了", lowered: []byte("连好了"), perSession: 5}
	if hits := s.Search(context.Background(), rec, q); len(hits) == 0 {
		t.Fatal("search must find text that only exists in the continuation file")
	}
}

// TestGeminiInlineToolStatus: newer Gemini CLI answers a call on the toolCalls entry
// itself — a status, and the output as resultDisplay or under result — while older files
// answer on the user row that follows (and echo the response under result without the
// output, which is why a result field alone is not an answer, see TestGeminiSource). A
// file carrying both must show each result once, with the status the entry recorded.
func TestGeminiInlineToolStatus(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "projB", "chats", "session-2026-10-01T10-00-00.jsonl"),
		`{"sessionId":"g-2","startTime":"2026-10-01T10:00:00Z","lastUpdated":"2026-10-01T10:05:00Z"}`,
		`{"type":"user","id":"u1","timestamp":"t1","content":[{"text":"build it"}]}`,
		`{"type":"gemini","id":"g1","timestamp":"t2","content":"","toolCalls":[`+
			`{"id":"run-1","name":"run_shell_command","args":{"command":"npm run build"},"status":"error","resultDisplay":"npm ERR! missing script"},`+
			`{"id":"rep-1","name":"replace","args":{"file_path":"config.ts","old_string":"a","new_string":"b"},"status":"success","resultDisplay":{"fileName":"config.ts","fileDiff":"--- a\n+++ b\n-a\n+b\n"}},`+
			`{"id":"cancel-1","name":"run_shell_command","args":{"command":"sleep 9"},"status":"cancelled","result":[{"functionResponse":{"id":"cancel-1","name":"run_shell_command","response":{"error":"Command was cancelled by the user."}}}]},`+
			`{"id":"late-1","name":"read_file","args":{"file_path":"x"},"status":"success"}]}`,
		// The user row repeats run-1 (already answered: skipped) and brings late-1's
		// output (the status came earlier without output: applied here)
		`{"type":"user","id":"u2","timestamp":"t3","content":[{"functionResponse":{"id":"run-1","name":"run_shell_command","response":{"output":"npm ERR! missing script"}}},{"functionResponse":{"id":"late-1","name":"read_file","response":{"output":"the file"}}}]}`,
	)
	s := newGeminiSource(root)
	msgs := s.Messages(s.List()[0], messageQuery{limit: 10})
	parts := msgs[1]["content"].([]map[string]any)
	results := map[string]map[string]any{}
	calls := 0
	for _, p := range parts {
		switch p["type"] {
		case "toolCall":
			calls++
		case "toolResult":
			results[toStr(p["callId"])] = p
		}
	}
	if calls != 4 || len(results) != 3 {
		t.Fatalf("calls = %d, inline results = %d: %v", calls, len(results), parts)
	}
	if r := results["run-1"]; r["status"] != statusError || r["content"] != "npm ERR! missing script" || r["toolName"] != "run_shell_command" {
		t.Errorf("failed command = %v", r)
	}
	if r := results["rep-1"]; r["status"] != statusOK || !strings.Contains(toStr(r["content"]), "+b") {
		t.Errorf("replace = %v", r)
	}
	if r := results["cancel-1"]; r["status"] != statusInterrupted || r["content"] != "Command was cancelled by the user." {
		t.Errorf("cancelled = %v", r)
	}
	later := msgs[2]["content"].([]map[string]any)
	if len(later) != 1 || later[0]["callId"] != "late-1" || later[0]["status"] != statusOK || later[0]["content"] != "the file" {
		t.Fatalf("the user row must skip the answered call and carry the late one with its status: %v", later)
	}
}
