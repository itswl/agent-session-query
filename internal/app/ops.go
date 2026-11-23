package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/keep"
	"github.com/itswl/agent-session-query/internal/render"
)

// Resolve finds the one session a reference names — "claude/3f2a9c", or just the start
// of an id — and the turn it names ("…#4"), -1 when it names none. Its errors are worded
// for whoever typed the reference.
func (a *App) Resolve(ctx context.Context, ref string) (index.Session, int, error) {
	_, _, turn := index.SplitRef(ref)
	s, err := a.Index.Resolve(ctx, ref, a.Stores())
	if err != nil {
		var amb *index.AmbiguousError
		switch {
		case errors.As(err, &amb):
			return s, turn, fmt.Errorf("%w — give more of the id, or name the store: <store>/<id>", err)
		case errors.Is(err, index.ErrNotFound):
			return s, turn, fmt.Errorf("%w: %q", index.ErrNotFound, ref)
		}
		return s, turn, err
	}
	Resumes(&s)
	a.keeping(&s)
	return s, turn, nil
}

// Self is the session an agent asking after its own is working in, and how it was found.
type Self struct {
	index.Session
	// ByID says the agent's CLI named the session. Otherwise it is the session most
	// recently written to in one of the directories the agent asked from.
	ByID bool
	// Newer is, for a session named, another of the same CLI written to in its directory
	// since: the name is the one the CLI gave when the agent started asking, and the agent
	// may have moved on to a new session (Claude Code's /clear), or be working beside it.
	Newer *index.Session
}

// Self finds the session an agent is working in. id is that session's id as the agent's
// CLI gives it, if it does — source names the CLI — and the session is that one, when the
// index has it. Otherwise it is the session most recently written to in one of dirs, the
// directories the agent asks from: an agent at work writes its session as it goes, so
// that is its own, unless another works in the same directory. Only this machine's stores
// are looked in: a labelled store is another machine's, or a copy.
func (a *App) Self(ctx context.Context, source, id string, dirs []string) (Self, error) {
	var stores []string
	for _, name := range a.Stores() {
		if !strings.Contains(name, "@") && (source == "" || name == source) {
			stores = append(stores, name)
		}
	}
	if len(stores) == 0 {
		return Self{}, fmt.Errorf("%w: this asq does not read %s's sessions", index.ErrNotFound, source)
	}
	if id != "" {
		s, err := a.Index.Resolve(ctx, source+"/"+id, stores)
		var amb *index.AmbiguousError
		switch {
		case err == nil && s.ID == id:
			if s, err = a.latestOf(ctx, s); err != nil {
				return Self{}, err
			}
			self := Self{Session: s, ByID: true}
			if s.Cwd != "" {
				later, _, err := a.Index.Sessions(ctx, index.Filter{Stores: stores, In: []string{s.Cwd}, Since: s.UpdatedAt}, index.Page{Limit: 2})
				if err != nil {
					return Self{}, err
				}
				for i := range later {
					if later[i].Ref != s.Ref && later[i].UpdatedAt.After(s.UpdatedAt) {
						self.Newer = &Listed(later[i : i+1])[0]
						break
					}
				}
			}
			Resumes(&self.Session)
			return self, nil
		case err != nil && !errors.Is(err, index.ErrNotFound) && !errors.As(err, &amb):
			return Self{}, err
		}
	}
	// Each directory by every path that may name it as a CLI recorded it (see Paths). The
	// answer names them as they were given.
	var named []string
	for _, d := range dirs {
		if d != "" {
			named = appendNew(named, d)
		}
	}
	if len(named) == 0 {
		return Self{}, fmt.Errorf("%w: no directory to look in", index.ErrNotFound)
	}
	where, err := a.Paths(ctx, named, false)
	if err != nil {
		return Self{}, err
	}
	list, _, err := a.Index.Sessions(ctx, index.Filter{Stores: stores, In: where}, index.Page{Limit: 1})
	if err != nil {
		return Self{}, err
	}
	if len(list) == 0 {
		return Self{}, fmt.Errorf("%w: none has run in %s", index.ErrNotFound, strings.Join(named, " or "))
	}
	return Self{Session: Listed(list)[0]}, nil
}

// latestOf is the session its CLI goes on writing under the id s has: s itself, or where
// the CLI goes on with a session in a file of its own under that id, the latest of those.
// A Codex thread reverted goes on in a rollout its adapter names <thread>_<rollout>, and
// the thread Codex names is that one, not the rollout the revert took turns back from.
func (a *App) latestOf(ctx context.Context, s index.Session) (index.Session, error) {
	spec, ok := catalog.Spec(s.Source)
	if !ok || spec.ResumeID == nil {
		return s, nil
	}
	list, _, err := a.Index.Sessions(ctx, index.Filter{Stores: []string{s.Source}, Text: s.ID, Archived: true}, index.Page{Limit: 12})
	if err != nil {
		return s, err
	}
	for _, l := range list { // the most recently written first
		if l.ID != s.ID && spec.ResumeID(l.ID) == s.ID && l.UpdatedAt.After(s.UpdatedAt) {
			return l, nil
		}
	}
	return s, nil
}

// Paths is a directory by every path a CLI may have recorded as a session's working
// directory: each of dirs as it was given, cleaned — which on Windows also writes each /
// as \ — and with its links resolved, as /tmp is /private/tmp on a Mac; and every working
// directory this machine's sessions ran in whose links lead to the same place, or with
// below to a place beneath it. That last is the only way to the path a CLI recorded
// through a link when the directory was given with its links resolved — by a process
// with no PWD, or after cd -P: nothing in /private/tmp/x says that /tmp/x leads there.
// Directories are compared without regard to case, as the index's filters compare them.
func (a *App) Paths(ctx context.Context, dirs []string, below bool) ([]string, error) {
	var out, reals []string
	for _, d := range dirs {
		if d == "" {
			continue
		}
		out = appendNew(out, d, filepath.Clean(d))
		real, err := filepath.EvalSymlinks(d)
		if err != nil {
			continue // not on this machine: nothing recorded here leads to it
		}
		out = appendNew(out, real)
		reals = appendNew(reals, real)
	}
	if len(reals) == 0 {
		return out, nil
	}
	// A labelled store's paths are another machine's; the copies asq keeps are of this
	// machine's own stores, and their paths are this machine's
	var stores []string
	for _, st := range a.Set.All() {
		if !strings.Contains(st.Name, "@") || keep.IsKept(st.Source) {
			stores = append(stores, st.Name)
		}
	}
	cwds, err := a.Index.Cwds(ctx, stores)
	if err != nil {
		return nil, err
	}
	// A directory the paths given already hold, as the index's filter holds them, is not
	// looked at: a --project of . in a repository is every directory the repository's
	// sessions ran in, and each would be a term of every query, and a look at the disk
	given := len(out)
	for _, cwd := range cwds {
		if ctx.Err() != nil {
			break
		}
		if slices.Contains(out, cwd) || below && slices.ContainsFunc(out[:given], func(d string) bool { return dirHolds(d, cwd, true) }) {
			continue
		}
		real, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(reals, func(r string) bool { return dirHolds(r, real, below) }) {
			out = append(out, cwd)
		}
	}
	return out, ctx.Err()
}

// dirHolds reports whether path is dir, or with below one beneath it, without regard to case
// or to the separator it is written with: /work/api holds /work/api/web, and not
// /work/api-old.
func dirHolds(dir, path string, below bool) bool {
	dir, path = strings.ToLower(dir), strings.ToLower(path)
	if path == dir {
		return true
	}
	base := strings.TrimRight(dir, `/\`)
	return below && (path == base || strings.HasPrefix(path, base+"/") || strings.HasPrefix(path, base+`\`))
}

// appendNew appends to list each of paths it does not hold yet.
func appendNew(list []string, paths ...string) []string {
	for _, p := range paths {
		if !slices.Contains(list, p) {
			list = append(list, p)
		}
	}
	return list
}

// Above lists the transcripts a subagent's sits beneath, the session it ran in first:
// usually that one alone, and for an agent another agent started, the agents between.
func (a *App) Above(ctx context.Context, s *index.Session) ([]index.Session, error) {
	chain := []index.Session{}
	for cur, depth := *s, 0; cur.Parent != nil && depth < 8; depth++ {
		parent, ok, err := a.Index.Parent(ctx, &cur)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		chain = append([]index.Session{parent}, chain...)
		cur = parent
	}
	return Listed(chain), nil
}

// Brief renders a session's handoff brief: its outline, its failed calls, each with where
// the same command later passed, and the files it names as changed, each with how it
// stands now — looked up as the files report looks them up, a file within lookWait and not
// at all in a folder a look is waiting in, and for what the brief names alone.
func (a *App) Brief(ctx context.Context, s *index.Session, o render.BriefOptions) (string, error) {
	turns, err := a.Index.Outline(ctx, s)
	if err != nil {
		return "", err
	}
	plan, err := render.PlanBrief(s, turns, o)
	if err != nil {
		return "", err
	}
	look := newLooker(a)
	look.sessions[s.Ref] = s
	if s.Stats.Failures > 0 {
		if o.Failures, err = a.Index.FailuresOf(ctx, s); err != nil {
			return "", err
		}
	}
	o.Files = plan.Files
	for i := range o.Files {
		if ctx.Err() != nil {
			break
		}
		o.Files[i].Now, o.Files[i].Waiting = look.fileNow(ctx, &o.Files[i])
	}
	return render.Brief(s, turns, o)
}

// BriefOf reads what a brief is asked to cover as the command line, the API and the MCP
// tool take it: one turn, the turn to start from, or a moment to start after — "all"
// for the whole session, which is no moment at all. Each says which turns the brief
// covers, so it is given one of them: given two, one would be taken and the other
// dropped without a word, and the caller handed a brief of turns it did not ask for.
func BriefOf(turn, from int, since string) (render.BriefOptions, error) {
	since = strings.TrimSpace(since)
	var asked []string
	if turn != 0 {
		asked = append(asked, fmt.Sprintf("turn %d", turn))
	}
	if from != 0 {
		asked = append(asked, fmt.Sprintf("from %d", from))
	}
	if since != "" {
		asked = append(asked, "since "+since)
	}
	if n := len(asked); n > 1 {
		return render.BriefOptions{}, fmt.Errorf("%s and %s each say which turns the brief covers: give one of them",
			strings.Join(asked[:n-1], ", "), asked[n-1])
	}
	o := render.BriefOptions{Turn: turn, From: from}
	if since == "all" {
		o.All = true
		return o, nil
	}
	var err error
	if o.Since, err = When(since, time.Now()); err != nil {
		return o, fmt.Errorf("since: %w", err)
	}
	return o, nil
}

// Handoff is what a brief covers when it is handed to someone, as the command line and
// the MCP tool hand it: what is named — a turn, the turn to start from, a moment — and
// with none of them the whole session. A brief of the latest turn alone hands over
// where the session ended without how it got there: its last ask may be "delete the
// scratch script", and the fix made before it is not in it.
func Handoff(turn, from int, since string) (render.BriefOptions, error) {
	if turn == 0 && from == 0 && strings.TrimSpace(since) == "" {
		since = "all"
	}
	return BriefOf(turn, from, since)
}

// NamedTurn is the turn a brief is asked for when its ref may end #N — named, -1 or 0 for
// none — and turn may name one too: they are one turn, or the brief is not told which.
func NamedTurn(named, turn int) (int, error) {
	switch {
	case named <= 0:
		return turn, nil
	case turn != 0 && turn != named:
		return 0, fmt.Errorf("the ref's #%d and turn %d each say which turn the brief covers: give one of them", named, turn)
	}
	return named, nil
}

// Pack renders the sessions a query selects as one document: the most recent limit of
// them, oldest first. It also says how many sessions the document holds.
func (a *App) Pack(ctx context.Context, q Query, limit int, withTurns bool) (string, int, error) {
	f, err := a.Filter(q)
	if err != nil {
		return "", 0, err
	}
	if limit <= 0 {
		limit = 20
	}
	list, total, err := a.Index.Sessions(ctx, f, index.Page{Limit: limit})
	if err != nil {
		return "", 0, err
	}
	entries := make([]render.PackEntry, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- { // the list is newest first; a pack reads oldest first
		e := render.PackEntry{Session: list[i]}
		// A session of more turns than one was opened for its first ask, and ended on
		// another: the pack says both
		if withTurns || list[i].Stats.Turns > 1 {
			turns, err := a.Index.Outline(ctx, &list[i])
			if err != nil {
				return "", 0, err
			}
			if withTurns {
				e.Turns = turns
			}
			for j := range turns {
				if turns[j].Index > 0 {
					e.First = &turns[j]
					break
				}
			}
		}
		entries = append(entries, e)
	}
	return render.Pack(entries, total, withTurns), len(entries), nil
}

// TurnRange reads which turns of a session are meant: the one a reference named ("…#4"),
// or a spec — "3", "3-5", "last", "all". It reports false when neither names any, which
// is a request for the session's outline.
func TurnRange(spec string, named int) (index.TurnRange, bool, error) {
	spec = strings.TrimSpace(spec)
	switch {
	case spec == "" && named >= 0:
		return index.TurnRange{First: named, Last: named}, true, nil
	case spec == "":
		return index.TurnRange{}, false, nil
	case spec == "last":
		return index.TurnRange{First: 1, Last: -1, Tail: 1}, true, nil
	case spec == "all":
		return index.TurnRange{First: 0, Last: -1}, true, nil
	}
	bad := fmt.Errorf("turns: want 3, 3-5, last or all, got %q", spec)
	from, to, isRange := strings.Cut(spec, "-")
	first, err := strconv.Atoi(strings.TrimSpace(from))
	if err != nil || first < 0 {
		return index.TurnRange{}, false, bad
	}
	last := first
	if isRange {
		if last, err = strconv.Atoi(strings.TrimSpace(to)); err != nil || last < first {
			return index.TurnRange{}, false, bad
		}
	}
	return index.TurnRange{First: first, Last: last}, true, nil
}

// StoreStatus is one configured store: where it is, and what the index holds of it.
type StoreStatus struct {
	Source   string `json:"source"`
	Title    string `json:"title"`
	Location string `json:"location"`
	Found    bool   `json:"found"`
	// Sessions is what the list shows of the store; Transcripts everything read from
	// it — subagents', archived and empty ones included.
	Sessions    int `json:"sessions"`
	Transcripts int `json:"transcripts"`
}

// Health is where the program stands.
type Health struct {
	Version string `json:"version"`
	// Executable is the file this process was started from, and Replaced says that an
	// upgrade has been installed over it since: the process goes on with the old one until
	// it is restarted (see Program)
	Executable string          `json:"executable"`
	Replaced   bool            `json:"replaced"`
	Index      string          `json:"index"`
	Totals     index.Totals    `json:"totals"`
	Sync       index.SyncState `json:"sync"`
	Stores     []StoreStatus   `json:"stores"`
	Warnings   []index.Warning `json:"warnings"`
}

// Looked is a store asq looked in, and what it found there.
type Looked struct {
	Source   string
	Location string
	Found    bool
	Unread   bool // there, and could not be read
}

// State says what was found there, in a few words.
func (l Looked) State() string {
	switch {
	case !l.Found:
		return "not there"
	case l.Unread:
		return "there, and could not be read"
	}
	return "there, and empty"
}

// NothingFound is every store this process reads, found or not, when the index holds no
// session of any of them; nil when it holds one. An empty answer then says where asq looked,
// rather than only that there is nothing: a service started without the variable that
// moves a CLI's store, or a container with a store mounted in the wrong place, finds none.
func (a *App) NothingFound(ctx context.Context) ([]Looked, error) {
	_, n, err := a.Index.Sessions(ctx, index.Filter{Stores: a.Stores(), Archived: true, Agents: true, Empty: true}, index.Page{Limit: 1})
	if err != nil || n > 0 {
		return nil, err
	}
	warnings, err := a.Index.Warnings(ctx, a.Stores())
	if err != nil {
		return nil, err
	}
	unread := map[string]bool{}
	for _, w := range warnings {
		unread[w.Source] = true
	}
	var looked []Looked
	for _, st := range a.Set.All() {
		if keep.IsKept(st.Source) && !st.Source.Exists() {
			continue // nothing of that CLI has been kept: not a place a CLI's sessions were looked for
		}
		looked = append(looked, Looked{Source: st.Name, Location: st.Source.Location(), Found: st.Source.Exists(), Unread: unread[st.Name]})
	}
	return looked, nil
}

// Health reports the stores, what the index holds, and which stores could not be read.
func (a *App) Health(ctx context.Context) (Health, error) {
	h := Health{Version: Version, Index: a.Index.Path(), Sync: a.Index.State()}
	h.Executable, h.Replaced = Program()
	var err error
	if h.Totals, err = a.Index.Totals(ctx, a.Stores()); err != nil {
		return h, err
	}
	for _, st := range a.Set.All() {
		if keep.IsKept(st.Source) && !st.Source.Exists() {
			continue // nothing of that CLI has been kept
		}
		_, n, err := a.Index.Sessions(ctx, index.Filter{Stores: []string{st.Name}, Archived: true, Agents: true, Empty: true}, index.Page{Limit: 1})
		if err != nil {
			return h, err
		}
		// What the list shows of it, which reads every store: a copy folded into the
		// session it is a copy of is listed as that one, not as a session of its own store
		_, listed, err := a.Index.Sessions(ctx, index.Filter{Stores: a.Stores(), Store: st.Name}, index.Page{Limit: 1})
		if err != nil {
			return h, err
		}
		h.Stores = append(h.Stores, StoreStatus{Source: st.Name, Title: st.Spec.Title, Location: st.Source.Location(), Found: st.Source.Exists(), Sessions: listed, Transcripts: n})
	}
	h.Warnings, err = a.Index.Warnings(ctx, a.Stores())
	return h, err
}
