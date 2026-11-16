package app

import (
	"os"
	"path/filepath"
	"runtime"
)

// StateDir is where asq keeps what it could not read back off the transcripts: what a
// collector has already been sent, the secret a server signs its cookies with. The index
// is a cache and lives elsewhere; this is the directory that should survive.
//
// ASQ_STATE names it; otherwise it is the platform's place for a program's state.
func StateDir() string {
	dir := os.Getenv("ASQ_STATE")
	if dir == "" {
		switch {
		case os.Getenv("XDG_STATE_HOME") != "":
			dir = filepath.Join(os.Getenv("XDG_STATE_HOME"), "asq")
		case runtime.GOOS == "linux":
			if home, err := os.UserHomeDir(); err == nil {
				dir = filepath.Join(home, ".local", "state", "asq")
			}
		}
	}
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil || base == "" {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "asq")
	}
	return dir
}
