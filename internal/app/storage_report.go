package app

import (
	"sort"
	"time"

	"github.com/itswl/agent-session-query/internal/source"
)

// The storage report: what each source's sessions take on disk, grouped three ways —
// by source, by project, and by the age of the sessions. It answers "which CLI ate my
// disk" and "how much of it is old sessions I will not open again", the two questions
// a byte count alone cannot.

type storageSourceRow struct {
	Source   string `json:"source"`
	Sessions int    `json:"sessions"`
	Bytes    int64  `json:"bytes"`
	Overhead int64  `json:"overhead"`
}

type storageProjectRow struct {
	Project  string   `json:"project"`
	Sources  []string `json:"sources"`
	Sessions int      `json:"sessions"`
	Bytes    int64    `json:"bytes"`
}

type storageAgeRow struct {
	Bucket   string `json:"bucket"`
	Sessions int    `json:"sessions"`
	Bytes    int64  `json:"bytes"`
}

type storageReport struct {
	GeneratedAt string              `json:"generatedAt"`
	BySource    []storageSourceRow  `json:"bySource"`
	ByProject   []storageProjectRow `json:"byProject"`
	ByAge       []storageAgeRow     `json:"byAge"`
}

// storageAgeBuckets, widest first: the report's age dimension is "how much would I get
// back if I dropped sessions older than X"
var storageAgeBuckets = []struct {
	name string
	max  time.Duration
}{
	{"within 7 days", 7 * 24 * time.Hour},
	{"7 to 30 days", 30 * 24 * time.Hour},
	{"30 to 90 days", 90 * 24 * time.Hour},
	{"older than 90 days", 0}, // no upper bound
}

// storageReportFor assembles the report from whatever sources are enabled right now.
// Sessions a source cannot attribute to a project still count by source and by age,
// under a project of their own name.
func storageReportFor(sources []source.SessionSource, now time.Time) storageReport {
	report := storageReport{
		GeneratedAt: now.UTC().Format("2006-01-02T15:04:05"),
		BySource:    []storageSourceRow{},
		ByProject:   []storageProjectRow{},
		ByAge:       []storageAgeRow{},
	}

	projects := map[string]*storageProjectRow{}
	ages := make([]storageAgeRow, len(storageAgeBuckets))
	for i, bucket := range storageAgeBuckets {
		ages[i].Bucket = bucket.name
	}

	for _, src := range sources {
		reporter, ok := src.(source.StorageReporter)
		if !ok {
			continue
		}
		sessions, overhead := reporter.Storage()
		row := storageSourceRow{Source: src.Mode(), Overhead: overhead}
		for _, session := range sessions {
			row.Sessions++
			row.Bytes += session.Bytes

			project := session.Project
			if project == "" {
				project = "(no project)"
			}
			entry, ok := projects[project]
			if !ok {
				entry = &storageProjectRow{Project: project}
				projects[project] = entry
			}
			entry.Sessions++
			entry.Bytes += session.Bytes
			if !contains(entry.Sources, src.Mode()) {
				entry.Sources = append(entry.Sources, src.Mode())
			}

			if ts, ok := source.ParseTimestamp(session.UpdatedAt); ok {
				age := now.Sub(ts)
				for i, bucket := range storageAgeBuckets {
					if age <= bucket.max || bucket.max == 0 {
						ages[i].Sessions++
						ages[i].Bytes += session.Bytes
						break
					}
				}
				continue
			}
			// a session with no readable time is still somewhere; the last bucket is
			// "older than 90 days" in name only when it got there by age
			ages[len(ages)-1].Sessions++
			ages[len(ages)-1].Bytes += session.Bytes
		}
		report.BySource = append(report.BySource, row)
	}

	for _, entry := range projects {
		report.ByProject = append(report.ByProject, *entry)
	}
	sort.Slice(report.ByProject, func(i, j int) bool {
		return report.ByProject[i].Bytes > report.ByProject[j].Bytes
	})
	report.ByAge = ages
	return report
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
