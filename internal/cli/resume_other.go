//go:build !unix

package cli

// resume runs the line that reopens a session in the shell, which on Windows is the way a
// CLI installed as a .cmd script is found, and waits for it (see runShell).
func resume(_ string, _ []string, line string) error {
	return runShell(line)
}
