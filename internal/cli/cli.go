// Package cli is the command line: every question the program answers, asked from a
// terminal or a script.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/render"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
	"github.com/itswl/agent-session-query/internal/txt"
)

const usage = `asq — the session history of every agent CLI on this machine, as one record.

  asq ls                       recent sessions                  (--project, --since, --source …)
  asq search <words>           the turns that hold these words  ("a phrase", -not, ask:word)
  asq show <ref>               a session as its turns; <ref>#4 one turn, step by step; -f follows it live
  asq brief <ref>              a handoff brief of a session     (--turn N, --since 2h)
  asq pack                     several sessions as one document (--project, --since)
  asq export <ref>             a session whole                  (--format md|json|jsonl|html)

  asq usage                    what the model calls consumed    (--by model|day|source|project)
  asq failures                 the tool calls that failed       (--tool, --since)
  asq digest                   what a stretch of work added up to, per project
  asq files <path>             the turns that touched a file    (--changed)
  asq projects                 the directories sessions ran in

  asq resume <ref>             the command that reopens a session in its own CLI (--run)
  asq serve                    the web page and the HTTP API    (--port, --token)
  asq mcp                      an MCP server on stdio, for agents to ask their own history
  asq otel                     export sessions as OpenTelemetry traces
  asq doctor                   check every store: found, readable, and what it holds
  asq index                    where the index is and what is in it (rebuild, rm)

A <ref> is what ls and search print — claude/3f2a9c1e — or just the start of a session's
id. Every command takes --json. 'asq <command> -h' lists a command's flags.
`

// usageText is the usage, its first line naming the version this binary is.
func usageText() string {
	return strings.Replace(usage, "asq —", "asq "+app.Version+" —", 1)
}

// Run runs one command and returns the process's exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usageText())
		return 0
	}
	name, rest := args[0], args[1:]
	run, ok := commands[name]
	if !ok {
		switch name {
		case "-h", "--help", "help":
			fmt.Fprint(stdout, usageText())
			return 0
		case "-v", "--version", "version":
			fmt.Fprintln(stdout, "asq", app.Version)
			return 0
		}
		fmt.Fprintf(stderr, "asq: unknown command %q\n\n%s", name, usageText())
		return 2
	}
	// Ctrl+C or a TERM asks the command to finish what it is doing and stop. Once asked,
	// the signals are let go of: a second one ends the process at once, as it would
	// any other, for when finishing takes longer than anyone wants to wait.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	err := run(ctx, rest, &streams{in: stdin, out: stdout, err: stderr})
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errFailed):
		return 1
	}
	fmt.Fprintf(stderr, "asq %s: %v\n", name, err)
	if _, usage := err.(usageError); usage {
		return 2
	}
	return 1
}

type streams struct {
	in       io.Reader
	out, err io.Writer
}

// errFailed ends a command that already said what went wrong.
var errFailed = errors.New("failed")

type usageError struct{ error }

func usagef(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

var commands map[string]func(context.Context, []string, *streams) error

func init() {
	commands = map[string]func(context.Context, []string, *streams) error{
		"ls": cmdList, "list": cmdList, "sessions": cmdList,
		"search": cmdSearch, "s": cmdSearch,
		"show": cmdShow, "brief": cmdBrief, "pack": cmdPack, "export": cmdExport,
		"usage": cmdUsage, "failures": cmdFailures, "digest": cmdDigest, "files": cmdFiles, "projects": cmdProjects,
		"resume": cmdResume, "serve": cmdServe, "ping": cmdPing, "mcp": cmdMCP, "otel": cmdOTel, "doctor": cmdDoctor, "index": cmdIndex,
	}
}

// open opens the program and brings the index up to date, saying so on the terminal when
// that takes long enough to wonder about — a first run reads the whole history.
func open(ctx context.Context, c *command, io *streams) (*app.App, error) {
	cfg, err := c.config()
	if err != nil {
		return nil, usageError{err}
	}
	a, err := app.Open(cfg)
	if err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.Sync(ctx, index.SyncOptions{})
		done <- err
	}()
	// On a terminal the count is redrawn in place; anywhere else — a pipe, a log — a long
	// read is announced once, in a line of its own, and never with control sequences
	live := isTerminal(io.err)
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	said, started := false, time.Now()
	for {
		select {
		case err := <-done:
			if said && live {
				fmt.Fprint(io.err, "\r\x1b[K")
			}
			if ctx.Err() != nil {
				a.Close()
				return nil, ctx.Err()
			}
			if err != nil {
				fmt.Fprintf(io.err, "asq: the index could not be brought fully up to date: %v\n", err)
			}
			// Said with every answer: without it "no sessions" reads the same whether
			// there are none or the store could not be read
			warn(ctx, a, io)
			return a, nil
		case <-tick.C:
			st := a.Index.State()
			switch {
			case st.Total == 0:
			case live:
				fmt.Fprintf(io.err, "\rreading %d of %d transcripts…", st.Done, st.Total)
				said = true
			case !said && time.Since(started) > 2*time.Second:
				fmt.Fprintf(io.err, "asq: reading %d transcripts into the index…\n", st.Total)
				said = true
			}
		}
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// warn says which stores could not be read: without it "no sessions" reads the same
// whether there are none or the store broke.
func warn(ctx context.Context, a *app.App, io *streams) {
	warnings, _ := a.Index.Warnings(ctx, a.Stores())
	for _, w := range warnings {
		fmt.Fprintf(io.err, "asq: %s could not be read (%s): %s\n", w.Source, w.Location, w.Error)
	}
}

func cmdList(ctx context.Context, args []string, io *streams) error {
	c := newCommand("ls", io.err).selection(20)
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		c.q.Text = strings.Join(rest, " ") // `asq ls nginx`: sessions whose title, id or directory has it
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	f, err := a.Filter(c.q)
	if err != nil {
		return usageError{err}
	}
	list, total, err := a.Index.Sessions(ctx, f, index.Page{Limit: c.limit, Offset: c.offset})
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(io.out, map[string]any{"sessions": app.Listed(list), "total": total})
	}
	render.Sessions(io.out, list, total, c.style(io.out))
	return nil
}

func cmdSearch(ctx context.Context, args []string, io *streams) error {
	c := newCommand("search", io.err).selection(10)
	recent := c.fs.Bool("recent", false, "newest first, instead of best match first")
	per := c.fs.Int("per-session", 3, "at most this many turns of one session (0: no bound)")
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return usagef("what should it look for?  asq search <words>")
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	c.q.Agents = true // a subagent's turns are found with the rest
	f, err := a.Filter(c.q)
	if err != nil {
		return usageError{err}
	}
	if *per == 0 {
		*per = -1
	}
	res, err := a.Index.Search(ctx, index.ParseQuery(strings.Join(rest, " ")), f,
		index.SearchOptions{Limit: c.limit, Offset: c.offset, PerSession: *per, Recent: *recent})
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(io.out, app.ListedHits(res))
	}
	render.Hits(io.out, res, c.offset, c.style(io.out))
	return nil
}

// resolve finds the session a reference names, with the hint a terminal user needs.
func resolve(ctx context.Context, a *app.App, ref string) (index.Session, int, error) {
	s, turn, err := a.Resolve(ctx, ref)
	if errors.Is(err, index.ErrNotFound) {
		err = fmt.Errorf("%w (asq ls prints refs)", err)
	}
	return s, turn, err
}

func cmdShow(ctx context.Context, args []string, io *streams) error {
	c := newCommand("show", io.err)
	full := c.fs.Bool("full", false, "every output and all reasoning whole")
	seq := c.fs.Int("seq", -1, "one step whole, by the number in [brackets] beside it")
	turns := c.fs.String("turns", "", "these turns step by step: 3, 3-5, or last (default: the outline)")
	follow := c.fs.Bool("follow", false, "print the latest turn, then each step as the agent takes it, until interrupted")
	c.fs.BoolVar(follow, "f", false, "short for --follow")
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("which session?  asq show <ref>   (asq ls prints refs)")
	}
	if *follow {
		if c.json || *seq >= 0 {
			return usagef("--follow prints steps as text, as they come: it goes with neither --json nor --seq")
		}
		if *turns == "" {
			*turns = "last"
		}
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	s, turn, err := resolve(ctx, a, rest[0])
	if err != nil {
		return err
	}
	st := c.style(io.out)

	if *seq >= 0 {
		it, idx, err := a.Index.Item(ctx, &s, *seq)
		if err != nil {
			return fmt.Errorf("%s has no step %d", s.Ref, *seq)
		}
		if c.json {
			return printJSON(io.out, map[string]any{"session": s.Ref, "turn": idx, "item": it})
		}
		render.Item(io.out, &s, idx, &it, st)
		return nil
	}

	r, some, err := app.TurnRange(*turns, turn)
	if err != nil {
		return usageError{err}
	}
	if !some {
		outline, err := a.Index.Outline(ctx, &s)
		if err != nil {
			return err
		}
		children, err := a.Index.Children(ctx, &s)
		if err != nil {
			return err
		}
		if c.json {
			return printJSON(io.out, map[string]any{"session": s, "turns": outline, "agents": app.Listed(children)})
		}
		render.Outline(io.out, &s, outline, children, st)
		return nil
	}
	r.Full = *full
	list, err := a.Index.Turns(ctx, &s, r)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return fmt.Errorf("%s has no such turn (it has %d)", s.Ref, s.Stats.Turns)
	}
	if c.json {
		return printJSON(io.out, map[string]any{"session": s, "turns": list})
	}
	for i := range list {
		if i > 0 {
			fmt.Fprintln(io.out)
		}
		render.Turn(io.out, &s, &list[i], render.TurnOptions{Full: *full}, st)
	}
	if *follow {
		return followSession(ctx, a, &s, list[len(list)-1], *full, st, io)
	}
	return nil
}

// followSession prints what a session gains after the turn already printed: each new
// step as the index reads it, a new turn's opening line when one starts, and a tool
// call a second time when the result it was waiting for arrives.
//
// How closely it follows is the index's patience, not this loop's: a small transcript is
// read again within a second or two of changing, a very large one less often.
func followSession(ctx context.Context, a *app.App, s *index.Session, last model.Turn, full bool, st render.Style, io *streams) error {
	opts := render.TurnOptions{Full: full}
	turn, seq := last.Index, -1
	waiting := map[int]bool{} // tool calls printed before their result
	note := func(items []model.Item) {
		for i := range items {
			it := &items[i]
			seq = max(seq, it.Seq)
			if it.Kind == model.ToolUse && it.Tool != nil && !it.Tool.Done {
				waiting[it.Seq] = true
			}
		}
	}
	note(last.Items)
	fmt.Fprintln(io.err, "following "+s.Ref+" — interrupt to stop")
	stamp := s.UpdatedAt
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
		if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue // a store mid-write: the next look will do
		}
		cur, err := a.Index.Resolve(ctx, s.Ref, a.Stores())
		if err != nil {
			return fmt.Errorf("%s is gone", s.Ref)
		}
		if cur.UpdatedAt.Equal(stamp) && cur.Stats.Items == s.Stats.Items {
			continue
		}
		stamp, *s = cur.UpdatedAt, cur
		turns, err := a.Index.Turns(ctx, s, index.TurnRange{First: turn, Last: -1, Full: full})
		if err != nil {
			return err
		}
		for i := range turns {
			t := &turns[i]
			if t.Index > turn {
				turn = t.Index
				fmt.Fprintln(io.out)
				render.TurnHead(io.out, s, t, st)
			}
			for j := range t.Items {
				it := &t.Items[j]
				switch {
				case it.Seq > seq:
					render.Step(io.out, it, opts, st)
				case waiting[it.Seq] && it.Tool != nil && it.Tool.Done:
					delete(waiting, it.Seq)
					render.Step(io.out, it, opts, st)
				}
			}
			note(t.Items)
		}
	}
}

func cmdBrief(ctx context.Context, args []string, io *streams) error {
	c := newCommand("brief", io.err)
	turn := c.fs.Int("turn", 0, "brief this turn (default: the latest)")
	since := c.fs.String("since", "", "brief every turn that ran after this: 2h, 2026-10-05T08:00")
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("which session?  asq brief <ref>")
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	s, named, err := resolve(ctx, a, rest[0])
	if err != nil {
		return err
	}
	opts := render.BriefOptions{Turn: *turn}
	if opts.Turn == 0 && named > 0 {
		opts.Turn = named
	}
	if opts.Since, err = app.When(*since, time.Now()); err != nil {
		return usageError{err}
	}
	text, err := a.Brief(ctx, &s, opts)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(io.out, map[string]any{"session": s, "brief": text})
	}
	fmt.Fprint(io.out, text)
	return nil
}

func cmdPack(ctx context.Context, args []string, io *streams) error {
	c := newCommand("pack", io.err).selection(20)
	withTurns := c.fs.Bool("turns", false, "spell out every turn of every session, not only how each ended")
	if _, err := c.parse(args); err != nil {
		return err
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	text, n, err := a.Pack(ctx, c.q, c.limit, *withTurns)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(io.out, map[string]any{"pack": text, "sessions": n})
	}
	fmt.Fprint(io.out, text)
	return nil
}

func cmdExport(ctx context.Context, args []string, io *streams) error {
	c := newCommand("export", io.err)
	format := c.fs.String("format", render.FormatMarkdown, "one of: "+strings.Join(render.Formats, ", "))
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("which session?  asq export <ref> [--format html] > file")
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	s, _, err := resolve(ctx, a, rest[0])
	if err != nil {
		return err
	}
	turns, err := a.Index.Turns(ctx, &s, index.TurnRange{First: 0, Last: -1, Full: true})
	if err != nil {
		return err
	}
	if c.json {
		*format = render.FormatJSON
	}
	text, err := render.Export(&s, turns, *format)
	if err != nil {
		return usageError{err}
	}
	fmt.Fprint(io.out, text)
	return nil
}

func cmdUsage(ctx context.Context, args []string, io *streams) error {
	c := newCommand("usage", io.err).selection(20)
	by := c.fs.String("by", "", "group by: model, day, source, project, session (default: a summary)")
	if _, err := c.parse(args); err != nil {
		return err
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	f, err := a.Filter(c.q)
	if err != nil {
		return usageError{err}
	}
	w, err := a.Window(c.q, "")
	if err != nil {
		return usageError{err}
	}
	st := c.style(io.out)
	if *by != "" {
		rows, err := a.Index.Usage(ctx, f, w, *by, c.limit)
		if err != nil {
			return usageError{err}
		}
		total, err := a.Index.UsageTotal(ctx, f, w)
		if err != nil {
			return err
		}
		if c.json {
			return printJSON(io.out, map[string]any{"by": *by, "rows": rows, "total": total})
		}
		render.Usage(io.out, "By "+*by, rows, &total, st)
		return nil
	}
	rep, err := a.Index.UsageSummary(ctx, f, w)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(io.out, rep)
	}
	render.UsageReport(io.out, rep, c.q.Since == "" && c.q.Until == "", st)
	return nil
}

func cmdFailures(ctx context.Context, args []string, io *streams) error {
	c := newCommand("failures", io.err).selection(20)
	tool := c.fs.String("tool", "", "only this tool's calls, named exactly (Bash) or by how the name begins (mcp__playwright*)")
	if _, err := c.parse(args); err != nil {
		return err
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	f, err := a.Filter(c.q)
	if err != nil {
		return usageError{err}
	}
	w, err := a.Window(c.q, "7d")
	if err != nil {
		return usageError{err}
	}
	rep, err := a.Failures(ctx, f, w, *tool, index.Page{Limit: c.limit, Offset: c.offset})
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(io.out, rep)
	}
	render.Failures(io.out, rep, c.style(io.out))
	return nil
}

func cmdDigest(ctx context.Context, args []string, io *streams) error {
	c := newCommand("digest", io.err).selection(0)
	if _, err := c.parse(args); err != nil {
		return err
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	f, err := a.Filter(c.q)
	if err != nil {
		return usageError{err}
	}
	w, err := a.Window(c.q, "7d")
	if err != nil {
		return usageError{err}
	}
	rep, err := a.Index.Digest(ctx, f, w)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(io.out, rep)
	}
	render.Digest(io.out, rep, c.style(io.out))
	return nil
}

func cmdFiles(ctx context.Context, args []string, io *streams) error {
	c := newCommand("files", io.err).selection(30)
	changed := c.fs.Bool("changed", false, "only the turns that changed it, not those that read it")
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("which file?  asq files <part of its path>")
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	f, err := a.Filter(c.q)
	if err != nil {
		return usageError{err}
	}
	w, err := a.Window(c.q, "")
	if err != nil {
		return usageError{err}
	}
	list, total, err := a.Files(ctx, f, w, rest[0], *changed, index.Page{Limit: c.limit, Offset: c.offset})
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(io.out, map[string]any{"files": list, "total": total})
	}
	render.Files(io.out, list, total, c.style(io.out))
	return nil
}

func cmdProjects(ctx context.Context, args []string, io *streams) error {
	c := newCommand("projects", io.err).selection(0)
	if _, err := c.parse(args); err != nil {
		return err
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	f, err := a.Filter(c.q)
	if err != nil {
		return usageError{err}
	}
	list, err := a.Index.Projects(ctx, f)
	if err != nil {
		return err
	}
	if c.limit > 0 && len(list) > c.limit {
		list = list[:c.limit]
	}
	if c.json {
		return printJSON(io.out, map[string]any{"projects": list})
	}
	render.Projects(io.out, list, c.style(io.out))
	return nil
}

func cmdResume(ctx context.Context, args []string, io *streams) error {
	c := newCommand("resume", io.err)
	run := c.fs.Bool("run", false, "run the command instead of printing it")
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("which session?  asq resume <ref>")
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	s, _, err := resolve(ctx, a, rest[0])
	a.Close()
	if err != nil {
		return err
	}
	if s.Resume == "" {
		if s.Parent != nil {
			return fmt.Errorf("%s is a subagent's transcript: resume the session it ran in, %s", s.Ref, s.Root)
		}
		return fmt.Errorf("%s cannot be resumed: %s", s.Ref, app.NoResume(&s))
	}
	if !*run {
		fmt.Fprintln(io.out, s.Resume)
		return nil
	}
	dir, argv := app.ResumeArgs(&s)
	return resume(dir, argv, s.Resume)
}

func cmdDoctor(ctx context.Context, args []string, io *streams) error {
	c := newCommand("doctor", io.err)
	if _, err := c.parse(args); err != nil {
		return err
	}
	cfg, err := c.config()
	if err != nil {
		return usageError{err}
	}
	set, err := catalog.New(cfg.Stores, catalog.Env())
	if err != nil {
		return usageError{err}
	}
	type check struct {
		Source   string `json:"source"`
		Location string `json:"location"`
		Found    bool   `json:"found"`
		Sessions int    `json:"transcripts"`
		Newest   string `json:"newest,omitempty"`
		Turns    int    `json:"turns"`
		Items    int    `json:"items"`
		Error    string `json:"error,omitempty"`
		// Unread is what the newest transcripts held that the adapter did not know how to
		// read, by kind: the sign that a CLI changed its format
		Unread map[string]int `json:"unread,omitempty"`
		// Archived is how many of the transcripts read were moved in part where they are
		// not read (OpenClaw's cold storage)
		Archived int `json:"archived,omitempty"`
	}
	st := c.style(io.out)
	var checks []check
	failed := false
	for _, store := range set.All() {
		ck := check{Source: store.Name, Location: store.Source.Location(), Found: store.Source.Exists()}
		if ck.Found {
			var lk look
			lk, err = deepRead(ctx, store)
			ck.Sessions, ck.Newest, ck.Turns, ck.Items, ck.Unread, ck.Archived = lk.sessions, lk.newest, lk.turns, lk.items, lk.unread, lk.archived
			if err != nil {
				ck.Error, failed = err.Error(), true
			}
		}
		checks = append(checks, ck)
	}
	if c.json {
		if err := printJSON(io.out, map[string]any{"version": app.Version, "stores": checks, "ok": !failed}); err != nil {
			return err
		}
	} else {
		// Which asq this is comes first: what a store reads as depends on it
		fmt.Fprintf(io.out, "asq %s\n", app.Version)
		for _, ck := range checks {
			switch {
			case !ck.Found:
				fmt.Fprintf(io.out, "· %-12s not found          %s\n", ck.Source, st.Path(ck.Location))
			case ck.Error != "":
				fmt.Fprintf(io.out, "✗ %-12s FAILED             %s\n    %s\n", ck.Source, st.Path(ck.Location), ck.Error)
			case ck.Newest == "" && ck.Archived > 0:
				fmt.Fprintf(io.out, "✓ %-12s %4d transcripts   the newest are in cold storage, where they are not read   %s\n", ck.Source, ck.Sessions, st.Path(ck.Location))
			default:
				fmt.Fprintf(io.out, "✓ %-12s %4d transcripts   newest read whole: %s, %s   %s\n", ck.Source, ck.Sessions, plural(ck.Turns, "turn"), plural(ck.Items, "step"), st.Path(ck.Location))
			}
			if ck.Archived > 0 && ck.Newest != "" {
				fmt.Fprintf(io.out, "  · %s of the newest read were moved in part to cold storage, where they are not read\n", plural(ck.Archived, "transcript"))
			}
			if len(ck.Unread) > 0 {
				fmt.Fprintf(io.out, "  %s %s\n", st.Warn("!"), unreadLine(ck.Unread))
			}
		}
	}
	if failed {
		return errFailed
	}
	return nil
}

// deepRead lists a store and reads its newest session whole. A list alone proves little:
// a format that drifted lists fine and reads as empty. It also reads the next newest, a
// few, for what they held that the adapter did not know: a format that drifted in part
// reads as less than it is, with nothing failing.
func deepRead(ctx context.Context, store index.Store) (look, error) {
	var lk look
	refs, err := store.Source.Scan(ctx)
	if err != nil {
		return lk, fmt.Errorf("listing: %w", err)
	}
	lk.sessions = len(refs)
	if len(refs) == 0 {
		return lk, nil
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ModTime.After(refs[j].ModTime) })
	// The newest that holds a conversation. A session opened and closed with nothing said
	// reads as empty and proves nothing, and a store can open with hundreds of those — a
	// gateway that starts a session for every scheduled run, most of which say nothing —
	// so the look goes on, newest first, for as long as a person would wait for it.
	//
	// What was not understood is what the conversations read held, and the empty sessions
	// newer than the first of them: a session that reads as empty because none of its
	// records were understood is how a format that drifted shows. An empty session older
	// than a conversation found is one of the many a gateway leaves, and is not counted.
	// Once one conversation is found, the next two are looked for among the next
	// doctorLook transcripts and no further.
	deadline := time.Now().Add(5 * time.Second)
	var firstErr error
	tried, read, past, stored := 0, 0, 0, 0 // stored: read as nothing but that it was moved
	for _, ref := range refs {
		if ctx.Err() != nil || (tried >= 5 && time.Now().After(deadline)) || read >= doctorReads || past >= doctorLook {
			break
		}
		tried++
		if read > 0 {
			past++
		}
		got, err := readOne(ctx, store.Source, ref)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("reading %s: %w", ref.Key, err)
			}
			continue
		}
		spoke, moved := said(got), has(got, model.EventColdStorage)
		if moved {
			lk.archived++
			if !spoke {
				stored++
			}
		}
		if spoke || read == 0 {
			for kind, n := range got.Unread {
				if lk.unread == nil {
					lk.unread = map[string]int{}
				}
				lk.unread[kind] += n
			}
		}
		if spoke {
			if read == 0 {
				lk.newest, lk.turns, lk.items = got.Session.ID, got.Stats.Turns, got.Stats.Items
			}
			read++
		}
	}
	switch {
	case read > 0:
		return lk, nil
	case firstErr != nil:
		return lk, firstErr
	case stored == tried:
		// Every one read was moved whole to where it is not read, and says so: that is why
		// they are empty, and not a format that drifted
		return lk, nil
	}
	return lk, fmt.Errorf("the %d newest sessions all read as empty: %s may have changed how it writes them", tried, store.Name)
}

// look is what doctor found of one store.
type look struct {
	sessions     int
	newest       string // the newest that holds a conversation; "" when none was found
	turns, items int    // of that one
	unread       map[string]int
	archived     int // of the sessions read, how many were moved in part where they are not read
}

// has reports whether a session read holds an event of that kind.
func has(got transcript.Collected, event string) bool {
	for _, turn := range got.Turns {
		for _, it := range turn.Items {
			if it.Kind == model.Event && it.Event == event {
				return true
			}
		}
	}
	return false
}

// said reports whether a session read holds a conversation: something asked, answered or
// done. Events alone do not make one — among them what an adapter says of the session
// itself, as that part of it was moved where it is not read (OpenClaw's cold storage):
// a session that says only that proves nothing of how its CLI writes.
func said(got transcript.Collected) bool {
	for _, turn := range got.Turns {
		for _, it := range turn.Items {
			if it.Kind != model.Event {
				return true
			}
		}
	}
	return false
}

// doctorReads is how many of a store's newest conversations doctor reads for what they
// held that was not understood.
const doctorReads = 3

// doctorLook is how many transcripts past the newest conversation doctor looks through
// for the others.
const doctorLook = 10

// unreadLine says what was not read, the most frequent first.
func unreadLine(unread map[string]int) string {
	kinds := make([]string, 0, len(unread))
	for k := range unread {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if unread[kinds[i]] != unread[kinds[j]] {
			return unread[kinds[i]] > unread[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%s ×%d", k, unread[k]))
	}
	return "not understood in the newest transcripts, and passed over: " + strings.Join(parts, ", ") +
		" — the CLI may have changed how it writes them; what is shown is what was understood"
}

func readOne(ctx context.Context, src sources.Source, ref sources.Ref) (transcript.Collected, error) {
	col := &transcript.Collector{}
	w := transcript.New(col)
	if err := src.Read(ctx, ref, w); err != nil {
		return transcript.Collected{}, err
	}
	stats, err := w.Finish()
	return transcript.Collected{Session: w.S, Stats: stats, Turns: col.Turns, Unread: w.Unreads()}, err
}

func cmdIndex(ctx context.Context, args []string, io *streams) error {
	c := newCommand("index", io.err)
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	cfg, err := c.config()
	if err != nil {
		return usageError{err}
	}
	path := cfg.IndexPath
	if path == "" {
		path = index.DefaultPath()
	}
	action := ""
	if len(rest) > 0 {
		action = rest[0]
	}
	switch action {
	case "path":
		fmt.Fprintln(io.out, path)
		return nil
	case "rm":
		if err := index.Remove(path); err != nil {
			return err
		}
		fmt.Fprintln(io.out, "removed", path, "— it is rebuilt from the stores on the next run")
		return nil
	case "rebuild":
		if err := index.Remove(path); err != nil {
			return err
		}
	case "":
	default:
		return usagef("asq index [path | rebuild | rm]")
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	h, err := a.Health(ctx)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(io.out, h)
	}
	sessions := plural(h.Totals.Sessions, "session")
	if h.Totals.Archived > 0 {
		sessions += fmt.Sprintf(" (%d archived)", h.Totals.Archived)
	}
	fmt.Fprintf(io.out, "%s\n  %s · %s · %s · %s · last brought up to date in %s\n", path,
		sessions, plural(h.Totals.Agents, "subagent transcript"), plural(h.Totals.Turns, "turn"),
		strconv.FormatFloat(float64(h.Totals.Bytes)/(1<<20), 'f', 1, 64)+" MB", txt.Duration(time.Duration(h.Sync.Took)*time.Millisecond))
	for _, s := range h.Stores {
		if !s.Found {
			fmt.Fprintf(io.out, "  · %-12s not found      %s\n", s.Source, s.Location)
			continue
		}
		fmt.Fprintf(io.out, "  ✓ %-12s %5d transcripts   %s\n", s.Source, s.Transcripts, s.Location)
	}
	return nil
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
