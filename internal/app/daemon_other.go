//go:build !windows

package app

import (
	"fmt"
	"syscall"
)

// detachAttr puts the child in its own session (setsid), detached from the
// current terminal's session and process group, so closing the terminal — or the
// terminal receiving SIGHUP — does not take the server down with it.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func stopHint(pid int) string {
	return fmt.Sprintf("kill %d", pid)
}
