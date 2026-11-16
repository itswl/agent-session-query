package app

import (
	"context"
	"path/filepath"
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
		a.running, a.last, a.began = reading, last, began
		a.mu.Unlock()
		return reading
	}
	over := func(reading chan struct{}) {
		close(reading)
		a.mu.Lock()
		a.running = nil
		a.mu.Unlock()
	}
	synced := func() time.Time { return a.Index.State().At }

	// Never read: the first read is waited for, as long as the caller will
	first := underWay(time.Time{}, time.Now())
	if start := time.Now(); a.Fresh(ctx, maxAge, 80*time.Millisecond) || time.Since(start) < 80*time.Millisecond {
		t.Error("an index that was never read answered as if it had been")
	}
	over(first)

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
	// And then it is fresh, with nothing to do
	read = synced()
	if !a.Fresh(ctx, maxAge, 5*time.Second) || !synced().Equal(read) {
		t.Error("an index read a moment ago was read again")
	}
}
