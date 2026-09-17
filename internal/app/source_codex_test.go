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

// TestCodexMetaSalvage：没有 session_meta 时，从别的行把字段凑回来。
// 行的形状取自真实 rollout：turn_context 只带 cwd，token_usage_record 只带 session_id，
// 而 response_item（消息）带的是 id="msg_…"——那个绝不能被当成会话 ID。
func TestCodexMetaSalvage(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "2026", "09", "13", "rollout-y.jsonl"),
		`{"type":"response_item","payload":{"type":"message","role":"user","id":"msg_should_not_win","content":[{"text":"hi"}]}}`,
		`{"type":"turn_context","payload":{"cwd":"/w/shape"}}`,
		`{"type":"token_usage_record","payload":{"session_id":"codex-salvaged","usage":{"input_tokens":1}}}`,
	)
	r := newCodexSource(root).List()[0]
	if got := r.str("sessionId"); got != "codex-salvaged" {
		t.Fatalf("sessionId = %q —— 不能拿消息行的 msg_ id", got)
	}
	if got := r.str("cwd"); got != "/w/shape" {
		t.Fatalf("cwd = %q", got)
	}
}

// TestCodexMessageIDNeverBecomesSessionID：整个文件只有消息行时，
// 宁可退回文件名，也不能把 msg_… 当成会话 ID
func TestCodexMessageIDNeverBecomesSessionID(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "2026", "09", "13", "rollout-only-msgs.jsonl"),
		`{"type":"response_item","payload":{"type":"message","role":"user","id":"msg_aaa","content":[{"text":"hi"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_bbb","content":[{"text":"yo"}]}}`,
	)
	r := newCodexSource(root).List()[0]
	if got := r.str("sessionId"); got != "rollout-only-msgs" {
		t.Fatalf("sessionId = %q，应当退回文件名", got)
	}
}
