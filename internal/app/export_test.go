package app

import (
	"os"
	"time"
)

// SetStat stands in for how a file is looked at, and SetLookWait for how long a look is
// waited for, in a test; each returns how to put it back.
func SetStat(f func(string) (os.FileInfo, error)) func() {
	old := osStat
	osStat = f
	return func() { osStat = old }
}

func SetLookWait(d time.Duration) func() {
	old := lookWait
	lookWait = d
	return func() { lookWait = old }
}
