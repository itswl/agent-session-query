package cli

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	goio "io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/server"
	"github.com/itswl/agent-session-query/internal/service"
)

// newMachine is the machine asq service sets the service up on; a test gives it another.
var newMachine = service.ThisMachine

// cmdService keeps asq serve running under this system's service manager — launchd,
// systemd or Task Scheduler — by the definition each reads, written with this binary's
// full path and the environment asq reads in this shell, so that the service serves what
// asq serve here would.
func cmdService(ctx context.Context, args []string, io *streams) error {
	c := newCommand("service", io.err)
	host := c.fs.String("host", "", "install: the address it listens on (default ASQ_HOST, or 127.0.0.1); anything but loopback needs ASQ_TOKEN")
	port := c.fs.Int("port", 0, "install: the port it listens on (default ASQ_PORT, or 7717)")
	proxyHeader := c.fs.String("proxy-header", "", "install: the header in which a proxy in front names the caller (takes ASQ_TOKEN)")
	var origins multi
	c.fs.Var(&origins, "cors-origin", "install: another origin whose pages may call the API (repeatable)")
	force := c.fs.Bool("force", false, "install: replace a service for asq that asq did not write; uninstall: remove one")
	dryRun := c.fs.Bool("dry-run", false, "install: print what would be written and run, and do neither")
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	action := "status"
	if len(rest) > 0 {
		action = rest[0]
	}
	if len(rest) > 1 || action != "install" && action != "status" && action != "uninstall" {
		return usagef("asq service [install | status | uninstall]")
	}
	var refused error
	c.fs.Visit(func(f *flag.Flag) {
		switch {
		case f.Name == "json" || f.Name == "no-color":
		case f.Name == "force" && action != "status":
		case f.Name == "force" && refused == nil:
			refused = usagef("--force is for asq service install and uninstall")
		case action != "install" && refused == nil:
			refused = usagef("--%s is for asq service install", f.Name)
		}
	})
	if refused != nil {
		return refused
	}
	m := newMachine(DefaultPort)
	self := thisBinary()

	switch action {
	case "status":
		st, err := m.Status(self)
		if err != nil {
			return err
		}
		if c.json {
			return printJSON(io.out, st)
		}
		printStatus(io.out, st)
		return nil
	case "uninstall":
		gone, err := m.Uninstall(*force)
		if err != nil {
			return err
		}
		if c.json {
			return printJSON(io.out, gone)
		}
		if !gone.Removed {
			fmt.Fprintf(io.out, "%s: no service for asq to remove\n", gone.Manager)
			return nil
		}
		fmt.Fprintf(io.out, "%s: removed %s\n", gone.Manager, gone.Definition)
		for _, cmd := range gone.Ran {
			fmt.Fprintf(io.out, "  ran      %s\n", cmd)
		}
		return nil
	}

	// install: serve with the flags given here, and the environment asq reads in this shell
	cfg, err := c.config()
	if err != nil {
		return usageError{err}
	}
	if _, err := catalog.New(cfg.Stores, catalog.Env()); err != nil {
		return usageError{err}
	}
	s := service.Setup{Binary: self, Args: []string{"serve"}, Env: serviceEnv(os.Getenv)}
	flagArg := func(name, value string) {
		if value != "" {
			s.Args = append(s.Args, "--"+name, value)
		}
	}
	flagArg("host", *host)
	if *port != 0 {
		flagArg("port", strconv.Itoa(*port))
	}
	flagArg("proxy-header", *proxyHeader)
	for _, o := range origins {
		flagArg("cors-origin", o)
	}
	indexPath := index.DefaultPath()
	if c.indexPath != "" {
		if indexPath, err = filepath.Abs(c.indexPath); err != nil {
			return err
		}
		flagArg("index", indexPath)
	}
	flagArg("only", c.only)
	for _, p := range c.paths {
		// A store named from here is named from wherever the service starts: absolute
		name, dir, _ := strings.Cut(p, "=")
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		flagArg("path", name+"="+dir)
	}
	s.Writable = []string{filepath.Dir(indexPath), app.StateDir()}

	// What asq serve would refuse, refused before there is a service that cannot start
	listen := cmp.Or(*host, os.Getenv("ASQ_HOST"), "127.0.0.1")
	if os.Getenv("ASQ_TOKEN") == "" && !server.IsLoopback(listen) {
		return usagef("listening on %s needs a token, and asq serve would refuse to start without one: set ASQ_TOKEN in this shell — install writes it into the service's definition, its owner's alone", listen)
	}

	if *dryRun {
		p, err := m.Plan(s)
		if err != nil {
			return err
		}
		if c.json {
			return printJSON(io.out, p)
		}
		fmt.Fprintf(io.out, "%s: would write %s\n\n%s\nand run\n", p.Manager, p.Definition, p.Content)
		for _, cmd := range p.Commands {
			fmt.Fprintf(io.out, "  %s\n", cmd)
		}
		for _, note := range p.Notes {
			fmt.Fprintf(io.out, "· %s\n", note)
		}
		return nil
	}
	done, err := m.Install(s, *force)
	if err != nil && done == nil {
		return err
	}
	if c.json {
		if jerr := printJSON(io.out, done); jerr != nil || err != nil {
			return cmp.Or(err, jerr)
		}
		if !done.Answers {
			return errFailed
		}
		return nil
	}
	verb := "wrote"
	if done.Manager == "Task Scheduler" {
		verb = "registered"
	}
	fmt.Fprintf(io.out, "%s: %s %s\n", done.Manager, verb, done.Definition)
	fmt.Fprintf(io.out, "  runs     %s\n", commandLine(done.Command))
	if len(done.Env) > 0 {
		fmt.Fprintf(io.out, "  with     %s\n", strings.Join(done.Env, ", "))
	}
	for _, cmd := range done.Ran {
		fmt.Fprintf(io.out, "  ran      %s\n", cmd)
	}
	if err != nil {
		return err
	}
	if done.Answers {
		fmt.Fprintf(io.out, "  answers  %s\n", done.Address)
	} else {
		fmt.Fprintf(io.out, "  ✗ nothing answers at %s\n", done.Address)
		for _, line := range done.Said {
			fmt.Fprintf(io.out, "    %s\n", line)
		}
	}
	if done.Log != "" {
		fmt.Fprintf(io.out, "  log      %s\n", done.Log)
	}
	for _, note := range done.Notes {
		fmt.Fprintf(io.out, "· %s\n", note)
	}
	if !done.Answers {
		return errFailed
	}
	return nil
}

func printStatus(out goio.Writer, st *service.Status) {
	if !st.SetUp {
		fmt.Fprintf(out, "%s: no service for asq — asq service install sets one up\n", st.Manager)
		if st.Loaded {
			fmt.Fprintf(out, "  state    %s, though its definition is gone\n", st.State)
		}
	} else {
		whose := "written by asq service install"
		if !st.Ours {
			whose = "set up by hand: asq service install --force replaces it"
		}
		fmt.Fprintf(out, "%s: %s, %s\n", st.Manager, st.Definition, whose)
		runs := commandLine(st.Command)
		if st.Missing {
			runs += "  ✗ not there: removed, or moved — asq service install writes it anew"
		}
		fmt.Fprintf(out, "  runs     %s\n", runs)
		if len(st.Env) > 0 {
			fmt.Fprintf(out, "  with     %s\n", strings.Join(st.Env, ", "))
		}
		state := "not loaded"
		if st.Loaded {
			state = st.State
			if st.PID > 0 {
				state += ", pid " + strconv.Itoa(st.PID)
			}
		}
		fmt.Fprintf(out, "  state    %s\n", state)
		if st.Answers {
			fmt.Fprintf(out, "  answers  %s\n", st.Address)
		} else {
			fmt.Fprintf(out, "  answers  no — nothing at %s\n", st.Address)
		}
		if st.Log != "" {
			fmt.Fprintf(out, "  log      %s\n", st.Log)
		}
		if len(st.Command) > 0 && st.This != "" && st.Command[0] != st.This && !st.Missing {
			fmt.Fprintf(out, "· it runs another asq than this one, %s: asq service install has it run this one\n", st.This)
		}
		if len(st.Down) > 0 {
			fmt.Fprintf(out, "· to take it down by hand: %s\n", strings.Join(st.Down, "; "))
		}
	}
	for _, other := range st.Others {
		fmt.Fprintf(out, "· %s\n", other)
	}
}

// thisBinary is this asq by the path a service should run it by: the path it was started
// by, or — where that is, or leads to, a version's own directory — the one its package
// manager keeps from version to version.
func thisBinary() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	return service.Stable(self)
}

// serviceEnv is the environment asq serve reads, as this shell has it: asq's own
// variables, those Go finds the cache, config and state directories by, the zone a
// report's days are counted in, and each CLI's own for moving its store — which a service
// does not get from a shell.
func serviceEnv(getenv func(string) string) []service.Var {
	names := []string{"ASQ_HOST", "ASQ_PORT", "ASQ_TOKEN", "ASQ_CORS_ORIGIN", "ASQ_PROXY_HEADER",
		"ASQ_INDEX", "ASQ_STATE", "ASQ_ONLY", "ASQ_PATH",
		"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "TZ"}
	// Which variables a CLI's store moves by is the adapter's to know: it is asked
	env := catalog.Env()
	env.Getenv = func(name string) string {
		names = append(names, name)
		return getenv(name)
	}
	for _, spec := range catalog.All {
		spec.Locate(env)
	}
	var vars []service.Var
	seen := map[string]bool{}
	for _, name := range names {
		if v := getenv(name); v != "" && !seen[name] {
			vars = append(vars, service.Var{Name: name, Value: v})
		}
		seen[name] = true
	}
	return vars
}

// commandLine is a command as a person would type it.
func commandLine(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = app.ShellArg(a)
	}
	return strings.Join(words, " ")
}
