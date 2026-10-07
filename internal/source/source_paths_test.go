package source

import (
	"path/filepath"
	"strings"
	"testing"
)

// writeClaudeFixture lays down one minimal claude session: two lines, enough for List to
// pick up the id and the cwd and for the file to count as one session.
func writeClaudeFixture(t *testing.T, root, project, sessionID, cwd string) {
	t.Helper()
	write(t, filepath.Join(root, project, sessionID+".jsonl"),
		`{"type":"user","sessionId":"`+sessionID+`","cwd":"`+cwd+`","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}]}}`,
	)
}

func modeNames(sources []SessionSource) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.Mode())
	}
	return out
}

func TestPathFlagSet(t *testing.T) {
	var p PathFlag
	if err := p.Set("claude=/some/dir"); err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 || p[0].Mode != "claude" || p[0].Label != "" || p[0].Dir != "/some/dir" {
		t.Fatalf("unlabeled = %v", p)
	}
	if err := p.Set("claude:box-2=/other/dir"); err != nil {
		t.Fatal(err)
	}
	if p[1].Label != "box-2" || p[1].Dir != "/other/dir" {
		t.Fatalf("labeled = %v", p[1])
	}
	for _, bad := range []string{
		"claude",          // no =dir
		"claude=",         // empty dir
		"claude:BOX=/d",   // label charset
		"claude:=/d",      // empty label
		"nope=/d",         // unknown source
		"auto=/d",         // not a source
		"hermes=/d",       // not a directory-shaped source
		"openclaw:lab=/d", // …labeled or not
	} {
		if err := p.Set(bad); err == nil {
			t.Errorf("--path %q should have been rejected", bad)
		}
	}
}

func TestBuildSourcesLabeledInstances(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	dirA, dirB := t.TempDir(), t.TempDir()
	writeClaudeFixture(t, dirA, "proj-a", "aaaa", "/data")
	writeClaudeFixture(t, dirB, "proj-b", "bbbb", "/data")

	sources, err := BuildSources("auto", []SourcePath{
		{Mode: "claude", Label: "probe-a", Dir: dirA},
		{Mode: "claude", Label: "probe-b", Dir: dirB},
	})
	if err != nil {
		t.Fatal(err)
	}
	var a SessionSource
	found := map[string]bool{}
	for _, s := range sources {
		found[s.Mode()] = true
		if s.Mode() == "claude:probe-a" {
			a = s
		}
	}
	if a == nil || !found["claude:probe-b"] {
		t.Fatalf("labeled instances missing from %v", found)
	}
	recs := a.List()
	if len(recs) != 1 {
		t.Fatalf("claude:probe-a listed %d records", len(recs))
	}
	if recs[0].Source != "claude:probe-a" {
		t.Fatalf("source = %q", recs[0].Source)
	}
	if recs[0].Cwd != "probe-a:/data" {
		t.Fatalf("cwd = %q", recs[0].Cwd)
	}
	if recs[0].ProjectName() != "probe-a:/data" {
		t.Fatalf("project = %q", recs[0].ProjectName())
	}
	// The final result names the labeled source too
	if fin := a.Final(recs[0]); fin == nil || fin["source"] != "claude:probe-a" {
		t.Fatalf("final source = %v", fin["source"])
	}
	// The wrapped source caches its records, so a second List hands the same rows back:
	// prefixing must be a copy, or the label accumulates
	if again := a.List(); again[0].Cwd != "probe-a:/data" {
		t.Fatalf("a second List double-prefixed the cwd: %q", again[0].Cwd)
	}
}

func TestBuildSourcesRelocation(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	dir := t.TempDir()
	writeClaudeFixture(t, dir, "proj", "cccc", "/work")
	sources, err := BuildSources("all", []SourcePath{{Mode: "claude", Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	var claude SessionSource
	n := 0
	for _, s := range sources {
		if strings.HasPrefix(s.Mode(), "claude") {
			claude = s
			n++
		}
	}
	if claude == nil || n != 1 {
		t.Fatalf("expected exactly one claude instance, got %d in %v", n, modeNames(sources))
	}
	if claude.Location() != dir {
		t.Fatalf("location = %q, want %q", claude.Location(), dir)
	}
	if claude.Mode() != "claude" {
		t.Fatalf("a relocation keeps the plain mode, got %q", claude.Mode())
	}
}

func TestBuildSourcesDuplicateLabelErrors(t *testing.T) {
	setHome(t, t.TempDir())
	_, err := BuildSources("auto", []SourcePath{
		{Mode: "claude", Label: "dup", Dir: "/a"},
		{Mode: "claude", Label: "dup", Dir: "/b"},
	})
	if err == nil || !strings.Contains(err.Error(), "claude:dup") {
		t.Fatalf("duplicate label = %v", err)
	}
}

// A labeled instance is an explicit request: it is enabled whatever auto detects, even
// when the directory does not exist (which the startup log warns about).
func TestBuildSourcesLabeledMissingDirEnabledAnyway(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	missing := filepath.Join(home, "not-there", ".claude", "projects")
	sources, err := BuildSources("auto", []SourcePath{{Mode: "claude", Label: "ghost", Dir: missing}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range modeNames(sources) {
		if name == "claude:ghost" {
			return
		}
	}
	t.Fatalf("claude:ghost missing from %v", modeNames(sources))
}
