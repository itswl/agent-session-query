package source

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// agyText is the text of a message built here: one text block
func agyText(msg map[string]any) string {
	blocks, _ := msg["content"].([]map[string]any)
	if len(blocks) == 0 {
		return ""
	}
	return ToStr(blocks[0]["content"])
}

// newAntigravityFixture builds the layout agy writes: two brain roots under one gemini
// dir, a transcript per session, and the conversation index beside the CLI's root.
func newAntigravityFixture(t *testing.T, summaries bool) (*AntigravitySource, string) {
	t.Helper()
	root := t.TempDir()

	writeSession := func(brain, id string, lines ...string) {
		dir := filepath.Join(root, brain, "brain", id, ".system_generated", "logs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "transcript_full.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// One conversation the index knows — with a rewritten step, which the CLI does while
	// the turn is still finishing — and one it does not
	writeSession("antigravity-cli", "sess-1",
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-12T10:00:00Z","content":"<USER_REQUEST>fix the build</USER_REQUEST><ADDITIONAL_METADATA>noise</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:01Z","content":"on it","input_tokens":80,"cache_read_tokens":20,"output_tokens":12}`,
		`{"step_index":2,"source":"MODEL","type":"RUN_COMMAND","status":"DONE","created_at":"2026-10-12T10:00:02Z","content":""}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:03Z","content":"done now","input_tokens":100,"cache_read_tokens":20,"output_tokens":12}`,
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:04Z","content":"all set","input_tokens":5,"output_tokens":1}`,
	)
	writeSession("antigravity", "sess-2",
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-10-13T09:00:00Z","content":"explain the test harness"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-13T09:00:05Z","content":"it runs fixtures"}`,
	)

	if summaries {
		db, err := sql.Open("sqlite", sqliteURI(filepath.Join(root, "antigravity-cli", "conversation_summaries.db")))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`CREATE TABLE conversation_summaries(
			conversation_id TEXT, title TEXT, workspace_uris TEXT, preview TEXT, last_modified_time TEXT)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?,?,?,?,?)`,
			"sess-1", "Build fix", `["file:///work/repo"]`, "preview", "2026-10-12T10:00:09Z"); err != nil {
			t.Fatal(err)
		}
	}
	return NewAntigravitySource(root), root
}

// TestAntigravityList: the index answers title, workspace and time; a session it does not
// know falls back to the transcript's own opening ask
func TestAntigravityList(t *testing.T) {
	src, _ := newAntigravityFixture(t, true)
	if !src.Exists() {
		t.Fatal("a brain root exists")
	}
	if got := src.Mode(); got != "antigravity" {
		t.Fatalf("mode = %q", got)
	}
	records := src.List()
	if len(records) != 2 {
		t.Fatalf("listed %d sessions, want 2 (CLI and IDE roots, deduplicated)", len(records))
	}
	byID := map[string]Record{}
	for _, r := range records {
		byID[r.SessionID] = r
	}
	one := byID["sess-1"]
	// workspace_uris is a file:// URI: the path it yields follows the OS
	if one.ShortKey != "Build fix" || one.Cwd != filepath.FromSlash("/work/repo") || one.UpdatedAt != "2026-10-12T10:00:09Z" {
		t.Fatalf("indexed session = %+v, want the summary's title, workspace and time", one)
	}
	if !one.HasFile || one.File == "" || one.Status != "done" {
		t.Fatalf("indexed session record = %+v", one)
	}
	two := byID["sess-2"]
	if two.ShortKey != "explain the test harness" {
		t.Fatalf("unindexed session title = %q, want the first USER_INPUT", two.ShortKey)
	}
	if two.UpdatedAt != "2026-10-13T09:00:05Z" {
		t.Fatalf("unindexed session time = %q, want the last step's", two.UpdatedAt)
	}
	if got := one.ResumeCommand(); got != "agy --conversation sess-1" {
		t.Fatalf("resume = %q", got)
	}
}

// TestAntigravityListWithoutIndex: with no conversation_summaries.db the list is built
// from the transcripts alone
func TestAntigravityListWithoutIndex(t *testing.T) {
	src, _ := newAntigravityFixture(t, false)
	records := src.List()
	if len(records) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(records))
	}
	for _, r := range records {
		if r.ShortKey == "" {
			t.Fatalf("session %s has no title", r.SessionID)
		}
	}
}

// TestAntigravityMessages: steps arrive once, in step order, latest line for a rewritten
// step; the user's prompt loses agy's own context blocks; an unrecognized step stays
// visible
func TestAntigravityMessages(t *testing.T) {
	src, _ := newAntigravityFixture(t, true)
	record := src.List()[0]
	for _, r := range src.List() {
		if r.SessionID == "sess-1" {
			record = r
		}
	}
	msgs := src.Messages(record, MessageQuery{Limit: 50})
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (steps 0..3, step 1 once)", len(msgs))
	}
	if msgs[0]["role"] != "user" || agyText(msgs[0]) != "fix the build" {
		t.Fatalf("first message = %v (%q), want the user's own words", msgs[0]["role"], agyText(msgs[0]))
	}
	if msgs[1]["role"] != "assistant" || agyText(msgs[1]) != "done now" {
		t.Fatalf("rewritten step = %q, want the latest line", agyText(msgs[1]))
	}
	event, _ := msgs[2]["content"].([]map[string]any)[0]["kind"].(string)
	if msgs[2]["role"] != "system" || event != "RUN_COMMAND" {
		t.Fatalf("unknown step = %v, want a visible event row naming RUN_COMMAND", msgs[2])
	}
	if msgs[3]["role"] != "assistant" || agyText(msgs[3]) != "all set" {
		t.Fatalf("last message = %v", msgs[3])
	}

	// The latest N keeps the end
	last := src.Messages(record, MessageQuery{Limit: 2, FromEnd: true})
	if len(last) != 2 || agyText(last[1]) != "all set" {
		t.Fatalf("latest two = %v", last)
	}
}

// TestAntigravityFinal: the last model step, the conversation count, and agy's counters
// summed over the steps that finished — with a rewritten step counted once, at its
// corrected numbers
func TestAntigravityFinal(t *testing.T) {
	src, _ := newAntigravityFixture(t, true)
	var record Record
	for _, r := range src.List() {
		if r.SessionID == "sess-1" {
			record = r
		}
	}
	final := src.Final(record)
	if final == nil || final["text"] != "all set" || final["messageCount"] != 3 {
		t.Fatalf("final = %v", final)
	}
	usage, _ := final["usage"].(map[string]any)
	if usage["inputTokens"] != int64(105) || usage["cacheReadTokens"] != int64(20) || usage["outputTokens"] != int64(13) {
		t.Fatalf("usage = %v, want 105 in / 20 cache / 13 out", usage)
	}

	// The second read answers from the final cache without touching the file
	before := src.Final(record)
	if before["text"] != final["text"] {
		t.Fatalf("cached final = %v", before)
	}
}

// TestAntigravityMissingRoots: a gemini dir without either brain is simply absent
func TestAntigravityMissingRoots(t *testing.T) {
	src := NewAntigravitySource(t.TempDir())
	if src.Exists() {
		t.Fatal("no brain roots, Exists must say so")
	}
	if got := src.List(); len(got) != 0 {
		t.Fatalf("empty roots listed %d sessions", len(got))
	}
}
