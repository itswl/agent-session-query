package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/index"
)

// A question is held up for a read of the stores when nobody has read them for a while —
// and not for a read that is following them anyway. On a machine with an agent at work
// some transcript is always being read again; a list of sessions does not wait for it.
func TestWhenAQuestionWaitsForTheStores(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(Config{
		IndexPath: filepath.Join(dir, "index.db"),
		Stores:    catalog.Config{Only: []string{"claude"}, Paths: []catalog.Path{{Agent: "claude", Dir: filepath.Join(dir, "claude")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	const maxAge = 2 * time.Second

	// A read is under way, in the state the test puts it in; it ends when the test says
	underWay := func(last, began time.Time) chan struct{} {
		reading := make(chan struct{})
		a.mu.Lock()
		a.running, a.caught, a.last, a.began = reading, reading, last, began
		a.mu.Unlock()
		return reading
	}
	over := func(reading chan struct{}) {
		close(reading)
		a.mu.Lock()
		a.running, a.caught = nil, nil
		a.mu.Unlock()
	}
	synced := func() time.Time { return a.Index.State().At }

	// Never read: the first read is waited for, as long as the caller will
	first := underWay(time.Time{}, time.Now())
	if start := time.Now(); a.Fresh(ctx, maxAge, 80*time.Millisecond) || time.Since(start) < 80*time.Millisecond {
		t.Error("an index that was never read answered as if it had been")
	}
	over(first)

	// The first read has read what is new, and goes on to read the rest again for an
	// adapter that reads it differently now: that is not waited for
	reading, caught := make(chan struct{}), make(chan struct{})
	close(caught)
	a.mu.Lock()
	a.running, a.caught, a.last, a.began = reading, caught, time.Time{}, time.Now()
	a.mu.Unlock()
	a.mu.Lock()
	a.caughtAt = time.Now()
	a.mu.Unlock()
	if start := time.Now(); !a.Fresh(ctx, maxAge, 5*time.Second) || time.Since(start) > time.Second {
		t.Errorf("a question waited %v for a history read again only for a new reading", time.Since(start))
	}
	// and while that goes on for longer than maxAge, nothing new is read: not fresh, and
	// not waited for either
	a.mu.Lock()
	a.caughtAt = time.Now().Add(-2 * maxAge)
	a.mu.Unlock()
	if start := time.Now(); a.Fresh(ctx, maxAge, 5*time.Second) || time.Since(start) > time.Second {
		t.Errorf("a question %v into a long reading again was answered as fresh, or waited for it", time.Since(start))
	}
	if b := a.Behind(); b < 2*maxAge || b > 2*maxAge+time.Minute {
		t.Errorf("behind by %v, %v after it caught up", b, 2*maxAge)
	}
	over(reading)
	if b := a.Behind(); b != 0 {
		t.Errorf("behind by %v with no sync under way", b)
	}

	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	read := synced()

	// Followed: the read under way began while the one before it was fresh. It may take
	// as long as it likes; the question is answered now
	now := time.Now()
	following := underWay(now.Add(-10*time.Second), now.Add(-9*time.Second))
	if start := time.Now(); !a.Fresh(ctx, maxAge, 5*time.Second) || time.Since(start) > time.Second {
		t.Errorf("a question waited %v for a read that was only following the stores", time.Since(start))
	}
	over(following)

	// Not followed: the read under way is the first in a long while. The question waits
	// for it — for that one, and not for one of its own queued behind it
	late := underWay(now.Add(-time.Minute), now)
	if start := time.Now(); a.Fresh(ctx, maxAge, 80*time.Millisecond) || time.Since(start) < 80*time.Millisecond {
		t.Error("a stale index answered as fresh while it was still being read")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		over(late)
	}()
	if start := time.Now(); !a.Fresh(ctx, maxAge, 5*time.Second) || time.Since(start) > 2*time.Second {
		t.Error("the read that was under way ended, and the question went on waiting")
	}
	time.Sleep(50 * time.Millisecond)
	if !synced().Equal(read) {
		t.Error("a second read was queued behind the one under way")
	}

	// Nobody reading, and nobody has for a while: read now, and wait for that
	a.mu.Lock()
	a.last = time.Now().Add(-time.Minute)
	a.mu.Unlock()
	if !a.Fresh(ctx, maxAge, 5*time.Second) || !synced().After(read) {
		t.Error("a stale index was not read again when asked")
	}
	// Questions that arrive together share one read: the first claims it before it lets
	// go, and the rest find it under way rather than each starting its own
	a.mu.Lock()
	a.last = time.Now().Add(-time.Minute)
	a.mu.Unlock()
	before := a.syncs.Load()
	gate := make(chan struct{})
	var asked sync.WaitGroup
	for range 32 {
		asked.Add(1)
		go func() {
			defer asked.Done()
			<-gate
			if !a.Fresh(ctx, maxAge, 5*time.Second) {
				t.Error("a question was not answered fresh")
			}
		}()
	}
	close(gate)
	asked.Wait()
	if n := a.syncs.Load() - before; n != 1 {
		t.Errorf("questions arriving together on a stale index made %d reads, not one", n)
	}

	// And then it is fresh, with nothing to do
	read = synced()
	if !a.Fresh(ctx, maxAge, 5*time.Second) || !synced().Equal(read) {
		t.Error("an index read a moment ago was read again")
	}
}

// A sync that has caught up with the stores, and goes on to read the history again for a
// new reading, lets the questions waiting for it go then, and not when it ends.
func TestCaughtUpIsWhenTheQuestionsGo(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	line := `{"type":"user","uuid":"u1","timestamp":"` + at + `","sessionId":"s1","cwd":"/w","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.MkdirAll(filepath.Join(dir, "claude", "-w"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude", "-w", "s1.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Open(Config{
		IndexPath: filepath.Join(dir, "index.db"),
		Stores:    catalog.Config{Only: []string{"claude"}, Paths: []catalog.Path{{Agent: "claude", Dir: filepath.Join(dir, "claude")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	if res, err := a.Sync(ctx, index.SyncOptions{}); err != nil || res.Read != 1 {
		t.Fatalf("first read: %+v, %v", res, err)
	}
	// As an older reading left it
	db, err := sql.Open("sqlite", a.Index.Path())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE sessions SET reads = 0`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	let, held := false, false
	res, err := a.Sync(ctx, index.SyncOptions{CaughtUp: func() {
		a.mu.Lock()
		caught, running, at := a.caught, a.running, a.caughtAt
		a.mu.Unlock()
		if at.IsZero() {
			held = false // the time it caught up is what tells a question later on
			return
		}
		select {
		case <-caught:
			let = true
		default:
		}
		select {
		case <-running:
		default:
			held = true
		}
	}})
	if err != nil || res.Read != 1 {
		t.Fatalf("reading again: %+v, %v", res, err)
	}
	if !let || !held {
		t.Errorf("when the sync caught up: questions let go %v, the sync still under way %v; want both", let, held)
	}
}
