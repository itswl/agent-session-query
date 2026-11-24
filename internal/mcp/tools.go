package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/render"
	"github.com/itswl/agent-session-query/internal/txt"
)

// Budgets, in characters. A tool's answer lands in a context window somebody pays for.
const (
	defaultChars = 16_000
	maxChars     = 400_000
	lineWidth    = 240 // where a one-line quote of an ask or an answer is cut
)

// tool is one question an agent can ask.
type tool struct {
	name  string
	title string
	about string
	props []prop
	need  []string
	run   func(ctx context.Context, s *Server, a *args) (string, error)
}

// prop is one argument of a tool.
type prop struct {
	name, kind, about string
}

// The arguments that select sessions, shared by every tool that takes a selection.
var (
	pSource  = prop{"source", "string", "Only one CLI's sessions (claude, codex, gemini, opencode, grok, antigravity, pi, dsh, hermes, openclaw, copilot) or one labeled store (claude@box2)."}
	pProject = prop{"project", "string", "Only sessions whose working directory contains this, e.g. a repository's name. \".\" (or ./web, ..) is the directory you work in and those below it, which asq mcp started on stdio can tell, and asq over HTTP only when the request names a Codex thread."}
	pBranch  = prop{"branch", "string", "Only sessions that opened on a git branch containing this."}
	pSince   = prop{"since", "string", "Only after this: a span back from now (90m, 12h, 7d, 2w), a date (2026-09-01), or today, yesterday or a weekday (monday) — the midnight that day began with on the clock of the machine asq runs on, which may not be yours."}
	pUntil   = prop{"until", "string", "Only before this; same forms as since."}
	pLimit   = prop{"limit", "integer", "How many to return."}
	pOffset  = prop{"offset", "integer", "Skip this many first: the next page."}
	pChars   = prop{"max_chars", "integer", "Cut the answer at about this many characters (default 16000)."}
	pRef     = prop{"ref", "string", "A session as the other tools print it — claude/3f2a9c1e — or the start of its id. Append #N for a turn: claude/3f2a9c1e#4. \"this\" (or this#4) is the session you are working in, which asq mcp started on stdio can tell, and asq over HTTP only when the request names it, as Codex does."}
)

var tools = []tool{
	{
		name:  "search",
		title: "Search the history",
		about: "Full-text search over every turn of every session: what was asked, what the agent replied and thought, the tools it called and what they printed, and the session's title, searched with its first turn. " +
			"Returns the matching turns, best first, each with its ref (store/id#turn), how the turn ended (✓ no call failed, or each that did the same command passed later: ✓ 1 failed, passed later; ✗N failed, and how many of those passed later; ⚠ interrupted), the lines the words were found in, and the turn's final answer. " +
			"When no turn holds every word, it says which words no turn holds at all and returns the turns that hold the most of them, marked as partial matches, each with the words it lacks — " +
			"and when the filters given left out turns that match whole, how many, and which filter to drop to see them. " +
			"Use it for \"how did I solve this before\", \"where did that error come up\", \"which session touched X\". " +
			"Query syntax: bare words must all appear, each matching as a prefix and in any form of the English word (retries finds retried); \"quoted words\" match as a phrase, as written; -word excludes; " +
			"ask:word, reply:word, thinking:word, tool:word, output:word and title:word confine a word to the person's ask, the agent's reply, its reasoning, a tool's input, a tool's output, or the session's title.",
		props: []prop{
			{"query", "string", "What to look for, e.g.  connection refused  ·  \"rate limit\" -test  ·  ask:deploy output:ECONNREFUSED"},
			pSource, pProject, pBranch, pSince, pUntil,
			{"archived", "boolean", "Include sessions their CLI has archived."},
			{"recent", "boolean", "Newest first instead of best match first."},
			{"per_session", "integer", "At most this many turns of any one session (default 3; 0 for no bound)."},
			pLimit, pOffset, pChars,
		},
		need: []string{"query"},
		run:  runSearch,
	},
	{
		name:  "sessions",
		title: "List sessions",
		about: "Recent sessions, newest first: one line of where and when, one of what it was about. Each starts with the session's ref, " +
			"and, where its transcript tells, says where its latest turn stands: working, waiting on a tool call (a command running, or a permission prompt), or idle (done, the person to answer). " +
			"Use it for \"what was I working on\", for \"which agents are waiting on me\" (state waiting,idle), and to find a session by its title, id or directory; use search to find where something was said.",
		props: []prop{
			{"text", "string", "Only sessions whose title, id or directory contains this."},
			pSource, pProject, pBranch, pSince, pUntil,
			{"state", "string", "Only sessions whose latest turn stands so, by what their transcript says: working, waiting (on a tool call: a command running, or a permission prompt), idle (the agent is done and the person is to answer), or several, comma-separated. Said for Claude Code, Codex, Gemini CLI, OpenCode and DeepSeek Harness."},
			{"archived", "boolean", "Include sessions their CLI has archived."},
			{"agents", "boolean", "Include subagents' transcripts as sessions of their own."},
			pLimit, pOffset, pChars,
		},
		run: runSessions,
	},
	{
		name:  "read",
		title: "Read a session",
		about: "Read one session at the depth asked for. " +
			"With only ref: its outline — every turn's ask and how it ended, and its subagents. " +
			"With a turn (ref ending #4, or turns): those turns step by step — replies, reasoning, each tool call and how it ended, every step numbered [seq]. " +
			"With seq: that one step whole — a tool call's full input and output. " +
			"Read the outline first; a long session read whole does not fit.",
		props: []prop{
			pRef,
			{"turns", "string", "Which turns to read step by step: 3, 3-5, last, or all."},
			{"seq", "integer", "One step whole, by the number in [brackets] beside it."},
			{"full", "boolean", "With turns: print every output and all reasoning whole instead of a line each."},
			pChars,
		},
		need: []string{"ref"},
		run:  runRead,
	},
	{
		name:  "brief",
		title: "Handoff brief",
		about: "A handoff brief of a session, as Markdown: what was asked, which files were changed and how each stands now, what failed and whether the same command passed later, and the agent's final answer. " +
			"It covers the whole session — what it added up to, then its latest turns one by one — unless one of turn, from or since names a part of it, or the ref ends #N for that turn alone. " +
			"Use it to pick up where another agent left off, or to hand a piece of work to someone else: ref \"this\" briefs your own session. It is extracted from the record, not summarized by a model.",
		props: []prop{
			pRef,
			{"turn", "integer", "Brief this turn alone."},
			{"from", "integer", "Brief this turn and every turn after it."},
			{"since", "string", "Brief every turn that ran after this moment: 2h, 2026-10-05T08:00. Without turn, from or since the brief is of the whole session."},
			pChars,
		},
		need: []string{"ref"},
		run:  runBrief,
	},
	{
		name:  "pack",
		title: "Pack several sessions",
		about: "Several sessions as one Markdown document, oldest first: for each, what was asked and what it concluded. " +
			"Use it to carry the history of a project or a stretch of days into another conversation — \"how did this get here\".",
		props: []prop{
			pSource, pProject, pBranch, pSince, pUntil,
			{"text", "string", "Only sessions whose title, id or directory contains this."},
			{"turns", "boolean", "Spell out every turn of every session, not only how each ended."},
			pLimit, pChars,
		},
		run: runPack,
	},
	{
		name:  "usage",
		title: "Token usage",
		about: "What the model calls consumed — calls, the prompt (in), output and total tokens, how much of the output was reasoning, and the cache hit rate — and, in every grouping but by model, how long agents worked on the turns (agent time: from each ask to the last thing the agent did, summed). " +
			"Without by: rolling windows (24h, 7d, 30d, all), then by model, source, project and session — a session by its ref, title and directory, its subagents' calls in it. Use it for \"where did the tokens go\", \"which session ate them\", \"how long did agents work each week\" (by week).",
		props: []prop{
			{"by", "string", "One breakdown instead of the summary: model, day, week (by the Monday it begins on), month, source, project or session. Days, weeks and months are the latest, in order."},
			pSource, pProject, pBranch, pSince, pUntil, pLimit, pChars,
		},
		run: runUsage,
	},
	{
		name:  "failures",
		title: "Failed tool calls",
		about: "The tool calls that failed or were interrupted, newest first: the command, how it ended and the end of its output, with the ref of the turn it happened in. " +
			"Covers the last 7 days unless since says otherwise, and says so; when none there match, it says how many match before then — widen since rather than conclude it never happened. Use it for \"what keeps going wrong\" and to see how an error was hit before. " +
			"When the same program and subcommand (go test, pnpm build, git push) later succeeded in the same session, a line says where (\"passed 40m later: <ref>#<turn> step <n>\") and, if it was run differently, how (\"as $ go test ./internal/x\"): judge from that whether it is the same thing passing. What changed in between is how it was got past — read those turns rather than guess.",
		props: []prop{
			{"tool", "string", "Only this tool's calls, named exactly as the answer prints it (Bash is not bash); a trailing * matches by prefix: mcp__playwright*."},
			{"query", "string", "Only the calls whose command or the end of whose output holds these words, anywhere, in any case: ECONNREFUSED · \"go test\" · migrate -dry. Each word must be there; -word must not; \"a phrase\" as written."},
			{"open", "boolean", "Only the failures whose command did not pass later: what is still broken."},
			{"by", "string", "command: one entry per command that failed (go test, terraform plan) — how often, in how many sessions, how many still open, and the latest — the ones still open first. For \"what keeps failing\"."},
			pSource, pProject, pBranch, pSince, pUntil, pLimit, pOffset, pChars,
		},
		run: runFailures,
	},
	{
		name:  "digest",
		title: "Digest of a stretch of work",
		about: "What a stretch of work added up to, per project: sessions, turns, tool calls and failures (and how many of those the same command passed later), tokens, files changed by kind, models used, " +
			"and how long agents worked and on how many days, with a grid of when by weekday and hour; " +
			"then each project's latest sessions — as many as max_chars leaves room for, the same number for each — with its ref, when it was last at work, how its turns ended (✓; ✗N failed, and how many of those passed later; ✓ 1 failed, passed later once each did; ⚠ interrupted), its title and the last thing its agent said. " +
			"Covers the last 7 days unless since says otherwise; the answer's first line says which moments that was, on the clock of the machine asq runs on. Use it for a standup, a weekly report, \"what did I do today\", \"where did the time go\".",
		props: []prop{pSource, pProject, pBranch, pSince, pUntil, pChars},
		run:   runDigest,
	},
	{
		name:  "files",
		title: "Turns that touched a file",
		about: "Which turns read or changed a file, newest first, each with the ask that led to it. Use it for \"who changed this file and why\". Each line also says how the file stands now against what that turn left: \"as this turn left it\" (the text it wrote is there, and nothing has written the file since — or the turn wrote it whole and it is that, to the byte), \"not written since\", \"written since\" (with what this turn wrote still in it, or by its time alone), \"changed since\" (what this turn wrote is no longer all there), \"deleted\"; nothing for a file not on this machine. A turn's account of a file written or changed since may be out of date: read the file before acting on it.",
		props: []prop{
			{"path", "string", "Part of the file's path, e.g. internal/server/auth.go or just auth.go."},
			{"changed", "boolean", "Only the turns that changed it, not those that only read it."},
			pSource, pProject, pBranch, pSince, pUntil, pLimit, pOffset, pChars,
		},
		need: []string{"path"},
		run:  runFiles,
	},
	{
		name:  "projects",
		title: "Projects",
		about: "The working directories sessions ran in, most recently used first, with how many sessions and turns each holds and which CLIs worked there.",
		props: []prop{pSource, pProject, pSince, pUntil, pLimit, pChars},
		run:   runProjects,
	},
}

// describe is tools/list: every tool with the schema of its arguments.
func describe() []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		props := map[string]any{}
		for _, p := range t.props {
			props[p.name] = map[string]any{"type": p.kind, "description": p.about}
		}
		schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(t.need) > 0 {
			schema["required"] = t.need
		}
		out = append(out, map[string]any{
			"name":        t.name,
			"title":       t.title,
			"description": t.about,
			"inputSchema": schema,
			// Nothing here writes, and nothing reaches past this machine's own records
			"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
		})
	}
	return out
}

func find(name string) (tool, bool) {
	for _, t := range tools {
		if t.name == name {
			return t, true
		}
	}
	return tool{}, false
}

// call runs tools/call. What goes wrong with the question — a ref that names nothing, an
// argument misspelled — is told to the model as the tool's answer, so it can ask again;
// only a call that is not a call at all is a protocol error.
func (s *Server) call(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Meta      meta           `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeParams, Message: "unreadable params: " + err.Error()}
	}
	t, ok := find(p.Name)
	if !ok {
		names := make([]string, len(tools))
		for i, t := range tools {
			names[i] = t.name
		}
		return nil, &rpcError{Code: codeParams, Message: fmt.Sprintf("unknown tool %q (there are: %s)", p.Name, strings.Join(names, ", "))}
	}
	text, err := s.run(ctx, t, p.Arguments, p.Meta.asker())
	if err != nil {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": s.failed(ctx, err).Error()}}, "isError": true}, nil
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil
}

// failed is what is said of an answer that could not be given: that the question was
// cancelled, or that the index is damaged and is being made anew, where either is why.
func (s *Server) failed(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.New("cancelled")
	}
	if s.App.Index.Damage(err) {
		return fmt.Errorf("%w — the index is damaged, and is set aside and made anew from the stores at their next look: ask again in a few seconds", err)
	}
	return err
}

// standing is where the index stood as an answer was read: whether it had caught up with
// the stores, and if not, how far it had got. Where it stood before the answer was read is
// what the answer can say of it: a sync that caught up while it was being read may not
// have caught up with what it read.
type standing struct {
	fresh  bool
	st     index.SyncState
	behind time.Duration
}

// look brings the index up to date before a question, as far as it may in the time it is
// given, and says where it stood then.
func (s *Server) look(ctx context.Context) standing {
	fresh := s.App.Fresh(ctx, s.MaxAge, s.Wait)
	return standing{fresh: fresh, st: s.App.Index.State(), behind: s.App.Behind()}
}

// notes are what an answer, or the error in its place, says beneath it of how far it can
// be taken: that the index is still reading, that a store could not be read, and, when no
// CLI's sessions were found at all, where asq looked.
func (s *Server) notes(ctx context.Context, at standing) []string {
	var notes []string
	if !at.fresh {
		if at.behind > 0 {
			notes = append(notes, fmt.Sprintf("The index is reading the history again for a newer reading of it (%s): every session is here, but what changed in the last %s is not yet.", at.st.Progress(), at.behind.Round(time.Second)))
		} else if at.st.Running && at.st.Total > 0 {
			notes = append(notes, fmt.Sprintf("The index is still reading the history (%s): this answer covers what has been read so far.", at.st.Progress()))
		} else {
			notes = append(notes, "The index is still catching up: this answer may miss the last few seconds.")
		}
	}
	if warnings, err := s.App.Index.Warnings(ctx, s.App.Stores()); err == nil {
		for _, w := range warnings {
			notes = append(notes, fmt.Sprintf("%s could not be read (%s): its sessions may be missing here.", w.Source, txt.OneLine(w.Error, 160)))
		}
	}
	if looked, err := s.App.NothingFound(ctx); err == nil && len(looked) > 0 {
		places := make([]string, len(looked))
		for i, l := range looked {
			places[i] = fmt.Sprintf("%s %s (%s)", l.Source, l.Location, l.State())
		}
		notes = append(notes, "No CLI's sessions were found on the machine asq runs on. It looked in: "+strings.Join(places, ", ")+
			". asq doctor there says more; a store somewhere else is named with ASQ_PATH.")
	}
	return notes
}

// bracketed is notes as they follow what they are about: in brackets, after a blank line.
func bracketed(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return "\n\n[" + strings.Join(notes, " ") + "]"
}

func (s *Server) run(ctx context.Context, t tool, given map[string]any, who asker) (string, error) {
	a := &args{given: given, who: who}
	if err := a.check(t); err != nil {
		return "", err
	}
	budget := a.num("max_chars", defaultChars)
	if a.err != nil {
		return "", a.err
	}
	a.budget = min(max(budget, 500), maxChars)

	at := s.look(ctx)
	// A project of "." is the directory the agent asking works in, not a part of a path
	if project := a.str("project"); app.Relative(project) {
		dir, how, err := s.workDir(ctx, who)
		if err != nil {
			return "", fmt.Errorf("project %q: %w", project, err)
		}
		dir = joinDir(dir, project)
		if a.dirs, err = s.App.Paths(ctx, []string{dir}, true); err != nil {
			return "", err
		}
		a.here = fmt.Sprintf("project %q is %s and the directories below it: %s.", project, dir, how)
	}
	text, err := t.run(ctx, s, a)
	if err != nil {
		return "", err
	}
	if a.err != nil {
		return "", a.err
	}
	// What is handed to another model leaves this machine with it: the shapes that
	// announce themselves as secrets do not go along
	text = cut(txt.Redact(strings.TrimRight(text, "\n")), a.budget)

	var notes []string
	if a.here != "" {
		notes = append(notes, a.here)
	}
	return text + bracketed(append(notes, s.notes(ctx, at)...)), nil
}

// joinDir is the directory a project of ".", "./web" or ".." names from dir. "." is dir as
// it was given: cleaned, it would be another spelling of it on Windows.
func joinDir(dir, project string) string {
	if project == "." {
		return dir
	}
	return filepath.Join(dir, filepath.FromSlash(project))
}

// cut holds an answer to its budget, on a line boundary, and says what to do about it.
func cut(text string, budget int) string {
	return cutFor(text, budget, "Ask for less — fewer turns, a smaller limit, a narrower query — or raise max_chars.")
}

// cutFor is cut, with what to do about it said for what was asked.
func cutFor(text string, budget int, advice string) string {
	return render.Cut(text, max(budget, 1), advice)
}

// style is how text is dressed for a model: plain, lines cut where a quote stops paying,
// and hints that name arguments rather than flags.
func style() render.Style {
	return render.Style{Width: lineWidth, Args: true, Home: os.Getenv("HOME")}
}

// args are a call's arguments, read leniently — models send "5" for 5 and "true" for
// true — and strictly about names: an argument the tool does not have is said so,
// because one silently dropped is a filter the caller believes was applied.
type args struct {
	given  map[string]any
	budget int
	err    error
	who    asker // what the request said of who sent it
	// dirs is the directory a project of "." was taken to be, by every path that names it,
	// and here says which that was, and why
	dirs []string
	here string
}

func (a *args) check(t tool) error {
	known := map[string]bool{}
	names := make([]string, 0, len(t.props))
	for _, p := range t.props {
		known[p.name] = true
		names = append(names, p.name)
	}
	var unknown []string
	for name := range a.given {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s takes no argument %q (it takes: %s)", t.name, strings.Join(unknown, `", "`), strings.Join(names, ", "))
	}
	for _, name := range t.need {
		if strings.TrimSpace(a.str(name)) == "" {
			return fmt.Errorf("%s needs %s", t.name, name)
		}
	}
	return a.err
}

func (a *args) fail(name, want string) {
	if a.err == nil {
		a.err = fmt.Errorf("%s: want %s, got %v", name, want, a.given[name])
	}
}

func (a *args) has(name string) bool {
	v, ok := a.given[name]
	return ok && v != nil
}

func (a *args) str(name string) string {
	switch v := a.given[name].(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	a.fail(name, "text")
	return ""
}

func (a *args) num(name string, def int) int {
	switch v := a.given[name].(type) {
	case nil:
		return def
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1e9 {
			return int(v)
		}
	case string:
		if v = strings.TrimSpace(v); v == "" {
			return def
		}
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	a.fail(name, "a whole number")
	return def
}

func (a *args) flag(name string) bool {
	switch v := a.given[name].(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "1":
			return true
		case "false", "no", "0", "":
			return false
		}
	case float64:
		return v != 0
	}
	a.fail(name, "true or false")
	return false
}

// limit is how many to return: the tool's default, and never more than a page an answer
// can hold.
func (a *args) limit(def int) int {
	n := a.num("limit", def)
	if n <= 0 {
		n = def
	}
	return min(n, 200)
}

func (a *args) offset() int { return max(a.num("offset", 0), 0) }

func (a *args) query() app.Query {
	q := app.Query{
		Source: a.str("source"), Project: a.str("project"), Branch: a.str("branch"), Text: a.str("text"),
		Since: a.str("since"), Until: a.str("until"), Archived: a.flag("archived"), Agents: a.flag("agents"),
	}
	if a.dirs != nil {
		q.Project, q.Dirs = "", a.dirs
	}
	return q
}

func runSearch(ctx context.Context, s *Server, a *args) (string, error) {
	q := a.query()
	q.Agents = true // a subagent's turns are found with the rest
	f, err := s.App.Filter(q)
	if err != nil {
		return "", err
	}
	per := a.num("per_session", 3)
	if a.has("per_session") && per <= 0 {
		per = -1
	}
	opts := index.SearchOptions{Limit: a.limit(8), Offset: a.offset(), PerSession: per, Recent: a.flag("recent")}
	res, err := s.App.Search(ctx, index.ParseQuery(a.str("query")), f, opts)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	render.Hits(&b, res, opts.Offset, style())
	if len(res.Hits) > 0 {
		b.WriteString("\nread with ref=\"<ref>\" opens a turn step by step.\n")
	}
	if res.Partial != nil {
		b.WriteString("These turns lack some of the words: another word for what one lacks may find a turn that holds them all.\n")
	}
	return b.String(), nil
}

func runSessions(ctx context.Context, s *Server, a *args) (string, error) {
	q := a.query()
	q.State = a.str("state")
	f, err := s.App.Filter(q)
	if err != nil {
		return "", err
	}
	list, total, err := s.App.Index.Sessions(ctx, f, index.Page{Limit: a.limit(15), Offset: a.offset()})
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	render.Sessions(&b, list, total, style())
	return b.String(), nil
}

func runRead(ctx context.Context, s *Server, a *args) (string, error) {
	sess, named, said, err := s.session(ctx, a.str("ref"), a.who)
	if err != nil {
		if _, this := thisRef(a.str("ref")); errors.Is(err, index.ErrNotFound) && !this {
			err = fmt.Errorf("%w — sessions and search print the refs there are", err)
		}
		return "", err
	}
	st := style()
	var b bytes.Buffer
	if said != "" {
		b.WriteString(said + "\n\n")
	}
	// What is left of the budget once the line that says which session this is has been
	// spent: the answer is cut at the budget, that line and all
	room := max(a.budget-b.Len(), 1)

	if a.has("seq") {
		seq := a.num("seq", -1)
		it, turn, err := s.App.Index.Item(ctx, &sess, seq)
		if err != nil {
			return "", fmt.Errorf("%s has no step %d (it has %d, numbered from 0)", sess.Ref, seq, sess.Stats.Items)
		}
		render.Item(&b, &sess, turn, &it, st)
		return b.String(), nil
	}

	r, some, err := app.TurnRange(a.str("turns"), named)
	if err != nil {
		return "", err
	}
	if !some {
		text, err := outline(ctx, s, &sess, room, st)
		return b.String() + text, err
	}
	r.Full = a.flag("full")
	turns, err := s.App.Index.Turns(ctx, &sess, r)
	if err != nil {
		return "", err
	}
	if len(turns) == 0 {
		return "", fmt.Errorf("%s has no such turn (it has %d)", sess.Ref, sess.Stats.Turns)
	}
	// The budget is spent turn by turn, so what is left out is whole turns that can be
	// asked for by number, not the tail of one
	render.FitTurns(&b, &sess, turns, r.Full, room, st)
	// Each turn already said what it left out; the cut that follows is only for a single
	// step printed whole that is larger than everything asked for
	a.budget = max(a.budget, min(b.Len(), a.budget+a.budget/2))
	return b.String(), nil
}

// outline prints a session as its turns. One too long for the budget keeps its last
// turns — where the work stands — and says how to read the ones before.
func outline(ctx context.Context, s *Server, sess *index.Session, budget int, st render.Style) (string, error) {
	turns, err := s.App.Index.Outline(ctx, sess)
	if err != nil {
		return "", err
	}
	children, err := s.App.Index.Children(ctx, sess)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	render.FitOutline(&b, sess, turns, app.Decorate(children), budget, st)
	b.WriteString("\nread with ref=\"" + sess.Ref + "#N\" opens turn N step by step.\n")
	return b.String(), nil
}

func runBrief(ctx context.Context, s *Server, a *args) (string, error) {
	sess, named, said, err := s.session(ctx, a.str("ref"), a.who)
	if err != nil {
		return "", err
	}
	turn, err := app.NamedTurn(named, a.num("turn", 0))
	if err != nil {
		return "", err
	}
	opts, err := app.Handoff(turn, a.num("from", 0), a.str("since"))
	if err != nil {
		return "", err
	}
	// Written to fit: cut at the budget, a brief would lose where things stand at the end
	if said != "" {
		said += "\n\n"
	}
	opts.MaxChars = max(a.budget-len(said), 1)
	text, err := s.App.Brief(ctx, &sess, opts)
	return said + text, err
}

func runPack(ctx context.Context, s *Server, a *args) (string, error) {
	text, _, err := s.App.Pack(ctx, a.query(), a.limit(12), a.flag("turns"))
	return text, err
}

func runUsage(ctx context.Context, s *Server, a *args) (string, error) {
	q := a.query()
	f, err := s.App.Filter(q)
	if err != nil {
		return "", err
	}
	w, err := s.App.Window(q, "")
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if by := strings.ToLower(strings.TrimSpace(a.str("by"))); by != "" {
		rows, err := s.App.Index.Usage(ctx, f, w, by, a.limit(20))
		if err != nil {
			return "", err
		}
		total, err := s.App.Index.UsageTotal(ctx, f, w)
		if err != nil {
			return "", err
		}
		render.Span(&b, w.Since, w.Until, style())
		render.Usage(&b, "By "+by, rows, &total, style())
		return b.String(), nil
	}
	rep, err := s.App.Index.UsageSummary(ctx, f, w)
	if err != nil {
		return "", err
	}
	render.Span(&b, w.Since, w.Until, style())
	render.UsageReport(&b, rep, q.Since == "" && q.Until == "", style())
	return b.String(), nil
}

func runFailures(ctx context.Context, s *Server, a *args) (string, error) {
	q := a.query()
	f, err := s.App.Filter(q)
	if err != nil {
		return "", err
	}
	w, err := s.App.Window(q, "7d")
	if err != nil {
		return "", err
	}
	fq := index.FailureQuery{Tool: a.str("tool"), Text: a.str("query"), Open: a.flag("open")}
	since := q.Since
	if since == "" {
		since = "7d"
	}
	within := render.Within(since, q.Until)
	var b bytes.Buffer
	switch by := strings.ToLower(strings.TrimSpace(a.str("by"))); by {
	case "":
		rep, err := s.App.Index.Failures(ctx, f, w, fq, index.Page{Limit: a.limit(10), Offset: a.offset()})
		if err != nil {
			return "", err
		}
		render.Failures(&b, rep, within, style())
	case "command":
		rep, err := s.App.Index.FailuresByCommand(ctx, f, w, fq, index.Page{Limit: a.limit(10), Offset: a.offset()})
		if err != nil {
			return "", err
		}
		render.FailureGroups(&b, rep, within, style())
	default:
		return "", fmt.Errorf("failures are grouped by command, not by %q", by)
	}
	return b.String(), nil
}

func runDigest(ctx context.Context, s *Server, a *args) (string, error) {
	q := a.query()
	f, err := s.App.Filter(q)
	if err != nil {
		return "", err
	}
	w, err := s.App.Window(q, "7d")
	if err != nil {
		return "", err
	}
	rep, err := s.App.Index.Digest(ctx, f, w)
	if err != nil {
		return "", err
	}
	// The sessions of each project give way to the answer's budget before the counts of
	// any project do
	var b bytes.Buffer
	render.DigestWithin(&b, rep, style(), a.budget)
	return b.String(), nil
}

func runFiles(ctx context.Context, s *Server, a *args) (string, error) {
	q := a.query()
	f, err := s.App.Filter(q)
	if err != nil {
		return "", err
	}
	w, err := s.App.Window(q, "")
	if err != nil {
		return "", err
	}
	list, total, err := s.App.Files(ctx, f, w, a.str("path"), a.flag("changed"), index.Page{Limit: a.limit(20), Offset: a.offset()})
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	render.Files(&b, list, total, style())
	return b.String(), nil
}

func runProjects(ctx context.Context, s *Server, a *args) (string, error) {
	f, err := s.App.Filter(a.query())
	if err != nil {
		return "", err
	}
	list, err := s.App.Index.Projects(ctx, f)
	if err != nil {
		return "", err
	}
	if n := a.limit(30); len(list) > n {
		list = list[:n]
	}
	var b bytes.Buffer
	render.Projects(&b, list, style())
	return b.String(), nil
}
