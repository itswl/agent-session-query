//go:build windows

package app

import (
	"fmt"
	"syscall"
)

// Windows 没有 setsid，对应的是这两个创建标志：
// DETACHED_PROCESS 不继承父进程的控制台，CREATE_NEW_PROCESS_GROUP 让它不跟着收 Ctrl+C。
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
