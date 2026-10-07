package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers shared by this package's tests (the app package has copies of its own)
// ---------------------------------------------------------------------------

// setHome points the home directory at a temp directory.
// os.UserHomeDir() reads %USERPROFILE% on Windows rather than $HOME, so both have to be
// set for the test to be genuinely isolated there.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// jsonUnmarshalString decodes one JSON line into a map, for fixtures written inline
func jsonUnmarshalString(line string, into *map[string]any) error {
	return json.Unmarshal([]byte(line), into)
}

// packSecret is secret-shaped: a known key prefix followed by a long mixed-case,
// digit-bearing run — what redactSecrets is built to catch.
var packSecret = "sk-" + strings.Repeat("aB3dE", 8)

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
