package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	for _, bad := range []string{"tomorrow", "mondays", "tod", "7", "d", "-3d", "7x", "2026-13-01"} {
		if _, err := app.When(bad, now); err == nil {
			t.Errorf("When(%q) is accepted", bad)
		}
	}
}

// A day's name is the midnight it began with, on this machine's clock: today's, yesterday's,
// and a weekday's — the latest of that name, today included.
func TestWhenADayIsNamed(t *testing.T) {
	friday := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	tuesday := time.Date(2026, 12, 1, 0, 30, 0, 0, time.Local)
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.Local) }
	for _, tc := range []struct {
		now  time.Time
		in   string
		want time.Time
	}{
		{friday, "today", day(11, 13)},
		{friday, " Yesterday ", day(11, 12)},
		{friday, "friday", day(11, 13)},
		{friday, "fri", day(11, 13)},
		{friday, "Thursday", day(11, 12)},
		{friday, "mon", day(11, 9)},
		{friday, "saturday", day(11, 7)},
		{friday, "sun", day(11, 8)},
		{tuesday, "yesterday", day(11, 30)},
		{tuesday, "wednesday", day(11, 25)},
		{tuesday, "today", day(12, 1)},
	} {
		got, err := app.When(tc.in, tc.now)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("When(%q) on %s = %v, %v; want %v", tc.in, tc.now.Format("Mon 01-02 15:04"), got, err, tc.want)
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
	// A session of two turns was opened for its first ask and ended on another: the pack
	// says both, and of a session of one turn its one ask
	text, _, _ = a.Pack(ctx, app.Query{}, 2, false)
	for _, want := range []string{"- Asked: fix the flaky test\n- Last asked: 现在给数据库加索引\n- Concluded: 索引已加上",
		"- Asked: rotate the deploy key\n- Concluded:"} {
		if !strings.Contains(text, want) {
			t.Errorf("the pack does not say %q:\n%s", want, text)
		}
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
	// A reverted thread's later rollout, named for the thread and the rollout, is reopened
	// by the thread's id
	later := index.Session{Session: model.Session{Source: "codex", ID: "c-1_r-2", Cwd: "/w"}}
	if got := app.Resume(&later); !strings.HasSuffix(got, "/w && codex resume c-1") {
		t.Errorf("a reverted thread's later rollout = %q", got)
	}
	if _, argv := app.ResumeArgs(&later); strings.Join(argv, " ") != "codex resume c-1" {
		t.Errorf("argv = %q", argv)
	}

	// Why not, said as it is: the directory unknown is not a CLI without the command
	for _, c := range []struct {
		s    model.Session
		says string
	}{
		{model.Session{Source: "gemini", ID: "g-1", Cwd: "qa"}, "Gemini CLI finds a session only from the directory it was worked in"},
		{model.Session{Source: "gemini", ID: "session-2026-11-10T08-00-1a2b3c4d", Cwd: "/w/qa"}, "Gemini CLI reopens a session by the id it keeps in its file"},
		{model.Session{Source: "dsh", ID: "d-1", Cwd: "/w"}, "has no command that reopens a session by its id"},
		{model.Session{Source: "codex", ID: "c-1"}, ""},
	} {
		s := index.Session{Session: c.s}
		why := app.NoResume(&s)
		if c.says == "" && why != "" || !strings.Contains(why, c.says) || (why == "") != (app.Resume(&s) != "") {
			t.Errorf("%s %s in %q: no resume because %q, resume %q; want it to say %q", c.s.Source, c.s.ID, c.s.Cwd, why, app.Resume(&s), c.says)
		}
	}
}

// A session read from a store added by name is not in its CLI's store here, where the CLI
// looks for it: there is no command for this machine, and the one for the machine it came
// from is given as that — in that machine's shell, as its paths say.
func TestASessionFromAStoreAddedByNameResumesWhereItCameFrom(t *testing.T) {
	box := index.Session{Session: model.Session{Source: "claude@box2", Agent: "claude", ID: "c-1", Cwd: "/w/qa"}}
	if got := app.Resume(&box); got != "" {
		t.Errorf("resume here = %q, want none", got)
	}
	if _, argv := app.ResumeArgs(&box); argv != nil {
		t.Errorf("resume args = %q, want none: asq resume --run would run it here", argv)
	}
	if why := app.NoResume(&box); !strings.Contains(why, "read from claude@box2, not from Claude Code's store on this machine") {
		t.Errorf("no resume because %q", why)
	}
	if got := app.ResumeThere(&box); got != "cd /w/qa && claude --resume c-1" {
		t.Errorf("resume there = %q", got)
	}
	win := index.Session{Session: model.Session{Source: "claude@laptop", Agent: "claude", ID: "c-2", Cwd: `C:\w\qa`}}
	if got := app.ResumeThere(&win); got != `cd /d C:\w\qa && claude --resume c-2` {
		t.Errorf("resume there, from a Windows machine = %q", got)
	}
	// What its CLI reopens nowhere is not reopened there either
	agent := index.Session{Session: model.Session{Source: "claude@box2", Agent: "claude", ID: "x1", Cwd: "/w/qa", Parent: &model.Parent{Session: "c-1"}}}
	if got := app.ResumeThere(&agent); got != "" {
		t.Errorf("resume there for a subagent = %q", got)
	}
	// The CLI's own store, relocated or not, is where it looks
	own := index.Session{Session: model.Session{Source: "claude", Agent: "claude", ID: "c-1", Cwd: "/w/qa"}}
	if app.ResumeThere(&own) != "" || app.Resume(&own) == "" {
		t.Errorf("own store: resume %q, there %q", app.Resume(&own), app.ResumeThere(&own))
	}
}

// What a brief is asked to cover, as the faces are given it: "all" is the whole session,
// not a moment — it is no bound in a filter, and a brief with no bound is of its latest turn.
func TestBriefOf(t *testing.T) {
	if o, err := app.BriefOf(0, 0, "all"); err != nil || !o.All || !o.Since.IsZero() {
		t.Errorf("since all = %+v, %v", o, err)
	}
	if o, err := app.BriefOf(0, 0, "2h"); err != nil || o.All || time.Since(o.Since) < 2*time.Hour-time.Minute {
		t.Errorf("since 2h = %+v, %v", o, err)
	}
	if o, err := app.BriefOf(3, 0, ""); err != nil || o.All || o.Turn != 3 || o.From != 0 {
		t.Errorf("turn 3 = %+v, %v", o, err)
	}
	// Each says which turns the brief covers: given two, one would be dropped unsaid
	for _, tc := range []struct {
		turn, from  int
		since, want string
	}{
		{2, 0, "all", "turn 2 and since all each say which turns the brief covers"},
		{0, 2, "2h", "from 2 and since 2h each say"},
		{3, 2, "", "turn 3 and from 2 each say"},
		{3, 2, " all ", "turn 3, from 2 and since all each say"},
	} {
		if _, err := app.BriefOf(tc.turn, tc.from, tc.since); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("turn %d, from %d, since %q = %v; want %q", tc.turn, tc.from, tc.since, err, tc.want)
		}
	}
	if o, err := app.BriefOf(0, 0, ""); err != nil || o.All || !o.Since.IsZero() {
		t.Errorf("nothing = %+v, %v", o, err)
	}
	if _, err := app.BriefOf(0, 0, "yesterdayish"); err == nil || !strings.HasPrefix(err.Error(), "since: ") {
		t.Errorf("an unreadable since = %v", err)
	}
}

// A handoff is of the whole session unless a part of it is named: its latest turn alone
// leaves out how the session got where it ended.
func TestHandoff(t *testing.T) {
	if o, err := app.Handoff(0, 0, ""); err != nil || !o.All {
		t.Errorf("nothing named = %+v, %v; want the whole session", o, err)
	}
	if o, err := app.Handoff(2, 0, ""); err != nil || o.All || o.Turn != 2 {
		t.Errorf("turn 2 = %+v, %v", o, err)
	}
	if o, err := app.Handoff(0, 3, ""); err != nil || o.All || o.From != 3 {
		t.Errorf("from 3 = %+v, %v", o, err)
	}
	if o, err := app.Handoff(0, 0, "2h"); err != nil || o.All || o.Since.IsZero() {
		t.Errorf("since 2h = %+v, %v", o, err)
	}
	if _, err := app.Handoff(2, 0, "all"); err == nil {
		t.Error("turn 2 and since all are taken together")
	}
}

// An agent asking after its own session: the one its CLI names, when the index has it;
// else the one most recently written to in the directory it asks from.
func TestSelf(t *testing.T) {
	a, h := fixture.Open(t)
	ctx := context.Background()
	self, err := a.Self(ctx, "claude", fixture.Alpha, []string{"/work/beta"})
	if err != nil || !self.ByID || self.ID != fixture.Alpha || self.Newer != nil || self.Resume == "" {
		t.Errorf("by its id = %+v, %v", self, err)
	}
	// An id the index does not hold — Claude Code's own after --continue — or none: the
	// directory decides
	for _, id := range []string{"cccc3333-0000-4000-8000-000000000003", ""} {
		self, err = a.Self(ctx, "claude", id, []string{"/nowhere", "/work/beta"})
		if err != nil || self.ByID || self.ID != fixture.Beta {
			t.Errorf("id %q, in /work/beta = %+v, %v", id, self, err)
		}
	}
	if _, err := a.Self(ctx, "", "", []string{"/work"}); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a directory above the sessions' = %v: a session is in one directory, exactly", err)
	}
	if _, err := a.Self(ctx, "codex", "", []string{"/work/alpha"}); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a CLI this asq does not read = %v", err)
	}

	// A new session in the same directory, written to since: after /clear, the one the
	// agent works in. The id names the old one still, and says that there is a newer
	const newer = "dddd4444-0000-4000-8000-000000000004"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows := `{"type":"user","uuid":"d-u1","timestamp":"` + now + `","sessionId":"` + newer + `","cwd":"/work/alpha","message":{"role":"user","content":"carry on"}}
{"type":"assistant","uuid":"d-a1","timestamp":"` + now + `","sessionId":"` + newer + `","message":{"id":"msg_d1","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"On it."}],"stop_reason":"end_turn"}}
`
	if err := os.WriteFile(filepath.Join(h.Dir, "-work-alpha", newer+".jsonl"), []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	self, err = a.Self(ctx, "claude", fixture.Alpha, nil)
	if err != nil || self.ID != fixture.Alpha || self.Newer == nil || self.Newer.ID != newer {
		t.Errorf("named, with a newer session beside it = %+v, %v", self, err)
	}
	if self, err = a.Self(ctx, "", "", []string{"/work/alpha"}); err != nil || self.ID != newer {
		t.Errorf("by its directory = %+v, %v: the session most recently written to", self, err)
	}
}
