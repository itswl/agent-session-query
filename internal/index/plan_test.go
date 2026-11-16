package index

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A report over a window reads the window. The filter that confines a query to the
// stores this process reads names, as a rule, every session there is; taken for a narrow
// search it starts the query from the sessions and walks all their turns, text and all,
// to find last week's. These are the plans, as the index makes them.
func TestAReportReadsItsWindowAndNotTheWholeIndex(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	f := reportFilter(Filter{Stores: []string{"claude", "codex", "gemini", "opencode", "grok", "antigravity", "pi", "dsh", "hermes", "openclaw"}})
	where, args := f.where()
	week := Window{Since: time.Now().Add(-7 * 24 * time.Hour)}
	plan := func(query string, args ...any) string {
		t.Helper()
		rows, err := ix.ro.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, detail)
		}
		return strings.Join(lines, "\n")
	}

	when, whenArgs := week.where("t.started_at")
	got := plan(digestTurns(where, when), append(append([]any{}, args...), whenArgs...)...)
	if !strings.Contains(got, "COVERING INDEX turns_report") {
		t.Errorf("a week's digest does not read the week from the index made for it:\n%s", got)
	}
	// The newest sessions come off the index that orders them; they are not all fetched and sorted
	got = plan(`SELECT s.sid FROM sessions s WHERE `+where+` ORDER BY s.updated_at DESC LIMIT 50`, args...)
	if !strings.Contains(got, "sessions_updated") || strings.Contains(got, "TEMP B-TREE FOR ORDER BY") {
		t.Errorf("the list of sessions is sorted instead of read in order:\n%s", got)
	}
}
