package index

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/claude"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// growing is a store of one session that grows a turn at a time: each turn an ask, a
// command whose output is long enough to go to the bodies table, and an answer. recorded,
// when set, is what the CLI keeps as the session's total beyond its responses.
type growing struct {
	turns    int
	recorded int64
	marked   bool // it marks each turn, and takes a read up at the mark it is offered
	// breaks makes a read taken up at a mark fail: "at once", or "after" its turns are read
	breaks string
}

func (growing) Location() string { return "test" }
func (growing) Exists() bool     { return true }
func (g growing) Scan(context.Context) ([]sources.Ref, error) {
	return []sources.Ref{{Key: "s", Stamp: fmt.Sprint(g.turns, g.recorded), ModTime: time.Now()}}, nil
}
func (g growing) Read(_ context.Context, _ sources.Ref, w *transcript.Writer) error {
	w.S.ID, w.S.Title = "grown", "A session that grows"
	start := 1
	if m := w.From; m != nil && g.marked {
		if json.Unmarshal(m.Source, &start) == nil && start <= g.turns {
			w.Resume(m)
			if g.breaks == "at once" {
				return errors.New("the transcript went away")
			}
		}
	}
	for i := start; i <= g.turns; i++ {
		at := time.Date(2026, 11, 1, 9, i, 0, 0, time.UTC)
		id := fmt.Sprint(i)
		w.Add(transcript.Message{Role: transcript.User, At: at, Blocks: []transcript.Block{transcript.TextOf("step " + id + " of the migration")}})
		if g.marked {
			w.MarkTurn(i)
		}
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
	if w.Resumed() && g.breaks == "after" {
		return errors.New("a line too long")
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
	if err := ix.writeAll(context.Background(), []*reading{rd})[0].err; err != nil {
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

// A read taken up at a mark enters what the CLI recorded beyond its responses against the
// new last turn as a whole read does: what the turns before the mark spent is counted in.
func TestRecordedTotalsWithAReadTakenUp(t *testing.T) {
	ix := openTemp(t)
	syncOf(t, ix, growing{turns: 4, recorded: 1000, marked: true})
	g := growing{turns: 5, recorded: 2000, marked: true}
	refs, _ := g.Scan(context.Background())
	if rd := ix.read(context.Background(), growingStore(g)[0], refs[0], sidOf(t, ix)); rd.from != 4 {
		t.Fatalf("the read was taken up at turn %d, want 4", rd.from)
	}
	syncOf(t, ix, g)
	fresh := openTemp(t)
	syncOf(t, fresh, g)
	if got, want := rowsOf(t, ix), rowsOf(t, fresh); got != want {
		t.Errorf("after the totals moved:\n%s\nwant:\n%s", got, want)
	}
}

// A read taken up at a mark that fails leaves the Writer as it was at the mark and some of
// the turns after it, which is no session to keep: the transcript is read whole instead.
func TestAReadTakenUpThatFailsIsReadWhole(t *testing.T) {
	for _, breaks := range []string{"at once", "after"} {
		ix := openTemp(t)
		syncOf(t, ix, growing{turns: 4, marked: true})
		g := growing{turns: 6, marked: true, breaks: breaks}
		refs, _ := g.Scan(context.Background())
		rd := ix.read(context.Background(), growingStore(g)[0], refs[0], sidOf(t, ix))
		if rd.err != "" || rd.from != 0 || len(rd.turns) != 6 {
			t.Errorf("failing %s: error %q, taken up at %d, %d turns; want a whole read of 6", breaks, rd.err, rd.from, len(rd.turns))
		}
	}
}

// A mark is taken up only by a read like the one that left it, and only while the session's
// row is as that read wrote it. Another asq that wrote the session since, with its stamp or
// its reading, or a mark left with another reading, other rows or another version of what a
// mark holds, has the transcript read whole.
func TestAMarkIsTakenUpOnlyAsItWasLeft(t *testing.T) {
	for _, c := range []struct {
		name, since string // since: what happened to the row after the read that left the mark
		taken       bool
	}{
		{"as it was left", ``, true},
		{"the row written since by another asq", `UPDATE sessions SET stamp = 'another'`, false},
		{"the row read since with another reading", `UPDATE sessions SET reads = reads + 1`, false},
		{"a mark left by another reading", `UPDATE sessions SET mark = json_set(mark, '$.reads', 2)`, false},
		{"a mark of other rows", `UPDATE sessions SET mark = json_set(mark, '$.rows', 2)`, false},
		{"a mark of another version", `UPDATE sessions SET mark = json_set(mark, '$.v', 2)`, false},
	} {
		ix := openTemp(t)
		syncOf(t, ix, growing{turns: 4, marked: true})
		if c.since != "" {
			if _, err := ix.rw.Exec(c.since); err != nil {
				t.Fatal(err)
			}
		}
		g := growing{turns: 5, marked: true}
		refs, _ := g.Scan(context.Background())
		rd := ix.read(context.Background(), growingStore(g)[0], refs[0], sidOf(t, ix))
		if taken := rd.from > 0; taken != c.taken || len(rd.turns) == 0 {
			t.Errorf("%s: taken up at turn %d, %d turns read; want taken up %v", c.name, rd.from, len(rd.turns), c.taken)
		}
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
	if err := ix.writeAll(context.Background(), []*reading{rd})[0].err; err != nil {
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
	if err := ix.writeAll(context.Background(), []*reading{rd})[0].err; err != nil {
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

// ending is a store of one session whose CLI marks the end of every turn: answered, and
// then, once over is set, marked over. n moves the stamp alone.
type ending struct {
	at   time.Time
	over bool
	n    int
}

func (ending) Location() string { return "test" }
func (ending) Exists() bool     { return true }
func (e ending) Scan(context.Context) ([]sources.Ref, error) {
	return []sources.Ref{{Key: "k", Stamp: fmt.Sprint(e.over, e.n), ModTime: time.Now()}}, nil
}
func (e ending) Read(_ context.Context, _ sources.Ref, w *transcript.Writer) error {
	w.S.ID, w.Tells = "ending", transcript.TellsByMarks
	w.Add(transcript.Message{Role: transcript.User, At: e.at, Blocks: []transcript.Block{transcript.TextOf("summarise the build log")}})
	w.Add(transcript.Message{Role: transcript.Assistant, At: e.at.Add(time.Second), Response: "r", Blocks: []transcript.Block{transcript.TextOf("The build is green.")}})
	if e.over {
		w.Add(transcript.Message{Role: transcript.System, At: e.at.Add(2 * time.Second), Ends: true})
	}
	return nil
}

// A turn marked over changes where the session stands and none of its turns. The change is
// told as the session's, as one to a turn is, so that a page showing the session at work
// learns that it is done; a session past being said to stand anywhere changes nothing shown.
func TestATurnMarkedOverIsAChange(t *testing.T) {
	for _, c := range []struct {
		ago  time.Duration
		told bool
	}{{time.Minute, true}, {WaitingFor + time.Hour, false}} {
		ix := openTemp(t)
		at := time.Now().Add(-c.ago).Truncate(time.Second)
		store := func(e ending) []Store {
			e.at = at
			return []Store{{Name: "codex", Spec: sources.Spec{Name: "codex", Reads: 1}, Source: e}}
		}
		if _, err := ix.Sync(context.Background(), store(ending{}), SyncOptions{}); err != nil {
			t.Fatal(err)
		}
		res, err := ix.Sync(context.Background(), store(ending{over: true}), SyncOptions{Force: true})
		if err != nil {
			t.Fatal(err)
		}
		if told := len(res.Changes) == 1 && res.Changes[0].ID == "ending"; res.Read != 1 || told != c.told {
			t.Errorf("written %s ago and marked over: read %d, changes %+v; want told %v", c.ago, res.Read, res.Changes, c.told)
		}
		// Read again with nothing moved, it is no change
		if again, err := ix.Sync(context.Background(), store(ending{over: true, n: 1}), SyncOptions{Force: true}); err != nil || again.Read != 1 || len(again.Changes) != 0 {
			t.Errorf("read again as it was: %+v (%v)", again, err)
		}
	}
}

// claudeRows is a Claude Code transcript of a few turns, as the CLI writes it: long
// outputs, a failure, an edit, a prompt queued mid-turn, a turn a screenshot pasted alone
// began and a screenshot a tool returned, a skill set off by a slash command, the title
// given and given again, another model taking over, the rows Code Mode and a plugin add,
// and the cost the session closes with.
func claudeRows() []any {
	type M = map[string]any
	ts := func(sec int) string { return time.Date(2026, 11, 1, 9, 0, sec, 0, time.UTC).Format(time.RFC3339Nano) }
	row := func(kind, uuid string, sec int, extra M) M {
		r := M{"type": kind, "uuid": uuid, "timestamp": ts(sec), "sessionId": "c1aa0000-0000-4000-8000-000000000001",
			"cwd": "/work/grown", "gitBranch": "main", "version": "2.1.300"}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	ask := func(uuid string, sec int, text string, extra ...M) M {
		r := row("user", uuid, sec, M{"message": M{"role": "user", "content": text}})
		for _, e := range extra {
			for k, v := range e {
				r[k] = v
			}
		}
		return r
	}
	answer := func(uuid string, sec int, response, stop string, content ...M) M {
		return row("assistant", uuid, sec, M{"message": M{"id": response, "role": "assistant", "model": "claude-opus-5-5", "stop_reason": stop,
			"content": content, "usage": M{"input_tokens": 4, "output_tokens": 30, "cache_read_input_tokens": 900}}})
	}
	bash := func(id, command string) M {
		return M{"type": "tool_use", "id": id, "name": "Bash", "input": M{"command": command}}
	}
	result := func(uuid string, sec int, id, out string, failed bool) M {
		return row("user", uuid, sec, M{"message": M{"role": "user", "content": []M{{"type": "tool_result", "tool_use_id": id, "content": out, "is_error": failed}}}})
	}
	lasted := func(uuid string, sec int) M {
		return row("system", uuid, sec, M{"subtype": "turn_duration", "durationMs": 9000})
	}
	long := strings.Repeat("compiling package migration\n", 200)
	png := M{"type": "image", "source": M{"type": "base64", "media_type": "image/png",
		"data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="}}
	return []any{
		M{"type": "file-history-snapshot", "messageId": "x"},
		ask("u1", 0, "run the migration"),
		answer("a1", 2, "msg_1", "tool_use", M{"type": "thinking", "thinking": "the schema first"}, bash("t1", "make migrate")),
		result("u2", 5, "t1", long+"Exit code 2", true),
		answer("a2", 7, "msg_2", "end_turn", M{"type": "text", "text": "The migration failed on the schema."}),
		lasted("d1", 8),
		M{"type": "ai-title", "aiTitle": "Run the migration", "sessionId": "c1aa0000-0000-4000-8000-000000000001"},
		ask("u3", 20, "fix the schema and run it again"),
		row("system", "f1", 21, M{"subtype": "model_refusal_fallback", "content": "Opus's safeguards flagged this request. Switched to Sonnet.",
			"level": "warning", "originalModel": "claude-opus-5-5", "fallbackModel": "claude-sonnet-5"}),
		answer("a3", 22, "msg_3", "tool_use", M{"type": "tool_use", "id": "t2", "name": "Edit",
			"input": M{"file_path": "/work/grown/db/schema.sql", "old_string": "INT", "new_string": "BIGINT"}}),
		result("u4", 23, "t2", "The file has been updated.", false),
		row("attachment", "q1", 24, M{"attachment": M{"type": "queued_command", "commandMode": "prompt", "prompt": "and keep a backup"}}),
		answer("a4", 25, "msg_4", "tool_use", bash("t3", "make migrate")),
		result("u5", 31, "t3", long+"done", false),
		answer("a5", 33, "msg_5", "end_turn", M{"type": "text", "text": "The migration is done, with a backup kept."}),
		lasted("d2", 34),
		row("user", "s1", 36, M{"message": M{"role": "user", "content": []M{png}}}),
		answer("s2", 37, "msg_s1", "tool_use", M{"type": "tool_use", "id": "t5", "name": "mcp__playwright__browser_take_screenshot", "input": M{}}),
		row("user", "s3", 38, M{"message": M{"role": "user", "content": []M{{"type": "tool_result", "tool_use_id": "t5",
			"content": []M{{"type": "text", "text": "Took the screenshot"}, png}}}}}),
		answer("s4", 39, "msg_s2", "end_turn", M{"type": "text", "text": "The two match."}),
		ask("u6", 40, "<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>the migration</command-args>"),
		ask("u7", 41, "You are reviewing a change. Follow these steps…", M{"isMeta": true}),
		answer("a6", 45, "msg_6", "end_turn", M{"type": "text", "text": "The migration looks right."}),
		M{"type": "custom-title", "customTitle": "Schema migration and review", "sessionId": "c1aa0000-0000-4000-8000-000000000001"},
		ask("u8", 60, "deploy it"),
		M{"type": "api-request-resumed", "sessionId": "c1aa0000-0000-4000-8000-000000000001"},
		row("attachment", "p1", 60, M{"attachment": M{"type": "plugin_note", "plugin": "notes", "content": []string{"deploys go through staging"}}}),
		row("system", "f2", 61, M{"subtype": "model_fallback", "level": "warning", "originalModel": "claude-opus-5-5", "fallbackModel": "claude-sonnet-5"}),
		answer("a7", 62, "msg_7", "tool_use", bash("t4", "make deploy")),
		M{"type": "host-row", "sessionId": "c1aa0000-0000-4000-8000-000000000001", "ranByToolUseId": "t4", "rowUuid": "a7", "block": M{"type": "text", "text": "shown"}},
		M{"type": "cost-state", "totalCostUSD": 0.37},
	}
}

// claudeIndex is a Claude Code store of one transcript, and an index of it.
func claudeIndex(t *testing.T, content []byte) (*Index, string, []Store) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "-work-grown", "c1aa0000-0000-4000-8000-000000000001.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return openTemp(t), path, []Store{{Name: "claude", Spec: claude.Spec, Source: claude.Spec.Open(root)}}
}

// heldOf is everything the index holds of its sessions, as rowsOf, with the session's row,
// the words of its title, and its failures with where each command passed later.
func heldOf(t *testing.T, ix *Index) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(rowsOf(t, ix))
	var title, live, firstID, lastID string
	var turns, failures int
	var cost float64
	if err := ix.rw.QueryRow(`SELECT title, live, first_id, last_id, turns, failures, cost FROM sessions`).
		Scan(&title, &live, &firstID, &lastID, &turns, &failures, &cost); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(&b, title, live, firstID, lastID, turns, failures, cost)
	for _, q := range []string{"title:schema", "title:migration", "title:review", "backup", "BIGINT", "deploy"} {
		res, err := ix.Search(context.Background(), ParseQuery(q), Filter{}, SearchOptions{Limit: 100, PerSession: -1})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(&b, q, res.Total)
	}
	for _, q := range []string{
		`SELECT idx || ' ' || path || ' ' || changed FROM files ORDER BY idx, path`,
		`SELECT idx || ' ' || seq || ' ' || tool || ' ' || command || ' ' || command_key || ' ' || later_idx || ' ' || later_seq || ' ' || judged FROM failures ORDER BY seq`,
	} {
		rows, err := ix.rw.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&b, line)
		}
		rows.Close()
	}
	return b.String()
}

// A Claude Code transcript read again as it grows is taken up where its latest turn
// began, wherever the read before stopped: the index then holds what one made from the
// transcript as it is holds, the title's words included, and the read made only the turns
// from the mark on.
func TestAGrowingTranscriptIsTakenUpAtItsLatestTurn(t *testing.T) {
	var whole bytes.Buffer
	enc := json.NewEncoder(&whole)
	enc.SetEscapeHTML(false)
	for _, r := range claudeRows() {
		enc.Encode(r)
	}
	content := whole.Bytes()
	freshOf := func(content []byte) string {
		fresh, _, stores := claudeIndex(t, content)
		if _, err := fresh.Sync(context.Background(), stores, SyncOptions{Force: true}); err != nil {
			t.Fatal(err)
		}
		return heldOf(t, fresh)
	}

	ix, path, stores := claudeIndex(t, nil)
	taken := 0
	for end := 1; end <= len(content); end++ {
		if content[end-1] != '\n' {
			continue
		}
		if err := os.WriteFile(path, content[:end], 0o644); err != nil {
			t.Fatal(err)
		}
		refs, _ := stores[0].Source.Scan(context.Background())
		var sid int64
		if ix.rw.QueryRow(`SELECT sid FROM sessions`).Scan(&sid) == nil {
			if rd := ix.read(context.Background(), stores[0], refs[0], sid); rd.from > 0 {
				taken++
				for _, row := range rd.turns {
					if row.idx < rd.from {
						t.Errorf("a read taken up at turn %d made turn %d", rd.from, row.idx)
					}
				}
			}
		}
		if _, err := ix.Sync(context.Background(), stores, SyncOptions{Force: true}); err != nil {
			t.Fatal(err)
		}
		// The title's words, when a row changed the title, are written with it, not later
		var stale int
		if err := ix.rw.QueryRow(`SELECT count(*) FROM words_stale`).Scan(&stale); err != nil || stale != 0 {
			t.Errorf("after the first %d bytes, %d turns' words are left to write (%v)", end, stale, err)
		}
		if got, want := heldOf(t, ix), freshOf(content[:end]); got != want {
			t.Fatalf("after the first %d bytes, the index of a transcript read as it grew:\n%s\nwant:\n%s", end, got, want)
		}
	}
	if taken < 10 {
		t.Errorf("%d reads were taken up at a mark", taken)
	}

	// Grown by many rows at once, from wherever the read before stopped
	want := freshOf(content)
	for end := 1; end < len(content); end++ {
		if content[end-1] != '\n' {
			continue
		}
		ix, path, stores := claudeIndex(t, content[:end])
		for _, b := range [][]byte{content[:end], content} {
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ix.Sync(context.Background(), stores, SyncOptions{Force: true}); err != nil {
				t.Fatal(err)
			}
		}
		if got := heldOf(t, ix); got != want {
			t.Fatalf("grown from the first %d bytes to the whole:\n%s\nwant:\n%s", end, got, want)
		}
	}
}

// A read taken up at a mark that changed the title has the title's words made from what the
// index holds, and written with the words of everything else its batch read: after the
// rest, as one segment of the words table (see wordsOp), and not between one session's
// rows and the next's.
func TestTitleWordsOfAReadTakenUpAreWrittenWithTheRest(t *testing.T) {
	rows := claudeRows()
	titled := slices.IndexFunc(rows, func(r any) bool { return r.(map[string]any)["type"] == "custom-title" })
	encode := func(rows []any, id string) []byte {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		for _, r := range rows {
			enc.Encode(r)
		}
		// Rows of their own, or the second would be taken for a copy of the first
		out := bytes.ReplaceAll(b.Bytes(), []byte("c1aa0000-0000-4000-8000-000000000001"), []byte(id))
		out = bytes.ReplaceAll(out, []byte(`"uuid":"`), []byte(`"uuid":"`+id[len(id)-1:]))
		return bytes.ReplaceAll(out, []byte(`"msg_`), []byte(`"msg_`+id[len(id)-1:]))
	}
	ids := []string{"c1aa0000-0000-4000-8000-00000000000a", "c1aa0000-0000-4000-8000-00000000000b"}
	root := t.TempDir()
	dir := filepath.Join(root, "-work-grown")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(id string, rows []any) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), encode(rows, id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		write(id, rows[:titled])
	}
	ix := openTemp(t)
	st := Store{Name: "claude", Spec: claude.Spec, Source: claude.Spec.Open(root)}
	ctx := context.Background()
	if _, err := ix.Sync(ctx, []Store{st}, SyncOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	// Segments are left as they are written, to be counted (see the test of a sync's words)
	if _, err := ix.rw.Exec(`INSERT INTO words (words, rank) VALUES ('automerge', 0)`); err != nil {
		t.Fatal(err)
	}
	segments := func() int {
		t.Helper()
		var n int
		if err := ix.rw.QueryRow(`SELECT count(*) FROM words_data`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := segments()

	// The first is given a new title and a turn, the second a turn
	write(ids[0], rows)
	write(ids[1], slices.Delete(slices.Clone(rows), titled, titled+1))
	refs, err := st.Source.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(refs, func(a, b sources.Ref) int { return strings.Compare(a.Key, b.Key) })
	var rds []*reading
	for _, ref := range refs {
		var sid int64
		if err := ix.rw.QueryRow(`SELECT sid FROM sessions WHERE key = ?`, ref.Key).Scan(&sid); err != nil {
			t.Fatal(err)
		}
		rd := ix.read(ctx, st, ref, sid)
		if rd.from <= titleTurn {
			t.Fatalf("%s: read taken up at turn %d, not after the title's", ref.Key, rd.from)
		}
		rds = append(rds, rd)
	}
	if !strings.Contains(rds[0].sess.ID, ids[0]) {
		t.Fatalf("the first read is of %s", rds[0].sess.ID)
	}
	for _, w := range ix.writeAll(ctx, rds) {
		if w.err != nil {
			t.Fatal(w.err)
		}
	}
	// One segment, and the page of what was deleted from the one before
	if n := segments() - before; n > 2 {
		t.Errorf("two reads taken up, one with a new title, were written as %d pages of the words table, not a segment and its deletions", n)
	}
	res, err := ix.Search(ctx, ParseQuery("title:review"), Filter{}, SearchOptions{Limit: 10, PerSession: -1})
	if err != nil || res.Total != 1 {
		t.Errorf("the new title is found in %d turns (%v), want 1", res.Total, err)
	}
	var stale int
	if err := ix.rw.QueryRow(`SELECT count(*) FROM words_stale`).Scan(&stale); err != nil || stale != 0 {
		t.Errorf("%d turns' words are left to write (%v)", stale, err)
	}
}
