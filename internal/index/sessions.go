package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
)

// ActiveWindow bounds what Active reports: the session's newest row is this recent.
//
// That is recency, not liveness. Nothing checks whether a process exists: a session
// whose agent exited a minute ago still reads as active until the window passes, and one
// that has been thinking for longer reads as idle while very much alive. Real liveness
// is not available across the CLIs — one registers only its background sessions, the
// rest expose nothing — and one dot that means "a process is running" for one CLI and
// "written recently" for the others is worse than an approximation that is at least
// the same everywhere. Where the transcript records it, Live says more: where the latest
// turn stands.
const ActiveWindow = 2 * time.Minute

// How long after its transcript was last written a session is still said to stand where
// the transcript left it (model.Live). An agent at work writes as it goes: one that has
// written nothing for WorkingFor has most likely stopped — its CLI closed, or killed, in
// the middle of a turn — and is not said to be working. Waiting, on a call or for the
// person, lasts: a permission prompt is answered when someone comes back to it, and an
// answer is read when they do. WaitingFor bounds that, so that a session left so for good
// is not said to need anyone for good.
const (
	WorkingFor = 10 * time.Minute
	WaitingFor = 24 * time.Hour
)

// LiveStates are where a turn can stand, as Filter.State takes them.
var LiveStates = []string{model.LiveWorking, model.LiveWaiting, model.LiveIdle}

// liveFor is how long a session is said to stand so after it was last written.
func liveFor(state string) time.Duration {
	if state == model.LiveWorking {
		return WorkingFor
	}
	return WaitingFor
}

// UnderWay reports whether turn idx is the one a session's agent is at: its latest, while the
// session is said to be working or waiting (Live). A turn whose ask is the last thing written
// is read as one the person stopped before the agent wrote anything (see transcript.Writer),
// and the turn the agent is answering now is that too until its first row comes. It is not
// said to be interrupted while the session stands so, and is once it no longer does.
func (s *Session) UnderWay(idx int) bool {
	return idx > 0 && idx == s.Stats.Turns && s.Live != nil && (s.Live.State == model.LiveWorking || s.Live.State == model.LiveWaiting)
}

// underWay is UnderWay in SQL, of turn t of session s, as of now, and its arguments.
func underWay(now time.Time) (string, []any) {
	return `(t.idx = s.turns AND (s.live_state = ? AND s.updated_at > ? OR s.live_state = ? AND s.updated_at > ?))`,
		[]any{model.LiveWorking, ms(now.Add(-WorkingFor)), model.LiveWaiting, ms(now.Add(-WaitingFor))}
}

// Session is a session as the index holds it: what its transcript said, what it adds
// up to, and where it sits among the others.
type Session struct {
	model.Session
	Stats model.Stats `json:"stats"`

	// Ref names the session wherever one is asked for.
	Ref string `json:"ref"`
	// Root is the session a subagent's transcript ran in, however deep it sits.
	Root   string `json:"root,omitempty"`
	Depth  int    `json:"depth,omitempty"`
	Agents int    `json:"agents,omitempty"` // subagent transcripts beneath this one

	LastAsk string `json:"lastAsk,omitempty"`
	Answer  string `json:"answer,omitempty"` // the last thing the agent said
	Active  bool   `json:"active,omitempty"` // written within ActiveWindow
	Error   string `json:"error,omitempty"`  // why its transcript could not be read whole

	// Resume is the command that reopens the session in its own CLI, on this machine. The
	// index does not know it; the layer that knows the CLIs fills it in.
	Resume string `json:"resume,omitempty"`
	// ResumeThere is, for a session read from a store added by name, the command that
	// reopens it where that store is its CLI's own: on the machine it came from, not this
	// one. Resume is then empty.
	ResumeThere string `json:"resumeThere,omitempty"`
	// Kept says how asq keeps a copy of the session (see internal/keep): "one", kept one by
	// one; "all", as every session of its CLI is; "copy", its CLI has deleted it and this is
	// the copy. Keepable says it is a session asq can keep one by one. The layer that knows
	// what is kept fills both in, for a session asked for on its own.
	Kept     string `json:"kept,omitempty"`
	Keepable bool   `json:"keepable,omitempty"`

	sid     int64
	rootSid int64
}

// SID is the session's row in this index: stable while the index file lives, and of no
// meaning outside it.
func (s *Session) SID() int64 { return s.sid }

// Filter selects sessions.
type Filter struct {
	// Stores are the stores this process reads; sessions of any other are not seen.
	// Empty means no restriction.
	Stores []string
	// Source keeps one store, or — given a CLI's name — every store of that CLI.
	Source  string
	Project string // part of the working directory
	// Dirs keeps the sessions whose working directory is one of these or below one of
	// them: one directory by every path that names it, as a CLI may have recorded it
	// with its symbolic links resolved or as they were.
	Dirs []string
	// In keeps the sessions whose working directory is exactly one of these: the one an
	// agent works in, by the paths that name it, and not those of the directories below.
	In     []string
	Branch string // part of the branch the session opened on
	Text   string // part of the title, the id, the directory or the path
	SID    int64  // one session alone, by its row in this index (Session.SID)
	Since  time.Time
	Until  time.Time
	// Archived includes the sessions their CLI has put away; Agents the subagents'
	// transcripts, which otherwise stay beneath the session they ran in.
	Archived bool
	Agents   bool
	// Empty includes the transcripts that hold no conversation: a CLI opened and closed.
	Empty bool
	// State keeps the sessions whose latest turn stands so, as Session.Live says it: by
	// what the transcript says, while it was last written recently enough to be believed.
	State []string

	// wide says the store term filters nothing — Stores names every store the index
	// holds — and the query is one the time should drive. See Index.widen.
	wide bool
}

// where renders the filter as conditions on the sessions table, aliased s.
func (f Filter) where() (string, []any) {
	conds := []string{"s.copy_of = 0"}
	var args []any
	if len(f.Stores) > 0 {
		// A copy is folded into the session it is a copy of where that one is seen. A
		// process that does not read its store — an index another configuration filled —
		// has the copy stand for it instead, rather than neither
		conds[0] = "(s.copy_of = 0 OR NOT EXISTS (SELECT 1 FROM sessions o WHERE o.sid = s.copy_of AND o.source IN (" + placeholders(len(f.Stores)) + ")))"
		for _, name := range f.Stores {
			args = append(args, name)
		}
		// The term names the stores this process reads. When that is every store in the
		// index it filters nothing, and if it chose the plan a report would start from
		// the sessions and walk all their turns to find a week's: the + keeps it out of
		// the choice. When it is some of them — a process confined to one store, in an
		// index another process fills with all of them — it is the narrowest thing the
		// query has, and is left to drive.
		term := "s.source IN ("
		if f.wide {
			term = "+s.source IN ("
		}
		conds = append(conds, term+placeholders(len(f.Stores))+")")
		for _, name := range f.Stores {
			args = append(args, name)
		}
	}
	if f.Source != "" {
		conds = append(conds, "(s.source = ? OR s.agent = ?)")
		args = append(args, f.Source, f.Source)
	}
	if f.Project != "" {
		conds = append(conds, "instr(lower(s.cwd), lower(?)) > 0")
		args = append(args, f.Project)
	}
	if len(f.Dirs) > 0 {
		// The directory itself, or one whose path begins with it and a separator: /work/api
		// holds /work/api/web and not /work/api-old. Without regard to case, as Project is:
		// a Mac's disk does not tell /Work from /work, and a shell keeps the current
		// directory in the case it was typed in.
		var either []string
		for _, dir := range f.Dirs {
			same, under := dirTerms(dir)
			either = append(either, "lower(s.cwd) = lower(?)", "instr(lower(s.cwd), lower(?)) = 1")
			args = append(args, same, under)
		}
		conds = append(conds, "("+strings.Join(either, " OR ")+")")
	}
	if len(f.In) > 0 {
		conds = append(conds, "s.cwd IN ("+placeholders(len(f.In))+")")
		for _, d := range f.In {
			args = append(args, d)
		}
	}
	if f.Branch != "" {
		conds = append(conds, "instr(lower(s.branch), lower(?)) > 0")
		args = append(args, f.Branch)
	}
	if f.Text != "" {
		conds = append(conds, "(instr(lower(s.title), lower(?)) > 0 OR instr(lower(s.id), lower(?)) > 0 OR instr(lower(s.cwd), lower(?)) > 0 OR instr(lower(s.path), lower(?)) > 0)")
		args = append(args, f.Text, f.Text, f.Text, f.Text)
	}
	if len(f.State) > 0 {
		now := time.Now()
		var either []string
		for _, state := range f.State {
			either = append(either, "(s.live_state = ? AND s.updated_at >= ?)")
			args = append(args, state, ms(now.Add(-liveFor(state))))
		}
		conds = append(conds, "("+strings.Join(either, " OR ")+")")
	}
	if f.SID != 0 {
		conds = append(conds, "s.sid = ?")
		args = append(args, f.SID)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "s.updated_at >= ?")
		args = append(args, ms(f.Since))
	}
	if !f.Until.IsZero() {
		conds = append(conds, "s.updated_at <= ?")
		args = append(args, ms(f.Until))
	}
	if !f.Archived {
		conds = append(conds, "s.archived = 0")
	}
	if !f.Agents {
		conds = append(conds, "s.parent_session = ''")
	}
	if !f.Empty {
		conds = append(conds, "s.items > 0")
	}
	return strings.Join(conds, " AND "), args
}

// dirTerms is a directory as the two things a working directory is compared with: the
// directory's own path, and how the paths below it begin — the path and a separator of
// the kind it is written with.
func dirTerms(dir string) (same, under string) {
	same = strings.TrimRight(dir, `/\`)
	if same == "" || strings.HasSuffix(same, ":") { // a root, / or C:\, which every path below it begins with
		return dir, dir
	}
	sep := "/"
	if strings.Contains(same, `\`) && !strings.Contains(same, "/") {
		sep = `\`
	}
	return same, same + sep
}

const sessionColumns = `s.sid, s.source, s.agent, s.id, s.path, s.title, s.cwd, s.branch, s.model, s.version, s.platform,
  s.started_at, s.updated_at, s.archived, s.running, s.live_state, s.live,
  s.parent_session, s.parent_agent, s.parent_call, s.parent_run, s.agent_type, s.fork,
  s.root_sid, s.depth, s.agents,
  s.turns, s.items, s.tools, s.failures, s.calls, s.tok_in, s.tok_out, s.tok_cr, s.tok_cw, s.tok_think, s.cost,
  s.last_ask, s.answer, s.error,
  (SELECT count(*) FROM failures x WHERE x.sid = s.sid AND x.later_seq > 0)`

type scanner interface{ Scan(dest ...any) error }

func scanSession(row scanner, now time.Time) (Session, error) {
	var s Session
	var started, updated int64
	var archived, running, fork int
	var liveState, live string
	var p model.Parent
	err := row.Scan(&s.sid, &s.Source, &s.Agent, &s.ID, &s.Path, &s.Title, &s.Cwd, &s.Branch, &s.Model, &s.Version, &s.Platform,
		&started, &updated, &archived, &running, &liveState, &live,
		&p.Session, &p.Agent, &p.Call, &p.Run, &p.Type, &fork,
		&s.rootSid, &s.Depth, &s.Agents,
		&s.Stats.Turns, &s.Stats.Items, &s.Stats.Tools, &s.Stats.Failures, &s.Stats.Calls,
		&s.Stats.Usage.Input, &s.Stats.Usage.Output, &s.Stats.Usage.CacheRead, &s.Stats.Usage.CacheWrite, &s.Stats.Usage.Reasoning, &s.Stats.CostUSD,
		&s.LastAsk, &s.Answer, &s.Error, &s.Stats.PassedLater)
	if err != nil {
		return s, err
	}
	s.StartedAt, s.UpdatedAt = fromMs(started), fromMs(updated)
	s.Archived, s.Running = archived != 0, running != 0
	if p.Session != "" {
		p.Fork, p.Depth = fork != 0, s.Depth
		s.Parent = &p
		s.Root = s.Source + "/" + p.Session
	}
	s.Ref = s.Session.Ref()
	s.Active = !s.UpdatedAt.IsZero() && now.Sub(s.UpdatedAt) < ActiveWindow
	if liveState != "" && !s.UpdatedAt.IsZero() && now.Sub(s.UpdatedAt) < liveFor(liveState) {
		var l model.Live
		if json.Unmarshal([]byte(live), &l) == nil && l.State == liveState {
			s.Live = &l
		}
	}
	return s, nil
}

// Page is one window of a list.
type Page struct {
	Limit  int
	Offset int
}

// Sessions lists the sessions a filter selects, the most recently written first, and
// says how many there are in all.
func (ix *Index) Sessions(ctx context.Context, f Filter, p Page) ([]Session, int, error) {
	// Newest first: the index on the time serves it, when the store term lets it
	where, args := ix.widen(ctx, f, true).where()
	var total int
	if err := ix.ro.QueryRowContext(ctx, `SELECT count(*) FROM sessions s WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	rows, err := ix.ro.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions s WHERE `+where+
		` ORDER BY s.updated_at DESC, s.sid DESC LIMIT ? OFFSET ?`, append(args, p.Limit, p.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	now := time.Now()
	out := []Session{}
	for rows.Next() {
		s, err := scanSession(rows, now)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, s)
	}
	return out, total, rows.Err()
}

// Cwds lists the working directories the sessions of these stores ran in, each once.
func (ix *Index) Cwds(ctx context.Context, stores []string) ([]string, error) {
	if len(stores) == 0 {
		return nil, nil
	}
	args := make([]any, len(stores))
	for i, name := range stores {
		args[i] = name
	}
	rows, err := ix.ro.QueryContext(ctx, `SELECT DISTINCT cwd FROM sessions WHERE cwd != '' AND copy_of = 0 AND source IN (`+placeholders(len(stores))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cwd string
		if err := rows.Scan(&cwd); err != nil {
			return nil, err
		}
		out = append(out, cwd)
	}
	return out, rows.Err()
}

// Children lists the subagent transcripts directly beneath a session or an agent, in
// the order they started.
func (ix *Index) Children(ctx context.Context, s *Session) ([]Session, error) {
	rows, err := ix.ro.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions s WHERE s.parent_sid = ? AND s.copy_of = 0 ORDER BY s.started_at, s.sid`, s.sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	out := []Session{}
	for rows.Next() {
		c, err := scanSession(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ErrNotFound says no session answers to a reference.
var ErrNotFound = errors.New("no such session")

// AmbiguousError says a reference fits more than one session.
type AmbiguousError struct {
	Ref     string
	Matches []Session // the first of them, a dozen at most
	Total   int       // how many it fits
}

func (e *AmbiguousError) Error() string {
	refs := make([]string, 0, len(e.Matches))
	for _, m := range e.Matches {
		refs = append(refs, m.Ref)
	}
	list := strings.Join(refs, ", ")
	if more := e.Total - len(e.Matches); more > 0 {
		list += fmt.Sprintf(", and %d more", more)
	}
	return fmt.Sprintf("%q fits %d sessions: %s", e.Ref, max(e.Total, len(e.Matches)), list)
}

// SplitRef takes a reference apart: "claude/3f2a9c#4" is the store claude, the id (or
// the start of one) 3f2a9c, and turn 4. The store and the turn are optional. turn is -1
// when none was named.
func SplitRef(ref string) (source, id string, turn int) {
	ref = strings.TrimSpace(ref)
	turn = -1
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		if n, err := strconv.Atoi(ref[i+1:]); err == nil && n >= 0 {
			turn, ref = n, ref[:i]
		}
	}
	if i := strings.IndexByte(ref, '/'); i > 0 && !strings.ContainsAny(ref[:i], ". \\") {
		return ref[:i], ref[i+1:], turn
	}
	return "", ref, turn
}

// Resolve finds the one session a reference names: by its whole id, or by the start of
// it when that fits only one — the way a commit is named by the start of its hash. A
// copy answers as the session it was folded into.
func (ix *Index) Resolve(ctx context.Context, ref string, stores []string) (Session, error) {
	source, id, _ := SplitRef(ref)
	if id == "" {
		return Session{}, ErrNotFound
	}
	conds, args := []string{}, []any{}
	if len(stores) > 0 {
		conds = append(conds, "s.source IN ("+placeholders(len(stores))+")")
		for _, name := range stores {
			args = append(args, name)
		}
	}
	if source != "" {
		conds = append(conds, "(s.source = ? OR s.agent = ?)")
		args = append(args, source, source)
	}
	base := ""
	if len(conds) > 0 {
		base = strings.Join(conds, " AND ") + " AND "
	}
	now := time.Now()
	const shown = 12 // the sessions an ambiguous reference is answered with
	find := func(cond string, extra ...any) ([]Session, error) {
		rows, err := ix.ro.QueryContext(ctx, `SELECT `+sessionColumns+`, s.copy_of FROM sessions s WHERE `+base+cond+
			` ORDER BY s.parent_session != '', s.updated_at DESC LIMIT `+strconv.Itoa(shown), append(append([]any{}, args...), extra...)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []Session
		for rows.Next() {
			var copyOf int64
			s, err := scanSession(copyScanner{rows, &copyOf}, now)
			if err != nil {
				return nil, err
			}
			if copyOf != 0 {
				if orig, err := ix.bySID(ctx, copyOf); err == nil && (len(stores) == 0 || slices.Contains(stores, orig.Source)) {
					s = orig
				}
			}
			out = append(out, s)
		}
		return dedupe(out), rows.Err()
	}
	for _, attempt := range []struct {
		cond string
		arg  any
	}{
		{"s.id = ?", id},
		{"substr(s.id, 1, ?) = ?", nil},
		{"instr(s.path, ?) > 0", id},
	} {
		extra := []any{attempt.arg}
		if attempt.arg == nil {
			extra = []any{len(id), id}
		}
		found, err := find(attempt.cond, extra...)
		if err != nil {
			return Session{}, err
		}
		switch len(found) {
		case 0:
			continue
		case 1:
			return found[0], nil
		}
		// A session and its own subagents can share the start of an id; the session wins
		if found[0].Parent == nil && found[1].Parent != nil {
			return found[0], nil
		}
		// How many it fits, which may be more than are listed: a copy counted as the session
		// it was folded into
		var total int
		err = ix.ro.QueryRowContext(ctx, `SELECT count(DISTINCT CASE WHEN s.copy_of != 0 THEN s.copy_of ELSE s.sid END) FROM sessions s WHERE `+base+attempt.cond,
			append(append([]any{}, args...), extra...)...).Scan(&total)
		if err != nil {
			return Session{}, err
		}
		return Session{}, &AmbiguousError{Ref: ref, Matches: found, Total: total}
	}
	return Session{}, ErrNotFound
}

type copyScanner struct {
	rows   *sql.Rows
	copyOf *int64
}

func (c copyScanner) Scan(dest ...any) error { return c.rows.Scan(append(dest, c.copyOf)...) }

func dedupe(list []Session) []Session {
	seen := map[int64]bool{}
	out := list[:0]
	for _, s := range list {
		if !seen[s.sid] {
			seen[s.sid] = true
			out = append(out, s)
		}
	}
	return out
}

func (ix *Index) bySID(ctx context.Context, sid int64) (Session, error) {
	row := ix.ro.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions s WHERE s.sid = ?`, sid)
	s, err := scanSession(row, time.Now())
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

// Warning is a store that could not be read. Without it "no sessions" reads the same
// whether there are none or the store broke.
type Warning struct {
	Source   string `json:"source"`
	Location string `json:"location,omitempty"`
	Error    string `json:"error"`
}

// Warnings lists the stores whose last scan failed.
func (ix *Index) Warnings(ctx context.Context, stores []string) ([]Warning, error) {
	q, args := `SELECT source, location, error FROM source_state WHERE error != ''`, []any{}
	if len(stores) > 0 {
		q += ` AND source IN (` + placeholders(len(stores)) + `)`
		for _, name := range stores {
			args = append(args, name)
		}
	}
	rows, err := ix.ro.QueryContext(ctx, q+` ORDER BY source`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Warning{}
	for rows.Next() {
		var w Warning
		if err := rows.Scan(&w.Source, &w.Location, &w.Error); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Project is a working directory and the sessions that ran in it.
type Project struct {
	Cwd       string         `json:"cwd"`
	Sessions  int            `json:"sessions"`
	Turns     int            `json:"turns"`
	UpdatedAt time.Time      `json:"updatedAt,omitzero"`
	Sources   map[string]int `json:"sources"`
}

func (ix *Index) projectsQuery(ctx context.Context, f Filter) (string, []any) {
	where, args := ix.widen(ctx, f, !f.Since.IsZero()).where()
	return `SELECT s.cwd, s.source, count(*), sum(s.turns), max(s.updated_at) FROM sessions s WHERE ` + where + `
GROUP BY s.cwd, s.source`, args
}

// Projects groups the sessions a filter selects by working directory, the most recently
// used first.
func (ix *Index) Projects(ctx context.Context, f Filter) ([]Project, error) {
	query, args := ix.projectsQuery(ctx, f)
	rows, err := ix.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string]*Project{}
	var order []*Project
	for rows.Next() {
		var cwd, source string
		var n, turns int
		var updated int64
		if err := rows.Scan(&cwd, &source, &n, &turns, &updated); err != nil {
			return nil, err
		}
		p := by[cwd]
		if p == nil {
			p = &Project{Cwd: cwd, Sources: map[string]int{}}
			by[cwd] = p
			order = append(order, p)
		}
		p.Sessions += n
		p.Turns += turns
		p.Sources[source] += n
		if t := fromMs(updated); t.After(p.UpdatedAt) {
			p.UpdatedAt = t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(order))
	for _, p := range order {
		out = append(out, *p)
	}
	sortBy(out, func(a, b Project) bool { return a.UpdatedAt.After(b.UpdatedAt) })
	return out, nil
}
