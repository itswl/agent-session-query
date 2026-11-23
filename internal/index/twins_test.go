package index_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

// twinsOpen opens the fixture's store and a copy of it beside it, labelled kept.
func twinsOpen(t *testing.T) (*app.App, fixture.History, string) {
	t.Helper()
	h := fixture.Write(t)
	kept := filepath.Join(t.TempDir(), "kept")
	h.Copy(t, kept)
	cfg := h.Config(t)
	cfg.Stores = catalog.Config{Only: []string{"claude"}, Paths: []catalog.Path{{Agent: "claude", Dir: h.Dir}, {Agent: "claude", Label: "kept", Dir: kept}}}
	a, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if _, err := a.Sync(t.Context(), index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	return a, h, kept
}

// what is everything the history adds up to, as the list, the reports and a search count it.
type what struct {
	sessions, failures, hits int
	tokens                   int64
	sources                  string
}

func adds(t *testing.T, a *app.App) what {
	t.Helper()
	ctx := t.Context()
	f := filter(a)
	list, n, err := a.Index.Sessions(ctx, f, index.Page{})
	if err != nil {
		t.Fatal(err)
	}
	var w what
	w.sessions = n
	for _, s := range list {
		w.sources += s.Source + "/" + s.ID[:4] + " "
	}
	rows, err := a.Index.Usage(ctx, f, index.Window{}, index.ByAll, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		w.tokens += r.Total
	}
	fails, err := a.Index.Failures(ctx, f, index.Window{}, index.FailureQuery{}, index.Page{})
	if err != nil {
		t.Fatal(err)
	}
	w.failures = fails.Total
	res, err := a.Index.Search(ctx, index.ParseQuery("rotate"), f, index.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w.hits = res.Total
	return w
}

// A store and a copy of it beside it — a backup added by name — hold every session twice.
// Each is counted once, as the CLI's own store holds it; and once the CLI deleted it, as
// the copy holds it, with nothing lost from what the history adds up to.
func TestASessionTwoStoresHoldIsCountedOnce(t *testing.T) {
	h := fixture.Write(t)
	alone, err := app.Open(h.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer alone.Close()
	if _, err := alone.Sync(t.Context(), index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	want := adds(t, alone)

	a, h, _ := twinsOpen(t)
	ctx := t.Context()
	got := adds(t, a)
	if got.sessions != want.sessions || got.tokens != want.tokens || got.failures != want.failures || got.hits != want.hits {
		t.Fatalf("with a copy of the store beside it = %+v, want %+v", got, want)
	}
	if got.sources != "claude/aaaa claude/bbbb " {
		t.Errorf("listed as %q, want each session as the CLI's own store holds it", got.sources)
	}
	byStore, _ := a.Index.Usage(ctx, filter(a), index.Window{}, index.BySource, 0)
	if len(byStore) != 1 || byStore[0].Key != "claude" {
		t.Errorf("usage by store = %+v, want claude alone", byStore)
	}
	// and the health report, which the page's store picker counts from, says the same
	if got := listedBy(t, a); got != "claude 2, claude@kept 0" {
		t.Errorf("sessions by store in the health report = %s, want them as the list has them", got)
	}
	// A ref to the copy is the session it is a copy of
	s, err := a.Index.Resolve(ctx, "claude@kept/"+fixture.Beta[:8], a.Stores())
	if err != nil || s.Source != "claude" {
		t.Errorf("claude@kept/%s = %s, %v; want the session in claude", fixture.Beta[:8], s.Ref, err)
	}
	// A subagent's transcript is counted once too, beneath the session that counts
	alpha, _ := a.Index.Resolve(ctx, fixture.Alpha, a.Stores())
	kids, _ := a.Index.Children(ctx, &alpha)
	if alpha.Agents != 1 || len(kids) != 1 || kids[0].Source != "claude" {
		t.Errorf("alpha has %d agents, children %v; want its one subagent, as claude holds it", alpha.Agents, refs(kids))
	}

	// The CLI deletes a session: the copy is the session now, and the totals do not move
	if err := os.Remove(filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	got = adds(t, a)
	if got.sessions != want.sessions || got.tokens != want.tokens || got.failures != want.failures || got.hits != want.hits {
		t.Fatalf("after the CLI deleted beta = %+v, want %+v", got, want)
	}
	if got.sources != "claude/aaaa claude@kept/bbbb " {
		t.Errorf("listed as %q, want beta as the copy holds it", got.sources)
	}
	if got := listedBy(t, a); got != "claude 1, claude@kept 1" {
		t.Errorf("sessions by store in the health report, after the CLI deleted beta = %s", got)
	}
	if s, err := a.Index.Resolve(ctx, "claude/"+fixture.Beta[:8], a.Stores()); err != nil || s.Source != "claude@kept" {
		t.Errorf("claude/%s = %s, %v; want the copy", fixture.Beta[:8], s.Ref, err)
	}
}

// listedBy is how many sessions the health report says each store has listed.
func listedBy(t *testing.T, a *app.App) string {
	t.Helper()
	h, err := a.Health(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var by []string
	for _, st := range h.Stores {
		by = append(by, fmt.Sprintf("%s %d", st.Source, st.Sessions))
	}
	return strings.Join(by, ", ")
}

// The CLI deletes a session's transcript and leaves its subagents': they hang beneath what
// another store keeps of the session, and are counted once.
func TestSubagentsHangBeneathTheSessionAnotherStoreKeeps(t *testing.T) {
	a, h, _ := twinsOpen(t)
	ctx := t.Context()
	if err := os.Remove(filepath.Join(h.Dir, "-work-alpha", fixture.Alpha+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	alpha, err := a.Index.Resolve(ctx, fixture.Alpha, a.Stores())
	if err != nil || alpha.Source != "claude@kept" {
		t.Fatalf("alpha = %s, %v", alpha.Ref, err)
	}
	kids, _ := a.Index.Children(ctx, &alpha)
	if alpha.Agents != 1 || len(kids) != 1 {
		t.Errorf("alpha has %d agents, children %v; want its one subagent", alpha.Agents, refs(kids))
	}
	all, _, _ := a.Index.Sessions(ctx, index.Filter{Stores: a.Stores(), Agents: true}, index.Page{})
	if len(all) != 3 {
		t.Errorf("with the subagents = %s, want alpha, its agent and beta once each", refs(all))
	}
}

// An index whose sessions an older asq linked has them linked again at the next sync,
// though that sync reads nothing: the copies are folded at once, not when a session moves.
func TestAnIndexLinkedByOlderRulesIsLinkedAgain(t *testing.T) {
	a, _, _ := twinsOpen(t)
	ctx := t.Context()
	// What an asq before the rule left: the copies not folded, and no word of the rules
	db, err := sql.Open("sqlite", a.Index.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET copy_of = 0; DELETE FROM meta WHERE key = 'link'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if n := adds(t, a).sessions; n != 4 {
		t.Fatalf("unfolded = %d sessions, want the four an older asq listed", n)
	}
	res, err := a.Sync(ctx, index.SyncOptions{})
	if err != nil || res.Read != 0 {
		t.Fatalf("sync = %+v, %v; want nothing read", res, err)
	}
	if n := adds(t, a).sessions; n != 2 {
		t.Errorf("after a sync that read nothing = %d sessions, want 2", n)
	}
}

// An index is shared by processes that may read different stores. One that reads the copy
// and not the store that counts the session has the copy stand for it, rather than see
// neither.
func TestACopyStandsForTheSessionWhenItsStoreIsNotRead(t *testing.T) {
	a, _, kept := twinsOpen(t)
	ctx := t.Context()
	cfg := app.Config{IndexPath: a.Index.Path(), Stores: catalog.Config{Only: []string{"codex"},
		Paths: []catalog.Path{{Agent: "codex", Dir: t.TempDir()}, {Agent: "claude", Label: "kept", Dir: kept}}}}
	other, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	got := adds(t, other)
	if got.sources != "claude@kept/aaaa claude@kept/bbbb " || got.failures != 1 || got.hits != 1 || got.tokens == 0 {
		t.Errorf("reading the copy alone = %+v", got)
	}
	if s, err := other.Index.Resolve(ctx, "claude/"+fixture.Beta[:8], other.Stores()); err != nil || s.Source != "claude@kept" {
		t.Errorf("claude/%s = %s, %v; want the copy, which is what this process reads", fixture.Beta[:8], s.Ref, err)
	}
	// The process that reads both still counts each once
	if got := adds(t, a); got.sources != "claude/aaaa claude/bbbb " {
		t.Errorf("reading both = %+v", got)
	}
}
