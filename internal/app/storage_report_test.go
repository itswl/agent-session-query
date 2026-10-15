package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/source"
)

// TestStorageReport: the three groupings the report answers — which source, which
// project, how old — from a source with two sessions in two projects of different ages.
func TestStorageReport(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	root := filepath.Join(home, "pi")
	now := time.Now().UTC()
	write(t, filepath.Join(root, "p1", "2026-10-01T10-00-00-000Z_aaaa.jsonl"),
		`{"type":"session","id":"aaaa","cwd":"/w/one"}`)
	write(t, filepath.Join(root, "p1", "stray.txt"), `x`)
	write(t, filepath.Join(root, "p2", "2026-05-01T10-00-00-000Z_bbbb.jsonl"),
		`{"type":"session","id":"bbbb","cwd":"/w/two"}`)

	pi := source.NewPiSource(root)
	sources := []source.SessionSource{pi}

	report := storageReportFor(sources, now)
	if len(report.BySource) != 1 || report.BySource[0].Source != "pi" {
		t.Fatalf("bySource = %v", report.BySource)
	}
	if report.BySource[0].Sessions != 2 || report.BySource[0].Overhead != int64(len("x")+1) {
		t.Fatalf("source row = %v", report.BySource[0])
	}
	projects := map[string]storageProjectRow{}
	for _, row := range report.ByProject {
		projects[row.Project] = row
	}
	if projects["/w/one"].Sessions != 1 || projects["/w/two"].Sessions != 1 {
		t.Fatalf("byProject = %v", report.ByProject)
	}
	var total int64
	for _, row := range report.ByAge {
		total += row.Bytes
	}
	if total != report.BySource[0].Bytes {
		t.Fatalf("age rows count %d, sessions count %d", total, report.BySource[0].Bytes)
	}
	var sessionBytes int64
	for _, row := range report.ByAge {
		sessionBytes += int64(row.Sessions)
	}
	if sessionBytes != 2 {
		t.Fatalf("age rows hold %d sessions, want 2", sessionBytes)
	}
}
