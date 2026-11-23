package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources"
)

// sessions is how many sessions the index holds, as a reader sees them.
func sessions(t *testing.T, ix *Index) int {
	t.Helper()
	var n int
	if err := ix.ro.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func syncMany(t *testing.T, ix *Index, n int) SyncResult {
	t.Helper()
	res, err := ix.Sync(context.Background(), []Store{{Name: "claude", Spec: sources.Spec{Name: "claude"}, Source: many{n: n}}}, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func open(t *testing.T, path string) *Index {
	t.Helper()
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

// An index file that is not a database, or whose pages do not hold together, is set aside
// and made anew: the index is a cache, and what it held is read again from the stores.
func TestADamagedIndexIsSetAsideAndMadeAnew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	garbage := []byte(strings.Repeat("not a database, whatever its name says. ", 200))
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	ix := open(t, path)
	if ix.Repaired == nil || !strings.Contains(ix.Repaired.Problem, "not a database") || ix.Repaired.Aside != path+".damaged" {
		t.Fatalf("repaired = %+v", ix.Repaired)
	}
	if kept, err := os.ReadFile(path + ".damaged"); err != nil || string(kept) != string(garbage) {
		t.Errorf("the damaged file was not kept beside the new one: %v", err)
	}
	syncMany(t, ix, 3)
	if n := sessions(t, ix); n != 3 {
		t.Errorf("the new index holds %d sessions; want 3", n)
	}
	if ck, err := Check(context.Background(), path); err != nil || !ck.Found || ck.Problem != "" {
		t.Errorf("the new index checks as %+v, %v", ck, err)
	}
	ix.Close()
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".damaged"); !os.IsNotExist(err) {
		t.Errorf("asq index rm leaves the damaged file: %v", err)
	}
}
