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

// TestCodexToolRows: a rollout is more than its message rows. The calls, their outputs,
// the thinking and the aborts each live on a row of their own, and for a long time none of
// them were read — a Codex session showed two people talking with nothing in between, and
// its brief listed no tools at all. The shapes are the ones real rollouts write: the older
// shell tool's JSON output with exit code and duration in metadata, the newer tools'
// text header ("Script completed / Wall time / Output:", "Exit code: N"), apply_patch's
// input, and the event_msg that records the user stopping a turn.
func TestCodexToolRows(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "2026", "10", "01", "rollout-2026-10-01T10-00-00-tools.jsonl")
	write(t, path,
		`{"timestamp":"2026-10-01T10:00:00Z","type":"session_meta","payload":{"id":"codex-tools","cwd":"/p","cli_version":"0.160.0"}}`,
		`{"timestamp":"2026-10-01T10:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
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
	)

	s := newCodexSource(root)
	rec := s.List()[0]
	if rec.str("model") != "gpt-6.1" {
		t.Errorf("the model lives on turn_context and was not picked up: %q", rec.str("model"))
	}

	msgs := s.Messages(rec, messageQuery{limit: 100})
	// Every row the reader shows is counted the same way by the list and by Final
	final := s.Final(rec)
	if n, _ := codexCountMessages(path, 0); n != len(msgs) || final["messageCount"] != len(msgs) {
		t.Fatalf("counts disagree: counter %d, messages %d, final %v", n, len(msgs), final["messageCount"])
	}
	// 4 user rows (2 injected) + 1 thinking (the encrypted-only one yields nothing) +
	// 5 calls + 5 outputs + 1 web search + 1 assistant + 1 abort = 18
	if len(msgs) != 18 {
		for i, m := range msgs {
			t.Logf("%2d %s %v", i, m["role"], m["content"])
		}
		t.Fatalf("messages = %d", len(msgs))
	}

	byCall := map[string]map[string]any{}
	var injected, thinking, aborted int
	for _, m := range msgs {
		if truthy(m["injected"]) {
			injected++
		}
		for _, block := range m["content"].([]map[string]any) {
			switch block["type"] {
			case "thinking":
				thinking++
				if block["content"] != "**Inspecting README**" {
					t.Errorf("thinking = %v", block["content"])
				}
			case "toolResult":
				byCall[toStr(block["callId"])] = block
				// A web search has no output row and answers itself on the assistant
				// message; every other result is a role=tool message of its own
				if m["role"] != "tool" && block["toolName"] != "web_search" {
					t.Errorf("a result rides on a role=tool message, got %v", m["role"])
				}
			case "event":
				aborted++
				if m["role"] != "system" || block["kind"] != eventInterrupted {
					t.Errorf("abort event = %v on role %v", block, m["role"])
				}
			}
		}
	}
	if injected != 2 {
		t.Errorf("the AGENTS.md and environment rows must be flagged injected, got %d", injected)
	}
	if thinking != 1 || aborted != 1 {
		t.Errorf("thinking = %d, aborted = %d", thinking, aborted)
	}

	// The older shell tool: JSON output, exit code and duration in metadata
	ls := byCall["call_ls"]
	if ls["toolName"] != "shell" || ls["status"] != statusOK || ls["exitCode"] != 0 || ls["durationMs"] != int64(100) || ls["content"] != "file1.txt\nfile2.txt" {
		t.Errorf("shell result = %v", ls)
	}
	// The exec tool: a text header that is information, not output
	read := byCall["call_read"]
	if read["toolName"] != "exec" || read["content"] != "# demo\n" || read["durationMs"] != int64(100) || read["status"] != statusOK {
		t.Errorf("exec result = %v", read)
	}
	// apply_patch: "Exit code: 0" header; a rejected patch is an error without one
	patch := byCall["call_patch"]
	if patch["toolName"] != "apply_patch" || patch["exitCode"] != 0 || patch["durationMs"] != int64(64) || !strings.HasPrefix(toStr(patch["content"]), "Success.") {
		t.Errorf("apply_patch result = %v", patch)
	}
	if bad := byCall["call_bad"]; bad["status"] != statusError {
		t.Errorf("a rejected patch must be an error, got %v", bad)
	}
	// A failing command: the exit code is the verdict
	build := byCall["call_build"]
	if build["status"] != statusError || build["exitCode"] != 101 || build["durationMs"] != int64(3000) {
		t.Errorf("failed command = %v", build)
	}

	// The call blocks carry the id their result answers, and decoded arguments
	calls := 0
	for _, m := range msgs {
		for _, block := range m["content"].([]map[string]any) {
			if block["type"] != "toolCall" {
				continue
			}
			calls++
			if block["name"] == "shell" {
				args := block["arguments"].(map[string]any)
				if block["id"] != "call_ls" || args["workdir"] != "/p" {
					t.Errorf("shell call = %v", block)
				}
			}
			if block["name"] == "apply_patch" && !strings.Contains(toStr(block["arguments"].(map[string]any)["input"]), "*** Begin Patch") {
				t.Errorf("apply_patch call lost its patch: %v", block)
			}
		}
	}
	if calls != 6 { // five paired calls and the web search
		t.Errorf("tool calls = %d", calls)
	}

	// Final: the last assistant words, the model, and usage from token_count when there
	// is no token_usage_record (total_token_usage is cumulative, so the last one is the
	// total, not a sum)
	if final["text"] != "README updated." || final["model"] != "gpt-6.1" {
		t.Errorf("final = %v", final)
	}
	usage := final["usage"].(map[string]any)
	if usage["inputTokens"] != int64(100) || usage["cacheReadTokens"] != int64(40) || usage["outputTokens"] != int64(9) {
		t.Errorf("usage = %v", usage)
	}

	// Search: a hit inside a call or its output names a speaker rather than the row type
	for line, want := range map[string]string{
		`{"type":"response_item","payload":{"type":"function_call","name":"shell"}}`:         "assistant",
		`{"type":"response_item","payload":{"type":"function_call_output","output":"x"}}`:    "tool",
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[]}}`:   "user",
		`{"type":"response_item","payload":{"type":"reasoning","summary":[{"text":"hmm"}]}}`: "assistant",
	} {
		var obj map[string]any
		if err := jsonUnmarshalString(line, &obj); err != nil {
			t.Fatal(err)
		}
		if got := hitRole(obj); got != want {
			t.Errorf("hitRole(%s) = %q, want %q", line, got, want)
		}
	}
}

// TestCodexOutputHeaders pins the header grammar: only a run of known lines closed by
// "Output:" is a header; anything else is output and stays whole.
func TestCodexOutputHeaders(t *testing.T) {
	body, outcome := codexOutput("Chunk ID: abc\nWall time: 2.5 seconds\nProcess exited with code 3\nOutput:\nboom\n")
	if body != "boom\n" || outcome.exitCode != 3 || !outcome.hasExit || outcome.durationMs != 2500 || outcome.status != statusError {
		t.Errorf("header parse = %q %+v", body, outcome)
	}
	body, outcome = codexOutput("Script failed\nWall time 0.2 seconds\nOutput:\n")
	if body != "" || outcome.status != statusError {
		t.Errorf("Script failed must be an error: %q %+v", body, outcome)
	}
	// A first line that merely looks like a header line, with no Output: closing it
	body, outcome = codexOutput("Exit code: 1 is what the docs say\nreal output")
	if body != "Exit code: 1 is what the docs say\nreal output" || outcome.hasExit {
		t.Errorf("a non-header must stay whole: %q %+v", body, outcome)
	}
	// JSON that is not the legacy shape is output
	body, _ = codexOutput(`{"result": 1}`)
	if body != `{"result": 1}` {
		t.Errorf("non-legacy JSON must stay whole: %q", body)
	}
}

// TestCodexInjected: what the CLI assembles is not what the person asked
func TestCodexInjected(t *testing.T) {
	for text, want := range map[string]bool{
		"# AGENTS.md instructions for /p\n\n<INSTRUCTIONS>\nx\n</INSTRUCTIONS>": true,
		"<environment_context>\n  <cwd>/p</cwd>\n</environment_context>":        true,
		"<recommended_plugins>\n- a\n</recommended_plugins>":                    true,
		"update the README":                       false,
		"<b>bold</b> is html, and this trails it": false,
		"<cwd>/p</cwd> please fix":                false,
	} {
		if got := codexInjected(text); got != want {
			t.Errorf("codexInjected(%q) = %v, want %v", text, got, want)
		}
	}
}
