package source

import (
	"os"
	"sync"
)

// A SQLite source is asked the same question again and again while the database under it
// has not changed a byte. The list is rebuilt on every poll and /usage re-aggregates the
// whole history; the CLIs are idle most of the time, so most of that work is repeated for
// nothing. dbSnapshotCache remembers the answer read at a fingerprint of the database and
// its companions, and a stat answers the next call when nothing moved.
//
// The fingerprint includes -wal / -shm / -journal because a write lands in the write-ahead
// log first: the main database's mtime stays put while the session data changes, and
// watching only the database file would serve last call's list forever.
type fileStamp struct {
	mod  int64
	size int64
}

// dbStamp is the database and the three files SQLite keeps beside it, in that order
type dbStamp [4]fileStamp

// stampDatabase stats a database and its companions. A missing companion is a stamp of
// its own (-1), so a -wal appearing or vanishing counts as a change.
func stampDatabase(path string) dbStamp {
	var st dbStamp
	for i, suffix := range [...]string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			st[i] = fileStamp{mod: -1, size: -1}
			continue
		}
		st[i] = fileStamp{mod: info.ModTime().UnixNano(), size: info.Size()}
	}
	return st
}

type snapshotEntry[T any] struct {
	stamp dbStamp
	value T
}

// dbSnapshotCache is one source's answers by database path: the value, and the stamp it
// was read at. Only successful reads are entered — a failed one must be retried on the
// next call, not cached.
//
// The stamp is taken before the read: a write that lands mid-read moves the stamp, so the
// entry is never reused for it and the next call re-reads; a value read while the file was
// changing is never served as if it were current.
type dbSnapshotCache[T any] struct {
	mu      sync.Mutex
	entries map[string]snapshotEntry[T]
}

// load returns the cached value when path's fingerprint is unchanged, else runs read once
// and banks its value under the fingerprint that was current before the read.
func (c *dbSnapshotCache[T]) load(path string, read func() (T, error)) (T, error) {
	stamp := stampDatabase(path)
	c.mu.Lock()
	entry, hit := c.entries[path]
	c.mu.Unlock()
	if hit && entry.stamp == stamp {
		return entry.value, nil
	}
	value, err := read()
	if err != nil {
		return value, err
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]snapshotEntry[T])
	}
	c.entries[path] = snapshotEntry[T]{stamp: stamp, value: value}
	c.mu.Unlock()
	return value, nil
}
