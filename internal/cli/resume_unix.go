//go:build unix

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// resume hands this process over to the CLI that reopens a session: asq becomes the
// agent, in the directory the session was worked in. Nothing is left between the agent
// and the terminal — no shell, no asq waiting beneath it — so a Ctrl+C or a TERM is the
// agent's alone, and when the agent ends, the command does.
func resume(dir string, argv []string, _ string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	if dir != "" {
		if err := os.Chdir(dir); err != nil {
			return err
		}
	}
	return syscall.Exec(path, argv, os.Environ())
}
