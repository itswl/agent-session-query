package copilot

import (
	"os"
	"path/filepath"
	"testing"
)

// The documents are what the yaml package's stringify (2.9.1) writes for these fields —
// the serializer Copilot CLI's JavaScript writes workspace.yaml with — and the shapes the
// runtime's serde_yaml writes besides: single quotes, escapes, block scalars.
func TestWorkspaceYAML(t *testing.T) {
	for _, c := range []struct {
		name string
		doc  string
		want map[string]string
	}{
		{"plain", "id: c0ffee00\ncwd: /work/alpha\nname: Fix the flaky order test\nuser_named: false\nsummary_count: 0\ncreated_at: 2026-11-17T09:00:00.000Z\n",
			map[string]string{"id": "c0ffee00", "cwd": "/work/alpha", "name": "Fix the flaky order test", "user_named": "false", "summary_count": "0", "created_at": "2026-11-17T09:00:00.000Z"}},
		{"folded past the line's width", "id: x\nname: Investigate why the payments service drops the sort order of line items in\n  the order test and fix it\ncwd: /work/a b\n",
			map[string]string{"id": "x", "name": "Investigate why the payments service drops the sort order of line items in the order test and fix it", "cwd": "/work/a b"}},
		{"quoted for a colon, and a quote that needs none", "id: y\nname: \"key: value with a colon # and a hash\"\nsummary: it's 'quoted'\ncwd: C:\\Users\\me\\work\n",
			map[string]string{"id": "y", "name": "key: value with a colon # and a hash", "summary": "it's 'quoted'", "cwd": `C:\Users\me\work`}},
		{"two lines, and a word that would read as true", "id: z\nname: |-\n  two\n  lines\nsummary: \"true\"\ncwd: /w/ünïcödé — dash\n",
			map[string]string{"id": "z", "name": "two\nlines", "summary": "true", "cwd": "/w/ünïcödé — dash"}},
		{"single-quoted and folded", "id: w\nname: '\"double\" quotes and a very long title that goes on and on past the eighty\n  column line width limit'\nsummary: \"- dash first\"\n",
			map[string]string{"id": "w", "name": `"double" quotes and a very long title that goes on and on past the eighty column line width limit`, "summary": "- dash first"}},
		// serde_yaml's shapes
		{"escapes", "name: \"tab\\there \\u00e9 \\\"q\\\" back\\\\slash\"\nsummary: 'it''s'\n",
			map[string]string{"name": "tab\there é \"q\" back\\slash", "summary": "it's"}},
		{"an escaped line break", "name: \"one \\\n  two\"\n", map[string]string{"name": "one two"}},
		{"folded block", "name: >-\n  one\n  two\n\n  three\n", map[string]string{"name": "one two\nthree"}},
		{"null and nothing", "name: ~\nsummary:\ncwd: null\n", map[string]string{"name": "", "summary": "", "cwd": ""}},
		{"a comment and a document marker", "---\n# the session\nname: plain # a note\n", map[string]string{"name": "plain"}},
		{"a nested collection is no scalar", "name: n\nlimits:\n  turns: 3\ntags:\n  - a\nflow: {a: 1}\ncwd: /w\n",
			map[string]string{"name": "n", "cwd": "/w"}},
		{"never closed", "name: \"open\nsummary: 'open\ncwd: /w\n", map[string]string{"cwd": "/w"}},
		{"bad escapes", "name: \"\\q\"\nsummary: \"\\x4\"\ncwd: \"\\uD800\"\n", map[string]string{}},
		{"escapes past the last character", "name: \"\\U00110000\"\nsummary: \"\\U80000041\"\ncwd: \"\\UFFFFFFFF\"\nid: \"\\U0010FFFF\"\n",
			map[string]string{"id": "\U0010FFFF"}},
		{"windows line ends", "name: crlf\r\ncwd: /w\r\n", map[string]string{"name": "crlf", "cwd": "/w"}},
	} {
		got := yamlScalars(c.doc)
		if len(got) != len(c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", c.name, k, got[k], v)
			}
		}
	}
}

// The name comes first, as in the CLI's own list of sessions; a file that is not there, or
// is far past what the CLI writes, says nothing.
func TestReadWorkspace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.yaml")
	if ws := readWorkspace(path); ws != (workspace{}) {
		t.Errorf("no file: %+v", ws)
	}
	if err := os.WriteFile(path, []byte("summary: the summary\nname: the name\ncwd: /w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ws := readWorkspace(path); ws != (workspace{Name: "the name", Summary: "the summary", Cwd: "/w"}) {
		t.Errorf("workspace = %+v", ws)
	}
	big := make([]byte, maxWorkspace+1)
	copy(big, "name: big\n")
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if ws := readWorkspace(path); ws != (workspace{}) {
		t.Errorf("a file past the bound: %+v", ws)
	}
}
