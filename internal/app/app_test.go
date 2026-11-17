package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
)

func TestWhen(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"":                     {},
		"all":                  {},
		"90m":                  now.Add(-90 * time.Minute),
		"12h":                  now.Add(-12 * time.Hour),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"2w":                   now.Add(-14 * 24 * time.Hour),
		"1.5h":                 now.Add(-90 * time.Minute),
		"2026-09-01":           time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local),
		"2026-09-01T08:30":     time.Date(2026, 9, 1, 8, 30, 0, 0, time.Local),
		"2026-09-01 08:30:15":  time.Date(2026, 9, 1, 8, 30, 15, 0, time.Local),
		"2026-09-01T08:30:00Z": time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC),
	} {
		got, err := app.When(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("When(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "7", "d", "-3d", "7x", "2026-13-01"} {
		if _, err := app.When(bad, now); err == nil {
			t.Errorf("When(%q) is accepted", bad)
		}
	}
}

func TestTurnRange(t *testing.T) {
	for _, tc := range []struct {
		spec  string
		named int
		want  index.TurnRange
		some  bool
	}{
		{"", -1, index.TurnRange{}, false},
		{"", 4, index.TurnRange{First: 4, Last: 4}, true},
		{"3", -1, index.TurnRange{First: 3, Last: 3}, true},
		{"3-5", -1, index.TurnRange{First: 3, Last: 5}, true},
		{" 3 - 5 ", -1, index.TurnRange{First: 3, Last: 5}, true},
		{"7", 4, index.TurnRange{First: 7, Last: 7}, true}, // said outright beats named in passing
		{"last", -1, index.TurnRange{First: 1, Last: -1, Tail: 1}, true},
		{"all", -1, index.TurnRange{First: 0, Last: -1}, true},
	} {
		got, some, err := app.TurnRange(tc.spec, tc.named)
		if err != nil || some != tc.some || got != tc.want {
			t.Errorf("TurnRange(%q, %d) = %+v, %v, %v", tc.spec, tc.named, got, some, err)
		}
	}
	for _, bad := range []string{"seven", "5-3", "-2", "3-", "1,2"} {
		if _, _, err := app.TurnRange(bad, -1); err == nil {
			t.Errorf("TurnRange(%q) is accepted", bad)
		}
	}
}

func TestResolveAndResume(t *testing.T) {
	a, _ := fixture.Open(t)
	ctx := context.Background()
	s, turn, err := a.Resolve(ctx, "aaaa1111#2")
	if err != nil || s.ID != fixture.Alpha || turn != 2 {
		t.Fatalf("Resolve = %q, %d, %v", s.ID, turn, err)
	}
	// "cd /work/alpha && …" under sh; cmd.exe spells the cd differently
	if !strings.HasPrefix(s.Resume, "cd ") || !strings.HasSuffix(s.Resume, "/work/alpha && claude --resume "+fixture.Alpha) {
		t.Errorf("resume = %q: it changes to the session's directory, then reopens it by id", s.Resume)
	}
	// The same, as a program to run: no shell in between
	if dir, argv := app.ResumeArgs(&s); dir != "/work/alpha" || strings.Join(argv, " ") != "claude --resume "+fixture.Alpha {
		t.Errorf("resume args = %q, %q", dir, argv)
	}
	if _, turn, _ := a.Resolve(ctx, "aaaa1111"); turn != -1 {
		t.Errorf("a ref that names no turn = %d", turn)
	}
	agent, _, err := a.Resolve(ctx, "claude/"+fixture.Agent)
	if err != nil || agent.Resume != "" {
		t.Errorf("a subagent's transcript is not something a CLI reopens: %q, %v", agent.Resume, err)
	}
	if _, argv := app.ResumeArgs(&agent); argv != nil {
		t.Errorf("resume args for a subagent = %q", argv)
	}
	above, err := a.Above(ctx, &agent)
	if err != nil || len(above) != 1 || above[0].ID != fixture.Alpha || above[0].Title != "Fix flaky test" {
		t.Errorf("above a subagent = %+v, %v: the session it ran in, with its title", above, err)
	}
	if above, _ := a.Above(ctx, &s); len(above) != 0 || above == nil {
		t.Errorf("above a session = %+v: nothing, as an empty list", above)
	}
	if _, _, err := a.Resolve(ctx, "nope"); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("an unknown ref names what was asked for: %v", err)
	}
}

func TestFilterIsConfinedToThisProcessesStores(t *testing.T) {
	a, _ := fixture.Open(t)
	f, err := a.Filter(app.Query{})
	if err != nil || len(f.Stores) != 1 || f.Stores[0] != "claude" {
		t.Errorf("filter = %+v, %v", f, err)
	}
	if _, err := a.Filter(app.Query{Source: "codex"}); err != nil {
		t.Errorf("a CLI this build reads but this run does not: an empty answer, not an error: %v", err)
	}
	if _, err := a.Filter(app.Query{Source: "emacs"}); err == nil {
		t.Error("an unknown source is accepted")
	}
	w, err := a.Window(app.Query{}, "7d")
	if err != nil || time.Since(w.Since) < 7*24*time.Hour-time.Minute || time.Since(w.Since) > 7*24*time.Hour+time.Minute {
		t.Errorf("default window = %+v, %v", w, err)
	}
	if w, _ := a.Window(app.Query{Since: "all"}, "7d"); !w.Since.IsZero() {
		t.Errorf("'all' asks for no bound, whatever the default: %v", w.Since)
	}
}

func TestFreshAndHealth(t *testing.T) {
	a, _ := fixture.Open(t)
	ctx := context.Background()
	if !a.Fresh(ctx, time.Minute, time.Second) {
		t.Error("an index synced a moment ago is fresh")
	}
	if !a.Fresh(ctx, 0, 10*time.Second) {
		t.Error("a sync that finishes within the wait leaves the index fresh")
	}
	h, err := a.Health(ctx)
	if err != nil || h.Totals.Sessions != 2 || h.Totals.Archived != 0 || h.Totals.Agents != 1 || h.Totals.Turns != 3 || len(h.Stores) != 1 || !h.Stores[0].Found || h.Stores[0].Sessions != 2 || h.Stores[0].Transcripts != 3 || len(h.Warnings) != 0 {
		t.Errorf("health = %+v, %v", h, err)
	}
	text, n, err := a.Pack(ctx, app.Query{}, 1, false)
	if err != nil || n != 1 || !strings.Contains(text, "Fix flaky test") || strings.Contains(text, "rotate") {
		t.Errorf("a pack of one is the most recent session: %d, %v", n, err)
	}
}

// A CLI that keeps sessions per project finds one only from the directory it was worked
// in: without that directory, there is no command to give, rather than one that fails.
func TestAResumeNeedsItsDirectoryWhereTheCLIDoes(t *testing.T) {
	if got := app.Resume(&index.Session{Session: model.Session{Source: "gemini", ID: "g-1", Cwd: "qa"}}); got != "" {
		t.Errorf("a Gemini session whose directory is only its name = %q", got)
	}
	if got := app.Resume(&index.Session{Session: model.Session{Source: "gemini", ID: "g-1", Cwd: "/w/qa"}}); !strings.HasSuffix(got, "/w/qa && gemini --resume g-1") {
		t.Errorf("a Gemini session in /w/qa = %q", got)
	}
	if got := app.Resume(&index.Session{Session: model.Session{Source: "codex", ID: "c-1"}}); got != "codex resume c-1" {
		t.Errorf("a Codex session, which resumes from anywhere = %q", got)
	}
}
