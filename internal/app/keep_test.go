package app_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/keep"
)

// keeping opens the fixture with a state directory of its own, where what is kept goes.
func keeping(t *testing.T) (*app.App, fixture.History, string) {
	t.Helper()
	state := t.TempDir()
	h := fixture.Write(t)
	cfg := h.Config(t)
	cfg.State = state
	a, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	sync(t, a)
	return a, h, filepath.Join(state, "kept", "claude")
}

func listed(t *testing.T, a *app.App) string {
	t.Helper()
	list, _, err := a.Index.Sessions(t.Context(), index.Filter{Stores: a.Stores()}, index.Page{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range list {
		out = append(out, s.Source+"/"+s.ID[:4])
	}
	return strings.Join(out, " ")
}

func tokens(t *testing.T, a *app.App) int64 {
	t.Helper()
	rows, err := a.Index.Usage(t.Context(), index.Filter{Stores: a.Stores()}, index.Window{}, index.ByAll, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("usage = %v, %v", rows, err)
	}
	return rows[0].Total
}

func sync(t *testing.T, a *app.App) index.SyncResult {
	t.Helper()
	res, err := a.Sync(t.Context(), index.SyncOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A process given no state directory keeps nothing, and says so when asked to.
func TestNoStateNoKeeping(t *testing.T) {
	a, _ := fixture.Open(t)
	if _, err := a.KeepAll(t.Context(), true); err == nil || !strings.Contains(err.Error(), "no state directory") {
		t.Errorf("keep all without a state directory = %v", err)
	}
}

// Nothing is copied anywhere until a person asks.
func TestNothingIsKeptUnlessAsked(t *testing.T) {
	a, _, kept := keeping(t)
	sync(t, a)
	if _, err := os.Stat(filepath.Dir(kept)); !os.IsNotExist(err) {
		t.Errorf("a folder of copies with nothing asked: %v", err)
	}
	ks, err := a.KeepStatus(t.Context())
	if err != nil || ks.All || len(ks.Sessions) != 0 || ks.Files != 0 || strings.Join(ks.Of, ",") != "Claude Code,Gemini CLI" {
		t.Errorf("status = %+v, %v", ks, err)
	}
}

// Every session of a CLI that deletes its own is copied, each file to the same path in the
// folder, with its time. While the CLI holds a session, the copy is not read; once the CLI
// deleted it, the copy is the session, and nothing of what the history adds up to moves.
func TestKeepAllKeepsWhatTheCLIDeletes(t *testing.T) {
	a, h, kept := keeping(t)
	ctx := t.Context()
	before, tok := listed(t, a), tokens(t, a)
	copied, err := a.KeepAll(ctx, true)
	if err != nil || copied.Transcripts != 3 || copied.Bytes == 0 {
		t.Fatalf("keep all = %+v, %v; want the two sessions and the subagent copied", copied, err)
	}
	for _, rel := range []string{
		filepath.Join("-work-alpha", fixture.Alpha+".jsonl"),
		filepath.Join("-work-alpha", fixture.Alpha, "subagents", "agent-"+fixture.Agent+".jsonl"),
		filepath.Join("-work-alpha", fixture.Alpha, "subagents", "agent-"+fixture.Agent+".meta.json"),
		filepath.Join("-work-beta", fixture.Beta+".jsonl"),
	} {
		live, err1 := os.Stat(filepath.Join(h.Dir, rel))
		copy, err2 := os.Stat(filepath.Join(kept, rel))
		if err1 != nil || err2 != nil || copy.Size() != live.Size() || !copy.ModTime().Equal(live.ModTime()) {
			t.Errorf("%s: copy %v, %v; live %v, %v", rel, copy, err2, live, err1)
		}
	}
	// The copies of what the CLI holds are not read, and the list is as it was
	if res := sync(t, a); res.Read != 0 {
		t.Errorf("the sync after copying read %d transcripts", res.Read)
	}
	if got := listed(t, a); got != before {
		t.Errorf("listed %q, want %q", got, before)
	}

	// The CLI deletes beta
	if err := os.Remove(filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl")); err != nil {
		t.Fatal(err)
	}
	sync(t, a)
	if got := listed(t, a); got != "claude/aaaa claude@kept/bbbb" {
		t.Errorf("after the CLI deleted beta: %q", got)
	}
	if got := tokens(t, a); got != tok {
		t.Errorf("tokens %d, want %d as before", got, tok)
	}
	res, _ := a.Index.Search(ctx, index.ParseQuery("rotate"), index.Filter{Stores: a.Stores()}, index.SearchOptions{})
	if res.Total != 1 {
		t.Errorf("search finds beta %d times", res.Total)
	}
	s, _, err := a.Resolve(ctx, "claude/"+fixture.Beta[:8])
	if err != nil || s.Source != "claude@kept" || s.Resume != "" || s.ResumeThere != "" || !strings.Contains(app.NoResume(&s), "Claude Code has deleted it") {
		t.Errorf("beta = %s, resume %q there %q (%q), %v", s.Ref, s.Resume, s.ResumeThere, app.NoResume(&s), err)
	}
	ks, err := a.KeepStatus(ctx)
	if err != nil || !ks.All || len(ks.Stores) != 1 || ks.Stores[0].Source != "claude@kept" || ks.Stores[0].Transcripts != 3 || ks.Stores[0].Gone != 1 || ks.Files != 4 || ks.Bytes == 0 {
		t.Errorf("status = %+v, %v", ks, err)
	}

	// Off: what is kept stays, and is still read
	if _, err := a.KeepAll(ctx, false); err != nil {
		t.Fatal(err)
	}
	sync(t, a)
	if got := listed(t, a); got != "claude/aaaa claude@kept/bbbb" {
		t.Errorf("with keeping off: %q", got)
	}
}

// A session being written is copied once it has rested a while, not at every line it
// gains; then it is copied as it is.
func TestAKeptSessionIsCopiedOnceItRests(t *testing.T) {
	a, h, kept := keeping(t)
	if _, err := a.KeepAll(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"user","uuid":"b-u9","timestamp":"` + time.Now().UTC().Format(time.RFC3339) + `","sessionId":"` + fixture.Beta + `","cwd":"/work/beta","message":{"role":"user","content":"and tell the team"}}` + "\n")
	f.Close()
	live, _ := os.Stat(path)
	copy := filepath.Join(kept, "-work-beta", fixture.Beta+".jsonl")
	sync(t, a)
	if got, _ := os.Stat(copy); got.Size() == live.Size() {
		t.Error("a transcript written a moment ago was copied at once")
	}
	rested := time.Now().Add(-keep.Settle - time.Minute)
	if err := os.Chtimes(path, rested, rested); err != nil {
		t.Fatal(err)
	}
	sync(t, a)
	live, _ = os.Stat(path) // its time as the filesystem keeps it
	if got, _ := os.Stat(copy); got.Size() != live.Size() || !got.ModTime().Equal(live.ModTime()) {
		t.Errorf("once it rested, the copy is %d bytes at %v; want %d at %v", got.Size(), got.ModTime(), live.Size(), live.ModTime())
	}
}

// A session is kept one by one, its subagents with it, and copied at once. No longer kept,
// its copy goes while the CLI still holds the session; once the CLI deleted it, the copy is
// all there is of it, and stays.
func TestKeepOneSession(t *testing.T) {
	a, h, kept := keeping(t)
	ctx := t.Context()
	alpha, _, _ := a.Resolve(ctx, fixture.Alpha)
	k, err := a.KeepSession(ctx, &alpha, true)
	if err != nil || k.Transcripts != 2 {
		t.Fatalf("keep alpha = %+v, %v; want it and its subagent", k, err)
	}
	st, _ := keep.Load(filepath.Dir(filepath.Dir(kept)))
	if !slices.Equal(st.Sessions, []string{"claude/" + fixture.Alpha}) || st.All {
		t.Errorf("settings = %+v", st)
	}
	alphaCopy := filepath.Join(kept, "-work-alpha", fixture.Alpha+".jsonl")
	if _, err := os.Stat(alphaCopy); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(kept, "-work-alpha", fixture.Alpha, "subagents", "agent-"+fixture.Agent+".meta.json")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(kept, "-work-beta")); !os.IsNotExist(err) {
		t.Errorf("beta was copied too: %v", err)
	}

	// No longer kept, while the CLI holds it: the copy goes
	if k, err := a.KeepSession(ctx, &alpha, false); err != nil || k.Stays != "" {
		t.Errorf("stop keeping alpha = %+v, %v", k, err)
	}
	if _, err := os.Stat(alphaCopy); !os.IsNotExist(err) {
		t.Errorf("the copy of a session the CLI holds stays: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kept, "-work-alpha")); !os.IsNotExist(err) {
		t.Errorf("the folders the copy was in stay: %v", err)
	}

	// Kept, then deleted by the CLI: the copy is the session, and stays when it is no longer kept
	beta, _, _ := a.Resolve(ctx, fixture.Beta)
	if _, err := a.KeepSession(ctx, &beta, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl")); err != nil {
		t.Fatal(err)
	}
	sync(t, a)
	beta, _, _ = a.Resolve(ctx, fixture.Beta)
	if beta.Source != "claude@kept" {
		t.Fatalf("beta = %s", beta.Ref)
	}
	if k, err := a.KeepSession(ctx, &beta, false); err != nil || k.Stays != "gone" {
		t.Errorf("stop keeping a session the CLI deleted = %+v, %v", k, err)
	}
	if _, err := os.Stat(filepath.Join(kept, "-work-beta", fixture.Beta+".jsonl")); err != nil {
		t.Errorf("the only copy of a session went: %v", err)
	}

	// What is not a session asq keeps says so
	agent, _, _ := a.Resolve(ctx, "claude/"+fixture.Agent)
	if _, err := a.KeepSession(ctx, &agent, true); err == nil || !strings.Contains(err.Error(), "subagent") {
		t.Errorf("keep a subagent = %v", err)
	}
	if _, err := a.KeepSession(ctx, &beta, true); err == nil || !strings.Contains(err.Error(), "keeps already") {
		t.Errorf("keep a kept copy = %v", err)
	}
}

// The folder the copies go in can be named, but not while the one before holds copies:
// they would no longer be read.
func TestKeepInAFolderOfOnesOwn(t *testing.T) {
	a, _, kept := keeping(t)
	ctx := t.Context()
	elsewhere := filepath.Join(t.TempDir(), "asq-kept")
	if err := a.KeepIn(elsewhere); err != nil {
		t.Fatal(err)
	}
	if _, err := a.KeepAll(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "claude", "-work-beta", fixture.Beta+".jsonl")); err != nil {
		t.Errorf("not kept in the folder named: %v", err)
	}
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Errorf("kept in the state directory as well: %v", err)
	}
	if err := a.KeepIn(""); err == nil || !strings.Contains(err.Error(), elsewhere) {
		t.Errorf("leaving a folder that holds copies = %v; want it refused, naming the folder", err)
	}
}
