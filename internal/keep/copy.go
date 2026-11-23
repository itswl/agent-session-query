package keep

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Settle is how long a file is left alone before it is copied: a session being written is
// copied once it rests, not at every line it gains. What a CLI deletes is what nobody has
// used for weeks.
const Settle = 5 * time.Minute

// Copy copies a session's files from the store at from into the folder at to, each to the
// same path beneath it as beneath the store, and reports whether the copy is whole and how
// many bytes it wrote. It is not whole while a file is still being written (see Settle),
// and a later Copy takes it up; force copies such a file as it is now. A file whose copy
// has its size and its time is not copied again. A file the store does not hold is passed
// over, and a copy of it made before is kept. Nothing is written outside to.
func Copy(files []string, from, to string, now time.Time, force bool) (whole bool, wrote int64, err error) {
	whole = true
	for _, path := range files {
		rel, rerr := filepath.Rel(from, path)
		if rerr != nil || !filepath.IsLocal(rel) {
			continue // not beneath the store: no adapter names such a file, and none is written
		}
		info, serr := os.Stat(path)
		if serr != nil || !info.Mode().IsRegular() {
			continue
		}
		if !force && now.Sub(info.ModTime()) < Settle {
			whole = false
			continue
		}
		dst := filepath.Join(to, rel)
		if have, err := os.Stat(dst); err == nil && same(have, info) {
			continue
		}
		n, cerr := copyFile(path, dst, info)
		wrote += n
		if cerr != nil {
			whole, err = false, errors.Join(err, cerr)
		}
	}
	return whole, wrote, err
}

// same reports whether a copy is of the file as it is: the same size, and the same time
// to within what a folder on another disk keeps of a time (FAT's two seconds).
func same(copy, file fs.FileInfo) bool {
	d := copy.ModTime().Sub(file.ModTime())
	return copy.Size() == file.Size() && d > -2*time.Second && d < 2*time.Second
}

// copyFile writes a file's copy, as it was when it was looked at: so many bytes, and its
// time. The copy is written beside where it goes and put in place whole, so that a reader
// finds the old copy or the new one, never part of one; it is its owner's alone.
func copyFile(src, dst string, info fs.FileInfo) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return 0, err
	}
	n, err := io.CopyN(tmp, in, info.Size())
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(tmp.Name(), info.ModTime(), info.ModTime())
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return n, fmt.Errorf("keeping %s: %w", src, err)
	}
	return n, nil
}

// Size is what a folder of copies holds: how many files, and how many bytes.
func Size(dir string) (files int, bytes int64) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				files++
				bytes += info.Size()
			}
		}
		return nil
	})
	return files, bytes
}
