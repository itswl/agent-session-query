package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources/claude"
)

// A session another store of its CLI holds, written after the one the CLI's own store
// holds, is folded into that one as it is written: before the sync is done and links the
// sessions, a first read with a backup beside the store does not count each session twice.
func TestACopyIsFoldedAsItIsWritten(t *testing.T) {
	const id = "f01d0000-0000-4000-8000-000000000001"
	ctx := context.Background()
	var stores []Store
	for _, name := range []string{"claude", "claude@box2"} {
		root := t.TempDir()
		path := filepath.Join(root, "-work-fold", id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"type":"user","uuid":"u1","timestamp":"2026-11-20T10:00:00Z","sessionId":"`+id+`","cwd":"/work/fold","message":{"role":"user","content":"fold me"}}`+"\n"+
			`{"type":"assistant","uuid":"a1","timestamp":"2026-11-20T10:00:05Z","sessionId":"`+id+`","message":{"id":"m1","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"folded"}],"stop_reason":"end_turn"}}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		stores = append(stores, Store{Name: name, Spec: claude.Spec, Source: claude.Spec.Open(root)})
	}
	own, other := stores[0], stores[1]

	ix := openTemp(t)
	if _, err := ix.Sync(ctx, []Store{own}, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	refs, err := other.Source.Scan(ctx)
	if err != nil || len(refs) != 1 {
		t.Fatalf("the copy's store = %v, %v", refs, err)
	}
	for _, w := range ix.writeAll(ctx, []*reading{ix.read(ctx, other, refs[0], 0)}) {
		if w.err != nil {
			t.Fatal(w.err)
		}
	}
	var copyOf, ownSID int64
	if err := ix.rw.QueryRow(`SELECT c.copy_of, o.sid FROM sessions c JOIN sessions o ON o.source = 'claude' WHERE c.source = 'claude@box2'`).Scan(&copyOf, &ownSID); err != nil {
		t.Fatal(err)
	}
	if copyOf != ownSID {
		t.Errorf("the copy, written before any link, is a copy of %d; want %d, the session the CLI's own store holds", copyOf, ownSID)
	}
	_, n, err := ix.Sessions(ctx, Filter{Stores: []string{"claude", "claude@box2"}}, Page{})
	if err != nil || n != 1 {
		t.Errorf("listed before any link = %d, %v; want the one session", n, err)
	}

}
