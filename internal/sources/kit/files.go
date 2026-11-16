package kit

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Exists reports whether a path is there.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Stamp fingerprints files by size and modification time: the cheapest thing that moves
// whenever a file is appended to, rewritten or replaced. A missing file contributes a
// dash, so one appearing later moves the stamp too. The second result is the latest
// modification time among them.
func Stamp(paths ...string) (string, time.Time) {
	var b strings.Builder
	var latest time.Time
	for i, path := range paths {
		if i > 0 {
			b.WriteByte(';')
		}
		info, err := os.Stat(path)
		if err != nil {
			b.WriteByte('-')
			continue
		}
		b.WriteString(strconv.FormatInt(info.Size(), 36))
		b.WriteByte('.')
		b.WriteString(strconv.FormatInt(info.ModTime().UnixNano(), 36))
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return b.String(), latest
}

// StampInfo is Stamp for a file already stat'ed.
func StampInfo(info os.FileInfo) string {
	return strconv.FormatInt(info.Size(), 36) + "." + strconv.FormatInt(info.ModTime().UnixNano(), 36)
}

// Home is the user's home directory.
func Home() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return "/root"
}
