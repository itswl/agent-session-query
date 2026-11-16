package cli

import (
	"os"
	"os/exec"
	"runtime"
)

// runShell runs a command line in the user's shell, the terminal handed over to it: a
// resumed agent is interactive.
func runShell(line string) error {
	cmd := exec.Command("sh", "-c", line)
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", line)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
