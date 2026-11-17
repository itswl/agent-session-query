package index

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// A session whose files have not moved is not read again — unless the adapter now reads
// its CLI's files differently (sources.Spec.Reads): then every session of the CLI read
// with an older reading is read again at the next sync, once. A session a newer reading
// stored is left to it, so that two binaries sharing an index do not undo each other.
func TestANewReadingIsReadAgain(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	sync := func(reads int) int {
		t.Helper()
		res, err := ix.Sync(ctx, []Store{{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: reads}, Source: many{n: 4}}}, SyncOptions{Force: true})
		if err != nil {
			t.Fatal(err)
		}
		return res.Read
	}
	type step struct {
		reads, want int
		why         string
	}
	steps := func(when string, steps []step) {
		t.Helper()
		for _, step := range steps {
			if n := sync(step.reads); n != step.want {
				t.Errorf("%s, reading %d: %d read, want %d (%s)", when, step.reads, n, step.want, step.why)
			}
		}
	}
	steps("an index", []step{
		{0, 4, "the first read"},
		{0, 0, "nothing moved"},
		{1, 4, "the first counted reading: every session read again"},
		{1, 0, "the same reading: nothing"},
		{2, 4, "a new reading: every session again"},
		{2, 0, "and once only"},
		{1, 0, "an older binary leaves what a newer reading stored"},
		{2, 0, "and the newer one finds it as it left it"},
	})

	// The files move, and an older binary is the one to read them: what it stored is its
	// reading, and the newer one reads them again, once
	moved := func(reads int, stamp string) int {
		t.Helper()
		res, err := ix.Sync(ctx, []Store{{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: reads}, Source: many{n: 4, stamp: stamp}}}, SyncOptions{Force: true})
		if err != nil {
			t.Fatal(err)
		}
		return res.Read
	}
	for _, step := range []struct {
		reads int
		stamp string
		want  int
		why   string
	}{
		{1, "2", 4, "an older binary reads what moved"},
		{2, "2", 4, "and the newer one reads it again"},
		{1, "2", 0, "once"},
		{2, "2", 0, "and once"},
	} {
		if n := moved(step.reads, step.stamp); n != step.want {
			t.Errorf("files moved, reading %d: %d read, want %d (%s)", step.reads, n, step.want, step.why)
		}
	}

	// An asq from before readings, at the same path, reads what moved and writes its own
	// reading over the row, saying nothing of readings: the reading the row names is not
	// the one it holds, and the files are read again, once
	if _, err := ix.rw.Exec(`UPDATE sessions SET stamp = '3'`); err != nil {
		t.Fatal(err)
	}
	if n := moved(2, "3"); n != 4 {
		t.Errorf("rows an asq from before readings rewrote: %d read again, want 4", n)
	}
	if n := moved(2, "3"); n != 0 {
		t.Errorf("and then %d", n)
	}
}

// gated is a store whose sessions in slow are not read until gate is closed.
type gated struct {
	keys []string
	slow map[string]bool
	gate chan struct{}
}

func (gated) Location() string { return "test" }
func (gated) Exists() bool     { return true }
func (g gated) Scan(context.Context) ([]sources.Ref, error) {
	refs := make([]sources.Ref, len(g.keys))
	for i, k := range g.keys {
		refs[i] = sources.Ref{Key: k, Stamp: "1", ModTime: time.Unix(int64(1790000000-i), 0)} // the new ones are the oldest
	}
	return refs, nil
}
func (g gated) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	if g.slow[ref.Key] {
		select {
		case <-g.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	w.S.ID = "s" + ref.Key
	w.Add(transcript.Message{Role: transcript.User, At: time.Now(), Blocks: []transcript.Block{{Type: transcript.Text, Text: "hello"}}})
	return nil
}

// A sync that reads the whole history again for a new reading reads what is new first,
// and says when it has: a question waits for that, and not for the rest.
func TestWhatIsNewIsReadBeforeWhatIsReadAgain(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	open := make(chan struct{})
	close(open)
	store := func(reads int, g gated) []Store {
		return []Store{{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: reads}, Source: g}}
	}
	if _, err := ix.Sync(ctx, store(1, gated{keys: []string{"0", "1", "2", "3"}, gate: open}), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	count := func(where string) (n int) {
		t.Helper()
		if err := ix.rw.QueryRow(`SELECT count(*) FROM sessions WHERE ` + where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Two sessions are new, and the four there were are to be read again: those wait
	// at the gate until the sync has said it caught up
	gate := make(chan struct{})
	g := gated{keys: []string{"0", "1", "2", "3", "4", "5"}, slow: map[string]bool{"0": true, "1": true, "2": true, "3": true}, gate: gate}
	changes, stop := ix.Subscribe()
	defer stop()
	calls := 0
	var atCatchUp struct{ sessions, older, told int }
	caughtUp := func() {
		calls++
		atCatchUp.sessions, atCatchUp.older = count("1"), count("reads < 2")
		for len(changes) > 0 {
			<-changes
			atCatchUp.told++
		}
		close(gate)
	}
	// within is a sync that must not wait at the gate: one that does is let through, late
	within := func(g gated, reads int, caughtUp func()) SyncResult {
		t.Helper()
		type result struct {
			res SyncResult
			err error
		}
		done := make(chan result, 1)
		go func() {
			res, err := ix.Sync(ctx, store(reads, g), SyncOptions{CaughtUp: caughtUp})
			done <- result{res, err}
		}()
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatal(r.err)
			}
			return r.res
		case <-time.After(10 * time.Second):
			close(g.gate)
			<-done
			t.Fatal("the sync waited for what it reads again before saying it caught up")
		}
		return SyncResult{}
	}
	res := within(g, 2, caughtUp)
	if calls != 1 {
		t.Fatalf("caught up %d times, want once", calls)
	}
	if atCatchUp.sessions != 6 || atCatchUp.older != 4 || atCatchUp.told != 2 {
		t.Errorf("when it caught up: %d sessions, %d of an older reading, %d changes told; want 6, 4, 2", atCatchUp.sessions, atCatchUp.older, atCatchUp.told)
	}
	if res.Read != 6 || count("reads = 2") != 6 {
		t.Errorf("after: %d read, %d of the new reading; want 6 and 6", res.Read, count("reads = 2"))
	}

	// Nothing to read again: nothing to catch up to. All of it to read again: caught up
	// before the first
	calls = 0
	g.keys = append(g.keys, "6")
	if _, err := ix.Sync(ctx, store(2, g), SyncOptions{CaughtUp: func() { calls++ }}); err != nil || calls != 0 {
		t.Errorf("a sync with only new sessions: caught up %d times (%v), want none", calls, err)
	}
	gate = make(chan struct{})
	g.gate, g.slow = gate, map[string]bool{"0": true}
	calls = 0
	if within(g, 3, func() { calls++; close(gate) }); calls != 1 {
		t.Errorf("a sync with only sessions to read again: caught up %d times, want once", calls)
	}

	// What is read again may be read first, beside a new session that takes longer: the
	// sync is caught up when the new one is in, and not when as many have been read
	gate = make(chan struct{})
	g.keys, g.gate, g.slow = append(g.keys, "7"), gate, map[string]bool{"7": true}
	time.AfterFunc(300*time.Millisecond, func() { close(gate) })
	newIn := -1
	within(g, 4, func() { newIn = count("key = '7'") })
	if newIn != 1 {
		t.Errorf("caught up with the new session in the index %d times, want once", newIn)
	}
}

// An index at a path of its own is the one an older asq used, from before readings: it
// gains them in place, however many processes open it at once, and every session in it is
// read again, once.
func TestAnIndexFromBeforeReadingsGainsThem(t *testing.T) {
	const column = "  reads      INTEGER NOT NULL DEFAULT 0,   -- the adapter's reading it was read with (sources.Spec.Reads)\n"
	if !strings.Contains(schema, column) {
		t.Fatal("the reads column is not where the test looks for it")
	}
	older := func() string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "index.db")
		old, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer old.Close()
		// as the older asq left it: in WAL mode, as every asq opens its index
		if _, err := old.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(strings.Replace(schema, column, "", 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO sessions (source, agent, key, stamp, id) VALUES ('claude', 'claude', '0', '1', 's0')`); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// Opened by many at once, all let go together, time and again
	var path string
	for round := range 4 {
		path = older()
		start := make(chan struct{})
		errs := make(chan error, 16)
		for range cap(errs) {
			go func() {
				<-start
				ix, err := Open(path)
				if err == nil {
					ix.Close()
				}
				errs <- err
			}()
		}
		close(start)
		for range cap(errs) {
			if err := <-errs; err != nil {
				t.Errorf("round %d, opened at once with others: %v", round, err)
			}
		}
	}

	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	sync := func() int {
		t.Helper()
		res, err := ix.Sync(context.Background(), []Store{{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: 1}, Source: many{n: 4}}}, SyncOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Read
	}
	if n := sync(); n != 4 {
		t.Errorf("the first sync read %d, want 4: the one from before, again, and three new", n)
	}
	if n := sync(); n != 0 {
		t.Errorf("and then %d", n)
	}
}
