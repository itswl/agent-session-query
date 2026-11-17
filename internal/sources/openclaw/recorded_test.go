package openclaw

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// schemaRecorded is the part of OpenClaw 2026.11.3's agent database this reads, its columns
// as that version declares them: a window's created_at beside the run's started_at and
// ended_at.
const schemaRecorded = `
CREATE TABLE session_windows (
	session_id TEXT NOT NULL PRIMARY KEY, session_key TEXT NOT NULL,
	created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, transcript_updated_at INTEGER,
	started_at INTEGER, ended_at INTEGER, status TEXT,
	model_provider TEXT, model TEXT, display_name TEXT);
CREATE TABLE transcript_events (
	session_id TEXT NOT NULL, seq INTEGER NOT NULL,
	event_json TEXT, created_at INTEGER NOT NULL,
	event_zstd BLOB, event_utf8_bytes INTEGER,
	PRIMARY KEY (session_id, seq));`

// recordedStore lays out testdata/<name>.jsonl as the agent database it was taken from and
// returns the state directory. The file holds the database's own rows, one per line: the
// sessions OpenClaw 2026.11.3 wrote for these tests, trimmed — the system prompt it keeps
// beside every run left out, long texts cut short — and with the paths, the provider and
// the model renamed. An event OpenClaw stored compressed is marked packed, and is stored
// compressed again.
//
// 2026.11.3.jsonl: a question run with --local (main); a conversation over two runs, then
// compacted (multi); a command that printed and one that exited 1 (shell); a file written
// and then edited (files); a command given background (bg); a run whose ask never reached
// the transcript, carried on by the Gateway's restart recovery (intr); a run interrupted
// while its command ran (intr2); the rest through a Gateway with exec approvals asked for:
// a command approved (appr-allow2) and one denied (appr-deny); a command the policy refused,
// one that timed out and one given background, whose completion woke the session (shell2);
// a Code Mode program that read a file and ran a command (code).
//
// 2026.11.3-awaiting-approval.jsonl: appr-deny as it stood while its command awaited the
// decision.
func recordedStore(t *testing.T, name string) string {
	t.Helper()
	state := t.TempDir()
	path := filepath.Join(state, "agents", "assistant", "agent", "openclaw-agent.sqlite")
	sourcetest.Write(t, filepath.Join(filepath.Dir(path), ".keep"), "")
	db, err := sql.Open("sqlite", kit.FileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaRecorded); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		var line struct {
			Table  string         `json:"table"`
			Row    map[string]any `json:"row"`
			Packed bool           `json:"packed"`
		}
		dec := json.NewDecoder(bytes.NewReader(lines.Bytes()))
		dec.UseNumber()
		if err := dec.Decode(&line); err != nil {
			t.Fatal(err)
		}
		if line.Packed {
			text := line.Row["event_json"].(string)
			line.Row["event_json"], line.Row["event_zstd"], line.Row["event_utf8_bytes"] = nil, packer.EncodeAll([]byte(text), nil), len(text)
		}
		cols := slices.Sorted(func(yield func(string) bool) {
			for col := range line.Row {
				if !yield(col) {
					return
				}
			}
		})
		args := make([]any, len(cols))
		for i, col := range cols {
			args[i] = line.Row[col]
			if n, ok := args[i].(json.Number); ok {
				args[i], _ = n.Int64()
			}
		}
		stmt := `INSERT INTO ` + line.Table + ` (` + strings.Join(cols, ", ") + `) VALUES (?` + strings.Repeat(", ?", len(cols)-1) + `)`
		if _, err := db.Exec(stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	return state
}

func recorded(t *testing.T) []sourcetest.Session {
	t.Helper()
	return sourcetest.All(t, Spec.Open(recordedStore(t, "2026.11.3")))
}

// Every row 2026.11.3 wrote is of a kind the adapter knows, and each session reads as what
// happened in it: a turn for every ask a person made, and the commands, edits and reads
// each ran, with how they ended.
func TestRecordedStoreReadsWhole(t *testing.T) {
	all := recorded(t)
	sourcetest.NothingUnread(t, all...)
	if len(all) != 11 {
		t.Fatalf("sessions = %d, want 11", len(all))
	}
	for _, s := range all {
		sourcetest.Invariants(t, s)
		if s.Session.Running {
			t.Errorf("%s reads as running: every run in the store ended", s.Session.ID)
		}
	}
	turns := map[string]int{
		"2ebd972a-487e-4b0a-8294-2e60fb2fd6ea": 1, // main
		"2a99ad88-3405-4747-a811-f90877584dc9": 2, // multi
		"95f70552-a80f-47a2-9626-2ec19da9b13f": 0, // intr: no person's ask is in it
		"952431bd-acf4-4119-846d-0b29d85badf8": 1, // shell2, the completion notice no ask
	}
	for id, want := range turns {
		if s := sourcetest.ByID(t, all, id); s.Stats.Turns != want {
			t.Errorf("%s: turns = %d, want %d", id, s.Stats.Turns, want)
		}
	}

	qa := sourcetest.ByID(t, all, "2ebd972a-487e-4b0a-8294-2e60fb2fd6ea")
	if qa.Session.Title != "What is the capital of France? Answer in one word." || qa.Session.Cwd != "/home/user/.openclaw/workspace" ||
		qa.Session.Model != "gemini-2.5-flash" || !strings.HasPrefix(qa.Turn(t, 1).Answer, "Paris") {
		t.Errorf("main = %+v, answer %q", qa.Session, qa.Turn(t, 1).Answer)
	}
	// OpenClaw's own figures for the run: input 15870, output 737 of which 706 reasoning
	if want := (model.Usage{Input: 15870, Output: 737, Reasoning: 706}); qa.Stats.Usage != want || qa.Stats.Calls != 1 {
		t.Errorf("main usage = %+v in %d calls, want %+v in 1", qa.Stats.Usage, qa.Stats.Calls, want)
	}

	shell := sourcetest.Tools(sourcetest.ByID(t, all, "70333ede-0fba-4ce9-8d2a-9b965913288c").Turn(t, 1))
	if len(shell) != 2 || shell[0].Command != "echo hello-asq" || shell[0].Status != model.StatusOK || shell[0].DurMs != 2402 ||
		shell[1].Command != "cat /nonexistent-asq-file" || shell[1].Status != model.StatusError || shell[1].Exit == nil || *shell[1].Exit != 1 || shell[1].DurMs != 2537 {
		t.Errorf("shell tools = %+v", shell)
	}

	files := sourcetest.Tools(sourcetest.ByID(t, all, "4e6f2f37-9065-4362-b891-d1e90eae14f4").Turn(t, 1))
	if len(files) != 2 || files[0].Name != "write" || files[1].Name != "edit" || files[0].Status != model.StatusOK || files[1].Status != model.StatusOK ||
		!slices.Equal(files[1].Files, []string{"notes.txt"}) {
		t.Errorf("files tools = %+v", files)
	}

	// Interrupted while its command ran: the call is there, and no result
	cut := sourcetest.Tools(sourcetest.ByID(t, all, "6f7ece01-e746-4afe-b4b4-15fce396f53e").Turn(t, 1))
	if len(cut) != 1 || cut[0].Command != "sleep 60; echo woke" || cut[0].Done || cut[0].Status != "" {
		t.Errorf("interrupted call = %+v", cut)
	}

	allowed := sourcetest.Tools(sourcetest.ByID(t, all, "6fee1fc8-0820-4b9b-857a-35802373eeb3").Turn(t, 1))
	denied := sourcetest.Tools(sourcetest.ByID(t, all, "9ab4194d-ee64-4d74-a8b7-f359c393fcc8").Turn(t, 1))
	if len(allowed) != 1 || allowed[0].Status != model.StatusOK || allowed[0].DurMs != 78 {
		t.Errorf("an approved command = %+v", allowed)
	}
	if len(denied) != 1 || denied[0].Status != model.StatusError || !strings.Contains(denied[0].Output, "user-denied") {
		t.Errorf("a denied command = %+v", denied)
	}

	code := sourcetest.Tools(sourcetest.ByID(t, all, "0007a976-40e9-4021-a709-d011417f95cf").Turn(t, 1))
	if len(code) != 3 || code[0].Name != "exec" || code[0].Status != model.StatusOK ||
		code[1].Name != "read" || code[1].DurMs != 22 || code[1].Status != model.StatusOK ||
		code[2].Command != "ls /nonexistent-asq-dir" || code[2].Exit == nil || *code[2].Exit != 2 || code[2].DurMs != 45 || code[2].Status != model.StatusError {
		t.Errorf("code mode tools = %+v", code)
	}

	// Compacted after its second run: the summary closes the turn it followed
	multi := sourcetest.ByID(t, all, "2a99ad88-3405-4747-a811-f90877584dc9")
	if items := multi.Turn(t, 2).Items; items[len(items)-1].Event != model.EventCompaction {
		t.Errorf("multi's last item = %+v", items[len(items)-1])
	}
	if want := (model.Usage{Input: 44923, Output: 350, CacheRead: 384, Reasoning: 287}); multi.Stats.Usage != want {
		t.Errorf("multi usage = %+v, want %+v", multi.Stats.Usage, want)
	}
}

// A window's started_at and ended_at are its latest run's: a session run twice began when
// its window was created, not when the second run did. A run with --local writes neither,
// and the session began when its window was created all the same — before the first row,
// which comes once the run is set up.
func TestRecordedSessionsBeginWhenCreated(t *testing.T) {
	all := recorded(t)
	created := map[string]int64{
		"952431bd-acf4-4119-846d-0b29d85badf8": 1794892792611, // started_at is the wake's run, 1794892815840
		"95f70552-a80f-47a2-9626-2ec19da9b13f": 1794891677634, // the run recovery made began at 1794892150608
		"2a99ad88-3405-4747-a811-f90877584dc9": 1794891301199, // --local: no started_at
		"2ebd972a-487e-4b0a-8294-2e60fb2fd6ea": 1794891078804,
	}
	for id, want := range created {
		s := sourcetest.ByID(t, all, id)
		if got := s.Session.StartedAt.UnixMilli(); got != want {
			t.Errorf("%s began at %d, want %d", id, got, want)
		}
		if len(s.Turns) > 1 && s.Turn(t, 1).StartedAt.Before(s.Session.StartedAt) {
			t.Errorf("%s: its first ask came before it began", id)
		}
	}
}

// What OpenClaw puts in the user's seat itself is no ask: the notice that a command given
// background finished, and the request to carry on after a restart. The notice is read as
// context in the turn whose command it reports, and the agent's silent reply to it is no
// answer: the turn's answer stays what the agent said last.
func TestRecordedNoticesAreNoAsks(t *testing.T) {
	all := recorded(t)
	s := sourcetest.ByID(t, all, "952431bd-acf4-4119-846d-0b29d85badf8")
	if s.Session.Title != "shell2" {
		t.Errorf("title = %q: the Gateway's display name for the session", s.Session.Title)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt tool reply tool reply tool reply context" {
		t.Errorf("items = %s", got)
	}
	if last := turn.Items[len(turn.Items)-1]; last.Text != "Exec completed (fresh-du, code 0) :: bg-finished" {
		t.Errorf("the notice = %+v", last)
	}
	if turn.Answer != "DONE" {
		t.Errorf("answer = %q, want what the agent said before the notice", turn.Answer)
	}
	// The response that stayed silent is still one, and its tokens count
	if want := (model.Usage{Input: 26572, Output: 424, CacheRead: 41600, Reasoning: 314}); s.Stats.Usage != want || s.Stats.Calls != 5 {
		t.Errorf("usage = %+v in %d calls, want %+v in 5", s.Stats.Usage, s.Stats.Calls, want)
	}
	tools := sourcetest.Tools(turn)
	if len(tools) != 3 || tools[0].Status != model.StatusError || tools[1].Status != model.StatusError || tools[1].DurMs != 3096 || tools[1].Exit != nil {
		t.Errorf("the refused and the timed-out command = %+v", tools)
	}
	// Given background, it answered that it runs on: no outcome yet
	if bg := tools[2]; !bg.Done || bg.Status != "" || bg.Command != "sleep 4; echo bg-finished" {
		t.Errorf("the background command = %+v", bg)
	}

	recovered := sourcetest.ByID(t, all, "95f70552-a80f-47a2-9626-2ec19da9b13f")
	before := recovered.Turn(t, 0)
	if len(before.Items) == 0 || before.Items[1].Kind != model.Context || !strings.HasPrefix(before.Items[1].Text, "[System] Your previous turn was interrupted") {
		t.Errorf("the request to carry on = %+v", before.Items)
	}
	if len(sourcetest.Tools(before)) != 4 || !strings.HasPrefix(before.Answer, "Recovered state after the restart") {
		t.Errorf("what the recovery did: %d tools, answer %q", len(sourcetest.Tools(before)), before.Answer)
	}
}

// While a command awaits approval the Gateway's run is in flight: the window has a
// started_at, no ended_at and no status, and the call has no result yet.
func TestRecordedRunAwaitingApprovalIsRunning(t *testing.T) {
	s := sourcetest.One(t, Spec.Open(recordedStore(t, "2026.11.3-awaiting-approval")))
	sourcetest.NothingUnread(t, s)
	if !s.Session.Running || s.Session.StartedAt.UnixMilli() != 1794892680490 {
		t.Errorf("session = %+v", s.Session)
	}
	if tools := sourcetest.Tools(s.Turn(t, 1)); len(tools) != 1 || tools[0].Done || tools[0].Command != "touch denied-marker.txt" {
		t.Errorf("tools = %+v", tools)
	}
}
