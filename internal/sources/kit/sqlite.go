package kit

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Another program's database is read the way a guest reads: read-only, waiting out the
// owner's writes, and never leaving a file behind.

// busyTimeout is how long a read waits for a write to finish before SQLite calls the
// database locked. Every database read here belongs to a program that is still writing
// to it; without a timeout SQLite gives up at once.
const busyTimeout = 2 * time.Second

// DSN is the read-only connection string for a database file. Backslashes inside a URI
// are ambiguous with escapes, so a Windows path is given forward slashes, which SQLite
// accepts there.
func DSN(path string) string {
	return FileURI(path) + "?mode=ro&_pragma=busy_timeout(" + strconv.FormatInt(busyTimeout.Milliseconds(), 10) + ")"
}

// FileURI names a database file the way SQLite takes a URI. Backslashes become slashes,
// and the three characters that mean something else inside a URI are escaped: a
// directory named "notes #2" is a path, not a path and a fragment.
func FileURI(path string) string {
	return "file:" + strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
}

// retryPauses are the waits between a read that met a hot journal and its next try.
//
// A hot journal is the -journal file a crashed write leaves behind, and a read-only
// connection cannot roll one back: SQLite fails the read and never consults the busy
// timeout. But a journal looks hot whenever no visible lock is held on the database, and
// the locks of a writer inside a container are not visible from the host: there the
// journal of a commit still in progress looks hot. Such a read was early, not wrong, and
// one a moment later finds the journal gone. A journal that outlasts every pause is a
// real one, and the read fails.
var retryPauses = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}

// Pause waits between retries; a test swaps it to finish the write in between.
var Pause = time.Sleep

// ReadDB opens a database read-only and runs read on it, again after each pause while
// the read keeps meeting a journal that looks hot. read must be safe to repeat.
func ReadDB(ctx context.Context, path string, read func(db *sql.DB) error) error {
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	err = read(db)
	for _, pause := range retryPauses {
		if !hotJournal(err) || ctx.Err() != nil {
			break
		}
		Pause(pause)
		err = read(db)
	}
	return err
}

func hotJournal(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_READONLY_ROLLBACK
}

// Columns is the set of a table's columns. Another program's schema moves between its
// versions, and SQLite rejects an unknown column outright — a query naming one that is
// gone does not degrade, it fails, and the failure looks like an empty database. An
// adapter reads what is there and builds its query from it.
func Columns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// HasTable reports whether a database has a table of that name.
func HasTable(db *sql.DB, name string) bool {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return err == nil && n > 0
}

// StampDB fingerprints a database together with the three files SQLite keeps beside it.
// A write lands in the write-ahead log first: the database's own mtime stays put while
// its content changes, so watching the one file would call a changed database unchanged.
func StampDB(path string) (string, time.Time) {
	return Stamp(path, path+"-wal", path+"-shm", path+"-journal")
}

// Snapshot remembers one answer read from a database at a fingerprint of it, so a store
// that has not changed a byte is not queried again. Only a successful read is kept, and
// the fingerprint is the one taken before the read: a write landing mid-read moves it,
// and the next call reads again. A write landing in the same tick of the filesystem's
// clock as the one before does not — a page rewritten in place keeps the file's size —
// so a fingerprint is trusted only once the database has been still (see Still).
type Snapshot[T any] struct {
	mu    sync.Mutex
	stamp string // "" while the value is only the last answer, served for no fingerprint
	value T
	ok    bool
}

// Load returns the remembered value while path's fingerprint holds, and reads otherwise.
// A fingerprint taken while the database was not yet still holds nothing, moved or not:
// what is read under it is returned, and kept only as the last answer, and the next call
// reads again. A read that fails returns its error together with the last value that was
// read, if there was one: a database locked for a moment is a reason to say so, not to
// forget what it said a moment ago.
func (s *Snapshot[T]) Load(path string, read func() (T, error)) (T, error) {
	stamp, mod := StampDB(path)
	if !Still(mod) {
		stamp = "" // no fingerprint is empty: this one matches nothing
	}
	s.mu.Lock()
	if s.ok && stamp != "" && s.stamp == stamp {
		value := s.value
		s.mu.Unlock()
		return value, nil
	}
	s.mu.Unlock()
	value, err := read()
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.ok {
			return s.value, err
		}
		return value, err
	}
	s.mu.Lock()
	s.stamp, s.value, s.ok = stamp, value, true
	s.mu.Unlock()
	return value, nil
}

// NullStr is a nullable column as a string.
func NullStr(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}
