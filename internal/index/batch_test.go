package index

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// talk is a store of n sessions of turns turns each, every turn asking a word of its own:
// w<session>x<turn>. A stamp other than "1" says something else in each.
type talk struct {
	n, turns int
	stamp    string
}

func (talk) Location() string { return "test" }
func (talk) Exists() bool     { return true }
func (s talk) Scan(context.Context) ([]sources.Ref, error) {
	refs := make([]sources.Ref, s.n)
	for i := range refs {
		stamp := s.stamp
		if stamp == "" {
			stamp = "1"
		}
		refs[i] = sources.Ref{Key: fmt.Sprint(i), Stamp: stamp, ModTime: time.Now().Add(-time.Duration(i) * time.Minute)}
	}
	return refs, nil
}
func (s talk) Read(_ context.Context, ref sources.Ref, w *transcript.Writer) error {
	w.S.ID = "s" + ref.Key
	at := time.Now().Add(-time.Hour)
	for t := 1; t <= s.turns; t++ {
		ask := fmt.Sprintf("w%sx%d", ref.Key, t)
		if s.stamp != "" && s.stamp != "1" {
			ask = "v" + s.stamp + ask
		}
		w.Add(transcript.Message{Role: transcript.User, At: at, Blocks: []transcript.Block{transcript.TextOf(ask)}})
		w.Add(transcript.Message{Role: transcript.Assistant, At: at.Add(time.Second), Blocks: []transcript.Block{transcript.TextOf("done")}})
		at = at.Add(time.Minute)
	}
	return nil
}

// The sessions a sync reads together are written in one transaction, and their words after
// everything else in it: the words table writes what it has gathered as a segment of its own
// before any statement that may be undone alone, and a turn's words written between its
// rows were a segment each.
func TestASyncWritesTheWordsOfWhatItReadAsOneSegment(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	// Segments are left as they are written, to be counted
	if _, err := ix.rw.Exec(`INSERT INTO words (words, rank) VALUES ('automerge', 0)`); err != nil {
		t.Fatal(err)
	}
	segments := func() int {
		t.Helper()
		var n int
		// Each segment is a leaf at least, beside the table's structure and its averages, and
		// what was deleted from a segment is a page beside it
		if err := ix.rw.QueryRow(`SELECT count(*) - 2 FROM words_data`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	store := []Store{{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: 1}, Source: talk{n: 4, turns: 5}}}
	if _, err := ix.Sync(ctx, store, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := segments(); n > 1 {
		t.Errorf("4 sessions of 5 turns read at once were written as %d segments of the words table, not one", n)
	}
	// Read again, saying something else: each turn's words go, and the new ones come
	store[0].Source = talk{n: 4, turns: 5, stamp: "2"}
	if _, err := ix.Sync(ctx, store, SyncOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	// One segment more, and what the first has had deleted from it
	if n := segments(); n > 3 {
		t.Errorf("the same sessions read again were written as %d more pages of the words table, not a segment and its deletions", n-1)
	}
	for _, c := range []struct {
		word string
		want bool
	}{{`"w0x1"`, false}, {`"w3x5"`, false}, {`"v2w0x1"`, true}, {`"v2w3x5"`, true}} {
		if got, err := ix.holds(ctx, c.word, Filter{}); err != nil || got != c.want {
			t.Errorf("a turn holds %s: %v (%v), want %v", c.word, got, err, c.want)
		}
	}
}

// A session that cannot be written does not keep out the others read with it: they are
// written each in a transaction of its own.
func TestASessionThatCannotBeWrittenLeavesTheOthersWritten(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	st := Store{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: 1}, Source: talk{n: 3, turns: 2}}
	refs, _ := st.Source.Scan(ctx)
	var rds []*reading
	for _, ref := range refs {
		rds = append(rds, ix.read(ctx, st, ref))
	}
	// Two turns of one number cannot both be stored
	rds[1].turns = append(rds[1].turns, rds[1].turns[0])
	got := ix.writeAll(ctx, rds)
	for i, w := range got {
		if (w.err != nil) != (i == 1) {
			t.Errorf("session %d: written %v, error %v", i, w.changed, w.err)
		}
	}
	for _, c := range []struct {
		word string
		want bool
	}{{`"w0x1"`, true}, {`"w1x1"`, false}, {`"w2x2"`, true}} {
		if held, err := ix.holds(ctx, c.word, Filter{}); err != nil || held != c.want {
			t.Errorf("a turn holds %s: %v (%v), want %v", c.word, held, err, c.want)
		}
	}
}
