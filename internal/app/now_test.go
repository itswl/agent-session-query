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
	big, bigText := filepath.Join(proj, "big.go"), strings.Repeat("x := 1 // a line of a large file\n", 400)

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
	// Calls asked for in one response, which run side by side: a row each, as Claude
	// Code writes them, under the response's one id
	together := func(msg, ts string, block map[string]any) {
		n++
		lines = append(lines, map[string]any{"type": "assistant", "uuid": "a" + itoa(n), "timestamp": ts, "sessionId": id,
			"message": map[string]any{"id": msg, "role": "assistant", "model": "claude-opus-5", "stop_reason": "tool_use",
				"usage":   map[string]any{"input_tokens": 1, "output_tokens": 1},
				"content": []map[string]any{block}}})
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
	use(at(9), "t5big", "Write", map[string]any{"file_path": big, "content": bigText}) // past what a turn keeps of an input
	result(at(9), "t5big", "ok", false)
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
	// Two builds side by side: the one that passed did not pass after the one that failed
	// (the second row written after the first call's answer came back)
	together("msg_par", at(17), map[string]any{"type": "tool_use", "id": "p1", "name": "Bash", "input": map[string]any{"command": "cargo build -p core"}})
	result(at(18), "p1", "error[E0308]: mismatched types", true)
	together("msg_par", at(19), map[string]any{"type": "tool_use", "id": "p2", "name": "Bash", "input": map[string]any{"command": "cargo build -p cli"}})
	result(at(20), "p2", "Finished", false)
	user(at(30), "try again")
	// Runs that end without saying the command passed: its status hidden by a pipe, and
	// started in the background
	use(at(30), "t8d", "Bash", map[string]any{"command": "go test ./... 2>&1 | tail -5"})
	result(at(30), "t8d", "--- FAIL: TestX\nFAIL", false)
	use(at(30), "t8e", "Bash", map[string]any{"command": "go test ./...", "run_in_background": true})
	result(at(30), "t8e", "Command running in background with ID: bash_1", false)
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
	// Another machine's sessions, which name paths that are here too: one in a labelled
	// store, one whose directory is not on this machine
	mine, theirs := filepath.Join(proj, "box2.go"), filepath.Join(proj, "other.go")
	other := func(file, sid, cwd, target string) {
		t.Helper()
		row := func(v map[string]any) string { raw, _ := json.Marshal(v); return string(raw) }
		text := row(map[string]any{"type": "user", "uuid": "o1", "timestamp": at(0), "sessionId": sid, "cwd": cwd,
			"message": map[string]any{"role": "user", "content": "write it"}}) + "\n" +
			row(map[string]any{"type": "assistant", "uuid": "o2", "timestamp": at(1), "sessionId": sid,
				"message": map[string]any{"id": "msg_o", "role": "assistant", "model": "claude-opus-5", "stop_reason": "tool_use",
					"content": []map[string]any{{"type": "tool_use", "id": "o", "name": "Write", "input": map[string]any{"file_path": target, "content": "theirs"}}}}}) + "\n" +
			row(map[string]any{"type": "user", "uuid": "o3", "timestamp": at(2), "sessionId": sid,
				"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "o", "content": "ok"}}}}) + "\n"
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	box2 := filepath.Join(dir, "box2")
	other(filepath.Join(box2, "-proj", "ffff6666-0000-4000-8000-000000000006.jsonl"), "ffff6666-0000-4000-8000-000000000006", proj, mine)
	other(filepath.Join(store, "-gone", "aaaa7777-0000-4000-8000-000000000007.jsonl"), "aaaa7777-0000-4000-8000-000000000007", "/no/such/machine/proj", theirs)

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
	put(big, bigText)
	put(mine, "theirs")
	put(theirs, "theirs")

	a, err := Open(Config{IndexPath: filepath.Join(dir, "index.db"),
		Stores: catalog.Config{Only: []string{"claude"}, Paths: []catalog.Path{{Agent: "claude", Dir: store}, {Agent: "claude", Label: "box2", Dir: box2}}}})
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
		big:       "same/content", // its write read whole, past the preview the turn keeps
		gone:      "deleted/",     // written by the turn, never put on disk here
		elsewhere: "-",            // not on this machine: nothing to say
		mine:      "-",            // a labelled store's session: another machine's, whatever is here
		theirs:    "-",            // a session whose directory is not here: likewise
	})

	// A formatter the turn ran changed what it wrote, while it ran: the file is as the
	// turn ended it, though its text is not what the edit said
	put(edited, "package x\n\nfunc brandNew() int {\n\treturn 42\n}\n")
	expect("formatted within the turn", map[string]string{edited: "same/time"})

	// The files move on, after the turn
	since := func(p, text string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	since(edited, "package x\n\nfunc brandNew() int { return 43 }\n")
	since(written, "the whole file\nas written\nand one more line\n")
	now := time.Now()
	if err := os.Chtimes(read, now, now); err != nil {
		t.Fatal(err)
	}
	expect("after they changed", map[string]string{
		edited:  "changed/content",
		written: "changed/content",
		read:    "written/time",
	})

	// Written since, with what the edit put there still in it: that it is all there is
	// known, and that nothing else changed is not — only a file the turn wrote whole, and
	// that is that to the byte, is as the turn left it
	since(edited, "package x\n\n// a comment added since\nfunc brandNew() int { return 42 }\n")
	since(written, "the whole file\nas written\n")
	expect("written since", map[string]string{edited: "written/content", written: "same/content"})

	// Where the same command next succeeded: not the failure's own retry, which failed
	// too; not a different command; the run of the next turn that passed
	rep, err := a.Index.Failures(ctx, f, index.Window{}, index.FailureQuery{}, index.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	later := map[string]*index.Later{}
	for _, x := range rep.Failures {
		later[x.Command+"@"+itoa(x.Seq)] = x.Later
	}
	if len(rep.Failures) != 4 {
		t.Fatalf("%d failures, want 4", len(rep.Failures))
	}
	for _, x := range rep.Failures {
		switch {
		case x.Command == "make lint":
			if x.Later != nil {
				t.Errorf("make lint never ran again, and passed later at %+v", x.Later)
			}
		case strings.HasPrefix(x.Command, "cargo"):
			if x.Later != nil {
				t.Errorf("a build beside the failed one passed later at %+v", x.Later)
			}
		case strings.Contains(x.Command, "go"):
			// The first go test that passed: a narrower one than failed, and shown as it ran
			if x.Later == nil || x.Later.Turn != 2 || !x.Later.At.After(x.At) || x.Later.Command != "go test ./internal/..." {
				t.Errorf("%q at step %d: passed later = %+v, want the second turn's first passing go test", x.Command, x.Seq, x.Later)
			}
		}
	}

	// The brief says what the two reports say: the failures that did not pass later as
	// open ones, those that did as where they passed; each changed file as it stands now
	s, _, err := a.Resolve(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	o, err := BriefOf(0, 0, "all")
	if err != nil {
		t.Fatal(err)
	}
	brief, err := a.Brief(ctx, &s, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## The whole session\n",
		" change the files · 4 failed, 2 passed later\n",
		"- Failed: Bash `make lint` — error [step ", "  lint failed\n",
		"- Failed: Bash `cargo build -p core` — error [step ", "  error[E0308]: mismatched types\n",
		"- Failed, passed later: Bash `go test ./...` — error [step ",
		"later: #2 step ", ", as `go test ./internal/...`\n",
		"  - edited.go (#1) · now: written since — what turn 1 wrote is still in it\n",
		"  - edited.go · now: written since — what this turn wrote is still in it\n",
		"  - written.txt · now: as this turn left it\n",
		"  - gone.go · now: deleted\n",
		"  - big.go · now: as this turn left it\n",
		"  - " + elsewhere + "\n", // another machine's: nothing said of it
		"- Read: read.go\n",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief: no %q in\n%s", want, brief)
		}
	}
	if n := strings.Count(brief, "- Failed, passed later: Bash `go test ./...`"); n != 2 {
		t.Errorf("%d of the two go test failures that passed later say so:\n%s", n, brief)
	}
	if strings.Contains(brief, "FAIL again") {
		t.Errorf("a failure that passed later quotes its output, as if it were still the question:\n%s", brief)
	}

	// A search's hit says the same of its turn: four calls failed, and the two go tests
	// passed later
	res, err := a.Search(ctx, index.ParseQuery("change files"), f, index.SearchOptions{})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Failed != 4 || res.Hits[0].PassedLater != 2 {
		t.Errorf("hits = %+v, %v: want the first turn, its four failures, two passed later", res.Hits, err)
	}

	// And the digest, in all, for the project and for the session it names
	digest, err := a.Index.Digest(ctx, f, index.Window{})
	if err != nil {
		t.Fatal(err)
	}
	var d index.Digest
	var listed index.DigestSession
	for _, p := range digest.Projects {
		for _, e := range p.List {
			if e.Ref == "claude/"+id {
				d, listed = p, e
			}
		}
	}
	for name, got := range map[string]int{"in all": digest.Total.PassedLater, "the project": d.PassedLater, "the session": listed.PassedLater} {
		if got != 2 {
			t.Errorf("digest, %s: passed later = %d, want 2 of the 4 failed", name, got)
		}
	}
	if d.Failures != 4 || listed.Failures != 4 || listed.Ref != "claude/"+id || listed.Turns != 2 {
		t.Errorf("digest's session = %+v", listed)
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
