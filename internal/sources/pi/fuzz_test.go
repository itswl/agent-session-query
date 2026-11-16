package pi

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// FuzzRead reads whatever a transcript file might hold, laid out where Scan finds it. An
// error is an answer; a panic, or a session that breaks what every session must hold, is
// not.
func FuzzRead(f *testing.F) {
	for _, lines := range [][]any{
		// A plain exchange: the header, pi's bookkeeping, a prompt and a response with
		// its thinking and what it cost
		{
			header("pi-1", "/home/u/proj"),
			M{"type": "model_change", "id": "mc1", "parentId": nil, "timestamp": "2026-10-06T18:39:01.000Z", "provider": "p", "modelId": "m"},
			M{"type": "thinking_level_change", "id": "tl1", "parentId": "mc1", "timestamp": "2026-10-06T18:39:02.000Z", "thinkingLevel": "high"},
			msgRow("m1", "2026-10-06T18:40:00.002Z", M{"role": "user", "timestamp": ms(0), "content": text("hello")}),
			msgRow("m2", "2026-10-06T18:40:05.100Z", M{
				"role": "assistant", "timestamp": ms(4 * time.Second), "model": "m", "provider": "p",
				"stopReason": "stop", "durationMs": 1200,
				"usage": M{"input": 100, "output": 50, "cacheRead": 300, "cacheWrite": 20, "reasoning": 30, "totalTokens": 470,
					"cost": M{"input": 0.001, "output": 0.002, "total": 0.0123}},
				"content": []M{{"type": "thinking", "thinking": "pondering", "thinkingSignature": "sig"}, {"type": "text", "text": "hi there"}},
			}),
		},
		// Tools: two calls in one response, their results, one failed and one with an image
		{
			header("pppp", "/w"),
			msgRow("m1", "2026-10-06T18:40:00Z", M{"role": "user", "content": text("run it"), "timestamp": ms(0)}),
			msgRow("m2", "2026-10-06T18:40:01Z", M{"role": "assistant", "stopReason": "toolUse", "timestamp": ms(time.Second), "durationMs": 800,
				"content": []M{
					{"type": "toolCall", "id": "c1", "name": "bash", "arguments": M{"command": "pwd"}},
					{"type": "toolCall", "id": "c2", "name": "read", "arguments": M{"path": "/w/a.go"}},
				}}),
			msgRow("m3", "2026-10-06T18:40:03Z", M{"role": "toolResult", "toolCallId": "c1", "toolName": "bash", "isError": true, "durationMs": 120,
				"timestamp": ms(2 * time.Second), "content": text("boom"),
				"usage": M{"input": 7, "output": 3, "cacheRead": 0, "cacheWrite": 0, "cost": M{"total": 0.001}}}),
			msgRow("m4", "2026-10-06T18:40:03Z", M{"role": "toolResult", "toolCallId": "c2", "toolName": "read", "isError": false,
				"timestamp": ms(2 * time.Second), "content": []M{{"type": "text", "text": "line 1"}, {"type": "image", "data": "AAAA", "mimeType": "image/png"}}}),
			msgRow("m5", "2026-10-06T18:40:04Z", M{"role": "assistant", "stopReason": "aborted", "errorMessage": "Request was aborted",
				"timestamp": ms(3 * time.Second), "content": []M{}}),
		},
		// Everything around the conversation: the prompt, the person's shell, an
		// extension's note, a compaction and a branch left behind, and the rows pi keeps
		// for itself
		{
			header("ctxx", "/w"),
			msgRow("m0", "2026-10-06T18:40:00Z", M{"role": "system", "content": "Be brief.", "sections": M{"cwd": "/w"}}),
			msgRow("m1", "2026-10-06T18:40:01Z", M{"role": "user", "content": "go"}),
			msgRow("m2", "2026-10-06T18:40:02Z", M{"role": "bashExecution", "command": "ls", "output": "a.txt", "exitCode": 2, "truncated": true, "fullOutputPath": "/tmp/o"}),
			M{"type": "custom_message", "id": "cm", "timestamp": "2026-10-06T18:40:03Z", "customType": "note", "content": "an extension's note", "display": true},
			M{"type": "compaction", "id": "cp", "timestamp": "2026-10-06T18:40:04Z", "summary": "what came before", "firstKeptEntryId": "m1", "tokensBefore": 900,
				"usage": M{"input": 100, "output": 20, "cacheRead": 0, "cacheWrite": 0}},
			M{"type": "branch_summary", "id": "bs", "timestamp": "2026-10-06T18:40:05Z", "fromId": "m1", "summary": "the branch left behind"},
			M{"type": "session_info", "id": "si", "timestamp": "2026-10-06T18:40:06Z", "name": "Release prep"},
			M{"type": "label", "id": "lb", "timestamp": "2026-10-06T18:40:07Z", "targetId": "m1", "label": "start"},
			M{"type": "custom", "id": "cu", "timestamp": "2026-10-06T18:40:07Z", "customType": "todo", "data": M{"open": 2}},
			M{"type": "context_edit", "id": "ce", "timestamp": "2026-10-06T18:40:07Z", "targetId": "m2", "replacement": nil},
			M{"type": "usage", "id": "us", "timestamp": "2026-10-06T18:40:08Z", "kind": "cache_warm", "provider": "p", "model": "m",
				"usage": M{"input": 0, "output": 1, "cacheRead": 5000, "cacheWrite": 0}},
		},
		// A fork: the parent's rows copied in ahead of its own, and a line cut off
		{
			M{"type": "session", "version": 3, "id": "fork", "timestamp": "2026-10-06T19:00:00.000Z", "cwd": "/w", "parentSession": "/s/parent.jsonl"},
			msgRow("p1", "2026-10-06T18:40:00Z", M{"role": "user", "content": text("the parent's ask")}),
			msgRow("p2", "2026-10-06T18:40:05Z", M{"role": "assistant", "model": "m", "content": text("the parent's reply"),
				"usage": M{"input": 1000, "output": 1000, "cacheRead": 0, "cacheWrite": 0}}),
			msgRow("f1", "2026-10-06T19:00:10Z", M{"role": "user", "content": text("try the other way")}),
			`{"type":"message","id":"half","message":{"role":"assistant","content":[{"type":"te`,
		},
	} {
		path := filepath.Join(f.TempDir(), "seed.jsonl")
		sourcetest.Write(f, path, lines...)
		seed, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(seed)
	}
	// One store for every input: a worker runs its inputs one at a time, and a directory
	// made and removed for each would be most of what an input costs
	root := f.TempDir()
	path := filepath.Join(root, "proj", "2026-10-06T18-39-00-000Z_fuzz.jsonl")
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
