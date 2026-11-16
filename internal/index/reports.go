package index

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Window bounds a report in time. The bounds are the moments things happened — when a
// turn was asked, a call was made, a command failed — not when a session was last
// written: a session that ran for a week lands in each of its days.
type Window struct {
	Since time.Time
	Until time.Time
}

func (w Window) where(column string) (string, []any) {
	var conds []string
	var args []any
	if !w.Since.IsZero() {
		conds = append(conds, column+" >= ?")
		args = append(args, ms(w.Since))
	}
	if !w.Until.IsZero() {
		conds = append(conds, column+" <= ?")
		args = append(args, ms(w.Until))
	}
	if len(conds) == 0 {
		return "1 = 1", nil
	}
	return strings.Join(conds, " AND "), args
}

// reportFilter is a session filter for a report: the time bounds are the report's own,
// and everything that happened counts — a subagent's work, and the work of a session its
// CLI has since put away. What was spent last week does not change when a session is
// archived.
func reportFilter(f Filter) Filter {
	f.Since, f.Until = time.Time{}, time.Time{}
	f.Agents, f.Archived, f.Empty = true, true, true
	return f
}

// UsageRow is what one group of model calls consumed, in tokens. It carries no cost: a
// dollar figure summed over sessions is only the cost of that usage when every one of
// them recorded one, and a reader of other programs' records keeps no price list to
// fill in the rest. What it carries instead is the share of the prompt served from
// cache, which every CLI records and which says what a price would.
type UsageRow struct {
	Key string `json:"key"` // the model, the day, the store or the directory
	// Sessions the calls were made in; a subagent's count toward the session it ran in
	Sessions int         `json:"sessions"`
	Calls    int         `json:"calls"`
	Usage    model.Usage `json:"usage"`
	Total    int64       `json:"total"`
	CacheHit float64     `json:"cacheHit"`
}

// Usage dimensions.
const (
	ByModel   = "model"
	ByDay     = "day"
	BySource  = "source"
	ByProject = "project"
	BySession = "session"
	ByAll     = ""
)

// Usage sums what the model calls in a window consumed, grouped by a dimension. Days
// are the local calendar's; the largest groups come first, days in order.
func (ix *Index) Usage(ctx context.Context, f Filter, w Window, by string, limit int) ([]UsageRow, error) {
	_, offset := time.Now().Zone()
	key := map[string]string{
		ByModel:   "u.model",
		ByDay:     "strftime('%Y-%m-%d', (u.at / 1000) + " + itoa(offset) + ", 'unixepoch')",
		BySource:  "s.source",
		ByProject: "s.cwd",
		BySession: "s.source || '/' || s.id",
		ByAll:     "''",
	}[by]
	if key == "" {
		return nil, fmt.Errorf("usage is grouped by model, day, source, project or session, not by %q", by)
	}
	where, args := reportFilter(f).where()
	when, whenArgs := w.where("u.at")
	order := "sum(u.tok_in + u.tok_out + u.tok_cr + u.tok_cw) DESC"
	if by == ByDay {
		order = "1"
	}
	// A subagent's calls are its session's: the sessions counted are the ones people had
	q := `SELECT ` + key + ` AS k, count(DISTINCT CASE WHEN s.root_sid > 0 THEN s.root_sid ELSE s.sid END), sum(u.calls), sum(u.tok_in), sum(u.tok_out), sum(u.tok_cr), sum(u.tok_cw), sum(u.tok_think)
FROM usage u JOIN sessions s ON s.sid = u.sid WHERE ` + where + ` AND ` + when + ` GROUP BY k ORDER BY ` + order
	args = append(args, whenArgs...)
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := ix.ro.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Key, &r.Sessions, &r.Calls, &r.Usage.Input, &r.Usage.Output, &r.Usage.CacheRead, &r.Usage.CacheWrite, &r.Usage.Reasoning); err != nil {
			return nil, err
		}
		r.Total, r.CacheHit = r.Usage.Total(), r.Usage.CacheHit()
		out = append(out, r)
	}
	return out, rows.Err()
}

// Failure is a tool call that failed.
type Failure struct {
	Session string    `json:"session"` // the session's reference
	Title   string    `json:"title,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	Turn    int       `json:"turn"`
	Seq     int       `json:"seq"`
	At      time.Time `json:"at,omitzero"`
	Tool    string    `json:"tool"`
	Kind    string    `json:"kind,omitempty"`
	Status  string    `json:"status"`
	Exit    *int      `json:"exit,omitempty"`
	Command string    `json:"command,omitempty"`
	Output  string    `json:"output,omitempty"` // the end of it, where it says why
}

// FailureCount is how often one tool failed.
type FailureCount struct {
	Tool  string `json:"tool"`
	Count int    `json:"count"`
}

// FailureReport is the failed calls of a window, newest first, and what failed most.
type FailureReport struct {
	Failures []Failure      `json:"failures"`
	Total    int            `json:"total"`
	ByTool   []FailureCount `json:"byTool"`
}

// Failures lists the tool calls that failed in a window. tool, when set, keeps one
// tool's calls — named exactly, or with a trailing * by how its name begins. ByTool
// counts every tool of the window regardless.
func (ix *Index) Failures(ctx context.Context, f Filter, w Window, tool string, p Page) (FailureReport, error) {
	rep := FailureReport{Failures: []Failure{}, ByTool: []FailureCount{}}
	where, args := reportFilter(f).where()
	when, whenArgs := w.where("x.at")
	args = append(args, whenArgs...)
	// The count per tool covers the whole window, whichever tool is asked for: it is what
	// there is to choose from, and it must not shrink to the choice just made
	every := ` FROM failures x JOIN sessions s ON s.sid = x.sid WHERE ` + where + ` AND ` + when
	rows, err := ix.ro.QueryContext(ctx, `SELECT x.tool, count(*)`+every+` GROUP BY x.tool ORDER BY 2 DESC, 1 LIMIT 30`, args...)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var c FailureCount
		if err := rows.Scan(&c.Tool, &c.Count); err != nil {
			rows.Close()
			return rep, err
		}
		rep.ByTool = append(rep.ByTool, c)
	}
	rows.Close()
	// A tool is named exactly — "Bash" is Claude Code's, "bash" is Pi's — or by how its
	// name begins: "mcp__playwright*"
	from := every
	if prefix, ok := strings.CutSuffix(tool, "*"); ok {
		if prefix != "" {
			from += ` AND substr(x.tool, 1, ?) = ?`
			args = append(args, utf8.RuneCountInString(prefix), prefix)
		}
	} else if tool != "" {
		from += ` AND x.tool = ?`
		args = append(args, tool)
	}
	if err := ix.ro.QueryRowContext(ctx, `SELECT count(*)`+from, args...).Scan(&rep.Total); err != nil {
		return rep, err
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	rows, err = ix.ro.QueryContext(ctx, `
SELECT s.source, s.id, s.title, s.cwd, x.idx, x.seq, x.at, x.tool, x.kind, x.status, x.exit_code, x.command, x.output`+from+`
ORDER BY x.at DESC, x.sid DESC, x.seq DESC LIMIT ? OFFSET ?`, append(args, p.Limit, p.Offset)...)
	if err != nil {
		return rep, err
	}
	defer rows.Close()
	for rows.Next() {
		var x Failure
		var source, id string
		var at int64
		var exit *int
		if err := rows.Scan(&source, &id, &x.Title, &x.Cwd, &x.Turn, &x.Seq, &at, &x.Tool, &x.Kind, &x.Status, &exit, &x.Command, &x.Output); err != nil {
			return rep, err
		}
		x.Session, x.At, x.Exit = source+"/"+id, fromMs(at), exit
		rep.Failures = append(rep.Failures, x)
	}
	return rep, rows.Err()
}

// FileTouch is one turn naming a file.
type FileTouch struct {
	Path    string    `json:"path"`
	Changed bool      `json:"changed"`
	Session string    `json:"session"`
	Title   string    `json:"title,omitempty"`
	Turn    int       `json:"turn"`
	At      time.Time `json:"at,omitzero"`
	Ask     string    `json:"ask,omitempty"`
}

// Files lists the turns that named a file whose path contains path, newest first:
// which sessions changed it, in which turn, and what was being asked there. With
// changedOnly, the turns that merely read it are left out.
func (ix *Index) Files(ctx context.Context, f Filter, w Window, path string, changedOnly bool, p Page) ([]FileTouch, int, error) {
	where, args := reportFilter(f).where()
	when, whenArgs := w.where("x.at")
	args = append(args, whenArgs...)
	cond := where + ` AND ` + when + ` AND instr(lower(x.path), lower(?)) > 0`
	args = append(args, path)
	if changedOnly {
		cond += ` AND x.changed = 1`
	}
	from := ` FROM files x JOIN sessions s ON s.sid = x.sid JOIN turns t ON t.sid = x.sid AND t.idx = x.idx WHERE ` + cond
	var total int
	if err := ix.ro.QueryRowContext(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	rows, err := ix.ro.QueryContext(ctx, `
SELECT x.path, x.changed, s.source, s.id, s.title, x.idx, x.at, json_extract(t.head, '$.ask')`+from+`
ORDER BY x.at DESC, x.sid DESC, x.idx DESC LIMIT ? OFFSET ?`, append(args, p.Limit, p.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []FileTouch{}
	for rows.Next() {
		var x FileTouch
		var source, id string
		var changed int
		var at int64
		var ask *string
		if err := rows.Scan(&x.Path, &changed, &source, &id, &x.Title, &x.Turn, &at, &ask); err != nil {
			return nil, 0, err
		}
		x.Changed, x.Session, x.At = changed != 0, source+"/"+id, fromMs(at)
		if ask != nil {
			x.Ask = *ask
		}
		out = append(out, x)
	}
	return out, total, rows.Err()
}

// Digest is what a stretch of work added up to in one project.
type Digest struct {
	Cwd      string         `json:"cwd"`
	Sessions int            `json:"sessions"`
	Turns    int            `json:"turns"`
	Tools    int            `json:"tools"`
	Failures int            `json:"failures"`
	FirstAt  time.Time      `json:"firstAt,omitzero"`
	LastAt   time.Time      `json:"lastAt,omitzero"`
	Usage    model.Usage    `json:"usage"`
	CacheHit float64        `json:"cacheHit"`
	Changed  map[string]int `json:"changed"` // files changed, counted as code / test / docs / config
	Models   map[string]int `json:"models"`  // model calls by model
	Sources  map[string]int `json:"sources"` // sessions by store
}

// DigestReport is a window's work, per project and in total.
type DigestReport struct {
	Projects []Digest `json:"projects"`
	Total    Digest   `json:"total"`
}

// Digest answers what a window's work added up to, per project: sessions, turns,
// failures, tokens, the files changed by kind, and which models and CLIs did the work.
// It counts the turns asked in the window, wherever their sessions began or ended.
func (ix *Index) Digest(ctx context.Context, f Filter, w Window) (DigestReport, error) {
	rep := DigestReport{Projects: []Digest{}, Total: newDigest("")}
	where, args := reportFilter(f).where()
	by := map[string]*Digest{}
	get := func(cwd string) *Digest {
		d := by[cwd]
		if d == nil {
			nd := newDigest(cwd)
			d = &nd
			by[cwd] = d
		}
		return d
	}

	// A session and a turn are a person's: the conversation, and what they asked in it. A
	// subagent's transcript is neither — but the tools it ran, what failed and what it
	// spent are work the session did, and are counted with it.
	when, whenArgs := w.where("t.started_at")
	rows, err := ix.ro.QueryContext(ctx, digestTurns(where, when), append(append([]any{}, args...), whenArgs...)...)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var cwd, source string
		var sessions, turns, tools, failures int
		var first, last int64
		if err := rows.Scan(&cwd, &source, &sessions, &turns, &tools, &failures, &first, &last); err != nil {
			rows.Close()
			return rep, err
		}
		for _, d := range []*Digest{get(cwd), &rep.Total} {
			d.Sessions += sessions
			d.Turns += turns
			d.Tools += tools
			d.Failures += failures
			if sessions > 0 {
				d.Sources[source] += sessions
			}
			if t := fromMs(first); !t.IsZero() && (d.FirstAt.IsZero() || t.Before(d.FirstAt)) {
				d.FirstAt = t
			}
			if t := fromMs(last); t.After(d.LastAt) {
				d.LastAt = t
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	when, whenArgs = w.where("u.at")
	rows, err = ix.ro.QueryContext(ctx, `
SELECT s.cwd, u.model, sum(u.calls), sum(u.tok_in), sum(u.tok_out), sum(u.tok_cr), sum(u.tok_cw), sum(u.tok_think)
FROM usage u JOIN sessions s ON s.sid = u.sid WHERE `+where+` AND `+when+` GROUP BY s.cwd, u.model`,
		append(append([]any{}, args...), whenArgs...)...)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var cwd, name string
		var calls int
		var u model.Usage
		if err := rows.Scan(&cwd, &name, &calls, &u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.Reasoning); err != nil {
			rows.Close()
			return rep, err
		}
		for _, d := range []*Digest{get(cwd), &rep.Total} {
			d.Usage.Add(u)
			if name != "" {
				d.Models[name] += calls
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	when, whenArgs = w.where("x.at")
	rows, err = ix.ro.QueryContext(ctx, `
SELECT DISTINCT s.cwd, x.path FROM files x JOIN sessions s ON s.sid = x.sid WHERE x.changed = 1 AND `+where+` AND `+when,
		append(append([]any{}, args...), whenArgs...)...)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var cwd, path string
		if err := rows.Scan(&cwd, &path); err != nil {
			rows.Close()
			return rep, err
		}
		kind := transcript.FileKind(path)
		get(cwd).Changed[kind]++
		rep.Total.Changed[kind]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	for _, d := range by {
		d.CacheHit = d.Usage.CacheHit()
		rep.Projects = append(rep.Projects, *d)
	}
	rep.Total.CacheHit = rep.Total.Usage.CacheHit()
	sortBy(rep.Projects, func(a, b Digest) bool {
		if a.Turns != b.Turns {
			return a.Turns > b.Turns
		}
		return a.Cwd < b.Cwd
	})
	return rep, nil
}

// digestTurns is what a digest adds up over the turns of a window. Everything it reads
// of a turn is in the index turns_report, so that a week's digest reads a week of small
// rows and not the turns themselves, text and all.
func digestTurns(where, when string) string {
	return `
SELECT s.cwd, s.source, count(DISTINCT t.sid) FILTER (WHERE s.parent_session = ''), count(*) FILTER (WHERE s.parent_session = ''),
  sum(t.tools), sum(t.failures), min(t.started_at), max(max(t.ended_at, t.started_at))
FROM turns t JOIN sessions s ON s.sid = t.sid WHERE t.idx > 0 AND ` + where + ` AND ` + when + ` GROUP BY s.cwd, s.source`
}

func newDigest(cwd string) Digest {
	return Digest{Cwd: cwd,
		Changed: map[string]int{transcript.FileCode: 0, transcript.FileTest: 0, transcript.FileDocs: 0, transcript.FileConfig: 0},
		Models:  map[string]int{}, Sources: map[string]int{}}
}

// Totals is what the whole index holds.
type Totals struct {
	Sessions int `json:"sessions"` // every session the index holds, archived ones included
	// Archived is how many of those their CLI has put away: the ones a list leaves out
	// unless asked, and the difference between this count and a list's.
	Archived int   `json:"archived"`
	Agents   int   `json:"agents"` // subagent transcripts
	Turns    int   `json:"turns"`
	Bytes    int64 `json:"bytes"` // the index file's size
}

// Totals counts what the index holds of the given stores.
func (ix *Index) Totals(ctx context.Context, stores []string) (Totals, error) {
	var t Totals
	f := Filter{Stores: stores, Archived: true, Agents: true}
	where, args := f.where()
	err := ix.ro.QueryRowContext(ctx, `
SELECT count(*) FILTER (WHERE s.parent_session = ''), count(*) FILTER (WHERE s.parent_session = '' AND s.archived != 0),
  count(*) FILTER (WHERE s.parent_session != ''), coalesce(sum(s.turns) FILTER (WHERE s.parent_session = ''), 0)
FROM sessions s WHERE `+where, args...).Scan(&t.Sessions, &t.Archived, &t.Agents, &t.Turns)
	if err != nil {
		return t, err
	}
	_ = ix.ro.QueryRowContext(ctx, `SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()`).Scan(&t.Bytes)
	return t, nil
}

// UsageReport is what the model calls consumed: over rolling windows, and by where.
type UsageReport struct {
	// Total is everything in the window: the breakdowns below list only their largest
	// groups, so their rows do not add up to it.
	Total     UsageRow   `json:"total"`
	Windows   []UsageRow `json:"windows"`
	ByModel   []UsageRow `json:"byModel"`
	ByDay     []UsageRow `json:"byDay"`
	BySource  []UsageRow `json:"bySource"`
	ByProject []UsageRow `json:"byProject"`
}

// UsageSummary answers what was consumed. The windows roll — 24h, 3d, 7d, 30d, all —
// because a calendar window reads differently depending on which day it is asked. The
// breakdowns cover w; the days, the last thirty when w is unbounded.
func (ix *Index) UsageSummary(ctx context.Context, f Filter, w Window) (UsageReport, error) {
	var rep UsageReport
	now := time.Now()
	for _, win := range []struct {
		name string
		d    time.Duration
	}{{"24h", 24 * time.Hour}, {"3d", 72 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}, {"all", 0}} {
		var bounds Window
		if win.d > 0 {
			bounds.Since = now.Add(-win.d)
		}
		rows, err := ix.Usage(ctx, f, bounds, ByAll, 0)
		if err != nil {
			return rep, err
		}
		row := UsageRow{}
		if len(rows) > 0 {
			row = rows[0]
		}
		row.Key = win.name
		rep.Windows = append(rep.Windows, row)
	}
	var err error
	if rep.Total, err = ix.UsageTotal(ctx, f, w); err != nil {
		return rep, err
	}
	if rep.ByModel, err = ix.Usage(ctx, f, w, ByModel, 12); err != nil {
		return rep, err
	}
	days := w
	if days.Since.IsZero() {
		days.Since = now.Add(-30 * 24 * time.Hour)
	}
	if rep.ByDay, err = ix.Usage(ctx, f, days, ByDay, 0); err != nil {
		return rep, err
	}
	if rep.BySource, err = ix.Usage(ctx, f, w, BySource, 0); err != nil {
		return rep, err
	}
	rep.ByProject, err = ix.Usage(ctx, f, w, ByProject, 12)
	return rep, err
}

// UsageTotal is everything the model calls in a window consumed, as one row.
func (ix *Index) UsageTotal(ctx context.Context, f Filter, w Window) (UsageRow, error) {
	rows, err := ix.Usage(ctx, f, w, ByAll, 0)
	if err != nil || len(rows) == 0 {
		return UsageRow{Key: "total"}, err
	}
	rows[0].Key = "total"
	return rows[0], nil
}

// FailuresOf lists one session's failed tool calls, in the order they happened.
func (ix *Index) FailuresOf(ctx context.Context, s *Session) ([]Failure, error) {
	rows, err := ix.ro.QueryContext(ctx, `SELECT idx, seq, at, tool, kind, status, exit_code, command, output FROM failures WHERE sid = ? ORDER BY seq`, s.sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Failure
	for rows.Next() {
		x := Failure{Session: s.Ref, Title: s.Title, Cwd: s.Cwd}
		var at int64
		if err := rows.Scan(&x.Turn, &x.Seq, &at, &x.Tool, &x.Kind, &x.Status, &x.Exit, &x.Command, &x.Output); err != nil {
			return nil, err
		}
		x.At = fromMs(at)
		out = append(out, x)
	}
	return out, rows.Err()
}
