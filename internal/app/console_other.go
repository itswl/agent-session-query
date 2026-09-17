//go:build !windows

package app

// enableUTF8Console only matters on Windows (see console_windows.go);
// terminals everywhere else are already UTF-8.
func enableUTF8Console() {}
