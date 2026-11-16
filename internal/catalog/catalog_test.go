package catalog

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources"
)

func TestEverySpecIsWhole(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range All {
		if spec.Name == "" || spec.Title == "" || spec.Locate == nil || spec.Open == nil || spec.Store == "" {
			t.Errorf("%q: a spec says its name, its title, where its store is, how to open it and how it is laid out: %+v", spec.Name, spec)
		}
		if seen[spec.Name] {
			t.Errorf("%q is listed twice", spec.Name)
		}
		seen[spec.Name] = true
		if strings.ToLower(spec.Name) != spec.Name || strings.ContainsAny(spec.Name, "@/= ") {
			t.Errorf("%q: a name is what --source takes and what a ref begins with", spec.Name)
		}
		// Every store is found from the home directory alone, and opening one reads nothing
		home := t.TempDir()
		where := spec.Locate(sources.Env{Home: home, Getenv: func(string) string { return "" }})
		if !strings.HasPrefix(where, home) {
			t.Errorf("%s looks for its store outside the home it was given: %s", spec.Name, where)
		}
		src := spec.Open(where)
		if src.Exists() {
			t.Errorf("%s reports a store in an empty home", spec.Name)
		}
		if refs, _ := src.Scan(t.Context()); len(refs) != 0 {
			t.Errorf("%s lists %d sessions in an empty home", spec.Name, len(refs))
		}
	}
	if len(All) != 10 {
		t.Errorf("%d CLIs", len(All))
	}
}

func TestParsePath(t *testing.T) {
	for in, want := range map[string]Path{
		"claude=/mnt/projects":     {Agent: "claude", Dir: "/mnt/projects"},
		"claude@box2=/mnt/box2":    {Agent: "claude", Label: "box2", Dir: "/mnt/box2"},
		"codex@old-mac=/a=b/c":     {Agent: "codex", Label: "old-mac", Dir: "/a=b/c"}, // only the first = divides
		`pi=C:\Users\me\.pi\agent`: {Agent: "pi", Dir: `C:\Users\me\.pi\agent`},
	} {
		got, err := ParsePath(in)
		if err != nil || got != want {
			t.Errorf("ParsePath(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"claude", "claude=", "emacs=/x", "claude@=/x", "claude@Box 2=/x", "=/x"} {
		if _, err := ParsePath(bad); err == nil {
			t.Errorf("ParsePath(%q) is accepted", bad)
		}
	}
	for _, spec := range All {
		_, err := ParsePath(spec.Name + "=/somewhere")
		if spec.Movable != (err == nil) {
			t.Errorf("%s: movable=%v but --path says %v", spec.Name, spec.Movable, err)
		}
	}
}

func TestConfiguration(t *testing.T) {
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }
	cfg, err := FromEnv(env(map[string]string{"ASQ_ONLY": "claude, codex", "ASQ_PATH": "claude@box2=/mnt/box2;codex=/srv/codex"}))
	if err != nil || len(cfg.Only) != 2 || len(cfg.Paths) != 2 {
		t.Fatalf("FromEnv = %+v, %v", cfg, err)
	}
	set, err := New(cfg, sources.Env{Home: "/home/me", Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(set.Names(), " "); got != "claude codex claude@box2" {
		t.Errorf("stores = %q: the two asked for, and the labeled one beside them", got)
	}
	where := map[string]string{}
	for _, st := range set.All() {
		where[st.Name] = st.Source.Location()
	}
	if where["claude"] != filepath.Join("/home/me", ".claude", "projects") || where["codex"] != "/srv/codex" || where["claude@box2"] != "/mnt/box2" {
		t.Errorf("locations = %v", where)
	}
	if len(set.Present()) != 0 {
		t.Error("stores that are not there are listed as present")
	}
	if spec, ok := Spec("claude@box2"); !ok || spec.Name != "claude" {
		t.Errorf("a labeled store is its CLI's: %+v, %v", spec, ok)
	}

	for name, vars := range map[string]map[string]string{
		"an unknown CLI":      {"ASQ_ONLY": "claude,emacs"},
		"a path with no dir":  {"ASQ_PATH": "claude"},
		"the same path twice": {"ASQ_PATH": "claude=/a,claude=/b"},
	} {
		if _, err := FromEnv(env(vars)); err == nil {
			t.Errorf("%s is accepted", name)
		}
	}
	if cfg, err := FromEnv(env(nil)); err != nil || len(cfg.Only) != 0 || len(cfg.Paths) != 0 {
		t.Errorf("nothing set = %+v, %v", cfg, err)
	}
}
