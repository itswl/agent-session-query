package app

import (
	"sort"
	"time"

	"github.com/itswl/agent-session-query/internal/source"
)

// The usage report: what the sessions consumed, grouped the ways the questions come —
// "how much in the last day", "which model eats the tokens". The windows are rolling
// (24h / 3d / 7d / 30d / all): a calendar window answers differently depending on what
// day it is. The numbers are the session totals the finals already compute, so a
// session's usage lands where it was last updated; a session still running reports so far.

type usageTotalsRow struct {
	Sessions int              `json:"sessions"`
	Calls    int              `json:"modelCalls"`
	Tokens   map[string]int64 `json:"tokens"`
	CacheHit float64          `json:"cacheHit"` // cacheRead / (input + cacheRead), 0..1
}

type usageModelRow struct {
	Model    string           `json:"model"`
	Sessions int              `json:"sessions"`
	Calls    int              `json:"modelCalls"`
	Tokens   map[string]int64 `json:"tokens"`
	CacheHit float64          `json:"cacheHit"`
}

type usageDayRow struct {
	Day      string           `json:"day"`
	Sessions int              `json:"sessions"`
	Tokens   map[string]int64 `json:"tokens"`
	CacheHit float64          `json:"cacheHit"`
}

type usageReport struct {
	GeneratedAt string                    `json:"generatedAt"`
	Windows     map[string]usageTotalsRow `json:"windows"` // 24h / 3d / 7d / 30d / all, rolling
	ByModel     []usageModelRow           `json:"byModel"`
	ByDay       []usageDayRow             `json:"byDay"` // the last 30 days, days with usage only
	BySource    []usageTotalsRow2         `json:"bySource"`
	ByProject   []usageProjectRow2        `json:"byProject"`
}

type usageTotalsRow2 struct {
	Source   string           `json:"source"`
	Sessions int              `json:"sessions"`
	Tokens   map[string]int64 `json:"tokens"`
	CacheHit float64          `json:"cacheHit"`
}

type usageProjectRow2 struct {
	Project  string           `json:"project"`
	Sessions int              `json:"sessions"`
	Tokens   map[string]int64 `json:"tokens"`
	CacheHit float64          `json:"cacheHit"`
}

// usageTokenOrder is what a tokens map holds — everything but the cost, which rides as
// its own field, and the total, which the parts already say
var usageTokenOrder = []string{
	"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "reasoningTokens",
}

func (r *usageTotalsRow) addUsage(tokens map[string]any, calls int) {
	r.Calls += calls
	for _, name := range usageTokenOrder {
		if n, ok := toInt64(tokens[name]); ok {
			r.Tokens[name] += n
		}
	}
}

// cacheHitOf is the share of prompt tokens served from cache: cacheRead over everything
// that would otherwise have been input. 0 when the source records no cache numbers —
// not a 0% hit rate, but the honest "unknown" a report can carry as a zero.
func cacheHitOf(tokens map[string]int64) float64 {
	denom := tokens["inputTokens"] + tokens["cacheReadTokens"]
	if denom <= 0 {
		return 0
	}
	return float64(tokens["cacheReadTokens"]) / float64(denom)
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// oneSessionUsage is what a single session contributed, read from the source that owns it
type oneSessionUsage struct {
	project   string
	model     string
	updatedAt string
	tokens    map[string]any
	calls     int
}

// usageSessionsOf reads one source's sessions and their usage: through the source's own
// Usage when it has one (the SQLite sources answer in one query), else by reading each
// session's final result, whose usage total the finals already compute.
func usageSessionsOf(src source.SessionSource) []oneSessionUsage {
	out := []oneSessionUsage{}
	if reporter, ok := src.(source.UsageReporter); ok {
		if rows := reporter.Usage(); rows != nil {
			for _, row := range rows {
				out = append(out, oneSessionUsage{
					project: row.Project, model: row.Model, updatedAt: row.UpdatedAt,
					tokens: row.Usage, calls: row.Calls,
				})
			}
			return out
		}
		// a source that answers nil keeps the generic path: its finals carry the totals
	}
	for _, rec := range src.List() {
		final := src.Final(rec)
		if final == nil {
			continue
		}
		tokens, _ := final["usage"].(map[string]any)
		if len(tokens) == 0 {
			continue
		}
		calls := 0
		if n, ok := toInt64(final["messageCount"]); ok {
			calls = int(n)
		}
		out = append(out, oneSessionUsage{
			project: rec.ProjectName(), model: toString(final["model"]), updatedAt: rec.UpdatedAt,
			tokens: tokens, calls: calls,
		})
	}
	return out
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

func usageReportFor(sources []source.SessionSource, now time.Time) usageReport {
	report := usageReport{
		GeneratedAt: now.UTC().Format("2006-01-02T15:04:05"),
		Windows:     map[string]usageTotalsRow{},
		ByModel:     []usageModelRow{},
		ByDay:       []usageDayRow{},
		BySource:    []usageTotalsRow2{},
		ByProject:   []usageProjectRow2{},
	}
	windows := map[string]*usageTotalsRow{
		"24h": {}, "3d": {}, "7d": {}, "30d": {}, "all": {},
	}
	for _, w := range windows {
		w.Tokens = map[string]int64{}
	}
	// rolling cutoffs, newest first: a session lands in every window it is young enough for
	rolling := []struct {
		name   string
		cutoff time.Time
	}{
		{"24h", now.Add(-24 * time.Hour)},
		{"3d", now.Add(-72 * time.Hour)},
		{"7d", now.AddDate(0, 0, -7)},
		{"30d", now.AddDate(0, 0, -30)},
	}

	utcDay := func(t time.Time) string { return t.UTC().Format("2006-01-02") }
	cutoffDay := utcDay(now.AddDate(0, 0, -29))

	models := map[string]*usageModelRow{}
	days := map[string]*usageDayRow{}
	perSource := map[string]*usageTotalsRow2{}
	projects := map[string]*usageProjectRow2{}

	for _, src := range sources {
		srcRow := &usageTotalsRow2{Source: src.Mode(), Tokens: map[string]int64{}}
		for _, session := range usageSessionsOf(src) {
			windows["all"].addUsage(session.tokens, session.calls)
			windows["all"].Sessions++
			srcRow.Sessions++
			for _, name := range usageTokenOrder {
				if n, ok := toInt64(session.tokens[name]); ok {
					srcRow.Tokens[name] += n
				}
			}

			if ts, ok := source.ParseTimestamp(session.updatedAt); ok {
				for _, window := range rolling {
					if ts.After(window.cutoff) {
						windows[window.name].addUsage(session.tokens, session.calls)
						windows[window.name].Sessions++
					}
				}
				if day := utcDay(ts); day >= cutoffDay {
					entry, ok := days[day]
					if !ok {
						entry = &usageDayRow{Day: day, Tokens: map[string]int64{}}
						days[day] = entry
					}
					entry.Sessions++
					for _, name := range usageTokenOrder {
						if n, ok := toInt64(session.tokens[name]); ok {
							entry.Tokens[name] += n
						}
					}
				}
			}

			model := session.model
			if model == "" {
				model = "(unknown model)"
			}
			entry, ok := models[model]
			if !ok {
				entry = &usageModelRow{Model: model, Tokens: map[string]int64{}}
				models[model] = entry
			}
			entry.Sessions++
			entry.Calls += session.calls
			for _, name := range usageTokenOrder {
				if n, ok := toInt64(session.tokens[name]); ok {
					entry.Tokens[name] += n
				}
			}

			project := session.project
			if project == "" {
				project = "(no project)"
			}
			proj, ok := projects[project]
			if !ok {
				proj = &usageProjectRow2{Project: project, Tokens: map[string]int64{}}
				projects[project] = proj
			}
			proj.Sessions++
			for _, name := range usageTokenOrder {
				if n, ok := toInt64(session.tokens[name]); ok {
					proj.Tokens[name] += n
				}
			}
		}
		if srcRow.Sessions > 0 {
			perSource[src.Mode()] = srcRow
		}
	}
	for name, row := range windows {
		row.CacheHit = cacheHitOf(row.Tokens)
		report.Windows[name] = *row
	}

	for _, row := range models {
		row.CacheHit = cacheHitOf(row.Tokens)
		report.ByModel = append(report.ByModel, *row)
	}
	sort.Slice(report.ByModel, func(i, j int) bool {
		return usageRowWeight(report.ByModel[i].Tokens) > usageRowWeight(report.ByModel[j].Tokens)
	})
	for _, row := range days {
		row.CacheHit = cacheHitOf(row.Tokens)
		report.ByDay = append(report.ByDay, *row)
	}
	sort.Slice(report.ByDay, func(i, j int) bool { return report.ByDay[i].Day < report.ByDay[j].Day })
	for _, row := range perSource {
		row.CacheHit = cacheHitOf(row.Tokens)
		report.BySource = append(report.BySource, *row)
	}
	sort.Slice(report.BySource, func(i, j int) bool {
		return usageRowWeight(report.BySource[i].Tokens) > usageRowWeight(report.BySource[j].Tokens)
	})
	for _, row := range projects {
		row.CacheHit = cacheHitOf(row.Tokens)
		report.ByProject = append(report.ByProject, *row)
	}
	sort.Slice(report.ByProject, func(i, j int) bool {
		return usageRowWeight(report.ByProject[i].Tokens) > usageRowWeight(report.ByProject[j].Tokens)
	})
	return report
}

// usageRowWeight orders rows by input+output — cache reads dwarf both and would sort a
// model that touched cache once above one that did all the work
func usageRowWeight(tokens map[string]int64) int64 {
	weight := int64(0)
	for _, name := range []string{"inputTokens", "outputTokens"} {
		weight += tokens[name]
	}
	return weight
}
