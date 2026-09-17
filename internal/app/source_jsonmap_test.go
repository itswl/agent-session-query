package app

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestJsonMapOpenClaw(t *testing.T) {
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "oc-1.jsonl")
	write(t, sessionFile,
		`{"type":"message","id":"om1","timestamp":"2026-09-13T15:00:00Z","stopReason":"stop","message":{"role":"assistant","content":[{"type":"text","text":"done"},{"type":"thinking","thinking":"reasoning"}]},"usage":{"input_tokens":11}}`,
	)
	// Build the index with json.Marshal rather than concatenating the path into a string
	// literal: on Windows sessionFile is C:\Users\..., backslashes are invalid JSON escapes,
	// and the whole sessions.json would be rejected as malformed, emptying the list.
	index, err := json.Marshal(map[string]any{
		"agent:default:hook:alert:prometheus:b5123b01": map[string]any{
			"sessionId":   "oc-1",
			"sessionFile": sessionFile,
			"updatedAt":   1789489016671,
			"status":      "done",
			"model":       "m",
			"runtimeMs":   123,
			"totalTokens": 9,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "sessions.json"), string(index))

	def := openclawDef(dir)
	def.sessionsJSON = filepath.Join(dir, "sessions.json")
	def.sessionsDir = dir
	s := newJsonMapSource(def)

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	if list[0].str("shortKey") != "hook:alert:prometheus:b5123b01" {
		t.Fatalf("shortKey = %v", list[0].str("shortKey"))
	}
	if list[0].str("updatedAt") != "2026-09-15 16:16:56" { // millisecond epoch to UTC, space separated
		t.Fatalf("updatedAt = %v", list[0].str("updatedAt"))
	}
	if list[0].get("runtimeMs") != float64(123) || list[0].get("totalTokens") != float64(9) {
		t.Fatalf("record = %v", list[0].fields)
	}
	if list[0].get("file") == nil || list[0].get("hasFile") != true {
		t.Fatalf("file = %v", list[0].fields["file"])
	}

	final := s.Final(list[0])
	if final["isFinal"] != true || final["text"] != "done" || final["thinking"] != "reasoning" {
		t.Fatalf("final = %v", final)
	}
	if final["stopReason"] != "stop" || final["messageCount"] != 1 {
		t.Fatalf("final = %v", final)
	}

	// With no file: hasFile=false, file=null, and final returns the fallback carrying error
	write(t, filepath.Join(dir, "sessions.json"),
		`{"k2":{"sessionId":"missing","updatedAt":0}}`,
	)
	list = s.List()
	if len(list) != 1 || list[0].get("file") != nil || list[0].get("hasFile") != false {
		t.Fatalf("list = %v", list[0].fields)
	}
	final = s.Final(list[0])
	if final["isFinal"] != false || final["error"] == nil {
		t.Fatalf("final = %v", final)
	}
}

func TestJsonMapHermes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "h-1.jsonl"),
		`{"role":"assistant","id":"hm1","timestamp":"2026-09-13T10:00:00Z","finish_reason":"stop","content":"all finished","reasoning":"reasoning"}`,
	)
	write(t, filepath.Join(dir, "sessions.json"),
		`{"hook:task:x":{"session_id":"h-1","updated_at":"2026-09-13T10:00:00Z","created_at":"2026-09-13T09:00:00Z","display_name":"demo","platform":"feishu","total_tokens":100,"estimated_cost_usd":0.01}}`,
	)

	def := hermesDef(dir)
	def.sessionsJSON = filepath.Join(dir, "sessions.json")
	def.sessionsDir = dir
	s := newJsonMapSource(def)

	list := s.List()
	if len(list) != 1 || list[0].str("status") != "done" || list[0].str("displayName") != "demo" {
		t.Fatalf("list = %v", list[0].fields)
	}
	msgs := s.Messages(list[0], messageQuery{limit: 50})
	if len(msgs) != 1 || msgs[0]["timestamp"] != "2026-09-13T10:00:00Z" {
		t.Fatalf("messages = %v", msgs)
	}
	parts := msgs[0]["content"].([]map[string]any)
	if parts[0]["type"] != "thinking" || parts[1]["content"] != "all finished" {
		t.Fatalf("parts = %v", parts)
	}
	final := s.Final(list[0])
	if final["text"] != "all finished" || final["thinking"] != "reasoning" || final["isFinal"] != true {
		t.Fatalf("final = %v", final)
	}
}
