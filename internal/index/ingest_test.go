package index

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// growing is a store of one session that grows a turn at a time: each turn an ask, a
// command whose output is long enough to go to the bodies table, and an answer. recorded,
// when set, is what the CLI keeps as the session's total beyond its responses.
type growing struct {
	turns    int
	recorded int64
}

func (growing) Location() string { return "test" }
func (growing) Exists() bool     { return true }
func (g growing) Scan(context.Context) ([]sources.Ref, error) {
	return []sources.Ref{{Key: "s", Stamp: fmt.Sprint(g.turns, g.recorded), ModTime: time.Now()}}, nil
}
func (g growing) Read(_ context.Context, _ sources.Ref, w *transcript.Writer) error {
	w.S.ID, w.S.Title = "grown", "A session that grows"
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for i := 1; i <= g.turns; i++ {
		at = at.Add(time.Minute)
		id := fmt.Sprint(i)
		w.Add(transcript.Message{Role: transcript.User, At: at, Blocks: []transcript.Block{transcript.TextOf("step " + id + " of the migration")}})
		w.Add(transcript.Message{Role: transcript.Assistant, At: at.Add(time.Second), Response: "r" + id + "a", Model: "m",
			Usage:  &model.Usage{Input: 10, Output: 5},
			Blocks: []transcript.Block{transcript.CallOf("c"+id, "Bash", map[string]any{"command": "make step" + id})}})
		w.Add(transcript.Message{Role: transcript.ToolRole, At: at.Add(2 * time.Second), Blocks: []transcript.Block{
			transcript.ResultOf("c"+id, "Bash", strings.Repeat("compiling package "+id+"\n", 400), transcript.Outcome{Status: model.StatusOK})}})
		w.Add(transcript.Message{Role: transcript.Assistant, At: at.Add(3 * time.Second), Response: "r" + id + "b", Model: "m",
			Usage:  &model.Usage{Input: 10, Output: 5},
			Blocks: []transcript.Block{transcript.TextOf("step " + id + " is done")}})
	}
	if g.recorded > 0 {
		w.S.Recorded = &model.Recorded{Usage: &model.Usage{Input: g.recorded, Output: 10 * int64(g.turns)}}
	}
	return nil
}

func growingStore(g growing) []Store {
	return []Store{{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: 1}, Source: g}}
}

func openTemp(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func syncOf(t *testing.T, ix *Index, g growing) {
	t.Helper()
	if _, err := ix.Sync(context.Background(), growingStore(g), SyncOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
}

// rowsOf is everything the index holds of its sessions' turns, for two indexes to be told
// apart by: each turn's row, its long text, its usage and its words.
func rowsOf(t *testing.T, ix *Index) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT idx, started_at, ended_at, items, tools, failures, hex(hash), head, hex(body), hex(src) FROM turns ORDER BY idx`,
		`SELECT idx, seq, part, hex(data) FROM bodies ORDER BY seq, part`,
		`SELECT idx, model, calls, tok_in, tok_out FROM usage ORDER BY idx, model`,
		`SELECT sid, idx FROM words_stale ORDER BY sid, idx`,
		`SELECT turns, items, tools, calls, tok_in, tok_out, last_ask, answer FROM sessions`,
	} {
		rows, err := ix.rw.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&b, vals...)
		}
		rows.Close()
	}
	for _, word := range []string{"compiling", "migration", "done", "make"} {
		res, err := ix.Search(context.Background(), ParseQuery(word), Filter{}, SearchOptions{Limit: 100, PerSession: -1})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(&b, word, res.Total)
	}
	return b.String()
}

func sidOf(t *testing.T, ix *Index) int64 {
	t.Helper()
	var sid int64
	if err := ix.rw.QueryRow(`SELECT sid FROM sessions`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	return sid
}

// A session read again because it grew makes the rows of the turns that are new or changed,
// and leaves the others as the index holds them: their words and their compressed bodies,
// most of what a read cost, are not made again. What it leaves is what a read from nothing
// would have made.
func TestAGrownSessionMakesOnlyWhatChanged(t *testing.T) {
	ix := openTemp(t)
	syncOf(t, ix, growing{turns: 8})

	grown := growing{turns: 9}
	st := growingStore(grown)[0]
	refs, _ := grown.Scan(context.Background())
	rd := ix.read(context.Background(), st, refs[0], sidOf(t, ix))
	var made []int
	for _, row := range rd.turns {
		if !row.same {
			made = append(made, row.idx)
		}
	}
	// The first holds the title, and the ninth is new and the last
	if fmt.Sprint(made) != "[1 9]" {
		t.Errorf("turns made again = %v, want [1 9]: the others did not change", made)
	}
	if rd.lastAsk != "step 9 of the migration" || rd.answer != "step 9 is done" {
		t.Errorf("the session's last ask %q and answer %q", rd.lastAsk, rd.answer)
	}
	if _, _, err := ix.write(context.Background(), rd); err != nil {
		t.Fatal(err)
	}

	fresh := openTemp(t)
	syncOf(t, fresh, grown)
	if got, want := rowsOf(t, ix), rowsOf(t, fresh); got != want {
		t.Errorf("the grown index differs from one read from nothing:\n%s\nwant:\n%s", got, want)
	}

	// A rebuild trusts nothing it holds
	rd = ix.read(context.Background(), st, refs[0], 0)
	for _, row := range rd.turns {
		if row.same {
			t.Fatalf("a read with no row to keep kept turn %d", row.idx)
		}
	}
}

// What the CLI recorded for the session beyond its responses is entered against the last
// turn. When a turn is added, that turn is the last no more: it is made again, without
// it, and the totals are counted once.
func TestRecordedTotalsMoveToTheNewLastTurn(t *testing.T) {
	ix := openTemp(t)
	syncOf(t, ix, growing{turns: 4, recorded: 1000})
	syncOf(t, ix, growing{turns: 5, recorded: 2000})
	var in, out int64
	if err := ix.rw.QueryRow(`SELECT sum(tok_in), sum(tok_out) FROM usage`).Scan(&in, &out); err != nil {
		t.Fatal(err)
	}
	if in != 2000 || out != 50 {
		t.Errorf("usage = %d in, %d out; want the recorded 2000 and 50, each counted once", in, out)
	}
	fresh := openTemp(t)
	syncOf(t, fresh, growing{turns: 5, recorded: 2000})
	if got, want := rowsOf(t, ix), rowsOf(t, fresh); got != want {
		t.Errorf("after the totals moved:\n%s\nwant:\n%s", got, want)
	}
}

// Rows made before turns were named — by an older asq — are named at the next read, and
// left as they are from then on.
func TestRowsMadeBeforeSrcAreNamed(t *testing.T) {
	ix := openTemp(t)
	syncOf(t, ix, growing{turns: 5})
	if _, err := ix.rw.Exec(`UPDATE turns SET src = x''`); err != nil {
		t.Fatal(err)
	}
	st := growingStore(growing{turns: 6})[0]
	refs, _ := growing{turns: 6}.Scan(context.Background())
	rd := ix.read(context.Background(), st, refs[0], sidOf(t, ix))
	for _, row := range rd.turns {
		if row.same {
			t.Fatalf("turn %d was kept, though nothing said what its rows were made from", row.idx)
		}
	}
	if _, _, err := ix.write(context.Background(), rd); err != nil {
		t.Fatal(err)
	}
	var unnamed int
	if err := ix.rw.QueryRow(`SELECT count(*) FROM turns WHERE length(src) = 0`).Scan(&unnamed); err != nil || unnamed != 0 {
		t.Errorf("%d turns are still unnamed (%v)", unnamed, err)
	}
	syncOf(t, ix, growing{turns: 7})
	fresh := openTemp(t)
	syncOf(t, fresh, growing{turns: 7})
	if got, want := rowsOf(t, ix), rowsOf(t, fresh); got != want {
		t.Errorf("after naming them:\n%s\nwant:\n%s", got, want)
	}
}

// Between a read and its write another asq can write the session. A turn the read kept
// that the other changed is left as the other wrote it, and the session is read again,
// whole, at the next sync. A turn whose words the other made stale is left for the next
// sync to write.
func TestATurnChangedUnderAReadIsReadAgain(t *testing.T) {
	ix := openTemp(t)
	syncOf(t, ix, growing{turns: 6})
	sid := sidOf(t, ix)
	st := growingStore(growing{turns: 7})[0]
	refs, _ := growing{turns: 7}.Scan(context.Background())
	rd := ix.read(context.Background(), st, refs[0], sid)

	// Turn 3 written anew by another asq; turn 4's words made stale by one
	for _, q := range []string{
		`UPDATE turns SET hash = x'00', src = x'' WHERE idx = 3`,
		`INSERT INTO words_stale (sid, idx) SELECT sid, 4 FROM sessions`,
	} {
		if _, err := ix.rw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ix.write(context.Background(), rd); err != nil {
		t.Fatal(err)
	}
	var stamp string
	var hash []byte
	var stale int
	ix.rw.QueryRow(`SELECT stamp FROM sessions`).Scan(&stamp)
	ix.rw.QueryRow(`SELECT hash FROM turns WHERE idx = 3`).Scan(&hash)
	ix.rw.QueryRow(`SELECT count(*) FROM words_stale WHERE idx = 4`).Scan(&stale)
	if stamp != "" || string(hash) != "\x00" {
		t.Errorf("stamp %q, turn 3 %x: the turn is to stay as the other wrote it, and the session to be read again", stamp, hash)
	}
	if stale != 1 {
		t.Errorf("turn 4's words, made stale under the read, are no longer to be written: %d", stale)
	}

	res, err := ix.Sync(context.Background(), growingStore(growing{turns: 7}), SyncOptions{Force: true})
	if err != nil || res.Read != 1 {
		t.Fatalf("the next sync = %+v, %v", res, err)
	}
	fresh := openTemp(t)
	syncOf(t, fresh, growing{turns: 7})
	if got, want := rowsOf(t, ix), rowsOf(t, fresh); got != want {
		t.Errorf("after reading it again:\n%s\nwant:\n%s", got, want)
	}
}

// An index whose turns an older asq made has no src column, and gains it when opened.
func TestAnIndexFromBeforeSrcGainsIt(t *testing.T) {
	column := "  src        BLOB NOT NULL DEFAULT x'',\n"
	if !strings.Contains(schema, column) {
		t.Fatal("the column is not where the test looks for it")
	}
	path := filepath.Join(t.TempDir(), "index.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(strings.Replace(schema, column, "", 1)); err != nil {
		t.Fatal(err)
	}
	old.Close()
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	syncOf(t, ix, growing{turns: 3})
	syncOf(t, ix, growing{turns: 4})
	var named int
	if err := ix.rw.QueryRow(`SELECT count(*) FROM turns WHERE length(src) > 0`).Scan(&named); err != nil || named != 4 {
		t.Errorf("named turns = %d, %v", named, err)
	}
}

// standing is a store of sessions each left in a state its transcript says, at a time
// before now: an ask, then a call or an answer.
type standing map[string]struct {
	ago  time.Duration
	last string // "ask", "call" or "answer"
}

func (standing) Location() string { return "test" }
func (standing) Exists() bool     { return true }
func (s standing) Scan(context.Context) ([]sources.Ref, error) {
	var refs []sources.Ref
	for key := range s {
		refs = append(refs, sources.Ref{Key: key, Stamp: "1", ModTime: time.Now()})
	}
	return refs, nil
}
func (s standing) Read(_ context.Context, ref sources.Ref, w *transcript.Writer) error {
	w.S.ID, w.Tells = ref.Key, transcript.TellsByRows
	at := time.Now().Add(-s[ref.Key].ago)
	w.Add(transcript.Message{Role: transcript.User, At: at, Blocks: []transcript.Block{transcript.TextOf("deploy " + ref.Key)}})
	switch s[ref.Key].last {
	case "call":
		w.Add(transcript.Message{Role: transcript.Assistant, At: at, Response: "r", Blocks: []transcript.Block{transcript.CallOf("c", "Bash", map[string]any{"command": "make deploy"})}})
	case "answer":
		w.Add(transcript.Message{Role: transcript.Assistant, At: at, Response: "r", Blocks: []transcript.Block{transcript.TextOf("Deployed.")}})
	}
	return nil
}

// Where a session's latest turn stands is kept with it, and said while the transcript was
// written recently enough to believe it: working for minutes, waiting or idle for a day.
// The list can be narrowed to the sessions that stand so.
func TestWhereALatestTurnStandsIsKeptAndBelieved(t *testing.T) {
	ix := openTemp(t)
	store := standing{
		"working":   {time.Minute, "ask"},
		"stopped":   {WorkingFor + time.Minute, "ask"},
		"waiting":   {2 * time.Hour, "call"},
		"idle":      {3 * time.Hour, "answer"},
		"forgotten": {WaitingFor + time.Hour, "answer"},
	}
	if _, err := ix.Sync(context.Background(), []Store{{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: 1}, Source: store}}, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	list, _, err := ix.Sessions(context.Background(), Filter{}, Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	said := map[string]string{}
	for _, s := range list {
		if s.Live != nil {
			said[s.ID] = s.Live.State
			if s.Live.State == model.LiveWaiting && (s.Live.Tool != "Bash" || s.Live.Command != "make deploy" || s.Live.Seq != 1) {
				t.Errorf("the call waited on = %+v", s.Live)
			}
		}
	}
	if want := map[string]string{"working": "working", "waiting": "waiting", "idle": "idle"}; fmt.Sprint(said) != fmt.Sprint(want) {
		t.Errorf("said = %v, want %v: a turn silent for long is not believed", said, want)
	}
	for _, c := range []struct {
		states []string
		want   string
	}{
		{[]string{"working"}, "[working]"},
		{[]string{"waiting", "idle"}, "[idle waiting]"},
		{[]string{"idle"}, "[idle]"},
	} {
		list, total, err := ix.Sessions(context.Background(), Filter{State: c.states}, Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, s := range list {
			ids = append(ids, s.ID)
		}
		slices.Sort(ids)
		if fmt.Sprint(ids) != c.want || total != len(ids) {
			t.Errorf("state %v = %v (%d), want %s", c.states, ids, total, c.want)
		}
	}
}
