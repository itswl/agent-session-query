// Package catalog is the list of agent CLIs the program reads, and the decision of which
// of their stores to read on this machine.
//
// Adding a CLI is one adapter package and one line in All.
package catalog

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/antigravity"
	"github.com/itswl/agent-session-query/internal/sources/claude"
	"github.com/itswl/agent-session-query/internal/sources/codex"
	"github.com/itswl/agent-session-query/internal/sources/dsh"
	"github.com/itswl/agent-session-query/internal/sources/gemini"
	"github.com/itswl/agent-session-query/internal/sources/grok"
	"github.com/itswl/agent-session-query/internal/sources/hermes"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/sources/openclaw"
	"github.com/itswl/agent-session-query/internal/sources/opencode"
	"github.com/itswl/agent-session-query/internal/sources/pi"
)

// All is every CLI the program can read, in the order they are listed.
var All = []sources.Spec{
	claude.Spec,
	codex.Spec,
	gemini.Spec,
	opencode.Spec,
	grok.Spec,
	antigravity.Spec,
	pi.Spec,
	dsh.Spec,
	hermes.Spec,
	openclaw.Spec,
}

// Find returns the spec of the CLI with that name.
func Find(name string) (sources.Spec, bool) {
	for _, spec := range All {
		if spec.Name == name {
			return spec, true
		}
	}
	return sources.Spec{}, false
}

// Names lists the CLIs' names.
func Names() []string {
	out := make([]string, 0, len(All))
	for _, spec := range All {
		out = append(out, spec.Name)
	}
	return out
}

// Path is one store outside its CLI's default location: `claude=/mnt/disk/projects`
// relocates Claude Code's store, and `claude@box2=/mnt/box2/projects` adds a second one
// beside it, whose sessions carry the source "claude@box2".
type Path struct {
	Agent string // the CLI
	Label string // empty: relocate; set: an instance of its own
	Dir   string
}

// ParsePath reads `name[@label]=directory`.
func ParsePath(value string) (Path, error) {
	spec, dir, ok := strings.Cut(value, "=")
	if !ok || dir == "" {
		return Path{}, fmt.Errorf("--path %q: want name[@label]=directory", value)
	}
	agent, label, labeled := strings.Cut(spec, "@")
	if labeled && !validLabel(label) {
		return Path{}, fmt.Errorf("--path %q: a label is lowercase letters, digits and dashes", value)
	}
	s, known := Find(agent)
	if !known {
		return Path{}, fmt.Errorf("--path %q: unknown source %q (one of: %s)", value, agent, strings.Join(Names(), ", "))
	}
	if !s.Movable {
		return Path{}, fmt.Errorf("--path %q: %s keeps its sessions across a whole data directory and cannot be pointed at this way", value, agent)
	}
	return Path{Agent: agent, Label: label, Dir: dir}, nil
}

func validLabel(label string) bool {
	if label == "" {
		return false
	}
	for _, r := range label {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// Config says which stores to read.
type Config struct {
	// Only keeps these CLIs; empty reads every one whose store exists.
	Only []string
	// Paths are the stores outside their default location.
	Paths []Path
}

// FromEnv reads the configuration every asq process on a machine shares: ASQ_ONLY, a
// comma-separated list of CLIs, and ASQ_PATH, comma-separated `name[@label]=directory`
// entries. A page server, an MCP server started by some agent and a command typed in a
// terminal then all read the same stores without being told the same flags.
func FromEnv(getenv func(string) string) (Config, error) {
	var cfg Config
	cfg.Only = splitList(getenv("ASQ_ONLY"))
	for _, entry := range splitList(getenv("ASQ_PATH")) {
		p, err := ParsePath(entry)
		if err != nil {
			return cfg, fmt.Errorf("ASQ_PATH: %w", err)
		}
		cfg.Paths = append(cfg.Paths, p)
	}
	return cfg, cfg.validate()
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (c Config) validate() error {
	for _, name := range c.Only {
		if _, ok := Find(name); !ok {
			return fmt.Errorf("unknown source %q (one of: %s)", name, strings.Join(Names(), ", "))
		}
	}
	seen := map[string]bool{}
	for _, p := range c.Paths {
		key := p.Agent + "@" + p.Label
		if seen[key] {
			return fmt.Errorf("--path: %s given twice", strings.TrimSuffix(key, "@"))
		}
		seen[key] = true
	}
	return nil
}

// Set is the stores a configuration names. Each adapter is built once and kept: an
// adapter remembers what it has read of its store, and that must outlive one scan.
type Set struct {
	mu     sync.Mutex
	stores []index.Store // every configured store, present or not
}

// Env is the real environment: the user's home and the process's variables.
func Env() sources.Env { return sources.Env{Home: kit.Home(), Getenv: os.Getenv} }

// New builds the set of stores a configuration names.
func New(cfg Config, env sources.Env) (*Set, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	only := map[string]bool{}
	for _, name := range cfg.Only {
		only[name] = true
	}
	moved := map[string]string{}
	for _, p := range cfg.Paths {
		if p.Label == "" {
			moved[p.Agent] = p.Dir
		}
	}
	set := &Set{}
	for _, spec := range All {
		if len(only) > 0 && !only[spec.Name] {
			continue
		}
		location := spec.Locate(env)
		if dir, ok := moved[spec.Name]; ok {
			location = dir
		}
		set.stores = append(set.stores, index.Store{Name: spec.Name, Spec: spec, Source: spec.Open(location)})
	}
	// A labeled store is asked for by name: it is read whatever Only says
	for _, p := range cfg.Paths {
		if p.Label == "" {
			continue
		}
		spec, _ := Find(p.Agent)
		set.stores = append(set.stores, index.Store{Name: p.Agent + "@" + p.Label, Spec: spec, Source: spec.Open(p.Dir)})
	}
	return set, nil
}

// All lists every configured store, whether or not it is there.
func (s *Set) All() []index.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]index.Store(nil), s.stores...)
}

// Present lists the stores that exist right now. It is asked again before every sync: a
// CLI that had never been run when the program started creates its store the first time
// it is used, and its sessions should appear without a restart.
func (s *Set) Present() []index.Store {
	var out []index.Store
	for _, st := range s.All() {
		if st.Source.Exists() {
			out = append(out, st)
		}
	}
	return out
}

// Names lists the configured stores' names.
func (s *Set) Names() []string {
	all := s.All()
	out := make([]string, 0, len(all))
	for _, st := range all {
		out = append(out, st.Name)
	}
	return out
}

// Spec returns the CLI spec behind a store name ("claude@box2" → Claude Code's).
func Spec(source string) (sources.Spec, bool) {
	agent, _, _ := strings.Cut(source, "@")
	return Find(agent)
}
