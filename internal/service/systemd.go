package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// systemd keeps a user unit: a file in the user's ~/.config/systemd/user, run by the
// user's own systemd as them.
type systemd struct{ m *Machine }

func (u *systemd) name() string  { return "systemd" }
func (u *systemd) where() string { return filepath.Join(u.m.Config, "systemd", "user", Unit) }
func (u *systemd) log() string   { return "journalctl --user -u asq" }

// render writes what docs/deploy.md has a person write by hand. The home directory is
// mounted read-only for the service: it reads the stores, and writes only its index and
// its state, whose directories must be there for systemd to start it.
func (u *systemd) render(s Setup) []byte {
	var b strings.Builder
	b.WriteString("# " + Mark + ": asq service uninstall removes it, and asq service install writes it anew\n")
	b.WriteString("[Unit]\nDescription=asq — agent session history\nAfter=network.target\n\n[Service]\n")
	words := []string{unitWord(s.Binary, true)}
	for _, a := range s.Args {
		words = append(words, unitWord(a, true))
	}
	b.WriteString("ExecStart=" + strings.Join(words, " ") + "\n")
	for _, v := range s.Env {
		b.WriteString("Environment=" + unitWord(v.Name+"="+v.Value, false) + "\n")
	}
	b.WriteString("Restart=on-failure\nNoNewPrivileges=true\nProtectSystem=strict\nProtectHome=read-only\n")
	for _, dir := range s.Writable {
		b.WriteString("ReadWritePaths=" + unitWord(dir, false) + "\n")
	}
	b.WriteString("PrivateTmp=true\n\n[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

// unitWord is one word of a unit's setting. % starts a specifier, $ a variable in
// ExecStart; a space, a quote or a backslash needs the word quoted.
func unitWord(s string, dollars bool) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if dollars {
		s = strings.ReplaceAll(s, "$", "$$")
	}
	if s != "" && !strings.ContainsAny(s, " \t\"'\\;") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// parse reads back what a unit runs: ExecStart, and the variables Environment and
// EnvironmentFile set — the one docs/deploy.md writes by hand keeps them in a file.
func (u *systemd) parse(def []byte) Setup {
	var s Setup
	section := ""
	text := strings.ReplaceAll(string(def), "\\\n", " ")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			section = line
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section != "[Service]" {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ExecStart":
			words := unitWords(value, u.m.Home, true)
			if len(words) > 0 {
				words[0] = strings.TrimLeft(words[0], "-@:+!")
				s.Binary, s.Args = words[0], words[1:]
			}
		case "Environment":
			for _, w := range unitWords(value, u.m.Home, false) {
				if name, v, ok := strings.Cut(w, "="); ok {
					s.Env = append(s.Env, Var{name, v})
				}
			}
		case "EnvironmentFile":
			path := unitWords(strings.TrimPrefix(strings.TrimSpace(value), "-"), u.m.Home, false)
			if len(path) == 1 {
				s.Env = append(s.Env, envFile(path[0])...)
			}
		}
	}
	return s
}

// unitWords splits a setting into its words, unquoted, with %% and %h — and in ExecStart
// $$ — said as what they stand for.
func unitWords(s, home string, dollars bool) []string {
	var words []string
	var w strings.Builder
	quote, have := byte(0), false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote, have = c, true
		case c == '\\' && i+1 < len(s):
			i++
			w.WriteByte(s[i])
			have = true
		case quote == 0 && (c == ' ' || c == '\t'):
			if have {
				words = append(words, w.String())
				w.Reset()
				have = false
			}
		default:
			w.WriteByte(c)
			have = true
		}
	}
	if have {
		words = append(words, w.String())
	}
	for i, word := range words {
		var b strings.Builder
		for j := 0; j < len(word); j++ {
			switch {
			case word[j] == '%' && j+1 < len(word) && word[j+1] == '%':
				b.WriteByte('%')
				j++
			case word[j] == '%' && j+1 < len(word) && word[j+1] == 'h':
				b.WriteString(home)
				j++
			case dollars && word[j] == '$' && j+1 < len(word) && word[j+1] == '$':
				b.WriteByte('$')
				j++
			default:
				b.WriteByte(word[j])
			}
		}
		words[i] = b.String()
	}
	return words
}

// envFile reads the KEY=value lines of an environment file.
func envFile(path string) []Var {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var env []Var
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if name, v, ok := strings.Cut(line, "="); ok {
			env = append(env, Var{strings.TrimSpace(name), strings.Trim(strings.TrimSpace(v), `"'`)})
		}
	}
	return env
}

func (u *systemd) current() ([]byte, bool) { return readFile(u.where()) }
func (u *systemd) write(def []byte) error  { return writeFile(u.where(), def) }

// load has systemd read the unit, start it at every login, and start it now — or again,
// so that a running one takes what was written.
func (u *systemd) load() ([]string, error) {
	var ran []string
	for _, step := range [][]string{{"daemon-reload"}, {"enable", Unit}, {"restart", Unit}} {
		args := append([]string{"--user"}, step...)
		ran = append(ran, said("systemctl", args...))
		if out, err := u.m.Run("systemctl", args...); err != nil {
			return ran, fmt.Errorf("systemctl %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return ran, nil
}

func (u *systemd) state() (bool, string, int) {
	out, err := u.m.Run("systemctl", "--user", "show", Unit, "--property=LoadState,ActiveState,SubState,MainPID")
	if err != nil {
		return false, "", 0
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	if props["LoadState"] != "loaded" {
		return false, "", 0
	}
	state := props["ActiveState"]
	if sub := props["SubState"]; sub != "" && sub != state {
		state += " (" + sub + ")"
	}
	pid, _ := strconv.Atoi(props["MainPID"])
	return true, state, pid
}

func (u *systemd) unload() []string {
	u.m.Run("systemctl", "--user", "disable", "--now", Unit)
	return []string{said("systemctl", "--user", "disable", "--now", Unit)}
}

func (u *systemd) remove() ([]string, error) {
	if err := removeFile(u.where()); err != nil {
		return nil, err
	}
	u.m.Run("systemctl", "--user", "daemon-reload")
	return []string{said("systemctl", "--user", "daemon-reload")}, nil
}

func (u *systemd) loadSteps() []string {
	return []string{
		said("systemctl", "--user", "daemon-reload"),
		said("systemctl", "--user", "enable", "--now", Unit),
	}
}

func (u *systemd) unloadSteps() []string {
	return []string{
		said("systemctl", "--user", "disable", "--now", Unit),
		said("rm", u.where()),
		said("systemctl", "--user", "daemon-reload"),
	}
}
