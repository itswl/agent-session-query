package index

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
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

// A reader busy with the old file while the process follows the new one is let go of once
// it is done, rather than kept to answer the next question from the old file.
func TestAReaderBusyWhileTheFileIsFollowedIsLetGo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not remove a file another program holds open")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	server := open(t, path)
	syncMany(t, server, 3)
	busy, err := server.ro.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := busy.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("the busy reader reads %d sessions, %v", n, err)
	}

	// asq index rebuild, and a session more
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	syncMany(t, open(t, path), 4)
	if res := syncMany(t, server, 4); !strings.Contains(res.Reopened, "replaced") {
		t.Errorf("reopened = %q", res.Reopened)
	}
	busy.Close() // done, and back to the pool
	if n := sessions(t, server); n != 4 {
		t.Errorf("the server reads %d sessions after following the file; want 4", n)
	}
}

// Damage a process met in its file, when another asq has set that file aside and made a new
// one already, was the old file's: the process follows the new one, and does not set it
// aside in turn.
func TestDamageOfAFileAlreadyReplacedIsNotSetAsideAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not move a file another program holds open")
	}
	path := filepath.Join(t.TempDir(), "index.db")
	server := open(t, path)
	syncMany(t, server, 3)

	// The CLI found the file damaged, set it aside and made it anew
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	cli := open(t, path)
	syncMany(t, cli, 3)

	// The server's next question read the old file, and met the damage
	bad := filepath.Join(t.TempDir(), "bad.db")
	os.WriteFile(bad, []byte(strings.Repeat("x", 4096)), 0o600)
	db, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(bad))
	_, damage := db.Exec(`SELECT * FROM sqlite_master`)
	db.Close()
	if !server.Damage(damage) {
		t.Fatalf("%v is not taken for damage", damage)
	}
	res := syncMany(t, server, 3)
	if !strings.Contains(res.Reopened, "replaced") {
		t.Errorf("reopened = %q; want the new file followed", res.Reopened)
	}
	if _, err := os.Stat(path + ".damaged"); !os.IsNotExist(err) {
		t.Errorf("the file the CLI made was set aside as damaged: %v", err)
	}
	if n := sessions(t, cli); n != 3 {
		t.Errorf("the CLI's index holds %d sessions; want 3", n)
	}
}

// bulky is a store of n sessions, each holding size bytes that do not compress.
type bulky struct{ n, size int }

func (bulky) Location() string                              { return "test" }
func (bulky) Exists() bool                                  { return true }
func (b bulky) Scan(context.Context) ([]sources.Ref, error) { return many{n: b.n}.Scan(nil) }
func (b bulky) Read(_ context.Context, ref sources.Ref, w *transcript.Writer) error {
	w.S.ID = "s" + ref.Key
	raw := make([]byte, b.size/2)
	rand.Read(raw)
	text := hex.EncodeToString(raw)
	w.Add(transcript.Message{Role: transcript.User, At: time.Now(), Blocks: []transcript.Block{transcript.TextOf("hello")}})
	w.Add(transcript.Message{Role: transcript.Assistant, At: time.Now(), Blocks: []transcript.Block{transcript.TextOf(text)}})
	return nil
}

// What sessions leaving the index free is given back to the file system, once it is worth
// it — by a file made to give it back. One made otherwise keeps it, and says so.
func TestSpaceIsGivenBack(t *testing.T) {
	defer func(was int64) { giveFrom = was }(giveFrom)
	giveFrom = 256 << 10
	ctx := context.Background()
	fill := func(ix *Index, n int) {
		t.Helper()
		if _, err := ix.Sync(ctx, []Store{{Name: "claude", Spec: sources.Spec{Name: "claude"}, Source: bulky{n: n, size: 64 << 10}}}, SyncOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	ix := open(t, filepath.Join(t.TempDir(), "index.db"))
	fill(ix, 40)
	full, err := ix.Space(ctx)
	if err != nil || !full.GivesBack || full.Bytes < 40*(32<<10) {
		t.Fatalf("a new index = %+v, %v", full, err)
	}
	fill(ix, 8) // the store's cleanup took 32 of them
	after, err := ix.Space(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Free >= giveFrom || after.Bytes > full.Bytes/2 || after.Keeps() {
		t.Errorf("after most sessions left: %+v; it was %+v", after, full)
	}

	// A file made without it: the space stays, free, for the next sessions to fill
	old := filepath.Join(t.TempDir(), "index.db")
	db, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(old)+"?_pragma=journal_mode(WAL)")
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ix = open(t, old)
	fill(ix, 40)
	fill(ix, 8)
	kept, err := ix.Space(ctx)
	if err != nil || kept.GivesBack || kept.Free < giveFrom || !kept.Keeps() {
		t.Errorf("a file made without auto_vacuum, after most sessions left: %+v, %v", kept, err)
	}
	totals, err := ix.Totals(ctx, []string{"claude"})
	if err != nil || totals.Free != kept.Free || totals.Bytes != kept.Bytes {
		t.Errorf("totals = %+v, %v; want the space %+v", totals, err, kept)
	}
}
