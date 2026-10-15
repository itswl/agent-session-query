package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRunCheck: --check is the drift defence — every source named with found or not,
// the session count, and a deep read of the newest session; a source that fails to list
// fails the check, a source with no data is merely named as not found.
func TestRunCheck(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("LOCALAPPDATA", xdg)

	write(t, filepath.Join(home, ".pi", "agent", "sessions", "p", "2026-10-01T10-00-00-000Z_aaaa.jsonl"),
		`{"type":"session","id":"pi-check","cwd":"/w/pi"}`,
		`{"type":"message","id":"m1","message":{"role":"user","timestamp":1791312000000,"content":[{"type":"text","text":"set up the reverse proxy"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"proxy_pass goes like this"}]}}`,
	)

	var out strings.Builder
	code := runCheck("auto", nil, &out)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (nothing present failed):\n%s", code, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"pi", "ok", "1 sessions · newest reads 2 messages · final ok",
		"claude", "not found",
		"gemini", "not found",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("check output missing %q:\n%s", want, text)
		}
	}

	// A database that exists but does not parse fails the check — exactly the drift
	// this command exists to catch, where the page would just show an empty list
	write(t, filepath.Join(xdg, "opencode", "opencode.db"), ``)
	out.Reset()
	if code := runCheck("auto", nil, &out); code != 1 {
		t.Fatalf("exit = %d, want 1 (a present source failed):\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "LIST FAILED") {
		t.Fatalf("the failure is not named:\n%s", out.String())
	}
}

func TestRunCheckNamedMode(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	write(t, filepath.Join(home, ".pi", "agent", "sessions", "p", "x.jsonl"),
		`{"type":"session","id":"x"}`)

	var out strings.Builder
	if code := runCheck("pi", nil, &out); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "claude") {
		t.Fatalf("a named mode checks only itself:\n%s", out.String())
	}
	if code := runCheck("nope", nil, &out); code != 2 {
		t.Fatalf("an unknown mode exits 2, got %d", code)
	}
}
