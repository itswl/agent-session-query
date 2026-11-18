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
		{"ECONNREFUSED 数据库 -orders", nil, nil, nil,
			"in all its forms: #1 says order, #2 orders"},
		{"flaky -ECONNREFUSED -数据库", nil, nil, nil,
			"one word, which the exclusions leave nowhere: nothing to come close with"},
		{"ECONNREFUSED flaky map order sorting keys 数据库", []string{"aaaa1111#1 lacks 数据库"}, &index.Partial{Words: 7, Held: 6}, nil,
			"all the words but one"},
		// Eight words, the best turns holding three: past the ways of choosing six that are
		// spelled out, the turns holding any are taken and ordered by how many they hold
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

	// Many words: the turns holding three first, then those holding two
	res, _ := a.Index.Search(ctx, index.ParseQuery("ECONNREFUSED sorting map 数据库 indexes tables rotate finished"), f, index.SearchOptions{PerSession: -1})
	var held []int
	for _, h := range res.Hits {
		held = append(held, 8-len(h.Lacks))
	}
	if !reflect.DeepEqual(held, []int{3, 3, 2, 2}) {
		t.Errorf("many words: the hits hold %v of them, want the most first: %q", held, hitNames(res))
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
	// #2, the later and shorter turn, holds flaky in the session's title alone: the turn
	// whose own words hold it comes first
	if names := hitNames(res); len(names) != 2 || names[0] != "aaaa1111#1" {
		t.Errorf("flaky = %q, want the turn that says it first", names)
	}

	// … however long that turn is, and however little the other: a word is weighed
	// against the length of the whole turn, and the title is in every one
	const id = "cccc3333-0000-4000-8000-000000000003"
	at := time.Now().Add(-time.Hour).UTC()
	row := func(s int, v map[string]any) string {
		v["sessionId"], v["uuid"], v["timestamp"] = id, "r"+strconv.Itoa(s), at.Add(time.Duration(s)*time.Second).Format(time.RFC3339Nano)
		raw, _ := json.Marshal(v)
		return string(raw) + "\n"
	}
	say := func(s int, role string, content any) string {
		return row(s, map[string]any{"type": role, "cwd": "/work/gamma", "message": map[string]any{"role": role, "content": content, "id": "m" + strconv.Itoa(s)}})
	}
	long := strings.Repeat("a line of a long build log with nothing in it\n", 300) + "then the key was rotated\n"
	text := `{"type":"ai-title","aiTitle":"Rotate the signing key"}` + "\n" +
		say(1, "user", "do the thing") +
		say(2, "assistant", []map[string]any{{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "make all"}}}) +
		say(3, "user", []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": long}}) +
		say(4, "assistant", []map[string]any{{"type": "text", "text": "Done."}}) +
		say(5, "user", "thanks") +
		say(6, "assistant", []map[string]any{{"type": "text", "text": "You are welcome."}})
	h := fixture.Write(t)
	path := filepath.Join(h.Dir, "-work-gamma", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := app.Open(h.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Sync(context.Background(), index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	res, _ = b.Index.Search(context.Background(), index.ParseQuery("rotate signing"), filter(b), index.SearchOptions{})
	if names := hitNames(res); len(names) != 2 || names[0] != "cccc3333#1" {
		t.Errorf("rotate signing = %q, want the long turn whose output says it first", names)
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
		"title:flaky":     2,
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
