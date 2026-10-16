package app

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/source"
)

// TestUsageReport: the windows, the model split and the day rows — from a source whose
// finals carry usage — with the cost kept a fraction (an integer cast once zeroed it).
func TestUsageReport(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	root := filepath.Join(home, "pi")
	now := time.Now().UTC()
	write(t, filepath.Join(root, "p", "2026-10-14T08-00-00-000Z_aaaa.jsonl"),
		`{"type":"session","id":"aaaa","cwd":"/w/one"}`,
		`{"type":"message","id":"m1","message":{"role":"user","timestamp":`+epochMillis(now.Add(-2*24*time.Hour))+`,"content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"message","id":"m2","timestamp":"`+now.Add(-2*24*time.Hour).UTC().Format("2006-01-02T15:04:05")+`","message":{"role":"assistant","stopReason":"stop","model":"glm-5.3","usage":{"input":1000,"output":200,"cacheRead":500},"content":[{"type":"text","text":"done"}]}}`,
	)
	// the fresh session lands "just now" — a relative offset would cross a UTC
	// midnight when the suite runs in the hour after one
	write(t, filepath.Join(root, "p", "2026-10-15T08-00-00-000Z_bbbb.jsonl"),
		`{"type":"session","id":"bbbb","cwd":"/w/two"}`,
		`{"type":"message","id":"m1","message":{"role":"user","timestamp":`+epochMillis(now)+`,"content":[{"type":"text","text":"go"}]}}`,
		`{"type":"message","id":"m2","timestamp":"`+now.UTC().Format("2006-01-02T15:04:05")+`","message":{"role":"assistant","stopReason":"stop","model":"glm-5.3","usage":{"input":50,"output":10},"content":[{"type":"text","text":"ok"}]}}`,
	)

	report := usageReportFor([]source.SessionSource{source.NewPiSource(root)}, now)
	if report.Windows["all"].Sessions != 2 {
		t.Fatalf("all = %v", report.Windows["all"])
	}
	if report.Windows["all"].Tokens["inputTokens"] != 1050 {
		t.Fatalf("input tokens = %v", report.Windows["all"].Tokens)
	}
	// cache hit: the only cache numbers are aaaa's 500 read against 1000+50 input
	if hit := report.Windows["all"].CacheHit; hit < 0.31 || hit > 0.33 {
		t.Fatalf("cacheHit = %v, want 500/1050", hit)
	}
	if report.ByModel[0].CacheHit != report.Windows["all"].CacheHit {
		t.Fatalf("the model row carries the same hit: %v", report.ByModel[0])
	}
	// the recent session lands in today, this week and this month; the two-day-old one
	// only in this month when the month is old enough — assert no more than the fresh one
	if report.Windows["today"].Sessions != 1 || report.Windows["today"].Tokens["inputTokens"] != 50 {
		t.Fatalf("today = %v", report.Windows["today"])
	}
	if report.Windows["this week"].Tokens["inputTokens"] != 1050 {
		t.Fatalf("this week = %v", report.Windows["this week"])
	}
	if len(report.ByModel) != 1 || report.ByModel[0].Model != "glm-5.3" {
		t.Fatalf("byModel = %v", report.ByModel)
	}
	if len(report.ByDay) != 2 {
		t.Fatalf("byDay = %v", report.ByDay)
	}
}

func epochMillis(t time.Time) string {
	return fmt.Sprintf("%d", t.UnixMilli())
}
