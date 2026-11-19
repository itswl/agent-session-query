package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/index"
)

// uninstall takes what asq wrote — the index, the state — and only that; the CLIs' stores it
// never touches. The rest is said as commands: this very binary, and a service found by what
// its file names.
func TestUninstallTakesWhatAsqWrote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("LocalAppData", filepath.Join(home, "local"))
	t.Setenv("ASQ_INDEX", "")
	state := filepath.Join(home, "state")
	t.Setenv("ASQ_STATE", state)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "session.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(index.DefaultPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index.DefaultPath(), []byte("i"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A service file naming this very binary, where this machine's service manager reads it
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	switch runtime.GOOS {
	case "darwin":
		p := filepath.Join(home, "Library", "LaunchAgents", "x.plist")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("<string>"+self+"</string>"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "linux":
		u := filepath.Join(home, "config", "systemd", "user", "asq.service")
		if err := os.MkdirAll(filepath.Dir(u), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(u, []byte("ExecStart="+self+" serve"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var out, errs bytes.Buffer
	if code := Run([]string{"uninstall"}, strings.NewReader(""), &out, &errs); code != 0 {
		t.Fatalf("asq uninstall: exit %d\n%s", code, &errs)
	}
	if _, err := os.Stat(index.DefaultPath()); !os.IsNotExist(err) {
		t.Error("the index is still there")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Error("the state is still there")
	}
	said := out.String()
	if !strings.Contains(said, "to finish, run:") {
		t.Errorf("asq uninstall said:\n%s", said)
	}
	if runtime.GOOS != "windows" && !strings.Contains(said, "rm ") {
		t.Errorf("no command said to remove the binary:\n%s", said)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(said, "launchctl bootout") {
		t.Errorf("the LaunchAgent was not said:\n%s", said)
	}
	if runtime.GOOS == "linux" && !strings.Contains(said, "systemctl --user disable --now asq") {
		t.Errorf("the unit was not said:\n%s", said)
	}
}

// --json answers what was removed and what to run, for a script or an agent to finish.
func TestUninstallJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("LocalAppData", filepath.Join(home, "local"))
	t.Setenv("ASQ_INDEX", "")
	state := filepath.Join(home, "state")
	t.Setenv("ASQ_STATE", state)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(index.DefaultPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index.DefaultPath(), []byte("i"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errs bytes.Buffer
	if code := Run([]string{"uninstall", "--json"}, strings.NewReader(""), &out, &errs); code != 0 {
		t.Fatalf("asq uninstall --json: exit %d\n%s", code, &errs)
	}
	var got uninstalled
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, &out)
	}
	if len(got.Removed) != 2 || got.Removed[0] != index.DefaultPath() || got.Removed[1] != state {
		t.Errorf("removed %v", got.Removed)
	}
	if len(got.Run) == 0 {
		t.Error("nothing to run was said")
	}
}

// A state dir that is a home — an ASQ_STATE pointed too wide — is refused, not removed.
func TestUninstallRefusesAHomeForState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ASQ_STATE", home)
	var out, errs bytes.Buffer
	if code := Run([]string{"uninstall"}, strings.NewReader(""), &out, &errs); code != 0 {
		t.Fatalf("asq uninstall: exit %d\n%s", code, &errs)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("the home was taken: %v", err)
	}
	if strings.Contains(out.String(), home+" —") {
		t.Errorf("the home was said as removed:\n%s", &out)
	}
}
