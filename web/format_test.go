package web

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/txt"
)

// The page says what the command line says: a wait the same length, cut down the same way —
// "passed 23s later" in both, not 23s in one and 24s in the other — and failures counted in
// the same words, red only while one is open.
func TestThePageSaysWhatTheCommandLineSays(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here to run the page's formatting in")
	}
	dir := t.TempDir()
	src, err := fs.ReadFile(Files, "js/format.js")
	if err != nil {
		t.Fatal(err)
	}
	run := `const f = await import('./format.mjs');
const waits = JSON.parse(process.argv[2]);
console.log(JSON.stringify({
  waits: waits.map((ms) => f.dur(ms)),
  failed: [[1, 0], [2, 1], [1, 1], [3, 3]].map(([failures, passedLater]) => [f.failed({ failures, passedLater }), f.open({ failures, passedLater })]),
}));`
	for name, body := range map[string]string{"format.mjs": string(src), "run.mjs": run} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	waits := []time.Duration{10 * time.Second, 23600 * time.Millisecond, 23999 * time.Millisecond, 59999 * time.Millisecond, 999 * time.Millisecond}
	ms := make([]float64, len(waits))
	for i, d := range waits {
		ms[i] = float64(d.Milliseconds())
	}
	arg, _ := json.Marshal(ms)
	cmd := exec.Command(node, filepath.Join(dir, "run.mjs"), string(arg))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Waits  []string
		Failed [][2]any
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("node said %q (%v)", out, err)
	}
	for i, d := range waits {
		if want := txt.Duration(d); got.Waits[i] != want {
			t.Errorf("a wait of %v reads %q on the page and %q on the command line", d, got.Waits[i], want)
		}
	}
	for i, want := range [][2]any{{"1 failed", 1.0}, {"2 failed, 1 passed later", 1.0}, {"1 failed, passed later", 0.0}, {"3 failed, all passed later", 0.0}} {
		if got.Failed[i] != want {
			t.Errorf("failures: the page says %v, want %v (words, and how many open)", got.Failed[i], want)
		}
	}
}

// format.js is run in node: which line a row of the list shows, how a turn names the files
// it changed and how agent time reads are decisions no syntax check sees.
const formatRun = `
const f = await import('./format.mjs');
const cases = JSON.parse(process.argv[2]);
console.log(JSON.stringify(cases.map(([fn, ...args]) => f[fn](...args))));
`

func TestFormat(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here to run format.js in")
	}
	const ask = "The invoices list endpoint takes 4 seconds for large accounts. Look at the query plan and fix it."
	cases := []struct {
		name string
		call []any
		want any
	}{
		// A session its CLI did not name is titled by its first ask, cut to a line: as its
		// last ask too, it is not said twice
		{"a title cut from the ask", []any{"repeats", "The invoices list endpoint takes 4 seconds for large accounts. Look at the query…", ask}, true},
		{"a title that is the ask's first line", []any{"repeats", "Fix the build", "Fix the build\n\nIt fails on CI since the bump."}, true},
		{"spaces as the title folds them", []any{"repeats", "Fix  the   build…", "Fix the build and the tests"}, true},
		{"a title of its own", []any{"repeats", "Cap webhook retries with exponential backoff", "Failed webhook deliveries are retried every minute."}, false},
		{"a later ask", []any{"repeats", "Fix the build", "now the docs"}, false},
		{"no title", []any{"repeats", "", ask}, false},
		{"no ask", []any{"repeats", "Fix the build", nil}, false},

		{"beneath the directory", []any{"beneath", "/work/api/web/app.js", "/work/api"}, "web/app.js"},
		{"beside it, by its name", []any{"beneath", "/work/api-old/main.go", "/work/api"}, "/work/api-old/main.go"},
		{"on Windows", []any{"beneath", `C:\work\api\cmd\main.go`, `C:\work\api`}, `cmd\main.go`},
		{"no directory", []any{"beneath", "/work/api/main.go", nil}, "/work/api/main.go"},

		// How long agents worked, in hours however many: 38 of them are not a day and a half
		{"seconds of work", []any{"work", 45_000}, "45s"},
		{"minutes of work", []any{"work", 12 * 60_000}, "12m"},
		{"hours of work", []any{"work", (38*60 + 20) * 60_000}, "38h 20m"},
		{"no work", []any{"work", 0}, ""},

		// How a turn ended, as its head says it: a turn whose work ended on an error says so
		// with the error's first line, and the line whole on hover — not fine, not failed,
		// which is for calls, and not interrupted when it was stopped too. The turn the agent
		// is at says where it stands
		{"a turn that ended on an error", []any{"turnStatus", map[string]any{"error": "402 Insufficient Balance (insufficient_balance)\n  at send (client.js:42)"}},
			[]any{"error", "error: 402 Insufficient Balance (insufficient_balance)", "402 Insufficient Balance (insufficient_balance)"}},
		{"an error, and stopped too", []any{"turnStatus", map[string]any{"error": "quota", "interrupted": true, "failures": 2}}, []any{"error", "error: quota", "quota"}},
		{"an error, and the agent at it", []any{"turnStatus", map[string]any{"error": "quota"}, "working"}, []any{"working", "working", ""}},
		{"calls that failed", []any{"turnStatus", map[string]any{"failures": 2, "passedLater": 1}}, []any{"failed", "2 failed calls, 1 passed later", ""}},
		{"stopped", []any{"turnStatus", map[string]any{"interrupted": true}}, []any{"interrupted", "interrupted", ""}},
		{"failed, and passed later", []any{"turnStatus", map[string]any{"failures": 1, "passedLater": 1}}, []any{"passed", "1 failed call, passed later", ""}},
		{"fine", []any{"turnStatus", map[string]any{}}, []any{"fine", "fine", ""}},

		// The turns x goes to: a failed call still open, or work that ended on an error
		{"a call still failing", []any{"wentWrong", map[string]any{"failures": 2, "passedLater": 1}}, true},
		{"an error it ended on", []any{"wentWrong", map[string]any{"error": "quota"}}, true},
		{"failures all passed later", []any{"wentWrong", map[string]any{"failures": 1, "passedLater": 1}}, false},
		{"stopped, nothing failed", []any{"wentWrong", map[string]any{"interrupted": true}}, false},
	}
	calls := make([][]any, len(cases))
	for i, tc := range cases {
		calls[i] = tc.call
	}
	dir := t.TempDir()
	src, err := fs.ReadFile(Files, "js/format.js")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"format.mjs": string(src), "run.mjs": formatRun} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	arg, err := json.Marshal(calls)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, filepath.Join(dir, "run.mjs"), string(arg))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got []any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node said %q (%v)", out, err)
	}
	for i, tc := range cases {
		if !reflect.DeepEqual(got[i], tc.want) {
			t.Errorf("%s: %s(%q) = %#v, want %#v", tc.name, tc.call[0], tc.call[1:], got[i], tc.want)
		}
	}
}

// The record's input is the part of the prompt neither read from cache nor written to it;
// "in", on the command line and a session's page, is the whole prompt. A view of tokens that
// labels the record's figure Input says 619 where the command line says 6.0M. (A tool call's
// Input, in a turn's steps, is what the tool was given.)
func TestInputIsNotThePrompt(t *testing.T) {
	label := regexp.MustCompile(`['"]Input['"]`)
	all := files(t)
	for _, name := range []string{"js/views/usage.js", "js/views/session.js", "js/views/digest.js"} {
		if label.MatchString(all[name]) {
			t.Errorf("%s labels a figure of tokens Input: the record's input is new input, the prompt is in", name)
		}
	}
}
