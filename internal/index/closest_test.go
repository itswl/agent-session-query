package index_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

// hitNames is each hit as the start of its session's id and its turn, with what it lacks.
func hitNames(res index.SearchResult) []string {
	var out []string
	for _, h := range res.Hits {
		name := h.Session.ID[:min(8, len(h.Session.ID))] + "#" + h.Ref[strings.LastIndex(h.Ref, "#")+1:]
		if len(h.Lacks) > 0 {
			name += " lacks " + strings.Join(h.Lacks, ",")
		}
		out = append(out, name)
	}
	return out
}

func TestSearchCloseToTheWords(t *testing.T) {
	a, _ := fixture.Open(t)
	ctx := context.Background()
	f := filter(a)
	f.Agents = true
	for _, c := range []struct {
		query   string
		hits    []string // in order
		partial *index.Partial
		missing []string
		why     string
	}{
		{"ECONNREFUSED", []string{"aaaa1111#1"}, nil, nil,
			"a search some turn matches whole is answered as it was"},
		{"ECONNREFUSED 数据库", []string{"aaaa1111#1 lacks 数据库", "aaaa1111#2 lacks ECONNREFUSED"}, &index.Partial{Words: 2, Held: 1}, nil,
			"two words no one turn holds: the turns that hold one, each saying which it lacks"},
		{"ECONNREFUSED flaky nothinglikethis", []string{"aaaa1111#1 lacks nothinglikethis"}, &index.Partial{Words: 3, Held: 2}, []string{"nothinglikethis"},
			"a word no turn holds is named, and the closest turns hold the rest"},
		{"nothinglikethis", nil, nil, []string{"nothinglikethis"},
			"a word alone that nothing holds"},
		{"ask:flaky ask:数据库", []string{"aaaa1111#1 lacks ask:数据库", "aaaa1111#2 lacks ask:flaky"}, &index.Partial{Words: 2, Held: 1}, nil,
			"words confined to a part are named as they were typed"},
		{"ECONNREFUSED 数据库 -user_id", []string{"aaaa1111#1 lacks 数据库"}, &index.Partial{Words: 2, Held: 1}, nil,
			"an excluded word stays excluded"},
		{"ECONNREFUSED 数据库 -orders", []string{"aaaa1111#1 lacks 数据库"}, &index.Partial{Words: 2, Held: 1}, nil,
			"as typed: #2 says orders, and #1, which says order, stays"},
		{"keys flaky -ECONNREFUSED", nil, nil, nil,
			"the turn that holds every word holds one excluded: no turn is said to lack a word, and none is called close"},
		{"flaky -ECONNREFUSED -数据库", nil, nil, nil,
			"one word, which the exclusions leave nowhere: nothing to come close with"},
		{"ECONNREFUSED flaky map order sorting keys 数据库", []string{"aaaa1111#1 lacks 数据库"}, &index.Partial{Words: 7, Held: 6}, nil,
			"all the words but one"},
		// Eight words, the best turns holding three: past the ways of choosing them that are
		// spelled out, the turns are counted
		{"ECONNREFUSED sorting map 数据库 indexes tables rotate finished", nil, &index.Partial{Words: 8, Held: 3}, nil,
			"many words"},
	} {
		res, err := a.Index.Search(ctx, index.ParseQuery(c.query), f, index.SearchOptions{PerSession: -1})
		if err != nil {
			t.Fatalf("%s: %v", c.query, err)
		}
		got := hitNames(res)
		if c.hits != nil && !reflect.DeepEqual(got, c.hits) {
			t.Errorf("%s: %q\n hits %q\n want %q", c.why, c.query, got, c.hits)
		}
		if !reflect.DeepEqual(res.Partial, c.partial) {
			t.Errorf("%s: %q partial = %+v, want %+v", c.why, c.query, res.Partial, c.partial)
		}
		if !reflect.DeepEqual(res.Missing, c.missing) {
			t.Errorf("%s: %q missing = %q, want %q", c.why, c.query, res.Missing, c.missing)
		}
		if res.Total != len(res.Hits) {
			t.Errorf("%s: %q total = %d for %d hits", c.why, c.query, res.Total, len(res.Hits))
		}
	}

	// Many words: the turns holding three, and not those holding two
	res, _ := a.Index.Search(ctx, index.ParseQuery("ECONNREFUSED sorting map 数据库 indexes tables rotate finished"), f, index.SearchOptions{PerSession: -1})
	var held []int
	for _, h := range res.Hits {
		held = append(held, 8-len(h.Lacks))
	}
	if !reflect.DeepEqual(held, []int{3, 3}) {
		t.Errorf("many words: the hits hold %v of them, want those that hold the most: %q", held, hitNames(res))
	}

	// A word a hit lacks is not shown as found where it is only part of another word: uild
	// is in "make build", and no turn holds it
	res, _ = a.Index.Search(ctx, index.ParseQuery("finished uild"), f, index.SearchOptions{})
	if len(res.Hits) != 1 || len(res.Hits[0].Matches) != 1 || !strings.Contains(res.Hits[0].Matches[0].Snippet, "build finished") {
		t.Errorf("finished uild = %+v", res.Hits)
	}
}

func TestHitsSayHowTheirTurnEnded(t *testing.T) {
	a, _ := fixture.Open(t)
	res, _ := a.Index.Search(context.Background(), index.ParseQuery("flaky"), filter(a), index.SearchOptions{})
	for _, h := range res.Hits {
		if h.Turn == 1 && (h.Tools != 1 || h.Failed != 1 || h.Interrupted) {
			t.Errorf("#1 = tools %d, failed %d, interrupted %v: one call, failed", h.Tools, h.Failed, h.Interrupted)
		}
	}
	// #2 is in the session the title names, and does not say flaky: it is not found
	if names := hitNames(res); !reflect.DeepEqual(names, []string{"aaaa1111#1"}) {
		t.Errorf("flaky = %q, want the turn that says it", names)
	}

	// A match in the session's title says so, and a match in another form says which
	res, _ = a.Index.Search(context.Background(), index.ParseQuery("title:flaky"), filter(a), index.SearchOptions{})
	if len(res.Hits) == 0 || len(res.Hits[0].Matches) == 0 || res.Hits[0].Matches[0].Kind != index.KindTitle || res.Hits[0].Matches[0].Snippet != "Fix flaky test" {
		t.Errorf("a title match = %+v", res.Hits)
	}
	res, _ = a.Index.Search(context.Background(), index.ParseQuery("reply:sorts"), filter(a), index.SearchOptions{})
	if len(res.Hits) != 1 || len(res.Hits[0].Matches) != 1 || res.Hits[0].Matches[0].Word != "sorting" || !strings.Contains(res.Hits[0].Matches[0].Snippet, "by sorting the keys") {
		t.Errorf("a match in another form = %+v", res.Hits)
	}
}

// A hit shows a place once: a phrase, or two words, found in the same output are that
// output's line, not one line for each. Alpha's go test printed "dial tcp: ECONNREFUSED
// 127.0.0.1:5432" and "exit status 1".
func TestAHitShowsAPlaceOnce(t *testing.T) {
	a, _ := fixture.Open(t)
	for _, query := range []string{`"exit status 1"`, `ECONNREFUSED "exit status"`, `output:econnrefused output:exit`} {
		res, err := a.Index.Search(context.Background(), index.ParseQuery(query), filter(a), index.SearchOptions{})
		if err != nil || len(res.Hits) != 1 {
			t.Fatalf("%s: %+v, %v", query, res.Hits, err)
		}
		var places []string
		for _, m := range res.Hits[0].Matches {
			places = append(places, m.Part+" "+strconv.Itoa(m.Seq))
		}
		if len(places) != 1 || places[0] != "output 2" {
			t.Errorf("%s: shown at %q, want the output of step 2 once", query, places)
		}
	}
}

func TestATitleIsFoundAsItChanges(t *testing.T) {
	h := fixture.Write(t)
	a, err := app.Open(h.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	// Claude Code names a session after it has begun: no turn changes, the title does
	path := filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"ai-title","aiTitle":"Key rotation for production"}` + "\n")
	f.Close()
	if _, err := a.Sync(ctx, index.SyncOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	res, _ := a.Index.Search(ctx, index.ParseQuery("production rotation"), filter(a), index.SearchOptions{})
	if res.Total != 1 || res.Hits[0].Session.ID != fixture.Beta {
		t.Errorf("the new title is not found: %+v", hitNames(res))
	}
}

// olderWords makes an index look as an asq before titles and stems left it: the words
// table of five columns, and nothing to say what it holds.
func olderWords(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`DROP TABLE words`,
		`CREATE VIRTUAL TABLE words USING fts5 (ask, reply, thinking, tool, output, content = '', contentless_delete = 1, tokenize = "unicode61 remove_diacritics 2")`,
		`INSERT INTO words (rowid, ask) SELECT (sid << 20) | idx, 'what an older asq wrote' FROM turns WHERE idx > 0`,
		`DELETE FROM meta WHERE key = 'words'`,
		`DROP TRIGGER words_stale_turn`,
		`DROP TRIGGER words_stale_title`,
		`DROP TABLE words_stale`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestWordsAreMadeAnewFromTheIndex(t *testing.T) {
	h := fixture.Write(t)
	cfg := h.Config(t)
	a, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	olderWords(t, cfg.IndexPath)

	a, err = app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	res, err := a.Sync(ctx, index.SyncOptions{})
	if err != nil || res.Read != 0 {
		t.Fatalf("the sync after = %+v, %v: the words are written from the index, and no transcript is read", res, err)
	}
	f := filter(a)
	f.Agents = true
	for query, want := range map[string]int{
		"older":           0, // what the older table held is gone
		"rotating":        1,
		"title:flaky":     1, // with the first turn
		"title:deploy":    1, // … and one taken from how the first ask begins
		"ECONNREFUSED":    1,
		"output:finished": 1, // the end of a long output, which the turn keeps only a preview of
		"数据库":             1,
	} {
		if got, _ := a.Index.Search(ctx, index.ParseQuery(query), f, index.SearchOptions{}); got.Total != want {
			t.Errorf("%q after the words were made anew: %d turns, want %d", query, got.Total, want)
		}
	}
	if st := a.Index.State(); st.Total != 3 || st.Done != 3 {
		t.Errorf("state = %+v: the sessions written again are counted as the sync's work", st)
	}

	// An older asq goes on using the table: it searches it, weighs the five columns it
	// knows, and writes a turn's words without a title or stems — which this asq writes
	// again at its next sync. It changes a title, and the words of that session's turns
	// are written again with it
	db, err := sql.Open("sqlite", cfg.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM (SELECT bm25(words, 8.0, 4.0, 1.0, 2.0, 1.0) FROM words WHERE words MATCH '"rotate"*')`).Scan(&n); err != nil || n != 1 {
		t.Errorf("an older asq's search = %d, %v", n, err)
	}
	beta, _ := a.Index.Resolve(ctx, fixture.Beta, a.Stores())
	id := beta.SID()<<20 | 1
	for _, q := range []string{
		`INSERT OR REPLACE INTO turns SELECT * FROM turns WHERE sid = ` + itoa(beta.SID()) + ` AND idx = 1`,
		`DELETE FROM words WHERE rowid = ` + itoa(id),
		`INSERT INTO words (rowid, ask, reply, thinking, tool, output) VALUES (` + itoa(id) + `, 'rotate the deploy key', 'Rotated', '', '', '')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("an older asq's write: %s: %v", q, err)
		}
	}
	if got, _ := a.Index.Search(ctx, index.ParseQuery("rotating"), f, index.SearchOptions{}); got.Total != 0 {
		t.Fatalf("the older asq's row has stems: %d", got.Total)
	}
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Index.Search(ctx, index.ParseQuery("rotating"), f, index.SearchOptions{}); got.Total != 1 {
		t.Errorf("rotating after an older asq wrote the turn, and a sync: %d", got.Total)
	}
	if _, err := db.Exec(`UPDATE sessions SET title = 'Rotate the deploy key nightly' WHERE sid = ?`, beta.SID()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Index.Search(ctx, index.ParseQuery("title:nightly"), f, index.SearchOptions{}); got.Total != 1 {
		t.Errorf("a title an older asq wrote, after a sync: %d", got.Total)
	}
	var stale int
	if err := db.QueryRow(`SELECT count(*) FROM words_stale`).Scan(&stale); err != nil || stale != 0 {
		t.Errorf("words_stale holds %d after the sync, %v", stale, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// turnSaid is a turn of a session a test writes: what was asked, what a Bash call printed
// (no call when empty), and what the agent replied.
type turnSaid struct{ ask, output, reply string }

// writeSession adds a Claude Code session to a history, in /work/gamma, an hour old: named
// title by its CLI when that is set, and a turn for each of turns.
func writeSession(t *testing.T, h fixture.History, id, title string, turns []turnSaid) {
	t.Helper()
	at := time.Now().Add(-time.Hour).UTC()
	var b strings.Builder
	line := func(v map[string]any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if title != "" {
		line(map[string]any{"type": "ai-title", "aiTitle": title})
	}
	n := 0
	say := func(role string, content any) {
		n++
		line(map[string]any{"type": role, "cwd": "/work/gamma", "sessionId": id, "uuid": "r" + strconv.Itoa(n),
			"timestamp": at.Add(time.Duration(n) * time.Second).Format(time.RFC3339Nano),
			"message":   map[string]any{"role": role, "content": content, "id": "m" + strconv.Itoa(n)}})
	}
	for i, turn := range turns {
		say("user", turn.ask)
		if turn.output != "" {
			call := "t" + strconv.Itoa(i)
			say("assistant", []map[string]any{{"type": "tool_use", "id": call, "name": "Bash", "input": map[string]any{"command": "make all"}}})
			say("user", []map[string]any{{"type": "tool_result", "tool_use_id": call, "content": turn.output}})
		}
		say("assistant", []map[string]any{{"type": "text", "text": turn.reply}})
	}
	path := filepath.Join(h.Dir, "-work-gamma", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// openSynced opens the program over a history and brings its index up to date.
func openSynced(t *testing.T, h fixture.History) *app.App {
	t.Helper()
	a, err := app.Open(h.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if _, err := a.Sync(t.Context(), index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestATitleFindsItsSessionOnce(t *testing.T) {
	h := fixture.Write(t)
	const id = "dddd4444-0000-4000-8000-000000000004"
	var turns []turnSaid
	for i := 1; i <= 12; i++ {
		turn := turnSaid{ask: "go on with step " + strconv.Itoa(i), reply: "Step " + strconv.Itoa(i) + " is done."}
		if i == 4 || i == 8 {
			turn.reply = "Postgres took step " + strconv.Itoa(i) + "."
		}
		turns = append(turns, turn)
	}
	writeSession(t, h, id, "Migrate the billing database to Postgres 17", turns)
	a := openSynced(t, h)
	ctx := context.Background()

	// The two turns that say it, and the first, which holds the title: the session is
	// found by its title once, and not in each of its turns
	res, err := a.Index.Search(ctx, index.ParseQuery("postgres"), filter(a), index.SearchOptions{PerSession: -1})
	names := hitNames(res)
	if err != nil || res.Total != 3 || res.Sessions != 1 || len(names) != 3 || !sameSet(names[:2], []string{"dddd4444#4", "dddd4444#8"}) || names[2] != "dddd4444#1" {
		t.Errorf("postgres = %q, total %d, %v: want #4 and #8, then #1 for the title", names, res.Total, err)
	} else if m := res.Hits[2].Matches; len(m) != 1 || m[0].Kind != index.KindTitle {
		t.Errorf("#1 is found by the title, and says so: %+v", m)
	}
	// A word of the title goes with the first turn's words, not another's
	res, _ = a.Index.Search(ctx, index.ParseQuery("billing step"), filter(a), index.SearchOptions{PerSession: -1})
	if names := hitNames(res); !reflect.DeepEqual(names, []string{"dddd4444#1"}) || res.Total != 1 {
		t.Errorf("billing step = %q, total %d", names, res.Total)
	}

	// Find in the session lists the turns whose own words hold it, as the page asks
	s, err := a.Index.Resolve(ctx, id, a.Stores())
	if err != nil {
		t.Fatal(err)
	}
	f := filter(a)
	f.SID = s.SID()
	res, err = a.Index.Search(ctx, index.ParseQuery("postgres"), f, index.SearchOptions{PerSession: -1, Limit: 100})
	if names := hitNames(res); err != nil || res.Total != 2 || !sameSet(names, []string{"dddd4444#4", "dddd4444#8"}) {
		t.Errorf("postgres in the session = %q, total %d, %v", names, res.Total, err)
	}
	res, _ = a.Index.Search(ctx, index.ParseQuery("billing step"), f, index.SearchOptions{PerSession: -1, Limit: 100})
	if res.Partial == nil || !reflect.DeepEqual(res.Missing, []string{"billing"}) {
		t.Errorf("billing step in the session = %q, partial %+v, missing %q: no turn says billing", hitNames(res), res.Partial, res.Missing)
	}
}

func TestPartialMatchesOfManyWords(t *testing.T) {
	// One long turn holds three of six words; three hundred short ones hold one each, and
	// each of them weighs more than the long one by the ranking
	h := fixture.Write(t)
	const id = "eeee5555-0000-4000-8000-000000000005"
	log := strings.Repeat("a line of a long build log with nothing in it\n", 200)
	turns := []turnSaid{{ask: "build it", output: log + "alpha bravo charlie\n" + log, reply: "Built."}}
	for i := range 300 {
		turns = append(turns, turnSaid{ask: []string{"delta", "echo", "foxtrot"}[i%3], reply: "ok"})
	}
	writeSession(t, h, id, "", turns)
	a := openSynced(t, h)
	q := index.ParseQuery("alpha bravo charlie delta echo foxtrot")
	for _, offset := range []int{0, 10, 50} {
		res, err := a.Index.Search(context.Background(), q, filter(a), index.SearchOptions{Limit: 10, Offset: offset, PerSession: -1})
		if err != nil {
			t.Fatal(err)
		}
		if p := res.Partial; p == nil || p.Words != 6 || p.Held != 3 || res.Total != 1 {
			t.Errorf("offset %d: partial %+v, total %d: want the one turn that holds three", offset, p, res.Total)
		}
		for _, h := range res.Hits {
			if len(h.Lacks) != 3 {
				t.Errorf("offset %d: %s lacks %q, and the closest lack three", offset, h.Ref, h.Lacks)
			}
		}
		if names := hitNames(res); offset == 0 && (len(names) != 1 || !strings.HasPrefix(names[0], "eeee5555#1 ")) || offset > 0 && len(names) > 0 {
			t.Errorf("offset %d: %q", offset, names)
		}
	}
}

func TestWordsOfANewerAsqAreLeftAlone(t *testing.T) {
	h := fixture.Write(t)
	cfg := h.Config(t)
	a, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", cfg.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count := func(q string) (n int) {
		t.Helper()
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A later asq opens the index while this one has it open: it makes the words table
	// anew, its own way, and names every turn for its next sync to write
	for _, q := range []string{
		`UPDATE meta SET value = '99' WHERE key = 'words'`,
		`INSERT OR IGNORE INTO words_stale (sid, idx) SELECT sid, idx FROM turns`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	queued := count(`SELECT count(*) FROM words_stale`)
	if queued == 0 {
		t.Fatal("nothing queued")
	}
	// This one leaves those turns to it, whether it reads anything or not
	for _, o := range []index.SyncOptions{{}, {Rebuild: true}} {
		if _, err := a.Sync(ctx, o); err != nil {
			t.Fatal(err)
		}
		if n := count(`SELECT count(*) FROM words_stale`); n != queued {
			t.Errorf("a sync (%+v) of the older asq left %d of the %d turns the newer one is to write", o, n, queued)
		}
	}
	a.Close()

	// … and opened again, it leaves the table as it is, and searches it
	a, err = app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	f := filter(a)
	f.Agents = true
	if got, _ := a.Index.Search(ctx, index.ParseQuery("rotating"), f, index.SearchOptions{}); got.Total != 1 {
		t.Errorf("rotating in the newer table: %d", got.Total)
	}
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'words'`).Scan(&v); err != nil || v != "99" || count(`SELECT count(*) FROM words_stale`) != queued {
		t.Errorf("the words table is %q, %v, with %d turns queued: the newer asq's, as it left them", v, err, count(`SELECT count(*) FROM words_stale`))
	}
}
