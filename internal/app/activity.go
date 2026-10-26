package app

import (
	"sort"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/source"
)

// Activity over a window: the failures a stretch of work hit, and what the stretch added
// up to per project. Both read the same material the rounds do — a session's messages cut
// into rounds — so the numbers agree with /rounds and /brief; neither introduces an index
// or a second parser.

// withinWindow reports whether an ISO time falls in [since, until); a zero bound is open.
// An unreadable time is kept rather than dropped, as the message windows do.
func withinWindow(iso string, since, until time.Time) bool {
	at, ok := source.ParseTimestamp(iso)
	if !ok {
		return true
	}
	if !since.IsZero() && at.Before(since) {
		return false
	}
	if !until.IsZero() && !at.Before(until) {
		return false
	}
	return true
}

// baseMode strips a --path label from a source name (claude:box2 → claude)
func baseMode(name string) string {
	if i := strings.IndexByte(name, ':'); i >= 0 {
		return name[:i]
	}
	return name
}

// sessionMatches is the selection both reports share: source by base mode, project as a
// substring of the session's project path, archived sessions out
func sessionMatches(rec source.Record, sourceName, project string) bool {
	if rec.Archived {
		return false
	}
	if sourceName != "" && baseMode(rec.Source) != sourceName && rec.Source != sourceName {
		return false
	}
	if project != "" && !strings.Contains(strings.ToLower(rec.ProjectName()), strings.ToLower(project)) {
		return false
	}
	return true
}

// failuresReport collects failed tool results across the sessions updated in the window.
// It scans the newest sessions first and stops once enough failures are in hand: a report
// is a page of recent trouble, not an audit of the whole history.
func (a *SessionQueryAPI) failuresReport(since, until time.Time, sourceName, project, tool string, limit int) map[string]any {
	type candidate struct {
		src source.SessionSource
		rec source.Record
	}
	candidates := []candidate{}
	for _, src := range a.activeSources() {
		for _, rec := range a.recordsOf(src) {
			if !sessionMatches(rec, sourceName, project) || !withinWindow(rec.UpdatedAt, since, until) {
				continue
			}
			candidates = append(candidates, candidate{src, rec})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].rec.NewerThan(candidates[j].rec)
	})

	out := []map[string]any{}
	scanned := 0
	toolNeedle := strings.ToLower(strings.TrimSpace(tool))
	for _, c := range candidates {
		if len(out) >= limit*4 {
			break // enough to sort and cut; older sessions wait for a narrower window
		}
		scanned++
		rounds := splitRounds(tailMessages(c.src, c.rec))
		for _, r := range rounds {
			for _, failure := range r.failed {
				if toolNeedle != "" && !strings.Contains(strings.ToLower(failure.tool), toolNeedle) {
					continue
				}
				out = append(out, map[string]any{
					"sessionId": c.rec.SessionID,
					"source":    c.rec.Source,
					"project":   c.rec.ProjectName(),
					"shortKey":  c.rec.ShortKey,
					"file":      nullString(c.rec.File),
					"round":     r.index,
					"at":        failure.at,
					"tool":      failure.tool,
					"command":   failure.command,
					"output":    failure.output,
				})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return source.ToStr(out[i]["at"]) > source.ToStr(out[j]["at"])
	})
	total := len(out)
	truncated := false
	if limit > 0 && len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return map[string]any{
		"failures":        out,
		"total":           total,
		"sessionsScanned": scanned,
		"truncated":       truncated,
	}
}

// projectDigest is one project's stretch of work
type projectDigest struct {
	project   string
	sessions  int
	rounds    int
	failures  int
	tokens    map[string]int64
	kinds     map[string]int
	models    map[string]int
	firstAt   string
	lastAt    string
	agents    map[string]bool
	archived  int
	subagents int
}

// digestReport answers what the window added up to, per project: sessions, rounds,
// failures, tokens (from the finals, so they match /usage), the files changed counted by
// kind, and the models and CLIs that did the work.
func (a *SessionQueryAPI) digestReport(since, until time.Time, sourceName, project string) map[string]any {
	byProject := map[string]*projectDigest{}
	get := func(name string) *projectDigest {
		d, ok := byProject[name]
		if !ok {
			d = &projectDigest{
				project: name,
				tokens:  map[string]int64{},
				kinds:   map[string]int{},
				models:  map[string]int{},
				agents:  map[string]bool{},
			}
			byProject[name] = d
		}
		return d
	}

	for _, src := range a.activeSources() {
		for _, rec := range a.recordsOf(src) {
			if rec.IsSubagent() || !sessionMatches(rec, sourceName, project) {
				continue
			}
			if !withinWindow(rec.UpdatedAt, since, until) {
				continue
			}
			d := get(rec.ProjectName())
			d.sessions++
			d.agents[baseMode(rec.Source)] = true
			if rec.Model != "" {
				d.models[rec.Model]++
			}
			if d.firstAt == "" || rec.UpdatedAt < d.firstAt {
				if _, ok := source.ParseTimestamp(rec.UpdatedAt); ok {
					d.firstAt = rec.UpdatedAt
				}
			}
			if rec.UpdatedAt > d.lastAt {
				d.lastAt = rec.UpdatedAt
			}

			rounds := splitRounds(tailMessages(src, rec))
			d.rounds += len(rounds)
			for _, r := range rounds {
				d.failures += r.failures
				for _, file := range r.changed {
					d.kinds[fileKind(file)]++
				}
			}

			if final := src.Final(rec); final != nil {
				if usage, ok := final["usage"].(map[string]any); ok {
					for name, value := range usage {
						if n, ok := source.ToFloat(value); ok {
							d.tokens[name] += int64(n)
						}
					}
				}
			}
		}
	}

	projects := make([]map[string]any, 0, len(byProject))
	totals := &projectDigest{
		tokens: map[string]int64{},
		kinds:  map[string]int{},
		models: map[string]int{},
		agents: map[string]bool{},
	}
	for _, d := range byProject {
		row := digestRow(d)
		projects = append(projects, row)
		totals.sessions += d.sessions
		totals.rounds += d.rounds
		totals.failures += d.failures
		for name, n := range d.tokens {
			totals.tokens[name] += n
		}
		for kind, n := range d.kinds {
			totals.kinds[kind] += n
		}
		for model, n := range d.models {
			totals.models[model] += n
		}
		for agent := range d.agents {
			totals.agents[agent] = true
		}
		if d.firstAt != "" && (totals.firstAt == "" || d.firstAt < totals.firstAt) {
			totals.firstAt = d.firstAt
		}
		if d.lastAt > totals.lastAt {
			totals.lastAt = d.lastAt
		}
	}
	sort.SliceStable(projects, func(i, j int) bool {
		return source.ToStr(projects[i]["lastAt"]) > source.ToStr(projects[j]["lastAt"])
	})
	return map[string]any{
		"since":    isoOrEmpty(since),
		"until":    isoOrEmpty(until),
		"totals":   digestRow(totals),
		"projects": projects,
	}
}

// digestRow shapes one project's numbers for JSON
func digestRow(d *projectDigest) map[string]any {
	models := make([]string, 0, len(d.models))
	for model := range d.models {
		models = append(models, model)
	}
	sort.SliceStable(models, func(i, j int) bool {
		if d.models[models[i]] != d.models[models[j]] {
			return d.models[models[i]] > d.models[models[j]]
		}
		return models[i] < models[j]
	})
	if len(models) > 5 {
		models = models[:5]
	}
	agents := make([]string, 0, len(d.agents))
	for agent := range d.agents {
		agents = append(agents, agent)
	}
	sort.Strings(agents)

	tokens := map[string]any{}
	for name, n := range d.tokens {
		if n != 0 {
			tokens[name] = n
		}
	}
	input, cache := d.tokens["inputTokens"], d.tokens["cacheReadTokens"]
	row := map[string]any{
		"project":  d.project,
		"sessions": d.sessions,
		"rounds":   d.rounds,
		"failures": d.failures,
		"tokens":   tokens,
		"kinds":    d.kinds,
		"models":   models,
		"sources":  agents,
	}
	if input+cache > 0 {
		row["cacheHit"] = float64(cache) / float64(input+cache)
	}
	if d.firstAt != "" {
		row["firstAt"] = d.firstAt
	}
	if d.lastAt != "" {
		row["lastAt"] = d.lastAt
	}
	return row
}

func isoOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05")
}

// nullString renders an absent string as JSON null, matching the session rows
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
