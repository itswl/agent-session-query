//go:build unix

package cli

import "syscall"

// canWrite says whether this account may write in the directory — for a binary in a
// system-wide place, removing it is sudo's to do, and the command said so. 2 is W_OK,
// the same on every unix, and named as a constant by the syscall package only on some.
func canWrite(dir string) bool {
	return syscall.Access(dir, 2) == nil
}
