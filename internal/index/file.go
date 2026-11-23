package index

// The file the index is kept in: a damaged one is set aside and made anew.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/itswl/agent-session-query/internal/sources/kit"
)

// create makes the index file when it is not there, readable by its owner alone: it holds
// everything the transcripts do. A file that cannot be opened is SQLite's to say so of.
func create(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		f.Close()
	}
	return nil
}

// Damaged reports whether err says the index file is damaged: not a database at all, or
// one whose pages do not hold together.
func Damaged(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code() & 0xff {
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return true
	}
	return false
}

// Repair is a damaged index file set aside, and a new one made in its place.
type Repair struct {
	Problem string // what SQLite said of the damaged file
	Aside   string // where it was put
}

func (r Repair) String() string {
	return fmt.Sprintf("the index was damaged (%s): it was set aside as %s, and a new one is made from the stores", r.Problem, r.Aside)
}

// setAside moves a damaged index file out of the way, beside where it was, as
// <name>.damaged — the last one alone is kept — and removes the log and the shared memory
// SQLite keeps beside it: they are the damaged file's, and would be taken for the new one's.
func setAside(path string) (string, error) {
	aside := path + damagedSuffix
	if err := os.Remove(aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(path, aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return aside, nil
}

const damagedSuffix = ".damaged"

// Checked is what a look at the whole index file found.
type Checked struct {
	Path  string `json:"path"`
	Found bool   `json:"found"`
	Bytes int64  `json:"bytes,omitempty"`
	// Problem is what SQLite found wrong with the file; empty when it holds together
	Problem string `json:"problem,omitempty"`
}

// Check reads the whole index file at path, and says whether it holds together as SQLite's
// quick_check has it, without changing it: none of the pages that hold it is damaged, and
// each table and index is in order. It reads every page, which is a second or two for a
// large one.
func Check(ctx context.Context, path string) (Checked, error) {
	ck := Checked{Path: path}
	if info, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return ck, nil
	} else if err != nil {
		return ck, err
	} else if info.IsDir() {
		return ck, fmt.Errorf("%s is a directory", path)
	}
	ck.Found = true
	db, err := sql.Open("sqlite", kit.FileURI(path)+"?mode=ro&_pragma=busy_timeout(10000)&_pragma=query_only(1)")
	if err != nil {
		return ck, err
	}
	defer db.Close()
	damaged := func(err error) (Checked, error) {
		if Damaged(err) {
			ck.Problem = err.Error()
			return ck, nil
		}
		return ck, err
	}
	if err := db.QueryRowContext(ctx, `SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()`).Scan(&ck.Bytes); err != nil {
		return damaged(err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check(5)`)
	if err != nil {
		return damaged(err)
	}
	defer rows.Close()
	var said []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return damaged(err)
		}
		if line != "ok" {
			said = append(said, line)
		}
	}
	if err := rows.Err(); err != nil {
		return damaged(err)
	}
	ck.Problem = strings.Join(said, "; ")
	return ck, nil
}
