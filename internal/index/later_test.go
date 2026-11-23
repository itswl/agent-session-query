package index

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// commands is a store of one session, whose turns each run commands that fail or pass.
type commands struct{ turns [][]ran }

type ran struct {
	line   string
	failed bool
}

func (commands) Location() string { return "test" }
func (commands) Exists() bool     { return true }
func (c commands) Scan(context.Context) ([]sources.Ref, error) {
	return []sources.Ref{{Key: "s", Stamp: strconv.Itoa(len(c.turns)), ModTime: time.Unix(1790000000, 0)}}, nil
}
func (c commands) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	w.S.ID = "s"
	at := time.Unix(1790000000, 0).UTC()
	next := func() time.Time { at = at.Add(time.Second); return at }
	for i, turn := range c.turns {
		w.Add(transcript.Message{Role: transcript.User, At: next(), Blocks: []transcript.Block{transcript.TextOf("turn " + strconv.Itoa(i+1))}})
		for j, r := range turn {
			id := "c" + strconv.Itoa(i) + "." + strconv.Itoa(j)
			w.Add(transcript.Message{Role: transcript.Assistant, At: next(), Response: id,
				Blocks: []transcript.Block{transcript.CallOf(id, "Bash", map[string]any{"command": r.line})}})
			exit := 0
			if r.failed {
				exit = 1
			}
			w.Add(transcript.Message{Role: transcript.ToolRole, At: next(),
				Blocks: []transcript.Block{transcript.ResultOf(id, "Bash", "out", transcript.Outcome{Status: transcript.StatusOf(r.failed), Exit: transcript.ExitOf(exit)})}})
		}
	}
	return nil
}

// Whether a failed command passed later is known of every failure the index holds, from
// when its session is read: in the turns written then, and in those before them that a turn
// written then may decide — a turn the transcript gains, or no longer has, whose own rows
// are not written again.
func TestAFailureSaysWhereItsCommandPassedLater(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	sync := func(c commands) {
		t.Helper()
		if _, err := ix.Sync(ctx, []Store{{Name: "claude", Spec: sources.Spec{Name: "claude"}, Source: c}}, SyncOptions{Force: true}); err != nil {
			t.Fatal(err)
		}
	}
	later := func(when string, want map[string]string) {
		t.Helper()
		s, err := ix.Resolve(ctx, "claude/s", nil)
		if err != nil {
			t.Fatal(err)
		}
		fails, err := ix.FailuresOf(ctx, &s)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, f := range fails {
			got[f.Command] = "-"
			if l := f.Later; l != nil {
				got[f.Command] = "#" + strconv.Itoa(l.Turn) + " " + l.Command
			}
		}
		for cmd, w := range want {
			if got[cmd] != w {
				t.Errorf("%s: %q passed later at %q, want %q", when, cmd, got[cmd], w)
			}
		}
		var unjudged int
		if err := ix.rw.QueryRow(`SELECT count(*) FROM failures WHERE judged < ?`, laterVersion).Scan(&unjudged); err != nil || unjudged != 0 {
			t.Errorf("%s: %d failures not judged, %v", when, unjudged, err)
		}
	}

	one := []ran{{"go test ./...", true}, {"make lint", true}}
	two := []ran{{"go  test ./internal/x", false}}
	sync(commands{[][]ran{one}})
	later("one turn", map[string]string{"go test ./...": "-", "make lint": "-"})

	// The transcript gains a turn in which go test passes: the first turn's rows are as they
	// were, and its failure has passed later all the same
	sync(commands{[][]ran{one, two}})
	later("a turn later", map[string]string{"go test ./...": "#2 go test ./internal/x", "make lint": "-"})

	// And the session, its turns and the digest say so
	s, err := ix.Resolve(ctx, "claude/s", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Stats.Failures != 2 || s.Stats.PassedLater != 1 {
		t.Errorf("session: %d failed, %d passed later; want 2, 1", s.Stats.Failures, s.Stats.PassedLater)
	}
	if list, _, err := ix.Sessions(ctx, Filter{}, Page{}); err != nil || len(list) != 1 || list[0].Stats.PassedLater != 1 {
		t.Errorf("sessions = %+v, %v; want the one, 1 passed later", list, err)
	}
	outline, err := ix.Outline(ctx, &s)
	if err != nil || len(outline) != 2 || outline[0].PassedLater != 1 || outline[1].PassedLater != 0 {
		t.Errorf("outline = %+v, %v; want turn 1 with 1 passed later", outline, err)
	}
	if turns, err := ix.Turns(ctx, &s, TurnRange{First: 1, Last: 1}); err != nil || len(turns) != 1 || turns[0].PassedLater != 1 {
		t.Errorf("turn 1 = %+v, %v; want 1 passed later", turns, err)
	}
	if d, err := ix.Digest(ctx, Filter{}, Window{}); err != nil || d.Total.Failures != 2 || d.Total.PassedLater != 1 || len(d.Projects) != 1 || d.Projects[0].PassedLater != 1 {
		t.Errorf("digest = %+v, %v; want 2 failed, 1 passed later", d, err)
	}

	// Rewritten without it: it no longer has
	sync(commands{[][]ran{one}})
	later("the turn gone", map[string]string{"go test ./...": "-"})

	// Past the turns looked in, it is not later than the failure
	sync(commands{[][]ran{one, nil, nil, nil, two}})
	later("four turns on", map[string]string{"go test ./...": "-"})
	sync(commands{[][]ran{one, nil, nil, two}})
	later("three turns on", map[string]string{"go test ./...": "#4 go test ./internal/x"})
}

// Failures no asq of this laterVersion judged — in an index an older asq wrote, or one that
// judged them otherwise — are judged at the next sync from the turns the index holds,
// without reading a transcript; and the turns an older asq writes have the failures before
// them judged again.
func TestFailuresAreJudgedFromTheIndex(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	c := commands{[][]ran{{{"go test ./...", true}}, {{"go test ./...", false}}}}
	sync := func() SyncResult {
		t.Helper()
		res, err := ix.Sync(ctx, []Store{{Name: "claude", Spec: sources.Spec{Name: "claude"}, Source: c}}, SyncOptions{Force: true})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := ix.rw.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	passed := func(when string, want bool) {
		t.Helper()
		var seq, judged int
		var key string
		if err := ix.rw.QueryRow(`SELECT command_key, later_seq, judged FROM failures`).Scan(&key, &seq, &judged); err != nil {
			t.Fatal(err)
		}
		if (seq > 0) != want || key != "go test" || judged != laterVersion {
			t.Errorf("%s: command %q, passed later at step %d, judged by %d; want passed later %v, judged by %d", when, key, seq, judged, want, laterVersion)
		}
	}
	sync()
	passed("read", true)

	// As an asq from before judgements left it
	exec(`UPDATE failures SET command_key = '', later_idx = 0, later_seq = 0, later_at = 0, later_command = '', judged = 0`)
	if res := sync(); res.Read != 0 {
		t.Errorf("%d read to judge failures, want none", res.Read)
	}
	passed("judged from the index", true)

	// An older asq deletes the second turn, then writes it again: the failure before it is
	// judged again each time
	var hash, body []byte
	var head string
	if err := ix.rw.QueryRow(`SELECT hash, head, body FROM turns WHERE idx = 2`).Scan(&hash, &head, &body); err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM turns WHERE idx = 2`)
	sync()
	passed("its turn gone", false)
	exec(`INSERT INTO turns (sid, idx, hash, head, body) SELECT sid, 2, ?, ?, ? FROM sessions`, hash, head, body)
	sync()
	passed("its turn back", true)

	// One a newer asq judged is left to it
	exec(`UPDATE failures SET later_seq = 0, judged = ?`, laterVersion+1)
	sync()
	var seq int
	if err := ix.rw.QueryRow(`SELECT later_seq FROM failures`).Scan(&seq); err != nil || seq != 0 {
		t.Errorf("a newer asq's judgement judged again: passed later at %d, %v", seq, err)
	}
}

// An index at a path of its own is the one an older asq used, from before judgements: it
// gains their columns in place.
func TestAnIndexFromBeforeJudgementsGainsThem(t *testing.T) {
	before := schema
	for _, col := range failuresColumns {
		line := ""
		for l := range strings.SplitSeq(before, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), col[0]+" ") {
				line = l + "\n"
			}
		}
		if line == "" {
			t.Fatalf("no column %s in the schema", col[0])
		}
		before = strings.Replace(before, line, "", 1)
	}
	path := filepath.Join(t.TempDir(), "index.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(before); err != nil {
		t.Fatal(err)
	}
	old.Close()
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	c := commands{[][]ran{{{"make", true}}, {{"make", false}}}}
	if _, err := ix.Sync(context.Background(), []Store{{Name: "claude", Spec: sources.Spec{Name: "claude"}, Source: c}}, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	var seq int
	if err := ix.rw.QueryRow(`SELECT later_seq FROM failures`).Scan(&seq); err != nil || seq == 0 {
		t.Errorf("passed later at %d, %v", seq, err)
	}
}

// What keeps failing, and what of it is still open: the failures by the command that failed,
// the commands with failures still open first, and a list of the open ones alone, counted
// with what it leaves out.
func TestFailuresByCommandAndStillOpen(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	a := commands{[][]ran{{{"go test ./a", true}, {"make lint", true}}, {{"go test ./a -run X", true}}}}
	b := commands{[][]ran{{{"go test ./b", true}}, {{"go test ./b", false}}, {{"python3 -c 'raise SystemExit(1)'", true}}}} // the latest
	stores := []Store{{Name: "claude", Spec: sources.Spec{Name: "claude"}, Source: a}, {Name: "codex", Spec: sources.Spec{Name: "codex"}, Source: b}}
	if _, err := ix.Sync(ctx, stores, SyncOptions{}); err != nil {
		t.Fatal(err)
	}

	groups, err := ix.FailuresByCommand(ctx, Filter{}, Window{}, FailureQuery{}, Page{})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, g := range groups.Commands {
		got = append(got, g.Command+"|"+g.Tool+"|"+strconv.Itoa(g.Count)+"|"+strconv.Itoa(g.Open)+"|"+strconv.Itoa(g.Sessions)+"|"+g.Last.Command)
	}
	want := []string{
		"go test||3|2|2|go test ./a -run X",            // still open twice, in one of its two sessions; the latest
		"|Bash|1|1|1|python3 -c 'raise SystemExit(1)'", // code given inline: no command, so the tool's
		"make lint||1|1|1|make lint",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") || groups.Total != 3 || groups.Open != 4 || groups.PassedLater != 1 {
		t.Errorf("by command: total %d, open %d, passed later %d:\n%s\nwant 3, 4, 1:\n%s", groups.Total, groups.Open, groups.PassedLater, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The open ones alone: go test's two in the one session, counted with the one left out
	groups, err = ix.FailuresByCommand(ctx, Filter{}, Window{}, FailureQuery{Open: true, Text: "go test"}, Page{})
	if err != nil || len(groups.Commands) != 1 || groups.Commands[0].Count != 2 || groups.Commands[0].Sessions != 1 || groups.Open != 2 || groups.PassedLater != 1 {
		t.Errorf("by command, open, go test = %+v, %v", groups, err)
	}
	list, err := ix.Failures(ctx, Filter{}, Window{}, FailureQuery{Open: true}, Page{})
	if err != nil || list.Total != 4 || len(list.Failures) != 4 || list.Open != 4 || list.PassedLater != 1 {
		t.Errorf("open = %+v, %v; want the 4 open of 5", list, err)
	}
	for _, f := range list.Failures {
		if f.Later != nil {
			t.Errorf("an open-only list holds %q, which passed later", f.Command)
		}
	}
	if all, err := ix.Failures(ctx, Filter{}, Window{}, FailureQuery{}, Page{}); err != nil || all.Total != 5 {
		t.Errorf("every failure = %d, %v; want 5", all.Total, err)
	}
	// Nothing open in a window, but before it: said, the open filter kept
	none, err := ix.Failures(ctx, Filter{}, Window{Since: time.Unix(1790000000, 0).Add(time.Hour)}, FailureQuery{Open: true}, Page{})
	if err != nil || none.Total != 0 || none.Outside != 4 {
		t.Errorf("open after the sessions = %+v, %v; want none, and 4 outside", none, err)
	}
}
