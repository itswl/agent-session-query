// Package render turns what the index answers into text a person or an agent reads: the
// handoff brief, the pack, a session exported whole, and the tables the command line and
// the MCP tools print.
package render

import (
	"fmt"
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

// briefMaxTurns caps how many turns a brief spells out in full.
const briefMaxTurns = 12

// BriefOptions select what a brief covers.
type BriefOptions struct {
	// Turn spells out one turn; zero means the latest.
	Turn int
	// Since spells out every turn that ran after a moment, and opens with what that
	// stretch adds up to: the brief for handing the same session over a second time.
	Since time.Time
	// Failures are the session's failed tool calls. A brief names the ones in the turns
	// it spells out: "one call failed" hands over a question, the command hands over
	// the answer.
	Failures []index.Failure
}

// briefMaxFailures caps how many failed calls a turn of a brief names.
const briefMaxFailures = 4

// Brief renders a session's handoff brief as Markdown. turns is the session's outline —
// its turns without their items — which is all a brief reads. The headings are the
// interface the receiving side prompts around, not decoration.
func Brief(s *index.Session, turns []model.Turn, o BriefOptions) (string, error) {
	var real []model.Turn
	for _, t := range turns {
		if t.Index > 0 {
			real = append(real, t)
		}
	}
	var selected []int
	switch {
	case !o.Since.IsZero():
		// A turn still running at that moment counts: it is work the receiver has not
		// seen either
		for _, t := range real {
			if end := lastOf(t); end.IsZero() || !end.Before(o.Since) {
				selected = append(selected, t.Index)
			}
		}
		if len(selected) == 0 {
			return "", fmt.Errorf("no turn of this session ran since %s", o.Since.Local().Format("2006-01-02 15:04"))
		}
	case o.Turn != 0:
		if o.Turn < 1 || o.Turn > len(real) {
			return "", fmt.Errorf("turn must be 1..%d, got %d", len(real), o.Turn)
		}
		selected = []int{o.Turn}
	case len(real) > 0:
		selected = []int{real[len(real)-1].Index}
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
	}
	if len(real) > 0 {
		fmt.Fprintf(&b, "- %s · %s → %s\n", plural(len(real), "turn"), shortTime(real[0].StartedAt), shortTime(workEnd(real[len(real)-1])))
	}
	if s.Agents > 0 {
		fmt.Fprintf(&b, "- %s ran beneath it\n", plural(s.Agents, "subagent"))
	}

	if len(real) > 1 {
		b.WriteString("\n## Turns\n\n")
		shown := real
		if len(shown) > 20 {
			shown = append(append([]model.Turn{}, real[:5]...), real[len(real)-15:]...)
		}
		for i, t := range shown {
			if i == 5 && len(shown) < len(real) {
				fmt.Fprintf(&b, "- … %d turns not shown\n", len(real)-len(shown))
			}
			mark, note := "✓", ""
			switch {
			case t.Failures > 0:
				mark, note = "✗", fmt.Sprintf(" · %d failed", t.Failures)
			case t.Interrupted:
				mark = "⚠"
			}
			fmt.Fprintf(&b, "- #%d %s %s %s%s\n", t.Index, shortTime(t.StartedAt), mark, txt.OneLine(t.Ask, 100), note)
		}
	}

	chosen := map[int]bool{}
	for _, i := range selected {
		chosen[i] = true
	}
	// A delta covers a stretch rather than one exchange, so it opens with what the
	// stretch adds up to: the receiver reads that first and the turns for the detail
	if !o.Since.IsZero() {
		var sum model.Turn
		files := map[string]bool{}
		for _, t := range real {
			if !chosen[t.Index] {
				continue
			}
			sum.Tools += t.Tools
			sum.Failures += t.Failures
			sum.Usage.Add(t.Usage)
			for _, f := range t.Files {
				files[f.Path] = files[f.Path] || f.Changed
			}
		}
		for path, changed := range files {
			sum.Files = append(sum.Files, model.File{Path: path, Changed: changed})
		}
		sort.Slice(sum.Files, func(i, j int) bool { return sum.Files[i].Path < sum.Files[j].Path })
		first, last := real[selected[0]-1], real[selected[len(selected)-1]-1]
		fmt.Fprintf(&b, "\n## Since %s\n\n", o.Since.Local().Format("2006-01-02 15:04"))
		fmt.Fprintf(&b, "- %s: #%d → #%d · %s → %s\n", plural(len(selected), "turn"), first.Index, last.Index,
			shortTime(first.StartedAt), shortTime(workEnd(last)))
		fileLines(&b, sum.Files)
		fmt.Fprintf(&b, "- Tools: %d calls · failed %d\n", sum.Tools, sum.Failures)
		if line := TokenLine(sum.Usage); line != "" {
			fmt.Fprintf(&b, "- Tokens: %s\n", line)
		}
		// Enough turns to follow the work, newest kept: a delta over a long absence would
		// otherwise be the whole session again
		if len(selected) > briefMaxTurns {
			selected = selected[len(selected)-briefMaxTurns:]
			chosen = map[int]bool{}
			for _, i := range selected {
				chosen[i] = true
			}
			fmt.Fprintf(&b, "- … only the latest %d turns are detailed below\n", briefMaxTurns)
		}
	}

	for _, t := range real {
		if !chosen[t.Index] {
			continue
		}
		head := "\n## Turn " + strconv.Itoa(t.Index)
		if t.Interrupted {
			head += " — interrupted"
		}
		b.WriteString(head + "\n\n")
		fmt.Fprintf(&b, "- Asked: %s\n", txt.Cap(t.Ask, 300))
		fileLines(&b, t.Files)
		if t.Tools > 0 {
			fmt.Fprintf(&b, "- Tools: %s\n", ToolLine(&t))
		}
		failureLines(&b, t.Index, o.Failures)
		if line := TokenLine(t.Usage); line != "" {
			fmt.Fprintf(&b, "- Tokens: %s\n", line)
		}
		ended := "(no reply — the turn was interrupted or is still running)"
		if t.Answer != "" {
			ended = txt.Cap(t.Answer, 500)
		}
		fmt.Fprintf(&b, "- Ended: %s\n", ended)
	}

	b.WriteString("\n## State at the end\n\n")
	state := "(nothing concluded — the agent said nothing in this session)"
	for i := len(real) - 1; i >= 0; i-- {
		if real[i].Answer != "" {
			state = txt.Cap(real[i].Answer, 400)
			break
		}
	}
	b.WriteString(state + "\n")
	if len(real) > 0 {
		fmt.Fprintf(&b, "\nDig deeper: `asq show %s#%d` reads a turn step by step (MCP: read with ref=\"%s#%d\"); `asq brief %s --since <when>` covers everything after a moment.\n",
			s.Ref, selected[len(selected)-1], s.Ref, selected[len(selected)-1], s.Ref)
	}
	return b.String(), nil
}

// failureLines names a turn's failed calls: what was run, how it ended, and the last
// thing it printed — which is where a command says why.
func failureLines(b *strings.Builder, turn int, failures []index.Failure) {
	n := 0
	for _, f := range failures {
		if f.Turn != turn {
			continue
		}
		if n++; n > briefMaxFailures {
			continue
		}
		what := f.Tool
		if f.Command != "" {
			what += " `" + txt.OneLine(f.Command, 160) + "`"
		}
		how := f.Status
		if f.Exit != nil {
			how += ", exit " + strconv.Itoa(*f.Exit)
		}
		line := "- Failed: " + what + " — " + how
		lines := strings.Split(strings.TrimSpace(f.Output), "\n")
		if last := txt.OneLine(lines[len(lines)-1], 200); last != "" {
			line += ": " + last
		}
		b.WriteString(line + " [step " + strconv.Itoa(f.Seq) + "]\n")
	}
	if n > briefMaxFailures {
		fmt.Fprintf(b, "- … and %d more failed calls\n", n-briefMaxFailures)
	}
}

// fileLines writes what a turn changed and what else it touched.
func fileLines(b *strings.Builder, files []model.File) {
	var changed, touched []string
	for _, f := range files {
		if f.Changed {
			changed = append(changed, f.Path)
		} else {
			touched = append(touched, f.Path)
		}
	}
	if len(changed) > 0 {
		fmt.Fprintf(b, "- Changed: %s\n", joinFiles(changed))
		fmt.Fprintf(b, "- Split: %s\n", KindLine(changed))
	}
	if len(touched) > 0 {
		fmt.Fprintf(b, "- Read: %s\n", joinFiles(touched))
	}
}

// joinFiles lists up to twenty files and says how many more there were.
func joinFiles(files []string) string {
	more := ""
	if len(files) > 20 {
		more = fmt.Sprintf(" … and %d more", len(files)-20)
		files = files[:20]
	}
	return strings.Join(files, ", ") + more
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

// ToolLine is a turn's tool calls by what they were for: "exec 3 · read 4 · failed 1".
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
		parts = append(parts, "failed "+strconv.Itoa(t.Failures))
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
			fmt.Fprintf(&b, " · %d failed", s.Stats.Failures)
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
