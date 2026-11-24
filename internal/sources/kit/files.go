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
// whenever a file is appended to, and whenever it is rewritten or replaced in a later tick
// of the filesystem's clock than the write before. A rewrite at the same size within the
// same tick leaves it as it was (see Still). A missing file contributes a dash, so one
// appearing later moves the stamp too. The second result is the latest modification time
// among them.
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

// still is how long files must have been left alone before a fingerprint of them stands
// for what is read after it.
//
// A write is stamped with the filesystem clock's latest tick, not with the moment: two
// writes within one tick get one modification time, and a file rewritten in place — a
// database's pages — keeps its size, so the second write leaves the fingerprint exactly
// where the first did. Once the newest of the files is older than the coarsest tick there
// is, any later write lands in a later tick, and moves it. A tick can be as short as a
// nanosecond (APFS); it is a few milliseconds on Linux whatever the filesystem keeps (the
// kernel stamps from a clock that moves once a jiffy: this is how tmpfs ties), a second on
// ext3 and HFS+, and two on FAT and exFAT. The third second is for a clock that runs a little ahead of a file
// server's, which makes a write look older than it is. A clock behind the server's makes
// one look as if it had not happened yet, and such a file is not still either.
const still = 3 * time.Second

// Now is the clock a fingerprint's age is judged by; a test swaps it to move a write
// into the past.
var Now = time.Now

// Still reports whether files whose newest modification time is mod have been left alone
// long enough for a fingerprint of them, taken just now, to stand for what is read next
// (see still). A value read under a fingerprint that is not still may be returned, but not
// served again for that fingerprint: the next call reads again.
func Still(mod time.Time) bool { return Now().Sub(mod) >= still }

// Home is the user's home directory.
func Home() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return "/root"
}
