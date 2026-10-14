package app

import (
	"context"
	"github.com/itswl/agent-session-query/internal/source"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
)

// Content search.
//
// No index. An index has to be written somewhere, maintained, and reasoned about when it
// goes stale, and that costs the "single binary, read-only, scp it anywhere" property.
// Measured locally, a bare scan over 562 MB / 143 sessions costs 1.9 s cold and 43-112 ms
// warm, and the warm number is the one an interactive page pays.
//
// The speed comes from ordering, not cleverness: run a case-insensitive Contains over the
// raw bytes first and only JSON-decode a line once it matches. That skips decoding for
// 99% of lines, and decoding is the expensive part of the scan.
//
// And the scan stops before the corpus runs out: once limit sessions have matched, the
// dispatcher stops handing out work (see search). The numbers above therefore price the
// worst case — a count-only limit=0 pass — rather than the usual page.
const (
	defaultSearchLimit      = 30 // how many sessions to return at most
	defaultSearchPerSession = 3  // how many hits per session at most
)

// searchableSource lets a source implement content search itself.
// Anything that does not falls back to the generic path: scan the session file (nearly
// every source is one jsonl per session).
type searchableSource interface {
	Search(ctx context.Context, r source.Record, q source.SearchQuery) []map[string]any
}

// searchOutcome is one search's results and its scale.
// scanned and matched are reported separately: once results are truncated to limit, the
// caller still needs to know how much was left out.
type searchOutcome struct {
	results []map[string]any
	scanned int // sessions actually scanned
	// matched counts the scanned sessions with a hit: exact when the scan ran to the end
	// of the corpus, a lower bound when it stopped at limit — scanStopped says which
	matched int
	stopped bool // the search was cancelled part-way; results are incomplete
	// Why results are short, kept apart rather than as one flag. A caller that sees fewer
	// hits than it asked for needs to know which knob to turn, and the two answers are
	// different knobs: sessionsCut is limit, hitsCut is per_session. One boolean covering
	// both says only "something was cut" and leaves the caller guessing.
	sessionsCut bool // the page was cut by Limit: more sessions matched, or the scan stopped there
	hitsCut     bool // some session had more hits than per_session returned
	// scanStopped: the scan stopped once limit sessions had matched instead of walking the
	// rest of the corpus, so an unscanned session may match too
	scanStopped bool
}

// search looks through every enabled source, newest session first.
//
// A result is a session. A Claude Code subagent's transcript is searched as part of the
// session it ran in, and its hits come back in that session's result, each naming the
// agent it is in (agentId, agent, agentType) so it can be opened. As results of their own,
// one session's agents crowded out every other session: 23 of the first 50 results for
// "error" were the agents of four sessions. Only a subagent whose session is outside the
// selection stands alone, as its own record with parentSessionId.
func (a *SessionQueryAPI) search(ctx context.Context, q source.SearchQuery) searchOutcome {
	type candidate struct {
		source source.SessionSource
		rec    source.Record
	}
	type unit struct {
		candidate
		agents []candidate // the subagents that ran in it, in the order they started
	}

	filter := sessionFilter{
		source: q.Source, project: q.Project, branch: q.Branch, since: q.Since, until: q.Until,
		subagents: !q.NoSubagents, archived: q.IncludeArchived,
	}
	all := []candidate{}
	if q.Pattern != "" {
		// Scoped: the pattern names one session, and only that one is searched. The source
		// picks between sessions of different CLIs that answer to the same pattern, the
		// way it does for get_session; the rest of the selection still applies to the one
		// session found, since a filter that was given is a filter that holds.
		if src, rec, ok := a.findSession(q.Pattern, q.Source); ok && filter.admits(rec) {
			all = append(all, candidate{source: src, rec: rec})
		}
	} else {
		a.eachSession(filter, func(src source.SessionSource, rec source.Record) {
			all = append(all, candidate{source: src, rec: rec})
		})
	}
	// A scoped search reads the one transcript it names; the rest is grouped by session
	units := make([]unit, 0, len(all))
	lead := map[string]int{} // source + session id → its unit
	for _, c := range all {
		if q.Pattern != "" || !c.rec.IsSubagent() {
			lead[c.source.Mode()+"\x00"+c.rec.SessionID] = len(units)
			units = append(units, unit{candidate: c})
		}
	}
	for _, c := range all {
		if q.Pattern != "" || !c.rec.IsSubagent() {
			continue
		}
		if i, ok := lead[c.source.Mode()+"\x00"+c.rec.ParentSessionID]; ok {
			units[i].agents = append(units[i].agents, c)
		} else {
			units = append(units, unit{candidate: c})
		}
	}
	for _, u := range units {
		sort.SliceStable(u.agents, func(i, j int) bool { return u.agents[i].rec.CreatedAt < u.agents[j].rec.CreatedAt })
	}
	// Newest first, so truncating at limit keeps the most recent — and so the scan can stop
	// once limit sessions have matched without changing what the page would hold
	sort.SliceStable(units, func(i, j int) bool { return units[i].rec.NewerThan(units[j].rec) })

	// Every transcript is a job of its own, a session's before its agents', so a session with
	// sixty agents is read across all the workers rather than by one of them: as one job per
	// session, a search that matched nothing took 3.5 s instead of 0.3. Each job's hits are
	// kept in its place in its session, and the session's result takes them in that order.
	type job struct{ unit, member int } // member 0 is the unit's own transcript, n its agents[n-1]
	order := []job{}
	hits := make([][][]map[string]any, len(units))
	for ui, u := range units {
		hits[ui] = make([][]map[string]any, len(u.agents)+1)
		for m := 0; m <= len(u.agents); m++ {
			order = append(order, job{ui, m})
		}
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > len(order) {
		workers = len(order)
	}
	// The counters are read while the scans are still running: the dispatcher stops handing
	// out work once limit sessions have matched.
	var scanned, found atomic.Int64
	matchedUnit := make([]atomic.Bool, len(units))
	scanStopped := false
	if workers > 0 {
		jobs := make(chan job)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					if source.Cancelled(ctx) {
						continue // drain the channel without starting more work
					}
					c := units[j.unit].candidate
					if j.member > 0 {
						c = units[j.unit].agents[j.member-1]
					}
					matches := safeParse(c.source.Mode(), "search results", func() []map[string]any {
						return searchOne(ctx, c.source, c.rec, q)
					})
					if j.member > 0 {
						for _, hit := range matches {
							hit["agentId"] = c.rec.SessionID
							hit["agent"] = c.rec.ShortKey
							if c.rec.AgentType != "" {
								hit["agentType"] = c.rec.AgentType
							}
						}
					} else {
						scanned.Add(1)
					}
					hits[j.unit][j.member] = matches
					if len(matches) > 0 && matchedUnit[j.unit].CompareAndSwap(false, true) {
						found.Add(1)
					}
				}
			}()
		}
		for _, j := range order {
			if source.Cancelled(ctx) {
				break
			}
			// Once limit sessions have matched, nothing further can reach the page:
			// candidates are newest-first and jobs are handed out in that order, so every
			// session still unscanned is older than every one that matched. What stopping
			// gives up is the exact total, which is why matched is a lower bound whenever
			// scanStopped says the scan ended here. (A session can be stopped part-way
			// through its agents; it keeps the hits read so far.)
			//
			// limit=0 is the count-only request — it wants exactly the total this would
			// give up — so it scans everything.
			if q.Limit > 0 && found.Load() >= int64(q.Limit) {
				scanStopped = true
				break
			}
			jobs <- j
		}
		close(jobs)
		wg.Wait()
	}

	out := searchOutcome{
		results:     []map[string]any{},
		scanned:     int(scanned.Load()),
		stopped:     source.Cancelled(ctx),
		scanStopped: scanStopped,
	}
	for i, parts := range hits {
		matches := []map[string]any{}
		for _, part := range parts {
			matches = append(matches, part...)
		}
		if len(matches) == 0 {
			continue
		}
		out.matched++
		if len(out.results) >= q.Limit {
			continue // still counted: sessionsCut says the page cut these matches off
		}
		// The extra hit ProbeLimit asked for is the evidence, and it is dropped here so no
		// caller ever sees more than it asked for.
		//
		// Counted only for sessions that reach the results. A session limit dropped
		// entirely is the sessions reason, not this one, and raising hits for it would send
		// the caller to turn per_session — which changes nothing, because every session it
		// can actually see came back whole.
		if len(matches) > q.PerSession {
			matches = matches[:q.PerSession]
			out.hitsCut = true
		}
		item := units[i].rec.Public()
		item["matches"] = matches
		item["matchCount"] = len(matches)
		out.results = append(out.results, item)
	}
	out.sessionsCut = out.matched > len(out.results) || out.scanStopped
	return out
}

// searchOne searches inside one session, using the source's own implementation when it
// has one and scanning the session file otherwise.
func searchOne(ctx context.Context, src source.SessionSource, rec source.Record, q source.SearchQuery) []map[string]any {
	if s, ok := src.(searchableSource); ok {
		return s.Search(ctx, rec, q)
	}
	return source.SearchFile(ctx, rec.File, q)
}
