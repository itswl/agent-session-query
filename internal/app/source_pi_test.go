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
		`{"type":"message","id":"m1","timestamp":"2026-09-13T13:05:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"message","id":"m2","timestamp":"2026-09-13T13:05:05Z","message":{"role":"assistant","stopReason":"stop","model":"gemini-3.8-flash","usage":{"input_tokens":10},"content":[{"type":"thinking","thinking":"pondering"},{"type":"text","text":"hi there"}]}}`,
	)

	s := newPiSource(root)
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}
	if list[0].SessionID != "pi-1" || list[0].Cwd != "/home/imwl/proj" {
		t.Fatalf("record = %v", list[0])
	}

	msgs := s.Messages(list[0], messageQuery{limit: 50})
	if len(msgs) != 2 || msgs[0]["role"] != "user" {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["id"] != "m1" || msgs[0]["timestamp"] != "2026-09-13T13:05:00Z" {
		t.Fatalf("msg0 = %v", msgs[0])
	}

	final := s.Final(list[0])
	if final["isFinal"] != true || final["text"] != "hi there" || final["thinking"] != "pondering" {
		t.Fatalf("final = %v", final)
	}
	if final["messageCount"] != 2 {
		t.Fatalf("messageCount = %v", final["messageCount"])
	}

	// limit takes the earliest N
	if got := s.Messages(list[0], messageQuery{limit: 1}); len(got) != 1 || got[0]["id"] != "m1" {
		t.Fatalf("limit=1 -> %v", got)
	}
	if got := s.Messages(list[0], messageQuery{limit: 0}); len(got) != 0 {
		t.Fatalf("limit=0 -> %v", got)
	}
}

// TestPiSessionLineNotFirst: when the session row is not the first line, another row's id
// must not become the session id. The original code took the first line's id
// unconditionally, and a model_change record carries its own id — measured locally, this
// really happened.
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
	if got := list[0].SessionID; got != "pi-real" {
		t.Fatalf("sessionId = %q; must not take model_change's id", got)
	}
	if got := list[0].Cwd; got != "/w/real" {
		t.Fatalf("cwd = %q", got)
	}
}

// TestPiNoSessionLine: with no session row at all (a truncated or resumed file), fall back
// to the uuid in the filename rather than reporting some event's id as the session id
func TestPiNoSessionLine(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "2026-09-04T09-05-45-921Z_01a06baa-b9c1.jsonl"),
		`{"type":"model_change","id":"e74f2cff","provider":"newapi"}`,
		`{"type":"message","id":"ff24cc88","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
	)

	list := newPiSource(root).List()
	if got := list[0].SessionID; got != "01a06baa-b9c1" {
		t.Fatalf("sessionId = %q; should fall back to the uuid in the filename", got)
	}
	if got := list[0].Cwd; got != "" {
		t.Fatalf("no session row should mean no cwd, got %q", got)
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

// TestPiToolResultPairing: a Pi tool call is a block on the assistant message and its
// result is a message of its own with role toolResult, toolCallId, toolName and isError
// riding on the message. Measured locally: 554 such pairs in the newest 40 sessions, 49
// of them failures — and the call used to come through without its name or arguments, the
// result as plain text under a role the page had no name for.
func TestPiToolResultPairing(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "2026-10-01T10-00-00_pppp.jsonl"),
		`{"type":"session","id":"pppp","cwd":"/w"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"run it"}],"timestamp":1000}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"pwd"}}],"stopReason":"toolUse"}}`,
		`{"type":"message","id":"m3","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"boom"}],"isError":true,"timestamp":1001}}`,
		`{"type":"message","id":"m4","message":{"role":"assistant","content":[{"type":"text","text":"it failed"}],"stopReason":"stop"}}`,
	)
	s := newPiSource(root)
	msgs := s.Messages(s.List()[0], messageQuery{limit: 10})
	call := msgs[1]["content"].([]map[string]any)[0]
	if call["type"] != "toolCall" || call["id"] != "c1" || call["name"] != "bash" || call["arguments"].(map[string]any)["command"] != "pwd" {
		t.Fatalf("call = %v", call)
	}
	if msgs[2]["role"] != "toolResult" {
		t.Fatalf("result role = %v", msgs[2]["role"])
	}
	result := msgs[2]["content"].([]map[string]any)
	if len(result) != 1 || result[0]["type"] != "toolResult" || result[0]["callId"] != "c1" ||
		result[0]["toolName"] != "bash" || result[0]["status"] != statusError || result[0]["content"] != "boom" {
		t.Fatalf("result = %v", result)
	}
}
