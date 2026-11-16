package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
	c.fs.StringVar(&c.q.Project, "project", "", "sessions whose working directory contains this")
	c.fs.StringVar(&c.q.Branch, "branch", "", "sessions that opened on a branch containing this")
	c.fs.StringVar(&c.q.Since, "since", "", "after this: 7d, 12h, 2026-09-01")
	c.fs.StringVar(&c.q.Until, "until", "", "before this")
	c.fs.BoolVar(&c.q.Archived, "archived", false, "include the sessions their CLI has put away")
	c.fs.BoolVar(&c.q.Agents, "agents", false, "include subagents' transcripts as sessions of their own")
	c.fs.IntVar(&c.limit, "limit", defaultLimit, "how many to show")
	c.fs.IntVar(&c.limit, "n", defaultLimit, "how many to show (short for --limit)")
	c.fs.IntVar(&c.offset, "offset", 0, "skip this many first")
	return c
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
	return rest, nil
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
	return app.Config{IndexPath: c.indexPath, Stores: stores}, nil
}

// style is how text is dressed for where it is going.
func (c *command) style(out io.Writer) render.Style {
	st := render.Style{Home: os.Getenv("HOME")}
	if f, ok := out.(*os.File); ok && !c.noColor && os.Getenv("NO_COLOR") == "" {
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
