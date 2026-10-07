package app

import (
	"context"
	"path/filepath"
	"testing"
)

// The fixtures below are the shapes a real Grok CLI 1.0.41 session writes: a JSON-RPC
// envelope per line carrying an epoch-seconds timestamp, with Grok's own events under
// _x.ai/session/update sharing the file with the Agent Client Protocol stream.
func grokFixture(t *testing.T) (root, summary string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "%2Fprivate%2Ftmp%2Fgrok-probe", "01a0cbfb-8d46-70c0-a73c-d53201e0e77c")
	summary = filepath.Join(dir, "summary.json")
	write(t, summary,
		`{"info":{"id":"01a0cbfb-8d46-70c0-a73c-d53201e0e77c","cwd":"/private/tmp/grok-probe"},`+
			`"session_summary":"read sample.txt","generated_title":"Reading sample.txt",`+
			`"created_at":"2026-09-23T01:55:34.929541Z","updated_at":"2026-09-23T01:55:39.711040Z",`+
			`"last_active_at":"2026-09-23T01:55:39.711040Z","num_messages":8,"num_chat_messages":6,`+
			`"current_model_id":"gpt-5.6-luna","session_kind":"headless"}`,
	)
	write(t, filepath.Join(dir, "updates.jsonl"),
		// Grok's own events are not transcript content and must not become messages
		`{"timestamp":1790128656,"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"hook_execution","event_name":"session_start","runs":[]}}}`,
		`{"timestamp":1790128656,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"read sample.txt"},"_meta":{"modelId":"gpt-5.6-luna","promptIndex":0}}}}`,
		`{"timestamp":1790128663,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"read_file","rawInput":{"target_file":"sample.txt"},"_meta":{"x.ai/tool":{"name":"read_file","kind":"read"}}}}}`,
		// A status-less update is the call being re-titled mid-flight: no output, no block
		`{"timestamp":1790128663,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","kind":"read","title":"Read sample.txt","locations":[{"path":"sample.txt"}]}}}`,
		`{"timestamp":1790128664,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"hello from fixture"}}],"rawOutput":{"type":"ReadFile"}}}}`,
		`{"timestamp":1790128665,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"checking the file"}}}}`,
		// Streamed text arrives in chunks and has to come back out as one block
		`{"timestamp":1790128666,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"The file says "}}}}`,
		`{"timestamp":1790128666,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello from fixture"}}}}`,
		`{"timestamp":1790128666,"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","stop_reason":"end_turn","usage":{"inputTokens":15667,"outputTokens":53,"totalTokens":31344,"cachedReadTokens":15624,"reasoningTokens":10,"modelCalls":2}}}}`,
	)
	return root, summary
}

func TestGrokSource(t *testing.T) {
	root, summary := grokFixture(t)
	s := newGrokSource(root)

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("list = %d records, want 1", len(list))
	}
	rec := list[0]
	if rec.SessionID != "01a0cbfb-8d46-70c0-a73c-d53201e0e77c" {
		t.Errorf("sessionId = %q", rec.SessionID)
	}
	// cwd drives project grouping, and Grok states it outright rather than only encoding
	// it into the directory name
	if rec.Cwd != "/private/tmp/grok-probe" || rec.project() != "/private/tmp/grok-probe" {
		t.Errorf("cwd = %q, project = %q", rec.Cwd, rec.project())
	}
	// Grok titles a session itself, so its title wins over the opening prompt
	if rec.ShortKey != "Reading sample.txt" {
		t.Errorf("shortKey = %q", rec.ShortKey)
	}
	if rec.UpdatedAt != "2026-09-23T01:55:39.711040Z" {
		t.Errorf("updatedAt = %q", rec.UpdatedAt)
	}
	if rec.Model != "gpt-5.6-luna" {
		t.Errorf("model = %q", rec.Model)
	}
	// The record points at the transcript, not at the index file the list is keyed on
	if filepath.Base(rec.File) != "updates.jsonl" || !rec.HasFile {
		t.Errorf("file = %q, hasFile = %v", rec.File, rec.HasFile)
	}

	msgs := s.Messages(rec, messageQuery{limit: 50})
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want one user and one assistant turn", len(msgs))
	}
	if msgs[0]["role"] != "user" || msgs[1]["role"] != "assistant" {
		t.Fatalf("roles = %v / %v", msgs[0]["role"], msgs[1]["role"])
	}
	// Epoch seconds come back out in the ISO shape every other source emits
	if msgs[0]["timestamp"] != "2026-09-23T01:57:36" {
		t.Errorf("timestamp = %v", msgs[0]["timestamp"])
	}

	parts := msgs[1]["content"].([]map[string]any)
	if len(parts) != 4 {
		t.Fatalf("assistant parts = %v", parts)
	}
	if parts[0]["type"] != "toolCall" || parts[0]["name"] != "read_file" {
		t.Errorf("parts[0] = %v", parts[0])
	}
	if args := parts[0]["arguments"].(map[string]any); args["target_file"] != "sample.txt" {
		t.Errorf("toolCall arguments = %v", args)
	}
	// The finished call becomes a result, labelled with the name its opening line carried
	if parts[1]["type"] != "toolResult" || parts[1]["toolName"] != "read_file" || parts[1]["content"] != "hello from fixture" {
		t.Errorf("parts[1] = %v", parts[1])
	}
	if parts[2]["type"] != "thinking" || parts[2]["content"] != "checking the file" {
		t.Errorf("parts[2] = %v", parts[2])
	}
	// Two chunks, one block
	if parts[3]["type"] != "text" || parts[3]["content"] != "The file says hello from fixture" {
		t.Errorf("parts[3] = %v", parts[3])
	}

	// The list's count and the opened session must agree, so they run the same grouping
	if n, _ := grokCountMessages(summary, 0); n != 2 {
		t.Errorf("grokCountMessages = %d, want 2", n)
	}
	// The grouper joins several update rows into one message, so it must refuse to
	// resume mid-file rather than split a group across the seam
	if _, resumed := grokCountMessages(summary, 1); resumed {
		t.Error("grokCountMessages claimed it can resume at an offset")
	}

	final := s.Final(rec)
	if final["messageCount"] != 2 || final["isFinal"] != true || final["stopReason"] != "end_turn" {
		t.Fatalf("final = %v", final)
	}
	if final["text"] != "The file says hello from fixture" || final["thinking"] != "checking the file" {
		t.Errorf("final text = %q, thinking = %q", final["text"], final["thinking"])
	}
	if final["model"] != "gpt-5.6-luna" {
		t.Errorf("final model = %v", final["model"])
	}
	if tcs := final["toolCalls"].([]any); len(tcs) != 1 || tcs[0].(map[string]any)["name"] != "read_file" {
		t.Errorf("final toolCalls = %v", final["toolCalls"])
	}
	// turn_completed is Grok's own event, so usage only arrives if the non-ACP lines are
	// read too; cachedReadTokens is Grok's spelling of the shared cacheReadTokens
	usage := final["usage"].(map[string]any)
	if usage["inputTokens"] != int64(15667) || usage["outputTokens"] != int64(53) {
		t.Errorf("usage = %v", usage)
	}
	if usage["cacheReadTokens"] != int64(15624) || usage["reasoningTokens"] != int64(10) {
		t.Errorf("usage = %v", usage)
	}
	if _, leaked := usage["modelCalls"]; leaked {
		t.Errorf("usage must drop fields the card has no use for: %v", usage)
	}
}

// A window from either end has to land on whole messages, and asking for the first one
// must stop the scan without losing the message being assembled.
func TestGrokMessageWindows(t *testing.T) {
	root, _ := grokFixture(t)
	s := newGrokSource(root)
	rec := s.List()[0]

	first := s.Messages(rec, messageQuery{limit: 1})
	if len(first) != 1 || first[0]["role"] != "user" {
		t.Fatalf("earliest 1 = %v", first)
	}
	last := s.Messages(rec, messageQuery{limit: 1, fromEnd: true})
	if len(last) != 1 || last[0]["role"] != "assistant" {
		t.Fatalf("latest 1 = %v", last)
	}
}

// Grok names the group directory after the working directory, URL-encoded, and swaps that
// for a slug plus a hash once the encoded form passes 255 bytes — recording the real path
// in a .cwd file. Without both fallbacks such a session would group under no project.
func TestGrokCwdFallbacks(t *testing.T) {
	root := t.TempDir()
	// A plus in a directory name survives: PathUnescape leaves it alone where
	// QueryUnescape would turn it into a space
	encoded := filepath.Join(root, "%2FUsers%2Fdev%2Fsome+project", "sid-1")
	write(t, filepath.Join(encoded, "summary.json"), `{"info":{"id":"sid-1"}}`)

	hashed := filepath.Join(root, "some-long-project-a1b2c3", "sid-2")
	write(t, filepath.Join(hashed, "summary.json"), `{"info":{"id":"sid-2"}}`)
	write(t, filepath.Join(root, "some-long-project-a1b2c3", ".cwd"), "/Users/dev/some/very/long/path")

	byID := map[string]record{}
	for _, rec := range newGrokSource(root).List() {
		byID[rec.SessionID] = rec
	}
	if got := byID["sid-1"].Cwd; got != "/Users/dev/some+project" {
		t.Errorf("decoded cwd = %q", got)
	}
	if got := byID["sid-2"].Cwd; got != "/Users/dev/some/very/long/path" {
		t.Errorf(".cwd cwd = %q", got)
	}
}

// Grok writes summary.json when the session is created, before anything is said. Such a
// session still belongs in the list, but it has no transcript to open.
func TestGrokSessionWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "%2Ftmp%2Fempty", "sid-new", "summary.json"),
		`{"info":{"id":"sid-new","cwd":"/tmp/empty"},"created_at":"2026-09-23T01:32:09.978229Z","updated_at":"2026-09-23T01:32:09.984705Z","num_messages":0,"current_model_id":"grok-4.6"}`)

	s := newGrokSource(root)
	list := s.List()
	if len(list) != 1 || list[0].HasFile {
		t.Fatalf("a created-but-unprompted session must be listed with no file: %v", list)
	}
	if list[0].ShortKey != "sid-new" {
		t.Errorf("shortKey = %q, want the session id as the last fallback", list[0].ShortKey)
	}
	if final := s.Final(list[0]); final != nil {
		t.Errorf("Final must report a missing transcript as nil, got %v", final)
	}
	if msgs := s.Messages(list[0], messageQuery{limit: 10}); len(msgs) != 0 {
		t.Errorf("messages = %v", msgs)
	}
}

// A search hit has to name its speaker, or every role filter quietly excludes the whole
// source: search_sessions with role=, and find_decisions / find_similar_question, which
// ask for assistant hits and would never surface a Grok session at all.
func TestGrokSearchHitsCarryARole(t *testing.T) {
	root, _ := grokFixture(t)
	rec := newGrokSource(root).List()[0]
	ctx := context.Background()

	all := searchFile(ctx, rec.File, searchQuery{
		needle: "sample.txt", lowered: []byte("sample.txt"), perSession: 10,
	})
	if len(all) == 0 {
		t.Fatal("the fixture should match")
	}
	for _, hit := range all {
		if toStr(hit["role"]) == "" {
			t.Errorf("a hit with no role is invisible to every role filter: %v", hit)
		}
	}

	roles := map[string]int{}
	for _, want := range []string{"user", "assistant"} {
		hits := searchFile(ctx, rec.File, searchQuery{
			needle: "sample.txt", lowered: []byte("sample.txt"), perSession: 10, role: want,
		})
		roles[want] = len(hits)
	}
	if roles["user"] == 0 || roles["assistant"] == 0 {
		t.Errorf("both speakers match this needle, got user=%d assistant=%d", roles["user"], roles["assistant"])
	}

	// Grok writes epoch seconds; a hit reports the time the way every other source does
	if ts := toStr(all[0]["timestamp"]); ts != "2026-09-23T01:57:36" {
		t.Errorf("hit timestamp = %q, want the ISO form", ts)
	}
}

// A prompt arrives in chunks, so a title taken from the first row that yields text stops
// wherever chunk one happened to end. Only reached before Grok writes a title of its own,
// which is exactly the new sessions sitting at the top of the list.
func TestGrokTitleJoinsChunkedPrompt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "%2Ftmp%2Fchunked", "sid-chunked")
	write(t, filepath.Join(dir, "summary.json"), `{"info":{"id":"sid-chunked","cwd":"/tmp/chunked"}}`)
	write(t, filepath.Join(dir, "updates.jsonl"),
		`{"timestamp":1790128000,"method":"session/update","params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"refactor the "}}}}`,
		`{"timestamp":1790128000,"method":"session/update","params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"parser to handle OSC"}}}}`,
		`{"timestamp":1790128009,"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"on it"}}}}`,
	)
	rec := newGrokSource(root).List()[0]
	if got := rec.ShortKey; got != "refactor the parser to handle OSC" {
		t.Errorf("shortKey = %q, want the whole prompt", got)
	}
}

// TestGrokArchivedSessionsAndFailedTool: Grok moves an archived session from
// ~/.grok/sessions to ~/.grok/archived_sessions beside it, and the list used to lose it
// there. A tool call that ends with status failed is a failure the result records.
func TestGrokArchivedSessionsAndFailedTool(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "sessions")
	live := filepath.Join(root, "%2Fw%2Flive", "11111111-1111-7111-8111-111111111111")
	write(t, filepath.Join(live, "summary.json"),
		`{"info":{"id":"11111111-1111-7111-8111-111111111111","cwd":"/w/live"},"generated_title":"Live one","last_active_at":"2026-10-01T10:00:00Z"}`)
	write(t, filepath.Join(live, "updates.jsonl"),
		`{"timestamp":1790000000,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hello"}}}}`)
	archived := filepath.Join(home, "archived_sessions", "%2Fw%2Fold", "22222222-2222-7222-8222-222222222222")
	write(t, filepath.Join(archived, "summary.json"),
		`{"info":{"id":"22222222-2222-7222-8222-222222222222","cwd":"/w/old"},"generated_title":"Archived one","last_active_at":"2026-09-01T10:00:00Z"}`)
	write(t, filepath.Join(archived, "updates.jsonl"),
		`{"timestamp":1789000000,"method":"session/update","params":{"sessionId":"s2","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"run the build"}}}}`,
		`{"timestamp":1789000001,"method":"session/update","params":{"sessionId":"s2","update":{"sessionUpdate":"tool_call","toolCallId":"call_9","title":"run_command","rawInput":{"command":"make"},"_meta":{"x.ai/tool":{"name":"run_command","kind":"execute"}}}}}`,
		`{"timestamp":1789000002,"method":"session/update","params":{"sessionId":"s2","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_9","status":"failed","content":[{"type":"content","content":{"type":"text","text":"make: *** No rule"}}]}}}`,
	)

	s := newGrokSource(root)
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("list = %d records, want the live and the archived session", len(list))
	}
	var old record
	for _, rec := range list {
		switch rec.ShortKey {
		case "Live one":
			if rec.Archived {
				t.Error("the live session must not be marked archived")
			}
		case "Archived one":
			old = rec
			if !rec.Archived {
				t.Error("the archived session must say so")
			}
		}
	}
	msgs := s.Messages(old, messageQuery{limit: 10})
	parts := msgs[1]["content"].([]map[string]any)
	if len(parts) != 2 || parts[0]["id"] != "call_9" || parts[1]["callId"] != "call_9" || parts[1]["status"] != statusError || parts[1]["toolName"] != "run_command" {
		t.Fatalf("archived transcript = %v", parts)
	}

	// A relocated root has no archived_sessions beside it to look at
	if newGrokSource(t.TempDir()).archivedRoot() != "" {
		t.Error("a root not named sessions must not invent a sibling")
	}
}
