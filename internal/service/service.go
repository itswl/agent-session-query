// Package service keeps `asq serve` running under the service manager of the system it is
// on — launchd on macOS, systemd on Linux, Task Scheduler on Windows: the definition each
// one reads, written with this binary's full path, and the commands that load it, say how
// it stands and take it down.
//
// What it sets up is asq's own, and only that is replaced or removed without being told
// to: a definition is known as asq's by the line Mark, and one without it — made by hand,
// to this machine's own taste — is left alone unless force says otherwise.
package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
)

// The names each manager knows the service by. They are the ones docs/deploy.md has a
// setup by hand use, so that a file made from the docs is found where asq looks.
const (
	Label = "io.github.itswl.asq" // launchd's label, and the plist's name
	Unit  = "asq.service"         // systemd's unit
	Task  = "asq"                 // Task Scheduler's task, in its root folder
)

// Mark is the line by which a definition asq wrote is told from one made by hand.
const Mark = "Written by asq service install"

// Setup is what the service runs.
type Setup struct {
	Binary string   // asq, by its full path
	Args   []string // what follows it: serve and its flags
	Env    []Var    // what it runs with beyond what the manager gives every service
	// Writable is what systemd lets it write in a home it otherwise only reads: the
	// index's directory and the state's.
	Writable []string
}

// Var is one variable of the service's environment.
type Var struct{ Name, Value string }

// Secret is the variable whose value is never shown: what a plan prints holds its name in
// its place.
const Secret = "ASQ_TOKEN"

// Machine is what the service is set up on, and how its manager is asked.
type Machine struct {
	OS     string // runtime.GOOS
	Home   string
	Config string // os.UserConfigDir: where systemd keeps a user's units
	Temp   string // where a task's definition is put for schtasks to read
	UID    int    // -1 on Windows
	User   string // the account, DOMAIN\name on Windows: whose logon starts the task
	Port   int    // where asq serve listens unless told otherwise
	// Run runs one of the manager's commands and returns what it printed.
	Run func(name string, args ...string) ([]byte, error)
	// Answers says whether an asq serve answers at host:port.
	Answers func(addr string) bool
	// Lingers says whether this user's systemd services outlive a logout.
	Lingers func() bool
	// Getenv is this process's environment. A task has none of its own and starts with the
	// user's, for which this stands in when status asks where it listens.
	Getenv func(string) string
	// Wait is how long Install waits for the service to answer.
	Wait time.Duration
}

// ThisMachine is the machine asq runs on.
func ThisMachine(port int) *Machine {
	m := &Machine{OS: runtime.GOOS, UID: os.Getuid(), Port: port, Temp: os.TempDir(),
		Run: run, Answers: answers, Getenv: os.Getenv, Wait: 15 * time.Second}
	m.Home, _ = os.UserHomeDir()
	m.Config, _ = os.UserConfigDir()
	m.Lingers = func() bool { return false }
	if u, err := user.Current(); err == nil {
		m.User = u.Username
		m.Lingers = func() bool {
			_, err := os.Stat(filepath.Join("/var/lib/systemd/linger", u.Username))
			return err == nil
		}
	}
	return m
}

func run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func answers(addr string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// manager is one system's service manager.
type manager interface {
	name() string // launchd, systemd, Task Scheduler
	// where names the definition: its file, or the task.
	where() string
	// log is where asq serve's log goes, or how to read it; empty for none.
	log() string
	render(s Setup) []byte
	parse(def []byte) Setup
	// current is the definition there is now.
	current() ([]byte, bool)
	// write puts a definition in place; load has the manager take it and start it, or
	// start it again, so that a running one takes what was written.
	write(def []byte) error
	load() ([]string, error)
	// state is what the manager says of the service: whether it holds it, and how it runs.
	state() (loaded bool, state string, pid int)
	// unload stops the service and has the manager let go of it; remove deletes it.
	unload() []string
	remove() ([]string, error)
	// The commands load and unload run, for a person to read or run.
	loadSteps() []string
	unloadSteps() []string
}

func (m *Machine) manager() (manager, error) {
	switch m.OS {
	case "darwin":
		return &launchd{m: m}, nil
	case "linux":
		return &systemd{m: m}, nil
	case "windows":
		return &scheduler{m: m}, nil
	}
	return nil, fmt.Errorf("asq service knows launchd, systemd and Task Scheduler, and %s has none of them: keep asq serve running with what this system uses (docs/deploy.md)", m.OS)
}

// Plan is what Install would do, done nothing of.
type Plan struct {
	Manager    string   `json:"manager"`
	Definition string   `json:"definition"`
	Content    string   `json:"content"`
	Commands   []string `json:"commands"`
	Notes      []string `json:"notes,omitempty"`
}

// Plan says what Install would write and run. The token's value is not in it: its name
// stands where it would be written.
func (m *Machine) Plan(s Setup) (*Plan, error) {
	mg, err := m.manager()
	if err != nil {
		return nil, err
	}
	shown := s
	shown.Env = nil
	for _, v := range s.Env {
		if v.Name == Secret {
			v.Value = "<" + Secret + ">"
		}
		shown.Env = append(shown.Env, v)
	}
	return &Plan{Manager: mg.name(), Definition: mg.where(), Content: string(mg.render(shown)),
		Commands: mg.loadSteps(), Notes: m.notes(mg, s)}, nil
}

// Installed is what Install did.
type Installed struct {
	Manager    string   `json:"manager"`
	Definition string   `json:"definition"`
	Command    []string `json:"command"`
	Env        []string `json:"env"` // the names: their values may be secrets
	Ran        []string `json:"ran"`
	Address    string   `json:"address"`
	Answers    bool     `json:"answers"`
	Log        string   `json:"log,omitempty"`
	// Said is what the service wrote to its log while Install waited, when it did not
	// come to answer: why, as often as not.
	Said  []string `json:"said,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

// Install writes the service's definition, has the manager load and start it, and waits
// for it to answer. A definition asq did not write is not replaced without force, nor is
// one set up beside another service for asq; and a port something else already answers
// on is refused, since the service would never get it.
func (m *Machine) Install(s Setup, force bool) (*Installed, error) {
	mg, err := m.manager()
	if err != nil {
		return nil, err
	}
	if m.UID == 0 {
		return nil, errors.New("this is root, whose home is not yours and whose service would read nobody's sessions: run asq service install as the user whose sessions it is to serve")
	}
	if old, there := mg.current(); there && !Marked(old) && !force {
		return nil, fmt.Errorf("%s is there and was not written by asq service install: --force replaces it", mg.where())
	}
	if others := m.others(); len(others) > 0 && !force {
		return nil, fmt.Errorf("%s — two services would start asq serve on one port: take that one down, or keep it instead of this (--force sets this one up anyway)", others[0])
	}
	// What answers is the service itself only while the manager says it runs: one it holds
	// that has stopped — a task whose window was closed, a job that failed — is not
	addr := Address(s, m.Port)
	if _, state, _ := mg.state(); !runs(state) && m.Answers(addr) {
		return nil, fmt.Errorf("something already answers at http://%s — an asq serve started by hand? Stop it, or give the service another --port", addr)
	}
	for _, dir := range s.Writable {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := mg.write(mg.render(s)); err != nil {
		return nil, err
	}
	done := &Installed{Manager: mg.name(), Definition: mg.where(), Command: append([]string{s.Binary}, s.Args...),
		Env: []string{}, Address: "http://" + addr, Log: mg.log(), Notes: m.notes(mg, s)}
	if m.OS != "windows" {
		for _, v := range s.Env {
			done.Env = append(done.Env, v.Name)
		}
	}
	// What the service writes from here on is what it says of this start
	logFile, logFrom := "", int64(0)
	if filepath.IsAbs(done.Log) {
		logFile = done.Log
		if info, err := os.Stat(logFile); err == nil {
			logFrom = info.Size()
		}
	}
	if done.Ran, err = mg.load(); err != nil {
		return done, err
	}
	for end := time.Now().Add(m.Wait); ; time.Sleep(250 * time.Millisecond) {
		if done.Answers = m.Answers(addr); done.Answers || time.Now().After(end) {
			break
		}
	}
	if !done.Answers && logFile != "" {
		done.Said = tail(logFile, logFrom, 5)
	}
	return done, nil
}

// runs says whether a state the manager gave is a service that runs: launchd's and Task
// Scheduler's running, systemd's active.
func runs(state string) bool { return state == "running" || strings.HasPrefix(state, "active") }

// notes are what a person should know of the service on this system beyond what it runs.
func (m *Machine) notes(mg manager, s Setup) []string {
	var out []string
	switch mg.(type) {
	case *systemd:
		if m.Lingers != nil && !m.Lingers() {
			out = append(out, "it stops when you log out, and starts when you log in, unless: sudo loginctl enable-linger "+app.ShellArg(m.User))
		}
	case *scheduler:
		out = append(out, "it runs in a console window of its own, which is the server: minimise it rather than close it")
		if len(s.Env) > 0 {
			var names []string
			for _, v := range s.Env {
				names = append(names, v.Name)
			}
			out = append(out, "a task has your user environment, not this shell's: "+strings.Join(names, ", ")+
				" reach it only from there — [Environment]::SetEnvironmentVariable('NAME', 'value', 'User'), then asq service install again")
		}
	}
	return out
}

// tail is the last lines written to a file past an offset.
func tail(path string, from int64, lines int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() < from {
		from = 0 // replaced meanwhile: all of it is new
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	if len(out) > lines {
		out = out[len(out)-lines:]
	}
	return out
}

// Status is how the service stands.
type Status struct {
	Manager    string `json:"manager"`
	Definition string `json:"definition"`
	SetUp      bool   `json:"set_up"`
	Ours       bool   `json:"written_by_asq"`
	// This is the asq asking, by the path a service would run it by.
	This    string   `json:"this"`
	Command []string `json:"command,omitempty"`
	Env     []string `json:"env,omitempty"` // the names
	// Missing says the binary it runs is not there: removed, or moved.
	Missing bool   `json:"binary_missing,omitempty"`
	Loaded  bool   `json:"loaded"`
	State   string `json:"state,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Address string `json:"address,omitempty"`
	Answers bool   `json:"answers"`
	Log     string `json:"log,omitempty"`
	// Others are the services for asq another tool set up: Homebrew's.
	Others []string `json:"others,omitempty"`
	// Down is how to take it down by hand: for a definition asq did not write, which
	// asq service uninstall leaves alone without --force.
	Down []string `json:"down,omitempty"`
}

// Status says how the service stands: its definition, what the manager says of it, and
// whether it answers. this is the binary asking.
func (m *Machine) Status(this string) (*Status, error) {
	mg, err := m.manager()
	if err != nil {
		return nil, err
	}
	st := &Status{Manager: mg.name(), Definition: mg.where(), This: this, Log: mg.log(), Others: m.others()}
	def, there := mg.current()
	st.Loaded, st.State, st.PID = mg.state()
	if !there {
		return st, nil
	}
	st.SetUp, st.Ours = true, Marked(def)
	s := mg.parse(def)
	if s.Binary != "" {
		st.Command = append([]string{s.Binary}, s.Args...)
		if _, err := os.Stat(s.Binary); err != nil {
			st.Missing = true
		}
	}
	for _, v := range s.Env {
		st.Env = append(st.Env, v.Name)
	}
	if _, task := mg.(*scheduler); task && m.Getenv != nil {
		// Where it listens is in the user's environment as much as in its arguments, as
		// install took it from this shell's
		for _, name := range []string{"ASQ_HOST", "ASQ_PORT"} {
			if v := m.Getenv(name); v != "" {
				s.Env = append(s.Env, Var{name, v})
			}
		}
	}
	addr := Address(s, m.Port)
	st.Address, st.Answers = "http://"+addr, m.Answers(addr)
	if !st.Ours {
		st.Down = mg.unloadSteps()
	}
	return st, nil
}

// Removed is what Uninstall did.
type Removed struct {
	Manager    string   `json:"manager"`
	Definition string   `json:"definition"`
	Removed    bool     `json:"removed"`
	Ran        []string `json:"ran"`
}

// Uninstall stops the service and removes its definition — one asq wrote, or with force
// any there is. A definition made by hand is otherwise left, and the commands that would
// take it down are given in the error.
func (m *Machine) Uninstall(force bool) (*Removed, error) {
	mg, err := m.manager()
	if err != nil {
		return nil, err
	}
	done := &Removed{Manager: mg.name(), Definition: mg.where(), Ran: []string{}}
	def, there := mg.current()
	if !there {
		return done, nil
	}
	if !Marked(def) && !force {
		return nil, fmt.Errorf("%s was not written by asq service install, and is left as it is: --force removes it, or by hand:\n  %s",
			mg.where(), strings.Join(mg.unloadSteps(), "\n  "))
	}
	done.Ran = mg.unload()
	more, err := mg.remove()
	done.Ran = append(done.Ran, more...)
	if err != nil {
		return done, err
	}
	done.Removed = true
	return done, nil
}

// others are the services for asq that another tool set up: Homebrew's, from `brew
// services start asq`. They start the same asq serve on the same port. Homebrew names a
// service sh.brew.asq, and one an older Homebrew started keeps the name it had then:
// homebrew.mxcl.asq under launchd, homebrew.asq under systemd.
func (m *Machine) others() []string {
	var files []string
	switch m.OS {
	case "darwin":
		dir := filepath.Join(m.Home, "Library", "LaunchAgents")
		files = []string{filepath.Join(dir, "sh.brew.asq.plist"), filepath.Join(dir, "homebrew.mxcl.asq.plist")}
	case "linux":
		dir := filepath.Join(m.Config, "systemd", "user")
		files = []string{filepath.Join(dir, "sh.brew.asq.service"), filepath.Join(dir, "homebrew.asq.service")}
	}
	var out []string
	for _, file := range files {
		if _, err := os.Stat(file); err == nil {
			out = append(out, "Homebrew's service for asq is set up too ("+file+"; brew services stop asq takes it down)")
		}
	}
	return out
}

// Marked says whether asq service install wrote a definition.
func Marked(def []byte) bool { return bytes.Contains(def, []byte(Mark)) }

// Address is where the service's asq serve listens, as host:port to ask it at: its flags,
// then its environment, then serve's defaults. A server listening on every address is
// asked on loopback.
func Address(s Setup, defaultPort int) string {
	host, port := "", ""
	for _, v := range s.Env {
		switch v.Name {
		case "ASQ_HOST":
			host = v.Value
		case "ASQ_PORT":
			port = v.Value
		}
	}
	for i := 0; i < len(s.Args); i++ {
		if !strings.HasPrefix(s.Args[i], "-") {
			continue
		}
		name, value, inline := strings.Cut(strings.TrimLeft(s.Args[i], "-"), "=")
		if name != "host" && name != "port" {
			continue
		}
		if !inline {
			if i+1 >= len(s.Args) {
				break
			}
			i++
			value = s.Args[i]
		}
		if name == "host" {
			host = value
		} else {
			port = value
		}
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 {
		port = strconv.Itoa(defaultPort)
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// Stable is the path a service should run asq by: one that names the same program after
// an upgrade. A package manager that keeps each version in a directory of its own has a
// path that does not move — Homebrew's opt, Scoop's current — and the version's own path
// is gone after the next upgrade and cleanup. A link into such a directory — Homebrew's
// bin/asq — is taken to the same path; any other path, a link of the user's own included,
// is kept as it is.
func Stable(path string) string {
	if stable := unversioned(path); stable != path {
		return stable
	}
	if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
		if stable := unversioned(real); stable != real {
			return stable
		}
	}
	return path
}

// unversioned is a path in a version's own directory as the one its package manager keeps
// from version to version, when there is one.
func unversioned(path string) string {
	sep := string(filepath.Separator)
	parts := strings.Split(path, sep)
	join := func(pieces ...[]string) string {
		var all []string
		for _, p := range pieces {
			all = append(all, p...)
		}
		return strings.Join(all, sep)
	}
	for i := 0; i+3 < len(parts); i++ {
		var stable string
		switch {
		// <prefix>/Cellar/asq/<version>/bin/asq → <prefix>/opt/asq/bin/asq
		case parts[i] == "Cellar" && parts[i+1] == "asq":
			stable = join(parts[:i], []string{"opt", "asq"}, parts[i+3:])
		// <scoop>\apps\asq\<version>\asq.exe → <scoop>\apps\asq\current\asq.exe
		case strings.EqualFold(parts[i], "apps") && strings.EqualFold(parts[i+1], "asq") && !strings.EqualFold(parts[i+2], "current"):
			stable = join(parts[:i+2], []string{"current"}, parts[i+3:])
		default:
			continue
		}
		if _, err := os.Stat(stable); err == nil {
			return stable
		}
	}
	return path
}

// writeFile puts a definition in place whole, its owner's alone: it may hold a token.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".asq-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readFile(path string) ([]byte, bool) {
	b, err := os.ReadFile(path)
	return b, err == nil
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// said is a command as a person would type it.
func said(name string, args ...string) string {
	words := []string{name}
	for _, a := range args {
		words = append(words, app.ShellArg(a))
	}
	return strings.Join(words, " ")
}
