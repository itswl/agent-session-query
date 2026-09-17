package app

import (
	"path/filepath"
	"testing"
)

func TestPiSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "2026-09-13T13-04-12-594Z_aaaa.jsonl")
	write(t, path,
		`{"type":"session","id":"pi-1","cwd":"/home/imwl/proj"}`,
		`{"type":"message","id":"m1","timestamp":"2026-09-13T13:05:00Z","message":{"role":"user","content":[{"type":"text","text":"你好"}]}}`,
		`{"type":"message","id":"m2","timestamp":"2026-09-13T13:05:05Z","message":{"role":"assistant","stopReason":"stop","model":"gemini-3.8-flash","usage":{"input_tokens":10},"content":[{"type":"thinking","thinking":"想想"},{"type":"text","text":"你好呀"}]}}`,
	)

	s := newPiSource(root)
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}
	if list[0].str("sessionId") != "pi-1" || list[0].str("cwd") != "/home/imwl/proj" {
		t.Fatalf("record = %v", list[0].fields)
	}

	msgs := s.Messages(list[0], messageQuery{limit: 50})
	if len(msgs) != 2 || msgs[0]["role"] != "user" {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["id"] != "m1" || msgs[0]["timestamp"] != "2026-09-13T13:05:00Z" {
		t.Fatalf("msg0 = %v", msgs[0])
	}

	final := s.Final(list[0])
	if final["isFinal"] != true || final["text"] != "你好呀" || final["thinking"] != "想想" {
		t.Fatalf("final = %v", final)
	}
	if final["messageCount"] != 2 {
		t.Fatalf("messageCount = %v", final["messageCount"])
	}

	// limit 取最早的前 N 条
	if got := s.Messages(list[0], messageQuery{limit: 1}); len(got) != 1 || got[0]["id"] != "m1" {
		t.Fatalf("limit=1 -> %v", got)
	}
	if got := s.Messages(list[0], messageQuery{limit: 0}); len(got) != 0 {
		t.Fatalf("limit=0 -> %v", got)
	}
}

// TestPiSessionLineNotFirst：session 行不在首行时，不能把别的行的 id 当会话 id。
// 原先无条件取第一行的 id，而 model_change 记录自己也有 id —— 本机实测真踩到了。
func TestPiSessionLineNotFirst(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "2026-09-04T09-05-45-921Z_01a06baa-real-uuid.jsonl")
	write(t, path,
		`{"type":"model_change","id":"e74f2cff","provider":"newapi","modelId":"m"}`,
		`{"type":"model_change","id":"deadbeef","provider":"newapi","modelId":"m2"}`,
		`{"type":"session","version":3,"id":"pi-real","cwd":"/w/real"}`,
	)

	list := newPiSource(root).List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	if got := list[0].str("sessionId"); got != "pi-real" {
		t.Fatalf("sessionId = %q —— 不能拿 model_change 的 id", got)
	}
	if got := list[0].str("cwd"); got != "/w/real" {
		t.Fatalf("cwd = %q", got)
	}
}

// TestPiNoSessionLine：完全没有 session 行时（被截断 / 续写的文件），
// 退回文件名里的 uuid，而不是把某条事件的 id 当会话 id 报出去
func TestPiNoSessionLine(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "2026-09-04T09-05-45-921Z_01a06baa-b9c1.jsonl"),
		`{"type":"model_change","id":"e74f2cff","provider":"newapi"}`,
		`{"type":"message","id":"ff24cc88","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
	)

	list := newPiSource(root).List()
	if got := list[0].str("sessionId"); got != "01a06baa-b9c1" {
		t.Fatalf("sessionId = %q，应当退回文件名里的 uuid", got)
	}
	if got := list[0].str("cwd"); got != "" {
		t.Fatalf("没有 session 行就不该凭空有 cwd: %q", got)
	}
}

func TestPiSessionIDFromStem(t *testing.T) {
	cases := map[string]string{
		"2026-09-04T09-05-45-921Z_01a06baa-b9c1": "01a06baa-b9c1",
		"no-underscore-here":                     "no-underscore-here",
		"trailing_":                              "trailing_",
	}
	for in, want := range cases {
		if got := piSessionID(in); got != want {
			t.Errorf("piSessionID(%q) = %q, want %q", in, got, want)
		}
	}
}
