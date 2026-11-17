package gemini

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// FuzzRead reads whatever a session file may hold. The input is written as a session file
// and again as the file that continues it, as Gemini CLI does when a session goes on in a
// new file: what one file leaves behind in the reader — the calls answered, a message's
// first place — is what the next one is read against. An error is an answer; a panic, a
// hang or a session that breaks the invariants is not.
func FuzzRead(f *testing.F) {
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	// One store for every input, written over each time: a fuzzing process runs its inputs
	// one after another, and a new directory per input costs more than the read. Each
	// input is read by a Source of its own, which has seen no other.
	root := f.TempDir()
	dir := filepath.Join(root, "p", "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, name := range []string{"session-2026-11-01T09-00-a.jsonl", "session-2026-11-01T09-01-a.jsonl"} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		sourcetest.Attempt(t, Spec.Open(root))
	})
}

// seeds are FuzzRead's starting inputs, and all that its regression run — go test without
// -fuzz — reads: a branch no seed reaches is one that run never checks.
var seeds = []string{
	// the metadata, a patch with messages, a call answered on the user row that follows
	`{"sessionId":"g-1","startTime":"2026-09-13T12:50:41Z","lastUpdated":"2026-09-13T12:55:00Z"}
{"$set":{"messages":[{"type":"user","id":"gu1","timestamp":"t1","content":[{"text":"hey"}]}]}}
{"type":"gemini","id":"gg1","timestamp":"t2","model":"gemini-2.5","thoughts":"mulling","tokens":{"input":3},"content":"hello"}
{"type":"gemini","id":"gg2","timestamp":"t3","model":"gemini-2.5","content":"","thoughts":[{"subject":"locate the file","description":"check package.json first"}],"toolCalls":[{"id":"c1","name":"read_file","args":{"file_path":"package.json"},"result":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"..."}}}]}]}
{"type":"user","id":"gu2","timestamp":"t4","content":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"{\"name\":\"larkin\"}"}}}]}
`,
	// calls answered in place, with every status
	`{"sessionId":"g-2","startTime":"2026-10-01T10:00:00Z","lastUpdated":"2026-10-01T10:05:00Z"}
{"type":"user","id":"u1","timestamp":"t1","content":[{"text":"build it"}]}
{"type":"gemini","id":"g1","timestamp":"t2","content":"","toolCalls":[{"id":"run-1","name":"run_shell_command","args":{"command":"npm run build"},"status":"error","resultDisplay":"npm ERR! missing script"},{"id":"rep-1","name":"replace","args":{"file_path":"config.ts","old_string":"a","new_string":"b"},"status":"success","resultDisplay":{"fileName":"config.ts","fileDiff":"--- a\n+++ b\n-a\n+b\n"}},{"id":"cancel-1","name":"run_shell_command","args":{"command":"sleep 9"},"status":"cancelled","result":[{"functionResponse":{"id":"cancel-1","name":"run_shell_command","response":{"error":"Command was cancelled by the user."}}}]},{"id":"late-1","name":"read_file","args":{"file_path":"x"},"status":"success"},{"id":"live-1","name":"run_shell_command","args":{"command":"tail -f log"},"status":"executing"}]}
{"type":"user","id":"u2","timestamp":"t3","content":[{"functionResponse":{"id":"run-1","name":"run_shell_command","response":{"output":"npm ERR! missing script"}}},{"functionResponse":{"id":"late-1","name":"read_file","response":{"output":"the file"}}}]}
`,
	// the usage counters, and rows that are not the conversation
	`{"sessionId":"g-3"}
{"type":"user","timestamp":"2026-10-01T10:00:00Z","content":[{"text":"think about it"}]}
{"type":"gemini","id":"a","timestamp":"2026-10-01T10:00:01Z","model":"gemini-2.5-pro","content":"one","tokens":{"input":15597,"output":133,"cached":0,"thoughts":396,"tool":0,"total":16126}}
{"type":"gemini","id":"b","timestamp":"2026-10-01T10:00:02Z","model":"gemini-2.5-pro","content":"two","tokens":{"input":1000,"output":50,"cached":700,"thoughts":200,"tool":30,"total":1280}}
{"type":"info","id":"i","timestamp":"2026-10-01T10:00:01Z","content":"update available"}
not json at all
{"$set":{"summary":"nothing to see","lastUpdated":"2026-10-01T10:30:00.000Z"}}
{"type":"error","id":"e","timestamp":"2026-10-01T10:00:03Z","content":"quota"}
`,
	// a message written again, and a patch that sets the messages again
	`{"sessionId":"g-2","startTime":"2026-09-13T12:50:41Z","lastUpdated":"2026-09-13T12:50:41Z"}
{"type":"user","id":"gu1","timestamp":"2026-09-13T12:50:42Z","content":[{"text":"read the config"}]}
{"type":"gemini","id":"gg1","timestamp":"2026-09-13T12:50:45Z","model":"gemini-2.5","content":"","tokens":{"input":100,"output":20},"toolCalls":[{"id":"c1","name":"read_file","args":{"file_path":"config.json"}}]}
{"type":"gemini","id":"gg1","timestamp":"2026-09-13T12:50:45Z","model":"gemini-2.5","content":"","tokens":{"input":100,"output":20},"toolCalls":[{"id":"c1","name":"read_file","args":{"file_path":"config.json"},"status":"success","resultDisplay":"{\"port\":8080}"}]}
{"$set":{"lastUpdated":"2026-09-13T12:51:00Z","messages":[{"type":"user","id":"gu1","timestamp":"2026-09-13T12:50:42Z","content":[{"text":"read the config"}]},{"type":"gemini","id":"gg2","timestamp":"2026-09-13T12:50:50Z","model":"gemini-2.5","content":"The port is 8080.","tokens":{"input":150,"output":10}}]}}
`,
	// the current format: parts rewritten from history, a synthetic thought, a rewind
	`{"sessionId":"0b5e1a7c-3c1d","projectHash":"9f2c","startTime":"2026-11-01T09:00:00.000Z","lastUpdated":"2026-11-01T09:00:00.000Z","kind":"main"}
{"id":"u1","timestamp":"2026-11-01T09:00:01.000Z","type":"user","content":[{"text":"what is in shot.png?"},{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}],"displayContent":[{"text":"what is in @shot.png?"}]}
{"id":"g1","timestamp":"2026-11-01T09:00:04.000Z","type":"gemini","content":[{"text":"**Reading**\n\nnotes","thought":true},{"functionCall":{"id":"read-1","name":"read_file","args":{"file_path":"diagram.png"}},"thoughtSignature":"c2ln"}],"thoughts":[{"subject":"Reading","description":"notes"}],"tokens":{"input":2000,"output":20,"cached":0,"thoughts":50,"tool":0,"total":2070},"toolCalls":[{"id":"read-1","name":"read_file","args":{"file_path":"diagram.png"},"status":"success","resultDisplay":"Read image file"}]}
{"id":"g2","timestamp":"2026-11-01T09:00:04.200Z","type":"gemini","content":[{"text":"Binary content received.","thought":true}],"thoughts":[],"tokens":null}
{"id":"u3","timestamp":"2026-11-01T09:01:00.000Z","type":"user","content":[{"text":"make the button blue"}]}
{"$rewindTo":"u3"}
{"id":"w1","timestamp":"2026-11-01T09:01:30.000Z","type":"warning","content":"Rewound."}
`,
	// counters below zero, at the most a count can be and past it (see
	// TestTokenCountsPastAnyModelCountNothing)
	`{"sessionId":"g-8"}
{"type":"user","id":"u","content":[{"text":"q"}]}
{"type":"gemini","id":"a","content":"x","tokens":{"input":9e18,"output":9e18,"thoughts":9e18,"tool":9e18}}
{"type":"gemini","id":"b","content":"y","tokens":{"input":1099511627776,"output":1099511627777,"thoughts":-1,"cached":5}}
`,
	// what the adapter does not know: a row type, a patch operator, a row and a patched
	// message that do not say what they are, a part (see TestUnknownKindsAreCounted)
	`{"sessionId":"g-7","projectHash":"9f2c","startTime":"2026-11-02T10:00:00Z"}
{"id":"u1","timestamp":"2026-11-02T10:00:01Z","type":"user","content":[{"text":"run the tests"}]}
{"id":"c1","timestamp":"2026-11-02T10:00:02Z","type":"checkpoint","content":"saved"}
{"$truncate":{"after":"u1"}}
{"id":"x1","timestamp":"2026-11-02T10:00:03Z","content":"a message that does not say what it is"}
{"$set":{"messages":[{"id":"x2","content":"nor does this one"}]}}
{"id":"g1","timestamp":"2026-11-02T10:00:05Z","type":"gemini","content":[{"text":"all green"},{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"1"}}],"tokens":{"input":10,"output":2}}
{"$set":null}
{}
`,
	// content as Gemini CLI also takes it, calls only the history holds, and parts and
	// entries with little in them (see TestContentAsGeminiCLIReadsIt and
	// TestCallsOnlyInTheHistoryAreRead)
	`{"sessionId":"g-10"}
{"id":"u1","timestamp":"2026-11-03T10:00:01Z","type":"user","content":{"text":"what is in a.go?"}}
{"id":"g1","timestamp":"2026-11-03T10:00:02Z","type":"gemini","content":[{"functionCall":{"id":"t-1","name":"read_file","args":{"file_path":"a.go"}}},{"functionCall":{"name":"glob","args":{"pattern":"*.go"}}},{"functionCall":{"id":"l-1","name":"list_directory"}}],"toolCalls":[{"id":"t-1","name":"read_file","args":{"file_path":"a.go"},"status":"success","resultDisplay":"package a"},{"id":"t-2","name":"glob","args":{"pattern":"*.go"},"status":"success","resultDisplay":"a.go b.go"},{"name":"list_directory","status":"success","resultDisplay":"a.go b.go"}]}
{"id":"h1","timestamp":"2026-11-03T10:00:03Z","type":"gemini","content":["Now b.go.","",{"thoughtSignature":"c2ln"},{"functionCall":{"id":"t-1","name":"read_file","args":{"file_path":"a.go"}}},{"functionCall":{"id":"h-1","name":"read_file","args":{"file_path":"b.go"}}},{"functionCall":{"name":"read_file"}}]}
{"id":"u2","timestamp":"2026-11-03T10:00:04Z","type":"user","content":{"functionResponse":{"id":"h-1","name":"read_file","response":{"output":"package b"}}}}
{"id":"u3","timestamp":"2026-11-03T10:00:05Z","type":"user","content":["and c.go?"]}
`,
}

// The seeds reach every place the reader counts a record it does not know: a message's
// type, a line's patch operator, a record that says nothing of what it is, a content part.
func TestSeedsReachWhatIsUnread(t *testing.T) {
	unread := map[string]int{}
	for _, seed := range seeds {
		root := t.TempDir()
		path := filepath.Join(root, "p", "chats", "session-2026-11-01T09-00-a.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, s := range sourcetest.All(t, Spec.Open(root)) {
			for kind, n := range s.Unread {
				unread[kind] += n
			}
		}
	}
	for _, kind := range []string{"row:checkpoint", "row:$truncate", "row:(untyped)", "part:codeExecutionResult"} {
		if unread[kind] == 0 {
			t.Errorf("no seed has a record counted as %s (all: %v)", kind, unread)
		}
	}
}
