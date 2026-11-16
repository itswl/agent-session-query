package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/index"
)

// A session edits, writes and reads files in a project that is on this machine; then the
// files move on, and the record is asked how they stand.
func TestFilesSayHowTheyStandNow(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	edited, written, read, gone := filepath.Join(proj, "edited.go"), filepath.Join(proj, "written.txt"), filepath.Join(proj, "read.go"), filepath.Join(proj, "gone.go")
	elsewhere := "/no/such/machine/dir/file.go"

	turnAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	at := func(s int) string { return turnAt.Add(time.Duration(s) * time.Second).Format(time.RFC3339Nano) }
	const id = "eeee5555-0000-4000-8000-000000000005"
	var lines []any
	n := 0
	user := func(ts string, content any) {
		n++
		lines = append(lines, map[string]any{"type": "user", "uuid": "u" + itoa(n), "timestamp": ts, "sessionId": id, "cwd": proj,
			"message": map[string]any{"role": "user", "content": content}})
	}
	use := func(ts, callID, name string, input map[string]any) {
		n++
		lines = append(lines, map[string]any{"type": "assistant", "uuid": "a" + itoa(n), "timestamp": ts, "sessionId": id,
			"message": map[string]any{"id": "msg_" + itoa(n), "role": "assistant", "model": "claude-opus-5", "stop_reason": "tool_use",
				"usage":   map[string]any{"input_tokens": 1, "output_tokens": 1},
				"content": []map[string]any{{"type": "tool_use", "id": callID, "name": name, "input": input}}}})
	}
	result := func(ts, callID, text string, failed bool) {
		user(ts, []map[string]any{{"type": "tool_result", "tool_use_id": callID, "content": text, "is_error": failed}})
	}

	user(at(0), "change the files")
	// Two edits of one file: the first's text the second replaces, so only the last
	// says what the turn left
	use(at(1), "t0", "Edit", map[string]any{"file_path": edited, "old_string": "func old() {}", "new_string": "func temporary() {}"})
	result(at(1), "t0", "ok", false)
	use(at(1), "t1", "Edit", map[string]any{"file_path": edited, "old_string": "func temporary() {}", "new_string": "func brandNew() int { return 42 }"})
	result(at(2), "t1", "ok", false)
	use(at(3), "t2", "Write", map[string]any{"file_path": written, "content": "the whole file\r\nas written\n"})
	result(at(4), "t2", "ok", false)
	use(at(5), "t3", "Read", map[string]any{"file_path": read})
	result(at(6), "t3", "package x", false)
	use(at(7), "t4", "Write", map[string]any{"file_path": gone, "content": "soon gone"})
	result(at(8), "t4", "ok", false)
	use(at(9), "t5", "Write", map[string]any{"file_path": elsewhere, "content": "another machine's"})
	result(at(10), "t5", "ok", false)
	use(at(10), "t5b", "Bash", map[string]any{"command": "go test ./pkg"}) // passed before the failure: not "later"
	result(at(10), "t5b", "ok", false)
	use(at(11), "t6", "Bash", map[string]any{"command": "go test ./..."})
	result(at(12), "t6", "FAIL", true)
	use(at(13), "t7", "Bash", map[string]any{"command": "make lint"})
	result(at(14), "t7", "lint failed", true)
	use(at(15), "t8", "Bash", map[string]any{"command": "go  test   ./..."}) // the same command, spaced differently
	result(at(16), "t8", "FAIL again", true)
	user(at(30), "try again")
	use(at(31), "t8b", "Bash", map[string]any{"command": "go vet ./..."}) // another subcommand: not the same thing passing
	result(at(31), "t8b", "ok", false)
	use(at(31), "t8c", "run_shell_command", map[string]any{"command": "go test ./..."}) // another tool: not the same thing either
	result(at(31), "t8c", "ok", false)
	use(at(31), "t9", "Bash", map[string]any{"command": "go test ./internal/..."}) // the same program and subcommand
	result(at(32), "t9", "ok", false)
	use(at(33), "t10", "Bash", map[string]any{"command": "go test ./..."})
	result(at(34), "t10", "ok", false)

	store := filepath.Join(dir, "claude")
	path := filepath.Join(store, "-proj", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		raw, _ := json.Marshal(l)
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// The files as the session left them, written while it ran
	put := func(p, text string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, turnAt.Add(5*time.Second), turnAt.Add(5*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	put(edited, "package x\n\nfunc brandNew() int { return 42 }\n")
	put(written, "the whole file\r\nas written\r\n") // on disk with the line endings the input had
	put(read, "package x\n")

	a, err := Open(Config{IndexPath: filepath.Join(dir, "index.db"),
		Stores: catalog.Config{Only: []string{"claude"}, Paths: []catalog.Path{{Agent: "claude", Dir: store}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := a.Filter(Query{})
	if err != nil {
		t.Fatal(err)
	}
	states := func() map[string]string {
		t.Helper()
		list, _, err := a.Files(ctx, f, index.Window{}, ".", false, index.Page{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, x := range list {
			if x.Now == nil {
				out[x.Path] = "-"
				continue
			}
			out[x.Path] = x.Now.State + "/" + x.Now.By
		}
		return out
	}
	expect := func(when string, want map[string]string) {
		t.Helper()
		got := states()
		for p, w := range want {
			if got[p] != w {
				t.Errorf("%s: %s is %q, want %q", when, filepath.Base(p), got[p], w)
			}
		}
	}

	expect("as the session left them", map[string]string{
		edited:    "same/content", // the edit's text is there
		written:   "same/content", // the whole file is what was written, line endings aside
		read:      "same/time",    // a file only read is judged by its time
		gone:      "deleted/",     // written by the turn, never put on disk here
		elsewhere: "-",            // not on this machine: nothing to say
	})

	// The files move on
	put(edited, "package x\n\nfunc brandNew() int { return 43 }\n")
	put(written, "the whole file\nas written\nand one more line\n")
	now := time.Now()
	if err := os.Chtimes(read, now, now); err != nil {
		t.Fatal(err)
	}
	expect("after they changed", map[string]string{
		edited:  "changed/content",
		written: "changed/content",
		read:    "written/time",
	})

	// A file put back as the turn left it reads as such, whatever its time says
	if err := os.WriteFile(edited, []byte("package x\n\nfunc brandNew() int { return 42 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect("put back", map[string]string{edited: "same/content"})

	// Where the same command next succeeded: not the failure's own retry, which failed
	// too; not a different command; the run of the next turn that passed
	rep, err := a.Failures(ctx, f, index.Window{}, "", index.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	later := map[string]*index.Later{}
	for _, x := range rep.Failures {
		later[x.Command+"@"+itoa(x.Seq)] = x.Later
	}
	if len(rep.Failures) != 3 {
		t.Fatalf("%d failures, want 3", len(rep.Failures))
	}
	for _, x := range rep.Failures {
		switch {
		case x.Command == "make lint":
			if x.Later != nil {
				t.Errorf("make lint never ran again, and passed later at %+v", x.Later)
			}
		case strings.Contains(x.Command, "go"):
			// The first go test that passed: a narrower one than failed, and shown as it ran
			if x.Later == nil || x.Later.Turn != 2 || !x.Later.At.After(x.At) || x.Later.Command != "go test ./internal/..." {
				t.Errorf("%q at step %d: passed later = %+v, want the second turn's first passing go test", x.Command, x.Seq, x.Later)
			}
		}
	}
}

func itoa(i int) string {
	raw, _ := json.Marshal(i)
	return string(raw)
}

// What a write left in a file, read from the input as each CLI writes it.
func TestWhatAWriteLeft(t *testing.T) {
	for name, tc := range map[string]struct {
		input     string
		path      string
		whole     string
		fragments []string
		ok        bool
	}{
		"an edit":                   {input: `{"file_path":"/p/a.go","old_string":"x","new_string":"func y() {}"}`, path: "/p/a.go", fragments: []string{"func y() {}"}, ok: true},
		"an edit that only cuts":    {input: `{"file_path":"/p/a.go","old_string":"func y() {}","new_string":""}`, path: "/p/a.go"},
		"an edit too small to tell": {input: `{"file_path":"/p/a.go","old_string":"{","new_string":"}"}`, path: "/p/a.go"},
		"several edits":             {input: `{"file_path":"/p/a.go","edits":[{"old_string":"a","new_string":"alpha := 1"},{"oldString":"b","newString":"beta := 2"}]}`, path: "/p/a.go", fragments: []string{"alpha := 1", "beta := 2"}, ok: true},
		"OpenCode's edit":           {input: `{"filePath":"/p/a.go","oldString":"a","newString":"gamma := 3"}`, path: "/p/a.go", fragments: []string{"gamma := 3"}, ok: true},
		"a whole file":              {input: `{"file_path":"/p/b.txt","content":"line one\r\nline two\n"}`, path: "/p/b.txt", whole: "line one\nline two\n", ok: true},
		"a patch that updates": {input: `{"input":"*** Begin Patch\n*** Update File: src/a.go\n@@ func a\n-old line\n+new line one\n+new line two\n context\n+another added\n*** End Patch"}`,
			path: "/repo/src/a.go", fragments: []string{"new line one\nnew line two", "another added"}, ok: true},
		"a patch that adds": {input: `{"command":["apply_patch","*** Begin Patch\n*** Add File: b.txt\n+first\n+second\n*** End Patch"]}`,
			path: "b.txt", whole: "first\nsecond", ok: true},
		"a patch to another file": {input: `{"input":"*** Begin Patch\n*** Update File: other.go\n+something long enough\n*** End Patch"}`, path: "/repo/a.go"},
		"a patch that moves":      {input: `{"input":"*** Begin Patch\n*** Update File: a.go\n*** Move to: b.go\n+something long enough\n*** End Patch"}`, path: "a.go"},
		"an input not known here": {input: `{"file_path":"/p/a.go","mode":"truncate"}`, path: "/p/a.go"},
		"not JSON":                {input: `nonsense`, path: "/p/a.go"},
	} {
		whole, fragments, ok := written(json.RawMessage(tc.input), tc.path)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", name, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if (whole != nil) != (tc.whole != "") || (whole != nil && *whole != tc.whole) {
			t.Errorf("%s: whole = %v, want %q", name, whole, tc.whole)
		}
		if strings.Join(fragments, "|") != strings.Join(tc.fragments, "|") {
			t.Errorf("%s: fragments = %q, want %q", name, fragments, tc.fragments)
		}
	}
}

// What counts as running a command again.
func TestCommandKey(t *testing.T) {
	for in, want := range map[string]string{
		"go test ./...":                        "go test",
		"go  test   -run TestX ./internal/x":   "go test",
		"cd /work/api && go test ./... | tail": "go test",
		"cd /work/api; make lint":              "make lint",
		"FOO=1 BAR=2 pnpm build --prod":        "pnpm build",
		"make":                                 "make",
		"./run.sh --fast":                      "./run.sh",
		"git push origin main":                 "git push",
		"python3 -c 'print(1)'":                "python3",
		"$(which go) test":                     "",
		"":                                     "",
		"   ":                                  "",
	} {
		if got := commandKey(in); got != want {
			t.Errorf("commandKey(%q) = %q, want %q", in, got, want)
		}
	}
}
