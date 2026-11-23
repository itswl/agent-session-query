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
