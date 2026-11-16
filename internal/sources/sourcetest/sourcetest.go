// Package sourcetest is what adapter tests share: writing a fixture store into a
// temporary directory, and reading a source back as whole sessions.
package sourcetest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Write writes a file of lines, creating its directory. A line may be a string, taken
// as it is, or any other value, written as JSON.
func Write(t testing.TB, path string, lines ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, line := range lines {
		switch v := line.(type) {
		case string:
			b.WriteString(v)
		default:
			// As the CLIs write it: < and > are themselves, not \u003c
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(v); err != nil {
				t.Fatal(err)
			}
			continue // Encode ends the line itself
		}
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Session is one session read whole.
type Session struct {
	Ref sources.Ref
	transcript.Collected
}

// Turn returns the turn with that index, failing the test when there is none.
func (s Session) Turn(t testing.TB, index int) model.Turn {
	t.Helper()
	for _, turn := range s.Turns {
		if turn.Index == index {
			return turn
		}
	}
	t.Fatalf("session %s has no turn %d (it has %d turns)", s.Session.ID, index, len(s.Turns))
	return model.Turn{}
}

// Read reads one session of a source.
func Read(t testing.TB, src sources.Source, ref sources.Ref) Session {
	t.Helper()
	c := &transcript.Collector{}
	w := transcript.New(c)
	if err := src.Read(context.Background(), ref, w); err != nil {
		t.Fatalf("read %s: %v", ref.Key, err)
	}
	stats, err := w.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return Session{Ref: ref, Collected: transcript.Collected{Session: w.S, Stats: stats, Turns: c.Turns}}
}

// All scans a source and reads every session it lists, ordered by session id.
func All(t testing.TB, src sources.Source) []Session {
	t.Helper()
	refs, err := src.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	out := make([]Session, 0, len(refs))
	for _, ref := range refs {
		out = append(out, Read(t, src, ref))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Session.ID < out[j].Session.ID })
	return out
}

// One scans a source that should hold exactly one session and reads it.
func One(t testing.TB, src sources.Source) Session {
	t.Helper()
	all := All(t, src)
	if len(all) != 1 {
		ids := make([]string, 0, len(all))
		for _, s := range all {
			ids = append(ids, s.Session.ID)
		}
		t.Fatalf("the source lists %d sessions, want 1: %v", len(all), ids)
	}
	return all[0]
}

// ByID finds a session by id among those read.
func ByID(t testing.TB, all []Session, id string) Session {
	t.Helper()
	for _, s := range all {
		if s.Session.ID == id {
			return s
		}
	}
	t.Fatalf("no session %q among %d", id, len(all))
	return Session{}
}

// Tools returns a turn's tool items.
func Tools(turn model.Turn) []*model.Tool {
	var out []*model.Tool
	for i := range turn.Items {
		if turn.Items[i].Tool != nil {
			out = append(out, turn.Items[i].Tool)
		}
	}
	return out
}

// Kinds renders a turn's items as their kinds, for a compact assertion on its shape:
// "prompt thinking tool reply".
func Kinds(turn model.Turn) string {
	parts := make([]string, 0, len(turn.Items))
	for _, it := range turn.Items {
		parts = append(parts, string(it.Kind))
	}
	return strings.Join(parts, " ")
}

// Env is an adapter environment rooted at a temporary home, with the given variables.
func Env(home string, vars ...string) sources.Env {
	m := map[string]string{}
	for i := 0; i+1 < len(vars); i += 2 {
		m[vars[i]] = vars[i+1]
	}
	return sources.Env{Home: home, Getenv: func(k string) string { return m[k] }}
}

// Unreadable makes a directory unlistable and returns what makes it listable again, which
// the end of the test does too. It skips the test where that cannot be arranged: as root,
// who reads everything, and on Windows, where a mode says nothing about who may list a
// directory.
func Unreadable(t testing.TB, dir string) (restore func()) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("a directory cannot be made unreadable here")
	}
	restore = func() { _ = os.Chmod(dir, 0o755) }
	if err := os.Chmod(dir, 0); err != nil {
		t.Skip(err)
	}
	t.Cleanup(restore)
	if _, err := os.ReadDir(dir); err == nil {
		restore()
		t.Skip("the directory is still readable")
	}
	return restore
}
