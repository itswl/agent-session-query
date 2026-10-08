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
func (a *SessionQueryAPI) search(ctx context.Context, q source.SearchQuery) searchOutcome {
	type candidate struct {
		source source.SessionSource
		rec    source.Record
	}

	all := []candidate{}
	if q.Pattern != "" {
		// Scoped: the pattern names one session, and only that one is searched
		if src, rec, ok := a.findSession(q.Pattern, ""); ok {
			all = append(all, candidate{source: src, rec: rec})
		}
	} else {
		for _, src := range a.activeSources() {
			for _, rec := range a.recordsOf(src) {
				if !q.Since.IsZero() && (rec.SortAt().IsZero() || rec.SortAt().Before(q.Since)) {
					continue
				}
				if !q.Until.IsZero() && (rec.SortAt().IsZero() || rec.SortAt().After(q.Until)) {
					continue
				}
				all = append(all, candidate{source: src, rec: rec})
			}
		}
	}
	// Newest first, so truncating at limit keeps the most recent — and so the scan can stop
	// once limit sessions have matched without changing what the page would hold
	sort.SliceStable(all, func(i, j int) bool { return all[i].rec.NewerThan(all[j].rec) })

	hits := make([][]map[string]any, len(all))
	workers := runtime.GOMAXPROCS(0)
	if workers > len(all) {
		workers = len(all)
	}
	// The counters are read while the scans are still running: the dispatcher stops handing
	// out sessions once limit of them have matched.
	var scanned, found atomic.Int64
	scanStopped := false
	if workers > 0 {
		jobs := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					if source.Cancelled(ctx) {
						continue // drain the channel without starting more work
					}
					c := all[i]
					matches := safeParse(c.source.Mode(), "search results", func() []map[string]any {
						return searchOne(ctx, c.source, c.rec, q)
					})
					hits[i] = matches
					scanned.Add(1)
					if len(matches) > 0 {
						found.Add(1)
					}
				}
			}()
		}
		for i := range all {
			if source.Cancelled(ctx) {
				break
			}
			// Once limit sessions have matched, nothing further can reach the page:
			// candidates are newest-first and jobs are handed out in that order, so every
			// session still unscanned is older than every one that matched. What stopping
			// gives up is the exact total, which is why matched is a lower bound whenever
			// scanStopped says the scan ended here.
			//
			// limit=0 is the count-only request — it wants exactly the total this would
			// give up — so it scans everything.
			if q.Limit > 0 && found.Load() >= int64(q.Limit) {
				scanStopped = true
				break
			}
			jobs <- i
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
	for i, matches := range hits {
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
		item := all[i].rec.Public()
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
