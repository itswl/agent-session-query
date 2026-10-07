package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

// TestSourceRegistryConsistency: a source's identity is spread across several
// hand-maintained lists — that is the price a ninth source pays. This test pins the ones
// a machine can check: every known mode is wired to a factory whose Exists() answers, in
// registry order; resumeCommands names only real modes; and the page's two registries
// follow the same lists. A mode added to knownModes alone now fails here instead of
// drifting silently (the filecache counter wiring is per-constructor and stays
// hand-checked; docs/development.md lists every place).
func TestSourceRegistryConsistency(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg"))

	// One plausible default location per source
	write(t, filepath.Join(home, ".pi", "agent", "sessions", "p", "x.jsonl"), `{"type":"session","id":"x"}`)
	write(t, filepath.Join(home, ".claude", "projects", "p", "x.jsonl"), `{"type":"user","uuid":"u","message":{"role":"user","content":"hi"}}`)
	write(t, filepath.Join(home, ".codex", "sessions", "2026", "01", "01", "rollout-x.jsonl"), `{}`)
	write(t, filepath.Join(home, ".gemini", "tmp", "p", "chats", "session-x.jsonl"), `{"sessionId":"g"}`)
	write(t, filepath.Join(home, ".grok", "sessions", "p", "s", "summary.json"), `{"info":{"id":"g"}}`)
	write(t, filepath.Join(home, ".hermes", "sessions", "sessions.json"), `{}`)
	write(t, filepath.Join(home, ".openclaw", "agents", "default", "agent", "openclaw-agent.sqlite"), ``)
	write(t, filepath.Join(home, "xdg", "opencode", "opencode.db"), ``)

	sources, err := buildSources("auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, source := range sources {
		got = append(got, source.Mode())
	}
	// A hard-coded universe rather than knownModes itself: comparing against the variable
	// under test would miss a mode removed from every list at once
	wantModes := []string{"claude", "codex", "gemini", "grok", "hermes", "openclaw", "opencode", "pi"}
	gotSorted := append([]string{}, got...)
	sort.Strings(gotSorted)
	if !reflect.DeepEqual(gotSorted, wantModes) {
		t.Fatalf("auto enabled %v, want the eight known modes %v — a mode was added or removed without this list (and its wiring) following", got, wantModes)
	}
	if !reflect.DeepEqual(got, knownModes) {
		t.Fatalf("auto enabled %v, want registry order %v", got, knownModes)
	}

	for mode := range resumeCommands {
		if !containsString(knownModes, mode) {
			t.Errorf("resumeCommands names %q, which is not in knownModes", mode)
		}
	}

	// The pre-SQLite OpenClaw fallback only builds when no agent database exists, so the
	// fixture above never reaches it; drive it directly, or a factory returning the wrong
	// definition there passes unnoticed (it did, before this phase existed)
	if err := os.Remove(filepath.Join(home, ".openclaw", "agents", "default", "agent", "openclaw-agent.sqlite")); err != nil {
		t.Fatal(err)
	}
	fallback, err := buildSources("openclaw", nil)
	if err != nil || len(fallback) != 1 || fallback[0].Mode() != "openclaw" {
		t.Fatalf("the OpenClaw json-map fallback must report Mode()=openclaw, got %v (err %v)", fallback, err)
	}

	sub, err := uiSub()
	if err != nil {
		t.Fatal(err)
	}
	appJS, err := fs.ReadFile(sub, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	jsList := func(pattern string) []string {
		t.Helper()
		m := regexp.MustCompile(pattern).FindSubmatch(appJS)
		if m == nil {
			t.Fatalf("app.js: pattern %q not found — the registry moved or was renamed", pattern)
		}
		names := []string{}
		for _, q := range regexp.MustCompile(`'([a-z0-9:-]+)'`).FindAllStringSubmatch(string(m[1]), -1) {
			names = append(names, q[1])
		}
		return names
	}

	sorted := append([]string{}, knownModes...)
	sort.Strings(sorted)
	sourceClasses := jsList(`const SOURCE_CLASSES = \[([^\]]*)\]`)
	sort.Strings(sourceClasses)
	if !reflect.DeepEqual(sourceClasses, sorted) {
		t.Errorf("app.js SOURCE_CLASSES = %v, want %v", sourceClasses, sorted)
	}

	countable := jsList(`const countable = \[([^\]]*)\]`)
	sort.Strings(countable)
	wantCountable := []string{"claude", "codex", "gemini", "grok", "pi"}
	if !reflect.DeepEqual(countable, wantCountable) {
		t.Errorf("app.js countable = %v, want the five file-backed modes %v", countable, wantCountable)
	}
}
