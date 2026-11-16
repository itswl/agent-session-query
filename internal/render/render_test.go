package render

import (
	"bytes"
	"encoding/json"
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
				Output: "```go\nfmt.Println(\"a fence inside an output\")\n```\n\x1b[31mred\x1b[0m </code><script>alert('out')</script>\nkey sk-Zx9Qw7Lm3Pv5Tr8Kd2Nf6Hb4"}},
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

func TestBriefNamesWhatFailed(t *testing.T) {
	s, turns := hostile()
	exit := 1
	text, err := Brief(s, turns, BriefOptions{Failures: []index.Failure{
		{Turn: 1, Seq: 2, Tool: "Bash", Status: "error", Exit: &exit, Command: "cat README.md", Output: "first line\nNo such file or directory"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Turn 1", "- Asked: ", "- Failed: Bash `cat README.md` — error, exit 1: No such file or directory [step 2]", "## State at the end", "main.go"} {
		if !strings.Contains(text, want) {
			t.Errorf("brief: no %q in\n%s", want, text)
		}
	}
	if _, err := Brief(s, turns, BriefOptions{Turn: 5}); err == nil {
		t.Error("a turn the session does not have is accepted")
	}
	if _, err := Brief(s, turns, BriefOptions{Since: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}); err == nil {
		t.Error("a delta with nothing in it is an empty brief instead of an error")
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
