package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj-x", "11111111-2222-3333-4444-555555555555.jsonl")
	write(t, path,
		`{"type":"user","uuid":"u1","sessionId":"cccc","cwd":"/home/imwl/proj","timestamp":"2026-09-14T07:14:06.596Z","message":{"role":"user","content":"写一句问候"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-09-14T07:14:07.000Z","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"子代理"}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-09-14T07:14:09.100Z","message":{"id":"msg_1","model":"claude-x","stop_reason":"end_turn","usage":{"input_tokens":5},"content":[{"type":"thinking","thinking":"想"},{"type":"text","text":"您好"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-09-14T07:14:10.100Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"file1"}]}]}}`,
	)

	s := newClaudeSource(root)
	list := s.List()
	if len(list) != 1 || list[0].str("sessionId") != "cccc" || list[0].str("cwd") != "/home/imwl/proj" {
		t.Fatalf("list = %v", list)
	}

	msgs := s.Messages(list[0], messageQuery{limit: 50})
	if len(msgs) != 3 { // sidechain 那条不算
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
	if final["isFinal"] != true || final["text"] != "您好" || final["thinking"] != "想" {
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

// TestClaudeHeadMetadataBeyondFiveLines：cwd 不在首行，前面可能有一串
// queue-operation 之类的非对话行。原先固定只读前 5 行，落在第 6 行开外就静默丢了
// cwd —— 列表里 project 变空、项目聚合失效，还不报错。
func TestClaudeHeadMetadataBeyondFiveLines(t *testing.T) {
	root := t.TempDir()
	lines := []string{}
	// 前 8 行都是没有 cwd 的前导行
	for i := 0; i < 8; i++ {
		lines = append(lines, `{"type":"queue-operation","operation":"enqueue","sessionId":"late-sid"}`)
	}
	lines = append(lines,
		`{"type":"user","uuid":"u1","sessionId":"late-sid","cwd":"/deep/in/the/file","timestamp":"t","message":{"role":"user","content":"hi"}}`)
	write(t, filepath.Join(root, "proj", "aaaa-bbbb.jsonl"), lines...)

	list := newClaudeSource(root).List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	if got := list[0].str("cwd"); got != "/deep/in/the/file" {
		t.Fatalf("第 9 行的 cwd 没读到: %q", got)
	}
	if got := list[0].str("sessionId"); got != "late-sid" {
		t.Fatalf("sessionId = %q", got)
	}
	// project 是按 cwd 推的，丢了 cwd 就会落进 ungrouped
	if got := toStr(list[0].public()["project"]); got != "/deep/in/the/file" {
		t.Fatalf("project = %q", got)
	}
}

// TestClaudeHeadStopsEarly：拿齐 sessionId + cwd 就该停，不要把整个文件读完
func TestClaudeHeadStopsEarly(t *testing.T) {
	root := t.TempDir()
	lines := []string{
		`{"type":"user","uuid":"u1","sessionId":"early","cwd":"/w","timestamp":"t","message":{"role":"user","content":"hi"}}`,
	}
	// 后面跟一条超长的行：真把整个文件读完的话，这里会明显变慢
	lines = append(lines, `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"`+strings.Repeat("x", 4_000_000)+`"}]}}`)
	write(t, filepath.Join(root, "proj", "cccc-dddd.jsonl"), lines...)

	list := newClaudeSource(root).List()
	if len(list) != 1 || list[0].str("cwd") != "/w" || list[0].str("sessionId") != "early" {
		t.Fatalf("list = %v", list)
	}
}
