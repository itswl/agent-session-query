package grok

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// recorded puts a session Grok CLI 1.0.77 wrote into a store under root, where Grok keeps
// it: in a group named after its cwd, in a directory named by its id. The files are kept
// in testdata/<name>, trimmed, with the home directory and the model renamed.
//
//	shell          two commands run together: one prints, one exits 2; hooks on every event
//	files          a file written, then edited, then a command
//	grep           a search that matched nothing
//	search         a file read, and a search that matched
//	plan           a plan-mode turn: the mode change, a todo list, a listing, the plan file
//	multi          three turns over three runs: -p, then -r, then -c
//	running        a session read mid-turn, by its whole-message log alone
//	shell-history  the shell session, by its whole-message log alone
func recorded(t testing.TB, root, name string) string {
	t.Helper()
	src := filepath.Join("testdata", name)
	var sum struct {
		Info struct{ ID, Cwd string }
	}
	if err := json.Unmarshal(readFile(t, filepath.Join(src, summaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, url.PathEscape(sum.Info.Cwd), sum.Info.ID)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		sourcetest.Write(t, filepath.Join(dir, e.Name()), string(readFile(t, filepath.Join(src, e.Name()))))
	}
	return dir
}

func ms(n int64) time.Time { return time.UnixMilli(n).UTC() }

// Grok marks a command that exited non-zero completed, as it does one that succeeded; the
// exit code in its rawOutput is what says the command failed, and Grok's own events.jsonl
// counts that call's outcome an error.
func TestRecordedCommandExitCodes(t *testing.T) {
	root := t.TempDir()
	recorded(t, root, "shell")
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	turn := s.Turn(t, 1)
	tools := sourcetest.Tools(turn)
	if len(tools) != 2 {
		t.Fatalf("tools = %d", len(tools))
	}
	echo, ls := tools[0], tools[1]
	if echo.Command != "echo hello-from-shell" || echo.Status != model.StatusOK || echo.Exit == nil || *echo.Exit != 0 || echo.Output != "hello-from-shell\n" {
		t.Errorf("echo = %+v", echo)
	}
	if ls.Command != "ls /nonexistent-asq-path" || ls.Status != model.StatusError || ls.Exit == nil || *ls.Exit != 2 ||
		ls.Output != "ls: cannot access '/nonexistent-asq-path': No such file or directory\n" {
		t.Errorf("ls = %+v", ls)
	}
	if turn.Failures != 1 || s.Stats.Failures != 1 {
		t.Errorf("failures = %d / %d", turn.Failures, s.Stats.Failures)
	}
}

// Grok's grep keeps the exit code of the search it ran, and a search that matched nothing
// exits 1. That is an answer: Grok's own events.jsonl counts the call a success.
func TestRecordedSearchThatMatchedNothing(t *testing.T) {
	root := t.TempDir()
	recorded(t, root, "grep")
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	tool := sourcetest.Tools(s.Turn(t, 1))[0]
	if tool.Name != "grep" || tool.Kind != model.ToolSearch || tool.Status != model.StatusOK || tool.Exit != nil {
		t.Errorf("grep = %+v", tool)
	}
	if want := "<workspace_result workspace_path=\"/home/user/work/files\">\nNo matches found\n</workspace_result>"; tool.Output != want {
		t.Errorf("output = %q, want %q", tool.Output, want)
	}
	if s.Stats.Failures != 0 {
		t.Errorf("failures = %d", s.Stats.Failures)
	}
}

// A search's content is a count — "found 1 matches" — and the lines it matched, as the
// model read them, are in rawOutput's stdout, written as a list of byte values. A file
// read has its lines for content, as the model read them.
func TestRecordedSearchOutput(t *testing.T) {
	root := t.TempDir()
	recorded(t, root, "search")
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	tools := sourcetest.Tools(s.Turn(t, 1))
	if len(tools) != 2 {
		t.Fatalf("tools = %d", len(tools))
	}
	if read := tools[0]; read.Name != "read_file" || read.Kind != model.ToolRead || read.Output != "1→beta\n" || len(read.Files) != 1 || read.Files[0] != "notes.txt" {
		t.Errorf("read_file = %+v", read)
	}
	want := "<workspace_result workspace_path=\"/home/user/work/files\">\nFound 1 matching lines\n/home/user/work/files/notes.txt\n1:beta\n</workspace_result>"
	if grep := tools[1]; grep.Name != "grep" || grep.Status != model.StatusOK || grep.Output != want {
		t.Errorf("grep = %+v", grep)
	}
}

// Two calls made together finish apart, and each result is filed when it arrived: one
// message for both would end the second at the first's time.
func TestRecordedResultsTogether(t *testing.T) {
	root := t.TempDir()
	recorded(t, root, "shell")
	tools := sourcetest.Tools(sourcetest.One(t, Spec.Open(root)).Turn(t, 1))
	if !tools[0].EndAt.Equal(ms(1794891049297)) || !tools[1].EndAt.Equal(ms(1794891049319)) {
		t.Errorf("ended %v and %v", tools[0].EndAt, tools[1].EndAt)
	}
}

// A line's timestamp is when it was written, in whole seconds, and Grok writes some lines
// seconds after what they record: the edit here completed 56 ms after it was called and
// its line is stamped four seconds on. The milliseconds Grok's agent stamped each update
// with are when it happened.
func TestRecordedTimes(t *testing.T) {
	root := t.TempDir()
	recorded(t, root, "files")
	s := sourcetest.One(t, Spec.Open(root))
	turn := s.Turn(t, 1)
	if !turn.StartedAt.Equal(ms(1794891061494)) || !turn.Items[0].At.Equal(ms(1794891061494)) {
		t.Errorf("turn started %v, prompt at %v", turn.StartedAt, turn.Items[0].At)
	}
	want := []struct{ at, end int64 }{
		{1794891066946, 1794891066995}, // write: the line is stamped 1794891069
		{1794891070562, 1794891070618}, // search_replace: stamped 1794891074
		{1794891074954, 1794891075017}, // run_terminal_command: stamped 1794891077
	}
	var i int
	for _, item := range turn.Items {
		if item.Tool == nil {
			continue
		}
		if i >= len(want) {
			t.Fatalf("more than %d tools", len(want))
		}
		if !item.At.Equal(ms(want[i].at)) || !item.Tool.EndAt.Equal(ms(want[i].end)) {
			t.Errorf("%s at %v, ended %v; want %v, %v", item.Tool.Name, item.At, item.Tool.EndAt, ms(want[i].at), ms(want[i].end))
		}
		i++
	}
	// The turn ends at its last words, and its last response runs from the reasoning
	// before them
	last := turn.Calls[len(turn.Calls)-1]
	if !turn.EndedAt.Equal(ms(1794891078330)) || !last.At.Equal(ms(1794891078029)) || !last.EndAt.Equal(ms(1794891078330)) || len(turn.Calls) != 4 {
		t.Errorf("turn ended %v, calls %+v", turn.EndedAt, turn.Calls)
	}
}

// A file written or edited has a diff for content, and a listing or a todo list none:
// what the tool told the model is in rawOutput, and is the call's output.
func TestRecordedOutputsFromRawOutput(t *testing.T) {
	root := t.TempDir()
	recorded(t, root, "files")
	recorded(t, root, "plan")
	all := sourcetest.All(t, Spec.Open(root))
	// The plan-mode turn's mode change and todo list are written as session/update, and
	// are passed over on purpose
	sourcetest.NothingUnread(t, all...)
	outputs := map[string]string{}
	for _, s := range all {
		for _, tool := range sourcetest.Tools(s.Turn(t, 1)) {
			if !tool.Done || tool.Status != model.StatusOK {
				t.Errorf("%s = %+v", tool.Name, tool)
			}
			outputs[tool.Name] += tool.Output + "|"
		}
	}
	for name, want := range map[string]string{
		"write":          "The file /home/user/work/files/notes.txt has been created.|The file /home/user/.grok/sessions/%2Fhome%2Fuser%2Fwork%2Fplan/01a1e7da-8139-7de2-bb3c-01ec61eee044/plan.md has been created.|",
		"search_replace": "The file notes.txt has been updated successfully.|",
		"todo_write":     "- [pending] 1: Write README.md\n- [pending] 2: Review README.md\n|",
		"list_dir":       "- /home/user/work/plan/\n|",
		// content of its own, which comes first
		"exit_plan_mode":       "Non-interactive plan-only run: plan mode stays on. Reply with the full plan and stop.|",
		"run_terminal_command": "exit code: 1\n|",
	} {
		if got := outputs[name]; got != want {
			t.Errorf("%s output = %q, want %q", name, got, want)
		}
	}
}

// Grok 1.0.77 counts the whole prompt as input, the cache read included: its totalTokens
// is input + output. Read as input beside the cache, every cached token counted twice —
// 56.7k in for a session Grok itself counts 34,326 tokens all told.
func TestRecordedUsageHoldsTheCache(t *testing.T) {
	root := t.TempDir()
	recorded(t, root, "multi")
	s := sourcetest.One(t, Spec.Open(root))
	// background_tasks, listed as each resumed run's prompt begins, is passed over on purpose
	sourcetest.NothingUnread(t, s)
	if s.Stats.Turns != 3 || s.Turn(t, 2).Ask != "What was the code word? Answer in one word." || s.Turn(t, 3).Answer != "eniregnat" {
		t.Fatalf("turns = %d", s.Stats.Turns)
	}
	for i, want := range []model.Usage{
		{Input: 11313, Output: 40, Reasoning: 37},
		{Input: 11374 - 11264, Output: 31, CacheRead: 11264, Reasoning: 26},
		{Input: 11419 - 11328, Output: 149, CacheRead: 11328, Reasoning: 143},
	} {
		if got := s.Turn(t, i+1).Usage; got != want {
			t.Errorf("turn %d usage = %+v, want %+v", i+1, got, want)
		}
	}
	// usage.json's session totals, which `grok usage` prints
	if s.Stats.Usage.Total() != 34326 || s.Stats.Usage.CacheRead != 22592 || s.Stats.Usage.Output != 220 {
		t.Errorf("session usage = %+v, total %d", s.Stats.Usage, s.Stats.Usage.Total())
	}
}

// A total past what the counters add up to, or one short by more than the cache, says
// nothing of where the cache is counted: the input is read as it stands.
func TestUsageTotalThatExplainsNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  M
		want model.Usage
	}{
		{"cache written, and counted in the input", M{"inputTokens": 900, "outputTokens": 10, "cacheCreationTokens": 800, "totalTokens": 910},
			model.Usage{Input: 100, Output: 10, CacheWrite: 800}},
		{"a total past the counters", M{"inputTokens": 900, "outputTokens": 10, "cachedReadTokens": 800, "totalTokens": 5000},
			model.Usage{Input: 900, Output: 10, CacheRead: 800}},
		{"a total short by more than the cache", M{"inputTokens": 900, "outputTokens": 10, "cachedReadTokens": 800, "totalTokens": 50},
			model.Usage{Input: 900, Output: 10, CacheRead: 800}},
		{"a cache larger than the input", M{"inputTokens": 100, "outputTokens": 10, "cachedReadTokens": 800, "totalTokens": 110},
			model.Usage{Input: 100, Output: 10, CacheRead: 800}},
	} {
		if got := usageOf(c.raw); got == nil || *got != c.want {
			t.Errorf("%s: usage = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Grok 1.0.77 opens a session's whole-message log with context of its own in the user's
// seat — the workspace and its rules, a reminder of the workflows on offer — which the
// stream does not carry. Read as prompts, a running session had a turn more than it
// would once its stream landed, opened by the rules.
func TestRecordedRunningLog(t *testing.T) {
	root := t.TempDir()
	recorded(t, root, "running")
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if !s.Session.Running || s.Stats.Turns != 1 {
		t.Fatalf("running = %v, turns = %d", s.Session.Running, s.Stats.Turns)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt thinking tool" {
		t.Errorf("items = %s", got)
	}
	if turn.Ask != "Run the shell command: sleep 20 && echo finished . Then reply with the single word: done." {
		t.Errorf("ask = %q", turn.Ask)
	}
	if tool := sourcetest.Tools(turn)[0]; tool.Done || tool.Command != "sleep 20 && echo finished" {
		t.Errorf("tool = %+v", tool)
	}
}

// The log keeps a command's result as the model was given it, its exit code on the first
// line: that is the result's outcome, and the rest its output, as the stream has it.
func TestRecordedLogExitCodes(t *testing.T) {
	root := t.TempDir()
	recorded(t, root, "shell-history")
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.NothingUnread(t, s)
	if s.Stats.Turns != 1 {
		t.Fatalf("turns = %d", s.Stats.Turns)
	}
	turn := s.Turn(t, 1)
	tools := sourcetest.Tools(turn)
	if len(tools) != 2 {
		t.Fatalf("tools = %d", len(tools))
	}
	if echo := tools[0]; echo.Status != model.StatusOK || echo.Exit == nil || *echo.Exit != 0 || echo.Output != "hello-from-shell\n" {
		t.Errorf("echo = %+v", echo)
	}
	if ls := tools[1]; ls.Status != model.StatusError || ls.Exit == nil || *ls.Exit != 2 || ls.Output != "ls: cannot access '/nonexistent-asq-path': No such file or directory\n" {
		t.Errorf("ls = %+v", ls)
	}
	if turn.Failures != 1 {
		t.Errorf("failures = %d", turn.Failures)
	}
	// usage.json's totals, read by the same rule as the stream's
	if s.Stats.Usage.Total() != 22970 {
		t.Errorf("usage = %+v, total %d", s.Stats.Usage, s.Stats.Usage.Total())
	}
}

func TestExitLine(t *testing.T) {
	for in, want := range map[string]struct {
		code int
		rest string
		ok   bool
	}{
		"exit: 0\nhello\n":   {0, "hello\n", true},
		"exit: 127":          {127, "", true},
		"exit: -1\n":         {-1, "", true},
		"exit: done\nhello":  {0, "", false},
		"hello\nexit: 0\n":   {0, "", false},
		"Exit code: 1\nnope": {0, "", false},
	} {
		code, rest, ok := exitLine(in)
		if code != want.code || rest != want.rest || ok != want.ok {
			t.Errorf("exitLine(%q) = %d, %q, %v", in, code, rest, ok)
		}
	}
}
