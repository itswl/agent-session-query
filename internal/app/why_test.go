package app_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	if len(why.Made) != 1 || !strings.HasPrefix(why.Made[0].Ref, "claude/"+madeIt+"#1") {
		t.Errorf("made = %+v", why.Made)
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
