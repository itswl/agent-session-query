package index

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/itswl/agent-session-query/internal/sources/kit"
)

// Index is the open index file.
type Index struct {
	path string
	rw   *sql.DB // one connection: every write goes through it, in order
	ro   *sql.DB // the readers
	enc  *zstd.Encoder
	dec  *zstd.Decoder

	syncMu  sync.Mutex // one sync at a time
	stateMu sync.Mutex
	state   SyncState
	subs    map[chan Change]struct{}
}

// DefaultPath is where the index lives unless told otherwise: the user's cache
// directory, since everything in it can be rebuilt. ASQ_INDEX moves it.
func DefaultPath() string {
	if p := os.Getenv("ASQ_INDEX"); p != "" {
		return p
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "asq", "index-v"+strconv.Itoa(schemaVersion)+".db")
}

// Open opens the index at path, creating it when it is not there. The file holds
// everything the transcripts do, so it is created readable by its owner alone.
func Open(path string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		f.Close()
	}
	uri := kit.FileURI(path)
	rw, err := sql.Open("sqlite", uri+"?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-65536)")
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	rw.SetMaxOpenConns(1)
	if _, err := rw.Exec(schema); err != nil {
		rw.Close()
		return nil, fmt.Errorf("index: %s: %w", path, err)
	}
	ro, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=busy_timeout(10000)&_pragma=query_only(1)&_pragma=cache_size(-32768)")
	if err != nil {
		rw.Close()
		return nil, fmt.Errorf("index: %w", err)
	}
	ro.SetMaxOpenConns(8)
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	return &Index{path: path, rw: rw, ro: ro, enc: enc, dec: dec, subs: map[chan Change]struct{}{}}, nil
}

// Path is the index file.
func (ix *Index) Path() string { return ix.path }

// Close releases the index.
func (ix *Index) Close() error {
	ix.dec.Close()
	ix.ro.Close()
	return ix.rw.Close()
}

// Remove deletes an index file and what SQLite keeps beside it.
func Remove(path string) error {
	var first error
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	return first
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMs(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// placeholders is "?,?,?" for n values.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// tx runs fn in one write transaction.
func (ix *Index) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := ix.rw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
