package app

import (
	"path/filepath"
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
	if len(list) != 1 || list[0].str("sessionId") != "g-1" || list[0].str("project") != "projA" {
		t.Fatalf("list = %v", list[0].fields)
	}
	if list[0].str("updatedAt") != "2026-09-13T12:55:00Z" { // uses the metadata time, never scans the whole file
		t.Fatalf("updatedAt = %v", list[0].str("updatedAt"))
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
