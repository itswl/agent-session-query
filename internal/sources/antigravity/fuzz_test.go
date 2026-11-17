package antigravity

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// fuzzSeeds are transcripts as agy writes them, taken from the tests: a turn with a step
// rewritten and a tool step, a model step still generating, agy's own context in the
// user's seat with lines that are not steps, fields of odd types, steps of types this
// reader does not know or does not yet read, counters that are not counts, and agy 1.5.8's
// tool calls with the steps that answer them — in order, failed, moved to the background,
// left unanswered — beside what agy tells the model.
var fuzzSeeds = [][]string{
	{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-12T10:00:00Z","content":"<USER_REQUEST>fix the build</USER_REQUEST><ADDITIONAL_METADATA>noise</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:01Z","content":"on it","input_tokens":80,"cache_read_tokens":20,"output_tokens":12}`,
		`{"step_index":2,"source":"MODEL","type":"RUN_COMMAND","status":"DONE","created_at":"2026-10-12T10:00:02Z","content":""}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:03Z","content":"done now","model":"gemini-3-pro","input_tokens":100,"cache_read_tokens":20,"output_tokens":12}`,
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:04Z","content":"all set","input_tokens":5,"output_tokens":1}`,
	},
	{
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-10-12T10:00:00Z","content":"<USER_REQUEST>go</USER_REQUEST>"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:01Z","content":"one","input_tokens":10,"cache_read_tokens":4,"cache_write_tokens":6,"output_tokens":3}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"GENERATING","created_at":"2026-10-12T10:00:02Z","content":"par","input_tokens":900,"output_tokens":900}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:03Z","content":"two"}`,
	},
	{
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-10-12T10:00:00Z","content":"<ADDITIONAL_METADATA>the date is today</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"type":"USER_INPUT","created_at":"2026-10-12T10:00:01Z","content":"<USER_REQUEST>\n  what changed?\n</USER_REQUEST>\n<ADDITIONAL_METADATA>x</ADDITIONAL_METADATA>"}`,
		`{"step_index":2,"type":"USER_INPUT","created_at":"2026-10-12T10:00:02Z","content":""}`,
		`{"step_index":3,"type":"CHECKPOINT","created_at":"2026-10-12T10:00:03Z","content":"saved"}`,
		`{"step_index":"four","type":"USER_INPUT","content":"a step with no index is not a step"}`,
		`not json`,
	},
	{
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-10-12T10:00:00Z","content":"<USER_REQUEST>what changed?</USER_REQUEST>"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":1791799201,"model":{"id":"gemini-3"},"content":"Two files changed.","input_tokens":120,"output_tokens":30}`,
	},
	{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-12T10:00:00Z","content":"<USER_REQUEST>tidy the repo</USER_REQUEST>"}`,
		`{"step_index":2,"source":"MODEL","type":"HOLOGRAM_PROJECTION","status":"RUNNING","created_at":"2026-10-12T10:00:02Z","content":"warming up"}`,
		`{"step_index":3,"source":"MODEL","type":"VIEW_FILE","status":"DONE","created_at":"2026-10-12T10:00:03Z","content":"package main"}`,
		`{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-12T10:00:05Z","content":"tidied","input_tokens":50,"output_tokens":6}`,
		`{"step_index":5,"source":"MODEL","type":"ERROR_MESSAGE","status":"DONE","created_at":"2026-10-12T10:00:06Z","content":"quota exceeded"}`,
		`{"step_index":6,"source":"MODEL","type":"NOTIFY_USER","status":"DONE","created_at":"2026-10-12T10:00:07Z","content":"Tidied; the plan is ready for review."}`,
	},
	{
		`{"step_index":0,"type":"USER_INPUT","content":"<USER_REQUEST>go</USER_REQUEST>"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","content":"one","input_tokens":9e18,"output_tokens":7}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"two","input_tokens":-40,"output_tokens":1099511627777,"cache_write_tokens":1099511627776}`,
	},
	{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-17T04:58:18Z","content":"<USER_REQUEST>\nread both\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-17T12:58:18+08:00.\n</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-17T04:58:18Z","model":"MODEL_GOOGLE_GEMINI_INTERNAL_BYOM","input_tokens":8949,"cache_read_tokens":0,"output_tokens":84,"thinking":"Two reads at once.","tool_calls":[{"name":"read_file","args":{"Path":"/etc/hostname"}},{"name":"read_file","args":{"Path":"/nonexistent.txt"}},"odd",{"args":{}}]}`,
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-17T04:58:23Z","content":"Created At: 2026-11-17T12:58:23+08:00\nCompleted At: 2026-11-17T12:58:23+08:00\nCommand exited with code 2.\n\nCommand output:\nls: cannot access\r\n"}`,
		`{"step_index":3,"source":"MODEL","type":"GENERIC","status":"ERROR","error":"file does not exist","created_at":"2026-11-17T04:58:23Z","content":"Created At: 2026-11-17T12:58:23+08:00\nCompleted At: 2026-11-17T12:58:23+08:00\n"}`,
		`{"step_index":4,"source":"MODEL","type":"GENERIC","status":"RUNNING","created_at":"2026-11-17T04:58:24Z","content":"Created At: 2026-11-17T12:58:24+08:00\nTool is running as a background task with task id: c/task-4"}`,
		`{"step_index":5,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","created_at":"2026-11-17T04:58:25Z","content":"The following is a <SYSTEM_MESSAGE> not actually sent by the user."}`,
		`{"step_index":6,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-17T04:58:26Z","content":"done","tool_calls":[{"name":"run_command","args":{"CommandLine":"sleep 8"}}]}`,
		`{"step_index":7,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-17T04:58:27Z","content":"Command exited with code x."}`,
		`{"step_index":8,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-17T04:58:28Z","error":{"code":1}}`,
	},
}

// FuzzRead lays out a gemini directory with one conversation whose transcript is the input,
// and, when it is given one, an index row naming that conversation with the input's
// workspace_uris and last_modified_time; then it reads the store as asq does. An input may
// be refused, but none may panic, hang, or read as a session that breaks what every session
// holds.
//
// The directory is made once and its two files rewritten for each input: a worker runs its
// inputs one at a time, and a fresh directory each time is most of what an input costs.
func FuzzRead(f *testing.F) {
	for i, lines := range fuzzSeeds {
		steps := []byte(strings.Join(lines, "\n") + "\n")
		f.Add(steps, "", "")
		if i == 0 {
			f.Add(steps, `["file:///work/my%20repo"]`, "2026-10-12T10:00:09Z")
			f.Add(steps, `["vscode-remote://x","file:///C:/work/app"]`, "2026-10-12 10:00:09.250")
		}
	}
	root := f.TempDir()
	path := transcriptPath(filepath.Join(root, "antigravity-cli", "brain", "fuzz-1"))
	index := filepath.Join(root, "antigravity-cli", "conversation_summaries.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, transcript []byte, uris, modified string) {
		if err := os.WriteFile(path, transcript, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(index); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if uris != "" || modified != "" {
			fuzzIndex(t, index, uris, modified)
		}
		sourcetest.Attempt(t, Spec.Open(root))
	})
}

// fuzzIndex writes the CLI's index with one row for the fuzzed conversation, its values
// bound rather than spelled into the statement: any bytes at all are a value.
func fuzzIndex(t *testing.T, path, uris, modified string) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(createIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES ('fuzz-1', 'Fuzzed', ?, '', ?)`, uris, modified); err != nil {
		t.Fatal(err)
	}
}
