package web

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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

// format.js is run in node: which line a row of the list shows and how a turn names the
// files it changed are decisions no syntax check sees.
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
		if got[i] != tc.want {
			t.Errorf("%s: %s(%q) = %#v, want %#v", tc.name, tc.call[0], tc.call[1:], got[i], tc.want)
		}
	}
}
