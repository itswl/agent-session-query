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
		`{"type":"response_item","timestamp":"t0","payload":{"type":"message","role":"developer","content":[{"text":"system"}]}}`,
		`{"type":"response_item","timestamp":"t1","payload":{"type":"message","role":"user","id":"c1","content":[{"text":"question"}]}}`,
		`{"type":"response_item","timestamp":"t2","payload":{"type":"message","role":"assistant","id":"c2","stop_reason":"stop","content":[{"text":"answer"}]}}`,
		// Two turns of usage: the card reports the session total, not the last turn's
		`{"type":"token_usage_record","payload":{"usage":{"input_tokens":7,"output_tokens":2}}}`,
		`{"type":"token_usage_record","payload":{"usage":{"input_tokens":30,"output_tokens":5,"cached_input_tokens":100}}}`,
	)

	s := newCodexSource(root)
	list := s.List()
	if len(list) != 1 || list[0].str("sessionId") != "codex-1" || list[0].str("cliVersion") != "0.154.0" {
		t.Fatalf("list = %v", list)
	}

	msgs := s.Messages(list[0], messageQuery{limit: 50})
	if len(msgs) != 2 { // the developer one does not count
		t.Fatalf("messages = %v", msgs)
	}
	final := s.Final(list[0])
	if final["text"] != "answer" || final["isFinal"] != true {
		t.Fatalf("final = %v", final)
	}
	// Normalised to the shared shape, and summed across both records: 7+30 in, 2+5 out,
	// 100 cached read
	usage := final["usage"].(map[string]any)
	for field, want := range map[string]int64{
		"inputTokens": 37, "outputTokens": 7, "cacheReadTokens": 100,
	} {
		if got := usage[field]; got != want {
			t.Errorf("%s = %v, want %d (usage = %v)", field, got, want, usage)
		}
	}
}

// TestCodexMetaNotFirstLine: the metadata row must still be found when it is not the first
// line, and a message row must not be mistaken for it (a message payload has id but
// neither session_id nor cwd)
func TestCodexMetaNotFirstLine(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "2026", "09", "13", "rollout-x.jsonl"),
		`{"type":"response_item","payload":{"type":"message","role":"user","id":"not-a-session","content":[{"text":"a message that came first"}]}}`,
		`{"type":"session_meta","payload":{"session_id":"codex-late","cwd":"/w/late","cli_version":"9.9"}}`,
	)
	list := newCodexSource(root).List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	if got := list[0].str("sessionId"); got != "codex-late" {
		t.Fatalf("sessionId = %q; must not take a message row's id", got)
	}
	if list[0].str("cwd") != "/w/late" || list[0].str("cliVersion") != "9.9" {
		t.Fatalf("record = %v", list[0].fields)
	}
}

// TestCodexMetaSalvage: with no session_meta, salvage the fields from other rows.
// The row shapes come from a real rollout: turn_context carries only cwd,
// token_usage_record only session_id, and response_item (a message) carries id="msg_...",
// which must never become the session ID.
func TestCodexMetaSalvage(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "2026", "09", "13", "rollout-y.jsonl"),
		`{"type":"response_item","payload":{"type":"message","role":"user","id":"msg_should_not_win","content":[{"text":"hi"}]}}`,
		`{"type":"turn_context","payload":{"cwd":"/w/shape"}}`,
		`{"type":"token_usage_record","payload":{"session_id":"codex-salvaged","usage":{"input_tokens":1}}}`,
	)
	r := newCodexSource(root).List()[0]
	if got := r.str("sessionId"); got != "codex-salvaged" {
		t.Fatalf("sessionId = %q; must not take a message row's msg_ id", got)
	}
	if got := r.str("cwd"); got != "/w/shape" {
		t.Fatalf("cwd = %q", got)
	}
}

// TestCodexMessageIDNeverBecomesSessionID: when a file holds nothing but message rows,
// fall back to the filename rather than letting msg_... become the session ID
func TestCodexMessageIDNeverBecomesSessionID(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "2026", "09", "13", "rollout-only-msgs.jsonl"),
		`{"type":"response_item","payload":{"type":"message","role":"user","id":"msg_aaa","content":[{"text":"hi"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_bbb","content":[{"text":"yo"}]}}`,
	)
	r := newCodexSource(root).List()[0]
	if got := r.str("sessionId"); got != "rollout-only-msgs" {
		t.Fatalf("sessionId = %q; should fall back to the filename", got)
	}
}
