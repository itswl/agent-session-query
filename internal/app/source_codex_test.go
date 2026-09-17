package app

import (
	"path/filepath"
	"testing"
)

func TestCodexSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "2026", "09", "13", "rollout-2026-09-13T23-07-05-abc.jsonl")
	write(t, path,
		`{"type":"session_meta","payload":{"session_id":"codex-1","cwd":"/home/imwl/x","cli_version":"0.154.0"}}`,
		`{"type":"response_item","timestamp":"t0","payload":{"type":"message","role":"developer","content":[{"text":"系统"}]}}`,
		`{"type":"response_item","timestamp":"t1","payload":{"type":"message","role":"user","id":"c1","content":[{"text":"问题"}]}}`,
		`{"type":"response_item","timestamp":"t2","payload":{"type":"message","role":"assistant","id":"c2","stop_reason":"stop","content":[{"text":"回答"}]}}`,
		`{"type":"token_usage_record","payload":{"usage":{"input_tokens":7}}}`,
	)

	s := newCodexSource(root)
	list := s.List()
	if len(list) != 1 || list[0].str("sessionId") != "codex-1" || list[0].str("cliVersion") != "0.154.0" {
		t.Fatalf("list = %v", list)
	}

	msgs := s.Messages(list[0], messageQuery{limit: 50})
	if len(msgs) != 2 { // developer 那条不算
		t.Fatalf("messages = %v", msgs)
	}
	final := s.Final(list[0])
	if final["text"] != "回答" || final["isFinal"] != true {
		t.Fatalf("final = %v", final)
	}
	if usage := final["usage"].(map[string]any); usage["input_tokens"] != float64(7) {
		t.Fatalf("usage = %v", usage)
	}
}

// TestCodexMetaNotFirstLine：元数据行不在首行时也要找得到，
// 且不能把消息行误判成元数据（消息 payload 有 id 但没有 session_id / cwd）
func TestCodexMetaNotFirstLine(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "2026", "09", "13", "rollout-x.jsonl"),
		`{"type":"response_item","payload":{"type":"message","role":"user","id":"not-a-session","content":[{"text":"先出现的消息"}]}}`,
		`{"type":"session_meta","payload":{"session_id":"codex-late","cwd":"/w/late","cli_version":"9.9"}}`,
	)
	list := newCodexSource(root).List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	if got := list[0].str("sessionId"); got != "codex-late" {
		t.Fatalf("sessionId = %q —— 不能拿消息行的 id", got)
	}
	if list[0].str("cwd") != "/w/late" || list[0].str("cliVersion") != "9.9" {
		t.Fatalf("record = %v", list[0].fields)
	}
}

// TestCodexMetaWithoutTypeName：上游若改了类型名，靠字段形状也要认得出来
func TestCodexMetaWithoutTypeName(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "2026", "09", "13", "rollout-y.jsonl"),
		`{"type":"turn_context","payload":{"session_id":"codex-shape","cwd":"/w/shape"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","id":"m1","content":[{"text":"hi"}]}}`,
	)
	list := newCodexSource(root).List()
	if got := list[0].str("sessionId"); got != "codex-shape" {
		t.Fatalf("sessionId = %q", got)
	}
}
