package app

import (
	"testing"

	"github.com/itswl/agent-session-query/internal/source"
)

func TestFindSessionMatchesBareModeOfLabeledInstance(t *testing.T) {
	dir := t.TempDir()
	writeClaudeFixture(t, dir, "proj", "cccc", "/data")
	sources, err := source.BuildSources("auto", []source.SourcePath{{Mode: "claude", Label: "probe-a", Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	api := newSessionQueryAPI(sources, 0)
	if _, rec, ok := api.findSession("cccc", "claude"); !ok || rec.SessionID != "cccc" {
		t.Fatalf("bare-mode lookup = %q %v", rec.SessionID, ok)
	}
	if _, _, ok := api.findSession("cccc", "claude:probe-a"); !ok {
		t.Fatal("exact instance lookup failed")
	}
	if _, _, ok := api.findSession("cccc", "codex"); ok {
		t.Fatal("a labeled claude instance answered to codex")
	}
}
