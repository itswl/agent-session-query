package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/txt"
)

// Style decides how text is dressed. The zero Style is plain: what a pipe, a file or an
// agent gets.
type Style struct {
	Color bool
	Width int       // columns to fit a line in; 0 means do not cut
	Now   time.Time // the moment "2h ago" is counted from; zero means now
	Home  string    // shown as ~ in a path
	// Args says the reader passes options as named arguments — an agent calling a tool —
	// rather than as flags, so a hint names "limit" where a terminal is told "--limit".
	Args bool
}

// opt names an option the way this reader would pass it.
func (s Style) opt(name string) string {
	if s.Args {
		return name
	}
	return "--" + name
}

func (s Style) paint(code, text string) string {
	if !s.Color || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s Style) bold(t string) string   { return s.paint("1", t) }
func (s Style) dim(t string) string    { return s.paint("2", t) }
func (s Style) red(t string) string    { return s.paint("31", t) }
func (s Style) green(t string) string  { return s.paint("32", t) }
func (s Style) yellow(t string) string { return s.paint("33", t) }
func (s Style) cyan(t string) string   { return s.paint("36", t) }

func (s Style) now() time.Time {
	if s.Now.IsZero() {
		return time.Now()
	}
	return s.Now
}

// Path shortens a directory for display: the home directory as ~.
func (s Style) Path(p string) string {
	if s.Home != "" && (p == s.Home || strings.HasPrefix(p, s.Home+"/")) {
		return "~" + p[len(s.Home):]
	}
	return p
}

// fit cuts a line of text to what is left of the width.
func (s Style) fit(text string, used int) string {
	if s.Width <= 0 {
		return text
	}
	return txt.Truncate(text, max(s.Width-used, 20), "…")
}

// ShortRefs gives each session the shortest reference that still tells it from the
// others in the list: the store and the start of its id, the way a commit is named.
func ShortRefs(list []index.Session) []string {
	out := make([]string, len(list))
	for i, s := range list {
		n := min(stem(s.ID)+8, len(s.ID))
		for n < len(s.ID) {
			clash := false
			for j, o := range list {
				if j != i && o.Source == s.Source && o.ID != s.ID && strings.HasPrefix(o.ID, s.ID[:n]) {
					clash = true
					break
				}
			}
			if !clash {
				break
			}
			n += 4
		}
		out[i] = s.Source + "/" + s.ID[:min(n, len(s.ID))]
	}
	return out
}

// stem is how much of an id is a word every id of its store opens with — "session-",
// "ses_", "rollout-" — and so tells nothing apart: a short reference starts counting
// after it.
func stem(id string) int {
	word := false // a letter no hexadecimal number has: "deadbeef-…" is an id, not a word
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			word = word || (r|0x20) > 'f'
		case (r == '-' || r == '_') && word && i >= 3 && i <= 12:
			return i + 1
		default:
			return 0
		}
	}
	return 0
}

func marks(st Style, failures int, interrupted bool) string {
	switch {
	case failures > 0:
		return st.red("✗" + strconv.Itoa(failures))
	case interrupted:
		return st.yellow("⚠")
	}
	return st.green("✓")
}

// Sessions prints a list of sessions, two lines each: where and when, then what.
func Sessions(w io.Writer, list []index.Session, total int, st Style) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No sessions.")
		return
	}
	refs := ShortRefs(list)
	width := 0
	for _, r := range refs {
		width = max(width, len(r))
	}
	for i, s := range list {
		state := ""
		switch {
		case s.Active:
			state = st.green(" ●")
		case s.Archived:
			state = st.dim(" archived")
		}
		facts := []string{txt.Ago(s.UpdatedAt, st.now()), plural(s.Stats.Turns, "turn")}
		if s.Stats.Failures > 0 {
			facts = append(facts, st.red(strconv.Itoa(s.Stats.Failures)+" failed"))
		}
		if s.Agents > 0 {
			facts = append(facts, plural(s.Agents, "agent"))
		}
		if s.Cwd != "" {
			facts = append(facts, st.Path(s.Cwd))
		}
		if s.Branch != "" {
			facts = append(facts, s.Branch)
		}
		fmt.Fprintf(w, "%s%s  %s\n", st.cyan(fmt.Sprintf("%-*s", width, refs[i])), state, st.dim(strings.Join(facts, " · ")))
		title := txt.OneLine(s.Title, 0)
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "  %s\n", st.fit(title, 2))
	}
	if total > len(list) {
		fmt.Fprintln(w, st.dim(fmt.Sprintf("… %d more (of %d): %s, %s, or narrow with %s / %s", total-len(list), total, st.opt("limit"), st.opt("offset"), st.opt("project"), st.opt("since"))))
	}
}

// Hits prints search results: the turn, what was asked there, and where the words are.
func Hits(w io.Writer, res index.SearchResult, offset int, st Style) {
	if len(res.Hits) == 0 {
		fmt.Fprintln(w, "No turn matches.")
		return
	}
	fmt.Fprintln(w, st.dim(fmt.Sprintf("%s in %s", plural(res.Total, "turn"), plural(res.Sessions, "session"))))
	for i, h := range res.Hits {
		where := []string{h.At.Local().Format("2006-01-02 15:04")}
		if h.Session.Cwd != "" {
			where = append(where, st.Path(h.Session.Cwd))
		}
		if h.Session.Parent != nil {
			where = append(where, "subagent of "+h.Session.Root)
		}
		fmt.Fprintf(w, "\n%s %s  %s\n", st.dim(strconv.Itoa(offset+i+1)+"."), st.cyan(h.Ref), st.dim(strings.Join(where, " · ")))
		fmt.Fprintf(w, "   %s %s\n", st.dim("ask   "), st.fit(txt.OneLine(h.Ask, 0), 10))
		for _, m := range h.Matches {
			label := string(m.Kind)
			switch {
			case m.Kind == model.Prompt:
				continue // the ask is already there, whole
			case m.Kind == model.ToolUse:
				label = m.Part
			}
			text := m.Snippet
			if m.Tool != "" {
				text = m.Tool + " › " + text
			}
			fmt.Fprintf(w, "   %s %s\n", st.dim(fmt.Sprintf("%-6s", label)), st.fit(text, 10))
		}
		if len(h.Matches) == 0 && h.Answer != "" {
			fmt.Fprintf(w, "   %s %s\n", st.dim("reply "), st.fit(txt.OneLine(h.Answer, 0), 10))
		}
	}
	if shown := offset + len(res.Hits); shown < res.Total {
		fmt.Fprintln(w, st.dim(fmt.Sprintf("\n… more turns match: %s %d for the next page, or narrow the search", st.opt("offset"), shown)))
	}
}

// Outline prints a session as its turns: what was asked in each, and how it ended.
func Outline(w io.Writer, s *index.Session, turns []model.Turn, children []index.Session, st Style) {
	title := txt.OneLine(s.Title, 0)
	if title == "" {
		title = "(untitled)"
	}
	fmt.Fprintln(w, st.bold(title))
	facts := []string{s.Ref}
	for _, f := range []string{st.Path(s.Cwd), s.Branch, s.Model} {
		if f != "" {
			facts = append(facts, f)
		}
	}
	fmt.Fprintln(w, st.dim(strings.Join(facts, " · ")))
	sums := []string{plural(s.Stats.Turns, "turn"), plural(s.Stats.Tools, "tool call")}
	if s.Stats.Failures > 0 {
		sums = append(sums, strconv.Itoa(s.Stats.Failures)+" failed")
	}
	if line := TokenLine(s.Stats.Usage); line != "" {
		sums = append(sums, line)
	}
	if s.Stats.CostUSD > 0 {
		sums = append(sums, fmt.Sprintf("$%.2f", s.Stats.CostUSD))
	}
	if !s.StartedAt.IsZero() {
		sums = append(sums, s.StartedAt.Local().Format("2006-01-02 15:04")+" → "+s.UpdatedAt.Local().Format("01-02 15:04"))
	}
	fmt.Fprintln(w, st.dim(strings.Join(sums, " · ")))
	if s.Parent != nil {
		fmt.Fprintln(w, st.dim("subagent ("+orDash(s.Parent.Type)+") of "+s.Root))
	}
	if s.Resume != "" {
		fmt.Fprintln(w, st.dim("resume: "+s.Resume))
	}
	if s.Error != "" {
		fmt.Fprintln(w, st.red("could not be read whole: "+s.Error))
	}
	fmt.Fprintln(w)
	for _, t := range turns {
		if t.Index == 0 {
			continue
		}
		head := fmt.Sprintf("#%-3d %s  %s", t.Index, shortTime(t.StartedAt), marks(st, t.Failures, t.Interrupted))
		facts := []string{}
		if d := workEnd(t).Sub(t.StartedAt); d > 0 {
			facts = append(facts, txt.Duration(d))
		}
		if t.Tools > 0 {
			facts = append(facts, plural(t.Tools, "tool"))
		}
		if n := changed(t.Files); n > 0 {
			facts = append(facts, "changed "+plural(n, "file"))
		}
		fmt.Fprintf(w, "%s  %s\n", head, st.dim(strings.Join(facts, " · ")))
		fmt.Fprintf(w, "     %s\n", st.fit(txt.OneLine(t.Ask, 0), 5))
		if t.Answer != "" {
			fmt.Fprintf(w, "     %s %s\n", st.dim("→"), st.dim(st.fit(txt.OneLine(t.Answer, 0), 7)))
		}
	}
	if len(children) > 0 {
		fmt.Fprintln(w, st.dim("\nSubagents:"))
		for i, ref := range ShortRefs(children) {
			c := children[i]
			fmt.Fprintf(w, "  %s  %s  %s\n", st.cyan(ref), st.dim(orDash(c.Parent.Type)), st.fit(txt.OneLine(c.Title, 0), len(ref)+20))
		}
	}
}

func changed(files []model.File) int {
	n := 0
	for _, f := range files {
		if f.Changed {
			n++
		}
	}
	return n
}

// TurnOptions tune how a turn is printed.
type TurnOptions struct {
	// Full prints every output and all reasoning whole. Without it a step is a line, and
	// only a failed step shows the end of its output.
	Full bool
	// MaxChars stops printing once this much has been written, saying what was left out:
	// what an agent reads is paid for by the token.
	MaxChars int
}

// Turn prints one turn step by step.
func Turn(w io.Writer, s *index.Session, t *model.Turn, o TurnOptions, st Style) {
	out := &budget{w: w, left: o.MaxChars}
	TurnHead(out, s, t, st)
	for i := range t.Items {
		if out.spent() {
			fmt.Fprintln(w, st.dim(fmt.Sprintf("… %d more steps left out: ask for fewer turns, or one step with its seq", len(t.Items)-i)))
			return
		}
		item(out, &t.Items[i], o, st)
	}
}

// TurnHead prints the line a turn opens with: where it is, when, and what it added up to.
func TurnHead(w io.Writer, s *index.Session, t *model.Turn, st Style) {
	if t.Index == 0 {
		fmt.Fprintln(w, st.bold("Before the first ask")+st.dim("  "+s.Ref+"#0"))
		return
	}
	facts := []string{s.Ref + "#" + strconv.Itoa(t.Index), t.StartedAt.Local().Format("2006-01-02 15:04")}
	if d := workEnd(*t).Sub(t.StartedAt); d > 0 {
		facts = append(facts, txt.Duration(d))
	}
	if t.Tools > 0 {
		facts = append(facts, ToolLine(t))
	}
	if line := TokenLine(t.Usage); line != "" {
		facts = append(facts, line)
	}
	if t.Interrupted {
		facts = append(facts, "interrupted")
	}
	fmt.Fprintln(w, st.dim(strings.Join(facts, " · ")))
}

// Step prints one step the way a turn prints it: what following a session prints as
// each arrives.
func Step(w io.Writer, it *model.Item, o TurnOptions, st Style) { item(w, it, o, st) }

// budget stops writing once a number of characters has gone through.
type budget struct {
	w    io.Writer
	left int
	done bool
}

func (b *budget) Write(p []byte) (int, error) {
	if b.left > 0 {
		if b.left -= len(p); b.left <= 0 {
			b.done = true
		}
	}
	return b.w.Write(p)
}

func (b *budget) spent() bool { return b.done }

func clock(t time.Time) string {
	if t.IsZero() {
		return "        "
	}
	return t.Local().Format("15:04:05")
}

// item prints one step of a turn.
func item(w io.Writer, it *model.Item, o TurnOptions, st Style) {
	at := st.dim(clock(it.At))
	body := func(text string, limit int) {
		text = strings.TrimRight(txt.StripControls(text), "\n")
		if !o.Full && limit > 0 {
			text = txt.Cap(text, limit)
		}
		for _, line := range strings.Split(text, "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	more := func(n int) {
		if n > 0 && !o.Full {
			fmt.Fprintln(w, st.dim(fmt.Sprintf("    … %s in all: seq %d reads it whole", sizeOf(n), it.Seq)))
		}
	}
	inherited := ""
	if it.Inherited {
		inherited = st.dim(" (copied from the parent session)")
	}
	switch it.Kind {
	case model.Prompt:
		label := "ASK"
		if it.Queued {
			label = "ASK (typed while the agent worked)"
		}
		fmt.Fprintf(w, "\n%s %s%s\n", at, st.bold(label), inherited)
		body(it.Text, 4000)
		more(it.Bytes)
	case model.Context:
		fmt.Fprintf(w, "%s %s %s\n", at, st.dim("context"), st.dim(st.fit(txt.OneLine(it.Text, 0), 20)))
	case model.Reply:
		fmt.Fprintf(w, "\n%s %s%s\n", at, st.bold("REPLY"), inherited)
		body(it.Text, 6000)
		more(it.Bytes)
	case model.Thinking:
		if o.Full {
			fmt.Fprintf(w, "%s %s\n", at, st.dim("thinking"))
			body(it.Text, 0)
			return
		}
		fmt.Fprintf(w, "%s %s %s\n", at, st.dim("thinking"), st.dim(st.fit(txt.OneLine(it.Text, 0), 20)))
	case model.Event:
		fmt.Fprintf(w, "%s %s %s\n", at, st.yellow("event "+it.Event), st.fit(txt.OneLine(it.Text, 0), 20+len(it.Event)))
	case model.ToolUse:
		tool := it.Tool
		what := tool.Command
		if what == "" && len(tool.Files) > 0 {
			what = strings.Join(tool.Files, ", ")
		}
		if what == "" {
			what = compactJSON(tool.Input)
		}
		outcome := ""
		switch {
		case !tool.Done:
			outcome = st.dim("  (no result)")
		case tool.Status == model.StatusInterrupted:
			outcome = "  " + st.yellow("⚠ interrupted")
		case tool.Failed():
			outcome = "  " + st.red("✗ failed")
		case tool.Status == model.StatusOK:
			// Said outright, so that no mark at all keeps meaning what it means: the CLI
			// did not record how the call ended
			outcome = "  " + st.green("✓")
		}
		if tool.Exit != nil && *tool.Exit != 0 {
			outcome += st.red(" · exit " + strconv.Itoa(*tool.Exit))
		}
		if tool.DurMs > 0 {
			outcome += st.dim(" · " + txt.Duration(time.Duration(tool.DurMs)*time.Millisecond))
		}
		fmt.Fprintf(w, "%s %s %s%s%s\n", at, st.cyan(tool.Name), st.fit(txt.OneLine(what, 0), 30+len(tool.Name)), outcome, st.dim("  ["+strconv.Itoa(it.Seq)+"]"))
		switch {
		case o.Full:
			if what != tool.Command && len(tool.Input) > 0 {
				body(indentJSON(tool.Input), 0)
			}
			if tool.Output != "" {
				body(tool.Output, 0)
			}
		case tool.Failed() && tool.Output != "":
			// The end of a failed step's output is where it says why
			lines := strings.Split(strings.TrimRight(txt.StripControls(tool.Output), "\n"), "\n")
			if len(lines) > 8 {
				lines = append([]string{"…"}, lines[len(lines)-8:]...)
			}
			for _, line := range lines {
				fmt.Fprintf(w, "    %s\n", st.dim(st.fit(line, 4)))
			}
		}
	}
}

func sizeOf(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KB"
	}
	return strconv.Itoa(n) + " bytes"
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		val := v[k]
		s, ok := val.(string)
		if !ok {
			b, _ := json.Marshal(val)
			s = string(b)
		}
		parts = append(parts, k+"="+txt.OneLine(s, 80))
	}
	return strings.Join(parts, " ")
}

func indentJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// Item prints one step whole: what `show <ref> --seq N` and an agent asking for one
// output get.
func Item(w io.Writer, s *index.Session, turn int, it *model.Item, st Style) {
	fmt.Fprintln(w, st.dim(fmt.Sprintf("%s#%d · step %d · %s", s.Ref, turn, it.Seq, it.At.Local().Format("2006-01-02 15:04:05"))))
	item(w, it, TurnOptions{Full: true}, st)
}

// Usage prints what model calls consumed, one row per group. total, when given, is the
// whole the rows are parts of — which a list cut to its largest groups does not add up
// to, so it is never summed from them.
func Usage(w io.Writer, title string, rows []index.UsageRow, total *index.UsageRow, st Style) {
	fmt.Fprintln(w, st.bold(title))
	if len(rows) == 0 {
		fmt.Fprintln(w, "  nothing recorded")
		return
	}
	width := 8
	for _, r := range rows {
		width = max(width, len([]rune(st.Path(r.Key))))
	}
	width = min(width, 48)
	fmt.Fprintln(w, st.dim(fmt.Sprintf("  %-*s %8s %9s %9s %9s %6s %9s", width, "", "calls", "in", "out", "total", "cache", "cost")))
	line := func(key string, r index.UsageRow) string {
		return fmt.Sprintf("  %-*s %8d %9s %9s %9s %5.0f%% %9s", width+len(key)-len([]rune(key)), key, r.Calls,
			txt.Tokens(r.Usage.Prompt()), txt.Tokens(r.Usage.Output), txt.Tokens(r.Usage.Total()), 100*r.Usage.CacheHit(), cost(r.CostUSD))
	}
	for _, r := range rows {
		key := txt.Truncate(st.Path(r.Key), width, "…")
		if key == "" {
			key = "(none)"
		}
		fmt.Fprintln(w, line(key, r))
	}
	if total != nil && len(rows) > 1 {
		fmt.Fprintln(w, st.dim(line("total", *total)))
	}
}

// UsageReport prints the whole answer to "what did this cost": the rolling windows,
// when the question set no bounds of its own, then where it went.
func UsageReport(w io.Writer, rep index.UsageReport, rolling bool, st Style) {
	if rolling {
		Usage(w, "Rolling windows", rep.Windows, nil, st)
		fmt.Fprintln(w)
	}
	Usage(w, "By model", rep.ByModel, &rep.Total, st)
	fmt.Fprintln(w)
	Usage(w, "By source", rep.BySource, &rep.Total, st)
	fmt.Fprintln(w)
	Usage(w, "By project", rep.ByProject, &rep.Total, st)
}

func cost(usd float64) string {
	if usd <= 0 {
		return "—"
	}
	return fmt.Sprintf("$%.2f", usd)
}

// Failures prints the tool calls that failed, newest first, and what failed most.
func Failures(w io.Writer, rep index.FailureReport, st Style) {
	by := make([]string, 0, len(rep.ByTool))
	for _, c := range rep.ByTool {
		by = append(by, fmt.Sprintf("%s %d", c.Tool, c.Count))
	}
	switch {
	case rep.Total == 0 && len(by) > 0:
		// A tool is named exactly, and the same tool has another name in another CLI:
		// say what there is rather than "nothing"
		fmt.Fprintln(w, "No failed calls of that tool. In this window: "+strings.Join(by, " · "))
		return
	case rep.Total == 0:
		fmt.Fprintln(w, "No failed tool calls.")
		return
	}
	fmt.Fprintln(w, st.dim(plural(rep.Total, "failure")+" · in this window: "+strings.Join(by, " · ")))
	for _, f := range rep.Failures {
		head := []string{f.At.Local().Format("01-02 15:04"), f.Session + "#" + strconv.Itoa(f.Turn)}
		how := f.Status
		if f.Exit != nil {
			how += " · exit " + strconv.Itoa(*f.Exit)
		}
		fmt.Fprintf(w, "\n%s  %s  %s\n", st.dim(head[0]), st.cyan(head[1]), st.red(f.Tool+" "+how))
		if f.Command != "" {
			fmt.Fprintf(w, "  $ %s\n", st.fit(txt.OneLine(f.Command, 0), 4))
		}
		if f.Output != "" {
			lines := strings.Split(strings.TrimSpace(f.Output), "\n")
			if len(lines) > 4 {
				lines = lines[len(lines)-4:]
			}
			for _, line := range lines {
				fmt.Fprintf(w, "  %s\n", st.dim(st.fit(line, 2)))
			}
		}
	}
	if len(rep.Failures) < rep.Total {
		fmt.Fprintln(w, st.dim(fmt.Sprintf("\n… %d more: %s, or narrow with %s / %s / %s", rep.Total-len(rep.Failures), st.opt("limit"), st.opt("tool"), st.opt("project"), st.opt("since"))))
	}
}

// Digest prints what a window's work added up to, per project.
func Digest(w io.Writer, rep index.DigestReport, st Style) {
	if len(rep.Projects) == 0 {
		fmt.Fprintln(w, "Nothing happened in that window.")
		return
	}
	line := func(d index.Digest) string {
		parts := []string{plural(d.Sessions, "session"), plural(d.Turns, "turn"), plural(d.Tools, "tool call")}
		if d.Failures > 0 {
			parts = append(parts, strconv.Itoa(d.Failures)+" failed")
		}
		if t := TokenLine(d.Usage); t != "" {
			parts = append(parts, t)
		}
		if d.CostUSD > 0 {
			parts = append(parts, cost(d.CostUSD))
		}
		return strings.Join(parts, " · ")
	}
	counts := func(m map[string]int, order []string) string {
		keys := order
		if keys == nil {
			for k := range m {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || (m[keys[i]] == m[keys[j]] && keys[i] < keys[j]) })
		}
		var parts []string
		for _, k := range keys {
			if m[k] > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
			}
		}
		return strings.Join(parts, " · ")
	}
	fmt.Fprintf(w, "%s  %s\n", st.bold("Total"), line(rep.Total))
	for _, d := range rep.Projects {
		name := st.Path(d.Cwd)
		if name == "" {
			name = "(no project)"
		}
		fmt.Fprintf(w, "\n%s\n  %s\n", st.bold(name), line(d))
		if c := counts(d.Changed, []string{"code", "test", "docs", "config"}); c != "" {
			fmt.Fprintf(w, "  %s %s\n", st.dim("changed"), c)
		}
		if c := counts(d.Models, nil); c != "" {
			fmt.Fprintf(w, "  %s %s\n", st.dim("models "), c)
		}
		fmt.Fprintf(w, "  %s %s · %s → %s\n", st.dim("with   "), counts(d.Sources, nil), shortTime(d.FirstAt), shortTime(d.LastAt))
	}
}

// Files prints which turns touched a file, newest first.
func Files(w io.Writer, list []index.FileTouch, total int, st Style) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No turn names a file like that.")
		return
	}
	for _, f := range list {
		verb := st.dim("read   ")
		if f.Changed {
			verb = st.green("changed")
		}
		fmt.Fprintf(w, "%s  %s  %s  %s\n", st.dim(f.At.Local().Format("2006-01-02 15:04")), verb, st.cyan(f.Session+"#"+strconv.Itoa(f.Turn)), st.Path(f.Path))
		if f.Ask != "" {
			fmt.Fprintf(w, "    %s\n", st.dim(st.fit(txt.OneLine(f.Ask, 0), 4)))
		}
	}
	if total > len(list) {
		fmt.Fprintln(w, st.dim(fmt.Sprintf("… %d more", total-len(list))))
	}
}

// Projects prints the working directories sessions ran in.
func Projects(w io.Writer, list []index.Project, st Style) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No sessions.")
		return
	}
	for _, p := range list {
		name := st.Path(p.Cwd)
		if name == "" {
			name = "(no project)"
		}
		keys := make([]string, 0, len(p.Sources))
		for k := range p.Sources {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			keys[i] = fmt.Sprintf("%s %d", k, p.Sources[k])
		}
		fmt.Fprintf(w, "%s\n  %s\n", st.bold(name), st.dim(fmt.Sprintf("%s · %s · %s · %s",
			plural(p.Sessions, "session"), plural(p.Turns, "turn"), txt.Ago(p.UpdatedAt, st.now()), strings.Join(keys, " · "))))
	}
}
