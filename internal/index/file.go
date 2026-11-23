package index

// The file the index is kept in: that this process reads and writes the file at the index's
// path and not one another asq took away, and that a damaged one is set aside and made anew.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/itswl/agent-session-query/internal/sources/kit"
)

// How many connections each pool keeps open while idle: the writer's one, and two readers.
const (
	rwIdle = 1
	roIdle = 2
)

// pool opens a pool of connections to the index file, each made by a connector of the
// generation gen counts.
func pool(dsn string, gen *atomic.Int64, open, idle int) (*sql.DB, error) {
	base, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(&connector{Connector: base, gen: gen})
	db.SetMaxOpenConns(open)
	db.SetMaxIdleConns(idle)
	return db, nil
}

// connector opens the connections of one pool. Each is marked with the generation of the
// index file it was opened to, and one of an earlier generation is let go of when it is
// next taken from the pool or put back in it (see Index.follow).
//
// The last connection to a file closing deletes the log and the shared memory beside it,
// by their names — but not when the file has been removed or renamed since it was opened,
// which SQLite looks at first: letting go of a file another asq replaced leaves the new
// file's alone.
type connector struct {
	driver.Connector
	gen *atomic.Int64
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	gen := c.gen.Load()
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &fileConn{Conn: conn, gen: gen, now: c.gen}, nil
}

// fileConn is a connection, and the generation of the index file it was opened to. It
// passes on everything the driver's connections do (see sqlite.NewConnector).
type fileConn struct {
	driver.Conn
	gen int64
	now *atomic.Int64
}

func (c *fileConn) Unwrap() driver.Conn { return c.Conn }

// current reports whether the connection is to the file at the index's path, as last
// looked at.
func (c *fileConn) current() bool { return c.gen == c.now.Load() }

func (c *fileConn) ResetSession(ctx context.Context) error {
	if !c.current() {
		return driver.ErrBadConn
	}
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *fileConn) IsValid() bool {
	if !c.current() {
		return false
	}
	v, ok := c.Conn.(driver.Validator)
	return !ok || v.IsValid()
}

func (c *fileConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin() //nolint:staticcheck // what database/sql does without BeginTx
}

func (c *fileConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *fileConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.Conn.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *fileConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := c.Conn.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *fileConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// letGo closes the connections a pool holds idle, and keeps idle ones again from now.
func letGo(db *sql.DB, idle int) {
	db.SetMaxIdleConns(0)
	db.SetMaxIdleConns(idle)
}

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

// identify is the file at path, as os.SameFile tells one file from another. On Windows that
// is looked up from the path when first asked, and asked here, so that it is this file's
// and not the one at the path when it is next compared.
func identify(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err == nil {
		os.SameFile(info, info)
	}
	return info, err
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

// Damage reports whether err, met reading or writing the index, says the file is damaged;
// if it does, the next sync sets the file aside and makes a new one in its place (see
// follow). The index is a cache: what a new one holds is read again from the stores.
func (ix *Index) Damage(err error) bool {
	if !Damaged(err) {
		return false
	}
	problem := err.Error()
	ix.damage.CompareAndSwap(nil, &problem)
	return true
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

// follow keeps this process on the file at the index's path. Another asq may have removed
// the file (asq index rm), made it anew (asq index rebuild), or set it aside as damaged;
// a process that went on with the file it had open would go on alone, reading and writing
// what no other asq sees, and keeping the space of a file nobody can name. A file this
// process found damaged (see Damage) is set aside here. Either way every connection to the
// old file is let go of — the idle ones now, those in use once they are done — and the file
// at the path, made when there is none, is prepared and used from now on.
//
// It runs as a sync begins, under syncMu: a sync is the one writer, and the one thing a
// process that runs for long does regularly. It says what it did, or "" when nothing.
func (ix *Index) follow() (string, error) {
	var why string
	damage := ix.damage.Swap(nil)
	if damage == nil {
		now, err := os.Stat(ix.path)
		switch {
		case err == nil && os.SameFile(now, ix.file):
			return "", nil
		case err == nil:
			why = "the index file was replaced, by another asq or by hand: using the new one"
		case errors.Is(err, fs.ErrNotExist):
			why = "the index file was removed: a new one is made in its place, from the stores"
		default:
			return "", err
		}
	}
	ix.rwGen.Add(1)
	ix.roGen.Add(1)
	letGo(ix.rw, rwIdle)
	letGo(ix.ro, roIdle)
	if damage != nil {
		aside, err := setAside(ix.path)
		if err != nil {
			ix.damage.CompareAndSwap(nil, damage) // tried again at the next sync
			return "", fmt.Errorf("the index is damaged (%s), and could not be set aside: %w", *damage, err)
		}
		why = Repair{Problem: *damage, Aside: aside}.String()
	}
	if err := create(ix.path); err != nil {
		return why, err
	}
	if err := prepare(ix.rw); err != nil {
		ix.Damage(err)
		return why, err
	}
	info, err := identify(ix.path)
	if err != nil {
		return why, err
	}
	ix.file = info
	ix.recount()
	return why, nil
}

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
