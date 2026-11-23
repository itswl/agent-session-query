package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
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
	pProject = prop{"project", "string", "Only sessions whose working directory contains this, e.g. a repository's name."}
	pBranch  = prop{"branch", "string", "Only sessions that opened on a git branch containing this."}
	pSince   = prop{"since", "string", "Only after this: a span back from now (90m, 12h, 7d, 2w) or a date (2026-09-01)."}
	pUntil   = prop{"until", "string", "Only before this; same forms as since."}
	pLimit   = prop{"limit", "integer", "How many to return."}
	pOffset  = prop{"offset", "integer", "Skip this many first: the next page."}
	pChars   = prop{"max_chars", "integer", "Cut the answer at about this many characters (default 16000)."}
	pRef     = prop{"ref", "string", "A session as the other tools print it — claude/3f2a9c1e — or the start of its id. Append #N for a turn: claude/3f2a9c1e#4. \"this\" (or this#4) is the session you are working in, which asq mcp started on stdio can tell and asq over HTTP cannot."}
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
		about: "Recent sessions, newest first: one line of where and when, one of what it was about. Each starts with the session's ref. " +
			"Use it for \"what was I working on\" and to find a session by its title, id or directory; use search to find where something was said.",
		props: []prop{
			{"text", "string", "Only sessions whose title, id or directory contains this."},
			pSource, pProject, pBranch, pSince, pUntil,
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
		about: "A handoff brief of a session, as Markdown: what was asked, which files were changed and how each stands now, what failed and whether the same command passed later, and the agent's final answer — for the latest turn, one named, a turn and those after it, everything since a moment, or the whole session. " +
			"Use it to pick up where another agent left off, or to hand a piece of work to someone else: ref \"this\" briefs your own session. It is extracted from the record, not summarized by a model.",
		props: []prop{
			pRef,
			{"turn", "integer", "Brief this turn instead of the latest."},
			{"from", "integer", "Brief this turn and every turn after it."},
			{"since", "string", "Brief every turn that ran after this moment: 2h, 2026-10-05T08:00 — or all, for the whole session."},
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
		about: "What the model calls consumed — calls, input, output and total tokens, cache hit rate, cost where the CLI recorded one. " +
			"Without by: rolling windows (24h, 7d, 30d, all), then by model, source and project.",
		props: []prop{
			{"by", "string", "One breakdown instead of the summary: model, day, source, project or session."},
			pSource, pProject, pBranch, pSince, pUntil, pLimit, pChars,
		},
		run: runUsage,
	},
	{
		name:  "failures",
		title: "Failed tool calls",
		about: "The tool calls that failed or were interrupted, newest first: the command, how it ended and the end of its output, with the ref of the turn it happened in. " +
			"Covers the last 7 days unless since says otherwise. Use it for \"what keeps going wrong\" and to see how an error was hit before. " +
			"When the same program and subcommand (go test, pnpm build, git push) later succeeded in the same session, a line says where (\"passed 40m later: <ref>#<turn> step <n>\") and, if it was run differently, how (\"as $ go test ./internal/x\"): judge from that whether it is the same thing passing. What changed in between is how it was got past — read those turns rather than guess.",
		props: []prop{
			{"tool", "string", "Only this tool's calls, named exactly as the answer prints it (Bash is not bash); a trailing * matches by prefix: mcp__playwright*."},
			{"query", "string", "Only the calls whose command or the end of whose output holds these words, anywhere, in any case: ECONNREFUSED · \"go test\" · migrate -dry. Each word must be there; -word must not; \"a phrase\" as written."},
			pSource, pProject, pBranch, pSince, pUntil, pLimit, pOffset, pChars,
		},
		run: runFailures,
	},
	{
		name:  "digest",
		title: "Digest of a stretch of work",
		about: "What a stretch of work added up to, per project: sessions, turns, tool calls and failures, tokens, files changed by kind, models used. " +
			"Covers the last 7 days unless since says otherwise. Use it for a standup, a weekly report, \"where did the time go\".",
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
	text, err := s.run(ctx, t, p.Arguments)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("cancelled")
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}, nil
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil
}

func (s *Server) run(ctx context.Context, t tool, given map[string]any) (string, error) {
	a := &args{given: given}
	if err := a.check(t); err != nil {
		return "", err
	}
	budget := a.num("max_chars", defaultChars)
	if a.err != nil {
		return "", a.err
	}
	a.budget = min(max(budget, 500), maxChars)

	fresh := s.App.Fresh(ctx, s.MaxAge, s.Wait)
	// Where the index stood before the answer was read is what the answer can say of it: a
	// sync that caught up while it was being read may not have caught up with what it read
	st, behind := s.App.Index.State(), s.App.Behind()
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
	if !fresh {
		if behind > 0 {
			notes = append(notes, fmt.Sprintf("The index is reading the history again for a newer reading of it (%d of %d transcripts): every session is here, but what changed in the last %s is not yet.", st.Done, st.Total, behind.Round(time.Second)))
		} else if st.Running && st.Total > 0 {
			notes = append(notes, fmt.Sprintf("The index is still reading the history (%d of %d transcripts): this answer covers what has been read so far.", st.Done, st.Total))
		} else {
			notes = append(notes, "The index is still catching up: this answer may miss the last few seconds.")
		}
	}
	if warnings, err := s.App.Index.Warnings(ctx, s.App.Stores()); err == nil {
		for _, w := range warnings {
			notes = append(notes, fmt.Sprintf("%s could not be read (%s): its sessions may be missing here.", w.Source, txt.OneLine(w.Error, 160)))
		}
	}
	if len(notes) > 0 {
		text += "\n\n[" + strings.Join(notes, " ") + "]"
	}
	return text, nil
}

// cut holds an answer to its budget, on a line boundary, and says what to do about it.
func cut(text string, budget int) string {
	if len(text) <= budget {
		return text
	}
	at := budget
	for at > 0 && !utf8.RuneStart(text[at]) {
		at--
	}
	if nl := strings.LastIndexByte(text[:at], '\n'); nl > budget/2 {
		at = nl
	}
	return text[:at] + fmt.Sprintf("\n\n[Cut here: %d more characters. Ask for less — fewer turns, a smaller limit, a narrower query — or raise max_chars.]", len(text)-at)
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
	return app.Query{
		Source: a.str("source"), Project: a.str("project"), Branch: a.str("branch"), Text: a.str("text"),
		Since: a.str("since"), Until: a.str("until"), Archived: a.flag("archived"), Agents: a.flag("agents"),
	}
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
	f, err := s.App.Filter(a.query())
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
	sess, named, said, err := s.session(ctx, a.str("ref"))
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
	left := room
	for i := range turns {
		if i > 0 {
			if left <= 0 {
				fmt.Fprintf(&b, "\n[Turns #%d–#%d left out: read them with turns=\"%d-%d\".]\n", turns[i].Index, turns[len(turns)-1].Index, turns[i].Index, turns[len(turns)-1].Index)
				break
			}
			b.WriteByte('\n')
		}
		before := b.Len()
		render.Turn(&b, &sess, &turns[i], render.TurnOptions{Full: r.Full, MaxChars: max(left, 1)}, st)
		left -= b.Len() - before
	}
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
	draw := func(turns []model.Turn) string {
		var b bytes.Buffer
		render.Outline(&b, sess, turns, app.Decorate(children), st)
		return b.String()
	}
	text := draw(turns)
	kept := turns
	for len(text) > budget && len(kept) > 4 {
		kept = kept[len(kept)-max(len(kept)*budget/len(text)*9/10, 4):]
		text = draw(kept)
	}
	if len(kept) < len(turns) && len(kept) > 0 {
		first := kept[0].Index
		text += fmt.Sprintf("\n[Turns before #%d left out: read with turns=\"1-%d\" to see them step by step, or raise max_chars for the whole outline.]\n", first, first-1)
	}
	text += "\nread with ref=\"" + sess.Ref + "#N\" opens turn N step by step.\n"
	return text, nil
}

func runBrief(ctx context.Context, s *Server, a *args) (string, error) {
	sess, named, said, err := s.session(ctx, a.str("ref"))
	if err != nil {
		return "", err
	}
	turn := a.num("turn", 0)
	if turn == 0 && named > 0 {
		turn = named
	}
	opts, err := app.BriefOf(turn, a.num("from", 0), a.str("since"))
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
		render.Usage(&b, "By "+by, rows, &total, style())
		return b.String(), nil
	}
	rep, err := s.App.Index.UsageSummary(ctx, f, w)
	if err != nil {
		return "", err
	}
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
	rep, err := s.App.Index.Failures(ctx, f, w, a.str("tool"), a.str("query"), index.Page{Limit: a.limit(10), Offset: a.offset()})
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	render.Failures(&b, rep, style())
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
	var b bytes.Buffer
	render.Digest(&b, rep, style())
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
