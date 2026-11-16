package index

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A report over a window reads the window, and a process that reads one store reads
// that store's rows. Which of the two drives a query is the store term's to decide: it
// steps aside only when it names every store the index holds, for a query the time
// should drive (see Filter.where). These are the plans, as the index makes them.
func TestAQueryReadsWhatItAsksForAndNotTheWholeIndex(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	every := []string{"claude", "codex", "gemini", "opencode", "grok", "antigravity", "pi", "dsh", "hermes", "openclaw"}
	week := Window{Since: time.Now().Add(-7 * 24 * time.Hour)}
	plan := func(query string, args ...any) string {
		t.Helper()
		rows, err := ix.ro.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
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
	digest := func(f Filter, w Window) string {
		t.Helper()
		where, args := ix.widen(ctx, reportFilter(f), !w.Since.IsZero()).where()
		when, whenArgs := w.where("t.started_at")
		return plan(digestTurns(where, when), append(append([]any{}, args...), whenArgs...)...)
	}
	list := func(f Filter) string {
		t.Helper()
		where, args := ix.widen(ctx, f, true).where()
		return plan(`SELECT s.sid FROM sessions s WHERE `+where+` ORDER BY s.updated_at DESC LIMIT 50`, args...)
	}
	usage := func(f Filter, w Window) string {
		t.Helper()
		where, args := ix.widen(ctx, reportFilter(f), !w.Since.IsZero()).where()
		when, whenArgs := w.where("u.at")
		return plan(`SELECT u.model, sum(u.tok_in) FROM usage u JOIN sessions s ON s.sid = u.sid WHERE `+where+` AND `+when+` GROUP BY u.model`,
			append(append([]any{}, args...), whenArgs...)...)
	}

	// The index holds two stores, and the process reads both
	for _, source := range []string{"claude", "codex"} {
		if _, err := ix.rw.Exec(`INSERT INTO sessions (source, agent, key, stamp, id) VALUES (?, ?, ?, '', ?)`, source, source, source+"-1", source+"-1"); err != nil {
			t.Fatal(err)
		}
	}
	ix.recount()

	if got := digest(Filter{Stores: every}, week); !strings.Contains(got, "COVERING INDEX turns_report") {
		t.Errorf("a week's digest does not read the week from the index made for it:\n%s", got)
	}
	if got := list(Filter{Stores: every}); !strings.Contains(got, "sessions_updated") || strings.Contains(got, "TEMP B-TREE FOR ORDER BY") {
		t.Errorf("the list of sessions is sorted instead of read in order:\n%s", got)
	}
	// All time, the store term drives as it always did: a window that is everything is
	// not narrowed by the index on the time
	if got := usage(Filter{Stores: every}, Window{}); !strings.Contains(got, "sessions_parent") {
		t.Errorf("all-time usage does not start from the stores:\n%s", got)
	}

	// A process that reads one of them — ASQ_ONLY=codex in an MCP server's environment,
	// beside a server that indexes everything — reads that store's rows
	only := Filter{Stores: []string{"codex"}}
	for name, got := range map[string]string{"a week's digest": digest(only, week), "a week's usage": usage(only, week), "the list": list(only)} {
		if !strings.Contains(got, "sessions_parent") {
			t.Errorf("%s, for one store of two, does not start from that store:\n%s", name, got)
		}
	}

	// A store this process has not heard of turns up — another process indexed it — and
	// the store term is the narrowest thing again
	if _, err := ix.rw.Exec(`INSERT INTO sessions (source, agent, key, stamp, id) VALUES ('pi@box2', 'pi', 'k', '', 'p-1')`); err != nil {
		t.Fatal(err)
	}
	ix.recount()
	if got := digest(Filter{Stores: every}, week); strings.Contains(got, "turns_report") {
		t.Errorf("with a store in the index this process does not read, the week is read for every store:\n%s", got)
	}
}
