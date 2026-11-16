package grok

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
	"github.com/itswl/agent-session-query/internal/transcript"
)

const sessionID = "01a0cbfb-8d46-70c0-a73c-d53201e0e77c"

// harness is what T3 Code closes every prompt it hands Grok with, glued to the last word.
const harness = "<runtime_info>In case you're asked: you run in T3 Code.</runtime_info><pull_request_linking>Link every PR.</pull_request_linking>"

// fixture is the shapes a real Grok CLI 1.0.41 session writes: a JSON-RPC envelope per
// line carrying an epoch-seconds timestamp, with Grok's own events under
// _x.ai/session/update sharing the file with the Agent Client Protocol stream.
func fixture(t testing.TB) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, "%2Fprivate%2Ftmp%2Fgrok-probe", sessionID)
	sourcetest.Write(t, filepath.Join(dir, "summary.json"),
		`{"info":{"id":"01a0cbfb-8d46-70c0-a73c-d53201e0e77c","cwd":"/private/tmp/grok-probe"},`+
			`"session_summary":"read sample.txt","generated_title":"Reading sample.txt",`+
			`"created_at":"2026-09-23T01:55:34.929541Z","updated_at":"2026-09-23T01:55:39.711040Z",`+
			`"last_active_at":"2026-09-23T01:55:39.711040Z","num_messages":8,"num_chat_messages":6,`+
			`"current_model_id":"gpt-5.6-luna","session_kind":"headless"}`,
	)
	sourcetest.Write(t, filepath.Join(dir, "updates.jsonl"),
		// Grok's own events are not transcript content and must not become messages
		`{"timestamp":1790128656,"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"hook_execution","event_name":"session_start","runs":[]}}}`,
		`{"timestamp":1790128656,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"read sample.txt"},"_meta":{"modelId":"gpt-5.6-luna","promptIndex":0}}}}`,
		`{"timestamp":1790128663,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"read_file","rawInput":{"target_file":"sample.txt"},"_meta":{"x.ai/tool":{"name":"read_file","kind":"read"}}}}}`,
		// A status-less update is the call being re-titled mid-flight: no output, no result
		`{"timestamp":1790128663,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","kind":"read","title":"Read sample.txt","locations":[{"path":"sample.txt"}]}}}`,
		`{"timestamp":1790128664,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"hello from fixture"}}],"rawOutput":{"type":"ReadFile"}}}}`,
		`{"timestamp":1790128665,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"checking the file"}}}}`,
		// Streamed text arrives in chunks and has to come back out as one block
		`{"timestamp":1790128666,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"The file says "}}}}`,
		`{"timestamp":1790128666,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello from fixture"}}}}`,
		`{"timestamp":1790128666,"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","stop_reason":"end_turn","usage":{"inputTokens":15667,"outputTokens":53,"totalTokens":31344,"cachedReadTokens":15624,"reasoningTokens":10,"modelCalls":2}}}}`,
	)
	return root, dir
}

type M = map[string]any

// chunk is one line of the Agent Client Protocol stream.
func chunk(ts int, kind, text string) M {
	return M{"timestamp": ts, "method": "session/update",
		"params": M{"update": M{"sessionUpdate": kind, "content": M{"type": "text", "text": text}}}}
}

// turnCompleted is the row Grok closes a turn with.
func turnCompleted(ts int, usage M) M {
	return M{"timestamp": ts, "method": "_x.ai/session/update",
		"params": M{"update": M{"sessionUpdate": "turn_completed", "stop_reason": "end_turn", "usage": usage}}}
}

func at(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func TestSession(t *testing.T) {
	root, dir := fixture(t)
	s := sourcetest.One(t, Spec.Open(root))
	// The hook run is a kind Grok writes, passed over on purpose
	sourcetest.NothingUnread(t, s)

	// cwd is what groups sessions into projects, and Grok states it outright rather than
	// only encoding it into the directory name
	if s.Session.ID != sessionID || s.Session.Cwd != "/private/tmp/grok-probe" || s.Session.Path != dir || s.Ref.Key != dir {
		t.Errorf("session = %+v, ref = %+v", s.Session, s.Ref)
	}
	// Grok titles a session itself, so its title wins over the opening prompt
	if s.Session.Title != "Reading sample.txt" {
		t.Errorf("title = %q, want the title Grok generated", s.Session.Title)
	}
	if s.Session.Model != "gpt-5.6-luna" || s.Session.Running || s.Session.Archived {
		t.Errorf("session = %+v", s.Session)
	}
	// The summary's own times, not the transcript's
	if want := time.Date(2026, 9, 23, 1, 55, 34, 929541000, time.UTC); !s.Session.StartedAt.Equal(want) {
		t.Errorf("startedAt = %v, want %v", s.Session.StartedAt, want)
	}
	if want := time.Date(2026, 9, 23, 1, 55, 39, 711040000, time.UTC); !s.Session.UpdatedAt.Equal(want) {
		t.Errorf("updatedAt = %v, want %v", s.Session.UpdatedAt, want)
	}

	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d: the hook run is not conversation", len(s.Turns))
	}
	turn := s.Turn(t, 1)
	// Nine lines, four items: the re-titling is nothing, and two chunks are one reply
	if got := sourcetest.Kinds(turn); got != "prompt tool thinking reply" {
		t.Fatalf("items = %s", got)
	}
	if turn.Ask != "read sample.txt" || turn.Answer != "The file says hello from fixture" {
		t.Errorf("ask %q answer %q", turn.Ask, turn.Answer)
	}
	if turn.Items[2].Text != "checking the file" {
		t.Errorf("thinking = %q", turn.Items[2].Text)
	}
	tool := sourcetest.Tools(turn)[0]
	if tool.CallID != "call_1" || tool.Name != "read_file" || tool.Kind != model.ToolRead || !tool.Done ||
		tool.Status != model.StatusOK || tool.Output != "hello from fixture" {
		t.Errorf("tool = %+v", tool)
	}
	if len(tool.Files) != 1 || tool.Files[0] != "sample.txt" || !strings.Contains(string(tool.Input), `"target_file"`) {
		t.Errorf("tool input = %s, files = %v", tool.Input, tool.Files)
	}

	// Epoch seconds, and each piece at its own: the ask when it was typed, the call when it
	// was made, its result when it came back, the turn's end at the agent's last message
	if !turn.StartedAt.Equal(at(1790128656)) || !turn.Items[0].At.Equal(time.Date(2026, 9, 23, 1, 57, 36, 0, time.UTC)) {
		t.Errorf("turn started %v, prompt at %v", turn.StartedAt, turn.Items[0].At)
	}
	if !turn.Items[1].At.Equal(at(1790128663)) || !tool.EndAt.Equal(at(1790128664)) || !turn.EndedAt.Equal(at(1790128665)) {
		t.Errorf("call at %v, result at %v, turn ended %v", turn.Items[1].At, tool.EndAt, turn.EndedAt)
	}

	// The turn's counters are on its last message from the agent and nowhere else. The
	// messages either side of the tool are the two responses Grok itself counted
	// (modelCalls: 2), and the prompt named the model that gave them.
	if len(turn.Calls) != 2 || turn.Calls[0].Usage != nil || turn.Calls[1].Usage == nil {
		t.Fatalf("calls = %+v", turn.Calls)
	}
	if turn.Calls[1].Stop != "end_turn" || turn.Calls[0].Model != "gpt-5.6-luna" || turn.Calls[1].Model != "gpt-5.6-luna" {
		t.Errorf("calls = %+v", turn.Calls)
	}
	// cachedReadTokens is Grok's spelling. The input already leaves the cache out and the
	// output already holds the reasoning: 15667 + 53 + 15624 is the turn's own totalTokens.
	want := model.Usage{Input: 15667, Output: 53, CacheRead: 15624, Reasoning: 10}
	if s.Stats.Usage != want || turn.Usage != want {
		t.Errorf("usage = %+v / %+v, want %+v", s.Stats.Usage, turn.Usage, want)
	}
	if s.Stats.Usage.Total() != 31344 {
		t.Errorf("total = %d, want the turn's totalTokens", s.Stats.Usage.Total())
	}
}

// Grok counts tokens per turn. Each turn's are its own, and a turn that closed on a
// tool's result rather than on words still carries what it cost.
func TestUsagePerTurn(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "%2Fw", "sid-turns")
	sourcetest.Write(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"sid-turns","cwd":"/w"},"current_model_id":"grok-4.6"}`)
	sourcetest.Write(t, filepath.Join(dir, "updates.jsonl"),
		chunk(1790000000, "user_message_chunk", "run the build"),
		chunk(1790000001, "agent_message_chunk", "Starting."),
		`{"timestamp":1790000002,"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"run_command","rawInput":{"command":"make"}}}}`,
		`{"timestamp":1790000009,"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"ok"}}]}}}`,
		turnCompleted(1790000010, M{"inputTokens": 100, "outputTokens": 20, "cachedReadTokens": 1000}),
		chunk(1790000100, "user_message_chunk", "and the tests"),
		chunk(1790000101, "agent_message_chunk", "They pass."),
		turnCompleted(1790000102, M{"inputTokens": 7, "outputTokens": 3}),
		// a turn that produced nothing and cost nothing leaves nothing behind
		chunk(1790000200, "user_message_chunk", "thanks"),
		turnCompleted(1790000201, nil),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Stats.Turns != 3 {
		t.Fatalf("turns = %d", s.Stats.Turns)
	}
	one, two, three := s.Turn(t, 1), s.Turn(t, 2), s.Turn(t, 3)
	if got := sourcetest.Kinds(one); got != "prompt reply tool" {
		t.Errorf("turn 1 = %s", got)
	}
	if one.Usage != (model.Usage{Input: 100, Output: 20, CacheRead: 1000}) {
		t.Errorf("turn 1 usage = %+v", one.Usage)
	}
	// The call that carries them is a message with no words: the turn's last was the tool's
	if len(one.Calls) != 2 || one.Calls[0].Usage != nil || one.Calls[1].Usage == nil || !one.EndedAt.Equal(at(1790000010)) {
		t.Errorf("turn 1 calls = %+v, ended %v", one.Calls, one.EndedAt)
	}
	if tool := sourcetest.Tools(one)[0]; tool.Name != "run_command" || tool.Command != "make" || !tool.EndAt.Equal(at(1790000009)) {
		t.Errorf("tool = %+v: without Grok's _meta, a call is named by its title", tool)
	}
	if two.Usage != (model.Usage{Input: 7, Output: 3}) || two.Answer != "They pass." || len(two.Calls) != 1 {
		t.Errorf("turn 2 = %+v / %q / %+v", two.Usage, two.Answer, two.Calls)
	}
	if got := sourcetest.Kinds(three); got != "prompt" || len(three.Calls) != 0 {
		t.Errorf("turn 3 = %s, calls %+v", got, three.Calls)
	}
	// With no prompt naming one, the model is the summary's
	if one.Calls[0].Model != "grok-4.6" {
		t.Errorf("model = %q", one.Calls[0].Model)
	}
	if s.Stats.Usage != (model.Usage{Input: 107, Output: 23, CacheRead: 1000}) {
		t.Errorf("session usage = %+v", s.Stats.Usage)
	}
}

// chatHistory is the whole-message log of the fixture's session while it still runs, in
// the shapes of a real 1.0.52 session's chat_history.jsonl; sessionUsage is the usage.json
// beside it.
var chatHistory = []any{
	`{"type":"system","content":"You are Grok released by xAI."}`,
	`{"type":"user","content":[{"type":"text","text":"<user_query>\n运行 ls 列出当前目录的文件。\n</user_query>"}]}`,
	`{"type":"reasoning","id":"","summary":[{"type":"summary_text","text":"The user wants a directory listing."}]}`,
	`{"type":"assistant","tool_calls":[{"id":"call_1","name":"run_terminal_command","arguments":"{\"command\":\"ls -la\",\"description\":\"Run ls\"}"}],"model_id":"gemini-3.8-flash","content":""}`,
	`{"type":"tool_result","tool_call_id":"call_1","content":"exit: 0\ntotal 4\nnote.txt"}`,
	`{"type":"assistant","content":"共有 1 个文件：note.txt。","model_id":"gemini-3.8-flash"}`,
}

const sessionUsage = `{"sessionId":"s1","session":{"inputTokens":19219,"outputTokens":74,"cachedReadTokens":6832,"turnCount":1}}`

// A session still running has no updates.jsonl — Grok flushes the stream when the session
// ends — and carries chat_history.jsonl, one whole message per line. That log is read
// instead. Measured against a real 1.0.52 session's chat_history.jsonl.
func TestRunningSessionReadsChatHistory(t *testing.T) {
	root, dir := fixture(t)
	stream, err := os.ReadFile(filepath.Join(dir, "updates.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "updates.jsonl")); err != nil {
		t.Fatal(err)
	}
	sourcetest.Write(t, filepath.Join(dir, "chat_history.jsonl"), chatHistory...)
	sourcetest.Write(t, filepath.Join(dir, "usage.json"), sessionUsage)

	src := Spec.Open(root)
	s := sourcetest.One(t, src)
	// The system line is a kind Grok writes, passed over on purpose
	sourcetest.NothingUnread(t, s)
	if !s.Session.Running {
		t.Error("a session with no stream yet is running, and says so")
	}
	if s.Session.Title != "Reading sample.txt" {
		t.Errorf("title = %q", s.Session.Title)
	}
	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d: the system prompt is not conversation, and the wrapped prompt is an ask", len(s.Turns))
	}
	turn := s.Turn(t, 1)
	// The same four items the stream would give: the reasoning with the call it led to, the
	// result, the closing words
	if got := sourcetest.Kinds(turn); got != "prompt thinking tool reply" {
		t.Fatalf("items = %s", got)
	}
	if turn.Ask != "运行 ls 列出当前目录的文件。" || turn.Items[0].Text != turn.Ask {
		t.Errorf("ask = %q / %q, want the words inside <user_query>", turn.Ask, turn.Items[0].Text)
	}
	tool := sourcetest.Tools(turn)[0]
	// arguments ride as a JSON string; a reader wants the object
	if tool.Name != "run_terminal_command" || tool.CallID != "call_1" || tool.Command != "ls -la" || !strings.Contains(tool.Output, "note.txt") {
		t.Errorf("tool = %+v", tool)
	}
	if !tool.Done || tool.Status != "" {
		t.Errorf("status = %q: the log names none, and none is not success", tool.Status)
	}
	if !strings.Contains(turn.Answer, "note.txt") {
		t.Errorf("answer = %q", turn.Answer)
	}
	// A response is its reasoning and what came of it: two here, each under the model its
	// own line names, while the session's is the summary's
	if len(turn.Calls) != 2 || turn.Calls[0].Model != "gemini-3.8-flash" || turn.Calls[1].Model != "gemini-3.8-flash" {
		t.Errorf("calls = %+v", turn.Calls)
	}
	if turn.Items[1].Call != 1 || turn.Items[2].Call != 1 || turn.Items[3].Call != 2 {
		t.Errorf("items = %+v", turn.Items)
	}
	if s.Session.Model != "gpt-5.6-luna" {
		t.Errorf("model = %q", s.Session.Model)
	}
	// The log has no counters; usage.json has the session's so far
	if want := (model.Usage{Input: 19219, Output: 74, CacheRead: 6832}); s.Stats.Usage != want {
		t.Errorf("usage = %+v, want %+v", s.Stats.Usage, want)
	}

	// The session ends, the stream lands: it is read from then on, and nothing is running
	if err := os.WriteFile(filepath.Join(dir, "updates.jsonl"), stream, 0o644); err != nil {
		t.Fatal(err)
	}
	done := sourcetest.One(t, src)
	sourcetest.NothingUnread(t, done)
	if done.Session.Running || done.Session.Recorded != nil || done.Turn(t, 1).Ask != "read sample.txt" {
		t.Errorf("ended session = %+v, ask %q", done.Session, done.Turn(t, 1).Ask)
	}
	if done.Ref.Stamp == s.Ref.Stamp {
		t.Error("the stream landing must move the stamp")
	}
}

// What the whole-message log leaves to the reader: a prompt after a prompt is a second
// message, reasoning with nothing after it yet is still said, and a line names its model.
func TestChatHistoryShapes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "%2Fw", "sid-live")
	sourcetest.Write(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"sid-live","cwd":"/w"}}`)
	sourcetest.Write(t, filepath.Join(dir, "chat_history.jsonl"),
		`{"type":"user","content":"list the files"}`,
		`{"type":"assistant","content":"one file.","model_id":"gemini-3.8-flash"}`,
		`{"type":"user","content":[{"type":"text","text":"<user_query>\ndeploy the gateway`+harness+`\n</user_query>"}]}`,
		`{"type":"assistant","content":"On it.","tool_calls":[{"id":"c1","name":"run_terminal_command","arguments":"not json"}],"model_id":"grok-4.6"}`,
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"Waiting on the deploy."}]}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Stats.Turns != 2 || !s.Session.Running {
		t.Fatalf("turns = %d, running = %v", s.Stats.Turns, s.Session.Running)
	}
	one, two := s.Turn(t, 1), s.Turn(t, 2)
	if one.Ask != "list the files" || one.Answer != "one file." || one.Calls[0].Model != "gemini-3.8-flash" {
		t.Errorf("turn 1 = %q / %q / %+v", one.Ask, one.Answer, one.Calls)
	}
	// The harness's blocks are not the ask, inside the wrapper or not
	if two.Ask != "deploy the gateway" {
		t.Errorf("ask = %q", two.Ask)
	}
	// A message's words come before the calls it makes
	if got := sourcetest.Kinds(two); got != "prompt reply tool thinking" {
		t.Errorf("turn 2 = %s", got)
	}
	if tool := sourcetest.Tools(two)[0]; tool.Done || string(tool.Input) != `{"raw":"not json"}` {
		t.Errorf("tool = %+v, input %s", tool, tool.Input)
	}
	if len(two.Calls) != 2 || two.Calls[0].Model != "grok-4.6" || two.Calls[1].Model != "grok-4.6" {
		t.Errorf("calls = %+v", two.Calls)
	}
	// No line named the session's model, so it is the last that answered
	if s.Session.Model != "grok-4.6" {
		t.Errorf("model = %q", s.Session.Model)
	}
	if s.Session.Title != "list the files" {
		t.Errorf("title = %q", s.Session.Title)
	}
}

// Grok names the group directory after the working directory, URL-encoded, and swaps that
// for a slug plus a hash once the encoded form passes 255 bytes — recording the real path
// in a .cwd file. Without both fallbacks such a session would belong to no project.
func TestCwdFallbacks(t *testing.T) {
	root := t.TempDir()
	// A plus in a directory name survives: PathUnescape leaves it alone where
	// QueryUnescape would turn it into a space
	sourcetest.Write(t, filepath.Join(root, "%2FUsers%2Fdev%2Fsome+project", "sid-1", "summary.json"), `{"info":{"id":"sid-1"}}`)
	sourcetest.Write(t, filepath.Join(root, "some-long-project-a1b2c3", "sid-2", "summary.json"), `{"info":{"id":"sid-2"}}`)
	sourcetest.Write(t, filepath.Join(root, "some-long-project-a1b2c3", ".cwd"), "/Users/dev/some/very/long/path")

	all := sourcetest.All(t, Spec.Open(root))
	sourcetest.NothingUnread(t, all...)
	if len(all) != 2 {
		t.Fatalf("sessions = %d: the .cwd file is not one", len(all))
	}
	if got := sourcetest.ByID(t, all, "sid-1").Session.Cwd; got != "/Users/dev/some+project" {
		t.Errorf("decoded cwd = %q", got)
	}
	if got := sourcetest.ByID(t, all, "sid-2").Session.Cwd; got != "/Users/dev/some/very/long/path" {
		t.Errorf(".cwd cwd = %q", got)
	}
}

// Grok writes summary.json when the session is created, before anything is said. Such a
// session is still listed; it has no transcript to read.
func TestSessionWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "%2Ftmp%2Fempty", "sid-new", "summary.json"),
		`{"info":{"id":"sid-new","cwd":"/tmp/empty"},"created_at":"2026-09-23T01:32:09.978229Z","updated_at":"2026-09-23T01:32:09.984705Z","num_messages":0,"current_model_id":"grok-4.6"}`)
	// a directory Grok has not made a session of is not one
	sourcetest.Write(t, filepath.Join(root, "%2Ftmp%2Fempty", "stray", "updates.jsonl"), chunk(1790000000, "user_message_chunk", "hello"))

	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "sid-new" || s.Session.Cwd != "/tmp/empty" || s.Session.Model != "grok-4.6" || s.Session.Running {
		t.Errorf("session = %+v", s.Session)
	}
	if len(s.Turns) != 0 || s.Stats.Items != 0 || s.Session.Title != "" {
		t.Errorf("turns = %d, items = %d, title = %q", len(s.Turns), s.Stats.Items, s.Session.Title)
	}
	// With no last_active_at, the time the summary itself was written
	if want := time.Date(2026, 9, 23, 1, 32, 9, 984705000, time.UTC); !s.Session.UpdatedAt.Equal(want) {
		t.Errorf("updatedAt = %v", s.Session.UpdatedAt)
	}
}

// A prompt arrives in chunks, so a title taken from the first row that yields text would
// stop wherever chunk one happened to end. It matters before Grok writes a title of its
// own, which is exactly the new sessions at the top of a list.
func TestTitleJoinsChunkedPrompt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "%2Ftmp%2Fchunked", "sid-chunked")
	sourcetest.Write(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"sid-chunked","cwd":"/tmp/chunked"}}`)
	sourcetest.Write(t, filepath.Join(dir, "updates.jsonl"),
		`{"timestamp":1790128000,"method":"session/update","params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"refactor the "}}}}`,
		`{"timestamp":1790128000,"method":"session/update","params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"parser to handle OSC"}}}}`,
		`{"timestamp":1790128009,"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"on it"}}}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Session.Title != "refactor the parser to handle OSC" || s.Turn(t, 1).Ask != "refactor the parser to handle OSC" {
		t.Errorf("title = %q, ask = %q, want the whole prompt", s.Session.Title, s.Turn(t, 1).Ask)
	}
	// The transcript's own times stand in where the summary records none
	if !s.Session.StartedAt.Equal(at(1790128000)) || !s.Session.UpdatedAt.Equal(at(1790128009)) {
		t.Errorf("times = %v … %v", s.Session.StartedAt, s.Session.UpdatedAt)
	}
}

// Chunks of one kind that follow each other are one block; a block of another kind
// between them, or another speaker, ends it. A reply is as long as it was, however many
// pieces it came in.
func TestChunksFoldIntoBlocks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "%2Fw", "sid-chunks")
	sourcetest.Write(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"sid-chunks"}}`)
	lines := []any{
		chunk(1790000000, "user_message_chunk", "explain it"),
		chunk(1790000001, "agent_thought_chunk", "first, "),
		chunk(1790000001, "agent_thought_chunk", "the parser"),
		chunk(1790000002, "agent_message_chunk", "It reads "),
		chunk(1790000002, "agent_message_chunk", "tokens."),
		chunk(1790000003, "agent_thought_chunk", "then the tree"),
		M{"timestamp": 1790000004, "method": "session/update", "params": M{"update": M{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "read_file"}}},
	}
	var long strings.Builder
	for i := 0; i < 3000; i++ {
		lines = append(lines, chunk(1790000005, "agent_message_chunk", "word "))
		long.WriteString("word ")
	}
	sourcetest.Write(t, filepath.Join(dir, "updates.jsonl"), lines...)

	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt thinking reply thinking tool reply" {
		t.Fatalf("items = %s", got)
	}
	for i, want := range []string{"explain it", "first, the parser", "It reads tokens.", "then the tree", "", long.String()} {
		if turn.Items[i].Text != want {
			t.Errorf("item %d = %.40q, want %.40q", i, turn.Items[i].Text, want)
		}
	}
	// One speaker throughout — the call was never answered — so one message, one response
	if len(turn.Calls) != 1 || turn.Answer != "It reads tokens.\n"+strings.TrimSpace(long.String()) || sourcetest.Tools(turn)[0].Done {
		t.Errorf("calls = %d, answer = %.40q", len(turn.Calls), turn.Answer)
	}
}

// Grok's stored title can be the prompt's opening words cut short, and T3 Code closes
// every prompt it hands Grok with blocks of its own: the cut can fall inside one.
func TestStoredTitle(t *testing.T) {
	prompt := "deploy the gateway" + harness
	for _, c := range []struct {
		name, summary, want string
	}{
		{"the generated title first",
			`{"generated_title":"Deploying the gateway","session_summary":"deploy the gateway"}`, "Deploying the gateway"},
		{"a title cut inside a harness block is cut where the block opens",
			`{"session_summary":"deploy the gateway<runtime_info>In case you're asked: you ru"}`, "deploy the gateway"},
		{"a title that is the whole extended prompt loses the blocks",
			`{"generated_title":"deploy the gateway<runtime_info>a</runtime_info><pull_request_linking>b</pull_request_linking>"}`, "deploy the gateway"},
		{"a title that is nothing but a block gives way to the next",
			`{"generated_title":"<runtime_info>In case you're asked: you ru","session_summary":"Gateway deploy"}`, "Gateway deploy"},
		{"plumbing is no title: the first ask is",
			`{"generated_title":"<system-reminder>The user opened a file","session_summary":""}`, "deploy the gateway"},
	} {
		root := t.TempDir()
		dir := filepath.Join(root, "%2Fw", "sid-title")
		sourcetest.Write(t, filepath.Join(dir, "summary.json"), c.summary)
		sourcetest.Write(t, filepath.Join(dir, "updates.jsonl"),
			chunk(1790000000, "user_message_chunk", prompt), chunk(1790000001, "agent_message_chunk", "deployed"))
		s := sourcetest.One(t, Spec.Open(root))
		sourcetest.NothingUnread(t, s)
		if s.Session.Title != c.want {
			t.Errorf("%s: title = %q, want %q", c.name, s.Session.Title, c.want)
		}
		// The transcript keeps the blocks — the model read them — and the ask does not
		if turn := s.Turn(t, 1); turn.Ask != "deploy the gateway" || turn.Items[0].Text != prompt {
			t.Errorf("%s: ask = %q, prompt = %q", c.name, turn.Ask, turn.Items[0].Text)
		}
		// A summary with no info names the session by its directory
		if s.Session.ID != "sid-title" || s.Session.Cwd != "/w" {
			t.Errorf("%s: session = %+v", c.name, s.Session)
		}
	}
}

// Grok moves an archived session from sessions to archived_sessions beside it, where it
// is still history. A tool call that ends with status failed is a failure.
func TestArchivedSessionsAndFailedTool(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "sessions")
	live := filepath.Join(root, "%2Fw%2Flive", "11111111-1111-7111-8111-111111111111")
	sourcetest.Write(t, filepath.Join(live, "summary.json"),
		`{"info":{"id":"11111111-1111-7111-8111-111111111111","cwd":"/w/live"},"generated_title":"Live one","last_active_at":"2026-10-01T10:00:00Z"}`)
	sourcetest.Write(t, filepath.Join(live, "updates.jsonl"),
		`{"timestamp":1790000000,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hello"}}}}`)
	archived := filepath.Join(home, "archived_sessions", "%2Fw%2Fold", "22222222-2222-7222-8222-222222222222")
	sourcetest.Write(t, filepath.Join(archived, "summary.json"),
		`{"info":{"id":"22222222-2222-7222-8222-222222222222","cwd":"/w/old"},"generated_title":"Archived one","last_active_at":"2026-09-01T10:00:00Z"}`)
	sourcetest.Write(t, filepath.Join(archived, "updates.jsonl"),
		`{"timestamp":1789000000,"method":"session/update","params":{"sessionId":"s2","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"run the build"}}}}`,
		`{"timestamp":1789000001,"method":"session/update","params":{"sessionId":"s2","update":{"sessionUpdate":"tool_call","toolCallId":"call_9","title":"run_command","rawInput":{"command":"make"},"_meta":{"x.ai/tool":{"name":"run_command","kind":"execute"}}}}}`,
		`{"timestamp":1789000002,"method":"session/update","params":{"sessionId":"s2","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_9","status":"failed","content":[{"type":"content","content":{"type":"text","text":"make: *** No rule"}}]}}}`,
	)

	// a trailing separator on the root does not hide the archive beside it
	all := sourcetest.All(t, Spec.Open(root+string(filepath.Separator)))
	sourcetest.NothingUnread(t, all...)
	if len(all) != 2 {
		t.Fatalf("sessions = %d, want the live and the archived one", len(all))
	}
	if live := sourcetest.ByID(t, all, "11111111-1111-7111-8111-111111111111"); live.Session.Archived || live.Session.Title != "Live one" {
		t.Errorf("live session = %+v", live.Session)
	}
	old := sourcetest.ByID(t, all, "22222222-2222-7222-8222-222222222222")
	if !old.Session.Archived || old.Session.Title != "Archived one" || old.Session.Cwd != "/w/old" {
		t.Errorf("archived session = %+v", old.Session)
	}
	turn := old.Turn(t, 1)
	tool := sourcetest.Tools(turn)[0]
	if tool.CallID != "call_9" || tool.Name != "run_command" || tool.Kind != model.ToolExec || tool.Command != "make" ||
		tool.Status != model.StatusError || tool.Output != "make: *** No rule" {
		t.Errorf("tool = %+v", tool)
	}
	if turn.Failures != 1 || old.Stats.Failures != 1 || sourcetest.Kinds(turn) != "prompt tool" {
		t.Errorf("failures = %d, items = %s", turn.Failures, sourcetest.Kinds(turn))
	}

	// A relocated root has no archived_sessions beside it to look at
	moved := filepath.Join(home, "elsewhere")
	sourcetest.Write(t, filepath.Join(moved, "%2Fw", "sid-moved", "summary.json"), `{"info":{"id":"sid-moved"}}`)
	sourcetest.Write(t, filepath.Join(home, "archived_sessions", "%2Fw", "sid-x", "summary.json"), `{"info":{"id":"sid-x"}}`)
	if s := sourcetest.One(t, Spec.Open(moved)); s.Session.ID != "sid-moved" || s.Session.Archived {
		t.Errorf("relocated = %+v", s.Session)
	}
}

// What is not a transcript line does not end the read, and a field that changed type
// costs that field alone.
func TestLinesThatAreNotUpdates(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "%2Fw", "sid-odd")
	sourcetest.Write(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"sid-odd"},"created_at":1790000000}`)
	sourcetest.Write(t, filepath.Join(dir, "updates.jsonl"),
		`{}`,
		`not json`,
		`{"timestamp":1790000000,"method":"session/update","params":{"update":{"sessionUpdate":"current_mode_update","currentModeId":"plan"}}}`,
		chunk(1790000001, "user_message_chunk", "read it"),
		// an image where text was expected, and an empty chunk, add nothing
		`{"timestamp":1790000001,"method":"session/update","params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"image","data":"…"}}}}`,
		chunk(1790000001, "agent_message_chunk", ""),
		`{"timestamp":"1790000002","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":7,"status":{"was":"a string"},"rawInput":{"target_file":"a.go"},"_meta":{"x.ai/tool":{"name":"read_file"}}}}}`,
		// still running, then finished with nothing to show
		`{"timestamp":1790000002,"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"in_progress"}}}`,
		`{"timestamp":1790000003,"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed"}}}`,
	)
	s := sourcetest.One(t, Spec.Open(root))
	// A line that is not JSON, or names no kind, is not a record of a new kind; a mode
	// change and a call still in progress are kinds Grok writes, passed over on purpose
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt tool" || turn.Ask != "read it" {
		t.Fatalf("items = %s, ask = %q", got, turn.Ask)
	}
	tool := sourcetest.Tools(turn)[0]
	if tool.Name != "read_file" || len(tool.Files) != 1 || tool.Files[0] != "a.go" || tool.Status != model.StatusOK || tool.Output != "" {
		t.Errorf("tool = %+v", tool)
	}
	if !turn.Items[1].At.Equal(at(1790000002)) || !s.Session.StartedAt.Equal(at(1790000000)) {
		t.Errorf("call at %v, session started %v", turn.Items[1].At, s.Session.StartedAt)
	}
}

// A record of a kind no Grok had written when this adapter was made is passed over and
// counted under its kind — that is how asq doctor learns the format moved — and the rest
// of the session reads as it would without it.
func TestUnknownKindsAreCounted(t *testing.T) {
	root, dir := fixture(t)
	want := sourcetest.One(t, Spec.Open(root))

	stream, err := os.ReadFile(filepath.Join(dir, "updates.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(stream)), "\n")
	unknown := []string{
		`{"timestamp":1790128663,"method":"session/update","params":{"update":{"sessionUpdate":"agent_video_chunk","content":{"type":"text","text":"frames"}}}}`,
		`{"timestamp":1790128663,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"memory_written","note":"remember this"}}}`,
		`{"timestamp":1790128663,"method":"_x.ai/session/compacted","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"a summary"}}}}`,
		`{"timestamp":1790128664,"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"cancelled","content":[{"type":"content","content":{"type":"text","text":"stopped"}}]}}}`,
	}
	// Between a call and its result, between the two chunks of the reply — which still
	// join into one block — and before the row that closes the turn
	var mixed []string
	mixed = append(mixed, lines[:3]...) // the hook run, the prompt, the call
	mixed = append(mixed, unknown[3])
	mixed = append(mixed, lines[3:7]...) // the call's re-titling and result, the thought, the reply's first chunk
	mixed = append(mixed, unknown[:3]...)
	mixed = append(mixed, lines[7], unknown[0], lines[8])
	if err := os.WriteFile(filepath.Join(dir, "updates.jsonl"), []byte(strings.Join(mixed, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := sourcetest.One(t, Spec.Open(root))
	wantUnread := map[string]int{
		"event:agent_video_chunk":        2,
		"event:memory_written":           1,
		"method:_x.ai/session/compacted": 1,
		"status:cancelled":               1,
	}
	if !reflect.DeepEqual(got.Unread, wantUnread) {
		t.Errorf("unread = %v, want %v", got.Unread, wantUnread)
	}
	if !reflect.DeepEqual(got.Turns, want.Turns) || got.Stats != want.Stats {
		t.Errorf("with records it does not know, the session reads otherwise:\n%+v\n%+v", got.Turns, want.Turns)
	}

	// The whole-message log of a session still running, likewise
	root, dir = fixture(t)
	if err := os.Remove(filepath.Join(dir, "updates.jsonl")); err != nil {
		t.Fatal(err)
	}
	sourcetest.Write(t, filepath.Join(dir, "usage.json"), sessionUsage)
	sourcetest.Write(t, filepath.Join(dir, "chat_history.jsonl"), chatHistory...)
	want = sourcetest.One(t, Spec.Open(root))
	history := append([]any{}, chatHistory[:3]...)
	history = append(history, `{"type":"handoff","to":"another agent","content":"carry on"}`)
	history = append(history, chatHistory[3:]...)
	sourcetest.Write(t, filepath.Join(dir, "chat_history.jsonl"), history...)
	got = sourcetest.One(t, Spec.Open(root))
	if !reflect.DeepEqual(got.Unread, map[string]int{"row:handoff": 1}) {
		t.Errorf("unread = %v", got.Unread)
	}
	if !reflect.DeepEqual(got.Turns, want.Turns) || got.Stats != want.Stats {
		t.Errorf("with a row it does not know, the log reads otherwise:\n%+v\n%+v", got.Turns, want.Turns)
	}
}

// A counter no turn could reach is not a count. On arm64 a float past the int64 range
// converts to the largest int64, and two such turns added up wrapped the session's input
// below zero.
func TestCountersPastAnyTurnAreNotRead(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "%2Fw", "sid-huge")
	sourcetest.Write(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"sid-huge"}}`)
	sourcetest.Write(t, filepath.Join(dir, "updates.jsonl"),
		chunk(1790000000, "user_message_chunk", "one"),
		turnCompleted(1790000001, M{"inputTokens": 1e19, "outputTokens": 5}),
		chunk(1790000002, "user_message_chunk", "two"),
		turnCompleted(1790000003, M{"inputTokens": 1e19, "outputTokens": 7, "cachedReadTokens": -3}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)
	if s.Stats.Usage != (model.Usage{Output: 12}) {
		t.Errorf("usage = %+v, want the counters that are counts", s.Stats.Usage)
	}
}

func scan(t *testing.T, src sources.Source) map[string]sources.Ref {
	t.Helper()
	refs, err := src.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	out := map[string]sources.Ref{}
	for _, ref := range refs {
		out[ref.Key] = ref
	}
	return out
}

// A session's stamp moves when any file its read depends on does, and holds while only
// another session changes.
func TestScanStamps(t *testing.T) {
	root, dir := fixture(t)
	other := filepath.Join(root, "%2Fw", "sid-other")
	sourcetest.Write(t, filepath.Join(other, "summary.json"), `{"info":{"id":"sid-other"}}`)
	src := Spec.Open(root)

	before := scan(t, src)
	if len(before) != 2 || before[dir].Stamp == "" || before[dir].ModTime.IsZero() {
		t.Fatalf("refs = %+v", before)
	}
	if again := scan(t, src); again[dir] != before[dir] || again[other] != before[other] {
		t.Errorf("an untouched store must scan the same: %+v then %+v", before, again)
	}

	appendTo := func(name, text string) {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(text + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	last := before[dir].Stamp
	for _, name := range []string{"updates.jsonl", "chat_history.jsonl", "usage.json", "summary.json"} {
		appendTo(name, " ")
		now := scan(t, src)
		if now[dir].Stamp == last {
			t.Errorf("%s changed and the stamp did not", name)
		}
		if now[other] != before[other] {
			t.Errorf("%s of one session moved another's stamp", name)
		}
		last = now[dir].Stamp
	}
}

// A store that is not there holds nothing. One that cannot be read is an error: its
// sessions are not gone, they could not be looked at.
func TestScanTellsUnreadableFromEmpty(t *testing.T) {
	home := t.TempDir()
	if refs, err := Spec.Open(filepath.Join(home, "sessions")).Scan(context.Background()); err != nil || len(refs) != 0 {
		t.Errorf("a missing store: %v, %v", refs, err)
	}
	// A path that runs through a file cannot be read, and on Unix that is an error of its
	// own ("not a directory"), not "nothing there". Windows reports the same path as not
	// found — which is what a missing store is, and it reads as one
	notDir := filepath.Join(home, "file", "sessions")
	sourcetest.Write(t, filepath.Join(home, "file"), "not a directory")
	refs, err := Spec.Open(notDir).Scan(context.Background())
	if runtime.GOOS == "windows" {
		if err != nil || len(refs) != 0 {
			t.Errorf("a path Windows does not find: %v, %v", refs, err)
		}
	} else if err == nil {
		t.Error("a store that cannot be read must not scan as empty")
	}
	// A file among the group directories is not a group
	root := filepath.Join(home, "sessions")
	sourcetest.Write(t, filepath.Join(root, ".DS_Store"), "x")
	sourcetest.Write(t, filepath.Join(root, "%2Fw", "sid", "summary.json"), `{"info":{"id":"sid"}}`)
	if refs, err := Spec.Open(root).Scan(context.Background()); err != nil || len(refs) != 1 {
		t.Errorf("refs = %v, err = %v", refs, err)
	}
	// A session that vanished between the scan and the read is an error, not an empty one
	src := Spec.Open(root)
	ref := scan(t, src)[filepath.Join(root, "%2Fw", "sid")]
	if err := os.RemoveAll(ref.Key); err != nil {
		t.Fatal(err)
	}
	if err := src.Read(context.Background(), ref, transcript.New(nil)); err == nil {
		t.Error("reading a session that is gone must fail")
	}
}

func TestLocate(t *testing.T) {
	if got := Spec.Locate(sourcetest.Env("/home/u")); got != filepath.Join("/home/u", ".grok", "sessions") {
		t.Errorf("default = %s", got)
	}
	// GROK_HOME moves the whole ~/.grok, and the sessions with it
	if got := Spec.Locate(sourcetest.Env("/home/u", "GROK_HOME", "/moved/grok-home")); got != filepath.Join("/moved/grok-home", "sessions") {
		t.Errorf("relocated = %s", got)
	}
	if Spec.Name != "grok" || Spec.Resume != "grok -r" || !Spec.Movable {
		t.Errorf("spec = %+v", Spec)
	}
}

// One directory nobody can look into is not the store nobody can: the rest is listed, and
// the error says the list is short — so that the sessions in the unreadable part are
// kept, not taken for deleted.
func TestOneUnreadableGroupDoesNotHideTheRest(t *testing.T) {
	root, _ := fixture(t)
	locked := filepath.Join(root, "%2Fwork%2Flocked")
	sourcetest.Write(t, filepath.Join(locked, "s-locked", "summary.json"), `{"info":{"id":"s-locked","cwd":"/work/locked"}}`)
	sourcetest.Unreadable(t, locked)
	refs, err := Spec.Open(root).Scan(context.Background())
	if err == nil {
		t.Error("a directory that could not be read went unreported")
	}
	if len(refs) != 1 {
		t.Errorf("refs = %d, want the one session that can be read", len(refs))
	}
}

func TestPromptWithMoreThanItsQuery(t *testing.T) {
	for in, want := range map[string]string{
		"<user_query>\nlist the files\n</user_query>":                                             "list the files",
		"<user_query>\nlist the files\n</user_query>\n<attached_files>\na.txt\n</attached_files>": "list the files",
		"<context>today</context>\n<user_query>list the files</user_query>":                       "list the files",
		"no wrapper at all":        "no wrapper at all",
		"<user_query>never closed": "<user_query>never closed",
	} {
		if got := userQuery(in); got != want {
			t.Errorf("userQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// The directory a session ran in is read from its group's .cwd when the summary names
// none, so that file is part of what a session is made of.
func TestTheGroupsCwdFileMovesTheStamp(t *testing.T) {
	root, dir := fixture(t)
	before := scan(t, Spec.Open(root))[dir].Stamp
	sourcetest.Write(t, filepath.Join(filepath.Dir(dir), ".cwd"), "/private/tmp/grok-probe")
	if after := scan(t, Spec.Open(root))[dir].Stamp; after == before {
		t.Error("the group's .cwd appeared and the session's stamp did not move")
	}
}
