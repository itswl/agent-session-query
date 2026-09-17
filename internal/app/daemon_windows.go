//go:build windows

package app

import (
	"fmt"
	"syscall"
)

// Windows has no setsid; these two creation flags are the equivalent.
// DETACHED_PROCESS keeps the child off the parent's console, and
// CREATE_NEW_PROCESS_GROUP keeps it from receiving the parent's Ctrl+C.
const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup,
		HideWindow:    true,
	}
}

func stopHint(pid int) string {
	return fmt.Sprintf("taskkill /PID %d /F", pid)
}
