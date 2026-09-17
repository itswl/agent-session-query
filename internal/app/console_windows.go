//go:build windows

package app

import "syscall"

// enableUTF8Console switches the console output code page to UTF-8.
//
// The legacy Windows console (cmd.exe) defaults to the local code page, so any
// non-ASCII byte we print comes out as garbage. Windows Terminal and PowerShell 7
// are already UTF-8, where this call is a no-op.
//
// It fails when there is no console at all (output redirected to a file, or
// running as a service). Ignoring that is fine: there is no code page to set.
func enableUTF8Console() {
	const cpUTF8 = 65001
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleOutputCP")
	if proc.Find() != nil {
		return
	}
	_, _, _ = proc.Call(uintptr(cpUTF8))
}
