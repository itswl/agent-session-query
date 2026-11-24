package index

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// refused is the error the second turn of a refusal ends on.
const refused = "402 Insufficient Balance: the account has no credit left for this request. (insufficient_balance)"

// refusal is a store of one session of two turns, written a minute before now: the first
// came to an error the CLI went on past, making the call again and having it answered; the
// second to a provider's refusal and no reply, after which the CLI wrote a step of its own.
// over says the CLI marked the second turn over; without it the session is at work on it.
type refusal struct {
	at   time.Time
	over bool
}

func (refusal) Location() string { return "test" }
func (refusal) Exists() bool     { return true }
func (r refusal) Scan(context.Context) ([]sources.Ref, error) {
	stamp := "at work"
	if r.over {
		stamp = "over"
	}
	return []sources.Ref{{Key: "k", Stamp: stamp, ModTime: r.at}}, nil
}
func (r refusal) Read(_ context.Context, _ sources.Ref, w *transcript.Writer) error {
	w.S.ID, w.S.Cwd, w.Tells = "refused", "/work/site", transcript.TellsByMarks
	add := func(sec int, role transcript.Role, ends bool, blocks ...transcript.Block) {
		w.Add(transcript.Message{Role: role, At: r.at.Add(time.Duration(sec) * time.Second), Ends: ends, Blocks: blocks})
	}
	step := transcript.EventOf("step", "Step 1 · 3s")
	step.Routine = true
	add(0, transcript.User, false, transcript.TextOf("deploy the site"))
	add(1, transcript.System, false, transcript.EventOf(model.EventError, "503 overloaded"))
	add(2, transcript.Assistant, false, transcript.TextOf("Deployed."))
	add(3, transcript.System, true)
	add(10, transcript.User, false, transcript.TextOf("now deploy the docs"))
	add(11, transcript.System, false, transcript.EventOf(model.EventError, refused))
	add(12, transcript.System, false, step)
	if r.over {
		add(13, transcript.System, true)
	}
	return nil
}

// A turn that ended on an error says so wherever its turns are read — the outline, the turns
// with their items, a search's hit, a digest's session — and the index keeps the error in
// the turn's row, for what reads turns without their heads. A turn the CLI went on past its
// error in has none. While the session is at work on the turn, it has not ended, on the
// error or otherwise: the CLI may make the call again.
func TestATurnThatEndedOnAnError(t *testing.T) {
	ix := openTemp(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	read := func(over bool) Session {
		t.Helper()
		if _, err := ix.Sync(ctx, []Store{{Name: "dsh", Spec: sources.Spec{Name: "dsh", Reads: 1}, Source: refusal{at: at, over: over}}}, SyncOptions{Force: true}); err != nil {
			t.Fatal(err)
		}
		s, err := ix.Resolve(ctx, "dsh/refused", nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// What each place says the two turns ended on
	ended := func(s Session) (outline, turns [2]string, hit, digest string) {
		t.Helper()
		out, err := ix.Outline(ctx, &s)
		if err != nil || len(out) != 2 {
			t.Fatalf("outline = %+v, %v", out, err)
		}
		all, err := ix.Turns(ctx, &s, TurnRange{First: 1, Last: -1})
		if err != nil || len(all) != 2 {
			t.Fatalf("turns = %+v, %v", all, err)
		}
		for i := range 2 {
			outline[i], turns[i] = out[i].Error, all[i].Error
		}
		res, err := ix.Search(ctx, ParseQuery("docs"), Filter{}, SearchOptions{})
		if err != nil || len(res.Hits) != 1 || res.Hits[0].Turn != 2 {
			t.Fatalf("search = %+v, %v", res, err)
		}
		d, err := ix.Digest(ctx, Filter{}, Window{})
		if err != nil || len(d.Projects) != 1 || len(d.Projects[0].List) != 1 {
			t.Fatalf("digest = %+v, %v", d, err)
		}
		return outline, turns, res.Hits[0].Error, d.Projects[0].List[0].Error
	}
	column := func() (errs [2]string) {
		t.Helper()
		rows, err := ix.rw.Query(`SELECT idx, error FROM turns WHERE idx > 0 ORDER BY idx`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for i := 0; rows.Next(); i++ {
			var idx int
			if err := rows.Scan(&idx, &errs[i]); err != nil {
				t.Fatal(err)
			}
		}
		return errs
	}

	s := read(true)
	if s.Live == nil || s.Live.State != model.LiveIdle {
		t.Fatalf("live = %+v, want the turn over", s.Live)
	}
	want := [2]string{"", refused}
	if outline, turns, hit, digest := ended(s); outline != want || turns != want || hit != refused || digest != refused {
		t.Errorf("over: outline %q, turns %q, hit %q, digest %q; want the second turn's error everywhere", outline, turns, hit, digest)
	}
	if got := column(); got != want {
		t.Errorf("the turns' rows hold %q, want %q", got, want)
	}

	// At work on the turn: the error is still the record's, and said of no turn
	s = read(false)
	if s.Live == nil || s.Live.State != model.LiveWorking {
		t.Fatalf("live = %+v, want at work", s.Live)
	}
	if outline, turns, hit, digest := ended(s); outline != [2]string{} || turns != [2]string{} || hit != "" || digest != "" {
		t.Errorf("at work: outline %q, turns %q, hit %q, digest %q; want no error said", outline, turns, hit, digest)
	}
	if got := column(); got != want {
		t.Errorf("at work, the turns' rows hold %q, want %q", got, want)
	}
}

// An index an asq made before turns said their errors holds each turn as that asq made it,
// without its error, read with its adapter's reading then. The adapters of the CLIs whose
// errors are read as error events read differently now, and raise their reading: the next
// sync reads their sessions again, once, though no file moved, and the turn that ended on an
// error says so from then on. A CLI that writes no error event reads as it did, and its
// sessions are not read again.
func TestTheSessionsOfAReadingRaisedForErrorsAreReadAgain(t *testing.T) {
	ix := openTemp(t)
	ctx := context.Background()
	stores := func(dsh int) []Store {
		return []Store{
			{Name: "dsh", Spec: sources.Spec{Name: "dsh", Reads: dsh}, Source: refusal{at: time.Now().Add(-time.Hour), over: true}},
			{Name: "codex", Spec: sources.Spec{Name: "codex", Reads: 3}, Source: many{n: 3}},
		}
	}
	reading := 2 // dsh's, before
	sync := func() int {
		t.Helper()
		res, err := ix.Sync(ctx, stores(reading), SyncOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Read
	}
	ended := func() string {
		t.Helper()
		s, err := ix.Resolve(ctx, "dsh/refused", nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := ix.Outline(ctx, &s)
		if err != nil || len(out) != 2 {
			t.Fatalf("outline = %+v, %v", out, err)
		}
		var column string
		if err := ix.rw.QueryRow(`SELECT error FROM turns WHERE sid = ? AND idx = 2`, s.sid).Scan(&column); err != nil || column != out[1].Error {
			t.Errorf("the row holds %q (%v), the head %q", column, err, out[1].Error)
		}
		return out[1].Error
	}
	if n := sync(); n != 4 {
		t.Fatalf("the first sync read %d, want 4", n)
	}

	// As the asq before left them: its turns without their errors
	if _, err := ix.rw.Exec(`UPDATE turns SET error = '', head = json_remove(head, '$.error'), hash = x'00', src = x'' WHERE error != ''`); err != nil {
		t.Fatal(err)
	}
	if got := ended(); got != "" {
		t.Fatalf("as the asq before left it, the turn ended on %q", got)
	}
	reading = 3 // dsh's now
	if n := sync(); n != 1 {
		t.Errorf("after the upgrade, %d sessions read again, want dsh's one alone", n)
	}
	if got := ended(); got != refused {
		t.Errorf("read again, the turn ended on %q, want %q", got, refused)
	}
	if n := sync(); n != 0 {
		t.Errorf("and then %d, want none", n)
	}
}

// An index an asq from before turns' errors made gains the column in place: a turn it holds
// has none, and a turn written now has its own.
func TestAnIndexFromBeforeTurnsErrorsGainsTheColumn(t *testing.T) {
	column := "  error      TEXT NOT NULL DEFAULT '',     -- the error its work ended on, as its head has it\n"
	if !strings.Contains(schema, column) {
		t.Fatalf("the column is not where the test looks for it: %q", column)
	}
	path := filepath.Join(t.TempDir(), "index.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(strings.Replace(schema, column, "", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO turns (sid, idx, hash, head, body) VALUES (7, 1, x'00', '{"index":1}', x'')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var held string
	if err := ix.rw.QueryRow(`SELECT error FROM turns WHERE sid = 7`).Scan(&held); err != nil || held != "" {
		t.Errorf("the turn from before holds %q (%v), want none", held, err)
	}
	ctx := context.Background()
	if _, err := ix.Sync(ctx, []Store{{Name: "dsh", Spec: sources.Spec{Name: "dsh", Reads: 1}, Source: refusal{at: time.Now().Add(-time.Hour), over: true}}}, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	var written string
	if err := ix.rw.QueryRow(`SELECT t.error FROM turns t JOIN sessions s ON s.sid = t.sid WHERE s.id = 'refused' AND t.idx = 2`).Scan(&written); err != nil || written != refused {
		t.Errorf("the turn written now holds %q (%v), want its error", written, err)
	}
}
