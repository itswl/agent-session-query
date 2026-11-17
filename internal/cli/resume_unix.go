//go:build unix

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// resume hands this process over to the CLI that reopens a session: asq becomes the
// agent, in the directory the session was worked in. Nothing is left between the agent
// and the terminal — no shell, no asq waiting beneath it — so a Ctrl+C or a TERM is the
// agent's alone, and when the agent ends, the command does.
//
// It is what `cd dir && agent …` in a shell would do, in a shell's order: the directory
// is changed first, PWD and OLDPWD say so, and the agent is looked for on PATH from
// there — a relative entry such as node_modules/.bin included.
func resume(dir string, argv []string, _ string) error {
	env := os.Environ()
	if dir != "" {
		was, _ := os.Getwd()
		if err := os.Chdir(dir); err != nil {
			return err
		}
		env = slices.DeleteFunc(env, func(kv string) bool {
			return strings.HasPrefix(kv, "PWD=") || strings.HasPrefix(kv, "OLDPWD=")
		})
		env = append(env, "PWD="+dir, "OLDPWD="+was)
	}
	path, err := exec.LookPath(argv[0])
	if errors.Is(err, exec.ErrDot) {
		err = nil // found through a relative entry of PATH, as a shell would find it
	}
	if err != nil {
		return err
	}
	if path, err = filepath.Abs(path); err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}
