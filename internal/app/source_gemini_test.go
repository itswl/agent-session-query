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
		`{"$set":{"messages":[{"type":"user","id":"gu1","timestamp":"t1","content":[{"text":"嗨"}]}]}}`,
		`{"type":"gemini","id":"gg1","timestamp":"t2","model":"gemini-2.5","thoughts":"琢磨","tokens":{"input":3},"content":"你好"}`,
		// 工具调用轮：content 是空串，内容在 toolCalls；thoughts 是数组（取 description）
		`{"type":"gemini","id":"gg2","timestamp":"t3","model":"gemini-2.5","content":"","thoughts":[{"subject":"找文件","description":"先看 package.json"}],"toolCalls":[{"id":"c1","name":"read_file","args":{"file_path":"package.json"},"result":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"..."}}}]}]}`,
		// user 行回传工具结果
		`{"type":"user","id":"gu2","timestamp":"t4","content":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"{\"name\":\"larkin\"}"}}}]}`,
	)

	s := newGeminiSource(root)
	list := s.List()
	if len(list) != 1 || list[0].str("sessionId") != "g-1" || list[0].str("project") != "projA" {
		t.Fatalf("list = %v", list[0].fields)
	}
	if list[0].str("updatedAt") != "2026-09-13T12:55:00Z" { // 用元数据时间，不扫全文件
		t.Fatalf("updatedAt = %v", list[0].str("updatedAt"))
	}

	msgs := s.Messages(list[0], 50)
	if len(msgs) != 4 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[1]["role"] != "assistant" {
		t.Fatalf("msg1 = %v", msgs[1])
	}
	// thoughts 字符串作为 thinking 插在最前面
	parts := msgs[1]["content"].([]map[string]any)
	if parts[0]["type"] != "thinking" || parts[0]["content"] != "琢磨" {
		t.Fatalf("parts = %v", parts)
	}

	// 工具调用轮：thinking（数组 thoughts）+ toolCall；不带 result（由下一条 user 行承载）
	parts2 := msgs[2]["content"].([]map[string]any)
	if len(parts2) != 2 || parts2[0]["type"] != "thinking" || parts2[0]["content"] != "先看 package.json" {
		t.Fatalf("parts2 = %v", parts2)
	}
	if parts2[1]["type"] != "toolCall" || parts2[1]["name"] != "read_file" {
		t.Fatalf("parts2 = %v", parts2)
	}
	if args := parts2[1]["arguments"].(map[string]any); args["file_path"] != "package.json" {
		t.Fatalf("toolCall args = %v", args)
	}

	// 工具结果回传：functionResponse → toolResult
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
	if final["thinking"] != "先看 package.json" {
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
