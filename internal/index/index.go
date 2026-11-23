package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

	// How many sessions the index holds of each store, as last counted (see widen)
	sourcesMu sync.Mutex
	sources   map[string]int
	sourcesAt time.Time
}

// widen marks a filter whose stores hold most of the index, for a query the time should
// drive — bounded in time, or ordered by it. Its store term then filters and does not
// choose the plan (see Filter.where): stores that are most of the index narrow it
// little, and the time narrows it more. Where they are the lesser part — a process
// confined to one store of an index others share — the store term is the narrower, and
// drives. Results are the same either way; only the work differs.
func (ix *Index) widen(ctx context.Context, f Filter, timed bool) Filter {
	f.wide = timed && len(f.Stores) > 0 && ix.mostly(ctx, f.Stores)
	return f
}

// mostly reports whether stores hold at least half the sessions in the index. The counts
// are kept for a minute and dropped when a sync of this process changes the index; a
// change another process made is noticed within the minute, and till then only costs
// work.
func (ix *Index) mostly(ctx context.Context, stores []string) bool {
	ix.sourcesMu.Lock()
	defer ix.sourcesMu.Unlock()
	if ix.sources == nil || time.Since(ix.sourcesAt) > time.Minute {
		rows, err := ix.ro.QueryContext(ctx, `SELECT source, count(*) FROM sessions GROUP BY source`)
		if err != nil {
			return false
		}
		have := map[string]int{}
		for rows.Next() {
			var s string
			var n int
			if rows.Scan(&s, &n) == nil {
				have[s] = n
			}
		}
		rows.Close()
		if rows.Err() != nil {
			return false
		}
		ix.sources, ix.sourcesAt = have, time.Now()
	}
	named, all := 0, 0
	for s, n := range ix.sources {
		all += n
		if slices.Contains(stores, s) {
			named += n
		}
	}
	return 2*named >= all
}

// recount drops the counts of sessions per store, after the index changed.
func (ix *Index) recount() {
	ix.sourcesMu.Lock()
	ix.sources = nil
	ix.sourcesMu.Unlock()
}

// addColumn adds a column that a table an older asq made lacks. An index at a path of its
// own — ASQ_INDEX, --index, a container's volume — is not named by schemaVersion: it is the
// one the older asq used, and gains the column, every row at what the declaration gives.
// The look is made again under the write lock: two processes opening such an index at once
// would otherwise both find the column missing, and one fail to add it.
func addColumn(db *sql.DB, table, column, decl string) error {
	has := func(q interface {
		QueryRow(string, ...any) *sql.Row
	}) (bool, error) {
		var n int
		err := q.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
		return n > 0, err
	}
	if ok, err := has(db); err != nil || ok {
		return err
	}
	tx, err := db.Begin() // BEGIN IMMEDIATE: see Open
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if ok, err := has(tx); err != nil || ok {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl); err != nil {
		return err
	}
	return tx.Commit()
}

// newWords makes the words table anew when an older asq made it — its wordsVersion lower
// than this one's, or none: dropped and created again, empty, with every turn the index
// holds named in words_stale for the next sync to write its words from what the index
// holds (see Index.refill). Nothing is read from a transcript for it, and an asq that does
// not know the table's new shape goes on using it: the columns it writes are the first
// five, as they were. A table a newer asq made is left as it is (see wordsAhead). The look
// is made again under the write lock, as in addColumn.
func newWords(db *sql.DB) error {
	ctx := context.Background()
	if v, err := wordsMadeBy(ctx, db); err != nil || v >= wordsVersion {
		return err
	}
	tx, err := db.Begin() // BEGIN IMMEDIATE: see Open
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if v, err := wordsMadeBy(ctx, tx); err != nil || v >= wordsVersion {
		return err
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS words`,
		wordsTable,
		`INSERT OR IGNORE INTO words_stale (sid, idx) SELECT sid, idx FROM turns`,
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('words', '` + strconv.Itoa(wordsVersion) + `')`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// querier is a database or a transaction, to read from.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// wordsMadeBy is the wordsVersion of the asq that made the words table: 0 when the index
// does not say, as one before wordsVersion leaves it.
func wordsMadeBy(ctx context.Context, q querier) (int, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'words'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	n, _ := strconv.Atoi(v)
	return n, err
}

// wordsAhead reports whether a newer asq made the words table: one that writes what it
// holds otherwise. This one leaves it as it is, searches it as well as it can, and writes
// the columns it knows for the turns it reads; what words_stale names is the newer asq's
// to write, and is not written or cleared here (see Index.write and Index.refill). It is
// asked at each write, since a newer asq may make the table anew while this one has the
// index open.
func wordsAhead(ctx context.Context, q querier) (bool, error) {
	v, err := wordsMadeBy(ctx, q)
	return v > wordsVersion, err
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

// Older lists the index files earlier versions of asq left beside this one's, at the
// default path: each a whole copy of the history as that version read it, which this one
// does not read. None when the index is at a path of its own (ASQ_INDEX).
func Older() []string {
	if os.Getenv("ASQ_INDEX") != "" {
		return nil
	}
	dir := filepath.Dir(DefaultPath())
	var old []string
	for v := range schemaVersion {
		path := filepath.Join(dir, "index-v"+strconv.Itoa(v)+".db")
		if _, err := os.Stat(path); err == nil {
			old = append(old, path)
		}
	}
	return old
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
	// The write-ahead log is reused after each checkpoint rather than shrunk; a burst of
	// writes — a rebuild — would leave it at its largest for good. journal_size_limit
	// cuts it back to a few megabytes whenever a checkpoint has emptied it.
	rw, err := sql.Open("sqlite", uri+"?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=journal_size_limit(4194304)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-65536)")
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	rw.SetMaxOpenConns(1)
	if _, err := rw.Exec(schema); err != nil {
		rw.Close()
		return nil, fmt.Errorf("index: %s: %w", path, err)
	}
	for _, col := range [][3]string{
		{"sessions", "reads", "INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "reads_seq", "INTEGER NOT NULL DEFAULT 0"},
		{"turns", "src", "BLOB NOT NULL DEFAULT x''"},
		{"sessions", "live_state", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "live", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumn(rw, col[0], col[1], col[2]); err != nil {
			rw.Close()
			return nil, fmt.Errorf("index: %s: %w", path, err)
		}
	}
	for _, col := range failuresColumns {
		if err := addColumn(rw, "failures", col[0], col[1]); err != nil {
			rw.Close()
			return nil, fmt.Errorf("index: %s: %w", path, err)
		}
	}
	for _, step := range []func(*sql.DB) error{
		func(db *sql.DB) error { _, err := db.Exec(readsTrigger); return err },
		func(db *sql.DB) error { _, err := db.Exec(wordsStale); return err },
		func(db *sql.DB) error { _, err := db.Exec(failuresJudged); return err },
		newWords,
	} {
		if err := step(rw); err != nil {
			rw.Close()
			return nil, fmt.Errorf("index: %s: %w", path, err)
		}
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
