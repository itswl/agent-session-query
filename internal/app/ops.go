package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/itswl/agent-session-query/internal/index"
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
	s.Resume = Resume(&s)
	return s, turn, nil
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

// Brief renders a session's handoff brief: its outline, and the failed calls of the
// turns the brief spells out.
func (a *App) Brief(ctx context.Context, s *index.Session, o render.BriefOptions) (string, error) {
	turns, err := a.Index.Outline(ctx, s)
	if err != nil {
		return "", err
	}
	if s.Stats.Failures > 0 {
		if o.Failures, err = a.Index.FailuresOf(ctx, s); err != nil {
			return "", err
		}
	}
	return render.Brief(s, turns, o)
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
		if withTurns {
			if e.Turns, err = a.Index.Outline(ctx, &list[i]); err != nil {
				return "", 0, err
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
	Version  string          `json:"version"`
	Index    string          `json:"index"`
	Totals   index.Totals    `json:"totals"`
	Sync     index.SyncState `json:"sync"`
	Stores   []StoreStatus   `json:"stores"`
	Warnings []index.Warning `json:"warnings"`
}

// Health reports the stores, what the index holds, and which stores could not be read.
func (a *App) Health(ctx context.Context) (Health, error) {
	h := Health{Version: Version, Index: a.Index.Path(), Sync: a.Index.State()}
	var err error
	if h.Totals, err = a.Index.Totals(ctx, a.Stores()); err != nil {
		return h, err
	}
	for _, st := range a.Set.All() {
		_, n, err := a.Index.Sessions(ctx, index.Filter{Stores: []string{st.Name}, Archived: true, Agents: true, Empty: true}, index.Page{Limit: 1})
		if err != nil {
			return h, err
		}
		_, listed, err := a.Index.Sessions(ctx, index.Filter{Stores: []string{st.Name}}, index.Page{Limit: 1})
		if err != nil {
			return h, err
		}
		h.Stores = append(h.Stores, StoreStatus{Source: st.Name, Title: st.Spec.Title, Location: st.Source.Location(), Found: st.Source.Exists(), Sessions: listed, Transcripts: n})
	}
	h.Warnings, err = a.Index.Warnings(ctx, a.Stores())
	return h, err
}
