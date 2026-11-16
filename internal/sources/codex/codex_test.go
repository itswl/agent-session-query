package codex

import (
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
		if got := sourcetest.One(t, Spec.Open(root)).Session.ID; got != c.want {
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
		`{"timestamp":"2026-10-01T10:00:31Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":9,"total_tokens":109},"last_token_usage":{"input_tokens":50}}}}`,
		`{"timestamp":"2026-10-01T10:01:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run it again"}]}}`,
		`{"timestamp":"2026-10-01T10:01:05Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t2","reason":"interrupted","duration_ms":95000}}`,
		`{"timestamp":"2026-10-01T10:01:06Z","type":"compacted","payload":{"message":""}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
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
	// No token_usage_record: the last cumulative token_count is the session's total
	if want := (model.Usage{Input: 60, Output: 9, CacheRead: 40}); s.Stats.Usage != want {
		t.Errorf("usage = %+v, want %+v", s.Stats.Usage, want)
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
	body, o := output([]byte(`"Chunk ID: abc\nWall time: 2.5 seconds\nProcess exited with code 3\nOutput:\nboom\n"`))
	if body != "boom\n" || o.Exit == nil || *o.Exit != 3 || o.DurMs != 2500 || o.Status != model.StatusError {
		t.Errorf("header parse = %q %+v", body, o)
	}
	body, o = output([]byte(`"Script failed\nWall time 0.2 seconds\nOutput:\n"`))
	if body != "" || o.Status != model.StatusError {
		t.Errorf("Script failed must be an error: %q %+v", body, o)
	}
	body, o = output([]byte(`"Exit code: 1 is what the docs say\nreal output"`))
	if body != "Exit code: 1 is what the docs say\nreal output" || o.Exit != nil {
		t.Errorf("a non-header must stay whole: %q %+v", body, o)
	}
	body, _ = output([]byte(`"{\"result\": 1}"`))
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
	for _, s := range sourcetest.All(t, Spec.Open(sessions)) {
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
