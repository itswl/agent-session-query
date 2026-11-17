package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// seeds are rollouts in the shapes the tests are built from: a session with records, the
// tool rows, a fork of itself, a subagent, a rollout with only snapshots, and the rows
// Codex writes that the adapter passes over, or cannot decode.
var seeds = [][]string{
	{
		`{"type":"session_meta","payload":{"session_id":"codex-1","cwd":"/home/u/x","cli_version":"0.154.0","git":{"branch":"main"}}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:05Z","payload":{"type":"message","role":"developer","content":[{"text":"system"}]}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:06Z","payload":{"type":"message","role":"user","id":"c1","content":[{"text":"question"}]}}`,
		`{"type":"turn_context","payload":{"turn_id":"t1","model":"gpt-6.1"}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:08Z","payload":{"type":"reasoning","summary":[{"text":"thinking it over"}]}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:09Z","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"ls\"]}","call_id":"call_1"}}`,
		`{"type":"token_usage_record","timestamp":"2026-09-13T23:07:09Z","payload":{"response_id":"resp_1","turn_id":"t1","usage":{"input_tokens":130,"cached_input_tokens":100,"output_tokens":5,"reasoning_output_tokens":3}}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:10Z","payload":{"type":"function_call_output","call_id":"call_1","output":"a.txt"}}`,
		`{"type":"response_item","timestamp":"2026-09-13T23:07:12Z","payload":{"type":"message","role":"assistant","id":"c2","content":[{"text":"answer"}]}}`,
		`{"type":"token_usage_record","timestamp":"2026-09-13T23:07:12Z","payload":{"response_id":"resp_2","turn_id":"t1","usage":{"input_tokens":7,"output_tokens":2}}}`,
	},
	{
		`{"timestamp":"2026-10-01T10:00:00Z","type":"session_meta","payload":{"id":"codex-tools","cwd":"/p","cli_version":"0.160.0"}}`,
		`{"timestamp":"2026-10-01T10:00:00Z","type":"turn_context","payload":{"turn_id":"t1","model":"gpt-6.1","cwd":"/p"}}`,
		`{"timestamp":"2026-10-01T10:00:02Z","type":"response_item","payload":{"type":"message","id":"msg_user","role":"user","content":[{"type":"input_text","text":"update the README"}]}}`,
		`{"timestamp":"2026-10-01T10:00:06Z","type":"response_item","payload":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"**Inspecting README**"}],"encrypted_content":"gAAA"}}`,
		`{"timestamp":"2026-10-01T10:00:08Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"ls\"],\"workdir\":\"/p\"}","call_id":"call_ls"}}`,
		`{"timestamp":"2026-10-01T10:00:09Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_ls","output":"{\"output\":\"file1.txt\\nfile2.txt\",\"metadata\":{\"exit_code\":0,\"duration_seconds\":0.1}}"}}`,
		`{"timestamp":"2026-10-01T10:00:10Z","type":"response_item","payload":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_read","name":"exec","input":"text((await tools.exec_command({cmd:\"sed -n '1,80p' README.md\"})).output)"}}`,
		`{"timestamp":"2026-10-01T10:00:11Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_read","output":[{"type":"input_text","text":"Script completed\nWall time 0.1 seconds\nOutput:\n"},{"type":"input_text","text":"# demo\n"}]}}`,
		`{"timestamp":"2026-10-01T10:00:15Z","type":"response_item","payload":{"type":"custom_tool_call","id":"ctc_4","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** Update File: README.md\n@@\n-old\n+new\n*** End Patch"}}`,
		`{"timestamp":"2026-10-01T10:00:16Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_patch","output":"Exit code: 0\nWall time: 0.064 seconds\nOutput:\nSuccess. Updated the following files:\nM README.md\n"}}`,
		`{"timestamp":"2026-10-01T10:00:18Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_bad","output":"apply_patch verification failed: context not found"}}`,
		`{"timestamp":"2026-10-01T10:00:20Z","type":"response_item","payload":{"type":"local_shell_call","call_id":"call_sh","action":{"type":"exec","command":["cargo","test"]}}}`,
		`{"timestamp":"2026-10-01T10:00:25Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_sh","output":"{\"output\":\"test failed\\n\",\"metadata\":{\"exit_code\":101,\"duration_seconds\":3}}"}}`,
		`{"timestamp":"2026-10-01T10:00:26Z","type":"response_item","payload":{"type":"web_search_call","status":"completed","action":{"type":"search","query":"cargo test flags"}}}`,
		`{"timestamp":"2026-10-01T10:00:30Z","type":"response_item","payload":{"type":"message","id":"msg_final","role":"assistant","content":[{"type":"output_text","text":"README updated."}]}}`,
		`{"timestamp":"2026-10-01T10:00:31Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":9,"total_tokens":109},"last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":9,"total_tokens":109}}}}`,
		`{"timestamp":"2026-10-01T10:01:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run it again"}]}}`,
		`{"timestamp":"2026-10-01T10:01:05Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t2","reason":"interrupted","duration_ms":95000}}`,
		`{"timestamp":"2026-10-01T10:01:06Z","type":"compacted","payload":{"message":""}}`,
	},
	{
		// a fork of the file itself, so the parent's turn ids are this file's own
		`{"type":"session_meta","payload":{"session_id":"fuzz","id":"fork-1","forked_from_id":"fuzz","cwd":"/w"}}`,
		`{"type":"turn_context","payload":{"turn_id":"t1","model":"gpt-6"}}`,
		`{"timestamp":"2026-10-23T11:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"ask t1"}]}}`,
		`{"timestamp":"2026-10-23T11:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"done t1"}]}}`,
		`{"timestamp":"2026-10-23T11:00:00Z","type":"token_usage_record","payload":{"response_id":"resp-t1","usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"session_meta","payload":{"session_id":"parent","id":"parent"}}`,
		`{"type":"turn_context","payload":{"turn_id":"t2","model":"gpt-6"}}`,
		`{"timestamp":"2026-10-23T11:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"ask t2"}]}}`,
	},
	{
		`{"type":"session_meta","payload":{"session_id":"root-1","id":"sub-1","parent_thread_id":"root-1","thread_source":"subagent","agent_path":"/root/pr_review","agent_nickname":"Halley","cwd":"/w"}}`,
		`{"timestamp":"2026-11-01T10:00:02Z","type":"response_item","payload":{"type":"agent_message","author":"/root","recipient":"/root/pr_review","content":[{"text":"read the diff and report"}]}}`,
		`{"timestamp":"2026-11-01T10:00:03Z","type":"response_item","payload":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{}","call_id":"c1"}}`,
		`{"timestamp":"2026-11-01T10:04:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"two findings"}]}}`,
		`{"timestamp":"2026-11-01T10:05:00Z","type":"response_item","payload":{"type":"agent_message","author":"/root/pr_review","recipient":"/root","content":[{"text":"two findings"}]}}`,
	},
	{
		`{"timestamp":"2026-08-01T10:00:00Z","type":"session_meta","payload":{"id":"codex-old","cwd":"/p"}}`,
		`{"type":"turn_context","payload":{"turn_id":"t1","model":"gpt-5"}}`,
		`{"timestamp":"2026-08-01T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]}}`,
		`{"timestamp":"2026-08-01T10:00:05Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}}`,
		`{"timestamp":"2026-08-01T10:00:06Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":50,"reasoning_output_tokens":10},"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":50,"reasoning_output_tokens":10}}}}`,
		`{"timestamp":"2026-08-01T10:00:30Z","type":"compacted","payload":{"message":"summary"}}`,
		`{"timestamp":"2026-08-01T10:00:31Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":0,"output_tokens":0},"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":50}}}}`,
		`{"timestamp":"2026-08-01T10:00:40Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1400,"cached_input_tokens":800,"output_tokens":70}}}}`,
	},
	{
		`{"timestamp":"2026-11-05T10:00:00Z","type":"session_meta","payload":{"id":"codex-quiet","cwd":"/p","cli_version":"0.167.0"}}`,
		`{"timestamp":"2026-11-05T10:00:00Z","type":"world_state","payload":{"full":true,"state":{"model":"gpt-6.1","tools":[]}}}`,
		`{"timestamp":"2026-11-05T10:00:01Z","type":"turn_context","payload":{"turn_id":"t1","model":"gpt-6.1","cwd":"/p"}}`,
		`{"timestamp":"2026-11-05T10:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"timestamp":"2026-11-05T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"what is in this picture?"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}}`,
		`{"timestamp":"2026-11-05T10:00:02Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage"}}}`,
		`{"timestamp":"2026-11-05T10:00:06Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_view","output":[{"type":"input_text","text":"cat.png"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}}`,
		`{"timestamp":"2026-11-05T10:00:09Z","type":"response_item","payload":{"type":"agent_message","author":"/root/helper","recipient":"/root","content":[{"type":"encrypted_content","encrypted_content":"gAAB"}]}}`,
		`{"timestamp":"2026-11-05T10:00:09Z","type":"inter_agent_communication_metadata","payload":{"trigger_turn":false}}`,
		`{"timestamp":"2026-11-05T10:00:10Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"A cat."}]}}`,
		`{"timestamp":"2026-11-05T10:00:10Z","type":"token_usage_record","payload":{"response_id":"resp_2","usage":{"input_tokens":120,"output_tokens":3}}}`,
		`{"timestamp":"2026-11-05T10:00:11Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","last_agent_message":"A cat."}}`,
		`{"timestamp":"2026-11-05T10:00:12Z","type":"event_msg","payload":{"type":"turn_aborted","reason":{"kind":"interrupted"}}}`,
		`{"timestamp":"2026-11-05T10:00:12Z","type":"token_usage_record","payload":{"response_id":"resp_3","usage":{"input_tokens":1e400,"output_tokens":-1}}}`,
	},
}

// FuzzRead reads whatever bytes a rollout might hold, from where Scan finds a rollout. An
// error is an answer; a panic, a hang, or a session that breaks what every session must
// hold is not.
func FuzzRead(f *testing.F) {
	for _, lines := range seeds {
		f.Add([]byte(strings.Join(lines, "\n") + "\n"))
	}
	// One store, its rollout written over by each input. With a directory made and removed
	// per input the fuzzer took about every other input for new coverage (measured: 51 of
	// 104) and spent its time minimizing them, at a few runs a second.
	root := filepath.Join(f.TempDir(), "sessions")
	path := filepath.Join(root, "2026", "11", "01", "rollout-2026-11-01T10-00-00-fuzz.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		sourcetest.Attempt(t, Spec.Open(root))
	})
}
