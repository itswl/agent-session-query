// Package app is the program without a face: the index, the stores it is kept in step
// with, and the handful of operations every way in — the command line, the HTTP API,
// the MCP server, the page — is a rendering of.
package app

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/txt"
)

// Version is the release this binary is, set at build time.
var Version = "dev"

// Config says where the index is and which stores to read.
type Config struct {
	IndexPath string // empty: the default
	Stores    catalog.Config
}

// App is the open program.
type App struct {
	Index *index.Index
	Set   *catalog.Set

	mu      sync.Mutex
	running chan struct{} // closed when the sync in flight ends; nil when none is
	began   time.Time     // when the sync in flight began
	last    time.Time     // when the last sync ended
	lastErr error
}

// Open opens the index and resolves the stores.
func Open(cfg Config) (*App, error) {
	set, err := catalog.New(cfg.Stores, catalog.Env())
	if err != nil {
		return nil, err
	}
	path := cfg.IndexPath
	if path == "" {
		path = index.DefaultPath()
	}
	ix, err := index.Open(path)
	if err != nil {
		return nil, err
	}
	return &App{Index: ix, Set: set}, nil
}

// Close releases the index.
func (a *App) Close() error { return a.Index.Close() }

// Stores names the stores this process reads. Every query is confined to them: an index
// file can hold what another configuration put there.
func (a *App) Stores() []string { return a.Set.Names() }

// Sync brings the index in step with the stores now. A sync already in flight is waited
// for instead of joined by a second one.
func (a *App) Sync(ctx context.Context, opts index.SyncOptions) (index.SyncResult, error) {
	a.mu.Lock()
	for a.running != nil {
		wait := a.running
		a.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return index.SyncResult{}, ctx.Err()
		}
		a.mu.Lock()
	}
	done := a.claim()
	a.mu.Unlock()
	return a.run(ctx, opts, done)
}

// claim marks a sync as under way, from now. a.mu must be held, and no sync running.
func (a *App) claim() chan struct{} {
	done := make(chan struct{})
	a.running, a.began = done, time.Now()
	return done
}

// run is the sync a claim was made for.
func (a *App) run(ctx context.Context, opts index.SyncOptions, done chan struct{}) (index.SyncResult, error) {
	res, err := a.Index.Sync(ctx, a.Set.Present(), opts)
	a.mu.Lock()
	a.running, a.last, a.lastErr = nil, time.Now(), err
	a.mu.Unlock()
	close(done)
	return res, err
}

// Fresh makes sure the index is as near to the stores as a question needs, and reports
// whether it is. That is one of three things:
//
//   - The stores were read no longer ago than maxAge. Nothing to do.
//   - They are being read now, and that read began while the one before it was still
//     fresh: the index is being followed, by a watcher or by a caller that came just
//     before. What it holds is what the stores held a moment ago, and the question is
//     answered from that. It is not held up for a read somebody else is already doing —
//     a transcript of fifty megabytes takes seconds to read again, an agent at work
//     makes it due again every few of them, and nobody asking for a list of sessions is
//     waiting for that.
//   - Nobody has read them for a while. They are read now, and the question waits for
//     that, at most wait: a first read of a large history takes longer than any one
//     question should, and the question is then answered from what has been read so far.
//     If a read is already under way, it is the one waited for; a second is not queued
//     behind it.
func (a *App) Fresh(ctx context.Context, maxAge, wait time.Duration) bool {
	a.mu.Lock()
	running, began, last := a.running, a.began, a.last
	switch {
	case last.IsZero() && running != nil:
		// the first read is under way: it is the one waited for
	case running == nil && !last.IsZero() && time.Since(last) <= maxAge:
		a.mu.Unlock()
		return true
	case running != nil && began.Sub(last) <= maxAge:
		a.mu.Unlock()
		return true
	}
	if running == nil {
		// Claimed before the lock is let go, so that the questions arriving with this
		// one find it under way and wait for it, rather than each starting its own. The
		// sync outlives the question that started it: a first read is not thrown away
		// because one caller stopped waiting
		done := a.claim()
		running = done
		go func() { _, _ = a.run(context.WithoutCancel(ctx), index.SyncOptions{}, done) }()
	}
	a.mu.Unlock()
	if wait <= 0 {
		select {
		case <-running:
			return true
		case <-ctx.Done():
			return false
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-running:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// Watch keeps the index in step until ctx ends, looking at the stores every interval.
// A look that finds nothing changed costs a stat per transcript.
func (a *App) Watch(ctx context.Context, interval time.Duration) {
	for {
		if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "sync: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Query is a selection of sessions as a person or an agent states it.
type Query struct {
	Source   string
	Project  string
	Branch   string
	Text     string
	Since    string // 7d, 12h, 2026-09-01, an RFC 3339 time
	Until    string
	Archived bool
	Agents   bool
}

// Filter turns a query into the index's filter, confined to this process's stores.
func (a *App) Filter(q Query) (index.Filter, error) {
	f := index.Filter{Stores: a.Stores(), Source: q.Source, Project: q.Project, Branch: q.Branch,
		Text: q.Text, Archived: q.Archived, Agents: q.Agents}
	if q.Source != "" {
		if err := a.checkSource(q.Source); err != nil {
			return f, err
		}
	}
	var err error
	if f.Since, err = When(q.Since, time.Now()); err != nil {
		return f, fmt.Errorf("since: %w", err)
	}
	if f.Until, err = When(q.Until, time.Now()); err != nil {
		return f, fmt.Errorf("until: %w", err)
	}
	return f, nil
}

// Window is the time bounds of a query, for the reports that are bounded by when things
// happened rather than by when a session was last written.
func (a *App) Window(q Query, defaultSince string) (index.Window, error) {
	since := q.Since
	if since == "" {
		since = defaultSince
	}
	var w index.Window
	var err error
	if w.Since, err = When(since, time.Now()); err != nil {
		return w, fmt.Errorf("since: %w", err)
	}
	if w.Until, err = When(q.Until, time.Now()); err != nil {
		return w, fmt.Errorf("until: %w", err)
	}
	return w, nil
}

func (a *App) checkSource(name string) error {
	for _, store := range a.Stores() {
		agent, _, _ := strings.Cut(store, "@")
		if store == name || agent == name {
			return nil
		}
	}
	if _, ok := catalog.Find(name); ok {
		return nil // a CLI this build reads, though not one configured here: an empty answer, not an error
	}
	return fmt.Errorf("unknown source %q (one of: %s)", name, strings.Join(a.Stores(), ", "))
}

// When reads a moment as people write one: "7d", "36h", "90m" or "2w" ago; a date; a
// date and time; "all" or nothing for no bound.
func When(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "all" {
		return time.Time{}, nil
	}
	if n := len(s); n > 1 {
		if amount, err := strconv.ParseFloat(s[:n-1], 64); err == nil && amount >= 0 {
			unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[s[n-1]]
			if unit > 0 {
				return now.Add(-time.Duration(amount * float64(unit))), nil
			}
		}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unreadable time %q (want 7d, 12h, 2026-09-01 or 2026-09-01T08:00:00Z)", s)
}

// Resume is the command that reopens a session in the CLI that wrote it, with the cd it
// needs, or "" when its CLI has no such command.
//
// The session is found from anywhere — the CLIs resolve an id across working
// directories. What the directory decides is where the resumed agent then works: it
// inherits the one it is launched in, and anywhere but the session's own it carries on
// about files its tools can no longer reach.
func Resume(s *index.Session) string {
	if s.Parent != nil || s.ID == "" {
		return "" // a subagent is not a session a CLI reopens; its parent is
	}
	spec, ok := catalog.Spec(s.Source)
	if !ok || spec.Resume == "" {
		return ""
	}
	cmd := spec.Resume + " " + ShellArg(s.ID)
	if isAbsolute(s.Cwd) {
		cd := "cd "
		if runtime.GOOS == "windows" {
			cd = "cd /d " // or cmd stays on the drive it was on
		}
		cmd = cd + ShellArg(s.Cwd) + " && " + cmd
	}
	return cmd
}

// isAbsolute: a plain absolute path, Unix or Windows.
func isAbsolute(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	return len(p) >= 3 && (p[0]|0x20 >= 'a' && p[0]|0x20 <= 'z') && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// ShellArg quotes a value that is not plainly safe to paste into this machine's shell.
// Ids are normally uuids and pass through untouched; the quoting is there so that one
// carrying a space or a quote cannot turn a copied command into two.
func ShellArg(s string) string { return quoteArg(s, runtime.GOOS == "windows") }

// quoteArg quotes for sh, or for cmd.exe — where a single quote is just a character and
// only double quotes group.
func quoteArg(s string, windows bool) string {
	safe := s != ""
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_./=@+:", r), windows && r == '\\':
		default:
			safe = false
		}
	}
	switch {
	case safe:
		return s
	case windows:
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Decorate fills in what the index does not know about a session: how to resume it.
func Decorate(list []index.Session) []index.Session {
	for i := range list {
		list[i].Resume = Resume(&list[i])
	}
	return list
}

// quote is how much of an ask or an answer a row of a list carries. The whole of either
// is the session's own to give, when it is read.
const quote = 400

// Listed prepares sessions for a list: how each resumes, and its last ask and answer cut
// to a row's worth. A page of fifty sessions with every final answer whole is a quarter
// of a megabyte nobody reads.
func Listed(list []index.Session) []index.Session {
	for i := range list {
		list[i].Resume = Resume(&list[i])
		list[i].LastAsk = txt.Cap(list[i].LastAsk, quote)
		list[i].Answer = txt.Cap(list[i].Answer, quote)
	}
	return list
}

// ListedHits does the same for what a search found.
func ListedHits(res index.SearchResult) index.SearchResult {
	for i := range res.Hits {
		h := &res.Hits[i]
		h.Ask, h.Answer = txt.Cap(h.Ask, quote), txt.Cap(h.Answer, quote)
		h.Session.Resume = Resume(&h.Session)
		h.Session.LastAsk, h.Session.Answer = "", "" // the hit's own ask and answer are the ones that matter
	}
	return res
}
