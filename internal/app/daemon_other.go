//go:build !windows

package app

import (
	"fmt"
	"syscall"
)

// detachAttr 让子进程自立门户（setsid）：脱离当前终端的会话与进程组，
// 这样关掉终端、或者终端收到 SIGHUP 时，它不会跟着一起死。
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func stopHint(pid int) string {
	return fmt.Sprintf("kill %d", pid)
}
