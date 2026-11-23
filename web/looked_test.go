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

const lookedRun = `
import { looked } from './format.mjs';
const cases = JSON.parse(process.argv[2]);
console.log(JSON.stringify(cases.map((c) => (looked(c.health, c.sync) || []).map((s) => s.source))));
`

// The sessions list says where asq looked when no store it reads held a session — and not
// while a first read is under way, nor when any store holds one.
func TestTheListSaysWhereItLookedWhenNothingWasFound(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here to run the page's format in")
	}
	none := map[string]any{"stores": []map[string]any{
		{"source": "claude", "found": false, "transcripts": 0},
		{"source": "codex", "found": true, "transcripts": 0},
	}}
	some := map[string]any{"stores": []map[string]any{
		{"source": "claude", "found": true, "transcripts": 0},
		{"source": "codex", "found": true, "transcripts": 3},
	}}
	cases := []struct {
		name   string
		Health any `json:"health"`
		Sync   any `json:"sync"`
		want   string
	}{
		{"nothing anywhere", none, map[string]any{"running": false}, "claude codex"},
		{"nothing yet, while reading", none, map[string]any{"running": true}, ""},
		{"one store holds some", some, nil, ""},
		{"no report yet", nil, nil, ""},
	}
	dir := t.TempDir()
	src, err := fs.ReadFile(Files, "js/format.js")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"format.mjs": string(src), "run.mjs": lookedRun} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	arg, _ := json.Marshal(cases)
	cmd := exec.Command(node, filepath.Join(dir, "run.mjs"), string(arg))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got [][]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node said %q (%v)", out, err)
	}
	for i, tc := range cases {
		if strings.Join(got[i], " ") != tc.want {
			t.Errorf("%s: looked in %q, want %q", tc.name, got[i], tc.want)
		}
	}
}
