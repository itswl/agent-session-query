package app

import "os"

// program is the file a process was started from, as it was then.
type program struct {
	path string
	info os.FileInfo
}

// self is the file this process was started from.
var self = programAt(executable())

func executable() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}

// programAt is the file at path as it is now; nothing, when there is none.
func programAt(path string) program {
	if path == "" {
		return program{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return program{path: path}
	}
	os.SameFile(info, info) // on Windows, which file this is is looked up when first asked: now, not when compared
	return program{path: path, info: info}
}

// replaced reports whether the file at the program's path is another now, or the same
// written over: an upgrade installed there. A file no longer there is not taken for one
// replaced — an install that removes it first and then writes it may be half done.
func (p program) replaced() bool {
	if p.info == nil {
		return false
	}
	now, err := os.Stat(p.path)
	if err != nil {
		return false
	}
	return !os.SameFile(p.info, now) || now.Size() != p.info.Size() || !now.ModTime().Equal(p.info.ModTime())
}

// Program is the file this process was started from, and whether it has been replaced since
// — an upgrade installed over it, which a process that runs for long, a server, goes on
// without until it is restarted. Nothing is asked of anywhere else: the file is compared
// with itself as it was when the process started.
func Program() (path string, replaced bool) { return self.path, self.replaced() }
