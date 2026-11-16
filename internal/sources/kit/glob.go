package kit

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Glob is filepath.Glob that says what it could not look into.
//
// The standard one skips a directory it cannot read as if it were empty. For a listing of
// sessions that silence is expensive: the sessions under a directory that could not be
// read are missing from the list, and what is missing from the list is taken for deleted.
// This returns the matches it found together with an error naming every directory it
// could not read, so the caller can say the list is short. A directory that is not there
// is not an error — it matches nothing.
func Glob(pattern string) ([]string, error) {
	pattern = filepath.Clean(pattern)
	segs := strings.Split(filepath.ToSlash(pattern), "/")
	// The literal start of the pattern is where the looking begins
	root := ""
	if filepath.IsAbs(pattern) {
		root = filepath.VolumeName(pattern) + string(filepath.Separator)
		segs = strings.Split(strings.TrimPrefix(filepath.ToSlash(pattern[len(filepath.VolumeName(pattern)):]), "/"), "/")
	}
	var out []string
	var unread error
	var walk func(dir string, segs []string)
	walk = func(dir string, segs []string) {
		seg, last := segs[0], len(segs) == 1
		if !strings.ContainsAny(seg, `*?[\`) {
			next := filepath.Join(dir, seg)
			if !last {
				walk(next, segs[1:])
			} else if _, err := os.Lstat(next); err == nil {
				out = append(out, next)
			}
			return
		}
		at := dir
		if at == "" {
			at = "."
		}
		entries, err := os.ReadDir(at)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				unread = errors.Join(unread, err)
			}
			return
		}
		for _, e := range entries {
			if ok, _ := filepath.Match(seg, e.Name()); !ok {
				continue
			}
			next := filepath.Join(dir, e.Name())
			switch {
			case last:
				out = append(out, next)
			case e.IsDir():
				walk(next, segs[1:])
			case e.Type()&fs.ModeSymlink != 0:
				if info, err := os.Stat(next); err == nil && info.IsDir() {
					walk(next, segs[1:])
				}
			}
		}
	}
	if len(segs) > 0 && segs[0] != "" {
		walk(root, segs)
	}
	return out, unread
}
