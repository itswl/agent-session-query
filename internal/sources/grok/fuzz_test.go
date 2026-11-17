package grok

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// FuzzRead feeds the adapter a session directory of arbitrary files: a summary, and a
// transcript that is either the update stream of a session that ended or, for one still
// running, the whole-message log with the usage.json beside it. Whatever the bytes, a
// read may fail, but it may not panic, hang, or give a session that breaks what every
// session must hold.
func FuzzRead(f *testing.F) {
	_, dir := fixture(f)
	summary := readFile(f, filepath.Join(dir, summaryFile))
	f.Add(summary, readFile(f, filepath.Join(dir, updatesFile)), false, []byte(nil))
	f.Add(summary, jsonl(f, chatHistory...), true, []byte(sessionUsage))

	// What Grok 1.0.77 wrote: commands that exited 0 and 2, writes and edits whose words
	// are in rawOutput, a search's matches as bytes, a plan-mode turn, and a session read
	// by its whole-message log
	for _, name := range []string{"shell", "files", "search", "plan"} {
		dir := filepath.Join("testdata", name)
		f.Add(readFile(f, filepath.Join(dir, summaryFile)), readFile(f, filepath.Join(dir, updatesFile)), false, []byte(nil))
	}
	f.Add(readFile(f, filepath.Join("testdata", "shell-history", summaryFile)), readFile(f, filepath.Join("testdata", "shell-history", historyFile)),
		true, readFile(f, filepath.Join("testdata", "shell-history", usageFile)))

	// Turns that close on a tool's result, on words and on nothing, and a call named by
	// its title alone
	f.Add([]byte(`{"info":{"id":"sid-turns","cwd":"/w"},"current_model_id":"grok-4.6"}`), jsonl(f,
		chunk(1790000000, "user_message_chunk", "run the build"),
		chunk(1790000001, "agent_message_chunk", "Starting."),
		`{"timestamp":1790000002,"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"run_command","rawInput":{"command":"make"}}}}`,
		`{"timestamp":1790000009,"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"failed","content":[{"type":"content","content":{"type":"text","text":"ok"}}]}}}`,
		turnCompleted(1790000010, M{"inputTokens": 100, "outputTokens": 20, "cachedReadTokens": 1000}),
		chunk(1790000100, "user_message_chunk", "and the tests"),
		chunk(1790000101, "agent_thought_chunk", "they ran before"),
		chunk(1790000101, "agent_message_chunk", "They pass."),
		turnCompleted(1790000102, M{"inputTokens": 7, "outputTokens": 3}),
		chunk(1790000200, "user_message_chunk", "thanks"),
		turnCompleted(1790000201, nil),
	), false, []byte(nil))

	// Lines that are not updates, fields of the wrong type, and kinds it does not know
	f.Add([]byte(`{"info":{"id":"sid-odd"},"created_at":1790000000}`), jsonl(f,
		`{}`,
		`not json`,
		`{"timestamp":1790000000,"method":"session/update","params":{"update":{"sessionUpdate":"current_mode_update","currentModeId":"plan"}}}`,
		chunk(1790000001, "user_message_chunk", "read it"),
		`{"timestamp":1790000001,"method":"session/update","params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"image","data":"…"}}}}`,
		`{"timestamp":"1790000002","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":7,"status":{"was":"a string"},"rawInput":{"target_file":"a.go"},"_meta":{"x.ai/tool":{"name":"read_file"}}}}}`,
		`{"timestamp":1790000002,"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"cancelled"}}}`,
		`{"timestamp":1790000003,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"memory_written"}}}`,
		`{"timestamp":1790000003,"method":"_x.ai/session/compacted","params":{"update":{}}}`,
		`{"timestamp":1790000003,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"plan","entries":[]}}}`,
		`{"timestamp":1790000003,"method":"session/update","params":{"update":{"sessionUpdate":"usage_update","used":31344,"size":256000}}}`,
		`{"timestamp":1790000003,"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed"}}}`,
	), false, []byte(nil))

	// Counters past what any turn could spend — two that an int64 holds but their sum does
	// not, which once added up to below zero, and one past what an int64 holds at all
	f.Add([]byte(`{"info":{"id":"sid-huge"}}`), jsonl(f,
		chunk(1790000000, "user_message_chunk", "one"),
		turnCompleted(1790000001, M{"inputTokens": 5e18, "outputTokens": 5}),
		chunk(1790000002, "user_message_chunk", "two"),
		turnCompleted(1790000003, M{"inputTokens": 5e18, "outputTokens": -7}),
		chunk(1790000004, "user_message_chunk", "three"),
		turnCompleted(1790000005, M{"inputTokens": 1e19, "outputTokens": model.MaxTokens + 1}),
	), false, []byte(nil))

	// A log with a second prompt, calls with arguments that are not JSON, and reasoning
	// with nothing after it yet
	f.Add([]byte(`{"info":{"id":"sid-live","cwd":"/w"}}`), jsonl(f,
		`{"type":"user","content":"list the files","prompt_index":0}`,
		`{"type":"user","content":"<user_info>\nOS Version: linux\n</user_info>"}`,
		`{"type":"assistant","content":"one file.","model_id":"gemini-3.8-flash"}`,
		`{"type":"user","content":[{"type":"text","text":"<user_query>\ndeploy the gateway`+harness+`\n</user_query>"}]}`,
		`{"type":"assistant","content":"On it.","tool_calls":[{"id":"c1","name":"run_terminal_command","arguments":"not json"}],"model_id":"grok-4.6"}`,
		`{"type":"tool_result","tool_call_id":"c1","content":"exit: 2\nno such file"}`,
		`{"type":"handoff","content":"carry on"}`,
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"Waiting on the deploy."}]}`,
	), true, []byte(`{"session":{"inputTokens":5,"outputTokens":"many"}}`))

	f.Fuzz(func(t *testing.T, summary, transcript []byte, running bool, usage []byte) {
		root := t.TempDir()
		dir := filepath.Join(root, "%2Fw", "sid")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{summaryFile: summary, updatesFile: transcript}
		if running {
			files = map[string][]byte{summaryFile: summary, historyFile: transcript, usageFile: usage}
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		sourcetest.Attempt(t, Spec.Open(root))
	})
}

func readFile(t testing.TB, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// jsonl renders lines as a JSONL file holds them: a string as it is, anything else as JSON.
func jsonl(t testing.TB, lines ...any) []byte {
	t.Helper()
	var out []byte
	for _, line := range lines {
		if s, ok := line.(string); ok {
			out = append(out, s...)
		} else {
			b, err := json.Marshal(line)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, b...)
		}
		out = append(out, '\n')
	}
	return out
}
