package app

import (
	"fmt"
	"github.com/itswl/agent-session-query/internal/source"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SessionQueryAPI dispatches requests to the individual source adapters.
//
// Session lists are cached per source with a short TTL (2 seconds by default, tunable
// with --cache-ttl, 0 disables it). Both lookup and listing go through it, so a single
// request never has to rescan every session file. Messages and final results always read
// from disk; the cache only covers the "which sessions exist" layer of metadata.
type SessionQueryAPI struct {
	sources  []source.SessionSource
	resolver source.Resolver
	cacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]*cachedRecords
}

// cachedRecords is one source's list cache.
//
// scan guarantees at most one scan per source at a time. When the cache has just expired
// and several requests arrive together, having each rescan the directory is pure waste
// (a cache stampede) — now one scans and the rest wait for its result.
type cachedRecords struct {
	scan    sync.Mutex
	at      time.Time
	records []source.Record
}

func newSessionQueryAPI(sources []source.SessionSource, cacheTTLSeconds float64) *SessionQueryAPI {
	api := newSessionQueryAPIWithResolver(nil, cacheTTLSeconds)
	api.sources = sources
	return api
}

// newSessionQueryAPIWithResolver builds an API whose source set is re-resolved on every
// query: the enabled sources are whatever the resolver reports at that moment, so a source
// created after startup (a CLI's first session) is listed without a restart.
func newSessionQueryAPIWithResolver(resolver source.Resolver, cacheTTLSeconds float64) *SessionQueryAPI {
	if cacheTTLSeconds < 0 {
		cacheTTLSeconds = 0
	}
	return &SessionQueryAPI{
		resolver: resolver,
		cacheTTL: time.Duration(cacheTTLSeconds * float64(time.Second)),
		cache:    map[string]*cachedRecords{},
	}
}

// activeSources is the set to query right now: the resolver's current answer when one was
// configured, otherwise the fixed slice the process was built with.
func (a *SessionQueryAPI) activeSources() []source.SessionSource {
	if a.resolver != nil {
		return a.resolver.Sources()
	}
	return a.sources
}

// recordsOf returns one source's session list (TTL cached)
func (a *SessionQueryAPI) recordsOf(src source.SessionSource) []source.Record {
	if a.cacheTTL <= 0 {
		return safeList(src)
	}

	a.mu.Lock()
	entry, ok := a.cache[src.Mode()]
	if !ok {
		entry = &cachedRecords{}
		a.cache[src.Mode()] = entry
	}
	a.mu.Unlock()

	entry.scan.Lock()
	defer entry.scan.Unlock()
	if !entry.at.IsZero() && time.Since(entry.at) < a.cacheTTL {
		return entry.records
	}
	entry.records = safeList(src)
	entry.at = time.Now()
	return entry.records
}

// safeList keeps one failing source from taking down the whole list
func safeList(src source.SessionSource) (out []source.Record) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[WARN] %s failed to list sessions: %v\n", src.Mode(), rec)
			out = nil
		}
	}()
	return src.List()
}

// listSessions returns the merged session list plus a weak validator (used as the ETag).
//
// The web page polls every 10 seconds and the list has almost always not changed. With an
// ETag those polls end at a 304, instead of serialising and transferring the entire list
// again every time.
//
// Subagents' transcripts are left out unless asked for: a session's own row says how many
// ran in it, and a list of everything would be more than half agents — 291 of them beside
// 246 sessions here. So are the sessions their CLI has archived, as each CLI leaves them
// out of its own list; archived says how many there are, listed or not.
func (a *SessionQueryAPI) listSessions(subagents, archived bool) ([]map[string]any, string, int) {
	all := []source.Record{}
	archivedTotal := 0
	for _, src := range a.activeSources() {
		for _, rec := range a.recordsOf(src) {
			if rec.Archived && !rec.IsSubagent() {
				archivedTotal++
			}
			if (subagents || !rec.IsSubagent()) && (archived || !rec.Archived) {
				all = append(all, rec)
			}
		}
	}
	// Newest update time first; equal times keep source order (stable sort)
	sort.SliceStable(all, func(i, j int) bool { return all[i].NewerThan(all[j]) })

	out := make([]map[string]any, 0, len(all))
	for _, item := range all {
		out = append(out, item.Public())
	}
	version := listVersion(all, a.listWarnings())
	if archivedTotal > 0 {
		// A session archived or brought back changes the count without touching the rows
		version = strings.TrimSuffix(version, `"`) + "a" + strconv.Itoa(archivedTotal) + `"`
	}
	return out, version, archivedTotal
}

// listWarnings names the sources whose last list failed, as {source, error}.
//
// It reads what the last scan hit instead of starting one: /health answers this without
// authentication, and an anonymous request should not be able to set off a walk over every
// session directory on the machine. A caller that wants a fresh answer lists first (which
// /sessions and the MCP tools do).
func (a *SessionQueryAPI) listWarnings() []map[string]any {
	out := []map[string]any{}
	for _, src := range a.activeSources() {
		reporter, ok := src.(source.ListErrorReporter)
		if !ok {
			continue
		}
		if err := reporter.ListError(); err != nil {
			out = append(out, map[string]any{
				"source": src.Mode(),
				"error":  err.Error(),
			})
		}
	}
	return out
}

// listVersion is the list's weak validator: membership, update time, status, or a
// background message count landing all change it.
//
// The warnings go in as well. A source that stops being readable changes none of the
// records — it just stops contributing any — and a 304 would then hide the one thing that
// did change.
func listVersion(records []source.Record, warnings []map[string]any) string {
	h := fnv.New64a()
	// The server's own version is part of it: after an upgrade every page's stored tag
	// stops matching, its next poll gets a full answer carrying the new version, and the
	// page reloads itself rather than running the previous version's script against the
	// new API until the browser happens to discard it.
	_, _ = h.Write([]byte(buildVersion))
	_, _ = h.Write([]byte{0x1e})
	for _, r := range records {
		for _, field := range []string{r.Source, r.Key, r.UpdatedAt, r.Status} {
			_, _ = h.Write([]byte(field))
			_, _ = h.Write([]byte{0})
		}
		// The background count arriving is a change like any other: without it in the
		// tag, a revalidating page keeps its 304 and its count-less body — the one that
		// renders "counting…" — until something unrelated moves.
		if r.HasCount {
			_, _ = h.Write([]byte(strconv.FormatInt(int64(r.MessageCount), 10)))
		}
		// So is a new subagent, which changes its session's row and not its file
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strconv.Itoa(r.Subagents)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte{0x1e})
	}
	for _, w := range warnings {
		_, _ = h.Write([]byte(source.ToStr(w["source"])))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(source.ToStr(w["error"])))
		_, _ = h.Write([]byte{0x1e})
	}
	return `W/"` + strconv.FormatUint(h.Sum64(), 16) + `"`
}

// findSession picks the best match across every enabled source (exact hits win, and no
// source shadows another)
// findSession locates the one session a pattern names. sourceWanted, when not empty,
// restricts the search to that source — an MCP client can hold a sessionId from
// list_sessions and the mode name, and ids alone are not unique across sources.
func (a *SessionQueryAPI) findSession(pattern, sourceWanted string) (source.SessionSource, source.Record, bool) {
	pattern = strings.TrimSpace(pattern)
	if strings.HasPrefix(pattern, "Run: ") {
		pattern = pattern[len("Run: "):]
	}
	if strings.HasPrefix(pattern, "Session: ") {
		pattern = pattern[len("Session: "):]
	}
	if pattern == "" {
		return nil, source.Record{}, false
	}
	patternLower := source.NormalizeForMatch(pattern)

	var bestSource source.SessionSource
	var bestRecord source.Record
	bestRank := -1
	for _, src := range a.activeSources() {
		if !sourceMatchesWanted(src.Mode(), sourceWanted) {
			continue
		}
		for _, item := range a.recordsOf(src) {
			rank := item.MatchRank(patternLower)
			if rank == -1 {
				continue
			}
			if bestRank == -1 || rank < bestRank {
				bestSource, bestRecord, bestRank = src, item, rank
			}
		}
	}
	if bestSource == nil {
		return nil, source.Record{}, false
	}
	return bestSource, bestRecord, true
}

func (a *SessionQueryAPI) getSession(pattern, sourceWanted string) (map[string]any, bool) {
	_, item, ok := a.findSession(pattern, sourceWanted)
	if !ok {
		return nil, false
	}
	return item.Public(), true
}

// safeParse keeps one session's parse blowing up from turning the request into a 500:
// log it and treat the result as "nothing parsed". Session files are written by other
// programs and their format can change at any time — messages and final both go through
// this.
func safeParse[T any](mode, what string, parse func() T) (out T) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[WARN] %s failed to parse %s: %v\n", mode, what, rec)
			var zero T
			out = zero
		}
	}()
	return parse()
}

func (a *SessionQueryAPI) getMessages(pattern, sourceWanted string, q source.MessageQuery) ([]map[string]any, bool) {
	src, item, ok := a.findSession(pattern, sourceWanted)
	if !ok {
		return nil, false
	}
	messages := safeParse(src.Mode(), "messages", func() []map[string]any {
		return src.Messages(item, q)
	})
	if messages == nil {
		messages = []map[string]any{}
	}
	return messages, true
}

func (a *SessionQueryAPI) getFinalMessage(pattern, sourceWanted string) (map[string]any, bool) {
	src, item, ok := a.findSession(pattern, sourceWanted)
	if !ok {
		return nil, false
	}

	result := safeParse(src.Mode(), "the final result", func() map[string]any {
		return src.Final(item)
	})

	if result == nil {
		status := item.Status
		if status == "" {
			status = "unknown"
		}
		result = map[string]any{
			"status":       status,
			"isFinal":      false,
			"isProcessing": status == "running",
			"messageCount": 0,
			"source":       item.Source,
			"error":        "Session file not available yet (session may be still initializing)",
		}
	}
	return result, true
}

// listProjects groups sessions by project.
//
// This is the one thing this tool can do that the others cannot: what you did on a given
// repository with Claude Code, Codex and Gemini, all in a single view.
// Note that hermes / openclaw have neither cwd nor project and land in ungrouped.
func (a *SessionQueryAPI) listProjects(archived bool) ([]map[string]any, int) {
	type bucket struct {
		sessions  int
		sources   map[string]int
		latest    source.Record
		hasLatest bool
	}
	order := []string{}
	buckets := map[string]*bucket{}
	ungrouped := 0

	for _, src := range a.activeSources() {
		for _, rec := range a.recordsOf(src) {
			if rec.IsSubagent() {
				continue // a project counts conversations; its sessions' agents are inside them
			}
			if rec.Archived && !archived {
				continue
			}
			name := rec.ProjectName()
			if name == "" {
				ungrouped++
				continue
			}
			b, ok := buckets[name]
			if !ok {
				b = &bucket{sources: map[string]int{}}
				buckets[name] = b
				order = append(order, name)
			}
			b.sessions++
			b.sources[rec.Source]++
			if !b.hasLatest || rec.NewerThan(b.latest) {
				b.latest, b.hasLatest = rec, true
			}
		}
	}

	out := make([]map[string]any, 0, len(order))
	for _, name := range order {
		b := buckets[name]
		sources := make([]string, 0, len(b.sources))
		for mode := range b.sources {
			sources = append(sources, mode)
		}
		sort.Strings(sources)
		out = append(out, map[string]any{
			"project":       name,
			"shortName":     filepath.Base(name),
			"sessions":      b.sessions,
			"sources":       sources,
			"sourceCounts":  b.sources,
			"updatedAt":     b.latest.UpdatedAt,
			"latestSession": b.latest.SessionID,
			"isActive":      b.latest.IsActive(),
		})
	}
	// Most recently touched projects first
	sort.SliceStable(out, func(i, j int) bool {
		return source.ToStr(out[i]["updatedAt"]) > source.ToStr(out[j]["updatedAt"])
	})
	return out, ungrouped
}

func sourceModes(sources []source.SessionSource) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.Mode())
	}
	return out
}

// sourceMatchesWanted reports whether a source answers a source argument from the MCP
// tools. The exact instance name matches, and the bare mode name matches every labeled
// instance of it: an MCP client holding "claude" from an earlier listing still finds a
// session that now lives in claude:probe-watch.
func sourceMatchesWanted(mode, wanted string) bool {
	return wanted == "" || mode == wanted || strings.HasPrefix(mode, wanted+":")
}

// sourceName reads a source argument: a mode name, which reaches every instance of that
// CLI, or the full name of one labeled instance (claude:box2) — the name the list and the
// page's source filter show. Anything else is refused: a typo should fail loudly, not
// quietly match nothing.
func (a *SessionQueryAPI) sourceName(raw string) (string, error) {
	want := strings.TrimSpace(raw)
	if want == "" || slices.Contains(source.KnownModes, want) {
		return want, nil
	}
	names := append([]string(nil), source.KnownModes...)
	for _, src := range a.activeSources() {
		if src.Mode() == want {
			return want, nil
		}
		if strings.Contains(src.Mode(), ":") {
			names = append(names, src.Mode())
		}
	}
	return "", fmt.Errorf("unknown source %q (choose from: %s)", want, strings.Join(names, " / "))
}

// sessionFilter is the selection every surface that picks sessions shares — the list, a
// search, a pack — so one set of words selects one set of sessions wherever it is said.
// They had drifted apart: list_sessions let "claude" reach the labeled claude:box2 while
// the pack compared names exactly, and a search could not be narrowed at all, so recent
// hits from other projects could fill its page before the project being asked about was
// reached.
type sessionFilter struct {
	source    string    // a mode, which reaches its labeled instances, or one instance's name
	project   string    // substring of the project: the cwd, or gemini's own project field
	branch    string    // substring of the git branch the session opened on
	since     time.Time // updated at or after; zero is unbounded
	until     time.Time // updated at or before; zero is unbounded
	subagents bool      // subagents' transcripts too, not only the sessions they ran in
	archived  bool      // the sessions their CLI archived too
}

// admits reports whether a session is inside the selection. A session whose update time
// will not parse falls outside a bounded window rather than being guessed onto either side.
func (f sessionFilter) admits(rec source.Record) bool {
	if !sourceMatchesWanted(rec.Source, f.source) {
		return false
	}
	if rec.IsSubagent() && !f.subagents {
		return false
	}
	if rec.Archived && !f.archived {
		return false
	}
	if f.project != "" && !strings.Contains(rec.ProjectName(), f.project) {
		return false
	}
	if f.branch != "" && !strings.Contains(rec.Branch, f.branch) {
		return false
	}
	at := rec.SortAt()
	if !f.since.IsZero() && (at.IsZero() || at.Before(f.since)) {
		return false
	}
	if !f.until.IsZero() && (at.IsZero() || at.After(f.until)) {
		return false
	}
	return true
}

// eachSession hands every session the filter admits to fn, with the source that reads it,
// in source order. A source the filter names out is passed over before it is listed, so
// narrowing by source also spares the other sources' scans.
func (a *SessionQueryAPI) eachSession(f sessionFilter, fn func(source.SessionSource, source.Record)) {
	for _, src := range a.activeSources() {
		if !sourceMatchesWanted(src.Mode(), f.source) {
			continue
		}
		for _, rec := range a.recordsOf(src) {
			if f.admits(rec) {
				fn(src, rec)
			}
		}
	}
}

// subagentsOf lists the subagents that ran under a session, in the order they started, so a
// reader sees the work as it was handed out; each carries its parentAgentId and depth for
// the nesting. A subagent's own pattern lists the agents below it.
func (a *SessionQueryAPI) subagentsOf(pattern, sourceWanted string) ([]source.Record, bool) {
	src, rec, ok := a.findSession(pattern, sourceWanted)
	if !ok {
		return nil, false
	}
	session := rec.SessionID
	if rec.IsSubagent() {
		session = rec.ParentSessionID
	}
	agents := map[string]source.Record{} // the session's subagents by id
	for _, item := range a.recordsOf(src) {
		if item.IsSubagent() && item.ParentSessionID == session {
			agents[item.SessionID] = item
		}
	}
	children := []source.Record{}
	for _, item := range agents {
		if !rec.IsSubagent() || descendsFrom(item, rec.SessionID, agents) {
			children = append(children, item)
		}
	}
	sort.Slice(children, func(i, j int) bool {
		if children[i].CreatedAt != children[j].CreatedAt {
			return children[i].CreatedAt < children[j].CreatedAt
		}
		return children[i].SessionID < children[j].SessionID // a map has no order of its own
	})
	return children, true
}

// subagentSummary is a subagent as get_session lists it: enough to pick one and open it
// by its sessionId, without the paths and project fields every row of the list repeats
func subagentSummary(rec source.Record) map[string]any {
	out := map[string]any{
		"sessionId":  rec.SessionID,
		"shortKey":   rec.ShortKey,
		"agentDepth": rec.AgentDepth,
		"createdAt":  rec.CreatedAt,
		"updatedAt":  rec.UpdatedAt,
	}
	if rec.AgentType != "" {
		out["agentType"] = rec.AgentType
	}
	if rec.ParentAgentID != "" {
		out["parentAgentId"] = rec.ParentAgentID
	}
	if rec.ParentToolUseID != "" {
		out["parentToolUseId"] = rec.ParentToolUseID
	}
	if rec.HasCount {
		out["messageCount"] = rec.MessageCount
	}
	return out
}

// descendsFrom reports whether a subagent was spawned, directly or further down, by the
// agent named; agents are its session's subagents by id
func descendsFrom(rec source.Record, agentID string, agents map[string]source.Record) bool {
	for hops := 0; rec.ParentAgentID != "" && hops <= len(agents); hops++ {
		if rec.ParentAgentID == agentID {
			return true
		}
		next, ok := agents[rec.ParentAgentID]
		if !ok {
			return false
		}
		rec = next
	}
	return false
}

// sessionsWhere is the session list narrowed by a filter, newest first like the list
// itself: the same rows, in the same order, that /sessions would show with the filter
// applied by hand.
func (a *SessionQueryAPI) sessionsWhere(f sessionFilter) []map[string]any {
	records := []source.Record{}
	a.eachSession(f, func(_ source.SessionSource, rec source.Record) {
		records = append(records, rec)
	})
	sort.SliceStable(records, func(i, j int) bool { return records[i].NewerThan(records[j]) })
	out := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.Public())
	}
	return out
}
