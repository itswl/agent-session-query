// Package render turns what the index answers into text a person or an agent reads: the
// handoff brief, the pack, a session exported whole, and the tables the command line and
// the MCP tools print.
package render

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/transcript"
	"github.com/itswl/agent-session-query/internal/txt"
)

// Briefs and packs describe; they never conclude.
//
// A session is not a document with a beginning and a conclusion: it is a working
// notebook — topics come and go, the person doubles back, the CLI closes mid-thought.
// Summarising one is lossy and quietly wrong, so what is extracted is the turn: bounded
// and factual — the ask in the person's own words, the files the tools changed, how the
// exchange ended. Everything here is deterministic extraction from the record.

// How much a brief says at most. A budget (BriefOptions.MaxChars) makes it say less.
const (
	briefMaxTurns    = 12 // turns spelled out in full, the latest kept
	briefMaxFailures = 4  // failed calls one turn names
	briefMaxFiles    = 20 // files one list names
	briefOutputLines = 4  // lines of a failed call's output quoted: its end, where a command says why
)

// BriefOptions select what a brief covers, and carry what is known of it now.
type BriefOptions struct {
	// Turn spells out one turn; zero means the latest.
	Turn int
	// From spells out a turn and every turn after it, Since every turn that ran after a
	// moment, All the whole session. Each opens with what the stretch adds up to: the
	// brief for handing a session over a second time, or whole.
	From  int
	Since time.Time
	All   bool
	// Failures are the session's failed tool calls, those of the turns the brief names
	// with where the same command later passed (index.Failure.Later). A brief names the
	// ones in the turns it spells out: "one call failed" hands over a question, the
	// command hands over the answer — and one that passed later is no open question.
	Failures []index.Failure
	// Files are the changed files the brief names (BriefPlan.Files), each with how it
	// stands now (index.FileTouch.Now, Waiting).
	Files []index.FileTouch
	// MaxChars, above zero, holds the brief to about that many characters: it spells out
	// fewer turns, then says less of each, then leaves out a part at a time, until it fits
	// or where things stand at the end is all that is left.
	MaxChars int
}

// BriefPlan is what a brief covers, worked out before it is written: what is known of it
// only now — where a failed command later passed, how a changed file stands — is looked
// up for what the brief names, and nothing else.
type BriefPlan struct {
	// Covered are the turns the brief covers, Detailed the latest of them, which it
	// spells out, and Listed the turns its list of turns shows.
	Covered, Detailed, Listed []int
	// Files are the changed files it names, each against the turn whose account of the
	// file it gives: the turn it spells out, or the last of the stretch that changed it.
	Files []index.FileTouch

	s       *index.Session
	real    []model.Turn
	byIndex map[int]*model.Turn
	stretch string // the heading of a brief over a stretch of turns; "" for one turn
	// The stretch's files: those it changed, the latest first, each with the last turn
	// that changed it; and those it only read, by name
	changed []fileAt
	read    []string
}

type fileAt struct {
	path string
	turn int
}

// Names reports whether the brief says anything of a turn: lists it, or covers it.
func (p *BriefPlan) Names(turn int) bool {
	return slices.Contains(p.Listed, turn) || slices.Contains(p.Detailed, turn)
}

// PlanBrief works out what a brief of a session covers. turns is the session's outline.
func PlanBrief(s *index.Session, turns []model.Turn, o BriefOptions) (*BriefPlan, error) {
	p := &BriefPlan{s: s, byIndex: map[int]*model.Turn{}}
	for _, t := range turns {
		if t.Index > 0 {
			p.real = append(p.real, t)
		}
	}
	for i := range p.real {
		p.byIndex[p.real[i].Index] = &p.real[i]
	}
	real := p.real
	switch {
	case o.All:
		p.stretch = "The whole session"
		for _, t := range real {
			p.Covered = append(p.Covered, t.Index)
		}
	case !o.Since.IsZero():
		// A turn still running at that moment counts: it is work the receiver has not
		// seen either
		for _, t := range real {
			if end := lastOf(t); end.IsZero() || !end.Before(o.Since) {
				p.Covered = append(p.Covered, t.Index)
			}
		}
		if len(p.Covered) == 0 {
			return nil, fmt.Errorf("no turn of this session ran since %s", o.Since.Local().Format("2006-01-02 15:04"))
		}
		p.stretch = "Since " + o.Since.Local().Format("2006-01-02 15:04")
	case o.From != 0:
		if p.byIndex[o.From] == nil {
			return nil, fmt.Errorf("from must be 1..%d, got %d", len(real), o.From)
		}
		for _, t := range real {
			if t.Index >= o.From {
				p.Covered = append(p.Covered, t.Index)
			}
		}
		p.stretch = "From turn " + strconv.Itoa(o.From)
	case o.Turn != 0:
		if p.byIndex[o.Turn] == nil {
			return nil, fmt.Errorf("turn must be 1..%d, got %d", len(real), o.Turn)
		}
		p.Covered = []int{o.Turn}
	case len(real) > 0:
		p.Covered = []int{real[len(real)-1].Index}
	}
	if len(p.Covered) == 0 {
		p.stretch = "" // a session with no turn yet: there is no stretch to add up
	}
	p.Detailed = p.Covered[max(0, len(p.Covered)-briefMaxTurns):]
	p.Listed = p.listed(full)

	// The files: each turn spelled out against what it changed, and the stretch against
	// the last turn of it that changed each — which is the account of the file that
	// matters now
	seen := map[fileAt]bool{}
	touch := func(path string, turn int) {
		if k := (fileAt{path, turn}); !seen[k] {
			seen[k] = true
			p.Files = append(p.Files, touchOf(s, p.byIndex[turn], path))
		}
	}
	if p.stretch != "" {
		last, read := map[string]int{}, map[string]bool{}
		for _, i := range p.Covered {
			for _, f := range p.byIndex[i].Files {
				if f.Changed {
					last[f.Path] = i
				} else {
					read[f.Path] = true
				}
			}
		}
		for path, turn := range last {
			p.changed = append(p.changed, fileAt{path, turn})
		}
		sort.Slice(p.changed, func(i, j int) bool {
			a, b := p.changed[i], p.changed[j]
			return a.turn > b.turn || a.turn == b.turn && a.path < b.path
		})
		for path := range read {
			if _, changed := last[path]; !changed {
				p.read = append(p.read, path)
			}
		}
		sort.Strings(p.read)
		for _, f := range p.changed[:min(len(p.changed), briefMaxFiles)] {
			touch(f.path, f.turn)
		}
	}
	for _, i := range p.Detailed {
		changed := changedOf(p.byIndex[i])
		for _, path := range changed[:min(len(changed), briefMaxFiles)] {
			touch(path, i)
		}
	}
	return p, nil
}

// touchOf is a turn's change to a file as the files report has it: what a look at the
// file now is measured against.
func touchOf(s *index.Session, t *model.Turn, path string) index.FileTouch {
	at := t.StartedAt
	if at.IsZero() {
		at = t.LastAt
	}
	ended := at
	for _, x := range []time.Time{t.EndedAt, t.LastAt} {
		if x.After(ended) {
			ended = x
		}
	}
	return index.FileTouch{Path: path, Changed: true, Session: s.Ref, Cwd: s.Cwd, Turn: t.Index, At: at, Ended: ended}
}

func changedOf(t *model.Turn) []string {
	var out []string
	for _, f := range t.Files {
		if f.Changed {
			out = append(out, f.Path)
		}
	}
	return out
}

// detail is how much a brief says. It says less, a step at a time, to fit a budget.
type detail struct {
	turns       int  // turns spelled out, the latest kept
	first, last int  // of a long session's list of turns, how many of the first and the latest it shows: none, no list
	files       int  // files one list names
	lines       int  // lines of a failed call's output quoted
	ask, answer int  // characters of a turn's ask and of its answer quoted
	sum         bool // a stretch opens with what it adds up to
	state       bool // it ends with where the session stood at its end
}

var (
	full  = detail{turns: briefMaxTurns, first: 5, last: 15, files: briefMaxFiles, lines: briefOutputLines, ask: 300, answer: 500, sum: true, state: true}
	least = detail{turns: 1, first: 2, last: 6, files: 6, lines: 1, ask: 300, answer: 500, sum: true, state: true}
	// Past least, a brief says less, a part at a time, to fit a budget: less of the ask and
	// the answer of the turn it spells out — the answer is where things stand at the end,
	// below — then nothing of the list of turns, of that turn, and of what the stretch adds
	// up to. Where things stand at the end, and how to dig deeper, are what it keeps.
	bare = []func(*detail){
		func(d *detail) { d.ask, d.answer = 150, 150 },
		func(d *detail) { d.first, d.last = 0, 0 },
		func(d *detail) { d.turns = 0 },
		func(d *detail) { d.sum = false },
	}
	// A brief of one turn is of that turn: past least it says less of it and leaves out
	// the list of turns, then where the session stood at its end — which the turn's own
	// end says, when it is the latest — and never the turn itself.
	bareTurn = []func(*detail){
		func(d *detail) { d.ask, d.answer = 150, 150 },
		func(d *detail) { d.first, d.last = 0, 0 },
		func(d *detail) { d.state = false },
	}
)

// listed is which turns the list of turns shows: all of them, or of a long session the
// first few and the latest.
func (p *BriefPlan) listed(d detail) []int {
	var out []int
	if len(p.real) < 2 {
		return out
	}
	for i, t := range p.real {
		if len(p.real) <= d.first+d.last || i < d.first || i >= len(p.real)-d.last {
			out = append(out, t.Index)
		}
	}
	return out
}

// Brief renders a session's handoff brief as Markdown. turns is the session's outline —
// its turns without their items — which is all a brief reads. The headings are the
// interface the receiving side prompts around, not decoration.
func Brief(s *index.Session, turns []model.Turn, o BriefOptions) (string, error) {
	p, err := PlanBrief(s, turns, o)
	if err != nil {
		return "", err
	}
	d := full
	d.turns = min(d.turns, max(len(p.Detailed), 1))
	text := p.write(o, d)
	// Fewer turns spelled out, the latest kept, until it fits; then less of each
	for o.MaxChars > 0 && len(text) > o.MaxChars && d.turns > 1 {
		d.turns = max(1, min(d.turns-1, d.turns*o.MaxChars/len(text)))
		text = p.write(o, d)
	}
	if o.MaxChars > 0 && len(text) > o.MaxChars {
		l := least
		l.turns = d.turns
		text = p.write(o, l)
		steps := bare
		if p.stretch == "" {
			steps = bareTurn
		}
		for _, less := range steps {
			if len(text) <= o.MaxChars {
				break
			}
			less(&l)
			text = p.write(o, l)
		}
	}
	return text, nil
}

// write writes the brief at one level of detail.
func (p *BriefPlan) write(o BriefOptions, d detail) string {
	s, real := p.s, p.real
	detailed := p.Detailed[max(0, len(p.Detailed)-d.turns):]
	failures := map[int][]index.Failure{}
	for _, f := range o.Failures {
		failures[f.Turn] = append(failures[f.Turn], f)
	}
	now := map[fileAt]*index.FileTouch{}
	for i := range o.Files {
		now[fileAt{o.Files[i].Path, o.Files[i].Turn}] = &o.Files[i]
	}

	var b strings.Builder
	title := txt.Clean(s.Title)
	if title == "" {
		title = s.ID
	}
	fmt.Fprintf(&b, "# Session brief: %s\n\n", title)
	fmt.Fprintf(&b, "- source %s · project %s\n", s.Source, orDash(s.Cwd))
	if s.Branch != "" {
		fmt.Fprintf(&b, "- branch: %s (when the session opened)\n", s.Branch)
	}
	fmt.Fprintf(&b, "- ref: %s\n", s.Ref)
	if s.Path != "" {
		fmt.Fprintf(&b, "- path: %s\n", s.Path)
	}
	// The cheapest handoff of all is to reopen the session in the CLI that wrote it
	if s.Resume != "" {
		fmt.Fprintf(&b, "- resume: %s\n", s.Resume)
	} else if s.ResumeThere != "" {
		fmt.Fprintf(&b, "- resume where %s came from: %s\n", s.Source, s.ResumeThere)
	}
	if len(real) > 0 {
		fmt.Fprintf(&b, "- %s · %s → %s\n", plural(len(real), "turn"), shortTime(real[0].StartedAt), shortTime(workEnd(real[len(real)-1])))
	}
	if s.Agents > 0 {
		fmt.Fprintf(&b, "- %s ran beneath it\n", plural(s.Agents, "subagent"))
	}

	if listed := p.listed(d); len(listed) > 0 {
		b.WriteString("\n## Turns\n\n")
		for i, n := range listed {
			if i > 0 && n != listed[i-1]+1 {
				fmt.Fprintf(&b, "- … %d turns not shown\n", len(real)-len(listed))
			}
			t := p.byIndex[n]
			mark, note := "✓", ""
			if t.Interrupted {
				mark = "⚠"
			}
			// A call that failed and then passed is a step of the work, not where it stands
			if t.Failures > 0 {
				passed := 0
				for _, f := range failures[n] {
					if f.Later != nil {
						passed++
					}
				}
				switch {
				case passed == t.Failures:
					note = fmt.Sprintf(" · %d failed, passed later", t.Failures)
				case passed > 0:
					mark, note = "✗", fmt.Sprintf(" · %d failed, %d passed later", t.Failures, passed)
				default:
					mark, note = "✗", fmt.Sprintf(" · %d failed", t.Failures)
				}
			}
			fmt.Fprintf(&b, "- #%d %s %s %s%s\n", t.Index, shortTime(t.StartedAt), mark, txt.OneLine(t.Ask, 100), note)
		}
	}

	// A stretch opens with what it adds up to: the receiver reads that first and the
	// turns for the detail
	if p.stretch != "" && d.sum {
		var sum model.Turn
		for _, i := range p.Covered {
			t := p.byIndex[i]
			sum.Tools += t.Tools
			sum.Failures += t.Failures
			sum.PassedLater += t.PassedLater
			sum.Usage.Add(t.Usage)
		}
		first, last := p.byIndex[p.Covered[0]], p.byIndex[p.Covered[len(p.Covered)-1]]
		fmt.Fprintf(&b, "\n## %s\n\n", p.stretch)
		fmt.Fprintf(&b, "- %s: #%d → #%d · %s → %s\n", plural(len(p.Covered), "turn"), first.Index, last.Index,
			shortTime(first.StartedAt), shortTime(workEnd(*last)))
		if len(p.changed) > 0 {
			paths := make([]string, len(p.changed))
			for i, f := range p.changed {
				paths[i] = f.path
			}
			shown := slices.Clone(p.changed[:min(len(p.changed), d.files)])
			sort.Slice(shown, func(i, j int) bool { return shown[i].path < shown[j].path })
			changedHead(&b, paths)
			for _, f := range shown {
				who := "turn " + strconv.Itoa(f.turn)
				fileLine(&b, fmt.Sprintf("%s (#%d)", inProject(s.Cwd, f.path), f.turn), now[f], who)
			}
			if more := len(p.changed) - len(shown); more > 0 {
				fmt.Fprintf(&b, "  - … and %d more, changed earlier\n", more)
			}
		}
		if len(p.read) > 0 {
			fmt.Fprintf(&b, "- Read: %s\n", joinFiles(s.Cwd, p.read, d.files))
		}
		fmt.Fprintf(&b, "- Tools: %d calls · failed %d%s\n", sum.Tools, sum.Failures, laterWords(sum.Failures, sum.PassedLater))
		if line := TokenLine(sum.Usage); line != "" {
			fmt.Fprintf(&b, "- Tokens: %s\n", line)
		}
		// Enough turns to follow the work, the latest kept: a stretch over a long absence,
		// or a whole session, would otherwise be the session again
		switch {
		case len(detailed) == len(p.Covered):
		case len(detailed) == 0:
			b.WriteString("- … no turn is detailed below, to keep to the budget\n")
		case len(detailed) == 1:
			b.WriteString("- … only the latest turn is detailed below\n")
		default:
			fmt.Fprintf(&b, "- … only the latest %d turns are detailed below\n", len(detailed))
		}
	}

	for _, i := range detailed {
		t := p.byIndex[i]
		head := "\n## Turn " + strconv.Itoa(t.Index)
		if t.Interrupted {
			head += " — interrupted"
		}
		b.WriteString(head + "\n\n")
		fmt.Fprintf(&b, "- Asked: %s\n", txt.Cap(t.Ask, d.ask))
		if changed := changedOf(t); len(changed) > 0 {
			changedHead(&b, changed)
			for _, path := range changed[:min(len(changed), d.files)] {
				fileLine(&b, inProject(s.Cwd, path), now[fileAt{path, i}], "this turn")
			}
			if more := len(changed) - d.files; more > 0 {
				fmt.Fprintf(&b, "  - … and %d more\n", more)
			}
		}
		var read []string
		for _, f := range t.Files {
			if !f.Changed {
				read = append(read, f.Path)
			}
		}
		if len(read) > 0 {
			fmt.Fprintf(&b, "- Read: %s\n", joinFiles(s.Cwd, read, d.files))
		}
		if t.Tools > 0 {
			fmt.Fprintf(&b, "- Tools: %s\n", ToolLine(t))
		}
		failureLines(&b, failures[i], d)
		if line := TokenLine(t.Usage); line != "" {
			fmt.Fprintf(&b, "- Tokens: %s\n", line)
		}
		ended := "(no reply — the turn was interrupted or is still running)"
		if t.Answer != "" {
			ended = txt.Cap(t.Answer, d.answer)
		}
		fmt.Fprintf(&b, "- Ended: %s\n", ended)
	}

	if d.state {
		b.WriteString("\n## State at the end\n\n")
		state := "(nothing concluded — the agent said nothing in this session)"
		for i := len(real) - 1; i >= 0; i-- {
			if real[i].Answer != "" {
				state = txt.Cap(real[i].Answer, 400)
				break
			}
		}
		b.WriteString(state + "\n")
	} else {
		b.WriteString("\n- … where the session stood at its end is left out, to keep to the budget\n")
	}
	if len(p.Detailed) > 0 {
		n := p.Detailed[len(p.Detailed)-1]
		fmt.Fprintf(&b, "\nDig deeper: `asq show %s#%d` reads a turn step by step (MCP: read with ref=\"%s#%d\"); `asq brief %s --since <when>` covers everything after a moment, `--since all` the whole session.\n",
			s.Ref, n, s.Ref, n, s.Ref)
	}
	return b.String()
}

// failureLines names a turn's failed calls: what was run and how it ended. One whose
// command later passed says where, and that is all it needs to say; one that did not
// quotes the end of what it printed, which is where a command says why. Those come first.
func failureLines(b *strings.Builder, failures []index.Failure, d detail) {
	var open, passed []index.Failure
	for _, f := range failures {
		if f.Later != nil {
			passed = append(passed, f)
		} else {
			open = append(open, f)
		}
	}
	all := append(open, passed...)
	for _, f := range all[:min(len(all), briefMaxFailures)] {
		what := f.Tool
		if f.Command != "" {
			what += " " + codeSpan(txt.OneLine(f.Command, 160))
		}
		how := f.Status
		if f.Exit != nil {
			how += ", exit " + strconv.Itoa(*f.Exit)
		}
		step := " [step " + strconv.Itoa(f.Seq) + "]"
		if l := f.Later; l != nil {
			line := "- Failed, passed later: " + what + " — " + how + step + " · passed " + laterBy(f.At, l.At) + " #" + strconv.Itoa(l.Turn) + " step " + strconv.Itoa(l.Seq)
			if l.Command != txt.OneLine(f.Command, 0) {
				line += ", as " + codeSpan(txt.OneLine(l.Command, 160))
			}
			b.WriteString(line + "\n")
			continue
		}
		b.WriteString("- Failed: " + what + " — " + how + step + "\n")
		quote(b, tail(f.Output, d.lines), "  ")
	}
	if more := len(all) - briefMaxFailures; more > 0 {
		line := fmt.Sprintf("- … and %d more failed calls", more)
		if n := len(passed) - max(briefMaxFailures-len(open), 0); n > 0 {
			line += fmt.Sprintf(", %d of them passed later", min(n, more))
		}
		b.WriteString(line + "\n")
	}
}

// tail is the last n lines of an output that say anything, cleaned, each cut to a line.
func tail(output string, n int) []string {
	lines := strings.Split(strings.TrimSpace(txt.Clean(output)), "\n")
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if line := strings.TrimRight(lines[i], " \t"); strings.TrimSpace(line) != "" {
			out = append(out, txt.Truncate(line, 200, " …"))
		}
	}
	slices.Reverse(out)
	return out
}

// quote writes lines as a code block, indented to sit beneath a list item, fenced with
// more backticks than any run of them inside.
func quote(b *strings.Builder, lines []string, indent string) {
	if len(lines) == 0 {
		return
	}
	text := strings.Join(lines, "\n")
	ticks := "```"
	for strings.Contains(text, ticks) {
		ticks += "`"
	}
	b.WriteString(indent + ticks + "\n")
	for _, line := range lines {
		b.WriteString(indent + line + "\n")
	}
	b.WriteString(indent + ticks + "\n")
}

// codeSpan writes text as inline code, with more backticks around it than any run of
// them inside: a command can hold one.
func codeSpan(text string) string {
	ticks := "`"
	for strings.Contains(text, ticks) {
		ticks += "`"
	}
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		return ticks + " " + text + " " + ticks
	}
	return ticks + text + ticks
}

// changedHead opens a list of changed files: how many, of which kinds.
func changedHead(b *strings.Builder, paths []string) {
	head := "- Changed: " + plural(len(paths), "file")
	if kinds := KindLine(paths); kinds != "" {
		head += " — " + kinds
	}
	b.WriteString(head + "\n")
}

// fileLine names a changed file and how it stands now against what the turn named who
// left; nothing more when that cannot be said.
func fileLine(b *strings.Builder, name string, t *index.FileTouch, who string) {
	line := "  - " + name
	if t != nil {
		if said := fileNowAgainst(t.Now, who); said != "" {
			line += " · " + said
		} else if t.Waiting != "" {
			line += " · " + waitingLine(t.Waiting)
		}
	}
	b.WriteString(line + "\n")
}

// joinFiles lists up to n files and says how many more there were.
func joinFiles(cwd string, files []string, n int) string {
	more := ""
	if len(files) > n {
		more = fmt.Sprintf(" … and %d more", len(files)-n)
		files = files[:n]
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = inProject(cwd, f)
	}
	return strings.Join(names, ", ") + more
}

// inProject names a file as the project does: relative to the session's directory, which
// the brief's head names, when it is beneath it.
func inProject(cwd, path string) string {
	for _, sep := range []string{"/", `\`} {
		dir := strings.TrimRight(cwd, sep)
		if dir == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(path, dir+sep); ok && rest != "" {
			return rest
		}
	}
	return path
}

// KindLine counts changed files as "4 code · 2 tests · 1 doc".
func KindLine(files []string) string {
	counts := map[string]int{}
	for _, f := range files {
		counts[transcript.FileKind(f)]++
	}
	var parts []string
	for _, kind := range []struct{ key, one, many string }{
		{transcript.FileCode, "code", "code"}, {transcript.FileTest, "test", "tests"},
		{transcript.FileDocs, "doc", "docs"}, {transcript.FileConfig, "config", "config"},
	} {
		if n := counts[kind.key]; n == 1 {
			parts = append(parts, "1 "+kind.one)
		} else if n > 1 {
			parts = append(parts, strconv.Itoa(n)+" "+kind.many)
		}
	}
	return strings.Join(parts, " · ")
}

// ToolLine is a turn's tool calls by what they were for: "exec 3 · read 4 · failed 1", and
// how many of those the same command passed later.
func ToolLine(t *model.Turn) string {
	kinds := make([]string, 0, len(t.ToolKinds))
	for kind := range t.ToolKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds)+1)
	for _, kind := range kinds {
		parts = append(parts, kind+" "+strconv.Itoa(t.ToolKinds[kind]))
	}
	if len(parts) == 0 {
		parts = append(parts, strconv.Itoa(t.Tools))
	}
	if t.Failures > 0 {
		parts = append(parts, "failed "+strconv.Itoa(t.Failures)+laterWords(t.Failures, t.PassedLater))
	}
	return strings.Join(parts, " · ")
}

// TokenLine is how usage reads in a line: "18.2k in · 900 out · cache 71%".
func TokenLine(u model.Usage) string {
	if u.IsZero() {
		return ""
	}
	var parts []string
	if p := u.Prompt(); p > 0 {
		parts = append(parts, txt.Tokens(p)+" in")
	}
	if u.Output > 0 {
		parts = append(parts, txt.Tokens(u.Output)+" out")
	}
	if u.CacheRead > 0 {
		parts = append(parts, "cache "+strconv.Itoa(int(100*u.CacheHit()+0.5))+"%")
	}
	return strings.Join(parts, " · ")
}

func lastOf(t model.Turn) time.Time {
	for _, at := range []time.Time{t.LastAt, t.EndedAt, t.StartedAt} {
		if !at.IsZero() {
			return at
		}
	}
	return time.Time{}
}

// workEnd is the last thing the agent did, falling back to the ask for a turn that never
// got one: the end a duration is measured to.
func workEnd(t model.Turn) time.Time {
	if !t.EndedAt.IsZero() {
		return t.EndedAt
	}
	return t.StartedAt
}

func shortTime(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.Local().Format("01-02 15:04")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// Pack renders several sessions as one document, oldest first: a pack answers "how did
// this get here", where a list answers "what am I doing". Each entry is what was asked
// and what it concluded, with the reference to read further.
type PackEntry struct {
	Session index.Session
	Turns   []model.Turn // the outline, when the pack spells the turns out
}

// packNotice says what a pack is and is not.
const packNotice = "> This is a record, not a summary. Each entry is what was asked and what that\n" +
	"> session concluded, extracted as written. Where two sessions disagree, the later one\n" +
	"> was said later — nothing here states what is currently true.\n"

// Pack renders the entries as Markdown. matching is how many sessions the selection held
// before it was cut to these.
func Pack(entries []PackEntry, matching int, withTurns bool) string {
	var b strings.Builder
	b.WriteString("# Session pack\n\n")
	sources := map[string]int{}
	var first, last time.Time
	for _, e := range entries {
		sources[e.Session.Source]++
		if at := e.Session.StartedAt; !at.IsZero() && (first.IsZero() || at.Before(first)) {
			first = at
		}
		if at := e.Session.UpdatedAt; at.After(last) {
			last = at
		}
	}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		names[i] = fmt.Sprintf("%s %d", name, sources[name])
	}
	fmt.Fprintf(&b, "- %s", plural(len(entries), "session"))
	if matching > len(entries) {
		fmt.Fprintf(&b, " — the most recent of %d matching", matching)
	}
	if len(names) > 0 {
		fmt.Fprintf(&b, " · %s", strings.Join(names, " · "))
	}
	if !first.IsZero() {
		fmt.Fprintf(&b, " · %s → %s", first.Local().Format("2006-01-02"), last.Local().Format("2006-01-02"))
	}
	b.WriteString("\n\n" + packNotice)

	for i, e := range entries {
		s := &e.Session
		title := txt.Clean(s.Title)
		if title == "" {
			title = s.ID
		}
		fmt.Fprintf(&b, "\n## %d. %s\n\n", i+1, title)
		fmt.Fprintf(&b, "- %s · %s · %s\n", s.UpdatedAt.Local().Format("2006-01-02 15:04"), s.Source, orDash(s.Cwd))
		fmt.Fprintf(&b, "- ref: %s · %s", s.Ref, plural(s.Stats.Turns, "turn"))
		if s.Stats.Failures > 0 {
			fmt.Fprintf(&b, " · %s", failedWords(s.Stats.Failures, s.Stats.PassedLater))
		}
		b.WriteString("\n")
		if withTurns && len(e.Turns) > 0 {
			for _, t := range e.Turns {
				if t.Index == 0 {
					continue
				}
				fmt.Fprintf(&b, "- #%d Asked: %s\n", t.Index, txt.OneLine(t.Ask, 240))
				if t.Answer != "" {
					fmt.Fprintf(&b, "  Concluded: %s\n", txt.OneLine(t.Answer, 320))
				}
			}
			continue
		}
		if s.LastAsk != "" {
			fmt.Fprintf(&b, "- Asked: %s\n", txt.OneLine(s.LastAsk, 300))
		}
		if s.Answer != "" {
			fmt.Fprintf(&b, "- Concluded: %s\n", txt.OneLine(s.Answer, 400))
		}
	}
	return b.String()
}
