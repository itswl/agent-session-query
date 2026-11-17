package index

import (
	"context"
	"path/filepath"
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
	path := filepath.Join(t.TempDir(), "index.db")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ix.Close() }()
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
	steps("a new index", []step{
		{0, 4, "the first read"},
		{0, 0, "nothing moved"},
		{1, 4, "the first counted reading: every session read again"},
		{1, 0, "the same reading: nothing"},
		{2, 4, "a new reading: every session again"},
		{2, 0, "and once only"},
		{1, 0, "an older binary leaves what a newer reading stored"},
		{2, 0, "and the newer one finds it as it left it"},
	})

	// An index from before readings were kept has none: it gains the column, every
	// session at reading 0, and keeps its sessions
	ix.Close()
	{
		raw, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.rw.Exec(`ALTER TABLE sessions DROP COLUMN reads`); err != nil {
			t.Fatal(err)
		}
		raw.Close()
	}
	if ix, err = Open(path); err != nil {
		t.Fatalf("an index without readings: %v", err)
	}
	var sessions, unread int
	if err := ix.rw.QueryRow(`SELECT count(*), count(*) FILTER (WHERE reads = 0) FROM sessions`).Scan(&sessions, &unread); err != nil {
		t.Fatal(err)
	}
	if sessions != 4 || unread != 4 {
		t.Fatalf("after the column came: %d sessions, %d at reading 0; want 4 and 4", sessions, unread)
	}
	steps("an index from before readings", []step{
		{2, 4, "read with no counted reading: every session again"},
		{2, 0, "and once only"},
	})
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
