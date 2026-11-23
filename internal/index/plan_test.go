package index

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// A report over a window reads the window, and a process that reads one store reads
// that store's rows. Which of the two drives a query is the store term's to decide: it
// steps aside when the stores it names are most of the index, for a query the time
// should drive (see Index.widen). These are the plans, as the index makes them.
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

	projects := func(f Filter) string {
		t.Helper()
		query, args := ix.projectsQuery(ctx, f)
		return plan(query, args...)
	}
	add := func(source string, n int) {
		t.Helper()
		for i := range n {
			key := fmt.Sprintf("%s-%d", source, i)
			if _, err := ix.rw.Exec(`INSERT INTO sessions (source, agent, key, stamp, id) VALUES (?, ?, ?, '', ?)`, source, strings.Split(source, "@")[0], key, key); err != nil {
				t.Fatal(err)
			}
		}
		ix.recount()
	}

	// The index holds two stores, and the process reads both
	add("claude", 30)
	add("codex", 10)

	if got := digest(Filter{Stores: every}, week); !strings.Contains(got, "COVERING INDEX turns_report") {
		t.Errorf("a week's digest does not read the week from the index made for it:\n%s", got)
	}
	if got := list(Filter{Stores: every}); !strings.Contains(got, "sessions_updated") || strings.Contains(got, "TEMP B-TREE FOR ORDER BY") {
		t.Errorf("the list of sessions is sorted instead of read in order:\n%s", got)
	}
	if got := projects(Filter{Stores: every, Since: week.Since}); !strings.Contains(got, "sessions_updated") {
		t.Errorf("a week's projects do not read the week:\n%s", got)
	}
	// All time, the store term drives as it always did: a window that is everything is
	// not narrowed by the index on the time
	if got := usage(Filter{Stores: every}, Window{}); !strings.Contains(got, "sessions_parent") {
		t.Errorf("all-time usage does not start from the stores:\n%s", got)
	}

	// A process that reads the lesser of them — ASQ_ONLY=codex in an MCP server's
	// environment, beside a server that indexes everything — reads that store's rows
	only := Filter{Stores: []string{"codex"}}
	for name, got := range map[string]string{"a week's digest": digest(only, week), "a week's usage": usage(only, week), "the list": list(only)} {
		if !strings.Contains(got, "sessions_parent") {
			t.Errorf("%s, for the lesser store of two, does not start from that store:\n%s", name, got)
		}
	}

	// A store this process does not read turns up — another process indexed it, once —
	// and while it is the lesser part of the index the week is still read from the time
	add("pi@box2", 5)
	if got := digest(Filter{Stores: every}, week); !strings.Contains(got, "turns_report") {
		t.Errorf("one small store this process does not read sent a week's digest through every session:\n%s", got)
	}
	// Once the stores this process reads are the lesser part, they drive
	add("pi@box3", 40)
	if got := digest(Filter{Stores: every}, week); strings.Contains(got, "turns_report") {
		t.Errorf("the stores this process reads are the lesser part of the index, and the week is read for every store:\n%s", got)
	}
}

// A search, and the files report, take of each turn they find which it is and when, and
// read that from turns_at rather than from the turn's row, which holds its text: a common
// word or a path most turns name would otherwise read most of the index. The planner takes
// the table's own key for a lookup by sid and idx unless the index is named.
func TestASearchReadsTheTurnsTimesFromTheirIndex(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
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
	where, args := Filter{Stores: []string{"claude"}}.where()
	search := plan(`SELECT t.sid, t.idx, bm25(words), t.started_at, t.last_at`+wordsFrom+` WHERE words MATCH ? AND `+where+` ORDER BY 3 LIMIT 10`,
		append([]any{"error"}, args...)...)
	if !strings.Contains(search, "COVERING INDEX turns_at") {
		t.Errorf("a search reads each turn it finds from the turn's row:\n%s", search)
	}
	files := plan(`SELECT count(*)`+filesFrom+where+` AND instr(lower(x.path), lower(?)) > 0`, append(args, "internal")...)
	if !strings.Contains(files, "COVERING INDEX turns_at") {
		t.Errorf("the files report counts its turns from their rows:\n%s", files)
	}
}

// A sync that changes which stores the index holds is a change in what the plans should
// be, at once and not within the minute the counts are kept.
func TestASyncRecountsTheStores(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	codex := []string{"codex"}
	if !ix.mostly(ctx, codex) {
		t.Fatal("an empty index: any store is most of it")
	}
	if _, err := ix.Sync(ctx, []Store{{Name: "claude", Spec: sources.Spec{Name: "claude"}, Source: many{n: 3}}}, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if ix.mostly(ctx, codex) {
		t.Error("after a sync brought in another store, the counts of the empty index were still used")
	}
}

// many is a store of n sessions of one message each.
// many is a store of n sessions, all at one stamp: "1" unless told otherwise.
type many struct {
	n     int
	stamp string
}

func (many) Location() string { return "test" }
func (many) Exists() bool     { return true }
func (m many) Scan(context.Context) ([]sources.Ref, error) {
	refs := make([]sources.Ref, m.n)
	for i := range refs {
		stamp := m.stamp
		if stamp == "" {
			stamp = "1"
		}
		refs[i] = sources.Ref{Key: fmt.Sprint(i), Stamp: stamp, ModTime: time.Now()}
	}
	return refs, nil
}
func (many) Read(_ context.Context, ref sources.Ref, w *transcript.Writer) error {
	w.S.ID = "s" + ref.Key
	w.Add(transcript.Message{Role: transcript.User, At: time.Now(), Blocks: []transcript.Block{{Type: transcript.Text, Text: "hello"}}})
	return nil
}
