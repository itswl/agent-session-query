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

// Warn paints a mark that asks to be read, without being a failure.
func (s Style) Warn(t string) string { return s.yellow(t) }

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

// marks is how a turn ended, in a mark: ✗ and how many calls failed, while the same command
// has not passed later for each of them; ⚠ when the person stopped it; ✓ otherwise.
func marks(st Style, t *model.Turn) string {
	switch {
	case t.Failures > t.PassedLater:
		return st.red("✗" + strconv.Itoa(t.Failures))
	case t.Interrupted:
		return st.yellow("⚠")
	}
	return st.green("✓")
}

// failedWords says how many calls failed, and how many of them the same command passed
// later: "2 failed, 1 passed later".
func failedWords(failed, passed int) string {
	return strconv.Itoa(failed) + " failed" + laterWords(failed, passed)
}

// laterWords is how many of the calls that failed the same command passed later, as it
// follows their count: ", 1 passed later", ", all passed later"; "" for none.
func laterWords(failed, passed int) string {
	switch {
	case passed <= 0:
		return ""
	case passed >= failed && failed == 1:
		return ", passed later"
	case passed >= failed:
		return ", all passed later"
	}
	return ", " + strconv.Itoa(passed) + " passed later"
}

// failedNote is failedWords painted: red while a failure is open, muted once the same
// command passed later for each — a step of the work, not where it stands.
func failedNote(st Style, failed, passed int) string {
	if passed >= failed {
		return st.dim(failedWords(failed, passed))
	}
	return st.red(strconv.Itoa(failed)+" failed") + st.dim(laterWords(failed, passed))
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
		if live := liveFact(s.Live, st); live != "" {
			facts = append([]string{live}, facts...)
		}
		if s.Stats.Failures > 0 {
			facts = append(facts, failedNote(st, s.Stats.Failures, s.Stats.PassedLater))
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

// Hits prints search results: each turn with how it ended, what was asked there, where
// the words are and what it concluded. A search no turn matched whole says which words
// no turn holds, and marks its hits as partial matches, each with the words it lacks —
// and, when the filters left out turns that match it whole, says so, and what to change.
func Hits(w io.Writer, res index.SearchResult, offset int, st Style) {
	none := "No turn"
	if o := res.Outside; o != nil && len(o.Filters) > 0 {
		none = "No turn the filters select"
	} else if o != nil {
		none = "No turn outside archived sessions"
	}
	if len(res.Hits) == 0 {
		if len(res.Missing) > 0 {
			fmt.Fprintf(w, "%s holds %s.\n", none, quoted(res.Missing, "or"))
		} else {
			fmt.Fprintf(w, "%s matches.\n", none)
		}
		outside(w, res.Outside, st)
		return
	}
	if p := res.Partial; p != nil {
		line := fmt.Sprintf("%s holds all %d words", none, p.Words)
		if len(res.Missing) > 0 {
			line += "; none holds " + quoted(res.Missing, "or")
		}
		fmt.Fprintln(w, line+".")
		outside(w, res.Outside, st)
		fmt.Fprintln(w, st.Warn(fmt.Sprintf("Partial matches, closest first: %s in %s, the closest holding %d of the %d.",
			plural(res.Total, "turn"), plural(res.Sessions, "session"), p.Held, p.Words)))
	} else {
		fmt.Fprintln(w, st.dim(fmt.Sprintf("%s in %s", plural(res.Total, "turn"), plural(res.Sessions, "session"))))
	}
	for i, h := range res.Hits {
		hit(w, offset+i+1, h, st)
	}
	if shown := offset + len(res.Hits); shown < res.Total {
		fmt.Fprintln(w, st.dim(fmt.Sprintf("\n… more turns match: %s %d for the next page, or narrow the search", st.opt("offset"), shown)))
	}
}

// outside says how many turns the filters kept from matching a search whole, and what
// would show them: the filters dropped, archived sessions included.
func outside(w io.Writer, o *index.Outside, st Style) {
	switch {
	case o == nil:
		return
	case o.Turns == 0 && len(o.Held) > 0:
		fmt.Fprintf(w, "None outside them matches either, though some hold %s.\n", quoted(o.Held, "and"))
		return
	case o.Turns == 0:
		fmt.Fprintln(w, "Nor does any outside them.")
		return
	}
	sessions := plural(o.Sessions, "session")
	if o.Archived {
		sessions = plural(o.Sessions, "archived session")
	}
	where := plural(o.Turns, "turn") + " in " + sessions
	var change []string
	if len(o.Filters) > 0 {
		where += " outside them"
		opts := make([]string, len(o.Filters))
		for i, name := range o.Filters {
			opts[i] = st.opt(name)
		}
		change = append(change, "drop "+strings.Join(opts, ", "))
	}
	if o.Archived {
		change = append(change, "add "+st.opt("archived"))
	}
	verb, them := "match", "them"
	if o.Turns == 1 {
		verb, them = "matches", "it"
	}
	fmt.Fprintln(w, st.Warn(fmt.Sprintf("%s %s: %s to see %s.", where, verb, strings.Join(change, " and "), them)))
}

// hit prints one turn a search found, numbered n: where and when, how it ended, the ask,
// the lines the words were found in, and the answer.
func hit(w io.Writer, n int, h index.Hit, st Style) {
	where := []string{h.At.Local().Format("2006-01-02 15:04")}
	if h.Session.Cwd != "" {
		where = append(where, st.Path(h.Session.Cwd))
	}
	if h.Session.Parent != nil {
		where = append(where, "subagent of "+h.Session.Root)
	}
	fmt.Fprintf(w, "\n%s %s  %s  %s\n", st.dim(strconv.Itoa(n)+"."), st.cyan(h.Ref), st.dim(strings.Join(where, " · ")), ending(h, st))
	if len(h.Lacks) > 0 {
		fmt.Fprintf(w, "   %s %s\n", st.dim("lacks "), st.Warn(st.fit(strings.Join(h.Lacks, " "), 10)))
	}
	fmt.Fprintf(w, "   %s %s\n", st.dim("ask   "), st.fit(txt.OneLine(h.Ask, 0), 10))
	answer := st.fit(txt.OneLine(h.Answer, 0), 10)
	for _, m := range h.Matches {
		label := string(m.Kind)
		switch {
		case m.Kind == model.Prompt:
			continue // the ask is already there, whole
		case m.Kind == model.Reply && answer != "" && strings.Contains(answer, strings.Trim(m.Snippet, "…")):
			continue // and so is the answer, below
		case m.Kind == model.ToolUse:
			label = m.Part
		}
		text := m.Snippet
		if m.Tool != "" {
			text = m.Tool + " › " + text
		}
		fmt.Fprintf(w, "   %s %s\n", st.dim(fmt.Sprintf("%-6s", label)), st.fit(text, 10))
	}
	if answer != "" {
		fmt.Fprintf(w, "   %s %s\n", st.dim("answer"), answer)
	}
}

// ending says how a hit's turn ended: the calls that failed, and how many of them the same
// command passed later — ✗ while one has not, ✓ once each has; that the person stopped it;
// or, neither, ✓.
func ending(h index.Hit, st Style) string {
	var parts []string
	switch {
	case h.Failed > h.PassedLater:
		parts = append(parts, st.red("✗")+failedNote(st, h.Failed, h.PassedLater))
	case h.Failed > 0:
		parts = append(parts, st.green("✓")+" "+failedNote(st, h.Failed, h.PassedLater))
	}
	if h.Interrupted {
		parts = append(parts, st.yellow("⚠ interrupted"))
	}
	if len(parts) == 0 {
		return st.green("✓")
	}
	return strings.Join(parts, st.dim(" · "))
}

// quoted lists words in quotation marks: “a”, “b” or “c”.
func quoted(words []string, and string) string {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = "“" + w + "”"
	}
	if len(q) == 1 {
		return q[0]
	}
	return strings.Join(q[:len(q)-1], ", ") + " " + and + " " + q[len(q)-1]
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
		sums = append(sums, failedWords(s.Stats.Failures, s.Stats.PassedLater))
	}
	if line := TokenLine(s.Stats.Usage); line != "" {
		sums = append(sums, line)
	}
	if s.Stats.CostUSD > 0 {
		sums = append(sums, Cost(s.Stats.CostUSD))
	}
	if !s.StartedAt.IsZero() {
		sums = append(sums, s.StartedAt.Local().Format("2006-01-02 15:04")+" → "+s.UpdatedAt.Local().Format("01-02 15:04"))
	}
	fmt.Fprintln(w, st.dim(strings.Join(sums, " · ")))
	if line := liveLine(s.Live, st); line != "" {
		fmt.Fprintln(w, line)
	}
	if s.Parent != nil {
		fmt.Fprintln(w, st.dim("subagent ("+orDash(s.Parent.Type)+") of "+s.Root))
	}
	if s.Resume != "" {
		fmt.Fprintln(w, st.dim("resume: "+s.Resume))
	} else if s.ResumeThere != "" {
		fmt.Fprintln(w, st.dim("resume where "+s.Source+" came from: "+s.ResumeThere))
	}
	if s.Error != "" {
		fmt.Fprintln(w, st.red("could not be read whole: "+s.Error))
	}
	fmt.Fprintln(w)
	for _, t := range turns {
		if t.Index == 0 {
			continue
		}
		head := fmt.Sprintf("#%-3d %s  %s", t.Index, shortTime(t.StartedAt), marks(st, &t))
		facts := []string{}
		if d := workEnd(t).Sub(t.StartedAt); d > 0 {
			facts = append(facts, txt.Duration(d))
		}
		if t.Tools > 0 {
			facts = append(facts, plural(t.Tools, "tool"))
		}
		if t.PassedLater > 0 {
			facts = append(facts, failedWords(t.Failures, t.PassedLater))
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

// liveFact is where a session's latest turn stands, as a fact of its line in a list:
// "working", "waiting on Bash: go test ./...", "idle". Empty when it is not said.
func liveFact(l *model.Live, st Style) string {
	if l == nil {
		return ""
	}
	switch l.State {
	case model.LiveWorking:
		return st.green("working")
	case model.LiveWaiting:
		return st.yellow(waitingOn(l, st, 48))
	case model.LiveIdle:
		return "idle"
	}
	return ""
}

// waitingOn names the call a session waits on: the tool, and what it runs or names.
func waitingOn(l *model.Live, st Style, width int) string {
	what := l.Command
	if what == "" && l.File != "" {
		what = st.Path(l.File)
	}
	on := "waiting on " + orDash(txt.OneLine(l.Tool, 60))
	if what = txt.OneLine(what, width); what != "" {
		on += ": " + what
	}
	return on
}

// liveLine says where a session's latest turn stands, and since when, as a line of its
// outline. Empty when it is not said.
func liveLine(l *model.Live, st Style) string {
	if l == nil {
		return ""
	}
	since := ""
	if !l.Since.IsZero() {
		since = " since " + l.Since.Local().Format("15:04") + ", " + txt.Ago(l.Since, st.now())
	}
	switch l.State {
	case model.LiveWorking:
		return st.green("working" + since)
	case model.LiveWaiting:
		step := ""
		if l.Seq > 0 {
			step = fmt.Sprintf(" [%d]", l.Seq)
		}
		return st.yellow(waitingOn(l, st, 120)+step+since) + st.dim(" — a command still running, or a prompt asking permission to run it")
	case model.LiveIdle:
		return "idle" + since + st.dim(" — the agent's turn is over: the next move is the person's")
	}
	return ""
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
	// reasoning is the part of out that was thinking, where the provider said: a dash is
	// "not said", which is not "none"
	fmt.Fprintln(w, st.dim(fmt.Sprintf("  %-*s %8s %9s %9s %9s %9s %6s", width, "", "calls", "in", "out", "reasoning", "total", "cache")))
	line := func(key string, r index.UsageRow) string {
		reasoning := "—"
		if r.Usage.Reasoning > 0 {
			reasoning = txt.Tokens(r.Usage.Reasoning)
		}
		return fmt.Sprintf("  %-*s %8d %9s %9s %9s %9s %5.0f%%", width+len(key)-len([]rune(key)), key, r.Calls,
			txt.Tokens(r.Usage.Prompt()), txt.Tokens(r.Usage.Output), reasoning, txt.Tokens(r.Usage.Total()), 100*r.Usage.CacheHit())
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

// Cost is a session's own cost as its CLI worked it out: "$1.84", and "<$0.01" for one
// that would otherwise read as nothing.
func Cost(usd float64) string {
	if usd < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", usd)
}

// Within says which stretch of time a report covers, from its bounds as they were asked
// for — "in the last 7 days", "since 2026-09-01", "until 2026-10-01" — or "" for all time.
func Within(since, until string) string {
	since, until = strings.TrimSpace(since), strings.TrimSpace(until)
	if since == "all" {
		since = ""
	}
	if until == "all" {
		until = ""
	}
	var parts []string
	if since != "" {
		if span := spanWords(since); span != "" {
			parts = append(parts, "in the last "+span)
		} else {
			parts = append(parts, "since "+since)
		}
	}
	if until != "" {
		if span := spanWords(until); span != "" {
			parts = append(parts, "until "+span+" ago")
		} else {
			parts = append(parts, "until "+until)
		}
	}
	return strings.Join(parts, ", ")
}

// spanWords is a span back from now as written for a command — 90m, 12h, 7d, 2w — in words:
// "90 minutes", "hour"; "" for what is not one.
func spanWords(s string) string {
	n := len(s)
	if n < 2 {
		return ""
	}
	unit := map[byte]string{'m': "minute", 'h': "hour", 'd': "day", 'w': "week"}[s[n-1]]
	amount, err := strconv.ParseFloat(s[:n-1], 64)
	if unit == "" || err != nil || amount < 0 {
		return ""
	}
	if amount == 1 {
		return unit
	}
	return strconv.FormatFloat(amount, 'f', -1, 64) + " " + unit + "s"
}

// Failures prints the tool calls that failed, newest first, and what failed most. within
// names the stretch of time they were looked for in (see Within).
func Failures(w io.Writer, rep index.FailureReport, within string, st Style) {
	if noFailures(w, rep.FailureCounts, within, st) {
		return
	}
	by := make([]string, 0, len(rep.ByTool))
	for _, c := range rep.ByTool {
		by = append(by, fmt.Sprintf("%s %d", c.Tool, c.Count))
	}
	fmt.Fprintln(w, st.dim(failuresCounted(rep.FailureCounts, rep.Total, "failure")+" · "+orAllTime(within)+": "+strings.Join(by, " · ")))
	for _, f := range rep.Failures {
		fmt.Fprintln(w)
		failure(w, f, "", "", 4, st)
	}
	if len(rep.Failures) < rep.Total {
		fmt.Fprintln(w, st.dim(fmt.Sprintf("\n… %d more: %s, or narrow with %s / %s / %s / %s", rep.Total-len(rep.Failures), st.opt("limit"), st.opt("open"), st.opt("tool"), st.opt("project"), st.opt("since"))))
	}
}

// FailureGroups prints the failed calls by the command that failed, those with failures
// still open first: how often each failed, in how many sessions, and the latest.
func FailureGroups(w io.Writer, rep index.FailureGroups, within string, st Style) {
	if noFailures(w, rep.FailureCounts, within, st) {
		return
	}
	fmt.Fprintln(w, st.dim(plural(rep.Total, "command")+" · "+failuresCounted(rep.FailureCounts, rep.Open+rep.PassedLater, "failure")+" · "+orAllTime(within)))
	for _, g := range rep.Commands {
		name := g.Command
		if name == "" {
			name = g.Tool + st.dim(" (no command)")
		}
		mark := st.red("✗")
		if g.Open == 0 {
			mark = st.green("✓")
		}
		fmt.Fprintf(w, "\n%s %s  %s %s\n", mark, st.bold(name), failedNote(st, g.Count, g.Count-g.Open), st.dim("· "+plural(g.Sessions, "session")))
		failure(w, g.Last, "  ", "latest ", 2, st)
	}
	if shown := len(rep.Commands); shown < rep.Total {
		fmt.Fprintln(w, st.dim(fmt.Sprintf("\n… %d more commands: %s, %s, or narrow with %s / %s / %s", rep.Total-shown, st.opt("limit"), st.opt("offset"), st.opt("open"), st.opt("project"), st.opt("since"))))
	}
}

// failuresCounted says how many failures match: "7 failures, 5 passed later", or of a list
// of the open ones, "2 of 7 failures still open". n is how many of what is listed.
func failuresCounted(c index.FailureCounts, n int, what string) string {
	if c.Asked.Open {
		return fmt.Sprintf("%d of %s still open", c.Open, plural(c.Open+c.PassedLater, what))
	}
	return plural(n, what) + laterWords(n, c.PassedLater)
}

func orAllTime(within string) string {
	if within == "" {
		return "of all time"
	}
	return within
}

// noFailures says, when no failure matches, that none does in the window, and what there
// is: the tools that failed in it, the failures an open-only list leaves out, the ones
// outside the window. It reports whether it said so.
func noFailures(w io.Writer, c index.FailureCounts, within string, st Style) bool {
	if c.Total > 0 {
		return false
	}
	by := make([]string, 0, len(c.ByTool))
	for _, n := range c.ByTool {
		by = append(by, fmt.Sprintf("%s %d", n.Tool, n.Count))
	}
	in := ""
	if within != "" {
		in = " " + within
	}
	// A tool is named exactly, and the same tool has another name in another CLI: say what
	// there is rather than "nothing" — and what the window alone left out
	tool, words := c.Asked.Tool != "", strings.TrimSpace(c.Asked.Text) != ""
	var line string
	switch {
	case c.Asked.Open && c.PassedLater > 0:
		of := ""
		switch {
		case tool && words:
			of = " of that tool holding those words"
		case tool:
			of = " of that tool"
		case words:
			of = " holding those words"
		}
		each := "it"
		switch {
		case c.PassedLater == 2:
			each = "both"
		case c.PassedLater > 2:
			each = "all " + strconv.Itoa(c.PassedLater)
		}
		line = "No failed call" + of + in + " is still open: the same command passed later for " + each
	case len(by) == 0 || !tool && !words:
		line = "No failed tool calls" + in
	case tool && words:
		line = "No failed calls of that tool hold those words" + in
	case tool:
		line = "No failed calls of that tool" + in
	default:
		line = "No failed calls hold those words" + in
	}
	if c.Outside > 0 {
		outside, all := "before then", st.opt("since")+" all"
		if strings.Contains(within, "until") {
			outside, all = "outside it", all+" and no "+st.opt("until")
		}
		open := ""
		if c.Asked.Open {
			open = "still open "
		}
		line += fmt.Sprintf("; %d %sfrom %s: %s", c.Outside, open, outside, all)
	}
	fmt.Fprintln(w, line+".")
	if len(by) > 0 {
		fmt.Fprintln(w, "Failed"+in+": "+strings.Join(by, " · "))
	}
	return true
}

// failure prints one failed call, indented: when, where, how it ended, its command, the
// last lines of its output, and where the same command passed later.
func failure(w io.Writer, f index.Failure, indent, label string, lines int, st Style) {
	how := f.Status
	if f.Exit != nil {
		how += " · exit " + strconv.Itoa(*f.Exit)
	}
	in := len(indent)
	fmt.Fprintf(w, "%s%s%s  %s  %s\n", indent, st.dim(label), st.dim(f.At.Local().Format("01-02 15:04")), st.cyan(f.Session+"#"+strconv.Itoa(f.Turn)), st.red(f.Tool+" "+how))
	if f.Command != "" {
		fmt.Fprintf(w, "%s  $ %s\n", indent, st.fit(txt.OneLine(f.Command, 0), in+4))
	}
	if f.Output != "" {
		out := strings.Split(strings.TrimSpace(f.Output), "\n")
		if len(out) > lines {
			out = out[len(out)-lines:]
		}
		for _, line := range out {
			fmt.Fprintf(w, "%s  %s\n", indent, st.dim(st.fit(line, in+2)))
		}
	}
	if l := f.Later; l != nil {
		fmt.Fprintf(w, "%s  %s %s\n", indent, st.green("↳ passed "+laterBy(f.At, l.At)), st.cyan(fmt.Sprintf("%s#%d step %d", f.Session, l.Turn, l.Seq)))
		if l.Command != txt.OneLine(f.Command, 0) {
			fmt.Fprintf(w, "%s    %s\n", indent, st.dim("as $ "+st.fit(l.Command, in+9)))
		}
	}
}

// laterBy says how long after a failure the same command succeeded.
func laterBy(failed, passed time.Time) string {
	if failed.IsZero() || passed.IsZero() || !passed.After(failed) {
		return "later:"
	}
	return txt.Duration(passed.Sub(failed)) + " later:"
}

// FileNowLine says how a file stands against what a turn left, and on what evidence; ""
// when nothing can be said.
func FileNowLine(n *index.FileNow) string { return fileNowAgainst(n, "this turn") }

// fileNowAgainst is FileNowLine with the turn named as the line is read: "this turn",
// "turn 5".
func fileNowAgainst(n *index.FileNow, turn string) string {
	if n == nil {
		return ""
	}
	switch {
	case n.State == index.FileSame && n.By == index.ByContent:
		return "now: as " + turn + " left it"
	case n.State == index.FileSame:
		return "now: not written since"
	case n.State == index.FileChanged:
		return "now: changed since — what " + turn + " wrote is no longer all there"
	case n.State == index.FileWritten && n.By == index.ByContent:
		return "now: written since — what " + turn + " wrote is still in it"
	case n.State == index.FileWritten:
		return "now: written since (by its time; the text was not compared)"
	case n.State == index.FileDeleted:
		return "now: deleted"
	}
	return ""
}

// waitingLine says that a file was not looked at, because a look in its folder waits.
func waitingLine(folder string) string {
	return "now: not looked at — a look in " + folder + " is waiting (on a Mac, macOS asking whether asq may read it)"
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
			parts = append(parts, failedWords(d.Failures, d.PassedLater))
		}
		if t := TokenLine(d.Usage); t != "" {
			parts = append(parts, t)
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
		if line := FileNowLine(f.Now); line != "" {
			paint := st.dim
			switch f.Now.State {
			case index.FileChanged, index.FileDeleted:
				paint = st.yellow
			case index.FileWritten:
				paint = st.yellow
			}
			fmt.Fprintf(w, "    %s\n", paint(line))
		} else if f.Waiting != "" {
			fmt.Fprintf(w, "    %s\n", st.dim(waitingLine(f.Waiting)))
		}
	}
	if total > len(list) {
		fmt.Fprintln(w, st.dim(fmt.Sprintf("… %d more", total-len(list))))
	}
}

// Blamed is a line of a file and the commit git says last changed it, as Why prints it.
// Commit is empty for a line not committed yet.
type Blamed struct {
	File    string // as it was given
	Path    string // its whole path
	Line    int
	Text    string
	Commit  string
	Subject string
	At      time.Time
}

// Why prints which session wrote a line: the line, the commit that last changed it, the
// turns whose calls made that commit, and the others that name it.
func Why(w io.Writer, b Blamed, made, mentioned []index.Hit, st Style) {
	fmt.Fprintf(w, "%s\n    %s\n", b.File+":"+strconv.Itoa(b.Line), st.fit(txt.OneLine(b.Text, 0), 4))
	if b.Commit == "" {
		fmt.Fprintf(w, "Not committed yet: asq files %s names the turns that changed the file.\n", st.Path(b.Path))
		return
	}
	short := b.Commit[:min(12, len(b.Commit))]
	line := []string{st.cyan(short)}
	if !b.At.IsZero() {
		line = append(line, b.At.Local().Format("2006-01-02 15:04"))
	}
	if b.Subject != "" {
		line = append(line, st.fit(txt.OneLine(b.Subject, 0), 34))
	}
	fmt.Fprintln(w, strings.Join(line, st.dim(" · ")))
	if len(made) == 0 {
		fmt.Fprintf(w, "\nNo turn made %s: it was committed outside the sessions asq reads. asq files %s names the turns that changed the file.\n", short, st.Path(b.Path))
	} else {
		fmt.Fprintf(w, "\nMade in %s:\n", plural(len(made), "turn"))
		for i, h := range made {
			hit(w, i+1, h, st)
		}
	}
	if len(mentioned) > 0 {
		label := "Named in " + plural(len(mentioned), "turn")
		if len(made) > 0 {
			label = "Also named in " + plural(len(mentioned), "turn")
		}
		fmt.Fprintf(w, "\n%s:\n", label)
		for i, h := range mentioned {
			hit(w, i+1, h, st)
		}
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
