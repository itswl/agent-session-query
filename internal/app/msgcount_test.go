package app

import (
	"path/filepath"
	"testing"
)

// TestCountersAgreeWithFinal: the list shows a message count without the session being
// opened, and it has to be the number the session itself reports. Two counters drifting
// apart would put a second wrong number on screen. This is the invariant that was checked
// by hand against 234 real sessions; here it is against fixtures, per source.
func TestCountersAgreeWithFinal(t *testing.T) {
	dir := t.TempDir()

	// claude: sidechain rows and non-conversation rows do not count
	claudePath := filepath.Join(dir, "proj", "claude.jsonl")
	write(t, claudePath,
		`{"type":"queue-operation"}`,
		`{"type":"user","message":{"role":"user","content":"one"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"two"}}`,
		`{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":"sub"}}`,
		`{"type":"user","message":{"role":"user","content":"three"}}`,
	)
	claude := newClaudeSource(dir)
	if got, want := claudeCountMessages(claudePath), finalCount(claude.Final(recordOfPath(claude, claudePath))); got != want {
		t.Errorf("claude: counter %d, final %d", got, want)
	}

	// pi: only type=message rows
	piPath := filepath.Join(dir, "proj", "pi.jsonl")
	write(t, piPath,
		`{"type":"session","id":"p1","cwd":"/w"}`,
		`{"type":"model_change"}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"q"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"a"}]}}`,
	)
	pi := newPiSource(dir)
	if got, want := piCountMessages(piPath), finalCount(pi.Final(recordOfPath(pi, piPath))); got != want {
		t.Errorf("pi: counter %d, final %d", got, want)
	}

	// codex: developer instructions do not count
	codexPath := filepath.Join(dir, "2026", "09", "18", "rollout-x.jsonl")
	write(t, codexPath,
		`{"type":"session_meta","payload":{"session_id":"c1","cwd":"/w"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"text":"system"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"q"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"a"}]}}`,
	)
	codex := newCodexSource(dir)
	if got, want := codexCountMessages(codexPath), finalCount(codex.Final(recordOfPath(codex, codexPath))); got != want {
		t.Errorf("codex: counter %d, final %d", got, want)
	}

	// gemini: conversation rows only
	geminiPath := filepath.Join(dir, "g", "chats", "session-x.jsonl")
	write(t, geminiPath,
		`{"sessionId":"g1","startTime":"2026-09-01T00:00:00Z"}`,
		`{"type":"user","content":[{"text":"q"}]}`,
		`{"type":"gemini","content":"a"}`,
	)
	gemini := newGeminiSource(dir)
	if got, want := geminiCountMessages(geminiPath), finalCount(gemini.Final(recordOfPath(gemini, geminiPath))); got != want {
		t.Errorf("gemini: counter %d, final %d", got, want)
	}
}

// recordOfPath finds the record a source built for one file
func recordOfPath(s SessionSource, path string) record {
	for _, rec := range s.List() {
		if rec.str("file") == path {
			return rec
		}
	}
	return record{}
}

// finalCount reads a final result's messageCount whatever numeric type it carries
func finalCount(final map[string]any) int {
	if final == nil {
		return -1
	}
	n, _ := toFloat(final["messageCount"])
	return int(n)
}
