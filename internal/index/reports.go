package index

import (
	"context"
	"database/sql"
	"errors"
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
	Key string `json:"key"` // the model, the day, the store, the directory or the session's ref
	// Title and Cwd name a session, in a row of one: what a ref alone does not say
	Title string `json:"title,omitempty"`
	Cwd   string `json:"cwd,omitempty"`
	// Sessions the calls were made in; a subagent's count toward the session it ran in
	Sessions int         `json:"sessions"`
	Calls    int         `json:"calls"`
	Usage    model.Usage `json:"usage"`
	Total    int64       `json:"total"`
	CacheHit float64     `json:"cacheHit"`
	// Turns and AgentMs are the person's turns asked in the row's group and how long the
	// agents worked on them (see agentMs) — in every grouping but by model, which a turn
	// is not: it may call on several.
	Turns   int   `json:"turns,omitempty"`
	AgentMs int64 `json:"agentMs,omitempty"`
}

// agentMs is how long the agents worked on a window's turns, in milliseconds: from each ask
// to the last thing the agent did, summed. Two agents at work at once count twice, as two
// people would; a subagent's time is in the turn that started it, and not counted again.
const agentMs = `sum(CASE WHEN t.started_at > 0 AND t.ended_at > t.started_at THEN t.ended_at - t.started_at ELSE 0 END)`

// Usage dimensions.
const (
	ByModel   = "model"
	ByDay     = "day"
	ByWeek    = "week"
	ByMonth   = "month"
	BySource  = "source"
	ByProject = "project"
	BySession = "session"
	ByAll     = ""
)

// rootOf is the session a transcript's work counts toward: the one it ran in, for a
// subagent's, and its own for any other.
const rootOf = `CASE WHEN s.root_sid > 0 THEN s.root_sid ELSE s.sid END`

// calendar reports whether a grouping is by the calendar: its rows come in order, not
// largest first.
func calendar(by string) bool { return by == ByDay || by == ByWeek || by == ByMonth }

// Usage sums what the model calls in a window consumed, grouped by a dimension. Days,
// weeks and months are the local calendar's — a week is keyed by the Monday it begins on,
// a month as 2026-11 — and come in order; other groups come largest first. By session, a
// subagent's calls are the session's it ran in, and a row names its session.
func (ix *Index) Usage(ctx context.Context, f Filter, w Window, by string, limit int) ([]UsageRow, error) {
	key := keyOf(by, "u.at")
	if key == "" {
		return nil, fmt.Errorf("usage is grouped by model, day, week, month, source, project or session, not by %q", by)
	}
	where, args := ix.widen(ctx, reportFilter(f), !w.Since.IsZero()).where()
	when, whenArgs := w.where("u.at")
	order := "sum(u.tok_in + u.tok_out + u.tok_cr + u.tok_cw) DESC"
	if calendar(by) {
		order = "1 DESC" // the latest, when there are more than limit, then put in order
	}
	named, root := "'', ''", ""
	if by == BySession {
		named, root = "coalesce(r.title, s.title), coalesce(r.cwd, s.cwd)", ` LEFT JOIN sessions r ON r.sid = `+rootOf
	}
	// A subagent's calls are its session's: the sessions counted are the ones people had
	q := `SELECT ` + key + ` AS k, ` + named + `, count(DISTINCT ` + rootOf + `), sum(u.calls), sum(u.tok_in), sum(u.tok_out), sum(u.tok_cr), sum(u.tok_cw), sum(u.tok_think)
FROM usage u JOIN sessions s ON s.sid = u.sid` + root + ` WHERE ` + where + ` AND ` + when + ` GROUP BY k ORDER BY ` + order
	args = append(args, whenArgs...)
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	if calendar(by) {
		q = `SELECT * FROM (` + q + `) ORDER BY 1`
	}
	rows, err := ix.ro.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Key, &r.Title, &r.Cwd, &r.Sessions, &r.Calls, &r.Usage.Input, &r.Usage.Output, &r.Usage.CacheRead, &r.Usage.CacheWrite, &r.Usage.Reasoning); err != nil {
			return nil, err
		}
		r.Total, r.CacheHit = r.Usage.Total(), r.Usage.CacheHit()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if by == ByModel || len(out) == 0 {
		return out, nil
	}
	// The turns of each group, and the agents' time on them
	at := map[string]*UsageRow{}
	for i := range out {
		at[out[i].Key] = &out[i]
	}
	when, whenArgs = w.where("t.started_at")
	where, args = ix.widen(ctx, reportFilter(f), !w.Since.IsZero()).where()
	rows, err = ix.ro.QueryContext(ctx, usageTurns(by, root, where, when), append(args, whenArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var turns int
		var ms int64
		if err := rows.Scan(&k, &turns, &ms); err != nil {
			return nil, err
		}
		if r := at[k]; r != nil {
			r.Turns, r.AgentMs = turns, ms
		}
	}
	return out, rows.Err()
}

// usageTurns counts the turns of each usage group, and adds up the agents' time on them. With
// no window to start from, the planner reads every turn of each session the filter keeps,
// and turns_at holds all it takes of them: from the turns' own rows, that is their text.
func usageTurns(by, root, where, when string) string {
	return `SELECT ` + keyOf(by, "t.started_at") + ` AS k, count(*), ` + agentMs + `
FROM turns t JOIN sessions s ON s.sid = t.sid` + root + ` WHERE t.idx > 0 AND s.parent_session = '' AND ` + where + ` AND ` + when + ` GROUP BY k`
}

// keyOf is what a usage grouping groups by, of a moment in the column at; "" for a grouping
// there is none of.
func keyOf(by, at string) string {
	_, offset := time.Now().Zone()
	local := "(" + at + " / 1000) + " + itoa(offset) + ", 'unixepoch'"
	return map[string]string{
		ByModel:   "u.model",
		ByDay:     "strftime('%Y-%m-%d', " + local + ")",
		ByWeek:    "date(" + local + ", '-6 days', 'weekday 1')",
		ByMonth:   "strftime('%Y-%m', " + local + ")",
		BySource:  "s.source",
		ByProject: "s.cwd",
		BySession: "coalesce(r.source || '/' || r.id, s.source || '/' || s.id)",
		ByAll:     "''",
	}[by]
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
	// Later is where the same program, asked the same thing by the same tool, next
	// succeeded in the same session — go test after go test — when it did within a few
	// turns; the fix, if there was one, lies between (see later.go).
	Later *Later `json:"passedLater,omitempty"`
}

// Later is a step that ran a failed command again, in some form, and succeeded.
type Later struct {
	Turn    int       `json:"turn"`
	Seq     int       `json:"seq"`
	At      time.Time `json:"at,omitzero"`
	Command string    `json:"command"` // as it was run then, which need not be as it failed
}

// FailureCount is how often one tool failed.
type FailureCount struct {
	Tool  string `json:"tool"`
	Count int    `json:"count"`
}

// FailureQuery narrows a report of failures.
type FailureQuery struct {
	// Tool keeps one tool's calls — named exactly, or with a trailing * by how its name
	// begins
	Tool string
	// Text keeps the calls whose command or output holds its words — each word, or "a
	// phrase", anywhere in either, without regard to case; -word keeps those that do not.
	// The output is the end of it the table keeps, where a command says why it failed.
	Text string
	// Open keeps the calls whose command did not pass later (see Failure.Later)
	Open bool
}

// FailureCounts is what a report of failures counts, however it lists them.
type FailureCounts struct {
	Total int `json:"total"` // what the list holds, across every page
	// Open and PassedLater are the calls that match, but for Open, by whether the same
	// command passed later: what a list of the open ones leaves out is counted too
	Open        int            `json:"open"`
	PassedLater int            `json:"passedLater"`
	ByTool      []FailureCount `json:"byTool"`
	// Outside is, when none in the window match, how many do outside it: what the window
	// alone hid, which an answer of none would otherwise say does not exist
	Outside int `json:"outside,omitempty"`
	// What the failures were narrowed to, for a reader to be told why there are none
	Asked FailureQuery `json:"-"`
}

// FailureReport is the failed calls of a window, newest first, and what failed most.
type FailureReport struct {
	Failures []Failure `json:"failures"`
	FailureCounts
}

// Failures lists the tool calls that failed in a window, as q narrows them. ByTool counts
// every tool of the window regardless. When none in the window match, Outside says how
// many do outside it.
func (ix *Index) Failures(ctx context.Context, f Filter, w Window, q FailureQuery, p Page) (FailureReport, error) {
	rep := FailureReport{Failures: []Failure{}}
	from, args, err := ix.failureCounts(ctx, f, w, q, &rep.FailureCounts)
	if err != nil || rep.Total == 0 {
		return rep, err
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	rows, err := ix.ro.QueryContext(ctx, `
SELECT s.source, s.id, s.title, s.cwd, `+failureColumns+from+`
ORDER BY x.at DESC, x.sid DESC, x.seq DESC LIMIT ? OFFSET ?`, append(args, p.Limit, p.Offset)...)
	if err != nil {
		return rep, err
	}
	defer rows.Close()
	for rows.Next() {
		var x Failure
		var source, id string
		if err := scanFailure(rows, &x, &source, &id, &x.Title, &x.Cwd); err != nil {
			return rep, err
		}
		x.Session = source + "/" + id
		rep.Failures = append(rep.Failures, x)
	}
	return rep, rows.Err()
}

// failureCounts counts the failures of a window as q narrows them, into c — Total, the
// calls that match — and gives what selects them: a FROM clause over the failures table,
// aliased x, and the sessions table, aliased s, with its arguments.
func (ix *Index) failureCounts(ctx context.Context, f Filter, w Window, q FailureQuery, c *FailureCounts) (string, []any, error) {
	c.ByTool, c.Asked = []FailureCount{}, q
	where, args := ix.widen(ctx, reportFilter(f), !w.Since.IsZero()).where()
	when, whenArgs := w.where("x.at")
	args = append(args, whenArgs...)
	// The count per tool covers the whole window, whichever tool is asked for: it is what
	// there is to choose from, and it must not shrink to the choice just made
	every := ` FROM failures x JOIN sessions s ON s.sid = x.sid WHERE ` + where + ` AND ` + when
	rows, err := ix.ro.QueryContext(ctx, `SELECT x.tool, count(*)`+every+` GROUP BY x.tool ORDER BY 2 DESC, 1 LIMIT 30`, args...)
	if err != nil {
		return "", nil, err
	}
	for rows.Next() {
		var n FailureCount
		if err := rows.Scan(&n.Tool, &n.Count); err != nil {
			rows.Close()
			return "", nil, err
		}
		c.ByTool = append(c.ByTool, n)
	}
	rows.Close()
	narrow, narrowArgs := failuresNarrowed(q.Tool, q.Text)
	args = append(args, narrowArgs...)
	if err := ix.ro.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE x.later_seq = 0), count(*) FILTER (WHERE x.later_seq > 0)`+every+narrow,
		args...).Scan(&c.Open, &c.PassedLater); err != nil {
		return "", nil, err
	}
	c.Total = c.Open + c.PassedLater
	if q.Open {
		narrow += ` AND x.later_seq = 0`
		c.Total = c.Open
	}
	if c.Total == 0 && (!w.Since.IsZero() || !w.Until.IsZero()) {
		// None in the window: whether the window is all that hid them
		where, args := ix.widen(ctx, reportFilter(f), false).where()
		err := ix.ro.QueryRowContext(ctx, `SELECT count(*) FROM failures x JOIN sessions s ON s.sid = x.sid WHERE `+where+narrow,
			append(args, narrowArgs...)...).Scan(&c.Outside)
		if err != nil {
			return "", nil, err
		}
	}
	return every + narrow, args, nil
}

// FailureGroup is the failed calls of one command — the program and what it was asked, as
// whether the same command passed later is judged: go test, terraform plan — or of one
// tool that runs none.
type FailureGroup struct {
	Command  string  `json:"command,omitempty"` // the program and its subcommand: go test
	Tool     string  `json:"tool,omitempty"`    // for the calls that ran no such command, the tool
	Count    int     `json:"count"`
	Open     int     `json:"open"`     // of which the same command did not pass later
	Sessions int     `json:"sessions"` // they failed in; a subagent's count as the session it ran in
	Last     Failure `json:"last"`     // the latest of them
}

// FailureGroups is the failed calls of a window by the command that failed, the commands
// with failures still open first, then those that failed most.
type FailureGroups struct {
	Commands []FailureGroup `json:"commands"`
	// Total counts the commands
	FailureCounts
}

// FailuresByCommand groups the failures of a window, as q narrows them, by the command
// that failed: what keeps failing, and what of it is still open.
func (ix *Index) FailuresByCommand(ctx context.Context, f Filter, w Window, q FailureQuery, p Page) (FailureGroups, error) {
	rep := FailureGroups{Commands: []FailureGroup{}}
	from, args, err := ix.failureCounts(ctx, f, w, q, &rep.FailureCounts)
	if err != nil || rep.Total == 0 {
		return rep, err
	}
	const group = `x.command_key, CASE WHEN x.command_key = '' THEN x.tool ELSE '' END` // a command, or a tool that runs none
	in := `WITH m AS (SELECT x.command_key AS g_command, CASE WHEN x.command_key = '' THEN x.tool ELSE '' END AS g_tool,
  CASE WHEN s.root_sid > 0 THEN s.root_sid ELSE s.sid END AS root,
  row_number() OVER (PARTITION BY ` + group + ` ORDER BY x.at DESC, x.sid DESC, x.seq DESC) AS rn,
  s.source, s.id, s.title, s.cwd, ` + failureColumns + from + `) `
	// The failures table's columns, as m names them
	last := strings.ReplaceAll(failureColumns, "x.", "m.")
	if err := ix.ro.QueryRowContext(ctx, in+`SELECT count(*) FROM (SELECT 1 FROM m GROUP BY g_command, g_tool)`, args...).Scan(&rep.Total); err != nil {
		return rep, err
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	rows, err := ix.ro.QueryContext(ctx, in+`
SELECT g.g_command, g.g_tool, g.n, g.open, g.sessions, m.source, m.id, m.title, m.cwd, `+last+`
FROM (SELECT g_command, g_tool, count(*) AS n, count(*) FILTER (WHERE later_seq = 0) AS open, count(DISTINCT root) AS sessions
      FROM m GROUP BY g_command, g_tool) g
JOIN m ON m.g_command = g.g_command AND m.g_tool = g.g_tool AND m.rn = 1
ORDER BY g.open > 0 DESC, g.open DESC, g.n DESC, m.at DESC LIMIT ? OFFSET ?`, append(args, p.Limit, p.Offset)...)
	if err != nil {
		return rep, err
	}
	defer rows.Close()
	for rows.Next() {
		var g FailureGroup
		var source, id string
		if err := scanFailure(rows, &g.Last, &g.Command, &g.Tool, &g.Count, &g.Open, &g.Sessions, &source, &id, &g.Last.Title, &g.Last.Cwd); err != nil {
			return rep, err
		}
		g.Last.Session = source + "/" + id
		rep.Commands = append(rep.Commands, g)
	}
	return rep, rows.Err()
}

// failuresNarrowed is what keeps the failed calls of one tool and those that hold some
// words, as conditions on the failures table aliased x, each after an AND. A tool is named
// exactly — "Bash" is Claude Code's, "bash" is Pi's — or by how its name begins:
// "mcp__playwright*".
func failuresNarrowed(tool, text string) (string, []any) {
	var cond string
	var args []any
	if prefix, ok := strings.CutSuffix(tool, "*"); ok {
		if prefix != "" {
			cond += ` AND substr(x.tool, 1, ?) = ?`
			args = append(args, utf8.RuneCountInString(prefix), prefix)
		}
	} else if tool != "" {
		cond += ` AND x.tool = ?`
		args = append(args, tool)
	}
	for _, t := range ParseQuery(text).Terms {
		has := `(instr(lower(x.command), lower(?)) > 0 OR instr(lower(x.output), lower(?)) > 0)`
		if t.Not {
			has = `NOT ` + has
		}
		cond += ` AND ` + has
		args = append(args, t.Text, t.Text)
	}
	return cond, args
}

// failureColumns are what a Failure is read from, of the failures table aliased x.
const failureColumns = `x.idx, x.seq, x.at, x.tool, x.kind, x.status, x.exit_code, x.command, x.output,
  x.later_idx, x.later_seq, x.later_at, x.later_command`

// scanFailure reads a Failure from failureColumns, after the columns named in front.
func scanFailure(row scanner, x *Failure, front ...any) error {
	var at, laterAt int64
	var later Later
	err := row.Scan(append(front, &x.Turn, &x.Seq, &at, &x.Tool, &x.Kind, &x.Status, &x.Exit, &x.Command, &x.Output,
		&later.Turn, &later.Seq, &laterAt, &later.Command)...)
	if err != nil {
		return err
	}
	x.At = fromMs(at)
	if later.Seq > 0 { // a step after the failure, which is at 0 or later
		later.At = fromMs(laterAt)
		x.Later = &later
	}
	return nil
}

// FileTouch is one turn naming a file.
type FileTouch struct {
	Path    string    `json:"path"`
	Changed bool      `json:"changed"`
	Session string    `json:"session"`
	Title   string    `json:"title,omitempty"`
	Cwd     string    `json:"cwd,omitempty"` // the session's directory, which a relative path is relative to
	Turn    int       `json:"turn"`
	At      time.Time `json:"at,omitzero"`
	Ask     string    `json:"ask,omitempty"`
	// Ended is when the turn ended: what the file is measured against.
	Ended time.Time `json:"-"`
	// Now is how the file stands now against what the turn left. Filled in by the program,
	// which can look at the file (see app.Files); nil when it cannot be said — the file is
	// not on this machine, or the path names nothing it can read.
	Now *FileNow `json:"now,omitempty"`
	// Waiting names the folder a look at the file is waiting in, when Now is nil for that:
	// on a Mac, macOS asking on its screen whether asq may read the folder (see app.lookWait).
	Waiting string `json:"waiting,omitempty"`
}

// FileNow is how a file stands now, against what a turn left it as.
//
// State is one of:
//
//	same     By content: everything the turn's last write to it put there is still in it,
//	         and it has not been written since — or the turn wrote it whole and it is that.
//	         By time: it has not been written since the turn ended.
//	changed  By content: written since, and some of what the turn's last write put there is
//	         no longer in it.
//	written  It has been written since the turn ended. By content: what the turn put there
//	         is still in it. By time: there was nothing to look for. What changed, if
//	         anything, is not known: a checkout or a formatter writes a file as well as an
//	         edit does.
//	deleted  It is gone, and the directory it was in is still here.
//
// By says which evidence: "content" compares the text the turn wrote with the file, and
// is the one to trust; "time" compares the file's modification time with the end of the
// turn, and says less.
type FileNow struct {
	State string `json:"state"`
	By    string `json:"by,omitempty"`
}

// The states and kinds of evidence of FileNow.
const (
	FileSame    = "same"
	FileChanged = "changed"
	FileWritten = "written"
	FileDeleted = "deleted"

	ByContent = "content"
	ByTime    = "time"
)

// filesFrom joins each file a turn named to its session, as s, and its turn, as t, from
// turns_at: the turns are counted and the page chosen from it, and only the page's turns
// are read for their ask. A path most turns name is otherwise a read of most of the index.
const filesFrom = ` FROM files x JOIN sessions s ON s.sid = x.sid JOIN turns t INDEXED BY turns_at ON t.sid = x.sid AND t.idx = x.idx WHERE `

// Files lists the turns that named a file whose path contains path, newest first:
// which sessions changed it, in which turn, and what was being asked there. With
// changedOnly, the turns that merely read it are left out.
func (ix *Index) Files(ctx context.Context, f Filter, w Window, path string, changedOnly bool, p Page) ([]FileTouch, int, error) {
	where, args := ix.widen(ctx, reportFilter(f), !w.Since.IsZero()).where()
	when, whenArgs := w.where("x.at")
	args = append(args, whenArgs...)
	cond := where + ` AND ` + when + ` AND instr(lower(x.path), lower(?)) > 0`
	args = append(args, path)
	if changedOnly {
		cond += ` AND x.changed = 1`
	}
	from := filesFrom + cond
	var total int
	if err := ix.ro.QueryRowContext(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	rows, err := ix.ro.QueryContext(ctx, `
SELECT x.path, x.changed, x.source, x.id, x.title, x.cwd, x.idx, x.at, max(t.ended_at, t.last_at, x.at), json_extract(t.head, '$.ask')
FROM (SELECT x.sid, x.idx, x.path, x.changed, x.at, s.source, s.id, s.title, s.cwd`+from+`
  ORDER BY x.at DESC, x.sid DESC, x.idx DESC LIMIT ? OFFSET ?) x
JOIN turns t ON t.sid = x.sid AND t.idx = x.idx
ORDER BY x.at DESC, x.sid DESC, x.idx DESC`, append(args, p.Limit, p.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []FileTouch{}
	for rows.Next() {
		var x FileTouch
		var source, id string
		var changed int
		var at, ended int64
		var ask *string
		if err := rows.Scan(&x.Path, &changed, &source, &id, &x.Title, &x.Cwd, &x.Turn, &at, &ended, &ask); err != nil {
			return nil, 0, err
		}
		x.Changed, x.Session, x.At, x.Ended = changed != 0, source+"/"+id, fromMs(at), fromMs(ended)
		if ask != nil {
			x.Ask = *ask
		}
		out = append(out, x)
	}
	return out, total, rows.Err()
}

// Digest is what a stretch of work added up to in one project.
type Digest struct {
	Cwd      string `json:"cwd"`
	Sessions int    `json:"sessions"`
	Turns    int    `json:"turns"`
	Tools    int    `json:"tools"`
	Failures int    `json:"failures"`
	// PassedLater is how many of the failures the same command passed later (see
	// Failure.Later)
	PassedLater int `json:"passedLater"`
	// AgentMs is how long agents worked on the turns (see agentMs); Days on how many days,
	// this machine's, a turn was asked
	AgentMs  int64          `json:"agentMs"`
	Days     int            `json:"days"`
	FirstAt  time.Time      `json:"firstAt,omitzero"`
	LastAt   time.Time      `json:"lastAt,omitzero"`
	Usage    model.Usage    `json:"usage"`
	CacheHit float64        `json:"cacheHit"`
	Changed  map[string]int `json:"changed"` // files changed, counted as code / test / docs / config
	Models   map[string]int `json:"models"`  // model calls by model
	Sources  map[string]int `json:"sources"` // sessions by store
	// List is the sessions behind the counts, the latest first: at most digestListed of
	// them, of the Sessions there are. A project's, not the total's.
	List []DigestSession `json:"list,omitempty"`
}

// DigestSession is one session of a project's digest: what it was, and how its turns in
// the window went. Its subagents' work is its own.
type DigestSession struct {
	Ref   string `json:"ref"`
	Title string `json:"title,omitempty"`
	// Turns are the person's, asked in the window; Tools and Failures its subagents' too
	Turns       int   `json:"turns"`
	Tools       int   `json:"tools"`
	Failures    int   `json:"failures"`
	PassedLater int   `json:"passedLater"` // as Digest.PassedLater
	AgentMs     int64 `json:"agentMs"`
	// Interrupted and Answer are of its last turn in the window: whether the person
	// stopped it, and the last thing the agent said
	Interrupted bool      `json:"interrupted,omitempty"`
	Answer      string    `json:"answer,omitempty"`
	FirstAt     time.Time `json:"firstAt,omitzero"`
	LastAt      time.Time `json:"lastAt,omitzero"`

	sid int64
}

// digestListed is how many sessions a project's digest names: the latest. The rest are a
// list of sessions away, narrowed to the project and the window.
const digestListed = 12

// DigestReport is a window's work, per project and in total.
type DigestReport struct {
	// Since and Until are the window, as this machine's clock has it: what "today" meant
	Since    time.Time `json:"since,omitzero"`
	Until    time.Time `json:"until,omitzero"`
	Projects []Digest  `json:"projects"`
	Total    Digest    `json:"total"`
	// When is when the agents worked, in milliseconds by weekday — Monday first — and hour,
	// on this machine's clock: a turn's time in each hour it ran through
	When [7][24]int64 `json:"when"`
}

// Digest answers what a window's work added up to, per project: sessions, turns,
// failures, tokens, the files changed by kind, and which models and CLIs did the work.
// It counts the turns asked in the window, wherever their sessions began or ended.
func (ix *Index) Digest(ctx context.Context, f Filter, w Window) (DigestReport, error) {
	rep := DigestReport{Since: w.Since, Until: w.Until, Projects: []Digest{}, Total: newDigest("")}
	where, args := ix.widen(ctx, reportFilter(f), !w.Since.IsZero()).where()
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

	// Of the failures of those turns, the ones whose command passed later: per project, and
	// per session — a subagent's toward the one it ran in — for the sessions listed
	passed := map[digestKey]int{}
	when, whenArgs = w.where("t.started_at")
	rows, err = ix.ro.QueryContext(ctx, `
SELECT s.cwd, `+rootOf+` AS root, count(*) FROM turns t JOIN failures x ON x.sid = t.sid AND x.idx = t.idx JOIN sessions s ON s.sid = t.sid
WHERE t.idx > 0 AND x.later_seq > 0 AND `+where+` AND `+when+` GROUP BY s.cwd, root`,
		append(append([]any{}, args...), whenArgs...)...)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var cwd string
		var root int64
		var n int
		if err := rows.Scan(&cwd, &root, &n); err != nil {
			rows.Close()
			return rep, err
		}
		get(cwd).PassedLater += n
		rep.Total.PassedLater += n
		passed[digestKey{cwd, root}] = n
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

	if err := ix.digestList(ctx, where, args, w, get, passed); err != nil {
		return rep, err
	}
	if err := ix.digestTime(ctx, where, args, w, get, &rep); err != nil {
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

// digestKey is a session of a project's digest: the project, and the session a
// transcript's work counts toward (rootOf).
type digestKey struct {
	cwd  string
	root int64
}

// digestList names each project's sessions in a window, the latest first, with how their
// turns there went: what a count of sessions does not say. passed is how many failures of
// each passed later.
func (ix *Index) digestList(ctx context.Context, where string, args []any, w Window, get func(string) *Digest, passed map[digestKey]int) error {
	when, whenArgs := w.where("t.started_at")
	rows, err := ix.ro.QueryContext(ctx, `
SELECT s.cwd, `+rootOf+` AS root, coalesce(r.source || '/' || r.id, s.source || '/' || s.id), coalesce(r.title, s.title),
  count(*) FILTER (WHERE s.parent_session = ''), sum(t.tools), sum(t.failures), min(t.started_at), max(max(t.ended_at, t.started_at))
FROM turns t JOIN sessions s ON s.sid = t.sid LEFT JOIN sessions r ON r.sid = `+rootOf+`
WHERE t.idx > 0 AND `+where+` AND `+when+` GROUP BY s.cwd, root ORDER BY 9 DESC, root DESC`,
		append(append([]any{}, args...), whenArgs...)...)
	if err != nil {
		return err
	}
	type at struct {
		d *Digest
		i int
	}
	var listed []at
	for rows.Next() {
		var cwd string
		var e DigestSession
		var first, last int64
		if err := rows.Scan(&cwd, &e.sid, &e.Ref, &e.Title, &e.Turns, &e.Tools, &e.Failures, &first, &last); err != nil {
			rows.Close()
			return err
		}
		e.FirstAt, e.LastAt = fromMs(first), fromMs(last)
		e.PassedLater = passed[digestKey{cwd, e.sid}]
		if d := get(cwd); len(d.List) < digestListed {
			d.List = append(d.List, e)
			listed = append(listed, at{d, len(d.List) - 1})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// How each ended: its last turn in the window, read for its answer alone
	for _, l := range listed {
		d := &l.d.List[l.i]
		var answer *string
		var stopped int
		err := ix.ro.QueryRowContext(ctx, `SELECT json_extract(t.head, '$.answer'), t.interrupted FROM turns t WHERE t.sid = ? AND t.idx > 0 AND `+when+` ORDER BY t.idx DESC LIMIT 1`,
			append([]any{d.sid}, whenArgs...)...).Scan(&answer, &stopped)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue // a session whose subagents alone worked in the window
		case err != nil:
			return err
		}
		if answer != nil {
			d.Answer = *answer
		}
		d.Interrupted = stopped != 0
	}
	return nil
}

// digestTime adds up when the agents worked on a window's turns: how long, per project and
// per session listed, on how many days, and in which hours of the week.
func (ix *Index) digestTime(ctx context.Context, where string, args []any, w Window, get func(string) *Digest, rep *DigestReport) error {
	when, whenArgs := w.where("t.started_at")
	rows, err := ix.ro.QueryContext(ctx, `
SELECT s.cwd, t.sid, t.started_at, max(t.ended_at, t.started_at) FROM turns t JOIN sessions s ON s.sid = t.sid
WHERE t.idx > 0 AND s.parent_session = '' AND t.started_at > 0 AND `+where+` AND `+when,
		append(append([]any{}, args...), whenArgs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type key struct {
		cwd string
		sid int64
	}
	bySession := map[key]int64{}
	days := map[string]map[string]bool{} // the days of each project, and "" of all
	for rows.Next() {
		var k key
		var start, end int64
		if err := rows.Scan(&k.cwd, &k.sid, &start, &end); err != nil {
			return err
		}
		ms := end - start
		d := get(k.cwd)
		d.AgentMs += ms
		rep.Total.AgentMs += ms
		bySession[k] += ms
		day := time.UnixMilli(start).Local().Format("2006-01-02")
		for _, cwd := range []string{k.cwd, ""} {
			if days[cwd] == nil {
				days[cwd] = map[string]bool{}
			}
			days[cwd][day] = true
		}
		spread(&rep.When, time.UnixMilli(start).Local(), time.UnixMilli(end).Local())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rep.Total.Days = len(days[""])
	for cwd, set := range days {
		if cwd != "" {
			get(cwd).Days = len(set)
		}
	}
	for k, ms := range bySession {
		d := get(k.cwd)
		for i := range d.List {
			if d.List[i].sid == k.sid {
				d.List[i].AgentMs = ms
			}
		}
	}
	return nil
}

// spread lays the time from start to end into the hours of the week it ran through.
func spread(grid *[7][24]int64, start, end time.Time) {
	for at := start; at.Before(end); {
		next := time.Date(at.Year(), at.Month(), at.Day(), at.Hour()+1, 0, 0, 0, at.Location())
		if !next.After(at) { // a clock put back: the hour comes again
			next = at.Truncate(time.Hour).Add(time.Hour)
		}
		if next.After(end) {
			next = end
		}
		grid[(int(at.Weekday())+6)%7][at.Hour()] += next.Sub(at).Milliseconds()
		at = next
	}
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
	Free     int64 `json:"free"`  // of which no longer in use (see Space)
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
	if sp, err := ix.Space(ctx); err == nil {
		t.Bytes, t.Free = sp.Bytes, sp.Free
	}
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
	BySession []UsageRow `json:"bySession"`
}

// UsageSummary answers what was consumed. The windows roll — 24h, 7d, 30d, all, the ones
// every view of the page offers — because a calendar window reads differently depending
// on which day it is asked. The breakdowns cover w; the days are the last thirty, or the
// days of w when it ends before now.
func (ix *Index) UsageSummary(ctx context.Context, f Filter, w Window) (UsageReport, error) {
	var rep UsageReport
	now := time.Now()
	for _, win := range []struct {
		name string
		d    time.Duration
	}{{"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}, {"all", 0}} {
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
	// A chart of days, the same whichever window the breakdowns cover — unless that window
	// ended some time ago, and its own days are the ones asked about
	days := Window{Since: now.Add(-30 * 24 * time.Hour)}
	if !w.Until.IsZero() && w.Until.Before(now.Add(-time.Minute)) {
		days = w
		if days.Since.IsZero() {
			days.Since = w.Until.Add(-30 * 24 * time.Hour)
		}
	}
	if rep.ByDay, err = ix.Usage(ctx, f, days, ByDay, 0); err != nil {
		return rep, err
	}
	if rep.BySource, err = ix.Usage(ctx, f, w, BySource, 0); err != nil {
		return rep, err
	}
	if rep.ByProject, err = ix.Usage(ctx, f, w, ByProject, 12); err != nil {
		return rep, err
	}
	rep.BySession, err = ix.Usage(ctx, f, w, BySession, 12)
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
	rows, err := ix.ro.QueryContext(ctx, `SELECT `+failureColumns+` FROM failures x WHERE x.sid = ? ORDER BY x.seq`, s.sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Failure
	for rows.Next() {
		x := Failure{Session: s.Ref, Title: s.Title, Cwd: s.Cwd}
		if err := scanFailure(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
