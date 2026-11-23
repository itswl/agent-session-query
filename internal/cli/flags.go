package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/render"
)

// command is one invocation's flags: the ones every command takes, and its own.
type command struct {
	fs   *flag.FlagSet
	name string
	help io.Writer

	// where the index is and which stores to read
	indexPath string
	only      string
	paths     multi

	// what the answer looks like
	json    bool
	noColor bool

	// which sessions
	q      app.Query
	limit  int
	offset int
	// here is the directory a --project of "." or "../api" was taken to be, absolute
	here string

	// maxChars holds a text answer to about that many characters (--max-chars); 0, no bound
	maxChars int
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func newCommand(name string, stderr io.Writer) *command {
	c := &command{name: name, fs: flag.NewFlagSet("asq "+name, flag.ContinueOnError)}
	c.fs.SetOutput(io.Discard) // what goes wrong with a flag is said once, by Run
	c.fs.Usage = func() {}
	c.help = stderr
	c.fs.StringVar(&c.indexPath, "index", "", "the index file (default: the user cache directory; ASQ_INDEX)")
	c.fs.StringVar(&c.only, "only", "", "read only these CLIs' stores, comma-separated (ASQ_ONLY)")
	c.fs.Var(&c.paths, "path", "a store outside its default place: name=dir relocates, name@label=dir adds one (repeatable; ASQ_PATH)")
	c.fs.BoolVar(&c.json, "json", false, "print JSON instead of text")
	c.fs.BoolVar(&c.noColor, "no-color", false, "plain text, even on a terminal")
	return c
}

// selection adds the flags that pick sessions.
func (c *command) selection(defaultLimit int) *command {
	c.fs.StringVar(&c.q.Source, "source", "", "one store, or one CLI's stores: "+strings.Join(catalog.Names(), ", "))
	c.fs.StringVar(&c.q.Project, "project", "", "sessions whose working directory contains this; ., .. or ./web: in that directory or below it")
	c.fs.StringVar(&c.q.Branch, "branch", "", "sessions that opened on a branch containing this")
	c.fs.StringVar(&c.q.Since, "since", "", "after this: 7d, 12h, today, monday, 2026-09-01")
	c.fs.StringVar(&c.q.Until, "until", "", "before this")
	c.fs.BoolVar(&c.q.Archived, "archived", false, "include the sessions their CLI has put away")
	c.fs.BoolVar(&c.q.Agents, "agents", false, "include subagents' transcripts as sessions of their own")
	c.fs.IntVar(&c.limit, "limit", defaultLimit, "how many to show")
	c.fs.IntVar(&c.limit, "n", defaultLimit, "how many to show (short for --limit)")
	c.fs.IntVar(&c.offset, "offset", 0, "skip this many first")
	return c
}

// budgeted adds --max-chars, which holds an answer to about that many characters, as an MCP
// tool's answer is held: for an agent that reads asq from a shell, and pays for what it
// reads by the token.
func (c *command) budgeted() *command {
	c.fs.IntVar(&c.maxChars, "max-chars", 0, "hold the answer to about this many characters (at least 500), as the MCP tools do; text only")
	return c
}

// held writes text held to the command's budget: cut on a line boundary, saying how much
// more there was and what to ask for instead.
func (c *command) held(w io.Writer, text string) { c.heldTo(w, text, c.maxChars) }

// heldTo is held, to a budget of its own.
func (c *command) heldTo(w io.Writer, text string, budget int) {
	cut := render.Cut(text, budget, "Ask for less — fewer --turns, a smaller --limit, a narrower query — or raise --max-chars.")
	if len(cut) != len(text) {
		cut += "\n"
	}
	io.WriteString(w, cut)
}

// parse reads flags wherever they stand among the arguments — `asq search retry --since
// 7d` and `asq search --since 7d retry` are the same command — and returns the rest.
func (c *command) parse(args []string) ([]string, error) {
	var rest []string
	for len(args) > 0 {
		if err := c.fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				c.usage()
				return nil, err
			}
			return nil, usagef("%v  (asq %s -h lists its flags)", err, c.name)
		}
		args = c.fs.Args()
		if len(args) == 0 {
			break
		}
		rest = append(rest, args[0])
		args = args[1:]
	}
	switch {
	case c.maxChars < 0:
		return nil, usagef("--max-chars wants a number of characters, got %d", c.maxChars)
	case c.maxChars > 0 && c.json:
		return nil, usagef("--max-chars holds text to a budget: it goes without --json")
	case c.maxChars > 0:
		c.maxChars = max(c.maxChars, 500)
	}
	// A project written as a path from the current directory is that directory, and
	// not a part of a path to look for: "." is in every path
	if app.Relative(c.q.Project) {
		dir, names, err := here(c.q.Project)
		if err != nil {
			return nil, fmt.Errorf("--project %s: %w", c.q.Project, err)
		}
		c.here, c.q.Project, c.q.Dirs = dir, "", names
	}
	return rest, nil
}

// here is a directory named from the current one, absolute, and the paths that name it:
// that one, as the shell names it, and the same with its symbolic links resolved when
// that is another — a CLI may have recorded either as a session's working directory.
// The paths a CLI recorded through a link to it are found once the index is open
// (app.Paths): given the directory with its links resolved, nothing says which lead there.
func here(name string) (dir string, names []string, err error) {
	if dir, err = filepath.Abs(name); err != nil {
		return "", nil, err
	}
	names = []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		names = append(names, real)
	}
	return dir, names, nil
}

// lookedIn says, beside an answer that is empty, which directory a --project of "." was
// taken to be: it is resolved where asq runs, which is not always where the reader thinks.
func (c *command) lookedIn(stderr io.Writer, empty bool) {
	if empty && c.here != "" {
		fmt.Fprintf(stderr, "asq: looked in %s and the directories below it\n", c.here)
	}
}

// usage lists a command's flags the way they are typed: two dashes, the value it takes,
// and what it is for.
func (c *command) usage() {
	fmt.Fprintf(c.help, "asq %s [flags]\n\n", c.name)
	c.fs.VisitAll(func(f *flag.Flag) {
		name := "--" + f.Name
		if len(f.Name) == 1 {
			name = "-" + f.Name
		}
		kind, about := flag.UnquoteUsage(f)
		if kind != "" {
			name += " " + kind
		}
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" && f.DefValue != "0s" {
			about += " (default " + f.DefValue + ")"
		}
		fmt.Fprintf(c.help, "  %-22s %s\n", name, about)
	})
}

// config is where the index is and which stores to read: the environment every asq
// process on the machine shares, then this command's own flags.
func (c *command) config() (app.Config, error) {
	stores, err := catalog.FromEnv(os.Getenv)
	if err != nil {
		return app.Config{}, err
	}
	if c.only != "" {
		stores.Only = nil
		for _, name := range strings.Split(c.only, ",") {
			if name = strings.TrimSpace(name); name != "" {
				stores.Only = append(stores.Only, name)
			}
		}
	}
	for _, value := range c.paths {
		p, err := catalog.ParsePath(value)
		if err != nil {
			return app.Config{}, err
		}
		stores.Paths = append(stores.Paths, p)
	}
	state := app.StateDir()
	stores.Kept = app.KeptFolder(state)
	return app.Config{IndexPath: c.indexPath, Stores: stores, State: state}, stores.Validate()
}

// style is how text is dressed for where it is going.
func (c *command) style(out io.Writer) render.Style {
	st := render.Style{Home: os.Getenv("HOME")}
	// What is held to a budget is counted in characters, and colour is characters too
	if f, ok := out.(*os.File); ok && !c.noColor && c.maxChars == 0 && os.Getenv("NO_COLOR") == "" {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			st.Color = true
			st.Width = terminalWidth()
		}
	}
	return st
}

func terminalWidth() int {
	if cols := os.Getenv("COLUMNS"); cols != "" {
		var n int
		if _, err := fmt.Sscanf(cols, "%d", &n); err == nil && n > 20 {
			return n
		}
	}
	return 120
}
