package keep

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

func put(t *testing.T, path, text string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// A copy is each file at its path beneath the folder, with its time, made once and again
// only when the file moved. Nothing is written for a file outside the store, and a file
// still being written waits unless forced.
func TestCopy(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "store"), filepath.Join(t.TempDir(), "kept")
	now := time.Now()
	old := now.Add(-time.Hour)
	session := filepath.Join(from, "p", "s.jsonl")
	put(t, session, "one\n", old)
	outside := filepath.Join(filepath.Dir(from), "secret")
	put(t, outside, "not the store's", old)
	files := []string{session, filepath.Join(from, "p", "missing.json"), outside, from}

	whole, n, err := Copy(files, from, to, now, false)
	if !whole || n != 4 || err != nil {
		t.Fatalf("copy = %v, %d, %v", whole, n, err)
	}
	got, err := os.Stat(filepath.Join(to, "p", "s.jsonl"))
	src, _ := os.Stat(session) // its time as the filesystem keeps it
	if err != nil || !got.ModTime().Equal(src.ModTime()) || runtime.GOOS != "windows" && got.Mode().Perm() != 0o600 {
		t.Errorf("copy = %v, %v", got, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(to)); len(entries) != 1 {
		t.Errorf("written beside the folder: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(to, "p", "missing.json")); !os.IsNotExist(err) {
		t.Errorf("a file the store does not hold: %v", err)
	}

	// Unchanged: nothing written
	if whole, n, _ := Copy(files, from, to, now, false); !whole || n != 0 {
		t.Errorf("again = %v, %d", whole, n)
	}
	// Written a moment ago: it waits, and is copied when forced
	put(t, session, "one\ntwo\n", now)
	if whole, n, _ := Copy(files, from, to, now, false); whole || n != 0 {
		t.Errorf("being written = %v, %d; want it to wait", whole, n)
	}
	if whole, n, _ := Copy(files, from, to, now, true); !whole || n != 8 {
		t.Errorf("forced = %v, %d", whole, n)
	}
	// The CLI deletes it: the copy stays
	os.Remove(session)
	if whole, _, _ := Copy(files, from, to, now, false); !whole {
		t.Error("a deleted file left the copy unwhole")
	}
	if b, err := os.ReadFile(filepath.Join(to, "p", "s.jsonl")); err != nil || string(b) != "one\ntwo\n" {
		t.Errorf("the copy after the CLI deleted the file = %q, %v", b, err)
	}
}

// A folder on another disk may keep a time to two seconds: a copy that close is the same.
func TestSameToWhatAFolderKeepsOfATime(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	put(t, filepath.Join(dir, "a"), "x", at)
	put(t, filepath.Join(dir, "b"), "x", at.Add(1500*time.Millisecond))
	put(t, filepath.Join(dir, "c"), "x", at.Add(3*time.Second))
	put(t, filepath.Join(dir, "d"), "xy", at)
	a, _ := os.Stat(filepath.Join(dir, "a"))
	for name, want := range map[string]bool{"b": true, "c": false, "d": false} {
		other, _ := os.Stat(filepath.Join(dir, name))
		if same(other, a) != want {
			t.Errorf("%s same as a: %v, want %v", name, !want, want)
		}
	}
}

// files is a store of one file per session, named by its path.
type files struct{ root string }

func (f files) Location() string { return f.root }
func (f files) Exists() bool     { _, err := os.Stat(f.root); return err == nil }
func (f files) Scan(ctx context.Context) ([]sources.Ref, error) {
	paths, _ := filepath.Glob(filepath.Join(f.root, "*", "*.jsonl"))
	var refs []sources.Ref
	for _, p := range paths {
		refs = append(refs, sources.Ref{Key: p, Stamp: "1"})
	}
	return refs, nil
}
func (f files) Read(context.Context, sources.Ref, *transcript.Writer) error { return nil }

// The kept store lists the copies of what the CLI's store no longer holds, and says how
// many it keeps in all.
func TestTheKeptStoreListsWhatTheCLIDeleted(t *testing.T) {
	live, kept := t.TempDir(), t.TempDir()
	old := time.Now().Add(-time.Hour)
	for _, name := range []string{"a", "b"} {
		put(t, filepath.Join(live, "p", name+".jsonl"), name, old)
		put(t, filepath.Join(kept, "p", name+".jsonl"), name, old)
	}
	s := Store(files{kept}, files{live})
	if refs, _ := s.Scan(t.Context()); len(refs) != 0 {
		t.Errorf("with the CLI holding both = %v", refs)
	}
	os.Remove(filepath.Join(live, "p", "b.jsonl"))
	refs, _ := s.Scan(t.Context())
	if len(refs) != 1 || refs[0].Key != filepath.Join(kept, "p", "b.jsonl") {
		t.Errorf("after the CLI deleted b = %v", refs)
	}
	if n, gone, err := Held(t.Context(), s); n != 2 || gone != 1 || err != nil {
		t.Errorf("held = %d, %d, %v", n, gone, err)
	}
	if !IsKept(s) || IsKept(files{live}) {
		t.Error("IsKept")
	}
}

func TestSettings(t *testing.T) {
	state := t.TempDir()
	if s, err := Load(state); err != nil || s.On() {
		t.Errorf("nothing saved = %+v, %v", s, err)
	}
	s := Settings{}.Star("claude", "a1", true).Star("gemini", "g1", true).Star("claude", "a2", true)
	s = s.Star("claude", "a1", false)
	if err := Save(state, s); err != nil {
		t.Fatal(err)
	}
	got, err := Load(state)
	if err != nil || !got.On() || got.All || !slices.Equal(got.IDs("claude"), []string{"a2"}) || !got.Starred("gemini", "g1") || got.Starred("claude", "a1") {
		t.Errorf("loaded = %+v, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(state, settingsFile)); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("settings file = %v, %v", info, err)
	}
	if got.Folder(state) != filepath.Join(state, "kept") || (Settings{Dir: "/x"}).Folder(state) != "/x" {
		t.Error("folder")
	}
}
