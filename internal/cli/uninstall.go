package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/index"
)

// cmdUninstall takes asq off the machine: what it wrote and only what it wrote — the index,
// a cache, and its state. What it should not do itself it says as commands: removing this
// very binary, wherever it was put, and taking down a service that keeps serve running.
// The CLIs' stores are not asq's to touch, and it never does.
func cmdUninstall(ctx context.Context, args []string, io *streams) error {
	c := newCommand("uninstall", io.err)
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef("asq uninstall takes nothing else")
	}
	cfg, err := c.config()
	if err != nil {
		return usageError{err}
	}

	// The index, and what an earlier asq left beside it — the same taking `asq index rm` does.
	path := cfg.IndexPath
	if path == "" {
		path = index.DefaultPath()
	}
	var removed, notes []string
	take := func(p, note string) error {
		if _, err := os.Stat(p); err != nil {
			return nil // not here: nothing to take, nothing to say
		}
		if err := index.Remove(p); err != nil {
			return err
		}
		removed, notes = append(removed, p), append(notes, note)
		return nil
	}
	if err := take(path, "the index, rebuilt from the stores on the next run"); err != nil {
		return err
	}
	if cfg.IndexPath == "" {
		for _, old := range index.Older() {
			if err := take(old, "the index an earlier asq left"); err != nil {
				return err
			}
		}
	}

	// The state: the sign-in key, what a collector has been sent. A state dir that is a
	// home — an ASQ_STATE gone wrong — is not ours to take.
	if state := app.StateDir(); worthTaking(state) {
		if _, err := os.Stat(state); err == nil {
			if err := os.RemoveAll(state); err != nil {
				return err
			}
			removed, notes = append(removed, state), append(notes, "the state: the sign-in key, what a collector was sent")
		}
	}

	run := finishCommands()
	if c.json {
		return printJSON(io.out, uninstalled{Removed: removed, Run: run})
	}
	for i, p := range removed {
		fmt.Fprintf(io.out, "removed %s — %s\n", p, notes[i])
	}
	if len(removed) == 0 {
		fmt.Fprintln(io.out, "nothing of asq's was here to remove")
	}
	fmt.Fprintln(io.out, "\nto finish, run:")
	for _, cmd := range run {
		fmt.Fprintln(io.out, "  "+cmd)
	}
	fmt.Fprintln(io.out, "\na line that put asq on the PATH, added when installing, can go too")
	return nil
}

type uninstalled struct {
	Removed []string `json:"removed"`
	Run     []string `json:"run"`
}

// worthTaking says whether a state directory is asq's to remove: not the home an
// ASQ_STATE gone wrong pointed at, not the root an empty name stood for.
func worthTaking(dir string) bool {
	if dir == "" || dir == string(filepath.Separator) {
		return false
	}
	if home, err := os.UserHomeDir(); err == nil && dir == home {
		return false
	}
	return true
}

// finishCommands says what uninstall did not do itself. Removing the binary is this
// command's own process, and taking a service down touches more of the machine than a
// directory of asq's: both are said, with this binary's real path, for a person to run.
func finishCommands() []string {
	self, err := os.Executable()
	if err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
	} else {
		self = ""
	}
	var cmds []string
	switch runtime.GOOS {
	case "windows":
		if self != "" {
			// The installer put the exe in a directory of its own; take the directory,
			// and only the exe when it lives somewhere shared
			target := self
			if filepath.Base(filepath.Dir(self)) == "asq" {
				target = filepath.Dir(self)
			}
			cmds = append(cmds, "Remove-Item -Recurse -Force "+app.ShellArg(target))
		}
		if taskExists("asq") {
			cmds = append(cmds, "schtasks /delete /tn asq")
		}
	default:
		if self != "" {
			rm := "rm " + app.ShellArg(self)
			if !canWrite(filepath.Dir(self)) {
				rm = "sudo " + rm
			}
			cmds = append(cmds, rm)
		}
		cmds = append(cmds, serviceCommands(self)...)
	}
	return cmds
}

// serviceCommands looks for what keeps `asq serve` running — a LaunchAgent under launchd, a
// unit under systemd — by what such a file cannot help naming: this binary's path, or the
// words of a setup made by hand. Found, the commands that take it down are said, not run:
// a service may have been set up to this machine's own taste, and is not asq's to stop.
func serviceCommands(self string) []string {
	saysAsq := func(b []byte) bool {
		if self != "" && bytes.Contains(b, []byte(self)) {
			return true
		}
		// What a plist writes with XML's escapes, and what a unit writes with systemd's
		// %h for the home this binary is in
		if self != "" && (bytes.Contains(b, []byte(xmlEscaped(self))) || bytes.Contains(b, []byte(systemdHome(self)))) {
			return true
		}
		return bytes.Contains(b, []byte("asq")) && bytes.Contains(b, []byte("serve"))
	}
	var cmds []string
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		plists, _ := filepath.Glob(filepath.Join(home, "Library", "LaunchAgents", "*.plist"))
		for _, p := range plists {
			if b, err := os.ReadFile(p); err != nil || !saysAsq(b) {
				continue
			}
			cmds = append(cmds,
				`launchctl bootout "gui/$(id -u)" `+app.ShellArg(p),
				"rm "+app.ShellArg(p))
		}
	case "linux":
		base, err := os.UserConfigDir()
		if err != nil {
			return nil
		}
		units, _ := filepath.Glob(filepath.Join(base, "systemd", "user", "*.service"))
		for _, u := range units {
			if b, err := os.ReadFile(u); err != nil || !saysAsq(b) {
				continue
			}
			name := strings.TrimSuffix(filepath.Base(u), ".service")
			cmds = append(cmds,
				"systemctl --user disable --now "+app.ShellArg(name),
				"rm "+app.ShellArg(u))
		}
	}
	return cmds
}

// taskExists says whether Windows' scheduler holds a task of this name.
func taskExists(name string) bool {
	c := exec.Command("schtasks", "/query", "/tn", name)
	return c.Run() == nil
}

func xmlEscaped(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}

func systemdHome(path string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, home+"/") {
		return "%h/" + path[len(home)+1:]
	}
	return ""
}
