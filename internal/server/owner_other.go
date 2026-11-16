//go:build !unix

package server

import "os"

// ownedHere reports whether a file belongs to the user this process runs as; known is
// false where the system cannot say, as here: on Windows a file's mode bits do not say
// who may read it either, and the directory is the user's own profile.
func ownedHere(os.FileInfo) (owned, known bool) { return false, false }
