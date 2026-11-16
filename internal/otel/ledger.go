package otel

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/itswl/agent-session-query/internal/sources/kit"
)

// The ledger remembers what was sent where: for each turn, the hash of what it held
// when a collector last took it. A turn whose hash still matches is not sent again; one
// that grew is, under the same ids.
//
// It is kept apart from the index on purpose. The index is a cache — rebuilt on a whim,
// replaced when its schema moves — and what a collector has already been sent is not
// something that can be read back off the transcripts. So the ledger lives with the
// state a program keeps, names turns by what they are (store, session id, turn) rather
// than by a row in some index file, and survives every rebuild.
type ledger struct {
	db   *sql.DB
	dest string
}

// LedgerPath is where the ledger lives unless told otherwise: ASQ_STATE, else the
// platform's place for a program's state.
func LedgerPath() string {
	dir := os.Getenv("ASQ_STATE")
	if dir == "" {
		switch {
		case os.Getenv("XDG_STATE_HOME") != "":
			dir = filepath.Join(os.Getenv("XDG_STATE_HOME"), "asq")
		case runtime.GOOS == "linux":
			if home, err := os.UserHomeDir(); err == nil {
				dir = filepath.Join(home, ".local", "state", "asq")
			}
		}
	}
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil || base == "" {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "asq")
	}
	return filepath.Join(dir, "otel-ledger.db")
}

// destOf names a collector in the ledger: its address without the credentials or the
// query a URL can carry. Two spellings of one endpoint are one destination.
func destOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return strings.TrimRight(endpoint, "/")
	}
	return strings.ToLower(u.Scheme+"://"+u.Host) + strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1/traces")
}

func openLedger(path, endpoint string) (*ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	db, err := sql.Open("sqlite", kit.FileURI(path)+"?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sent (
  dest    TEXT    NOT NULL,   -- the collector
  source  TEXT    NOT NULL,   -- the store
  session TEXT    NOT NULL,   -- the CLI's id for the session
  turn    INTEGER NOT NULL,
  hash    TEXT    NOT NULL,   -- what the turn held, and the shape it was sent in
  at      INTEGER NOT NULL,   -- when, unix seconds
  PRIMARY KEY (dest, source, session, turn)
) WITHOUT ROWID`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: %s: %w", path, err)
	}
	return &ledger{db: db, dest: destOf(endpoint)}, nil
}

func (l *ledger) close() error { return l.db.Close() }

type turnKey struct {
	source, session string
	turn            int
}

// sent reads what this collector has been sent of one store's sessions.
func (l *ledger) sent(ctx context.Context) (map[turnKey]string, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT source, session, turn, hash FROM sent WHERE dest = ?`, l.dest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[turnKey]string{}
	for rows.Next() {
		var k turnKey
		var hash string
		if err := rows.Scan(&k.source, &k.session, &k.turn, &hash); err != nil {
			return nil, err
		}
		out[k] = hash
	}
	return out, rows.Err()
}

// record writes down the turns a collector just took.
func (l *ledger) record(ctx context.Context, done map[turnKey]string) error {
	if len(done) == 0 {
		return nil
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for k, hash := range done {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sent (dest, source, session, turn, hash, at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (dest, source, session, turn) DO UPDATE SET hash = excluded.hash, at = excluded.at`,
			l.dest, k.source, k.session, k.turn, hash, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
