package cli

import (
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
)

// runShell runs a command line in the user's shell, the terminal handed over to it: a
// resumed agent is interactive.
//
// For as long as it runs, asq goes on catching Ctrl+C and TERM, however many come. A
// Ctrl+C at the terminal reaches the command too, which decides what it means — an
// agent may take it to stop a turn — and it must not end asq beneath it: the shell would
// take the terminal back from a program still using it. A TERM sent to asq alone is
// passed on, so that stopping asq stops what it started.
func runShell(line string) error {
	cmd := exec.Command("sh", "-c", line)
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", line)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(caught)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-caught:
				if sig == syscall.SIGTERM {
					_ = cmd.Process.Signal(sig)
				}
			case <-done:
				return
			}
		}
	}()
	return cmd.Wait()
}
