package app

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/itswl/agent-session-query/internal/source"
)

// runCheck is --check: the drift defence. An empty session list and a broken source look
// the same through the API, so this names, for every source, whether its data is there,
// how many sessions it lists, and whether the newest one still parses — the three facts
// that turn "nothing showed up" into an answer. Exit code: 0 all healthy, 1 at least one
// existing source failed, 2 bad flags.
func runCheck(mode string, paths []source.SourcePath, out io.Writer) int {
	targets, err := source.CheckTargets(mode, paths)
	if err != nil {
		fmt.Fprintln(out, err)
		return 2
	}

	rows := [][]string{}
	failed := false
	for _, t := range targets {
		if t.Source == nil {
			rows = append(rows, []string{t.Mode, "not found", t.Location})
			continue
		}
		recs := t.Source.List()
		if reporter, ok := t.Source.(source.ListErrorReporter); ok && reporter.ListError() != nil {
			failed = true
			rows = append(rows, []string{t.Mode, "LIST FAILED", t.Location + ": " + reporter.ListError().Error()})
			continue
		}
		detail := fmt.Sprintf("%d sessions", len(recs))
		if newest := newestRecord(recs); newest != nil {
			msgs := t.Source.Messages(*newest, source.MessageQuery{Limit: 5})
			detail += fmt.Sprintf(" · newest reads %d messages", len(msgs))
			if newest.HasFile && len(msgs) == 0 {
				detail += " (no messages parse — format drift?)"
			}
			if final := t.Source.Final(*newest); final != nil {
				detail += " · final ok"
			}
		}
		rows = append(rows, []string{t.Mode, "ok", t.Location + " · " + detail})
	}

	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	for _, row := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\n", row[0], row[1], row[2])
	}
	w.Flush()
	if failed {
		fmt.Fprintln(out, "at least one source failed to list; the log above names it")
		return 1
	}
	return 0
}

// newestRecord picks the record with the latest UpdatedAt — a source's List does not
// promise an order, but every record carries the timestamp it would be sorted by
func newestRecord(recs []source.Record) *source.Record {
	var newest *source.Record
	for i := range recs {
		if newest == nil || recs[i].UpdatedAt > newest.UpdatedAt {
			newest = &recs[i]
		}
	}
	return newest
}
