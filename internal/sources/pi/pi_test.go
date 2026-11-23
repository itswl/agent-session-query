package pi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

type M = map[string]any

func header(id, cwd string) M {
	return M{"type": "session", "version": 3, "id": id, "timestamp": "2026-10-06T18:39:00.000Z", "cwd": cwd}
}

// msgRow is a message row: written at rowTime, holding a message that carries its own time.
func msgRow(id, rowTime string, msg M) M {
	return M{"type": "message", "id": id, "parentId": nil, "timestamp": rowTime, "message": msg}
}

func text(s string) []M { return []M{{"type": "text", "text": s}} }

// ms is a message's own time, as pi writes it: epoch milliseconds
func ms(offset time.Duration) int64 {
	return time.Date(2026, 10, 6, 18, 40, 0, 0, time.UTC).Add(offset).UnixMilli()
}

func TestSession(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "proj", "2026-09-13T13-04-12-594Z_aaaa.jsonl"),
		header("pi-1", "/home/u/proj"),
		M{"type": "model_change", "id": "mc1", "parentId": "x", "timestamp": "2026-10-06T18:39:01.000Z", "provider": "p", "modelId": "gemini-3.8-flash"},
		M{"type": "thinking_level_change", "id": "tl1", "timestamp": "2026-10-06T18:39:02.000Z", "thinkingLevel": "high"},
		msgRow("m1", "2026-10-06T18:40:00.002Z", M{"role": "user", "timestamp": ms(0), "content": text("hello")}),
		msgRow("m2", "2026-10-06T18:40:05.100Z", M{
			"role": "assistant", "timestamp": ms(4 * time.Second), "model": "gemini-3.8-flash", "provider": "p",
			"stopReason": "stop", "durationMs": 1200, "responseId": "resp_1",
			"usage": M{"input": 100, "output": 50, "cacheRead": 300, "cacheWrite": 20, "reasoning": 30, "totalTokens": 470,
				"cost": M{"input": 0.001, "output": 0.002, "total": 0.0123}},
			"content": []M{{"type": "thinking", "thinking": "pondering", "thinkingSignature": "sig"}, {"type": "text", "text": "hi there"}},
		}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)

	if s.Session.ID != "pi-1" || s.Session.Cwd != "/home/u/proj" || s.Session.Model != "gemini-3.8-flash" {
		t.Errorf("session = %+v", s.Session)
	}
	if s.Session.Title != "hello" || s.Session.Version != "" {
		// The header's version is the session format's (3), not the CLI's
		t.Errorf("title %q, version %q", s.Session.Title, s.Session.Version)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt thinking reply" {
		t.Errorf("items = %s", got)
	}
	if turn.Ask != "hello" || turn.Answer != "hi there" {
		t.Errorf("ask %q answer %q", turn.Ask, turn.Answer)
	}
	if len(turn.Calls) != 1 {
		t.Fatalf("calls = %+v", turn.Calls)
	}
	// pi counts the cache apart from the input already, and the output with the reasoning
	want := model.Usage{Input: 100, Output: 50, CacheRead: 300, CacheWrite: 20, Reasoning: 30}
	call := turn.Calls[0]
	if call.Usage == nil || *call.Usage != want || call.CostUSD != 0.0123 || call.DurMs != 1200 || call.Stop != "stop" || call.Model != "gemini-3.8-flash" {
		t.Errorf("call = %+v", call)
	}
	if s.Stats.Usage != want || s.Stats.CostUSD != 0.0123 {
		t.Errorf("stats = %+v", s.Stats)
	}
}

// TestFlatSessionDir: --session-dir stores the <time>_<uuid>.jsonl files flat in the
// directory it was given — the convention a host or wrapper embedding pi uses — while the
// interactive default nests one directory per project. A root holding the flat shape must
// list its sessions all the same.
func TestFlatSessionDir(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "2026-10-14T08-10-05-693Z_01a13976-21f3-7461-a370-8f4009a9589b.jsonl"),
		M{"type": "session", "version": 3, "id": "01a13976-21f3-7461-a370-8f4009a9589b", "cwd": "/w/hosted"},
		msgRow("m1", "2026-10-14T08:10:06Z", M{"role": "user", "timestamp": 1791312000000, "content": text("run")}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "01a13976-21f3-7461-a370-8f4009a9589b" || s.Session.Cwd != "/w/hosted" || s.Turn(t, 1).Ask != "run" {
		t.Errorf("session = %+v", s.Session)
	}
}

// TestSessionRowNotFirst: when the session row is not the first line, another row's id must
// not become the session id. The original code took the first line's id unconditionally,
// and a model_change record carries its own id — measured locally, this really happened.
func TestSessionRowNotFirst(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "proj", "2026-09-04T09-05-45-921Z_01a06baa-real-uuid.jsonl"),
		M{"type": "model_change", "id": "e74f2cff", "provider": "newapi", "modelId": "m"},
		M{"type": "model_change", "id": "deadbeef", "provider": "newapi", "modelId": "m2"},
		header("pi-real", "/w/real"),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "pi-real" {
		t.Errorf("id = %q; must not take model_change's id", s.Session.ID)
	}
	if s.Session.Cwd != "/w/real" {
		t.Errorf("cwd = %q", s.Session.Cwd)
	}
}

// TestNoSessionRow: with no session row at all (a truncated or resumed file), the id is the
// uuid in the filename rather than some event's id
func TestNoSessionRow(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "proj", "2026-09-04T09-05-45-921Z_01a06baa-b9c1.jsonl"),
		M{"type": "model_change", "id": "e74f2cff", "provider": "newapi"},
		msgRow("ff24cc88", "2026-09-04T09:05:50Z", M{"role": "user", "content": text("hi")}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "01a06baa-b9c1" {
		t.Errorf("id = %q; should fall back to the uuid in the filename", s.Session.ID)
	}
	if s.Session.Cwd != "" {
		t.Errorf("no session row should mean no cwd, got %q", s.Session.Cwd)
	}
}

func TestIDFromStem(t *testing.T) {
	cases := map[string]string{
		"2026-09-04T09-05-45-921Z_01a06baa-b9c1": "01a06baa-b9c1",
		"no-underscore-here":                     "no-underscore-here",
		"trailing_":                              "trailing_",
	}
	for in, want := range cases {
		if got := idOf(in); got != want {
			t.Errorf("idOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestToolResultPairing: a Pi tool call is a block on the assistant message and its result
// is a message of its own with role toolResult, toolCallId, toolName and isError riding on
// the message. Measured locally: 554 such pairs in the newest 40 sessions, 49 of them
// failures.
func TestToolResultPairing(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "proj", "2026-10-01T10-00-00_pppp.jsonl"),
		header("pppp", "/w"),
		msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "content": []M{{"type": "image", "data": "iVBORw0KGgo="}, {"type": "text", "text": "run it"}}, "timestamp": ms(0)}),
		msgRow("m2", "2026-10-06T18:40:01Z", M{"role": "assistant", "stopReason": "toolUse", "timestamp": ms(time.Second), "durationMs": 800,
			"content": []M{
				{"type": "toolCall", "id": "c1", "name": "bash", "arguments": M{"command": "pwd"}},
				{"type": "toolCall", "id": "c2", "name": "read", "arguments": M{"path": "/w/a.go"}},
			}}),
		msgRow("m3", "2026-10-06T18:40:03Z", M{"role": "toolResult", "toolCallId": "c1", "toolName": "bash", "isError": true, "durationMs": 120,
			"timestamp": ms(2 * time.Second), "content": text("boom")}),
		// an image has no text: the blocks that do are the output, and the image is marked
		// after them
		msgRow("m4", "2026-10-06T18:40:03Z", M{"role": "toolResult", "toolCallId": "c2", "toolName": "read", "isError": false,
			"timestamp": ms(2 * time.Second), "content": []M{{"type": "text", "text": "line 1"}, {"type": "image", "data": "AAAA", "mimeType": "image/png"}, {"type": "text", "text": "line 2"}}}),
		msgRow("m5", "2026-10-06T18:40:04Z", M{"role": "assistant", "stopReason": "stop", "timestamp": ms(3 * time.Second), "content": text("it failed")}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt tool tool reply" {
		t.Errorf("items = %s", got)
	}
	// A picture pasted with the words is marked after them, as what its bytes say it is
	if turn.Ask != "run it\n[image · png · 8 bytes]" || len(turn.Items[0].Attachments) != 1 {
		t.Errorf("ask %q, %+v", turn.Ask, turn.Items[0].Attachments)
	}
	tools := sourcetest.Tools(turn)
	if len(tools) != 2 {
		t.Fatalf("tools = %+v", tools)
	}
	bash, read := tools[0], tools[1]
	if bash.Name != "bash" || bash.Kind != model.ToolExec || bash.CallID != "c1" || bash.Command != "pwd" {
		t.Errorf("call = %+v", bash)
	}
	if !bash.Done || bash.Status != model.StatusError || bash.Output != "boom" || bash.DurMs != 120 {
		t.Errorf("result = %+v", bash)
	}
	if read.Status != model.StatusOK || read.Output != "line 1\nline 2\n[image · png · 3 bytes]" || len(read.Files) != 1 || read.Files[0] != "/w/a.go" {
		t.Errorf("read = %+v", read)
	}
	if turn.Tools != 2 || turn.Failures != 1 {
		t.Errorf("tools %d, failures %d", turn.Tools, turn.Failures)
	}
}

// pi's tools take a path relative to the session's directory as well as a whole one: a
// file named either way is named by its whole path.
func TestRelativePathsAreNamedWhole(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "proj", "2026-10-01T10-00-00_rrrr.jsonl"),
		header("rrrr", "/w/api"),
		msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "content": text("fix the handler"), "timestamp": ms(0)}),
		msgRow("m2", "2026-10-06T18:40:01Z", M{"role": "assistant", "stopReason": "toolUse", "timestamp": ms(time.Second),
			"content": []M{
				{"type": "toolCall", "id": "c1", "name": "edit", "arguments": M{"path": "internal/handler.go", "oldText": "a", "newText": "b"}},
				{"type": "toolCall", "id": "c2", "name": "read", "arguments": M{"path": "/etc/hosts"}},
			}}),
		msgRow("m3", "2026-10-06T18:40:02Z", M{"role": "toolResult", "toolCallId": "c1", "toolName": "edit", "isError": false, "timestamp": ms(2 * time.Second), "content": text("Edited")}),
		msgRow("m4", "2026-10-06T18:40:02Z", M{"role": "toolResult", "toolCallId": "c2", "toolName": "read", "isError": false, "timestamp": ms(2 * time.Second), "content": text("127.0.0.1")}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	want := []model.File{{Path: "/w/api/internal/handler.go", Changed: true}, {Path: "/etc/hosts"}}
	if got := s.Turn(t, 1).Files; !reflect.DeepEqual(got, want) {
		t.Errorf("files = %+v, want %+v", got, want)
	}
}

// A failed command's exit code is the line pi's shell tool ends its result with; a command
// that passed has none, and nor has one that timed out or was aborted, which pi says in
// other words. The calls a code-mode script made are calls of their own, each with how it
// ended: one that failed inside a script is a failure, one still running when the script
// finished has no result, and a status pi had not written when this was made is counted.
func TestExitCodesAndNestedCalls(t *testing.T) {
	root := t.TempDir()
	result := func(id, call, name string, isError bool, out string, extra ...M) M {
		msg := M{"role": "toolResult", "toolCallId": call, "toolName": name, "isError": isError, "timestamp": ms(2 * time.Second), "content": text(out)}
		for _, e := range extra {
			for k, v := range e {
				msg[k] = v
			}
		}
		return msgRow(id, "2026-10-06T18:40:02Z", msg)
	}
	sourcetest.Write(t, filepath.Join(root, "proj", "2026-10-01T10-00-00_eeee.jsonl"),
		header("eeee", "/w/api"),
		msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "content": text("run the tests"), "timestamp": ms(0)}),
		msgRow("m2", "2026-10-06T18:40:01Z", M{"role": "assistant", "stopReason": "toolUse", "timestamp": ms(time.Second),
			"content": []M{
				{"type": "toolCall", "id": "c1", "name": "bash", "arguments": M{"command": "go test ./..."}},
				{"type": "toolCall", "id": "c2", "name": "bash", "arguments": M{"command": "sleep 99"}},
				{"type": "toolCall", "id": "c3", "name": "bash", "arguments": M{"command": "go vet ./..."}},
				{"type": "toolCall", "id": "c4", "name": "codemode", "arguments": M{"code": "await read('a.go'); await bash('make')"}},
			}}),
		result("r1", "c1", "bash", true, "--- FAIL: TestOrder\nFAIL\n\nCommand exited with code 1"),
		result("r2", "c2", "bash", true, "Command timed out after 5 seconds"),
		result("r3", "c3", "bash", false, "(no output)"),
		result("r4", "c4", "codemode", false, "done", M{"nestedCalls": M{"complete": false, "calls": []M{
			{"id": "n1", "name": "read", "arguments": M{"path": "a.go"}, "status": "ok", "durationMs": 3},
			{"id": "n2", "name": "bash", "arguments": M{"command": "make"}, "status": "error", "durationMs": 1500, "error": "make: *** [build] Error 2"},
			{"id": "n3", "name": "bash", "argumentsBytes": 90000, "status": "unfinished"},
			{"id": "n4", "name": "read", "arguments": M{"path": "b.go"}, "status": "paused"},
		}}}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	if fmt.Sprint(s.Unread) != "map[nestedCall:paused:1]" {
		t.Errorf("unread = %v", s.Unread)
	}
	turn := s.Turn(t, 1)
	tools := sourcetest.Tools(turn)
	if len(tools) != 8 || turn.Failures != 3 {
		t.Fatalf("%d tools, %d failures: %+v", len(tools), turn.Failures, tools)
	}
	code := func(tool *model.Tool) string {
		if tool.Exit == nil {
			return "none"
		}
		return strconv.Itoa(*tool.Exit)
	}
	for i, want := range []string{"1", "none", "none", "none", "none", "none", "none", "none"} {
		if got := code(tools[i]); got != want {
			t.Errorf("%s %s: exit %s, want %s", tools[i].Name, tools[i].Command, got, want)
		}
	}
	read, make, unfinished, unknown := tools[4], tools[5], tools[6], tools[7]
	if read.Name != "read" || read.Status != model.StatusOK || read.DurMs != 3 || fmt.Sprint(read.Files) != "[/w/api/a.go]" {
		t.Errorf("nested read = %+v", read)
	}
	if make.Command != "make" || make.Status != model.StatusError || make.Output != "make: *** [build] Error 2" || make.DurMs != 1500 {
		t.Errorf("nested make = %+v", make)
	}
	if unfinished.Done || unfinished.Input != nil || unknown.Done {
		t.Errorf("unfinished %+v, unknown %+v: no result", unfinished, unknown)
	}
}

// TestResultWithoutItsCall: a result names its tool itself, so one whose call is not in the
// transcript is still a tool item with the right name
func TestResultWithoutItsCall(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "2026-10-01T10-00-00_oooo.jsonl"),
		msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "content": text("go")}),
		msgRow("m2", "2026-10-06T18:40:01Z", M{"role": "toolResult", "toolCallId": "gone", "toolName": "read", "isError": false, "content": text("contents")}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	tools := sourcetest.Tools(s.Turn(t, 1))
	if len(tools) != 1 || tools[0].Name != "read" || tools[0].Output != "contents" || tools[0].Status != model.StatusOK {
		t.Errorf("tools = %+v", tools)
	}
}

// TestMessageTime: a row is written up to minutes after the message in it was created, and
// the message's own time is message.timestamp — epoch milliseconds, exact to the
// millisecond. The row's time is only the fallback. Measured over six sessions, the row
// trailed the message by 1 ms at the median, 11 s at p90 and six minutes at worst; a time
// taken from the row put a message after what it was found next to.
func TestMessageTime(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "proj", "2026-10-06T18-40-00-000Z_bbbb.jsonl"),
		header("pi-time", "/w"),
		msgRow("m1", "2026-10-06T18:40:00.400Z", M{"role": "user", "timestamp": 1791312000000, "content": text("set up the reverse proxy")}),
		// written eleven seconds after the message was created
		msgRow("m2", "2026-10-06T18:40:20.500Z", M{"role": "assistant", "timestamp": 1791312009500, "content": text("use proxy_pass for that")}),
		// a message with no time of its own takes its row's
		msgRow("m3", "2026-10-06T18:41:00.250Z", M{"role": "user", "content": text("thanks")}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	items := s.Items()
	if len(items) != 3 {
		t.Fatalf("items = %+v", items)
	}
	for i, want := range []time.Time{
		time.Date(2026, 10, 6, 18, 40, 0, 0, time.UTC),
		time.Date(2026, 10, 6, 18, 40, 9, 500_000_000, time.UTC),
		time.Date(2026, 10, 6, 18, 41, 0, 250_000_000, time.UTC),
	} {
		if !items[i].At.Equal(want) {
			t.Errorf("item %d at %s, want %s", i, items[i].At.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
		}
	}
}

// TestAbortedResponse: a response the user stopped is an interruption, and — empty as it is
// — still a call pi measured
func TestAbortedResponse(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "2026-10-01T10-00-00_iiii.jsonl"),
		msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "timestamp": ms(0), "content": text("write a long essay")}),
		msgRow("m2", "2026-10-06T18:40:06Z", M{"role": "assistant", "timestamp": ms(time.Second), "model": "m", "stopReason": "aborted", "errorMessage": "Request was aborted",
			"durationMs": 5384, "usage": M{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0}, "content": []M{}}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if !turn.Interrupted || sourcetest.Kinds(turn) != "prompt event" || turn.Items[1].Event != model.EventInterrupted || turn.Items[1].Text != "Request was aborted" {
		t.Errorf("turn = %+v", turn)
	}
	if len(turn.Calls) != 1 || turn.Calls[0].DurMs != 5384 || turn.Calls[0].Stop != "aborted" || turn.Calls[0].Usage != nil {
		t.Errorf("calls = %+v", turn.Calls)
	}
}

// TestUserContentAsString: a user message's content is a block array or a plain string
func TestUserContentAsString(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "2026-10-01T10-00-00_ssss.jsonl"),
		msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "content": "plain words"}),
		// a system message pi writes keeps its prompt in structured sections, not in content
		msgRow("m0", "2026-10-06T18:40:00Z", M{"role": "system", "content": "", "sections": M{"cwd": "/w"}}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Stats.Turns != 1 || s.Turn(t, 1).Ask != "plain words" {
		t.Errorf("turns = %+v", s.Turns)
	}
	if len(s.Turns) != 1 {
		t.Errorf("an empty system message is no context, and opens no turn 0: %+v", s.Turns)
	}
}

// TestWhatPiKeepsForItself: the prompt and tool loadout pi writes as a system message at
// the head of every session is what it rebuilds its state from, not conversation; what an
// extension, the person's own shell or a compaction put in front of the model is context
// and events, and none of it is anyone's ask.
func TestWhatPiKeepsForItself(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "2026-10-01T10-00-00_ctxx.jsonl"),
		header("ctxx", "/w"),
		msgRow("m0", "2026-10-06T18:40:00Z", M{"role": "system", "content": "Be brief."}),
		M{"type": "model_change", "id": "mc", "timestamp": "2026-10-06T18:40:00Z", "modelId": "m"},
		msgRow("m1", "2026-10-06T18:40:01Z", M{"role": "user", "content": text("go")}),
		msgRow("m2", "2026-10-06T18:40:02Z", M{"role": "bashExecution", "command": "ls", "output": "a.txt", "exitCode": 2}),
		M{"type": "custom_message", "id": "cm", "timestamp": "2026-10-06T18:40:03Z", "content": "an extension's note"},
		M{"type": "compaction", "id": "cp", "timestamp": "2026-10-06T18:40:04Z", "summary": "what came before",
			"usage": M{"input": 100, "output": 20, "cacheRead": 0, "cacheWrite": 0}},
		M{"type": "branch_summary", "id": "bs", "timestamp": "2026-10-06T18:40:05Z", "summary": "the branch left behind"},
		M{"type": "session_info", "id": "si", "timestamp": "2026-10-06T18:40:06Z", "name": "Release prep"},
		// The rest pi keeps for itself, each of a kind the adapter knows and passes over
		M{"type": "thinking_level_change", "id": "tl", "timestamp": "2026-10-06T18:40:07Z", "thinkingLevel": "high"},
		M{"type": "label", "id": "lb", "timestamp": "2026-10-06T18:40:07Z", "targetId": "m1", "label": "start"},
		M{"type": "custom", "id": "cu", "timestamp": "2026-10-06T18:40:07Z", "customType": "todo", "data": M{"open": 2}},
		M{"type": "context_edit", "id": "ce", "timestamp": "2026-10-06T18:40:07Z", "targetId": "m2", "replacement": nil},
		M{"type": "usage", "id": "us", "timestamp": "2026-10-06T18:40:07Z", "kind": "cache_warm", "provider": "p", "model": "m",
			"usage": M{"input": 0, "output": 1, "cacheRead": 5000, "cacheWrite": 0, "cost": M{"total": 0.001}}},
		// what pi makes of a compaction when it builds the model's context, should a host
		// write it out: the compaction row above is the event
		msgRow("m3", "2026-10-06T18:40:07Z", M{"role": "compactionSummary", "summary": "what came before", "tokensBefore": 900}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if len(s.Turns) != 1 || s.Stats.Turns != 1 {
		t.Fatalf("turns = %+v: the system message opens no turn 0", s.Turns)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt context context event event" {
		t.Errorf("items = %s", got)
	}
	if shell := turn.Items[1].Text; !strings.Contains(shell, "Ran `ls`") || !strings.Contains(shell, "exited with code 2") {
		t.Errorf("the person's shell command = %q", shell)
	}
	if turn.Items[3].Event != model.EventCompaction || turn.Items[3].Text != "what came before" || turn.Items[4].Event != "branch_summary" {
		t.Errorf("events = %+v", turn.Items[3:])
	}
	// What a compaction's summary cost is model work pi bills, and is counted
	if s.Stats.Usage != (model.Usage{Input: 100, Output: 20}) {
		t.Errorf("usage = %+v", s.Stats.Usage)
	}
	if s.Session.Title != "Release prep" {
		t.Errorf("title = %q, want the name /name gave it", s.Session.Title)
	}
}

// TestCountsThatCannotBe: a counter below zero, or past what any response counts, is
// damage and is read as none — not cut down to the bound, which would keep a number that
// was never a count; the counters beside it in the same response are kept. A cost or a
// duration is read the same way.
func TestCountsThatCannotBe(t *testing.T) {
	root := t.TempDir()
	huge := int64(5_000_000_000_000_000_000)
	sourcetest.Write(t, filepath.Join(root, "p", "2026-10-01T10-00-00_cnts.jsonl"),
		msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "content": text("go")}),
		msgRow("m2", "2026-10-06T18:40:01Z", M{"role": "assistant", "content": text("a"), "durationMs": -5,
			"usage": M{"input": -100, "output": 1, "cacheRead": 0, "cacheWrite": 0, "cost": M{"total": -1}}}),
		msgRow("m3", "2026-10-06T18:40:02Z", M{"role": "assistant", "content": text("b"), "durationMs": 1e300,
			"usage": M{"input": huge, "output": 2, "cacheRead": 0, "cacheWrite": 0, "cost": M{"total": 0.5}}}),
		M{"type": "compaction", "id": "cp", "timestamp": "2026-10-06T18:40:03Z", "summary": "s",
			"usage": M{"input": huge, "output": 3, "cacheRead": 0, "cacheWrite": 0}},
		// One past what any response counts is dropped, not cut to the bound; the bound
		// itself is a count. A cost past what any response has cost is none.
		msgRow("m4", "2026-10-06T18:40:04Z", M{"role": "assistant", "content": text("c"),
			"usage": M{"input": model.MaxTokens + 1, "output": 4, "cacheRead": model.MaxTokens, "cacheWrite": 0, "cost": M{"total": 2e9}}}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)
	if s.Stats.Usage != (model.Usage{Output: 10, CacheRead: model.MaxTokens}) || s.Stats.CostUSD != 0.5 {
		t.Errorf("stats = %+v", s.Stats)
	}
	for _, call := range s.Turn(t, 1).Calls {
		if call.DurMs != 0 {
			t.Errorf("a duration below zero or past any response's: %+v", call)
		}
	}
}

// TestExtensionMessagesOfOlderFiles: before the custom_message row, what an extension put
// in front of the model was a message of its own — role "hookMessage" until the format's
// version 3, "custom" in the files pi migrated. Both are context, and both are known.
func TestExtensionMessagesOfOlderFiles(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "2026-01-01T10-00-00_olds.jsonl"),
		M{"type": "session", "version": 2, "id": "olds", "timestamp": "2026-01-01T10:00:00Z", "cwd": "/w"},
		msgRow("m1", "2026-01-01T10:00:01Z", M{"role": "user", "content": text("go")}),
		msgRow("m2", "2026-01-01T10:00:02Z", M{"role": "hookMessage", "customType": "note", "display": true, "content": "from a hook"}),
		msgRow("m3", "2026-01-01T10:00:03Z", M{"role": "custom", "customType": "note", "display": true, "content": text("from an extension")}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt context context" {
		t.Errorf("items = %s", got)
	}
	if turn.Items[1].Text != "from a hook" || turn.Items[2].Text != "from an extension" {
		t.Errorf("items = %+v", turn.Items)
	}
}

// TestWhatIsNotKnownIsCounted: a row, a role or a block of a kind pi had not written when
// the adapter was made is counted under its name, which is how asq doctor learns that
// pi's format has moved on. The rest of the session reads as it would without them.
func TestWhatIsNotKnownIsCounted(t *testing.T) {
	known := []any{
		header("unkn", "/w"),
		msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "timestamp": ms(0), "content": text("go")}),
		msgRow("m2", "2026-10-06T18:40:02Z", M{"role": "assistant", "timestamp": ms(time.Second), "model": "m", "stopReason": "stop",
			"usage":   M{"input": 10, "output": 5, "cacheRead": 0, "cacheWrite": 0, "cost": M{"total": 0.01}},
			"content": text("done")}),
	}
	read := func(lines ...any) sourcetest.Session {
		root := t.TempDir()
		sourcetest.Write(t, filepath.Join(root, "p", "2026-10-06T18-40-00_unkn.jsonl"), lines...)
		return sourcetest.One(t, Spec.Open(root))
	}
	before := read(known...)
	after := read(
		known[0],
		M{"type": "checkpoint", "id": "c1", "timestamp": "2026-10-06T18:40:00Z", "ref": "abc"},
		M{"id": "c2", "timestamp": "2026-10-06T18:40:00Z"}, // a row that does not say what it is
		known[1],
		// a role pi has not written, holding no content: nothing of it can be read
		msgRow("m9", "2026-10-06T18:40:01Z", M{"role": "artifact", "timestamp": ms(500 * time.Millisecond), "title": "a chart"}),
		msgRow("m2", "2026-10-06T18:40:02Z", M{"role": "assistant", "timestamp": ms(time.Second), "model": "m", "stopReason": "stop",
			"usage": M{"input": 10, "output": 5, "cacheRead": 0, "cacheWrite": 0, "cost": M{"total": 0.01}},
			// a block that does not say what it is is counted; a null, which is no block, is not
			"content": []any{M{"type": "citation", "url": "https://example.com"}, M{}, nil, M{"type": "text", "text": "done"}}}),
	)
	want := map[string]int{"row:checkpoint": 1, "row:(untyped)": 1, "role:artifact": 1, "block:citation": 1, "block:(untyped)": 1}
	if !reflect.DeepEqual(after.Unread, want) {
		t.Errorf("unread = %v, want %v", after.Unread, want)
	}
	before.Session.Path, after.Session.Path = "", ""
	if !reflect.DeepEqual(after.Session, before.Session) || !reflect.DeepEqual(after.Stats, before.Stats) || !reflect.DeepEqual(after.Turns, before.Turns) {
		t.Errorf("read with what is not known:\n%+v\n%+v\nwithout it:\n%+v\n%+v", after.Session, after.Turns, before.Session, before.Turns)
	}
}

// TestForkInheritsItsParentsRows: /fork writes a new file — a header naming the file it
// came from, then the parent's rows as they were. The copy is the parent's work.
func TestForkInheritsItsParentsRows(t *testing.T) {
	root := t.TempDir()
	fork := header("fork", "/w")
	fork["timestamp"] = "2026-10-06T19:00:00.000Z"
	fork["parentSession"] = "/somewhere/2026-10-06_parent.jsonl"
	sourcetest.Write(t, filepath.Join(root, "p", "2026-10-06T19-00-00_fork.jsonl"),
		fork,
		msgRow("p1", "2026-10-06T18:40:00Z", M{"role": "user", "content": text("the parent's ask")}),
		msgRow("p2", "2026-10-06T18:40:05Z", M{"role": "assistant", "model": "m", "content": text("the parent's reply"),
			"usage": M{"input": 1000, "output": 1000, "cacheRead": 0, "cacheWrite": 0}}),
		msgRow("f1", "2026-10-06T19:00:10Z", M{"role": "user", "content": text("try the other way")}),
		msgRow("f2", "2026-10-06T19:00:20Z", M{"role": "assistant", "model": "m", "content": text("done"),
			"usage": M{"input": 5, "output": 7, "cacheRead": 0, "cacheWrite": 0}}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Stats.Turns != 1 || s.Turn(t, 1).Ask != "try the other way" || s.Session.Title != "try the other way" {
		t.Fatalf("turns = %+v title %q", s.Stats, s.Session.Title)
	}
	if s.Stats.Usage != (model.Usage{Input: 5, Output: 7}) {
		t.Errorf("usage = %+v: the copied rows are the parent's", s.Stats.Usage)
	}
	for _, it := range s.Turn(t, 0).Items {
		if !it.Inherited {
			t.Errorf("item %d of the copy is not inherited", it.Seq)
		}
	}
}

func TestLocate(t *testing.T) {
	home := "/home/u"
	cases := []struct {
		name string
		vars []string
		want string
	}{
		{"default", nil, filepath.Join(home, ".pi", "agent", "sessions")},
		{"agent dir moved", []string{"PI_CODING_AGENT_DIR", "/moved/agent"}, filepath.Join("/moved/agent", "sessions")},
		// the flat --session-dir layout replaces the directory outright, and wins
		{"session dir", []string{"PI_CODING_AGENT_SESSION_DIR", "/hosted"}, "/hosted"},
		{"session dir beats agent dir", []string{"PI_CODING_AGENT_DIR", "/moved/agent", "PI_CODING_AGENT_SESSION_DIR", "/hosted"}, "/hosted"},
	}
	for _, c := range cases {
		if got := Spec.Locate(sourcetest.Env(home, c.vars...)); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "proj", "2026-10-01T10-00-00_aaaa.jsonl")
	b := filepath.Join(root, "proj", "2026-10-01T11-00-00_bbbb.jsonl")
	flat := filepath.Join(root, "2026-10-01T12-00-00_cccc.jsonl")
	sourcetest.Write(t, a, header("aaaa", "/w"), msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "content": text("one")}))
	sourcetest.Write(t, b, header("bbbb", "/w"))
	sourcetest.Write(t, flat, header("cccc", "/w"))
	// none of these is a transcript
	sourcetest.Write(t, filepath.Join(root, "proj", "notes.txt"), "not a session")
	sourcetest.Write(t, filepath.Join(root, "README.md"), "nor this")
	if err := os.MkdirAll(filepath.Join(root, "proj", "dir.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}

	src := Spec.Open(root)
	stamps := func() map[string]string {
		refs, err := src.Scan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range refs {
			if r.ModTime.IsZero() {
				t.Errorf("%s has no modification time", r.Key)
			}
			out[r.Key] = r.Stamp
		}
		return out
	}
	before := stamps()
	if len(before) != 3 || before[a] == "" || before[b] == "" || before[flat] == "" {
		t.Fatalf("refs = %v", before)
	}
	// An untouched store scans to the same stamps
	if again := stamps(); again[a] != before[a] || again[b] != before[b] || again[flat] != before[flat] {
		t.Errorf("stamps moved with nothing changed: %v / %v", before, again)
	}
	// One session grows: its stamp moves and the others' hold
	sourcetest.Write(t, a, header("aaaa", "/w"),
		msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "content": text("one")}),
		msgRow("m2", "2026-10-06T18:40:01Z", M{"role": "assistant", "content": text("two")}))
	after := stamps()
	if after[a] == before[a] || after[b] != before[b] || after[flat] != before[flat] {
		t.Errorf("stamps after one session changed: %v / %v", before, after)
	}
}

func TestScanFollowsLinkedProjectDirectories(t *testing.T) {
	root, elsewhere := t.TempDir(), t.TempDir()
	sourcetest.Write(t, filepath.Join(elsewhere, "2026-10-01T10-00-00_llll.jsonl"), header("llll", "/w"))
	if err := os.Symlink(elsewhere, filepath.Join(root, "linked")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Session.ID != "llll" {
		t.Errorf("session = %+v", s.Session)
	}
}

func TestScanOfAMissingStore(t *testing.T) {
	src := Spec.Open(filepath.Join(t.TempDir(), "never-made"))
	if src.Exists() {
		t.Error("a store that is not there exists")
	}
	if refs, err := src.Scan(context.Background()); err != nil || len(refs) != 0 {
		t.Errorf("refs %v, err %v: a store that is not there is empty, not broken", refs, err)
	}
}

func TestBrokenLinesAreSkipped(t *testing.T) {
	root := t.TempDir()
	sourcetest.Write(t, filepath.Join(root, "p", "2026-10-01T10-00-00_bbbb.jsonl"),
		header("pi-b", "/w"),
		`{"type":"message","id":"half","message":{"role":"user","content":[{"type":"te`, // cut off mid-write
		`not json at all`,
		// JSON, but not an object: no record of any kind
		`null`, ` []`, `1`, `"x"`, `true`, `[{"type":"checkpoint"}]`,
		msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "content": text("after the damage")}),
		// a field of an unexpected type loses that field, not the message
		msgRow("m2", "2026-10-06T18:40:01Z", M{"role": "assistant", "durationMs": "slow", "usage": "none", "content": text("still here")}),
		// and a block that is not an object is no block, beside ones that are
		msgRow("m3", "2026-10-06T18:40:02Z", M{"role": "user", "content": []any{"a", nil, 5, []any{}, M{"type": "text", "text": "next"}}}),
		msgRow("m4", "2026-10-06T18:40:03Z", M{"role": "assistant", "content": []any{nil, M{"type": "text", "text": "fine"}, true}}),
	)
	s := sourcetest.One(t, Spec.Open(root))
	// A line that is not JSON, or not a JSON object, says nothing of what it was: it is
	// damage, not a kind
	sourcetest.NothingUnread(t, s)
	if !strings.HasPrefix(s.Turn(t, 1).Ask, "after the damage") || s.Turn(t, 1).Answer != "still here" {
		t.Errorf("turn = %+v", s.Turn(t, 1))
	}
	if s.Turn(t, 2).Ask != "next" || s.Turn(t, 2).Answer != "fine" {
		t.Errorf("turn = %+v", s.Turn(t, 2))
	}
}
