package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

// git runs git in dir as a test's own: no configuration of the machine's, a made-up name.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test",
		"-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+empty)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// claudeSession writes a Claude Code session of one turn into the fixture's store: an ask,
// a Bash call that printed output, and a reply.
func claudeSession(t *testing.T, h fixture.History, id, cwd, ask, output, reply string) {
	t.Helper()
	at := h.Now.Add(-time.Minute)
	var b strings.Builder
	for i, row := range []map[string]any{
		{"type": "user", "message": map[string]any{"role": "user", "content": ask}},
		{"type": "assistant", "message": map[string]any{"role": "assistant", "id": "m1", "model": "claude-opus",
			"content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "git commit -am 'trim titles'"}}}}},
		{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": output}}}},
		{"type": "assistant", "message": map[string]any{"role": "assistant", "id": "m2", "model": "claude-opus",
			"content": []map[string]any{{"type": "text", "text": reply}}}},
	} {
		row["uuid"], row["sessionId"], row["cwd"] = id+"-"+string(rune('a'+i)), id, cwd
		row["timestamp"] = at.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	path := filepath.Join(h.Dir, "-work-notes", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A line of a file goes back, through the commit git blame says last changed it, to the
// turn whose call made that commit — by the seven characters git commit printed of its
// id — and to the turns that only name it. A line committed outside any session is said to
// be; one not committed yet has no commit; and a file outside a repository is git's error.
func TestWhy(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on the PATH")
	}
	repo := t.TempDir()
	file := filepath.Join(repo, "notes.go")
	write := func(text string) {
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "init", "-q")
	write("package notes\n\nfunc Title(s string) string {\n\treturn s\n}\n")
	git(t, repo, "add", "notes.go")
	git(t, repo, "commit", "-q", "-m", "Add notes package")
	write("package notes\n\nimport \"strings\"\n\nfunc Title(s string) string {\n\treturn strings.TrimSpace(s)\n}\n")
	printed := git(t, repo, "commit", "-am", "Trim the title before it is stored")
	full := git(t, repo, "rev-parse", "HEAD")
	if !strings.HasPrefix(printed, "[main "+full[:7]) {
		t.Fatalf("git commit printed %q", printed)
	}

	h := fixture.Write(t)
	const madeIt, namedIt = "cccc3333-0000-4000-8000-000000000003", "dddd4444-0000-4000-8000-000000000004"
	claudeSession(t, h, madeIt, repo, "trim the titles, then commit", printed, "Committed.")
	claudeSession(t, h, namedIt, repo, "what was the last commit?", "commit "+full+"\nAuthor: test", "It trims titles.")
	// A commit whose hooks printed pages before git's line, and whose list of files ran on
	// after it: the line is in the part of the output a turn keeps no preview of
	const madeLoudly = "eeee5555-0000-4000-8000-000000000005"
	claudeSession(t, h, madeLoudly, repo, "commit it, hooks and all",
		strings.Repeat("pre-commit: checking formatting of the package ... ok\n", 40)+printed+"\n"+
			strings.Repeat(" create mode 100644 testdata/titles/case.txt\n", 120), "Committed; the hooks passed.")
	a, err := app.Open(h.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	ctx := context.Background()
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}

	why, err := a.Why(ctx, file, 6)
	if err != nil {
		t.Fatal(err)
	}
	if why.Commit != full || why.Subject != "Trim the title before it is stored" || why.Text != "\treturn strings.TrimSpace(s)" || why.At.IsZero() {
		t.Errorf("blame = %+v", why.Blame)
	}
	var made []string
	for _, h := range why.Made {
		made = append(made, h.Ref)
	}
	slices.Sort(made)
	if want := []string{"claude/" + madeIt + "#1", "claude/" + madeLoudly + "#1"}; !slices.Equal(made, want) {
		t.Errorf("made = %q, want %q", made, want)
	}
	if len(why.Mentioned) != 1 || !strings.HasPrefix(why.Mentioned[0].Ref, "claude/"+namedIt+"#1") {
		t.Errorf("mentioned = %+v", why.Mentioned)
	}

	why, err = a.Why(ctx, file, 1)
	if err != nil || why.Subject != "Add notes package" || len(why.Made) != 0 || len(why.Mentioned) != 0 {
		t.Errorf("line 1 = %+v, %v: committed outside any session", why, err)
	}

	write("package notes // changed, not committed\n\nimport \"strings\"\n")
	if why, err = a.Why(ctx, file, 1); err != nil || why.Commit != "" || why.Text != "package notes // changed, not committed" || why.Made != nil {
		t.Errorf("a line not committed = %+v, %v", why, err)
	}

	if _, err := a.Why(ctx, file, 99); err == nil || !strings.HasPrefix(err.Error(), "git blame: ") {
		t.Errorf("past the end = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "loose.go")
	if err := os.WriteFile(outside, []byte("package loose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Why(ctx, outside, 1); err == nil || !strings.HasPrefix(err.Error(), "git blame: ") {
		t.Errorf("outside a repository = %v", err)
	}
}

// Over HTTP — the API's route, and the MCP server's — the path names no directory a
// process was started in, and the server would run git where it is told to: so it must be
// whole, and under a directory a session ran in, by either spelling of it. What is wrong
// with such a request is the caller's to set right (app.Refused), as is what git refuses of
// the file; git missing from the machine is the server's own to say.
func TestWhyOverHTTP(t *testing.T) {
	a, c := fixture.OpenCommit(t)
	ctx := context.Background()
	refused := func(err error) bool {
		var r app.Refused
		return errors.As(err, &r)
	}

	// Whole, and under the directory the session ran in: the command line's answer, its
	// turn as a list shows it — which every face that answers with one reads
	why, err := a.WhyOverHTTP(ctx, c.File, 1)
	if err != nil || why.Path != c.File || why.Commit == "" || len(why.Made) != 1 || why.Made[0].Ref != "claude/"+fixture.Committed+"#1" {
		t.Fatalf("why = %+v, %v", why, err)
	}
	if h := why.Made[0]; h.Session.Resume == "" || h.Session.LastAsk != "" || h.Session.Answer != "" {
		t.Errorf("the turn is not as a search lists it: resume %q, last ask %q, answer %q", h.Session.Resume, h.Session.LastAsk, h.Session.Answer)
	}
	// So is a file beneath it
	deep := filepath.Join(c.Dir, "doc", "notes.md")
	if why, err = a.WhyOverHTTP(ctx, deep, 1); !refused(err) || why.Path != deep || !strings.HasPrefix(err.Error(), "git blame: ") {
		t.Errorf("beneath the directory = %+v, %v: git's word about the file, and only that", why.Path, err)
	}
	// And one spelled through a link to it, as /tmp is /private/tmp on a Mac — where a
	// link can be made
	link := filepath.Join(t.TempDir(), "here")
	if err := os.Symlink(c.Dir, link); err == nil {
		if why, err = a.WhyOverHTTP(ctx, filepath.Join(link, "notes.md"), 1); err != nil || len(why.Made) != 1 {
			t.Errorf("through a link = %+v, %v", why, err)
		}
	}

	// A relative path names no directory the server could know
	if _, err = a.WhyOverHTTP(ctx, "notes.md", 1); !refused(err) || !strings.Contains(err.Error(), `path "notes.md" is relative`) {
		t.Errorf("a relative path = %v", err)
	}
	// And the directory of a session is where the history gives git to look: elsewhere
	// the server would run it wherever a caller says
	outside := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(outside, []byte("in no session's directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Nor through a link inside one that leads out of it — of a file that is there, or of
	// one in a directory that is not: where git would run is what counts
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(c.Dir, "elsewhere")); err == nil {
		for _, through := range []string{"notes.md", filepath.Join("not-there-yet", "notes.md")} {
			if why, err = a.WhyOverHTTP(ctx, filepath.Join(c.Dir, "elsewhere", through), 1); !refused(err) || !strings.Contains(err.Error(), "under no directory") {
				t.Errorf("through a link out of the directory, %s = %+v, %v", through, why, err)
			}
		}
	}
	// In a way that says where they are listed
	if _, err = a.WhyOverHTTP(ctx, outside, 1); !refused(err) || !strings.Contains(err.Error(), "under no directory a session asq reads ran in") ||
		!strings.Contains(err.Error(), "projects lists them") {
		t.Errorf("outside every session's directory = %v", err)
	}

	// What is wrong with the question is the caller's: asq's word of the line, git's of
	// the file
	for _, tc := range []struct {
		line int
		want string
	}{
		{0, "line 0: lines are counted from 1"},
		{99, "git blame: file notes.md has only 1 line"},
	} {
		if _, err = a.WhyOverHTTP(ctx, c.File, tc.line); !refused(err) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("line %d = %v, want the caller's: %s", tc.line, err, tc.want)
		}
	}
}

// What git refuses is the caller's to set right — on the command line's terms too, since
// the HTTP API answers it 400 — and it is the caller's wherever the file is: not committed
// at all is not a repository, and one that names no file names none.
func TestWhyRefusesInTheCallersWords(t *testing.T) {
	a, _ := fixture.Open(t)
	ctx := context.Background()
	refused := func(err error) bool {
		var r app.Refused
		return errors.As(err, &r)
	}
	if _, err := a.Why(ctx, "notes.go", 0); !refused(err) || err.Error() != "line 0: lines are counted from 1" {
		t.Errorf("line 0 = %v", err)
	}
	if _, err := a.Why(ctx, "notes.go", -3); !refused(err) || err.Error() != "line -3: lines are counted from 1" {
		t.Errorf("line -3 = %v", err)
	}
	loose := filepath.Join(t.TempDir(), "loose.go")
	if err := os.WriteFile(loose, []byte("package loose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Why(ctx, loose, 1); !refused(err) || !strings.HasPrefix(err.Error(), "git blame: ") {
		t.Errorf("outside a repository = %v, want git's word, as the caller's", err)
	}
}

// Git not on the machine is not anything the caller asked wrong: said as the server's own,
// which the HTTP API answers 500.
func TestWhyWithoutGitIsTheServersOwn(t *testing.T) {
	a, c := fixture.OpenCommit(t)
	t.Setenv("PATH", t.TempDir())
	_, err := a.WhyOverHTTP(context.Background(), c.File, 1)
	var r app.Refused
	if err == nil || errors.As(err, &r) || !strings.Contains(err.Error(), "git is not on the PATH") {
		t.Errorf("without git = %v, want the server's own word that git is missing", err)
	}
}
