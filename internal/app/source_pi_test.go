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
