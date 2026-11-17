package codex

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

func TestSession(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "2026", "09", "13", "rollout-2026-09-13T23-07-05-abc.jsonl"),
		`{"type":"session_meta","payload":{"session_id":"codex-1","cwd":"/home/u/x","cli_version":"0.154.0","git":{"branch":"main"}}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:05Z","payload":{"type":"message","role":"developer","content":[{"text":"system"}]}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:06Z","payload":{"type":"message","role":"user","id":"c1","content":[{"text":"question"}]}}`,
		`{"type":"turn_context","payload":{"turn_id":"t1","model":"gpt-6.1"}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:08Z","payload":{"type":"reasoning","summary":[{"text":"thinking it over"}]}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:09Z","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"ls\"]}","call_id":"call_1"}}`,
		// one record per model response: it closes the rows before it
		`{"type":"token_usage_record","timestamp":"2026-09-13T23:07:09Z","payload":{"response_id":"resp_1","turn_id":"t1","usage":{"input_tokens":130,"cached_input_tokens":100,"output_tokens":5,"reasoning_output_tokens":3}}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:10Z","payload":{"type":"function_call_output","call_id":"call_1","output":"a.txt"}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:12Z","payload":{"type":"message","role":"assistant","id":"c2","content":[{"text":"answer"}]}}`,
		`{"type":"token_usage_record","timestamp":"2026-09-13T23:07:12Z","payload":{"response_id":"resp_2","turn_id":"t1","usage":{"input_tokens":7,"output_tokens":2}}}`,
		// the same record written again is the same response
		`{"type":"token_usage_record","timestamp":"2026-09-13T23:07:12Z","payload":{"response_id":"resp_2","turn_id":"t1","usage":{"input_tokens":7,"output_tokens":2}}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "codex-1" || s.Session.Version != "0.154.0" || s.Session.Cwd != "/home/u/x" || s.Session.Branch != "main" || s.Session.Model != "gpt-6.1" {
		t.Fatalf("session = %+v", s.Session)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt thinking tool reply" {
		t.Errorf("items = %s (the developer row is not conversation)", got)
	}
	if turn.Ask != "question" || turn.Answer != "answer" {
		t.Errorf("ask %q answer %q", turn.Ask, turn.Answer)
	}
	// Two responses, each its own call; the reasoning and the call are one response
	if len(turn.Calls) != 2 || turn.Calls[0].ID != "resp_1" || turn.Calls[0].Model != "gpt-6.1" {
		t.Fatalf("calls = %+v", turn.Calls)
	}
	if turn.Items[1].Call != 1 || turn.Items[2].Call != 1 || turn.Items[3].Call != 2 {
		t.Errorf("items name their calls wrongly: %+v", turn.Items)
	}
	// Codex counts the cached tokens inside the input: they are taken back out
	want := model.Usage{Input: 37, Output: 7, CacheRead: 100, Reasoning: 3}
	if s.Stats.Usage != want {
		t.Errorf("usage = %+v, want %+v", s.Stats.Usage, want)
	}
	if tool := sourcetest.Tools(turn)[0]; tool.Command != "ls" || tool.Output != "a.txt" || tool.Status != model.StatusOK {
		t.Errorf("tool = %+v", tool)
	}
}

func TestSessionID(t *testing.T) {
	const parent = "01a0a010-4923-7150-b331-03960a5c4acb"
	const child = "01a0a010-9b19-7ff0-9919-8ef807f17454"
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"meta not on the first line; a message's id is not the session's", []string{
			`{"type":"response_item","payload":{"type":"message","role":"user","id":"not-a-session","content":[{"text":"first"}]}}`,
			`{"type":"session_meta","payload":{"session_id":"codex-late","cwd":"/w/late","cli_version":"9.9"}}`,
		}, "codex-late"},
		{"no meta: salvaged from the rows that carry it", []string{
			`{"type":"response_item","payload":{"type":"message","role":"user","id":"msg_should_not_win","content":[{"text":"hi"}]}}`,
			`{"type":"turn_context","payload":{"cwd":"/w/shape"}}`,
			`{"type":"token_usage_record","payload":{"session_id":"codex-salvaged","usage":{"input_tokens":1}}}`,
		}, "codex-salvaged"},
		{"only messages: the filename", []string{
			`{"type":"response_item","payload":{"type":"message","role":"user","id":"msg_aaa","content":[{"text":"hi"}]}}`,
		}, "rollout-x"},
		{"a child's session_id names its parent; its own id is id", []string{
			`{"type":"session_meta","payload":{"session_id":"` + parent + `","id":"` + child + `","forked_from_id":"` + parent + `","cwd":"/w/sub"}}`,
		}, child},
		{"a fork with no id of its own: the filename, never the parent's id", []string{
			`{"type":"session_meta","payload":{"session_id":"the-parent","forked_from_id":"the-parent","cwd":"/w/sub"}}`,
		}, "rollout-x"},
	}
	for _, c := range cases {
		root := t.TempDir()
		lines := make([]any, len(c.lines))
		for i, l := range c.lines {
			lines[i] = l
		}
		sourcetest.Write(t, filepath.Join(root, "2026", "09", "13", "rollout-x.jsonl"), lines...)
		s := sourcetest.One(t, Spec.Open(root))
		sourcetest.NothingUnread(t, s)
		if got := s.Session.ID; got != c.want {
			t.Errorf("%s: id = %q, want %q", c.name, got, c.want)
		}
	}
}

// The shapes are the ones real rollouts write: the older shell tool's JSON output with
// exit code and duration in metadata, the newer tools' text header, apply_patch's input,
// and the event that records the user stopping a turn.
func TestToolRows(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "2026", "10", "01", "rollout-2026-10-01T10-00-00-tools.jsonl"),
		`{"timestamp":"2026-10-01T10:00:00Z","type":"session_meta","payload":{"id":"codex-tools","cwd":"/p","cli_version":"0.160.0"}}`,
		`{"timestamp":"2026-10-01T10:00:00Z","type":"turn_context","payload":{"turn_id":"t1","model":"gpt-6.1","cwd":"/p"}}`,
		`{"timestamp":"2026-10-01T10:00:00Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>sandbox</permissions instructions>"}]}}`,
		`{"timestamp":"2026-10-01T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /p\n\n<INSTRUCTIONS>\n- be brief\n</INSTRUCTIONS>"}]}}`,
		`{"timestamp":"2026-10-01T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/p</cwd>\n</environment_context>"}]}}`,
		`{"timestamp":"2026-10-01T10:00:02Z","type":"response_item","payload":{"type":"message","id":"msg_user","role":"user","content":[{"type":"input_text","text":"update the README"}]}}`,
		`{"timestamp":"2026-10-01T10:00:06Z","type":"response_item","payload":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"**Inspecting README**"}],"encrypted_content":"gAAA"}}`,
		`{"timestamp":"2026-10-01T10:00:06Z","type":"response_item","payload":{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"gAAB"}}`,
		`{"timestamp":"2026-10-01T10:00:08Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"ls\"],\"workdir\":\"/p\"}","call_id":"call_ls"}}`,
		`{"timestamp":"2026-10-01T10:00:09Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_ls","output":"{\"output\":\"file1.txt\\nfile2.txt\",\"metadata\":{\"exit_code\":0,\"duration_seconds\":0.1}}"}}`,
		`{"timestamp":"2026-10-01T10:00:10Z","type":"response_item","payload":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_read","name":"exec","input":"text((await tools.exec_command({cmd:\"sed -n '1,80p' README.md\"})).output)"}}`,
		`{"timestamp":"2026-10-01T10:00:11Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_read","output":[{"type":"input_text","text":"Script completed\nWall time 0.1 seconds\nOutput:\n"},{"type":"input_text","text":"# demo\n"}]}}`,
		`{"timestamp":"2026-10-01T10:00:15Z","type":"response_item","payload":{"type":"custom_tool_call","id":"ctc_4","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** Update File: README.md\n@@\n-old\n+new\n*** End Patch"}}`,
		`{"timestamp":"2026-10-01T10:00:16Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_patch","output":"Exit code: 0\nWall time: 0.064 seconds\nOutput:\nSuccess. Updated the following files:\nM README.md\n"}}`,
		`{"timestamp":"2026-10-01T10:00:17Z","type":"response_item","payload":{"type":"custom_tool_call","id":"ctc_5","call_id":"call_bad","name":"apply_patch","input":"*** Begin Patch\n*** Update File: notes.md\n@@\n-x\n+y\n*** End Patch"}}`,
		`{"timestamp":"2026-10-01T10:00:18Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_bad","output":"apply_patch verification failed: context not found"}}`,
		`{"timestamp":"2026-10-01T10:00:20Z","type":"response_item","payload":{"type":"function_call","id":"fc_2","name":"exec_command","arguments":"{\"cmd\":\"cargo test\",\"workdir\":\"/p\"}","call_id":"call_build"}}`,
		`{"timestamp":"2026-10-01T10:00:25Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_build","output":"{\"output\":\"test failed\\n\",\"metadata\":{\"exit_code\":101,\"duration_seconds\":3}}"}}`,
		`{"timestamp":"2026-10-01T10:00:26Z","type":"response_item","payload":{"type":"web_search_call","status":"completed","action":{"type":"search","query":"cargo test flags"}}}`,
		`{"timestamp":"2026-10-01T10:00:30Z","type":"response_item","payload":{"type":"message","id":"msg_final","role":"assistant","content":[{"type":"output_text","text":"README updated."}]}}`,
		`{"timestamp":"2026-10-01T10:00:31Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":9,"total_tokens":109},"last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":9,"total_tokens":109}}}}`,
		`{"timestamp":"2026-10-01T10:01:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run it again"}]}}`,
		`{"timestamp":"2026-10-01T10:01:05Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t2","reason":"interrupted","duration_ms":95000}}`,
		`{"timestamp":"2026-10-01T10:01:06Z","type":"compacted","payload":{"message":""}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Session.Model != "gpt-6.1" || s.Session.ID != "codex-tools" {
		t.Errorf("session = %+v", s.Session)
	}
	if len(s.Turns) != 3 {
		t.Fatalf("turns = %d, want the preamble and two asks", len(s.Turns))
	}
	// The AGENTS.md and environment rows are the CLI's, not the person's
	if got := sourcetest.Kinds(s.Turn(t, 0)); got != "context context" {
		t.Errorf("preamble = %s", got)
	}
	one := s.Turn(t, 1)
	if one.Ask != "update the README" || one.Answer != "README updated." {
		t.Errorf("ask %q answer %q", one.Ask, one.Answer)
	}
	// One thinking item: the row with only encrypted content shows nothing
	if got := sourcetest.Kinds(one); got != "prompt thinking tool tool tool tool tool tool reply" {
		t.Errorf("items = %s", got)
	}
	tools := map[string]*model.Tool{}
	for _, tool := range sourcetest.Tools(one) {
		tools[kitFirst(tool.CallID, tool.Name)] = tool
	}
	exit := func(tool *model.Tool) int {
		if tool.Exit == nil {
			return -1
		}
		return *tool.Exit
	}
	// The older shell tool: JSON output, exit code and duration in metadata
	if ls := tools["call_ls"]; ls.Name != "shell" || ls.Status != model.StatusOK || exit(ls) != 0 || ls.DurMs != 100 || ls.Output != "file1.txt\nfile2.txt" || ls.Command != "ls" {
		t.Errorf("shell = %+v", ls)
	}
	// The exec tool: a text header that is information, not output
	if read := tools["call_read"]; read.Name != "exec" || read.Output != "# demo\n" || read.DurMs != 100 || read.Status != model.StatusOK || !strings.Contains(read.Command, "sed -n") {
		t.Errorf("exec = %+v", read)
	}
	// apply_patch: the files come off the patch; a rejected patch is an error with no exit code
	if patch := tools["call_patch"]; exit(patch) != 0 || patch.DurMs != 64 || !strings.HasPrefix(patch.Output, "Success.") || len(patch.Files) != 1 || patch.Files[0] != "README.md" {
		t.Errorf("apply_patch = %+v", patch)
	}
	if bad := tools["call_bad"]; bad.Status != model.StatusError {
		t.Errorf("a rejected patch must be an error: %+v", bad)
	}
	if build := tools["call_build"]; build.Status != model.StatusError || exit(build) != 101 || build.DurMs != 3000 || build.Command != "cargo test" {
		t.Errorf("failed command = %+v", build)
	}
	// A web search has no output row and answers itself
	if search := tools["web_search"]; !search.Done || search.Status != model.StatusOK || search.Kind != model.ToolSearch {
		t.Errorf("web search = %+v", search)
	}
	if one.Failures != 2 || len(one.Files) != 2 {
		t.Errorf("failures = %d files = %+v", one.Failures, one.Files)
	}
	for _, f := range one.Files {
		if f.Changed != (f.Path == "README.md") {
			t.Errorf("%s changed = %v: only the patch that landed is a change", f.Path, f.Changed)
		}
	}
	two := s.Turn(t, 2)
	if !two.Interrupted || sourcetest.Kinds(two) != "prompt event event" || two.Items[1].Event != model.EventInterrupted || two.Items[2].Event != model.EventCompaction {
		t.Errorf("turn 2 = %s interrupted=%v", sourcetest.Kinds(two), two.Interrupted)
	}
	// No token_usage_record: the snapshot written after the response says what it cost
	if want := (model.Usage{Input: 60, Output: 9, CacheRead: 40}); s.Stats.Usage != want || one.Usage != want {
		t.Errorf("usage = %+v, turn 1's %+v, want %+v on the turn the response was made in", s.Stats.Usage, one.Usage, want)
	}
}

func kitFirst(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Only a run of known lines closed by "Output:" is a header; anything else is output and
// stays whole.
func TestOutputHeaders(t *testing.T) {
	none := func(kind string) { t.Errorf("unread %s", kind) }
	body, o := output([]byte(`"Chunk ID: abc\nWall time: 2.5 seconds\nProcess exited with code 3\nOutput:\nboom\n"`), none)
	if body != "boom\n" || o.Exit == nil || *o.Exit != 3 || o.DurMs != 2500 || o.Status != model.StatusError {
		t.Errorf("header parse = %q %+v", body, o)
	}
	body, o = output([]byte(`"Script failed\nWall time 0.2 seconds\nOutput:\n"`), none)
	if body != "" || o.Status != model.StatusError {
		t.Errorf("Script failed must be an error: %q %+v", body, o)
	}
	body, o = output([]byte(`"Exit code: 1 is what the docs say\nreal output"`), none)
	if body != "Exit code: 1 is what the docs say\nreal output" || o.Exit != nil {
		t.Errorf("a non-header must stay whole: %q %+v", body, o)
	}
	body, _ = output([]byte(`"{\"result\": 1}"`), none)
	if body != `{"result": 1}` {
		t.Errorf("non-legacy JSON must stay whole: %q", body)
	}
}

func TestArchivedAndCompressed(t *testing.T) {
	home := t.TempDir()
	meta := func(id string) string {
		return `{"type":"session_meta","payload":{"session_id":"` + id + `","id":"` + id + `","cwd":"/w"}}`
	}
	sessions := filepath.Join(home, "sessions")
	sourcetest.Write(t, filepath.Join(sessions, "2026", "10", "14", "rollout-2026-10-14T10-00-00-live.jsonl"), meta("live"),
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello"}]}}`)
	// While Codex compresses its history a rollout can exist in both forms: the plain one wins
	sourcetest.Write(t, filepath.Join(sessions, "2026", "10", "14", "rollout-2026-10-14T10-00-00-live.jsonl.zst"), "not zstd at all")
	sourcetest.Write(t, filepath.Join(home, "archived_sessions", "rollout-2026-10-01T10-00-00-flat.jsonl"), meta("flat"))
	sourcetest.Write(t, filepath.Join(home, "archived_sessions", "2026", "09", "01", "rollout-2026-09-01T10-00-00-dated.jsonl"), meta("dated"))

	got := map[string]bool{}
	all := sourcetest.All(t, Spec.Open(sessions))
	sourcetest.NothingUnread(t, all...)
	for _, s := range all {
		got[s.Session.ID] = s.Session.Archived
	}
	if len(got) != 3 || got["live"] || !got["flat"] || !got["dated"] {
		t.Fatalf("archived by id = %v, want live false, flat and dated true", got)
	}
	if (&Source{root: t.TempDir()}).archivedRoot() != "" {
		t.Error("a root not named sessions has no archive beside it")
	}
}

// A fork's rollout opens with its parent's turns copied in — Codex rewrites their
// timestamps but keeps the turn ids. The copied turns are the parent's: marked inherited,
// they are no turns of the fork and cost it nothing, so every real turn is counted once
// across the history. A fork of a fork subtracts the whole copied chain.
func TestForkInheritsCopiedTurns(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "10", "23")
	meta := func(id, sessionID, fork string) string {
		payload := `"session_id":"` + sessionID + `","id":"` + id + `","cwd":"/w"`
		if fork != "" {
			payload += `,"forked_from_id":"` + fork + `"`
		}
		return `{"type":"session_meta","payload":{` + payload + `}}`
	}
	turn := func(id string, in int, ts string) []any {
		return []any{
			`{"type":"turn_context","payload":{"turn_id":"` + id + `","model":"gpt-6"}}`,
			`{"timestamp":"` + ts + `","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"ask ` + id + `"}]}}`,
			`{"timestamp":"` + ts + `","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"done ` + id + `"}]}}`,
			`{"timestamp":"` + ts + `","type":"token_usage_record","payload":{"response_id":"resp-` + id + `","usage":{"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":1}}}`,
		}
	}
	join := func(first string, groups ...[]any) []any {
		out := []any{first}
		for _, g := range groups {
			out = append(out, g...)
		}
		return out
	}
	sourcetest.Write(t, filepath.Join(day, "rollout-2026-10-23T10-00-00-parent-1.jsonl"),
		join(meta("parent-1", "parent-1", ""), turn("t1", 10, "2026-10-23T10:00:01Z"), turn("t2", 20, "2026-10-23T10:00:02Z"))...)
	sourcetest.Write(t, filepath.Join(day, "rollout-2026-10-23T11-00-00-child-1.jsonl"),
		join(meta("child-1", "parent-1", "parent-1"), turn("t1", 10, "2026-10-23T11:00:00Z"), turn("t2", 20, "2026-10-23T11:00:01Z"), turn("t3", 40, "2026-10-23T11:00:02Z"))...)
	sourcetest.Write(t, filepath.Join(day, "rollout-2026-10-23T12-00-00-leaf-1.jsonl"),
		join(meta("leaf-1", "child-1", "child-1"), turn("t1", 10, "2026-10-23T12:00:00Z"), turn("t2", 20, "2026-10-23T12:00:01Z"),
			turn("t3", 40, "2026-10-23T12:00:02Z"), turn("t4", 80, "2026-10-23T12:00:03Z"))...)

	all := sourcetest.All(t, Spec.Open(root))
	sourcetest.NothingUnread(t, all...)
	var total int64
	for id, want := range map[string]int64{"parent-1": 30, "child-1": 40, "leaf-1": 80} {
		s := sourcetest.ByID(t, all, id)
		if s.Stats.Usage.Input != want {
			t.Errorf("%s input = %d, want %d (its own turns only)", id, s.Stats.Usage.Input, want)
		}
		total += s.Stats.Usage.Input
		if s.Session.Parent != nil {
			t.Errorf("%s: a fork a person carries on is no one's subagent", id)
		}
	}
	if total != 150 {
		t.Errorf("history total = %d, want 150 (t1..t4 counted once)", total)
	}
	leaf := sourcetest.ByID(t, all, "leaf-1")
	if leaf.Stats.Turns != 1 || leaf.Turn(t, 1).Ask != "ask t4" || leaf.Session.Title != "ask t4" {
		t.Errorf("the leaf's one turn is its own: %+v title %q", leaf.Stats, leaf.Session.Title)
	}
	for _, it := range leaf.Turn(t, 0).Items {
		if !it.Inherited {
			t.Errorf("item %d of the copied turns is not inherited", it.Seq)
		}
	}
}

func TestSubagentThread(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "11", "01")
	sourcetest.Write(t, filepath.Join(day, "rollout-2026-11-01T10-00-00-root-1.jsonl"),
		`{"type":"session_meta","payload":{"session_id":"root-1","id":"root-1","thread_source":"user","cwd":"/w"}}`,
		`{"timestamp":"2026-11-01T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"review this PR"}]}}`,
		`{"timestamp":"2026-11-01T10:00:01Z","type":"response_item","payload":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{}","call_id":"c1"}}`,
		`{"timestamp":"2026-11-01T10:05:00Z","type":"response_item","payload":{"type":"agent_message","author":"/root/pr_review","recipient":"/root","content":[{"text":"two findings"}]}}`,
	)
	sourcetest.Write(t, filepath.Join(day, "rollout-2026-11-01T10-00-02-sub-1.jsonl"),
		`{"type":"session_meta","payload":{"session_id":"root-1","id":"sub-1","parent_thread_id":"root-1","thread_source":"subagent","agent_path":"/root/pr_review","agent_nickname":"Halley","cwd":"/w"}}`,
		`{"timestamp":"2026-11-01T10:00:02Z","type":"response_item","payload":{"type":"agent_message","author":"/root","recipient":"/root/pr_review","content":[{"text":"read the diff and report"}]}}`,
		`{"timestamp":"2026-11-01T10:04:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"two findings"}]}}`,
	)
	all := sourcetest.All(t, Spec.Open(root))
	sourcetest.NothingUnread(t, all...)
	sub, main := sourcetest.ByID(t, all, "sub-1"), sourcetest.ByID(t, all, "root-1")
	if sub.Session.Parent == nil || sub.Session.Parent.Session != "root-1" || sub.Session.Parent.Type != "pr_review" {
		t.Fatalf("subagent parent = %+v", sub.Session.Parent)
	}
	// What the agent above it said to a subagent is that subagent's ask
	if sub.Stats.Turns != 1 || sub.Turn(t, 1).Ask != "read the diff and report" || sub.Turn(t, 1).Answer != "two findings" {
		t.Errorf("subagent turns = %+v", sub.Turns)
	}
	// What a subagent reports back is news that arrived mid-turn, not an ask
	turn := main.Turn(t, 1)
	if main.Stats.Turns != 1 || sourcetest.Kinds(turn) != "prompt tool context" {
		t.Errorf("main turn = %s", sourcetest.Kinds(turn))
	}
	if tool := sourcetest.Tools(turn)[0]; tool.Name != "collaboration.spawn_agent" || tool.Kind != model.ToolAgent {
		t.Errorf("spawn call = %+v", tool)
	}
}

func TestLocate(t *testing.T) {
	if got := Spec.Locate(sourcetest.Env("/home/u")); got != filepath.Join("/home/u", ".codex", "sessions") {
		t.Errorf("default = %s", got)
	}
	if got := Spec.Locate(sourcetest.Env("/home/u", "CODEX_HOME", "/data/codex")); got != filepath.Join("/data/codex", "sessions") {
		t.Errorf("relocated = %s", got)
	}
}

// A rollout too old to have token_usage_record has only the counters Codex writes after
// each response. Their running total reads like the session's total and is not: every
// shape below was measured on a real rollout or reported upstream from one. What adds up
// is what each response cost — counted once.
func TestSnapshotsOfARolloutWithNoRecords(t *testing.T) {
	snap := func(ts string, last, total [4]int) string {
		u := func(v [4]int) string {
			return fmt.Sprintf(`{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d,"total_tokens":%d}`, v[0], v[1], v[2], v[3], v[0]+v[2])
		}
		return `{"timestamp":"2026-08-01T10:` + ts + `Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":` + u(last) + `,"total_token_usage":` + u(total) + `}}}`
	}
	said := func(ts, text string) string {
		return `{"timestamp":"2026-08-01T10:` + ts + `Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}`
	}
	a, b, c, d := [4]int{1000, 600, 50, 10}, [4]int{2000, 1500, 80, 0}, [4]int{500, 0, 20, 0}, [4]int{700, 600, 30, 0}
	ab, ad := [4]int{3000, 2100, 130, 10}, [4]int{1700, 1200, 80, 10}
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "2026", "08", "01", "rollout-2026-08-01T10-00-00-old.jsonl"),
		`{"timestamp":"2026-08-01T10:00:00Z","type":"session_meta","payload":{"id":"codex-old","cwd":"/p"}}`,
		`{"type":"turn_context","payload":{"turn_id":"t1","model":"gpt-5"}}`,
		`{"timestamp":"2026-08-01T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]}}`,
		said("00:05", "first"), snap("00:06", a, a),
		// the same counters written again, later: not a second response
		snap("00:09", a, a),
		said("00:20", "second"), snap("00:21", b, ab),
		// after a compaction: the total repeated, and a last usage of nothing
		`{"timestamp":"2026-08-01T10:00:30Z","type":"compacted","payload":{"message":"summary"}}`,
		snap("00:31", [4]int{0, 0, 0, 0}, ab),
		// resumed in a new process: the total starts again from this response
		`{"timestamp":"2026-08-01T10:10:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"carry on"}]}}`,
		said("10:05", "third"), snap("10:06", c, c),
		// a response that continues from an earlier total than the newest
		said("10:20", "fourth"), snap("10:21", d, ad),
		// an old snapshot sent again at the end of the file
		snap("10:30", b, ab),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	// 400+500+500+100 uncached in, 600+1500+600 from cache, 50+80+20+30 out
	want := model.Usage{Input: 1500, Output: 180, CacheRead: 2700, Reasoning: 10}
	if s.Stats.Usage != want || s.Stats.Calls != 4 {
		t.Errorf("usage = %+v in %d calls, want %+v in 4", s.Stats.Usage, s.Stats.Calls, want)
	}
	if s.Session.Recorded != nil {
		t.Error("the snapshots are read per response: nothing is left to record against the session as a lump")
	}
	// Each response's cost lands in the turn it was made in
	if one, two := s.Turn(t, 1), s.Turn(t, 2); one.Usage.Output != 130 || two.Usage.Output != 50 || len(one.Calls) != 2 || one.Calls[0].Model != "gpt-5" {
		t.Errorf("turn 1 = %+v in %d calls, turn 2 = %+v", one.Usage, len(one.Calls), two.Usage)
	}
}

// Before Codex wrote last_token_usage a snapshot carried the running total alone. What a
// response cost is then how far it moved the total past its highest, and a total below
// that is an old one sent again: the session adds up to the highest total, not the last.
func TestSnapshotsThatCarryOnlyATotal(t *testing.T) {
	total := func(ts string, in, cached, out int) string {
		return fmt.Sprintf(`{"timestamp":"2026-07-01T10:00:%sZ","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}}`, ts, in, cached, out)
	}
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "2026", "07", "01", "rollout-2026-07-01T10-00-00-older.jsonl"),
		`{"timestamp":"2026-07-01T10:00:00Z","type":"session_meta","payload":{"id":"codex-older","cwd":"/p"}}`,
		`{"timestamp":"2026-07-01T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]}}`,
		total("05", 100, 40, 9),
		total("10", 250, 140, 20),
		total("11", 250, 140, 20), // written twice
		total("20", 400, 240, 31),
		total("30", 250, 140, 20), // an old one, sent again last
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if want := (model.Usage{Input: 160, Output: 31, CacheRead: 240}); s.Stats.Usage != want || s.Stats.Calls != 3 {
		t.Errorf("usage = %+v in %d calls, want %+v in 3: the highest total, not the last", s.Stats.Usage, s.Stats.Calls, want)
	}
}

// A rollout with records does not read its snapshots: a record says what its response
// cost, and the snapshots leave out what a compaction spent.
func TestRecordsOutrankSnapshots(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "2026", "10", "02", "rollout-2026-10-02T10-00-00-new.jsonl"),
		`{"timestamp":"2026-10-02T10:00:00Z","type":"session_meta","payload":{"id":"codex-new","cwd":"/p"}}`,
		`{"timestamp":"2026-10-02T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]}}`,
		`{"timestamp":"2026-10-02T10:00:05Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
		`{"timestamp":"2026-10-02T10:00:06Z","type":"token_usage_record","payload":{"response_id":"resp_1","usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":50}}}`,
		`{"timestamp":"2026-10-02T10:00:06Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":50},"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":50}}}}`,
		// the compaction's own request: a record, and a snapshot that leaves it out
		`{"timestamp":"2026-10-02T10:00:20Z","type":"token_usage_record","payload":{"response_id":"resp_2","usage":{"input_tokens":900,"cached_input_tokens":800,"output_tokens":70}}}`,
		`{"timestamp":"2026-10-02T10:00:21Z","type":"compacted","payload":{"message":"summary"}}`,
		`{"timestamp":"2026-10-02T10:00:21Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":0,"output_tokens":0},"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":50}}}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if want := (model.Usage{Input: 500, Output: 120, CacheRead: 1400}); s.Stats.Usage != want || s.Stats.Calls != 2 {
		t.Errorf("usage = %+v in %d calls, want %+v in 2", s.Stats.Usage, s.Stats.Calls, want)
	}
}

// A record is written as its response ends; a snapshot only after the outputs of the
// calls that response made (measured on real rollouts: call, record, output, snapshot).
// Read without its record, the response has been closed by the output by the time its
// snapshot arrives — and the snapshot's cost is still that response's, not a call of its
// own with nothing in it.
func TestASnapshotFollowsTheOutputsOfItsResponse(t *testing.T) {
	root := t.TempDir()
	usage := func(in, out int) string {
		return fmt.Sprintf(`{"input_tokens":%d,"cached_input_tokens":0,"output_tokens":%d,"total_tokens":%d}`, in, out, in+out)
	}
	sourcetest.Write(t, filepath.Join(root, "2026", "08", "02", "rollout-2026-08-02T10-00-00-order.jsonl"),
		`{"timestamp":"2026-08-02T10:00:00Z","type":"session_meta","payload":{"id":"codex-order","cwd":"/p"}}`,
		`{"type":"turn_context","payload":{"turn_id":"t1","model":"gpt-5"}}`,
		`{"timestamp":"2026-08-02T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"list the files"}]}}`,
		`{"timestamp":"2026-08-02T10:00:03Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"look first"}]}}`,
		`{"timestamp":"2026-08-02T10:00:04Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"}}`,
		`{"timestamp":"2026-08-02T10:00:05Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}","call_id":"call_2"}}`,
		`{"timestamp":"2026-08-02T10:00:06Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"a.go"}}`,
		`{"timestamp":"2026-08-02T10:00:07Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_2","output":"/p"}}`,
		`{"timestamp":"2026-08-02T10:00:07Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":`+usage(900, 40)+`,"total_token_usage":`+usage(900, 40)+`}}}`,
		`{"timestamp":"2026-08-02T10:00:09Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"One file: a.go."}]}}`,
		`{"timestamp":"2026-08-02T10:00:10Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":`+usage(1000, 12)+`,"total_token_usage":`+usage(1900, 52)+`}}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if len(turn.Calls) != 2 {
		t.Fatalf("calls = %d, want the two responses: %+v", len(turn.Calls), turn.Calls)
	}
	first, second := turn.Calls[0], turn.Calls[1]
	if first.Usage == nil || first.Usage.Input != 900 || second.Usage == nil || second.Usage.Output != 12 {
		t.Errorf("calls = %+v / %+v: each carries what its snapshot says it cost", first.Usage, second.Usage)
	}
	for _, it := range turn.Items {
		switch it.Kind {
		case model.Thinking, model.ToolUse:
			if it.Call != 1 {
				t.Errorf("%s (seq %d) belongs to call %d, want the first response", it.Kind, it.Seq, it.Call)
			}
		case model.Reply:
			if it.Call != 2 {
				t.Errorf("the reply belongs to call %d, want the second response", it.Call)
			}
		}
	}
}

// The rows Codex writes that say nothing a reader is shown — its setup, its bookkeeping,
// the event stream's copies of what the response items hold — are passed over knowingly:
// none is counted as unread, and the session reads as the response items alone make it.
func TestRowsCodexWritesAndPassesOver(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "2026", "11", "05", "rollout-2026-11-05T10-00-00-quiet.jsonl"),
		`{"timestamp":"2026-11-05T10:00:00Z","type":"session_meta","payload":{"id":"codex-quiet","cwd":"/p","cli_version":"0.167.0"}}`,
		`{"timestamp":"2026-11-05T10:00:00Z","type":"world_state","payload":{"full":true,"state":{"model":"gpt-6.1","tools":[]}}}`,
		`{"timestamp":"2026-11-05T10:00:00Z","type":"dynamic_tools_updated","payload":{"tools":[]}}`,
		`{"timestamp":"2026-11-05T10:00:00Z","type":"event_msg","payload":{"type":"thread_settings_applied","thread_id":"codex-quiet","thread_settings":{}}}`,
		`{"timestamp":"2026-11-05T10:00:01Z","type":"turn_context","payload":{"turn_id":"t1","model":"gpt-6.1","cwd":"/p"}}`,
		`{"timestamp":"2026-11-05T10:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"timestamp":"2026-11-05T10:00:01Z","type":"response_item","payload":{"type":"additional_tools","role":"developer","tools":[]}}`,
		`{"timestamp":"2026-11-05T10:00:01Z","type":"response_item","payload":{"type":"configuration_update","reasoning":{"effort":"high"}}}`,
		`{"timestamp":"2026-11-05T10:00:01Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>"}]}}`,
		`{"timestamp":"2026-11-05T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"what is in this picture?"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"},{"type":"input_audio","audio_url":"data:audio/wav;base64,AAAA"}]}}`,
		`{"timestamp":"2026-11-05T10:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"what is in this picture?"}}`,
		`{"timestamp":"2026-11-05T10:00:02Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage"}}}`,
		`{"timestamp":"2026-11-05T10:00:03Z","type":"response_item","payload":{"type":"ghost_snapshot","ghost_commit":{"id":"abc"}}}`,
		`{"timestamp":"2026-11-05T10:00:04Z","type":"event_msg","payload":{"type":"item_started","turn_id":"t1","item":{"type":"Reasoning"}}}`,
		`{"timestamp":"2026-11-05T10:00:04Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"a cat, probably"}],"content":[{"type":"reasoning_text","text":"look closer"}],"encrypted_content":"gAAA"}}`,
		`{"timestamp":"2026-11-05T10:00:04Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"a cat, probably"}}`,
		`{"timestamp":"2026-11-05T10:00:04Z","type":"event_msg","payload":{"type":"agent_reasoning_raw_content","text":"look closer"}}`,
		`{"timestamp":"2026-11-05T10:00:05Z","type":"response_item","payload":{"type":"function_call","name":"view_image","arguments":"{\"path\":\"cat.png\"}","call_id":"call_view"}}`,
		`{"timestamp":"2026-11-05T10:00:05Z","type":"token_usage_record","payload":{"response_id":"resp_1","usage":{"input_tokens":100,"output_tokens":10}}}`,
		`{"timestamp":"2026-11-05T10:00:06Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_view","output":[{"type":"input_text","text":"cat.png"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}}`,
		`{"timestamp":"2026-11-05T10:00:07Z","type":"event_msg","payload":{"type":"entered_review_mode","target":{"type":"uncommittedChanges"}}}`,
		`{"timestamp":"2026-11-05T10:00:08Z","type":"event_msg","payload":{"type":"exited_review_mode","review_output":null}}`,
		`{"timestamp":"2026-11-05T10:00:09Z","type":"response_item","payload":{"type":"agent_message","author":"/root/helper","recipient":"/root","content":[{"type":"encrypted_content","encrypted_content":"gAAB"},{"type":"input_text","text":"the picture is a cat"}]}}`,
		`{"timestamp":"2026-11-05T10:00:09Z","type":"inter_agent_communication_metadata","payload":{"trigger_turn":false}}`,
		`{"timestamp":"2026-11-05T10:00:10Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"A cat."}]}}`,
		`{"timestamp":"2026-11-05T10:00:10Z","type":"event_msg","payload":{"type":"agent_message","message":"A cat."}}`,
		`{"timestamp":"2026-11-05T10:00:10Z","type":"token_usage_record","payload":{"response_id":"resp_2","usage":{"input_tokens":120,"output_tokens":3}}}`,
		`{"timestamp":"2026-11-05T10:00:11Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","last_agent_message":"A cat."}}`,
		`{"timestamp":"2026-11-05T10:00:12Z","type":"security_risk_score","payload":{"scores":{},"sampled_at":"2026-11-05T10:00:12Z"}}`,
		`{"timestamp":"2026-11-05T10:00:20Z","type":"response_item","payload":{"type":"compaction","encrypted_content":"gAAC"}}`,
		`{"timestamp":"2026-11-05T10:00:20Z","type":"compacted","payload":{"message":"","replacement_history":[]}}`,
		`{"timestamp":"2026-11-05T10:00:20Z","type":"event_msg","payload":{"type":"context_compacted"}}`,
		`{"timestamp":"2026-11-05T10:00:20Z","type":"retained_context","payload":{"user_messages":[],"assistant_messages":[]}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if s.Stats.Turns != 1 || turn.Ask != "what is in this picture?" || turn.Answer != "A cat." {
		t.Errorf("%d turns, ask %q answer %q", s.Stats.Turns, turn.Ask, turn.Answer)
	}
	if got := sourcetest.Kinds(turn); got != "prompt thinking tool context reply event" {
		t.Errorf("items = %s", got)
	}
	if tool := sourcetest.Tools(turn)[0]; tool.Output != "cat.png" {
		t.Errorf("the image beside the output's text is not text: %q", tool.Output)
	}
	if want := (model.Usage{Input: 220, Output: 13}); s.Stats.Usage != want || s.Stats.Calls != 2 {
		t.Errorf("usage = %+v in %d calls, want %+v in 2", s.Stats.Usage, s.Stats.Calls, want)
	}
}

// What the adapter has never heard of — a row, a response item, an event, a role, an
// entry of a content list — is counted under its kind, and so is a record that will not
// decode, known or not; everything around them reads as it would without them.
func TestUnknownKindsAreCounted(t *testing.T) {
	lines := func(unknown bool) []any {
		var out []any
		// add writes a line; one that is unknown only with the unknown records
		add := func(line string, isUnknown bool) {
			if !isUnknown || unknown {
				out = append(out, line)
			}
		}
		// also is what an unknown record adds to a line that is there either way
		also := func(entries string) string {
			if !unknown {
				return ""
			}
			return entries
		}
		add(`{"timestamp":"2026-11-06T10:00:00Z","type":"session_meta","payload":{"id":"codex-new","cwd":"/p"}}`, false)
		add(`{"timestamp":"2026-11-06T10:00:00Z","type":"hologram_state","payload":{"depth":3}}`, true)
		add(`{"timestamp":"2026-11-06T10:00:01Z","type":"turn_context","payload":{"turn_id":"t1","model":"gpt-7"}}`, false)
		add(`{"timestamp":"2026-11-06T10:00:01Z","type":"response_item","payload":{"type":"message","role":"oracle","content":[{"type":"input_text","text":"a voice from nowhere"}]}}`, true)
		add(`{"timestamp":"2026-11-06T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}`+
			also(`,{"type":"input_hologram","hologram_id":"h1"},{"type":"input_poem","text":"read anyway"}`)+`]}}`, false)
		add(`{"timestamp":"2026-11-06T10:00:03Z","type":"event_msg","payload":{"type":"telepathy_begin","turn_id":"t1"}}`, true)
		add(`{"timestamp":"2026-11-06T10:00:04Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"make\"}","call_id":"call_1"}}`, false)
		add(`{"timestamp":"2026-11-06T10:00:04Z","type":"response_item","payload":{"type":"telepathy_call","call_id":"call_2","thought":"x"}}`, true)
		add(`{"timestamp":"2026-11-06T10:00:04Z","type":"token_usage_record","payload":{"response_id":"resp_1","usage":{"input_tokens":50,"output_tokens":5}}}`, false)
		add(`{"timestamp":"2026-11-06T10:00:05Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"Exit code: 0\nOutput:\nok\n"}`+
			also(`,{"type":"input_hologram","hologram_id":"h2"}`)+`]}}`, false)
		add(`{"timestamp":"2026-11-06T10:00:06Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Built."}]}}`, false)
		add(`{"timestamp":"2026-11-06T10:00:06Z","type":"token_usage_record","payload":{"response_id":"resp_2","usage":{"input_tokens":60,"output_tokens":2}}}`, false)
		// Records with a field of another type than the adapter reads it as: unknown kinds,
		// and known ones whose shape has moved on. Each is passed over whole, and counted
		add(`{"timestamp":"2026-11-06T10:00:07Z","type":"hologram_log","payload":{"id":42}}`, true)
		add(`{"timestamp":"2026-11-06T10:00:07Z","type":"event_msg","payload":{"type":"telepathy_end","status":{"completed":"done"}}}`, true)
		add(`{"timestamp":"2026-11-06T10:00:07Z","type":"response_item","payload":{"type":"hologram_call","name":{"tool":"projector"}}}`, true)
		add(`{"timestamp":"2026-11-06T10:00:07Z","type":"event_msg","payload":{"type":"turn_aborted","reason":{"kind":"interrupted"}}}`, true)
		add(`{"timestamp":"2026-11-06T10:00:07Z","type":"token_usage_record","payload":{"response_id":"resp_3","usage":{"input_tokens":"fifty","output_tokens":1}}}`, true)
		// A line that is not JSON says nothing of what it was, such as the end of a rollout
		// still being written: it is not counted
		add(`{"timestamp":"2026-11-06T10:00:08Z","type":"response_item","payload":{"type":"mess`, true)
		return out
	}
	read := func(unknown bool) sourcetest.Session {
		root := t.TempDir()
		sourcetest.Write(t, filepath.Join(root, "2026", "11", "06", "rollout-2026-11-06T10-00-00-new.jsonl"), lines(unknown)...)
		return sourcetest.One(t, Spec.Open(root))
	}
	plain, strange := read(false), read(true)
	sourcetest.NothingUnread(t, plain)
	want := map[string]int{"row:hologram_state": 1, "role:oracle": 1, "event:telepathy_begin": 1, "item:telepathy_call": 1, "part:input_hologram": 2,
		"undecodable:row:hologram_log": 1, "undecodable:event:telepathy_end": 1, "undecodable:item:hologram_call": 1,
		"undecodable:event:turn_aborted": 1, "undecodable:row:token_usage_record": 1}
	if fmt.Sprint(strange.Unread) != fmt.Sprint(want) {
		t.Errorf("unread = %v, want %v", strange.Unread, want)
	}
	// An entry of a kind never seen that holds text is read, not passed over
	if got := strange.Turn(t, 1).Ask; got != "fix the build\nread anyway" {
		t.Errorf("ask = %q", got)
	}
	a, b := plain.Turn(t, 1), strange.Turn(t, 1)
	if sourcetest.Kinds(a) != sourcetest.Kinds(b) || a.Answer != b.Answer || plain.Stats != strange.Stats ||
		sourcetest.Tools(a)[0].Output != sourcetest.Tools(b)[0].Output || sourcetest.Tools(b)[0].Output != "ok\n" {
		t.Errorf("with the unknown records the session reads differently:\n%s %+v\n%s %+v", sourcetest.Kinds(a), plain.Stats, sourcetest.Kinds(b), strange.Stats)
	}
}

// A counter no response could have — below zero, or past anything a response spends — is
// not a count, and reads as none: a corrupt row cannot take a session's tokens below zero,
// nor carry their sum past what an int64 holds and round to below zero there.
func TestCountsThatAreNotCounts(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "2026", "11", "07", "rollout-2026-11-07T10-00-00-corrupt.jsonl"),
		`{"type":"session_meta","payload":{"id":"codex-corrupt","cwd":"/p"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"q"}]}}`,
		`{"type":"token_usage_record","payload":{"response_id":"r1","usage":{"input_tokens":-5,"cached_input_tokens":-3,"cache_write_input_tokens":-2,"output_tokens":-7,"reasoning_output_tokens":-1}}}`,
		`{"type":"token_usage_record","payload":{"response_id":"r2","usage":{"input_tokens":9223372036854775807,"output_tokens":9223372036854775807}}}`,
		`{"type":"token_usage_record","payload":{"response_id":"r3","usage":{"input_tokens":9223372036854775807,"output_tokens":9223372036854775807}}}`,
		// a cache read past the input it is part of must not wrap the input round to the top
		`{"type":"token_usage_record","payload":{"response_id":"r4","usage":{"input_tokens":-9223372036854775808,"cached_input_tokens":1,"output_tokens":1}}}`,
		`{"type":"token_usage_record","payload":{"response_id":"r5","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":9}}}`,
		// numbers past what an int64, or even a float64, holds: none is a count, and the
		// counters beside them still are
		`{"type":"token_usage_record","payload":{"response_id":"r6","usage":{"input_tokens":1e400,"cached_input_tokens":1e19,"output_tokens":2}}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)
	sourcetest.NothingUnread(t, s)
	if want := (model.Usage{Input: 60, Output: 12, CacheRead: 41}); s.Stats.Usage != want {
		t.Errorf("usage = %+v, want %+v: only the counts that are counts", s.Stats.Usage, want)
	}
}

// A running total no session could reach is not a count either, and does not become the
// highest total the snapshots after it are measured against: they still add up.
func TestATotalThatIsNotACount(t *testing.T) {
	total := func(ts string, in, cached, out int64) string {
		return fmt.Sprintf(`{"timestamp":"2026-07-01T10:00:%sZ","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}}`, ts, in, cached, out)
	}
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "2026", "07", "01", "rollout-2026-07-01T10-00-00-corrupt.jsonl"),
		`{"timestamp":"2026-07-01T10:00:00Z","type":"session_meta","payload":{"id":"codex-corrupt","cwd":"/p"}}`,
		`{"timestamp":"2026-07-01T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]}}`,
		total("05", 100, 40, 9),
		total("10", 1<<50, 140, 20),
		total("20", 400, 240, 31),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if want := (model.Usage{Input: 160, Output: 31, CacheRead: 240}); s.Stats.Usage != want || s.Stats.Calls != 2 {
		t.Errorf("usage = %+v in %d calls, want %+v in 2", s.Stats.Usage, s.Stats.Calls, want)
	}
}

// A duration no command ran for — below zero, not a number, longer than thirty years —
// is none, rather than a conversion that overflows to whatever the machine makes of it.
func TestDurationsThatAreNotDurations(t *testing.T) {
	none := func(kind string) { t.Errorf("unread %s", kind) }
	for _, raw := range []string{
		`"{\"output\":\"x\",\"metadata\":{\"exit_code\":0,\"duration_seconds\":-1e300}}"`,
		`"{\"output\":\"x\",\"metadata\":{\"exit_code\":0,\"duration_seconds\":1e300}}"`,
		`"Wall time: 1e300 seconds\nOutput:\nx"`,
		`"Wall time: Inf seconds\nOutput:\nx"`,
		`"Wall time: NaN seconds\nOutput:\nx"`,
		`"Wall time: -3 seconds\nOutput:\nx"`,
	} {
		if body, o := output([]byte(raw), none); body != "x" || o.DurMs != 0 {
			t.Errorf("%s: %q in %d ms, want x in none", raw, body, o.DurMs)
		}
	}
}

// A command that outlasted the wait answers with its output so far and a session to poll:
// it has not ended, so it has no status — and nothing reading the record takes it for one
// that succeeded. One that ended says how.
func TestACommandStillRunningHasNoStatus(t *testing.T) {
	text, o := stripHeader("Chunk ID: b2\nWall time: 10.0 seconds\nProcess running with session ID 7\nOriginal token count: 3\nOutput:\n=== RUN TestX\n")
	if text != "=== RUN TestX\n" || o.Status != "" || o.Exit != nil {
		t.Errorf("still running: %q, %+v", text, o)
	}
	_, o = stripHeader("Chunk ID: b3\nWall time: 1.0 seconds\nProcess exited with code 0\nOriginal token count: 3\nOutput:\nok\n")
	if o.Status != model.StatusOK || o.Exit == nil || *o.Exit != 0 {
		t.Errorf("ended: %+v", o)
	}
	// The same, as the list of items an output may also be
	list := `[{"type":"input_text","text":"Chunk ID: d4\nWall time: 10.0 seconds\nProcess running with session ID 8\nOriginal token count: 3\nOutput:\n=== RUN TestY\n"}]`
	if text, o := output(json.RawMessage(list), func(string) {}); text != "=== RUN TestY\n" || o.Status != "" {
		t.Errorf("still running, as a list: %q, %+v", text, o)
	}
	list = `[{"type":"input_text","text":"Chunk ID: d5\nWall time: 1.0 seconds\nProcess exited with code 0\nOriginal token count: 3\nOutput:\nok\n"}]`
	if _, o := output(json.RawMessage(list), func(string) {}); o.Status != model.StatusOK {
		t.Errorf("ended, as a list: %+v", o)
	}
}
