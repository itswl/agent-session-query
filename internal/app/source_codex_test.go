package app

import (
	"path/filepath"
	"strings"
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

// TestCodexSubagentRolloutKeepsItsOwnID: a subagent's rollout is forked from another
// session, and its first session_meta names that session — `session_id` is the parent's
// id there, `id` is the file's own. Reading session_id first (as this did) listed every
// subagent as its parent: several rows shared one sessionId, and everything that keys
// sessions by id — the reader, /sessions/<id>, the page's own row reuse — could no longer
// tell them apart.
//
// The shapes are a real rollout's, reduced.
func TestCodexSubagentRolloutKeepsItsOwnID(t *testing.T) {
	const parent = "01a0a010-4923-7150-b331-03960a5c4acb"
	const child = "01a0a010-9b19-7ff0-9919-8ef807f17454"

	root := t.TempDir()
	write(t, filepath.Join(root, "2026", "09", "14", "rollout-2026-09-14T21-16-57-"+parent+".jsonl"),
		`{"type":"session_meta","payload":{"session_id":"`+parent+`","id":"`+parent+`",`+
			`"cwd":"/w/main","cli_version":"0.153.4"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"the parent session"}]}}`,
	)
	write(t, filepath.Join(root, "2026", "09", "14", "rollout-2026-09-14T21-17-18-"+child+".jsonl"),
		`{"type":"session_meta","payload":{"session_id":"`+parent+`","id":"`+child+`",`+
			`"forked_from_id":"`+parent+`","parent_thread_id":"`+parent+`","thread_source":"subagent",`+
			`"agent_nickname":"Carson","cwd":"/w/sub","cli_version":"0.153.4"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"review the architecture"}]}}`,
	)

	list := newCodexSource(root).List()
	if len(list) != 2 {
		t.Fatalf("list = %v", list)
	}
	byID := map[string]string{}
	for _, r := range list {
		byID[r.str("sessionId")] = filepath.Base(r.str("file"))
	}
	if file, ok := byID[child]; !ok {
		t.Fatalf("the subagent must be listed under its own id %s, got %v", child, byID)
	} else if !strings.Contains(file, child) {
		t.Fatalf("the parent's id was reported for %s", file)
	}
	if file, ok := byID[parent]; !ok || !strings.Contains(file, parent) {
		t.Fatalf("the parent session must keep its id, got %v", byID)
	}

	// cwd and cliVersion still come from the fork's own metadata row
	for _, r := range list {
		if r.str("sessionId") == child && r.str("cwd") != "/w/sub" {
			t.Fatalf("cwd = %q", r.str("cwd"))
		}
	}
}

// TestCodexForkedMetaWithoutOwnID: an older CLI that writes a fork without `id` leaves
// session_id naming the parent, so the filename is the only thing that belongs to this
// file — reporting the parent's id here would merge the two again.
func TestCodexForkedMetaWithoutOwnID(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "2026", "09", "14", "rollout-2026-09-14T21-17-18-forked.jsonl"),
		`{"type":"session_meta","payload":{"session_id":"the-parent","forked_from_id":"the-parent",`+
			`"cwd":"/w/sub","cli_version":"0.9"}}`,
	)
	r := newCodexSource(root).List()[0]
	if got := r.str("sessionId"); got != "rollout-2026-09-14T21-17-18-forked" {
		t.Fatalf("sessionId = %q; must not be the parent's", got)
	}
}
