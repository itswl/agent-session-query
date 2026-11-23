package index_test

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
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
	"github.com/itswl/agent-session-query/internal/transcript"
)

func filter(a *app.App) index.Filter { return index.Filter{Stores: a.Stores()} }

func refs(list []index.Session) string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.ID
	}
	return strings.Join(out, " ")
}

func TestSessionsListAndFilter(t *testing.T) {
	a, h := fixture.Open(t)
	ctx := context.Background()

	list, total, err := a.Index.Sessions(ctx, filter(a), index.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || refs(list) != fixture.Alpha+" "+fixture.Beta {
		t.Fatalf("sessions = %d %q: want the two sessions, newest first, the subagent beneath its own", total, refs(list))
	}
	alpha := list[0]
	if alpha.Title != "Fix flaky test" || alpha.Cwd != "/work/alpha" || alpha.Branch != "main" || alpha.Agents != 1 {
		t.Errorf("alpha = %+v", alpha.Session)
	}
	if alpha.Stats.Turns != 2 || alpha.Stats.Tools != 3 || alpha.Stats.Failures != 1 || alpha.Stats.Calls != 5 {
		t.Errorf("alpha stats = %+v", alpha.Stats)
	}
	if alpha.LastAsk != "现在给数据库加索引" || !strings.HasPrefix(alpha.Answer, "索引已加上") {
		t.Errorf("alpha ends %q → %q", alpha.LastAsk, alpha.Answer)
	}

	for name, tc := range map[string]struct {
		f    func(*index.Filter)
		want string
	}{
		"agents":  {func(f *index.Filter) { f.Agents = true }, fixture.Alpha + " " + fixture.Agent + " " + fixture.Beta},
		"project": {func(f *index.Filter) { f.Project = "beta" }, fixture.Beta},
		"branch":  {func(f *index.Filter) { f.Branch = "main" }, fixture.Alpha},
		"text":    {func(f *index.Filter) { f.Text = "flaky" }, fixture.Alpha},
		"since":   {func(f *index.Filter) { f.Since = h.Now.Add(-24 * time.Hour) }, fixture.Alpha},
		"until":   {func(f *index.Filter) { f.Until = h.Now.Add(-24 * time.Hour) }, fixture.Beta},
		"source":  {func(f *index.Filter) { f.Source = "codex" }, ""},
	} {
		f := filter(a)
		tc.f(&f)
		got, _, err := a.Index.Sessions(ctx, f, index.Page{})
		if err != nil {
			t.Fatal(err)
		}
		// the agents list is ordered by when each was last written
		if name == "agents" {
			if len(got) != 3 {
				t.Errorf("%s: %q", name, refs(got))
			}
			continue
		}
		if refs(got) != tc.want {
			t.Errorf("%s: got %q, want %q", name, refs(got), tc.want)
		}
	}
}

// A directory keeps the sessions that ran in it or below it, and not those in a directory
// whose name merely begins the same, as a part of a path would.
func TestADirectoryKeepsWhatRanInItAndBelowIt(t *testing.T) {
	a, _ := fixture.Open(t)
	both := fixture.Alpha + " " + fixture.Beta
	for _, tc := range []struct {
		dirs []string
		want string
	}{
		{[]string{"/work/alpha"}, fixture.Alpha},
		{[]string{"/work"}, both},
		{[]string{"/"}, both},
		{[]string{"/work/alp"}, ""},
		{[]string{"/work/alpha/db"}, ""},
		{[]string{"/Work/Alpha/"}, fixture.Alpha}, // a Mac's disk does not tell the two apart
		{[]string{"/private/work/beta", "/work/beta"}, fixture.Beta},
	} {
		f := filter(a)
		f.Dirs = tc.dirs
		got, _, err := a.Index.Sessions(context.Background(), f, index.Page{})
		if err != nil {
			t.Fatal(err)
		}
		if refs(got) != tc.want {
			t.Errorf("in %q: got %q, want %q", tc.dirs, refs(got), tc.want)
		}
	}
}

func TestResolve(t *testing.T) {
	a, _ := fixture.Open(t)
	ctx := context.Background()
	for _, ref := range []string{"claude/" + fixture.Alpha, fixture.Alpha, "aaaa1111", "claude/aaaa", "aaaa1111#2"} {
		s, err := a.Index.Resolve(ctx, ref, a.Stores())
		if err != nil || s.ID != fixture.Alpha {
			t.Errorf("Resolve(%q) = %q, %v", ref, s.ID, err)
		}
	}
	if _, _, turn := index.SplitRef("claude/aaaa1111#2"); turn != 2 {
		t.Errorf("turn = %d", turn)
	}
	if _, err := a.Index.Resolve(ctx, "claude/nope", a.Stores()); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("an unknown ref: %v", err)
	}
	// A store this process does not read is not seen, even though the index file holds it
	if _, err := a.Index.Resolve(ctx, fixture.Alpha, []string{"codex"}); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("another configuration's session: %v", err)
	}
}

func TestTurnsItemsAndWholeBodies(t *testing.T) {
	a, _ := fixture.Open(t)
	ctx := context.Background()
	alpha, _ := a.Index.Resolve(ctx, fixture.Alpha, a.Stores())

	outline, err := a.Index.Outline(ctx, &alpha)
	if err != nil {
		t.Fatal(err)
	}
	if len(outline) != 2 || outline[0].Index != 1 || len(outline[0].Items) != 0 {
		t.Fatalf("outline = %d turns, first %+v", len(outline), outline[0])
	}
	if outline[0].Steps != 4 || outline[1].Steps != 4 {
		t.Errorf("steps = %d and %d: an outline says how much each turn holds before any of it is fetched", outline[0].Steps, outline[1].Steps)
	}
	if outline[0].Ask != "fix the flaky test" || outline[0].Failures != 1 || !strings.HasPrefix(outline[0].Answer, "The test depended") {
		t.Errorf("turn 1 = %+v", outline[0])
	}
	if len(outline[1].Files) != 1 || outline[1].Files[0].Path != "/work/alpha/db/schema.sql" || !outline[1].Files[0].Changed {
		t.Errorf("turn 2 files = %+v", outline[1].Files)
	}

	turns, err := a.Index.Turns(ctx, &alpha, index.TurnRange{First: 1, Last: 1})
	if err != nil || len(turns) != 1 {
		t.Fatalf("turns = %d, %v", len(turns), err)
	}
	var kinds []string
	for _, it := range turns[0].Items {
		kinds = append(kinds, string(it.Kind))
	}
	if got := strings.Join(kinds, " "); got != "prompt thinking tool reply" {
		t.Errorf("items = %s", got)
	}
	tool := turns[0].Items[2].Tool
	if tool.Name != "Bash" || tool.Command != "go test ./..." || !tool.Failed() || tool.DurMs != 2500 || !strings.Contains(tool.Output, "ECONNREFUSED") {
		t.Errorf("tool = %+v", tool)
	}
	if last, _ := a.Index.Turns(ctx, &alpha, index.TurnRange{First: 1, Last: -1, Tail: 1}); len(last) != 1 || last[0].Index != 2 {
		t.Errorf("the last turn = %+v", last)
	}

	// A long output is kept as a preview in its turn and whole beside it
	beta, _ := a.Index.Resolve(ctx, fixture.Beta, a.Stores())
	turns, _ = a.Index.Turns(ctx, &beta, index.TurnRange{First: 1, Last: 1})
	var build *model.Item
	for i := range turns[0].Items {
		if turns[0].Items[i].Kind == model.ToolUse {
			build = &turns[0].Items[i]
		}
	}
	if build == nil {
		t.Fatal("no tool call in beta")
	}
	whole := strings.Repeat(fixture.LongLine+"\n", 400) + "build finished"
	if build.Tool.OutBytes != len(whole) || len(build.Tool.Output) >= len(whole) || !strings.HasSuffix(build.Tool.Output, "build finished") {
		t.Errorf("preview: %d bytes shown of %d recorded (whole is %d); it must keep the end", len(build.Tool.Output), build.Tool.OutBytes, len(whole))
	}
	item, turn, err := a.Index.Item(ctx, &beta, build.Seq)
	if err != nil || turn != 1 || item.Tool.Output != whole {
		t.Errorf("Item: turn %d, %d bytes, %v", turn, len(item.Tool.Output), err)
	}
	full, _ := a.Index.Turns(ctx, &beta, index.TurnRange{First: 1, Last: 1, Full: true})
	if full[0].Items[build.Seq-full[0].Items[0].Seq].Tool.Output != whole {
		t.Error("a turn read whole carries its outputs whole")
	}
	if _, _, err := a.Index.Item(ctx, &beta, 999); !errors.Is(err, index.ErrNoItem) {
		t.Errorf("a step that is not there: %v", err)
	}
}

func TestSubagentsHangBeneathTheirSession(t *testing.T) {
	a, _ := fixture.Open(t)
	ctx := context.Background()
	alpha, _ := a.Index.Resolve(ctx, fixture.Alpha, a.Stores())
	children, err := a.Index.Children(ctx, &alpha)
	if err != nil || len(children) != 1 {
		t.Fatalf("children = %d, %v", len(children), err)
	}
	child := children[0]
	if child.ID != fixture.Agent || child.Parent == nil || child.Parent.Type != "Explore" || child.Parent.Call != "toolu_agent" || child.Root != alpha.Ref || child.Depth != 1 {
		t.Errorf("child = %+v parent %+v root %q", child.Session, child.Parent, child.Root)
	}
	parent, ok, err := a.Index.Parent(ctx, &child)
	if err != nil || !ok || parent.ID != fixture.Alpha {
		t.Errorf("Parent = %q, %v, %v", parent.ID, ok, err)
	}
	if _, ok, _ := a.Index.Parent(ctx, &alpha); ok {
		t.Error("a session has no parent")
	}
}

func TestSearch(t *testing.T) {
	a, _ := fixture.Open(t)
	ctx := context.Background()
	f := filter(a)
	f.Agents = true
	for query, want := range map[string]string{
		"ECONNREFUSED":        "aaaa1111#1",      // a tool's output
		"econnref":            "aaaa1111#1",      // a prefix, any case
		"数据库":                 "aaaa1111#2",      // CJK, with no spaces to split on
		"加索引":                 "aaaa1111#2",      // … and from the middle of a run
		`"map order"`:         "aaaa1111#1",      // a phrase
		`"order map"`:         "",                // … is in order
		"ask:flaky":           "aaaa1111#1",      // only where the person said it
		"reply:flaky":         "",                //
		"output:finished":     "bbbb2222#1",      // the end of a long output is searchable whole
		"tool:schema":         "aaaa1111#2",      // a tool's input
		"flaky -ECONNREFUSED": "",                // a word that must not be there
		"rotat":               "bbbb2222#1",      //
		"missing indexes":     "aaaa1111#2 x1#1", // the call, and the subagent it started
		"nothinglikethis":     "",

		// Another form of an English word: "Rotated", "fixed by sorting the keys"
		"rotating":      "bbbb2222#1",
		"sorted keys":   "aaaa1111#1",
		`"sorted keys"`: "", // a quoted phrase is as written
		"reply:sorts":   "aaaa1111#1",
		"output:sorts":  "",           // … and in the part asked for
		"-sorting fix":  "",           // the one turn that says fix says sorting
		"索引 orders":     "aaaa1111#2", // Chinese beside English, in a turn's reply
		"ask:索引 order":  "aaaa1111#2", // … the English word in another form, in another part
		"flaky -orders": "aaaa1111#1", // an excluded word is excluded as typed: #1 says order

		// The session's title — "Fix flaky test" — is held with its first turn alone, and
		// so is one taken from how the first ask begins: beta's, "rotate the deploy key"
		"title:flaky":  "aaaa1111#1",
		"title:deploy": "bbbb2222#1",
		"title:数据库":    "",
	} {
		res, err := a.Index.Search(ctx, index.ParseQuery(query), f, index.SearchOptions{})
		if err != nil {
			t.Errorf("%s: %v", query, err)
			continue
		}
		var got []string
		for _, hit := range res.Hits {
			got = append(got, hit.Session.ID[:min(8, len(hit.Session.ID))]+"#"+strings.TrimPrefix(hit.Ref[strings.LastIndex(hit.Ref, "#"):], "#"))
		}
		if !sameSet(got, strings.Fields(want)) {
			t.Errorf("search %q = %v, want %q", query, got, want)
		}
		if res.Total != len(strings.Fields(want)) {
			t.Errorf("search %q total = %d", query, res.Total)
		}
	}

	res, _ := a.Index.Search(ctx, index.ParseQuery("ECONNREFUSED"), f, index.SearchOptions{})
	if len(res.Hits) != 1 || len(res.Hits[0].Matches) == 0 {
		t.Fatalf("hits = %+v", res.Hits)
	}
	m := res.Hits[0].Matches[0]
	if m.Kind != model.ToolUse || m.Part != "output" || m.Tool != "Bash" || !strings.Contains(m.Snippet, "ECONNREFUSED") {
		t.Errorf("match = %+v: it says where the word is, and shows it", m)
	}
	if res.Hits[0].Ask != "fix the flaky test" || res.Hits[0].Ref != "claude/"+fixture.Alpha+"#1" {
		t.Errorf("hit = %+v", res.Hits[0])
	}

	// The filter confines a search like it confines a list
	f.Project = "beta"
	if res, _ := a.Index.Search(ctx, index.ParseQuery("ECONNREFUSED"), f, index.SearchOptions{}); res.Total != 0 {
		t.Errorf("a search in another project found %d", res.Total)
	}

	// … and to one session: its own turns, and not those of the subagent it started,
	// whose transcript is a session of its own
	alpha, err := a.Index.Resolve(ctx, "claude/"+fixture.Alpha, a.Stores())
	if err != nil {
		t.Fatal(err)
	}
	f = filter(a)
	f.Agents = true
	f.SID = alpha.SID()
	res, _ = a.Index.Search(ctx, index.ParseQuery("missing indexes"), f, index.SearchOptions{})
	if res.Total != 1 || res.Sessions != 1 || len(res.Hits) != 1 || res.Hits[0].Ref != "claude/"+fixture.Alpha+"#2" {
		t.Errorf("in one session = %d in %d: %+v", res.Total, res.Sessions, res.Hits)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestReports(t *testing.T) {
	a, h := fixture.Open(t)
	ctx := context.Background()
	f := filter(a)

	total, err := a.Index.UsageTotal(ctx, f, index.Window{})
	if err != nil {
		t.Fatal(err)
	}
	want := model.Usage{Input: 285, Output: 207, CacheRead: 5500, CacheWrite: 50}
	if total.Usage != want || total.Calls != 8 || total.Sessions != 2 {
		t.Errorf("usage = %+v in %d calls of %d sessions, want %+v in 8 of 2: a subagent's tokens are spent too, in the session it ran in", total.Usage, total.Calls, total.Sessions, want)
	}
	byModel, _ := a.Index.Usage(ctx, f, index.Window{}, index.ByModel, 0)
	if len(byModel) != 2 || byModel[0].Key != "claude-opus-5" || byModel[1].Key != "claude-haiku-5" {
		t.Errorf("by model = %+v", byModel)
	}
	day, _ := a.Index.Usage(ctx, f, index.Window{Since: h.Now.Add(-24 * time.Hour)}, index.ByAll, 0)
	if len(day) != 1 || day[0].Usage.Input != 175 {
		t.Errorf("the last day = %+v, want alpha and its agent only", day)
	}
	if _, err := a.Index.Usage(ctx, f, index.Window{}, "colour", 0); err == nil {
		t.Error("an unknown grouping is an error, not a silent total")
	}
	rep, err := a.Index.UsageSummary(ctx, f, index.Window{})
	var keys []string
	for _, w := range rep.Windows {
		keys = append(keys, w.Key)
	}
	if err != nil || strings.Join(keys, " ") != "24h 7d 30d all" || rep.Windows[0].Usage.Input != 175 || rep.Windows[3].Usage.Input != 285 || rep.Total.Usage != want {
		t.Errorf("summary = %+v, %v: the windows every view offers, %v", rep, err, keys)
	}
	// The breakdowns cover the window asked for; the days are the last thirty whichever it is
	day1, err := a.Index.UsageSummary(ctx, f, index.Window{Since: h.Now.Add(-24 * time.Hour)})
	if err != nil || len(day1.ByProject) != 1 || day1.ByProject[0].Key != "/work/alpha" || len(day1.ByDay) != len(rep.ByDay) || len(rep.ByDay) < 2 {
		t.Errorf("the last 24 hours: by project %+v, %d days against %d for all time, %v", day1.ByProject, len(day1.ByDay), len(rep.ByDay), err)
	}

	fails, err := a.Index.Failures(ctx, f, index.Window{}, index.FailureQuery{}, index.Page{})
	if err != nil || fails.Total != 1 || len(fails.Failures) != 1 {
		t.Fatalf("failures = %+v, %v", fails, err)
	}
	if got := fails.Failures[0]; got.Tool != "Bash" || got.Turn != 1 || got.Command != "go test ./..." || got.Session != "claude/"+fixture.Alpha || !strings.Contains(got.Output, "exit status 1") {
		t.Errorf("failure = %+v", got)
	}
	// A tool is named exactly, or by how its name begins; what failed per tool covers the
	// window whichever is asked for
	for tool, want := range map[string]int{"Bash": 1, "bash": 0, "Ba": 0, "Ba*": 1, "B*": 1, "Edit": 0, "*": 1} {
		got, err := a.Index.Failures(ctx, f, index.Window{}, index.FailureQuery{Tool: tool}, index.Page{})
		if err != nil || got.Total != want || len(got.Failures) != want {
			t.Errorf("failures of %q = %d, %v; want %d", tool, got.Total, err, want)
		}
		if len(got.ByTool) != 1 || got.ByTool[0].Tool != "Bash" || got.ByTool[0].Count != 1 {
			t.Errorf("failures of %q: by tool = %+v, want the window's whatever the choice", tool, got.ByTool)
		}
	}
	// … and by the words its command or the end of its output holds
	for query, want := range map[string]int{
		"econnrefused": 1, "go test": 1, `"go test"`: 1, `"test go"`: 0, "-ECONNREFUSED": 0, "ECONNREFUSED -postgres": 1,
		"exit status 1": 1, "TestOrder 5432": 1, "5433": 0, "": 1,
	} {
		got, err := a.Index.Failures(ctx, f, index.Window{}, index.FailureQuery{Text: query}, index.Page{})
		if err != nil || got.Total != want || len(got.Failures) != want || len(got.ByTool) != 1 {
			t.Errorf("failures holding %q = %d, %v; want %d", query, got.Total, err, want)
		}
	}

	files, n, err := a.Index.Files(ctx, f, index.Window{}, "schema.sql", true, index.Page{})
	if err != nil || n != 1 || files[0].Turn != 2 || !files[0].Changed || files[0].Ask != "现在给数据库加索引" {
		t.Errorf("files = %+v, %d, %v", files, n, err)
	}

	digest, err := a.Index.Digest(ctx, f, index.Window{Since: h.Now.Add(-7 * 24 * time.Hour)})
	if err != nil || len(digest.Projects) != 2 || digest.Total.Sessions != 2 || digest.Total.Turns != 3 || digest.Total.Tools != 4 || digest.Total.Failures != 1 || digest.Total.Usage.Input != 285 {
		t.Fatalf("digest = %+v, %v", digest, err)
	}
	for _, d := range digest.Projects {
		// A subagent's transcript is not a session and its directive not a person's turn —
		// but its model call is work the session paid for
		if d.Cwd == "/work/alpha" && (d.Sessions != 1 || d.Turns != 2 || d.Models["claude-haiku-5"] != 1 || d.Changed["config"]+d.Changed["code"]+d.Changed["docs"]+d.Changed["test"] != 1) {
			t.Errorf("alpha's digest = %+v", d)
		}
	}

	projects, err := a.Index.Projects(ctx, f)
	if err != nil || len(projects) != 2 || projects[0].Cwd != "/work/alpha" || projects[0].Sessions != 1 {
		t.Errorf("projects = %+v, %v", projects, err)
	}
}

func TestSyncFollowsTheStore(t *testing.T) {
	h := fixture.Write(t)
	a, err := app.Open(h.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	changes, stop := a.Index.Subscribe()
	defer stop()

	first, err := a.Sync(ctx, index.SyncOptions{})
	if err != nil || first.Scanned != 3 || first.Read != 3 || len(first.Changes) != 3 {
		t.Fatalf("first sync = %+v, %v", first, err)
	}
	for range 3 {
		<-changes
	}
	stamps, _ := a.Index.TurnStamps(ctx, filter(a), index.Window{}, time.Time{})
	if len(stamps) != 4 {
		t.Fatalf("turn stamps = %d, want the four turns", len(stamps))
	}

	// Nothing changed: nothing is read
	again, err := a.Sync(ctx, index.SyncOptions{})
	if err != nil || again.Read != 0 || len(again.Changes) != 0 {
		t.Errorf("a sync of an unchanged store = %+v, %v", again, err)
	}

	// One transcript grows: it alone is read, and only its new turn is new
	path := filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"user","uuid":"b-u9","timestamp":"` + h.Now.Format(time.RFC3339) + `","sessionId":"` + fixture.Beta + `","cwd":"/work/beta","message":{"role":"user","content":"and tell the team"}}` + "\n")
	f.Close()
	grown, err := a.Sync(ctx, index.SyncOptions{Force: true})
	if err != nil || grown.Read != 1 || len(grown.Changes) != 1 || grown.Changes[0].ID != fixture.Beta {
		t.Fatalf("after an append = %+v, %v", grown, err)
	}
	after, _ := a.Index.TurnStamps(ctx, filter(a), index.Window{}, time.Time{})
	if len(after) != 5 {
		t.Fatalf("turn stamps = %d", len(after))
	}
	before := map[[2]int64]string{}
	for _, s := range stamps {
		before[[2]int64{s.SID, int64(s.Index)}] = s.Hash
	}
	moved := 0
	for _, s := range after {
		if old, ok := before[[2]int64{s.SID, int64(s.Index)}]; ok && old != s.Hash {
			moved++
		}
	}
	if moved != 0 {
		t.Errorf("%d turns that did not change have a new hash: an exporter would send them again", moved)
	}
	beta, _ := a.Index.Resolve(ctx, fixture.Beta, a.Stores())
	if beta.Stats.Turns != 2 || beta.LastAsk != "and tell the team" {
		t.Errorf("beta = %+v, last ask %q", beta.Stats, beta.LastAsk)
	}

	// A transcript is deleted: its session goes, and so does what was indexed of it
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	gone, err := a.Sync(ctx, index.SyncOptions{})
	if err != nil || gone.Removed != 1 || len(gone.Changes) != 1 || !gone.Changes[0].Gone {
		t.Fatalf("after a delete = %+v, %v", gone, err)
	}
	if _, err := a.Index.Resolve(ctx, fixture.Beta, a.Stores()); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a deleted session still resolves: %v", err)
	}
	res, _ := a.Index.Search(ctx, index.ParseQuery("rotate"), filter(a), index.SearchOptions{})
	if res.Total != 0 {
		t.Errorf("a deleted session is still found by search (%d)", res.Total)
	}
	if st := a.Index.State(); st.Running || st.At.IsZero() {
		t.Errorf("state = %+v", st)
	}
}

func TestRebuildReadsTheSame(t *testing.T) {
	a, _ := fixture.Open(t)
	ctx := context.Background()
	before, _ := a.Index.TurnStamps(ctx, filter(a), index.Window{}, time.Time{})
	res, err := a.Sync(ctx, index.SyncOptions{Rebuild: true})
	if err != nil || res.Read != 3 {
		t.Fatalf("rebuild = %+v, %v", res, err)
	}
	after, _ := a.Index.TurnStamps(ctx, filter(a), index.Window{}, time.Time{})
	if len(before) != len(after) {
		t.Fatalf("turns %d → %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Hash != after[i].Hash {
			t.Errorf("turn %d of sid %d hashes differently on a second read: the hash must name the content, not the read", before[i].Index, before[i].SID)
		}
	}
	if len(res.Changes) != 0 {
		t.Errorf("a rebuild that found nothing new announced %d changes", len(res.Changes))
	}
}

// totalled is a store whose CLI also keeps totals of its own for a session.
type totalled struct {
	sources.Source
	recorded map[string]*model.Recorded // by session id
}

func (s *totalled) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	if err := s.Source.Read(ctx, ref, w); err != nil {
		return err
	}
	if r, ok := s.recorded[w.S.ID]; ok {
		w.S.Recorded = r
	}
	return nil
}

// What a CLI totalled for a session beyond what its responses add up to is spending too —
// OpenCode names a session with a model call that leaves no message. It is in the
// session's own figures and, once, in everything summed from the usage table; and a
// total that holds less than the responses do is not a total of them.
func TestWhatASessionRecordedBeyondItsResponses(t *testing.T) {
	a, _ := fixture.Open(t)
	ctx := context.Background()
	stores := a.Set.Present()
	inner := stores[0].Source
	sync := func(recorded map[string]*model.Recorded) {
		t.Helper()
		stores[0].Source = &totalled{Source: inner, recorded: recorded}
		if _, err := a.Index.Sync(ctx, stores, index.SyncOptions{Rebuild: true}); err != nil {
			t.Fatal(err)
		}
	}
	bySession := func() map[string]index.UsageRow {
		t.Helper()
		rows, err := a.Index.Usage(ctx, filter(a), index.Window{}, index.BySession, 0)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]index.UsageRow{}
		for _, r := range rows {
			out[strings.TrimPrefix(r.Key, "claude/")] = r
		}
		return out
	}

	// Beta's two responses add up to 110 in and 25 out; alpha's to 170, 175, 5500 read
	// from cache and 50 written to it
	alphaSum := model.Usage{Input: 170, Output: 175, CacheRead: 5500, CacheWrite: 50}
	betaTotal := model.Usage{Input: 160, Output: 32, CacheRead: 400, Reasoning: 3}
	sync(map[string]*model.Recorded{
		fixture.Beta:  {Usage: &betaTotal, CostUSD: 0.5},
		fixture.Alpha: {Usage: &model.Usage{Input: 900, Output: 900, CacheRead: 5499, CacheWrite: 900}},
	})

	beta, err := a.Index.Resolve(ctx, fixture.Beta, a.Stores())
	if err != nil || beta.Stats.Usage != betaTotal || beta.Stats.CostUSD != 0.5 || beta.Stats.Calls != 2 {
		t.Errorf("beta = %+v, %v; want the recorded total over its two calls", beta.Stats, err)
	}
	rows := bySession()
	if got := rows[fixture.Beta]; got.Usage != betaTotal || got.Calls != 2 {
		t.Errorf("beta in the usage table = %+v, want %+v: what no response accounts for is entered once", got, betaTotal)
	}
	// The turn's own calls say what they said: the rest belongs to no one of them
	turns, err := a.Index.Outline(ctx, &beta)
	if err != nil || len(turns) != 1 {
		t.Fatalf("turns = %d, %v", len(turns), err)
	}
	if last := turns[0]; last.Usage != (model.Usage{Input: 110, Output: 25}) {
		t.Errorf("beta's turn = %+v, want what its own responses carried", last.Usage)
	}
	// One counter short of covering alpha's responses: the responses stand
	alpha, _ := a.Index.Resolve(ctx, fixture.Alpha, a.Stores())
	if alpha.Stats.Usage != alphaSum || rows[fixture.Alpha].Usage != alphaSum {
		t.Errorf("alpha = %+v, in the table %+v; want %+v", alpha.Stats.Usage, rows[fixture.Alpha].Usage, alphaSum)
	}
	total, _ := a.Index.UsageTotal(ctx, filter(a), index.Window{})
	want := alphaSum
	want.Add(betaTotal)
	want.Add(model.Usage{Input: 5, Output: 7}) // alpha's subagent
	if total.Usage != want {
		t.Errorf("total = %+v, want %+v", total.Usage, want)
	}

	// The same total read again changes nothing, and one that moved is followed
	sync(map[string]*model.Recorded{fixture.Beta: {Usage: &betaTotal}})
	if got := bySession()[fixture.Beta]; got.Usage != betaTotal {
		t.Errorf("read again, beta = %+v", got.Usage)
	}
	more := betaTotal
	more.Input += 40
	sync(map[string]*model.Recorded{fixture.Beta: {Usage: &more}})
	if got := bySession()[fixture.Beta]; got.Usage != more {
		t.Errorf("after the total moved, beta = %+v, want %+v", got.Usage, more)
	}
	// And with none recorded any more, the responses are all there is
	sync(nil)
	if got := bySession()[fixture.Beta]; got.Usage != (model.Usage{Input: 110, Output: 25}) {
		t.Errorf("with nothing recorded, beta = %+v", got.Usage)
	}
}

// partial is a store that lists some of its sessions and says it could not list the rest.
type partial struct {
	sources.Source
	hide string // a ref key left out of the listing
	err  error
}

func (p *partial) Scan(ctx context.Context) ([]sources.Ref, error) {
	refs, err := p.Source.Scan(ctx)
	if err != nil || p.err == nil {
		return refs, err
	}
	kept := refs[:0]
	for _, r := range refs {
		if !strings.Contains(r.Key, p.hide) {
			kept = append(kept, r)
		}
	}
	return kept, p.err
}

// A store read in part gives what it listed and loses nothing: a session it could not
// list is not a session that was deleted.
func TestAPartialScanDeletesNothing(t *testing.T) {
	a, h := fixture.Open(t)
	ctx := context.Background()
	stores := a.Set.Present()
	if len(stores) != 1 {
		t.Fatalf("stores = %d", len(stores))
	}
	broken := &partial{Source: stores[0].Source, hide: fixture.Beta, err: errors.New("one project could not be read")}
	stores[0].Source = broken

	// Alpha grows while beta's directory is unreadable
	f, err := os.OpenFile(filepath.Join(h.Dir, "-work-alpha", fixture.Alpha+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"user","uuid":"a-u9","timestamp":"` + h.Now.Format(time.RFC3339) + `","sessionId":"` + fixture.Alpha + `","cwd":"/work/alpha","message":{"role":"user","content":"one more thing"}}` + "\n")
	f.Close()

	res, err := a.Index.Sync(ctx, stores, index.SyncOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 {
		t.Errorf("%d sessions removed: what a short list leaves out is not gone", res.Removed)
	}
	if res.Read != 1 {
		t.Errorf("read = %d: the part that could be listed is still followed", res.Read)
	}
	if _, err := a.Index.Resolve(ctx, fixture.Beta, a.Stores()); err != nil {
		t.Errorf("the session that could not be listed: %v", err)
	}
	alpha, _ := a.Index.Resolve(ctx, fixture.Alpha, a.Stores())
	if alpha.LastAsk != "one more thing" {
		t.Errorf("alpha's last ask = %q", alpha.LastAsk)
	}
	warnings, _ := a.Index.Warnings(ctx, a.Stores())
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error, "one project could not be read") {
		t.Errorf("warnings = %+v: a store read in part says so", warnings)
	}

	// Readable again, and beta really deleted: now it goes
	broken.err = nil
	os.Remove(filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl"))
	if res, err := a.Index.Sync(ctx, stores, index.SyncOptions{}); err != nil || res.Removed != 1 {
		t.Errorf("after the store reads whole again: %+v, %v", res, err)
	}
	if warnings, _ := a.Index.Warnings(ctx, a.Stores()); len(warnings) != 0 {
		t.Errorf("warnings = %+v", warnings)
	}
}

// A store nobody could look into is not an empty store: its sessions stay, and it is
// said that it could not be read.
func TestAnUnreadableStoreIsSaidSoAndLosesNothing(t *testing.T) {
	a, h := fixture.Open(t)
	ctx := context.Background()
	sound := a.Index.State().Stores
	if sound == "" {
		t.Fatal("a sync leaves no word on the condition of the stores")
	}
	for _, dir := range []string{filepath.Join(h.Dir, "-work-beta"), h.Dir} {
		restore := sourcetest.Unreadable(t, dir)
		res, err := a.Sync(ctx, index.SyncOptions{})
		restore()
		if err != nil {
			t.Fatal(err)
		}
		// Whoever keeps a health report learns from this that it is out of date
		if a.Index.State().Stores == sound {
			t.Errorf("%s unreadable, and the stores' condition reads as before", filepath.Base(dir))
		}
		if res.Removed != 0 {
			t.Errorf("%s unreadable: %d sessions removed", filepath.Base(dir), res.Removed)
		}
		warnings, _ := a.Index.Warnings(ctx, a.Stores())
		if len(warnings) != 1 || warnings[0].Source != "claude" || warnings[0].Error == "" {
			t.Errorf("%s unreadable: warnings = %+v", filepath.Base(dir), warnings)
		}
		if list, total, _ := a.Index.Sessions(ctx, filter(a), index.Page{}); total != 2 {
			t.Errorf("%s unreadable: %d sessions listed (%s)", filepath.Base(dir), total, refs(list))
		}
	}
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if warnings, _ := a.Index.Warnings(ctx, a.Stores()); len(warnings) != 0 {
		t.Errorf("readable again, and still warned about: %+v", warnings)
	}
	if got := a.Index.State().Stores; got != sound {
		t.Errorf("readable again, and the stores' condition is %q, not the %q it was", got, sound)
	}
}
