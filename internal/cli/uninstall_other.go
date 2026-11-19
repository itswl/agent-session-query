//go:build !unix

package cli

// canWrite says whether this account may write in the directory. On Windows the binary
// sits in the user's own directory — where the installer put it — and needs no sudo.
func canWrite(dir string) bool {
	return true
}
