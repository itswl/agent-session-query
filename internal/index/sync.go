package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/itswl/agent-session-query/internal/sources"
)

// Store is one session store the index is kept in step with.
type Store struct {
	Name   string // "claude", or a labeled instance: "claude@box2"
	Spec   sources.Spec
	Source sources.Source
}

// SyncState is where a sync stands, for whoever is waiting on one.
type SyncState struct {
	Running bool      `json:"running"`
	Total   int       `json:"total,omitempty"` // transcripts this sync has to read
	Done    int       `json:"done,omitempty"`  // of which read
	At      time.Time `json:"at,omitzero"`     // when the last sync finished
	Took    int64     `json:"tookMs,omitempty"`
	// Stores names the condition the stores were last found in — which are there, which
	// could not be read — in a few characters that change when it does. A reader that
	// keeps a health report asks for a new one when this moves, and not otherwise.
	Stores string `json:"stores,omitempty"`
	// Began is when the sync in flight started.
	Began time.Time `json:"-"`
}

// Change names a session a sync wrote.
type Change struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	Gone   bool   `json:"gone,omitempty"`
}

// SyncResult is what one sync did.
type SyncResult struct {
	Scanned int      // transcripts the stores hold
	Read    int      // of which read, because new or changed
	Removed int      // transcripts the stores no longer hold
	Changes []Change // the sessions whose record changed
}

// SyncOptions tune one sync.
type SyncOptions struct {
	// Force reads a changed transcript even when it was read a moment ago.
	Force bool
	// Rebuild reads everything again, whatever the fingerprints say.
	Rebuild bool
	// CaughtUp, when set, is called once the sync has read every transcript that is new
	// or whose fingerprint moved, if it still has others to read: the ones read again only
	// because their adapter now reads them differently (sources.Spec.Reads), which can be
	// the whole history, and come last. The index is then as new as the stores, and what
	// is left only reads it better. It is called at most once, and not when ctx ends first.
	CaughtUp func()
}

// patience bounds how often a transcript that keeps changing is read again: no sooner
// than this many times what its last read cost, and never later than maxPatience. A
// small session is followed as it is written; a hundred-megabyte one, which costs
// seconds to read, is not read on every append.
const (
	patience    = 8
	maxPatience = 45 * time.Second
)

// State is where the index stands.
func (ix *Index) State() SyncState {
	ix.stateMu.Lock()
	defer ix.stateMu.Unlock()
	return ix.state
}

// Subscribe returns a channel of the sessions each sync writes, and a function that
// ends the subscription. A subscriber that falls behind misses changes rather than
// holding the sync back.
func (ix *Index) Subscribe() (<-chan Change, func()) {
	ch := make(chan Change, 256)
	ix.stateMu.Lock()
	ix.subs[ch] = struct{}{}
	ix.stateMu.Unlock()
	return ch, func() {
		ix.stateMu.Lock()
		delete(ix.subs, ch)
		ix.stateMu.Unlock()
	}
}

func (ix *Index) publish(changes []Change) {
	ix.stateMu.Lock()
	defer ix.stateMu.Unlock()
	for ch := range ix.subs {
		for _, c := range changes {
			select {
			case ch <- c:
			default:
			}
		}
	}
}

type known struct {
	sid    int64
	stamp  string
	reads  int
	id     string
	readMs int64
	readAt int64
}

type job struct {
	store Store
	ref   sources.Ref
	again bool // read only because the adapter's reading is newer than the index's
}

// Sync brings the index in step with the stores: every transcript that is new or whose
// fingerprint moved is read again, newest first, and every one a store no longer holds
// is removed. Only one sync runs at a time; a second caller waits for the first and then
// runs its own, which finds little left to do.
func (ix *Index) Sync(ctx context.Context, stores []Store, opts SyncOptions) (SyncResult, error) {
	ix.syncMu.Lock()
	defer ix.syncMu.Unlock()
	start := time.Now()
	var res SyncResult

	// What the stores hold now, against what was read before
	var jobs []job
	var gone []known
	type scanned struct {
		refs []sources.Ref
		err  error
	}
	scans := make([]scanned, len(stores))
	var wg sync.WaitGroup
	for i, st := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st.Source.Exists() {
				scans[i].refs, scans[i].err = st.Source.Scan(ctx)
			}
		}()
	}
	wg.Wait()
	now := time.Now()
	condition := sha256.New()
	for i, st := range stores {
		sc := &scans[i]
		errText := ""
		if sc.err != nil {
			errText = sc.err.Error()
		}
		fmt.Fprintf(condition, "%s\x00%s\n", st.Name, errText)
		if _, err := ix.rw.ExecContext(ctx, `
INSERT INTO source_state (source, location, error, scanned_at) VALUES (?,?,?,?)
ON CONFLICT (source) DO UPDATE SET location=excluded.location, error=excluded.error, scanned_at=excluded.scanned_at`,
			st.Name, st.Source.Location(), errText, now.UnixMilli()); err != nil {
			return res, err
		}
		// A store that could not be read is not an empty one: what is known of it is kept.
		// One that was read in part still gives what it listed, and loses nothing.
		partial := sc.err != nil
		if partial && len(sc.refs) == 0 {
			continue
		}
		have, err := ix.known(ctx, st.Name)
		if err != nil {
			return res, err
		}
		res.Scanned += len(sc.refs)
		seen := make(map[string]bool, len(sc.refs))
		for _, ref := range sc.refs {
			seen[ref.Key] = true
			k, ok := have[ref.Key]
			switch {
			case !ok, opts.Rebuild:
				jobs = append(jobs, job{store: st, ref: ref})
			case k.stamp != ref.Stamp && k.reads >= st.Spec.Reads:
				wait := min(time.Duration(k.readMs*patience)*time.Millisecond, maxPatience)
				if opts.Force || now.Sub(time.UnixMilli(k.readAt)) >= wait {
					jobs = append(jobs, job{store: st, ref: ref})
				}
			case k.reads < st.Spec.Reads:
				// Read with an older reading of the CLI's files (sources.Spec.Reads), or by an
				// asq that keeps none (see readsTrigger): read again, once, after what is new. A
				// session a newer reading stored is left to it, so that an older asq sharing
				// the index does not undo a newer one's reading
				jobs = append(jobs, job{store: st, ref: ref, again: k.stamp == ref.Stamp})
			}
		}
		for key, k := range have {
			if !seen[key] && !partial {
				gone = append(gone, k)
				res.Changes = append(res.Changes, Change{Source: st.Name, ID: k.id, Gone: true})
			}
		}
	}

	if len(gone) > 0 {
		err := ix.tx(ctx, func(tx *sql.Tx) error {
			for _, k := range gone {
				if err := deleteSession(ctx, tx, k.sid); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		res.Removed = len(gone)
	}

	// What is new before what is only read better, and newest first: the sessions someone
	// is most likely to ask about are there first
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].again != jobs[j].again {
			return !jobs[i].again
		}
		return jobs[i].ref.ModTime.After(jobs[j].ref.ModTime)
	})
	fresh := 0 // jobs that are not read again only for a new reading: they come first
	for fresh < len(jobs) && !jobs[fresh].again {
		fresh++
	}
	// Sessions with turns whose words are to be written again from the index (see refill):
	// before anything is read, since after the words table was made anew such a turn is
	// not found by a search at all. None when a newer asq made the table: they are its
	var stale int
	if ahead, err := wordsAhead(ctx, ix.rw); err != nil {
		return res, err
	} else if !ahead {
		if err := ix.rw.QueryRowContext(ctx, `SELECT count(DISTINCT sid) FROM words_stale`).Scan(&stale); err != nil {
			return res, err
		}
	}
	found := hex.EncodeToString(condition.Sum(nil)[:5])
	ix.setState(func(s *SyncState) {
		s.Running, s.Total, s.Done, s.Began, s.Stores = true, stale+len(jobs), 0, start, found
	})

	var firstErr error
	if stale > 0 {
		firstErr = ix.refill(ctx, func() { ix.setState(func(s *SyncState) { s.Done++ }) })
	}
	published := 0
	// settle makes what has been written so far whole: the sessions linked to one another
	// and the counts let go of
	settle := func() {
		if res.Read > 0 || res.Removed > 0 {
			ix.recount()
			if ctx.Err() == nil {
				if err := ix.link(ctx); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	tell := func() {
		if len(res.Changes) > published {
			ix.publish(res.Changes[published:])
			published = len(res.Changes)
		}
	}
	// caughtUp is when the index holds what the stores do, with a part of it read again
	// for a new reading still to come
	caughtUp := func() {
		if opts.CaughtUp == nil || ctx.Err() != nil {
			return
		}
		settle()
		tell()
		opts.CaughtUp()
	}
	if fresh == 0 && len(jobs) > 0 {
		caughtUp()
	}
	if len(jobs) > 0 {
		readings := make(chan *reading, 8)
		queue := make(chan job)
		workers := min(max(runtime.GOMAXPROCS(0)-1, 1), 8, len(jobs))
		var readers sync.WaitGroup
		for range workers {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for j := range queue {
					rd := ix.read(ctx, j.store, j.ref)
					rd.again = j.again
					readings <- rd
				}
			}()
		}
		go func() {
			defer close(queue)
			for _, j := range jobs {
				select {
				case queue <- j:
				case <-ctx.Done():
					return
				}
			}
		}()
		go func() {
			readers.Wait()
			close(readings)
		}()
		left := fresh
		for rd := range readings {
			if ctx.Err() != nil {
				continue // drain: the readers are stopping
			}
			_, changed, err := ix.write(ctx, rd)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			res.Read++
			if changed {
				res.Changes = append(res.Changes, Change{Source: rd.source, ID: rd.sess.ID})
			}
			ix.setState(func(s *SyncState) { s.Done++ })
			if !rd.again {
				if left--; left == 0 && fresh < len(jobs) {
					caughtUp()
				}
			}
		}
	}

	settle()
	ix.setState(func(s *SyncState) {
		s.Running, s.At, s.Took = false, time.Now(), time.Since(start).Milliseconds()
	})
	tell()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return res, firstErr
}

func (ix *Index) setState(fn func(*SyncState)) {
	ix.stateMu.Lock()
	fn(&ix.state)
	ix.stateMu.Unlock()
}

// known is what the index holds of one store: each transcript's fingerprint.
func (ix *Index) known(ctx context.Context, source string) (map[string]known, error) {
	rows, err := ix.rw.QueryContext(ctx, `SELECT sid, key, stamp, reads, id, read_ms, read_at FROM sessions WHERE source = ?`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]known{}
	for rows.Next() {
		var k known
		var key string
		if err := rows.Scan(&k.sid, &key, &k.stamp, &k.reads, &k.id, &k.readMs, &k.readAt); err != nil {
			return nil, err
		}
		out[key] = k
	}
	return out, rows.Err()
}

// Forget removes every session of the stores that are not among keep. A store that was
// configured once and is not anymore would otherwise stay in the index for good.
func (ix *Index) Forget(ctx context.Context, keep []Store) (int, error) {
	names := make([]any, len(keep))
	for i, st := range keep {
		names[i] = st.Name
	}
	where := "1 = 1"
	if len(names) > 0 {
		where = "source NOT IN (" + placeholders(len(names)) + ")"
	}
	var sids []int64
	rows, err := ix.rw.QueryContext(ctx, `SELECT sid FROM sessions WHERE `+where, names...)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return 0, err
		}
		sids = append(sids, sid)
	}
	rows.Close()
	if len(sids) == 0 {
		return 0, rows.Err()
	}
	err = ix.tx(ctx, func(tx *sql.Tx) error {
		for _, sid := range sids {
			if err := deleteSession(ctx, tx, sid); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM source_state WHERE `+where, names...)
		return err
	})
	ix.recount()
	return len(sids), err
}
