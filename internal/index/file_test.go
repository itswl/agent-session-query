package index

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// A file damaged deeper in — a page of a table, which opening it does not read — is found by
// the first sync that reads that table, and the sync sets it aside and reads the stores
// into a new one at once. Doctor's look finds it too.
func TestASyncThatMeetsDamageReadsAnew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	syncMany(t, ix, 3)
	var root, size int64
	if err := ix.ro.QueryRow(`SELECT rootpage, page_size FROM sqlite_master, pragma_page_size() WHERE name = 'sessions'`).Scan(&root, &size); err != nil {
		t.Fatal(err)
	}
	ix.Close() // everything in the file itself, none of it in the log
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(strings.Repeat("\xff", int(size))), (root-1)*size); err != nil {
		t.Fatal(err)
	}
	f.Close()

	ck, err := Check(context.Background(), path)
	if err != nil || !ck.Found || ck.Problem == "" {
		t.Errorf("doctor's look at a damaged file: %+v, %v", ck, err)
	}

	ix = open(t, path)
	if ix.Repaired != nil {
		t.Fatalf("opening read the damaged page after all (%+v): the sync is not what this tests", ix.Repaired)
	}
	res := syncMany(t, ix, 3)
	if !strings.Contains(res.Reopened, "damaged") || !strings.Contains(res.Reopened, path+".damaged") {
		t.Errorf("reopened = %q", res.Reopened)
	}
	if n := sessions(t, ix); n != 3 {
		t.Errorf("after the damage, the index holds %d sessions; want 3", n)
	}
}

// A damaged file met by a question rather than a sync — a search reading a page no sync
// reads — is set aside by the next sync.
func TestDamageMetElsewhereIsRepairedByTheNextSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	ix := open(t, path)
	syncMany(t, ix, 3)

	bad := filepath.Join(dir, "bad.db")
	os.WriteFile(bad, []byte(strings.Repeat("x", 4096)), 0o600)
	db, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(bad))
	_, damage := db.Exec(`SELECT * FROM sqlite_master`)
	db.Close()
	if !ix.Damage(damage) {
		t.Fatalf("%v is not taken for damage", damage)
	}
	if ix.Damage(fmt.Errorf("no such table: nothing")) {
		t.Error("any error is taken for damage")
	}
	res := syncMany(t, ix, 3)
	if !strings.Contains(res.Reopened, "set aside") {
		t.Errorf("reopened = %q", res.Reopened)
	}
	if _, err := os.Stat(path + ".damaged"); err != nil {
		t.Error(err)
	}
	if n := sessions(t, ix); n != 3 {
		t.Errorf("the new index holds %d sessions; want 3", n)
	}
	if res := syncMany(t, ix, 3); res.Reopened != "" {
		t.Errorf("a second sync opened the file anew too: %q", res.Reopened)
	}
}

// A server keeps its index open while asq index rebuild removes the file and makes it anew.
// It follows the new file: what it reads after is what everyone reads, and the file it had
// is let go of — without taking the new file's log with it.
func TestAProcessFollowsTheFileAtItsPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not remove a file another program holds open: asq index rebuild fails there instead")
	}
	path := filepath.Join(t.TempDir(), "index.db")
	server := open(t, path)
	syncMany(t, server, 3)

	// asq index rebuild
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	cli := open(t, path)
	syncMany(t, cli, 3)

	// A session is written; the server reads it, into the file at the path
	res := syncMany(t, server, 4)
	if !strings.Contains(res.Reopened, "replaced") {
		t.Errorf("reopened = %q", res.Reopened)
	}
	if n := sessions(t, cli); n != 4 {
		t.Errorf("the rebuilt index holds %d sessions; want the 4 the server has read", n)
	}
	if n := sessions(t, server); n != 4 {
		t.Errorf("the server reads %d sessions; want 4", n)
	}

	// What the other asq writes now is in the file for anyone who opens it: letting go of
	// the old file did not delete the new one's log
	syncMany(t, cli, 5)
	if n := sessions(t, open(t, path)); n != 5 {
		t.Errorf("a process opening the index now reads %d sessions; want 5", n)
	}

	// asq index rm, with nothing to make it anew: the server makes it, from the stores
	server.Close()
	cli.Close()
	server = open(t, path)
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	if res := syncMany(t, server, 5); !strings.Contains(res.Reopened, "removed") {
		t.Errorf("reopened = %q", res.Reopened)
	}
	if n := sessions(t, open(t, path)); n != 5 {
		t.Errorf("after rm, the index at the path holds %d sessions; want 5", n)
	}
}
