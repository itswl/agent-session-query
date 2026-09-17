//go:build !windows

package app

// enableUTF8Console 只在 Windows 上有意义（见 console_windows.go）；
// 其它系统的终端本来就是 UTF-8。
func enableUTF8Console() {}
