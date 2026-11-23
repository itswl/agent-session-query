package index_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

// A search the filters keep from matching says how many turns would match without them,
// and which they are: an agent that narrowed its search as it was told would otherwise take
// "no turn holds it" for "it was never dealt with". Beta, three days old, alone says rotate;
// alpha, an hour old, says keys.
func TestASearchSaysWhatTheFiltersLeftOut(t *testing.T) {
	a, h := fixture.Open(t)
	ctx := context.Background()
	day := h.Now.Add(-24 * time.Hour)
	search := func(query string, f index.Filter) index.SearchResult {
		t.Helper()
		f.Stores, f.Agents = a.Stores(), true
		res, err := a.Index.Search(ctx, index.ParseQuery(query), f, index.SearchOptions{})
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return res
	}
	for _, c := range []struct {
		query string
		f     index.Filter
		hits  int
		want  *index.Outside
		why   string
	}{
		{"rotate", index.Filter{Since: day}, 0, &index.Outside{Turns: 1, Sessions: 1, Filters: []string{"since"}},
			"a word only a turn before the window holds"},
		{"rotate", index.Filter{Since: day, Project: "alpha"}, 0, &index.Outside{Turns: 1, Sessions: 1, Filters: []string{"project", "since"}},
			"each filter set is named"},
		{"rotate key", index.Filter{Since: day}, 1, &index.Outside{Turns: 1, Sessions: 1, Filters: []string{"since"}},
			"the closest turns within the window, and beta outside it, which holds both"},
		{"rotate flaky", index.Filter{Since: day}, 1, &index.Outside{Filters: []string{"since"}},
			"no turn holds both, outside the window either: said, so that dropping it is not tried for nothing"},
		{"nothinglikethis", index.Filter{Since: day}, 0, &index.Outside{Filters: []string{"since"}},
			"a word no turn holds anywhere"},
		{"nothinglikethis", index.Filter{}, 0, nil,
			"nothing filtered, nothing to say"},
		{"rotate", index.Filter{}, 1, nil,
			"a search turns match whole"},
		{"rotate", index.Filter{Until: day, Branch: "release"}, 1, nil,
			"a search the filters let match"},
	} {
		res := search(c.query, c.f)
		if len(res.Hits) != c.hits || !reflect.DeepEqual(res.Outside, c.want) {
			t.Errorf("%s: %q: %d hits, outside %+v; want %d, %+v", c.why, c.query, len(res.Hits), res.Outside, c.hits, c.want)
		}
	}

	// A search of one session, which is how the page's Find asks, is of that session alone
	alpha, err := a.Index.Resolve(ctx, fixture.Alpha, a.Stores())
	if err != nil {
		t.Fatal(err)
	}
	if res := search("rotate", index.Filter{SID: alpha.SID()}); res.Total != 0 || res.Outside != nil {
		t.Errorf("a search of one session = %d, outside %+v", res.Total, res.Outside)
	}

	// Beta archived, as Codex archives a session: a search leaves it out unless asked, and
	// says that it holds the word
	db, err := sql.Open("sqlite", a.Index.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE sessions SET archived = 1 WHERE id = ?`, fixture.Beta); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		query string
		f     index.Filter
		hits  int
		want  *index.Outside
	}{
		{"rotate", index.Filter{}, 0, &index.Outside{Turns: 1, Sessions: 1, Archived: true}},
		{"rotate", index.Filter{Since: day}, 0, &index.Outside{Turns: 1, Sessions: 1, Filters: []string{"since"}, Archived: true}},
		{"rotate", index.Filter{Since: day, Archived: true}, 0, &index.Outside{Turns: 1, Sessions: 1, Filters: []string{"since"}}},
		{"rotate", index.Filter{Archived: true}, 1, nil},
	} {
		res := search(c.query, c.f)
		if len(res.Hits) != c.hits || !reflect.DeepEqual(res.Outside, c.want) {
			t.Errorf("beta archived: %q %+v: %d hits, outside %+v; want %d, %+v", c.query, c.f, len(res.Hits), res.Outside, c.hits, c.want)
		}
	}
}
