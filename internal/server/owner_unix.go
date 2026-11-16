//go:build unix

package server

import (
	"os"
	"syscall"
)

// ownedHere reports whether a file belongs to the user this process runs as; known is
// false where the system cannot say.
func ownedHere(info os.FileInfo) (owned, known bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	return int(st.Uid) == os.Getuid(), true
}
