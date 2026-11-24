package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
)

// A session whose text tries everything a transcript can hold that a renderer must not
// act on: markup, a fence inside an output, escape sequences, a key.
func hostile() (*index.Session, []model.Turn) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	exit := 1
	s := &index.Session{
		Session: model.Session{Source: "claude", Agent: "claude", ID: "3f2a9c1e-aaaa", Title: `<img src=x onerror=alert(1)> & "quotes"`,
			Cwd: "/work/<proj>", Branch: "main", Model: "claude-opus-5", StartedAt: at, UpdatedAt: at.Add(time.Hour)},
		Stats: model.Stats{Turns: 1, Tools: 1, Failures: 1, Usage: model.Usage{Input: 1200, Output: 300, CacheRead: 8000}},
		Ref:   "claude/3f2a9c1e-aaaa",
	}
	turns := []model.Turn{{
		Index: 1, StartedAt: at, EndedAt: at.Add(90 * time.Second),
		Ask:    "<script>alert('ask')</script> fix it",
		Answer: "Done — see </pre><script>alert('answer')</script>",
		Tools:  1, Failures: 1, ToolKinds: map[string]int{"exec": 1},
		Files: []model.File{{Path: "/work/<proj>/main.go", Changed: true}},
		Usage: model.Usage{Input: 1200, Output: 300, CacheRead: 8000},
		Items: []model.Item{
			{Seq: 0, Kind: model.Prompt, At: at, Text: "<script>alert('ask')</script> fix it"},
			{Seq: 1, Kind: model.Thinking, At: at.Add(time.Second), Text: "thinking </details><b>bold</b>"},
			{Seq: 2, Kind: model.ToolUse, At: at.Add(2 * time.Second), Tool: &model.Tool{CallID: "t1", Name: "Bash", Kind: model.ToolExec,
				Input: json.RawMessage(`{"command":"cat README.md"}`), Command: "cat README.md", Done: true, Status: model.StatusError, Exit: &exit, DurMs: 2300,
				Output: "```go\nfmt.Println(\"a fence inside an output\")\n```\n\x1b[31mred\x1b[0m </code><script>alert('out')</script>\nkey sk-" + "Zx9Qw7Lm3Pv5Tr8Kd2Nf6Hb4"}},
			{Seq: 3, Kind: model.Event, At: at.Add(3 * time.Second), Event: model.EventCompaction, Text: "compacted <i>here</i>"},
			{Seq: 4, Kind: model.Reply, At: at.Add(4 * time.Second), Text: "Done — see </pre><script>alert('answer')</script>"},
		},
	}}
	return s, turns
}

func TestExportHTMLRunsNothing(t *testing.T) {
	s, turns := hostile()
	page, err := Export(s, turns, FormatHTML)
	if err != nil {
		t.Fatal(err)
	}
	for _, live := range []string{"<script", "<img", "<b>bold", "<i>here</i>", "\x1b["} {
		if strings.Contains(page, live) {
			t.Errorf("the page carries %q as markup, not as text", live)
		}
	}
	for _, shown := range []string{"&lt;script&gt;alert(&#39;ask&#39;)&lt;/script&gt; fix it", "&lt;img src=x onerror=alert(1)&gt;", "cat README.md", "a fence inside an output"} {
		if !strings.Contains(page, shown) {
			t.Errorf("the page does not show %q", shown)
		}
	}
	for _, outside := range []string{"http://", "https://", "<link", "<iframe", " src=\""} {
		if strings.Contains(page, outside) {
			t.Errorf("the page reaches outside itself (%q): it must stand on its own", outside)
		}
	}
}

func TestExportMarkdownKeepsItsFences(t *testing.T) {
	s, turns := hostile()
	md, err := Export(s, turns, FormatMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	// An output that holds a fence is wrapped in a longer one, or it would end its own block
	if !strings.Contains(md, "````\n```go\n") {
		t.Errorf("the output's own fence closes the block it is quoted in:\n%s", md)
	}
	for _, want := range []string{"# ", "ref: claude/3f2a9c1e-aaaa", "fix it", "cat README.md", "exit 1", "compaction"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown: no %q", want)
		}
	}
	if strings.Contains(md, "\x1b[") {
		t.Error("escape sequences reach a document")
	}
	if !strings.Contains(md, "```\nthinking </details>") {
		t.Error("reasoning that closes a tag closes the block it is quoted in")
	}
}

func TestExportJSONIsTheRecord(t *testing.T) {
	s, turns := hostile()
	raw, err := Export(s, turns, FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Session struct{ Ref, Title string }
		Turns   []model.Turn
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	// An export is the data: it goes back as stored, key and all — it is the owner's file
	if doc.Session.Ref != s.Ref || len(doc.Turns) != 1 || len(doc.Turns[0].Items) != 5 || !strings.Contains(doc.Turns[0].Items[2].Tool.Output, "sk-Zx9Qw7") {
		t.Errorf("json = %+v", doc)
	}

	lines, err := Export(s, turns, FormatJSONL)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(lines), "\n") {
		var row struct {
			Type string `json:"type"`
			Turn int    `json:"turn"`
			Seq  *int   `json:"seq"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("a line is not an object: %s", line)
		}
		kinds = append(kinds, row.Type)
		if row.Type == "item" && (row.Turn != 1 || row.Seq == nil) {
			t.Errorf("an item says which turn it is in: %s", line)
		}
	}
	if got := strings.Join(kinds, " "); got != "session turn item item item item item" {
		t.Errorf("jsonl = %s", got)
	}
	if _, err := Export(s, turns, "docx"); err == nil {
		t.Error("an unknown format is accepted")
	}
}

func TestTextIsPlainUnlessATerminalAsks(t *testing.T) {
	s, turns := hostile()
	var plain, colour bytes.Buffer
	Turn(&plain, s, &turns[0], TurnOptions{}, Style{})
	Turn(&colour, s, &turns[0], TurnOptions{}, Style{Color: true})
	if strings.Contains(plain.String(), "\x1b") {
		t.Errorf("a plain turn carries escapes — its own or the transcript's:\n%q", plain.String())
	}
	if !strings.Contains(colour.String(), "\x1b[") {
		t.Error("a terminal asked for colour and got none")
	}
	if strings.Contains(colour.String(), "\x1b[31mred") {
		t.Error("the transcript's own colour codes reach the terminal")
	}
	for _, want := range []string{"Bash", "cat README.md", "exit 1", "[2]", "compaction"} {
		if !strings.Contains(plain.String(), want) {
			t.Errorf("turn: no %q in\n%s", want, plain.String())
		}
	}

	// A budget stops between steps and says so
	var cut bytes.Buffer
	Turn(&cut, s, &turns[0], TurnOptions{Full: true, MaxChars: 80}, Style{})
	if !strings.Contains(cut.String(), "more steps left out") || strings.Contains(cut.String(), "Done — see") {
		t.Errorf("a turn past its budget:\n%s", cut.String())
	}

	var one bytes.Buffer
	Item(&one, s, 1, &turns[0].Items[2], Style{})
	if !strings.Contains(one.String(), "step 2") || !strings.Contains(one.String(), "a fence inside an output") {
		t.Errorf("item:\n%s", one.String())
	}
}

// An event its CLI writes as a matter of course is said quietly, and one worth noticing
// keeps the colour that says so: a turn's routine steps do not look like its failure.
func TestARoutineEventIsSaidQuietly(t *testing.T) {
	s, turns := hostile()
	at := turns[0].StartedAt
	turn := model.Turn{Index: 1, StartedAt: at, Ask: "summarise the README", Items: []model.Item{
		{Seq: 0, Kind: model.Prompt, At: at, Text: "summarise the README"},
		{Seq: 1, Kind: model.Event, At: at.Add(time.Second), Event: "error", Text: "402 Insufficient Balance"},
		{Seq: 2, Kind: model.Event, At: at.Add(2 * time.Second), Event: "step", Text: "Step 1 · 2s", Routine: true},
	}}
	var plain, colour bytes.Buffer
	Turn(&plain, s, &turn, TurnOptions{}, Style{})
	Turn(&colour, s, &turn, TurnOptions{}, Style{Color: true})
	for _, want := range []string{"event error 402 Insufficient Balance", "event step Step 1 · 2s"} {
		if !strings.Contains(plain.String(), want) {
			t.Errorf("no %q in\n%s", want, plain.String())
		}
	}
	yellow, dim := "\x1b[33m", "\x1b[2m"
	if out := colour.String(); !strings.Contains(out, yellow+"event error") || !strings.Contains(out, dim+"event step") || strings.Contains(out, yellow+"event step") {
		t.Errorf("the error is to stand out and the step not:\n%q", out)
	}
}

func TestShortRefs(t *testing.T) {
	mk := func(source, id string) index.Session {
		return index.Session{Session: model.Session{Source: source, ID: id}}
	}
	got := ShortRefs([]index.Session{
		mk("claude", "3f2a9c1e-0000-4000-8000-000000000001"),
		mk("claude", "3f2a9c1e-0000-4000-9999-000000000002"), // the same first eight: both grow until they differ
		mk("codex", "3f2a9c1e-0000-4000-8000-000000000001"),  // another store's is no clash
		mk("pi", "abc"),
	})
	want := []string{"claude/3f2a9c1e-0000-4000-8", "claude/3f2a9c1e-0000-4000-9", "codex/3f2a9c1e", "pi/abc"}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i][:len(want[i])-1]) || got[0] == got[1] {
			t.Errorf("ShortRefs = %q, want like %q", got, want)
			break
		}
	}
	if got[2] != "codex/3f2a9c1e" || got[3] != "pi/abc" {
		t.Errorf("ShortRefs = %q", got)
	}
	// A word every id of a store opens with tells nothing apart: counting starts after it
	named := ShortRefs([]index.Session{mk("dsh", "session-25a910c0-7b99-4f54-a741-2ded94506463"), mk("opencode", "ses_7f3a9c1e2b4d5a6f"), mk("claude", "deadbeef-0000-4000-8000-000000000001"), mk("x", "ab-cdef0123456789")})
	if want := []string{"dsh/session-25a910c0", "opencode/ses_7f3a9c1e", "claude/deadbeef", "x/ab-cdef0"}; strings.Join(named, " ") != strings.Join(want, " ") {
		t.Errorf("ShortRefs = %q, want %q", named, want)
	}
}

// A brief names a failed call by what was run and how it ended. One whose command did
// not pass later quotes the end of what it printed, where a command says why, fenced so
// that what it printed cannot end the block; one that passed later says where, and is not
// handed over as an open problem — in the turn, nor in the list of turns.
func TestBriefNamesWhatFailed(t *testing.T) {
	s, turns := hostile()
	exit := 1
	failed := index.Failure{Turn: 1, Seq: 2, Tool: "Bash", Status: "error", Exit: &exit, Command: "cat README.md",
		Output: "first line\n```\n\x1b[31mNo such file\x1b[0m\n\nkey " + "sk-" + "Zx9Qw7Lm3Pv5Tr8Kd2Nf6Hb4\n"}
	text, err := Brief(s, turns, BriefOptions{Failures: []index.Failure{failed}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Turn 1", "- Asked: ", "- Failed: Bash `cat README.md` — error, exit 1 [step 2]\n  ````\n  first line\n  ```\n  No such file\n  key [redacted]\n  ````\n", "## State at the end", "main.go"} {
		if !strings.Contains(text, want) {
			t.Errorf("brief: no %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") || strings.Contains(text, "Zx9Qw7") {
		t.Errorf("a quoted output keeps its escapes or its key:\n%s", text)
	}

	// The same command passed two turns on: where, and how it was run then
	second := turns[0]
	second.Index, second.Failures, second.StartedAt = 2, 0, turns[0].StartedAt.Add(time.Hour)
	third := second
	third.Index, third.StartedAt = 3, second.StartedAt.Add(time.Hour)
	failed.Later = &index.Later{Turn: 3, Seq: 9, At: failed.At.Add(40 * time.Minute), Command: "cat ./README.md"}
	failed.At = turns[0].StartedAt
	failed.Later.At = failed.At.Add(40 * time.Minute)
	all := []model.Turn{turns[0], second, third}
	text, err = Brief(s, all, BriefOptions{Turn: 1, Failures: []index.Failure{failed}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- Failed, passed later: Bash `cat README.md` — error, exit 1 [step 2] · passed 40m00s later: #3 step 9, as `cat ./README.md`\n",
		"- #1 10-01 ", " fix it · 1 failed, passed later\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("brief of a failure that passed later: no %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "- Failed: ") || strings.Contains(text, "first line") || strings.Contains(text, "✗") {
		t.Errorf("a failure that passed later is handed over as an open one:\n%s", text)
	}

	// More than a turn names: the open ones first, whatever their order — a failure that
	// passed later is not what is left out of the open ones' room
	var many []index.Failure
	for i := range 5 {
		f := failed
		f.Seq, f.Command = 10+i, fmt.Sprintf("make step%d", i)
		if i == 4 {
			f.Later = nil
		}
		many = append(many, f)
	}
	all[0].Failures = 5
	text, _ = Brief(s, all, BriefOptions{Turn: 1, Failures: many})
	if !strings.Contains(text, "- Failed: Bash `make step4`") || !strings.Contains(text, "- … and 1 more failed calls, 1 of them passed later\n") ||
		!strings.Contains(text, " fix it · 5 failed, 4 passed later\n") {
		t.Errorf("five failures, the last of them open:\n%s", text)
	}

	// Commands are quoted whole, backticks and all
	failed.Command, failed.Later = "echo `date` && make", nil
	text, _ = Brief(s, turns, BriefOptions{Failures: []index.Failure{failed}})
	if !strings.Contains(text, "Bash ``echo `date` && make`` — ") {
		t.Errorf("a command with a backtick in it ends its own code span:\n%s", text)
	}

	for _, o := range []BriefOptions{{Turn: 5}, {From: 5}, {From: -1}} {
		if _, err := Brief(s, turns, o); err == nil {
			t.Errorf("%+v: a turn the session does not have is accepted", o)
		}
	}
	if _, err := Brief(s, turns, BriefOptions{Since: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}); err == nil {
		t.Error("a delta with nothing in it is an empty brief instead of an error")
	}
}

// long is a session of n turns, each changing a file and asking and answering at length.
func long(n int) (*index.Session, []model.Turn) {
	s, one := hostile()
	s.Title, s.Cwd = "A long session", "/work/proj"
	var turns []model.Turn
	for i := 1; i <= n; i++ {
		t := one[0]
		t.Index, t.Failures, t.Items = i, 0, nil
		t.StartedAt = one[0].StartedAt.Add(time.Duration(i) * time.Hour)
		t.EndedAt = t.StartedAt.Add(time.Minute)
		t.Ask = fmt.Sprintf("ask %d: %s", i, strings.Repeat("the words of a long ask ", 20))
		t.Answer = fmt.Sprintf("answer %d: %s", i, strings.Repeat("the words of a long answer ", 30))
		t.Files = []model.File{{Path: fmt.Sprintf("/work/proj/pkg/f%d.go", i), Changed: true}, {Path: "/work/proj/shared.go", Changed: true}, {Path: "/work/proj/README.md"}}
		turns = append(turns, t)
	}
	return s, turns
}

// A brief of a stretch — a turn and every one after it, or the whole session — opens with
// what the stretch adds up to, each file it changed against the last turn of it that
// changed it, and spells out its latest turns.
func TestBriefOfAStretch(t *testing.T) {
	s, turns := long(30)
	plan, err := PlanBrief(s, turns, BriefOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Covered) != 30 || len(plan.Detailed) != briefMaxTurns || plan.Detailed[0] != 30-briefMaxTurns+1 {
		t.Errorf("the whole session covers %d turns and details %v", len(plan.Covered), plan.Detailed)
	}
	if !plan.Names(1) || !plan.Names(30) || plan.Names(10) {
		t.Errorf("a brief names the turns it lists and details, and no others: %v %v", plan.Listed, plan.Detailed)
	}
	var shared *index.FileTouch
	for i := range plan.Files {
		if f := &plan.Files[i]; f.Path == "/work/proj/shared.go" && f.Turn == 30 && f.Session == s.Ref && !f.Ended.IsZero() {
			shared = f
		}
	}
	if shared == nil {
		t.Fatalf("a file changed in every turn is not looked at against the last: %+v", plan.Files)
	}
	shared.Now = &index.FileNow{State: index.FileChanged, By: index.ByContent}
	text, err := Brief(s, turns, BriefOptions{All: true, Files: plan.Files})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## The whole session\n", "- 30 turns: #1 → #30", "- Changed: 31 files — 31 code\n",
		"  - shared.go (#30) · now: changed since — what turn 30 wrote is no longer all there\n", "  - … and 11 more, changed earlier\n",
		"- Read: README.md\n", "- … only the latest 12 turns are detailed below", "## Turn 19\n", "## Turn 30\n",
		"  - shared.go · now: changed since — what this turn wrote is no longer all there\n", "`--turn N` one turn, `--since <when>` everything after a moment.\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("the whole session: no %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "## Turn 18\n") {
		t.Error("the whole session spells out more than its latest turns")
	}

	text, err = Brief(s, turns, BriefOptions{From: 28})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## From turn 28\n", "- 3 turns: #28 → #30", "## Turn 28\n", "## Turn 30\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("from a turn: no %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "## Turn 27") || strings.Contains(text, "only the latest") {
		t.Errorf("from turn 28:\n%s", text)
	}
}

// Held to a budget, a brief spells out fewer turns, then says less of each — and still
// ends with where things stand.
func TestBriefKeepsToItsBudget(t *testing.T) {
	s, turns := long(40)
	whole, err := Brief(s, turns, BriefOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{len(whole) * 3 / 4, len(whole) / 2, 4000} {
		text, err := Brief(s, turns, BriefOptions{All: true, MaxChars: budget})
		if err != nil {
			t.Fatal(err)
		}
		if len(text) > budget || len(text) >= len(whole) {
			t.Errorf("held to %d, a brief of %d is %d", budget, len(whole), len(text))
		}
		for _, want := range []string{"## The whole session", "## Turn 40\n", "## State at the end", "Dig deeper"} {
			if !strings.Contains(text, want) {
				t.Errorf("held to %d: no %q in\n%s", budget, want, text)
			}
		}
	}

	// Below what it takes at the least detail, it says less of the turn's ask and answer,
	// then leaves out the list of turns, the turn it spells out and what the stretch adds
	// up to, a part at a time: where things stand at the end is what it keeps
	least, _ := Brief(s, turns, BriefOptions{All: true, MaxChars: len(whole) / 4})
	floor, _ := Brief(s, turns, BriefOptions{All: true, MaxChars: 1})
	if !strings.Contains(least, "## Turns\n") || !strings.Contains(least, "## Turn 40\n") || len(floor) >= len(least) {
		t.Fatalf("the least detail is not what was meant (%d, %d):\n%s", len(least), len(floor), least)
	}
	state := "\n## State at the end\n\nanswer 40: the words"
	for _, tc := range []struct {
		o         BriefOptions
		want, not []string
	}{
		{BriefOptions{All: true, MaxChars: len(least) - 1}, []string{"## Turns\n", "## The whole session", "## Turn 40\n"}, nil},
		{BriefOptions{All: true, MaxChars: 1500}, []string{"## The whole session", "- … no turn is detailed below, to keep to the budget\n"}, []string{"## Turns\n", "## Turn 40"}},
		{BriefOptions{MaxChars: 1500}, []string{"## Turn 40\n"}, []string{"## Turns\n"}},
		{BriefOptions{All: true, MaxChars: len(floor)}, nil, []string{"## Turns\n", "## The whole session", "## Turn 40"}},
		{BriefOptions{All: true, MaxChars: 1}, nil, []string{"## Turns\n", "## The whole session", "## Turn 40"}},
		// A brief of one turn is of that turn, the latest or an earlier one: held to little,
		// it keeps the turn and leaves out where the session stood, saying so
		{BriefOptions{MaxChars: 1}, []string{"## Turn 40\n", "- … where the session stood at its end is left out"}, []string{"## Turns\n", "## State at the end"}},
		{BriefOptions{Turn: 3, MaxChars: 1}, []string{"## Turn 3\n", "- … where the session stood at its end is left out"}, []string{"## Turns\n", "## State at the end", "answer 40"}},
	} {
		text, err := Brief(s, turns, tc.o)
		if err != nil {
			t.Fatal(err)
		}
		if tc.o.MaxChars >= len(floor) && len(text) > tc.o.MaxChars {
			t.Errorf("held to %d, a brief is %d:\n%s", tc.o.MaxChars, len(text), text)
		}
		if tc.o.All && !strings.Contains(text, state) || !strings.HasSuffix(text, "`--since <when>` everything after a moment.\n") {
			t.Errorf("held to %d, a brief does not end with where things stand:\n%s", tc.o.MaxChars, text)
		}
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("held to %d: no %q in\n%s", tc.o.MaxChars, want, text)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(text, not) {
				t.Errorf("held to %d: %q in\n%s", tc.o.MaxChars, not, text)
			}
		}
	}
}

// A usage report is tokens: what went in, what came out, how much of that was reasoning,
// and how much of the prompt the cache served. It carries no cost — a sum of dollars over
// sessions most of which recorded none is not the cost of the tokens beside it — while a
// session's own cost, where its CLI worked one out, stays with the session.
func TestUsageIsTokensAndASessionKeepsItsCost(t *testing.T) {
	rows := []index.UsageRow{
		{Key: "deepseek/deepseek-reasoner", Sessions: 2, Calls: 9, Usage: model.Usage{Input: 15254, Output: 319, CacheRead: 9000, CacheWrite: 1200, Reasoning: 316}},
		{Key: "claude-opus-5", Sessions: 1, Calls: 3, Usage: model.Usage{Input: 100, Output: 40, CacheRead: 900}},
	}
	total := index.UsageRow{Key: "total", Sessions: 3, Calls: 12, Usage: model.Usage{Input: 15354, Output: 359, CacheRead: 9900, CacheWrite: 1200, Reasoning: 316}}
	var out bytes.Buffer
	Usage(&out, "By model", rows, &total, Style{})
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 || strings.Join(strings.Fields(lines[1]), " ") != "calls in out reasoning total cache" {
		t.Fatalf("usage table:\n%s", out.String())
	}
	// in is the whole prompt, cached or not; the cache column is the share of it that was read from cache
	if got := strings.Join(strings.Fields(lines[2]), " "); got != "deepseek/deepseek-reasoner 9 25.5k 319 316 25.8k 35%" {
		t.Errorf("a row that recorded reasoning and cache writes = %q", got)
	}
	if got := strings.Join(strings.Fields(lines[3]), " "); got != "claude-opus-5 3 1.0k 40 — 1.0k 90%" {
		t.Errorf("a row whose provider said nothing of reasoning = %q", got)
	}
	if strings.Contains(out.String(), "$") || strings.Contains(strings.ToLower(out.String()), "cost") {
		t.Errorf("a usage table shows money:\n%s", out.String())
	}
	for name, v := range map[string]any{"usage row": rows[0], "digest": index.Digest{}} {
		raw, _ := json.Marshal(v)
		if strings.Contains(strings.ToLower(string(raw)), "cost") {
			t.Errorf("a %s carries a cost: %s", name, raw)
		}
	}
	raw, _ := json.Marshal(rows[0])
	for _, key := range []string{`"cacheWrite":1200`, `"reasoning":316`, `"cacheHit"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("a usage row has no %s: %s", key, raw)
		}
	}

	// The session's own figure is the session's
	s, turns := hostile()
	for usd, want := range map[float64]string{1.836: "$1.84", 0.0000551: "<$0.01"} {
		s.Stats.CostUSD = usd
		var outline bytes.Buffer
		Outline(&outline, s, turns, nil, Style{})
		if !strings.Contains(outline.String(), " · "+want+" · ") {
			t.Errorf("a session that cost $%v reads:\n%s", usd, strings.SplitN(outline.String(), "\n", 4)[2])
		}
	}
	s.Stats.CostUSD = 0
	var outline bytes.Buffer
	Outline(&outline, s, turns, nil, Style{})
	if strings.Contains(outline.String(), "$") {
		t.Errorf("a session with no cost recorded shows one:\n%s", outline.String())
	}
}

// A search the filters, or archived sessions being left out, kept from matching says what
// would show the turns that match: the flags to drop and to add, in the command line's
// words or an agent's.
func TestASearchSaysWhatWouldShowWhatItLeftOut(t *testing.T) {
	for _, c := range []struct {
		outside *index.Outside
		style   Style
		want    string
	}{
		{&index.Outside{Turns: 1, Sessions: 1, Archived: true}, Style{},
			"No turn outside archived sessions holds “retry”.\n1 turn in 1 archived session matches: add --archived to see it.\n"},
		{&index.Outside{Turns: 3, Sessions: 2, Filters: []string{"project", "since"}, Archived: true}, Style{Args: true},
			"No turn the filters select holds “retry”.\n3 turns in 2 archived sessions outside them match: drop project, since and add archived to see them.\n"},
		{&index.Outside{Filters: []string{"since"}}, Style{},
			"No turn the filters select holds “retry”.\nNor does any outside them.\n"},
	} {
		var b bytes.Buffer
		Hits(&b, index.SearchResult{Hits: []index.Hit{}, Missing: []string{"retry"}, Outside: c.outside}, 0, c.style)
		if b.String() != c.want {
			t.Errorf("outside %+v:\n%s\nwant\n%s", c.outside, b.String(), c.want)
		}
	}
}

// A failure the same command passed later is a step of the work: the list, the outline and a
// search's hit say it, muted, and keep the red ✗ for a failure that is still open; a pack, an
// export and a brief of a stretch count it the same way.
func TestAFailureThatPassedLaterIsNotAnOpenOne(t *testing.T) {
	s, turns := hostile()
	colour := Style{Color: true}
	red, dim := "\x1b[31m", "\x1b[2m"
	for _, tc := range []struct {
		failed, passed   int
		words, mark, end string
		open             bool
	}{
		{1, 0, "1 failed", "✗1", "✗1 failed", true},
		{2, 1, "2 failed, 1 passed later", "✗2", "✗2 failed, 1 passed later", true},
		{1, 1, "1 failed, passed later", "✓", "✓ 1 failed, passed later", false},
		{3, 3, "3 failed, all passed later", "✓", "✓ 3 failed, all passed later", false},
	} {
		s.Stats.Failures, s.Stats.PassedLater = tc.failed, tc.passed
		turns[0].Failures, turns[0].PassedLater = tc.failed, tc.passed
		var list, outline, hits, painted bytes.Buffer
		Sessions(&list, []index.Session{*s}, 1, Style{})
		Outline(&outline, s, turns, nil, Style{})
		Hits(&hits, index.SearchResult{Hits: []index.Hit{{Session: *s, Turn: 1, Ref: s.Ref + "#1", Failed: tc.failed, PassedLater: tc.passed}}, Total: 1, Sessions: 1}, 0, Style{})
		Sessions(&painted, []index.Session{*s}, 1, colour)
		if !strings.Contains(list.String(), " · "+tc.words+" · ") {
			t.Errorf("%d failed, %d passed later: the list says\n%s", tc.failed, tc.passed, list.String())
		}
		if !strings.Contains(outline.String(), "  "+tc.mark+"  ") || !strings.Contains(outline.String(), tc.words) {
			t.Errorf("%d failed, %d passed later: the outline says\n%s", tc.failed, tc.passed, outline.String())
		}
		if !strings.Contains(hits.String(), "  "+tc.end+"\n") {
			t.Errorf("%d failed, %d passed later: the hit says\n%s", tc.failed, tc.passed, hits.String())
		}
		if got := strings.Contains(painted.String(), red+strconv.Itoa(tc.failed)+" failed"); got != tc.open {
			t.Errorf("%d failed, %d passed later: painted red %v, want %v:\n%q", tc.failed, tc.passed, got, tc.open, painted.String())
		}
		if !tc.open && !strings.Contains(painted.String(), dim+tc.words) {
			t.Errorf("%d failed, %d passed later: not muted:\n%q", tc.failed, tc.passed, painted.String())
		}
		if pack := Pack([]PackEntry{{Session: *s}}, 1, false); !strings.Contains(pack, " · "+tc.words+"\n") {
			t.Errorf("%d failed, %d passed later: the pack says\n%s", tc.failed, tc.passed, pack)
		}
		if md, err := Export(s, turns, FormatMarkdown); err != nil || !strings.Contains(md, " · "+tc.words+" · ") {
			t.Errorf("%d failed, %d passed later: the export says %v\n%s", tc.failed, tc.passed, err, md)
		}
		want := "- Tools: 1 calls · failed " + strconv.Itoa(tc.failed) + strings.TrimPrefix(tc.words, strconv.Itoa(tc.failed)+" failed") + "\n"
		if brief, err := Brief(s, turns, BriefOptions{All: true}); err != nil || !strings.Contains(brief, want) {
			t.Errorf("%d failed, %d passed later: no %q in the brief of the whole session %v\n%s", tc.failed, tc.passed, want, err, brief)
		}
	}
}

// Where a session's latest turn stands is said in its line of a list and in its outline:
// the call it waits on by what it runs, cleaned like any other text from a transcript.
func TestWhereALatestTurnStandsIsSaid(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	now := at.Add(3 * time.Minute)
	st := Style{Now: now}
	session := func(id string, live *model.Live) index.Session {
		return index.Session{Session: model.Session{Source: "claude", ID: id, Title: id, StartedAt: at, UpdatedAt: at, Live: live},
			Ref: "claude/" + id}
	}
	list := []index.Session{
		session("aaaa1111", &model.Live{State: model.LiveWorking, Since: at}),
		session("bbbb2222", &model.Live{State: model.LiveWaiting, Since: at, Tool: "Bash", Command: "make \x1b[31mdeploy\x1b[0m\nstaging", Seq: 7}),
		session("cccc3333", &model.Live{State: model.LiveWaiting, Since: at, Tool: "Edit", File: "/work/a.go", Seq: 3}),
		session("dddd4444", &model.Live{State: model.LiveIdle, Since: at}),
		session("eeee5555", nil),
	}
	var b bytes.Buffer
	Sessions(&b, list, len(list), st)
	out := b.String()
	for _, want := range []string{"aaaa1111  working · ", "bbbb2222  waiting on Bash: make deploy staging · ", "cccc3333  waiting on Edit: /work/a.go · ",
		"dddd4444  idle · ", "eeee5555  3m ago"} {
		if !strings.Contains(out, want) {
			t.Errorf("the list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("an escape sequence reached the list: %q", out)
	}

	b.Reset()
	Outline(&b, &list[1], nil, nil, st)
	if want := "waiting on Bash: make deploy staging [7] since " + at.Local().Format("15:04") + ", 3m ago — a command still running, or a prompt asking permission to run it"; !strings.Contains(b.String(), want) {
		t.Errorf("the outline lacks %q:\n%s", want, b.String())
	}
	b.Reset()
	Outline(&b, &list[3], nil, nil, st)
	if !strings.Contains(b.String(), "idle since "+at.Local().Format("15:04")+", 3m ago — the agent's turn is over") {
		t.Errorf("the outline of an idle session:\n%s", b.String())
	}
}

// Why prints the line, the commit that last changed it, and the turns: those that made it,
// then those that name it. A line no turn made says so, and one not committed yet says
// that; each points at files for the turns that changed the file. What the file and git
// say is printed as a line of text, whatever escape it holds.
func TestWhy(t *testing.T) {
	at := time.Date(2026, 11, 5, 10, 8, 0, 0, time.UTC)
	hit := func(ref, ask string) index.Hit {
		return index.Hit{Ref: ref, At: at, Ask: ask, Answer: "Committed.", Session: index.Session{Session: model.Session{Cwd: "/work/notes"}}}
	}
	b := Blamed{File: "notes.go", Path: "/work/notes/notes.go", Line: 6, Text: "\treturn strings.TrimSpace(s)\x1b]52;c;aGk=\x07",
		Commit: "3f9c2ab4e5f60718293a4b5c6d7e8f9012345678", Subject: "Trim the title", At: at}
	var out bytes.Buffer
	Why(&out, b, []index.Hit{hit("claude/aaaa#1", "trim the titles")}, []index.Hit{hit("codex/bbbb#2", "what was the last commit?")}, Style{})
	text := out.String()
	for _, want := range []string{"notes.go:6\n", "return strings.TrimSpace(s)", "3f9c2ab4e5f6 · ", "Trim the title",
		"Made in 1 turn:", "1. claude/aaaa#1", "Also named in 1 turn:", "1. codex/bbbb#2"} {
		if !strings.Contains(text, want) {
			t.Errorf("why lacks %q:\n%s", want, text)
		}
	}
	if strings.ContainsRune(text, 0x1b) || strings.Index(text, "Made in") > strings.Index(text, "Also named") {
		t.Errorf("why:\n%q", text)
	}
	out.Reset()
	Why(&out, b, nil, nil, Style{})
	if text := out.String(); !strings.Contains(text, "No turn made 3f9c2ab4e5f6") || !strings.Contains(text, "asq files /work/notes/notes.go") || strings.Contains(text, "named in") {
		t.Errorf("made by none:\n%s", text)
	}
	out.Reset()
	Why(&out, Blamed{File: "notes.go", Path: "/work/notes/notes.go", Line: 1, Text: "package notes"}, nil, nil, Style{})
	if text := out.String(); !strings.Contains(text, "Not committed yet: asq files /work/notes/notes.go") {
		t.Errorf("not committed:\n%s", text)
	}
}

// By session, a row is the session as ls names it — the shortest ref that tells it from the
// others listed — with what it was beneath its figures: a ref alone says nothing a reader can
// recognise.
func TestUsageBySessionNamesItsSessions(t *testing.T) {
	rows := []index.UsageRow{
		{Key: "claude/8a80b919-fbeb-4925-968b-46642528af23", Title: "Cap webhook retries", Cwd: "/home/u/work/billing-api", Sessions: 1, Calls: 11, Usage: model.Usage{Input: 400, Output: 39, CacheRead: 4000}},
		{Key: "codex/rollout-2026-11-23T10-00-00-0e9a1c2b", Title: "", Cwd: "/home/u/work/infra", Sessions: 1, Calls: 2, Usage: model.Usage{Input: 50, Output: 10}},
	}
	var out bytes.Buffer
	Usage(&out, "By session", rows, &index.UsageRow{Key: "total", Calls: 13}, Style{Home: "/home/u"})
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("by session:\n%s", out.String())
	}
	if f := strings.Fields(lines[2]); f[0] != "claude/8a80b919" || f[1] != "11" {
		t.Errorf("a session's row = %q, want its short ref and its figures", lines[2])
	}
	if got := strings.TrimSpace(lines[3]); got != "Cap webhook retries · ~/work/billing-api" {
		t.Errorf("beneath it = %q, want its title and directory", got)
	}
	if f := strings.Fields(lines[4]); f[0] != "codex/rollout-2026-11-" {
		t.Errorf("a ref past its store's word = %q", lines[4])
	}
	if got := strings.TrimSpace(lines[5]); got != "~/work/infra" {
		t.Errorf("a session with no title = %q, want its directory", got)
	}
	// One with neither is still named as ls names it, and the rest with it; nothing beneath
	out.Reset()
	Usage(&out, "By session", append(rows, index.UsageRow{Key: "gemini/5d1e0c77-2f00-4b1a-9c3e-000000000003", Calls: 1}), nil, Style{Home: "/home/u"})
	lines = strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 7 || strings.Fields(lines[2])[0] != "claude/8a80b919" || strings.Fields(lines[6])[0] != "gemini/5d1e0c77" {
		t.Errorf("by session, one unnamed:\n%s", out.String())
	}
	// Any other grouping is one line a row, its key whole — a model's too, slash and all
	out.Reset()
	Usage(&out, "By project", []index.UsageRow{{Key: "/home/u/work/infra", Calls: 2}}, nil, Style{Home: "/home/u"})
	if lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n"); len(lines) != 3 || strings.Fields(lines[2])[0] != "~/work/infra" {
		t.Errorf("by project:\n%s", out.String())
	}
	out.Reset()
	Usage(&out, "By model", []index.UsageRow{{Key: "openrouter/anthropic-claude-opus-5-20261101", Calls: 2}}, nil, Style{})
	if lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n"); len(lines) != 3 || strings.Fields(lines[2])[0] != "openrouter/anthropic-claude-opus-5-20261101" {
		t.Errorf("by model:\n%s", out.String())
	}
}

// A digest names the sessions behind each project's counts, the latest first, each with how
// it ended — the failures the same command passed later told from those it did not — and the
// last thing its agent said; and it says which moments it covers, on this machine's clock.
func TestDigestNamesItsSessions(t *testing.T) {
	since := time.Date(2026, 11, 23, 0, 0, 0, 0, time.Local)
	rep := index.DigestReport{
		Since: since,
		Total: index.Digest{Sessions: 14, Turns: 20, Tools: 9, Failures: 3, PassedLater: 2},
		Projects: []index.Digest{{
			Cwd: "/home/u/work/api", Sessions: 14, Turns: 20, Tools: 9, Failures: 3, PassedLater: 2,
			List: []index.DigestSession{
				{Ref: "claude/8a80b919-fbeb-4925-968b-46642528af23", Title: "Cap webhook retries", Turns: 2, Tools: 8, Failures: 2, PassedLater: 1,
					Answer: "Each retry now waits\ntwice as long.", LastAt: since.Add(10 * time.Hour)},
				{Ref: "codex/0e9a1c2b-0000-4000-8000-000000000001", Turns: 1, Tools: 1, Failures: 1, PassedLater: 1, Interrupted: true, LastAt: since.Add(9 * time.Hour)},
			},
		}},
	}
	var out bytes.Buffer
	Digest(&out, rep, Style{Home: "/home/u"})
	text := out.String()
	for _, want := range []string{
		"Since " + since.Format("Mon 2006-01-02 15:04 -07:00") + "\n",
		"Total  14 sessions · 20 turns · 9 tool calls · 3 failed, 2 passed later\n",
		"\n~/work/api\n",
		"  claude/8a80b919  11-23 10:00 · 2 turns · 8 tool calls  ✗2 failed, 1 passed later\n    Cap webhook retries\n    → Each retry now waits twice as long.\n",
		"  codex/0e9a1c2b  11-23 09:00 · 1 turn · 1 tool call  ✓ 1 failed, passed later · ⚠ interrupted\n    (untitled)\n",
		"  … 12 more sessions: asq ls --project /home/u/work/api --since 2026-11-23T00:00\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in the digest:\n%s", want, text)
		}
	}
	// An agent is told the tool and its arguments; failures none of which passed later are
	// counted alone
	rep.Total.PassedLater, rep.Projects[0].PassedLater, rep.Projects[0].List[0].PassedLater = 0, 0, 0
	rep.Projects[0].Cwd = "/home/u/my work"
	out.Reset()
	Digest(&out, rep, Style{Args: true})
	text = out.String()
	for _, want := range []string{
		"Total  14 sessions · 20 turns · 9 tool calls · 3 failed\n",
		"  claude/8a80b919  11-23 10:00 · 2 turns · 8 tool calls  ✗2 failed\n",
		`  … 12 more sessions: sessions(project="/home/u/my work", since="2026-11-23T00:00")` + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in the agent's digest:\n%s", want, text)
		}
	}
	out.Reset()
	Digest(&out, rep, Style{})
	if !strings.Contains(out.String(), `asq ls --project "/home/u/my work" --since`) {
		t.Errorf("a directory with a space, for a shell:\n%s", out.String())
	}
}

// Held to a budget, a digest names fewer sessions of each project, the same number for
// each, before it gives up any project's counts: cut at the budget, the sessions of the
// first project would leave out the last project whole.
func TestDigestWithinABudgetKeepsEveryProject(t *testing.T) {
	since := time.Date(2026, 11, 23, 0, 0, 0, 0, time.Local)
	rep := index.DigestReport{Since: since, Total: index.Digest{Sessions: 15, Turns: 15}}
	for p := range 3 {
		d := index.Digest{Cwd: fmt.Sprintf("/w/project-%d", p), Sessions: 5, Turns: 5}
		for i := range 5 {
			d.List = append(d.List, index.DigestSession{Ref: fmt.Sprintf("claude/%d%d000000-aaaa", p, i), Title: fmt.Sprintf("Session %d of project %d", i, p),
				Turns: 1, Answer: strings.Repeat("an answer that takes room ", 6), LastAt: since.Add(time.Duration(10-i) * time.Hour)})
		}
		rep.Projects = append(rep.Projects, d)
	}
	var whole bytes.Buffer
	Digest(&whole, rep, Style{Args: true})
	listed := func(text string, p int) int { return strings.Count(text, fmt.Sprintf("of project %d\n", p)) }

	var out bytes.Buffer
	DigestWithin(&out, rep, Style{Args: true}, whole.Len())
	if out.String() != whole.String() {
		t.Errorf("a digest that fits is not the digest:\n%s", out.String())
	}
	for budget, want := range map[int]int{whole.Len() * 2 / 3: -1, 0: 0} {
		out.Reset()
		DigestWithin(&out, rep, Style{Args: true}, budget)
		text := out.String()
		if budget > 0 && len(text) > budget {
			t.Errorf("a digest in %d bytes is %d:\n%s", budget, len(text), text)
		}
		first := listed(text, 0)
		if want >= 0 && first != want || want < 0 && (first == 0 || first == 5) {
			t.Errorf("in %d bytes, project 0 names %d sessions:\n%s", budget, first, text)
		}
		rest := "  … " + plural(5-first, "more session") + ": "
		if first == 0 {
			rest = "  … 5 sessions: "
		}
		for p := range 3 {
			if !strings.Contains(text, fmt.Sprintf("/w/project-%d\n  5 sessions", p)) || listed(text, p) != first ||
				!strings.Contains(text, rest+fmt.Sprintf(`sessions(project="/w/project-%d"`, p)) {
				t.Errorf("in %d bytes, project %d is not as the others, %d sessions and %q:\n%s", budget, p, first, rest, text)
			}
		}
	}
}

// How long agents worked reads in hours however many there are — no one worked the day and
// a half that 38 hours would otherwise read as — and is a column of usage where the rows are
// groups of turns, which a model is not.
func TestAgentTime(t *testing.T) {
	for ms, want := range map[int64]string{45_000: "45s", 12 * 60_000: "12m", 65 * 60_000: "1h05m", (38*60 + 20) * 60_000: "38h20m"} {
		if got := AgentTime(ms); got != want {
			t.Errorf("AgentTime(%d) = %q, want %q", ms, got, want)
		}
	}
	rows := []index.UsageRow{
		{Key: "2026-11-16", Calls: 9, Turns: 4, AgentMs: 65 * 60_000},
		{Key: "2026-11-23", Calls: 2},
	}
	var out bytes.Buffer
	Usage(&out, "By week", rows, &index.UsageRow{Key: "total", Calls: 11, Turns: 4, AgentMs: 65 * 60_000}, Style{})
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if f := strings.Fields(lines[1]); len(lines) != 5 || strings.Join(f[len(f)-2:], " ") != "agent time" {
		t.Fatalf("by week:\n%s", out.String())
	}
	for i, want := range map[int]string{2: "1h05m", 3: "—", 4: "1h05m"} {
		if f := strings.Fields(lines[i]); f[len(f)-1] != want {
			t.Errorf("row %d = %q, want it to end %q", i, lines[i], want)
		}
	}
	out.Reset()
	Usage(&out, "By model", []index.UsageRow{{Key: "claude-opus-5", Calls: 3}}, nil, Style{})
	if strings.Contains(out.String(), "agent time") {
		t.Errorf("a model's row has a time:\n%s", out.String())
	}
}

// When agents worked is a row a weekday, Monday first, a mark an hour as high as the time in
// it against the busiest, and the day's time at the end of its row.
func TestWhenAgentsWorked(t *testing.T) {
	var grid [7][24]int64
	grid[0][10] = 60 * 60_000 // Monday 10:00
	grid[6][23] = 15 * 60_000 // Sunday 23:00
	var out bytes.Buffer
	When(&out, grid, Style{})
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 10 || !strings.Contains(lines[1], "█ is 1h00m in an hour") {
		t.Fatalf("when:\n%s", out.String())
	}
	if want := "  Mon  " + strings.Repeat("·", 10) + "█" + strings.Repeat("·", 13) + "  1h00m"; lines[3] != want {
		t.Errorf("Monday = %q, want %q", lines[3], want)
	}
	if want := "  Tue  " + strings.Repeat("·", 24); lines[4] != want {
		t.Errorf("Tuesday = %q, want %q", lines[4], want)
	}
	if want := "  Sun  " + strings.Repeat("·", 23) + "▃  15m"; lines[9] != want {
		t.Errorf("Sunday = %q, want %q", lines[9], want)
	}
	out.Reset()
	When(&out, [7][24]int64{}, Style{})
	if out.Len() != 0 {
		t.Errorf("no time recorded draws %q", out.String())
	}
	// The digest says it, with the time and days of each project
	out.Reset()
	Digest(&out, index.DigestReport{When: grid, Total: index.Digest{Sessions: 1, Turns: 2, AgentMs: 75 * 60_000, Days: 2},
		Projects: []index.Digest{{Cwd: "/w", Sessions: 1, Turns: 2, AgentMs: 75 * 60_000, Days: 2}}}, Style{})
	if !strings.Contains(out.String(), "Total  1 session · 2 turns · 0 tool calls · 1h15m of agent time on 2 days\n\nWhen agents worked") {
		t.Errorf("digest:\n%s", out.String())
	}
}

// Held to a budget, an outline keeps its latest turns and a run of turns leaves out whole
// turns, each saying how to ask for what it left out — in arguments to an agent's tool, in
// flags to a terminal.
func TestFitting(t *testing.T) {
	s, turns := long(40)
	var whole bytes.Buffer
	FitOutline(&whole, s, turns, nil, 0, Style{})
	for _, tc := range []struct {
		st              Style
		hint, more, one string
		steps           string
	}{
		{Style{}, "--turns 1-", "[Turns #2–#5 left out: --turns 2-5 shows them.]", "[Turn #2 left out: --turns 2 shows it.]", "ask for fewer --turns, or one step with its --seq"},
		{Style{Args: true}, `read with turns="1-`, `[Turns #2–#5 left out: read them with turns="2-5".]`, `[Turn #2 left out: read it with turns="2".]`, "ask for fewer turns, or one step with its seq"},
	} {
		var b bytes.Buffer
		FitOutline(&b, s, turns, nil, 12000, tc.st)
		text := b.String()
		if len(text) > 12000 || !strings.Contains(text, "#40 ") || strings.Contains(text, "#1 ") || !strings.Contains(text, "[Turns before #") || !strings.Contains(text, tc.hint) {
			t.Errorf("an outline held to 12000 characters (%d whole) is %d:\n%s", whole.Len(), len(text), text)
		}
		b.Reset()
		FitTurns(&b, s, turns[:5], false, 1, tc.st)
		if text := b.String(); !strings.Contains(text, s.Ref+"#1 ·") || strings.Contains(text, s.Ref+"#2 ·") || !strings.Contains(text, tc.more) {
			t.Errorf("five turns held to less than one:\n%s", text)
		}
		// One turn left out is named as one, not as a run of one
		b.Reset()
		FitTurns(&b, s, turns[:2], false, 1, tc.st)
		if text := b.String(); !strings.Contains(text, tc.one) || strings.Contains(text, "#2–#2") {
			t.Errorf("two turns held to less than one:\n%s", text)
		}
		// A turn cut short says how to ask for less in the reader's own terms
		b.Reset()
		hs, steps := hostile()
		FitTurns(&b, hs, steps[:1], true, 80, tc.st)
		if text := b.String(); !strings.Contains(text, tc.steps) {
			t.Errorf("a turn held to less than itself does not say %q:\n%s", tc.steps, text)
		}
		b.Reset()
		FitTurns(&b, s, turns[:5], false, 0, tc.st)
		if text := b.String(); !strings.Contains(text, s.Ref+"#5 ·") || strings.Contains(text, "left out") {
			t.Errorf("five turns with no budget:\n%s", text)
		}
	}
	var b bytes.Buffer
	FitOutline(&b, s, turns, nil, 0, Style{})
	if b.String() != whole.String() || strings.Contains(whole.String(), "left out") {
		t.Error("an outline with no budget is the outline")
	}
	if got := Cut("one\ntwo\nthree\n", 0, "x"); got != "one\ntwo\nthree\n" {
		t.Errorf("no budget cuts nothing: %q", got)
	}
	if got := Cut(strings.Repeat("a line of text\n", 100), 300, "Ask for less."); len(got) > 400 || !strings.HasSuffix(got, "more characters. Ask for less.]") {
		t.Errorf("cut = %q", got)
	}
}
