package web

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The router is run in node against a history of its own: where Back and Esc go is a
// matter of the entries behind a page, which no syntax check sees.
const routerRun = `
const cases = JSON.parse(process.argv[2]);
let entries = [], at = 0, onPop = () => {};
Object.defineProperty(globalThis, 'location', { get: () => new URL(entries[at].url), configurable: true });
globalThis.scrollY = 0;
globalThis.addEventListener = (type, fn) => { if (type === 'popstate') onPop = fn; };
globalThis.document = { addEventListener() {} };
globalThis.history = {
  scrollRestoration: 'auto',
  get state() { return entries[at].state; },
  pushState(state, _, url) { entries.splice(at + 1, Infinity, { state, url: new URL(url, location.href).href }); at++; },
  replaceState(state, _, url) { entries[at] = { state, url: url ? new URL(url, location.href).href : entries[at].url }; },
  go(n) { at = Math.max(0, Math.min(entries.length - 1, at + n)); onPop(); },
};
const router = await import('./router.mjs');
const out = [];
for (const c of cases) {
  entries = [{ state: null, url: new URL(c.from, 'http://asq.test').href }];
  at = 0;
  router.start(() => {}, () => c.home);
  for (const step of c.steps) {
    if (step === 'back') router.back();
    else router.go(step);
  }
  out.push(location.pathname + location.search + location.hash);
}
console.log(JSON.stringify(out));
`

// Back from a session — its button, or Esc — returns to the list or the report it was
// opened from, as it was left: the project it was narrowed to, its window, past the turns
// gone to within the session. A session opened from anywhere else leaves for the
// sessions list the page gives, narrowed to the project it follows.
func TestBackGoesWhereTheSessionWasOpenedFrom(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here to run the router in")
	}
	const report = "/failures?project=%2Fwork%2Fapi&since=30d"
	cases := []struct {
		name  string
		From  string   `json:"from"`
		Steps []string `json:"steps"`
		Home  string   `json:"home"`
		want  string
	}{
		{"from the list", "/?project=%2Fwork%2Fapi&since=7d", []string{"/s/claude/a1", "back"}, "/", "/?project=%2Fwork%2Fapi&since=7d"},
		{"from a search", "/search?q=deploy", []string{"/s/claude/a1#3", "/s/claude/a1#4", "back"}, "/", "/search?q=deploy"},
		{"from a report", report, []string{"/s/claude/a1#2.7", "back"}, "/?project=%2Fwork%2Fapi", report},
		{"from a report, past turns gone to", report, []string{"/s/claude/a1#2.7", "/s/claude/a1#5", "/s/claude/a1#6", "back"}, "/?project=%2Fwork%2Fapi", report},
		{"from each report", "/usage?project=%2Fwork%2Fapi", []string{"/s/claude/a1", "back"}, "/", "/usage?project=%2Fwork%2Fapi"},
		{"from the digest", "/digest?since=24h", []string{"/s/claude/a1", "back"}, "/", "/digest?since=24h"},
		{"from the files", "/files?project=%2Fwork%2Fapi", []string{"/s/claude/a1", "back"}, "/", "/files?project=%2Fwork%2Fapi"},
		{"opened by its address", "/s/claude/a1", []string{"back"}, "/?project=%2Fwork%2Fapi", "/?project=%2Fwork%2Fapi"},
		{"from another session", report, []string{"/s/claude/a1", "/s/claude/x1", "back"}, "/?project=%2Fwork%2Fapi", "/?project=%2Fwork%2Fapi"},
	}
	dir := t.TempDir()
	src, err := fs.ReadFile(Files, "js/router.js")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"router.mjs": string(src), "run.mjs": routerRun} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	arg, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, filepath.Join(dir, "run.mjs"), string(arg))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node said %q (%v)", out, err)
	}
	for i, tc := range cases {
		if got[i] != tc.want {
			t.Errorf("%s: %s then %s ends at %s, want %s", tc.name, tc.From, strings.Join(tc.Steps, ", "), got[i], tc.want)
		}
	}
}
