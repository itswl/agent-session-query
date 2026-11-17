package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/index"
)

// A look at a file that does not come back — on a Mac, macOS asking on its screen whether
// asq may read the folder, with nobody there to answer — does not hold the answer: the file
// is listed, not judged, and named as waiting; one look waits, however often it is asked;
// and once the look comes back, the files are judged again.
func TestALookThatWaitsDoesNotHoldTheAnswer(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(proj, "main.go")
	if err := os.WriteFile(file, []byte("package main\nfunc brandNew() int { return 42 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	const id = "ffff6666-0000-4000-8000-000000000006"
	rows := []map[string]any{
		{"type": "user", "uuid": "u1", "timestamp": at, "sessionId": id, "cwd": proj, "message": map[string]any{"role": "user", "content": "change it"}},
		{"type": "assistant", "uuid": "a1", "timestamp": at, "sessionId": id, "message": map[string]any{"id": "m1", "role": "assistant", "model": "claude-opus-5", "stop_reason": "tool_use",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1}, "content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "Edit",
				"input": map[string]any{"file_path": file, "old_string": "func old() {}", "new_string": "func brandNew() int { return 42 }"}}}}},
		{"type": "user", "uuid": "u2", "timestamp": at, "sessionId": id, "cwd": proj, "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}}}},
	}
	store := filepath.Join(dir, "claude")
	path := filepath.Join(store, "-proj", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range rows {
		raw, _ := json.Marshal(r)
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Open(Config{IndexPath: filepath.Join(dir, "index.db"), Stores: catalog.Config{Only: []string{"claude"}, Paths: []catalog.Path{{Agent: "claude", Dir: store}}}})
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
	files := func() []index.FileTouch {
		t.Helper()
		list, _, err := a.Files(ctx, f, index.Window{}, "", true, index.Page{Limit: 50})
		if err != nil || len(list) != 1 {
			t.Fatalf("files: %+v, %v", list, err)
		}
		return list
	}
	if got := files()[0]; got.Now == nil || got.Waiting != "" {
		t.Fatalf("before: now %+v, waiting %q", got.Now, got.Waiting)
	}

	// Every look in the project waits until the question is answered
	answered := make(chan struct{})
	var looks atomic.Int32
	defer SetLookWait(40 * time.Millisecond)()
	defer SetStat(func(p string) (os.FileInfo, error) {
		if strings.HasPrefix(p, proj) {
			looks.Add(1)
			<-answered
		}
		return os.Stat(p)
	})()
	once := false
	answer := func() {
		if !once {
			once = true
			close(answered)
		}
	}
	defer answer() // the looks left waiting end before the stand-in is put back

	for i := range 3 {
		start := time.Now()
		got := files()[0]
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("ask %d waited %v for a look that does not come back", i+1, took)
		}
		if got.Now != nil || got.Waiting != guarded(proj) {
			t.Errorf("ask %d: now %+v, waiting %q; want none, and %q", i+1, got.Now, got.Waiting, guarded(proj))
		}
	}
	if n := looks.Load(); n != 1 {
		t.Errorf("%d looks were left waiting in one folder, want 1", n)
	}

	// Answered: the files are judged again
	answer()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := files()[0]
		if got.Now != nil && got.Waiting == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("answered, and still: now %+v, waiting %q", got.Now, got.Waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The folder a look waits in is the one a privacy question names: under home its first
// folder, Library's second; elsewhere the first two.
func TestTheFolderALookWaitsIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sep := string(filepath.Separator)
	for path, want := range map[string]string{
		filepath.Join(home, "Documents", "proj", "a.go"):                                  filepath.Join(home, "Documents"),
		filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs", "x.md"): filepath.Join(home, "Library", "Mobile Documents"),
		filepath.Join(home, "orca", "agent-session-query", "main.go"):                     filepath.Join(home, "orca"),
		filepath.Join(home, "notes.md"):                                                   filepath.Join(home, "notes.md"),
		sep + filepath.Join("Volumes", "Disk", "work", "a.go"):                            sep + filepath.Join("Volumes", "Disk"),
		sep + filepath.Join("srv", "app", "x", "y.go"):                                    sep + filepath.Join("srv", "app"),
	} {
		if got := guarded(path); got != want {
			t.Errorf("guarded(%q) = %q, want %q", path, got, want)
		}
	}
}
