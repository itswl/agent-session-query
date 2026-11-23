package service

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fake is a service manager that keeps what it is told in memory: launchctl, systemctl,
// schtasks and PowerShell as far as the package asks them. Nothing here runs a real one.
type fake struct {
	t       *testing.T
	m       *Machine
	ran     []string
	loaded  bool   // launchd holds the job; systemd has read the unit
	running bool   // the service runs, and answers
	task    []byte // the task Task Scheduler holds, as PowerShell gives it
	taken   bool   // something else answers at the port
	stdin   []byte // the file schtasks /create was given, as it was
	// starts is what a start does besides running; a test that wants a service that
	// fails sets it
	starts func()
}

func newFake(t *testing.T, goos string) *fake {
	t.Helper()
	home := t.TempDir()
	f := &fake{t: t}
	f.m = &Machine{OS: goos, Home: home, Config: filepath.Join(home, ".config"), Temp: t.TempDir(),
		UID: 501, User: "me", Port: 7717, Run: f.run, Answers: f.answers, Lingers: func() bool { return false }}
	if goos == "windows" {
		f.m.UID, f.m.User = -1, `PC\me`
	}
	f.starts = func() { f.running = true }
	return f
}

func (f *fake) answers(string) bool { return f.taken || f.running }

func (f *fake) run(name string, args ...string) ([]byte, error) {
	f.ran = append(f.ran, said(name, args...))
	fail := errors.New("exit status 1")
	switch name + " " + strings.Join(args, " ") {
	// launchd
	case "launchctl print gui/501/" + Label:
		if !f.loaded {
			return []byte("Could not find service \"" + Label + "\" in domain for user gui: 501\n"), errors.New("exit status 113")
		}
		state, pid := "not running", ""
		if f.running {
			state, pid = "running", "\tpid = 4242\n"
		}
		return []byte("gui/501/" + Label + " = {\n\tactive count = 1\n\tstate = " + state + "\n\n\tprogram = asq\n" +
			"\tinherited environment = {\n\t\tstate = nested\n\t}\n\n" + pid + "\tlast exit code = 2\n}\n"), nil
	case "launchctl bootstrap gui/501 " + (&launchd{m: f.m}).where():
		if f.loaded {
			return []byte("Bootstrap failed: 5: Input/output error\n"), fail
		}
		f.loaded = true
		f.starts()
		return nil, nil
	case "launchctl bootout gui/501/" + Label:
		f.loaded, f.running = false, false
		return nil, nil
	// systemd
	case "systemctl --user daemon-reload":
		_, err := os.Stat((&systemd{m: f.m}).where())
		f.loaded = err == nil
		return nil, nil
	case "systemctl --user enable " + Unit:
		return nil, nil
	case "systemctl --user restart " + Unit:
		f.starts()
		return nil, nil
	case "systemctl --user disable --now " + Unit:
		f.running = false
		return nil, nil
	case "systemctl --user show " + Unit + " --property=LoadState,ActiveState,SubState,MainPID":
		if !f.loaded {
			return []byte("LoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\n"), nil
		}
		if f.running {
			return []byte("LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=4242\n"), nil
		}
		return []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\nMainPID=0\n"), nil
	// Task Scheduler
	case "schtasks /run /tn " + Task:
		if f.task == nil {
			return nil, fail
		}
		f.starts()
		return nil, nil
	case "schtasks /end /tn " + Task:
		if !f.running {
			return []byte("ERROR: The task is not running.\n"), fail
		}
		f.running = false
		return nil, nil
	case "schtasks /delete /tn " + Task + " /f":
		f.task, f.running = nil, false
		return nil, nil
	}
	if name == "schtasks" && len(args) == 6 && args[0] == "/create" && args[3] == "/xml" {
		b, err := os.ReadFile(args[4])
		if err != nil {
			f.t.Fatal(err)
		}
		f.stdin, f.task = b, utf8Of(b)
		return nil, nil
	}
	if name == "powershell.exe" {
		script := args[len(args)-1]
		switch {
		case f.task == nil:
			return []byte("No MSFT_ScheduledTask objects found\n"), fail
		case strings.Contains(script, "Export-ScheduledTask"):
			return f.task, nil
		case strings.Contains(script, "Get-ScheduledTask"):
			if f.running {
				return []byte("Running 267009\r\n"), nil
			}
			return []byte("Ready 1\r\n"), nil
		}
	}
	f.t.Fatalf("not a command the fake knows: %s %q", name, args)
	return nil, nil
}

// setup is what the cli asks for: this binary, serve and its flags, and a variable.
func (f *fake) setup() Setup {
	bin := filepath.Join(f.m.Home, ".local", "bin", "asq")
	if f.m.OS == "windows" {
		bin = filepath.Join(f.m.Home, "AppData", "Local", "Programs", "asq", "asq.exe")
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		f.t.Fatal(err)
	}
	return Setup{Binary: bin, Args: []string{"serve", "--port", "7920"},
		Env:      []Var{{"ASQ_TOKEN", "0123456789abcdef0123"}, {"CODEX_HOME", filepath.Join(f.m.Home, "codex")}},
		Writable: []string{filepath.Join(f.m.Home, "cache", "asq"), filepath.Join(f.m.Home, "state", "asq")}}
}

func TestInstallStatusUninstall(t *testing.T) {
	for _, tc := range []struct {
		os               string
		install, removal []string
		state            string
		notes            []string
	}{
		{os: "darwin",
			install: []string{"launchctl bootstrap gui/501 {where}"},
			removal: []string{"launchctl bootout gui/501/" + Label},
			state:   "running"},
		{os: "linux",
			install: []string{"systemctl --user daemon-reload", "systemctl --user enable asq.service", "systemctl --user restart asq.service"},
			removal: []string{"systemctl --user disable --now asq.service", "systemctl --user daemon-reload"},
			state:   "active (running)",
			notes:   []string{"sudo loginctl enable-linger me"}},
		{os: "windows",
			install: []string{"schtasks /run /tn asq"},
			removal: []string{"schtasks /end /tn asq", "schtasks /delete /tn asq /f"},
			state:   "running",
			notes:   []string{"console window", "ASQ_TOKEN, CODEX_HOME reach it only from there"}},
	} {
		t.Run(tc.os, func(t *testing.T) {
			f := newFake(t, tc.os)
			s := f.setup()
			st, err := f.m.Status(s.Binary)
			if err != nil || st.SetUp || st.Loaded {
				t.Fatalf("before: %+v %v", st, err)
			}

			done, err := f.m.Install(s, false)
			if err != nil {
				t.Fatal(err)
			}
			mg, _ := f.m.manager()
			want := slices.Clone(tc.install)
			for i := range want {
				want[i] = strings.ReplaceAll(want[i], "{where}", said(mg.where()))
			}
			if !done.Answers || done.Address != "http://127.0.0.1:7920" || !reflect.DeepEqual(done.Ran, want) {
				t.Errorf("install: %+v, want it to have run %q and to answer at 7920", done, want)
			}
			if !reflect.DeepEqual(done.Command, append([]string{s.Binary}, s.Args...)) {
				t.Errorf("command %q", done.Command)
			}
			for _, note := range tc.notes {
				if !strings.Contains(strings.Join(done.Notes, "\n"), note) {
					t.Errorf("notes %q, want %q", done.Notes, note)
				}
			}
			// The values of what it runs with are not said, and on Windows not written
			wantEnv := []string{"ASQ_TOKEN", "CODEX_HOME"}
			if tc.os == "windows" {
				wantEnv = []string{}
			}
			if !reflect.DeepEqual(done.Env, wantEnv) {
				t.Errorf("env %q, want %q", done.Env, wantEnv)
			}
			for _, dir := range s.Writable {
				if _, err := os.Stat(dir); err != nil {
					t.Errorf("%s was not made: %v", dir, err)
				}
			}
			// launchd makes no directory for a log, and starts no job whose log it cannot open
			if logFile := mg.log(); filepath.IsAbs(logFile) {
				if _, err := os.Stat(filepath.Dir(logFile)); err != nil {
					t.Errorf("the log's directory was not made: %v", err)
				}
			}

			st, err = f.m.Status(s.Binary)
			if err != nil {
				t.Fatal(err)
			}
			if !st.SetUp || !st.Ours || !st.Loaded || st.State != tc.state || !st.Answers || st.Missing || st.Down != nil {
				t.Errorf("status: %+v", st)
			}
			if !reflect.DeepEqual(st.Command, done.Command) {
				t.Errorf("status reads back %q, wrote %q", st.Command, done.Command)
			}
			if tc.os != "windows" && !reflect.DeepEqual(st.Env, []string{"ASQ_TOKEN", "CODEX_HOME"}) {
				t.Errorf("status env %q", st.Env)
			}

			f.ran = nil
			gone, err := f.m.Uninstall(false)
			if err != nil {
				t.Fatal(err)
			}
			if !gone.Removed || !reflect.DeepEqual(gone.Ran, tc.removal) {
				t.Errorf("uninstall: %+v, want %q", gone, tc.removal)
			}
			if _, there := mg.current(); there {
				t.Error("the definition is still there")
			}
			st, _ = f.m.Status(s.Binary)
			if st.SetUp || st.Loaded || f.running {
				t.Errorf("after: %+v", st)
			}
			again, err := f.m.Uninstall(false)
			if err != nil || again.Removed {
				t.Errorf("a second uninstall: %+v %v", again, err)
			}
		})
	}
}

func TestInstallAgainStartsItAgain(t *testing.T) {
	for goos, want := range map[string][]string{
		"darwin":  {"launchctl bootout gui/501/" + Label},
		"linux":   {"systemctl --user restart asq.service"},
		"windows": {"schtasks /end /tn asq", "schtasks /run /tn asq"},
	} {
		t.Run(goos, func(t *testing.T) {
			f := newFake(t, goos)
			s := f.setup()
			if _, err := f.m.Install(s, false); err != nil {
				t.Fatal(err)
			}
			s.Args = []string{"serve", "--port", "7921"}
			done, err := f.m.Install(s, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, cmd := range want {
				if !slices.Contains(done.Ran, cmd) {
					t.Errorf("ran %q, want %q among them", done.Ran, cmd)
				}
			}
			if !f.running || done.Address != "http://127.0.0.1:7921" {
				t.Errorf("not running anew: %+v", done)
			}
		})
	}
}

// What docs/deploy.md has a person write by hand, at the names asq uses.
var byHand = map[string]string{
	"darwin": `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.github.itswl.asq</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/YOU/.local/bin/asq</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Standard</string>
  <key>StandardErrorPath</key><string>/Users/YOU/Library/Logs/asq.log</string>
</dict>
</plist>
`,
	"linux": `# ~/.config/systemd/user/asq.service
[Unit]
Description=asq — agent session history
After=network.target

[Service]
ExecStart=%h/.local/bin/asq serve
EnvironmentFile=-%h/.config/asq/env
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%h/.cache/asq %h/.local/state/asq
PrivateTmp=true

[Install]
WantedBy=default.target
`,
	"windows": `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><URI>\asq</URI></RegistrationInfo>
  <Actions Context="Author">
    <Exec>
      <Command>"C:\Users\me\AppData\Local\Programs\asq\asq.exe"</Command>
      <Arguments>serve</Arguments>
    </Exec>
  </Actions>
</Task>
`,
}

// place puts a definition where the manager looks, as a person would have.
func (f *fake) place(def string) {
	mg, _ := f.m.manager()
	switch f.m.OS {
	case "windows":
		f.task = []byte(def)
	default:
		if err := writeFile(mg.where(), []byte(def)); err != nil {
			f.t.Fatal(err)
		}
		f.loaded = true
	}
	f.running = true
}

func TestASetupByHandIsLeftAlone(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			f := newFake(t, goos)
			f.place(byHand[goos])
			mg, _ := f.m.manager()
			s := f.setup()

			if _, err := f.m.Install(s, false); err == nil || !strings.Contains(err.Error(), "--force replaces it") {
				t.Errorf("install over one made by hand: %v", err)
			}
			st, err := f.m.Status(s.Binary)
			if err != nil {
				t.Fatal(err)
			}
			if !st.SetUp || st.Ours || !reflect.DeepEqual(st.Down, mg.unloadSteps()) || len(st.Command) < 2 || st.Command[1] != "serve" {
				t.Errorf("status: %+v", st)
			}
			if _, err := f.m.Uninstall(false); err == nil || !strings.Contains(err.Error(), mg.unloadSteps()[0]) {
				t.Errorf("uninstall of one made by hand: %v", err)
			}
			if def, _ := mg.current(); !bytes.Equal(utf8Of(def), []byte(byHand[goos])) {
				t.Error("it was touched")
			}

			if _, err := f.m.Install(s, true); err != nil {
				t.Fatal(err)
			}
			if def, _ := mg.current(); !Marked(def) {
				t.Error("--force did not replace it")
			}
			f.place(byHand[goos])
			if gone, err := f.m.Uninstall(true); err != nil || !gone.Removed {
				t.Errorf("--force did not remove it: %+v %v", gone, err)
			}
		})
	}
}

func TestADefinitionByHandIsReadBack(t *testing.T) {
	home := t.TempDir()
	env := filepath.Join(home, ".config", "asq", "env")
	if err := os.MkdirAll(filepath.Dir(env), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte("# the token\nASQ_TOKEN=0123456789abcdef0123\nCODEX_HOME=\"/srv/codex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Machine{Home: home}
	for _, tc := range []struct {
		mg   manager
		def  string
		want Setup
	}{
		{&launchd{m: m}, byHand["darwin"], Setup{Binary: "/Users/YOU/.local/bin/asq", Args: []string{"serve"}}},
		{&systemd{m: m}, byHand["linux"], Setup{Binary: home + "/.local/bin/asq", Args: []string{"serve"},
			Env: []Var{{"ASQ_TOKEN", "0123456789abcdef0123"}, {"CODEX_HOME", "/srv/codex"}}}},
		{&scheduler{m: m}, byHand["windows"], Setup{Binary: `C:\Users\me\AppData\Local\Programs\asq\asq.exe`, Args: []string{"serve"}}},
		{&scheduler{m: m}, string(utf16Of([]byte(byHand["windows"]))), Setup{Binary: `C:\Users\me\AppData\Local\Programs\asq\asq.exe`, Args: []string{"serve"}}},
	} {
		if got := tc.mg.parse([]byte(tc.def)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.mg.name(), got, tc.want)
		}
	}
}

func TestWhatIsWrittenIsReadBack(t *testing.T) {
	m := &Machine{Home: t.TempDir(), User: `PC\me & you`}
	awkward := Setup{
		Binary: filepath.Join(m.Home, "my tools", "100%", "asq"),
		Args:   []string{"serve", "--path", `claude@box=/mnt/a b/"q"/$HOME\`, "--cors-origin", "https://a.example", "--only", "claude,codex"},
		Env:    []Var{{"ASQ_TOKEN", `a&b<c>"d" 'e' $f %g \h`}, {"CODEX_HOME", "/srv/my codex"}},
	}
	for _, mg := range []manager{&launchd{m: m}, &systemd{m: m}, &scheduler{m: m}} {
		def := mg.render(awkward)
		if !Marked(def) {
			t.Errorf("%s: not marked", mg.name())
		}
		want := Setup{Binary: awkward.Binary, Args: awkward.Args, Env: awkward.Env}
		if _, ok := mg.(*scheduler); ok {
			want.Env = nil // a task has the user's environment, not its own
		}
		if got := mg.parse(def); !reflect.DeepEqual(got, want) {
			t.Errorf("%s reads back\n%+v\nfrom what it wrote, not\n%+v\n%s", mg.name(), got, want, def)
		}
		if mg.name() != "systemd" {
			d := xml.NewDecoder(bytes.NewReader(def))
			d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
			for {
				if _, err := d.Token(); err == io.EOF {
					break
				} else if err != nil {
					t.Errorf("%s: not XML: %v", mg.name(), err)
					break
				}
			}
		}
	}
}

func TestTheDefinitionsKeepItRunning(t *testing.T) {
	m := &Machine{Home: t.TempDir(), Config: t.TempDir(), User: `PC\me`}
	s := Setup{Binary: "/opt/asq", Args: []string{"serve"}, Writable: []string{"/c/asq", "/s/asq"}}
	for _, tc := range []struct {
		mg   manager
		want []string
	}{
		{&launchd{m: m}, []string{
			"<key>Label</key><string>io.github.itswl.asq</string>",
			"<key>RunAtLoad</key><true/>",
			"<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>",
			"<key>ProcessType</key><string>Standard</string>",
			"<key>StandardErrorPath</key><string>" + filepath.Join(m.Home, "Library", "Logs", "asq.log") + "</string>",
		}},
		{&systemd{m: m}, []string{
			"ExecStart=/opt/asq serve\n", "Restart=on-failure\n", "ProtectHome=read-only\n", "ProtectSystem=strict\n",
			"ReadWritePaths=/c/asq\n", "ReadWritePaths=/s/asq\n", "WantedBy=default.target\n",
		}},
		// Task Scheduler's defaults undone: 72 hours, battery, no restart, priority 7
		{&scheduler{m: m}, []string{
			"<LogonTrigger>\n      <Enabled>true</Enabled>\n      <UserId>PC\\me</UserId>",
			"<LogonType>InteractiveToken</LogonType>",
			"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
			"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
			"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
			"<StopOnIdleEnd>false</StopOnIdleEnd>",
			"<RestartOnFailure>\n      <Interval>PT1M</Interval>\n      <Count>999</Count>",
			"<Priority>5</Priority>",
			"<Command>/opt/asq</Command>\n      <Arguments>serve</Arguments>",
		}},
	} {
		def := string(tc.mg.render(s))
		for _, w := range tc.want {
			if !strings.Contains(def, w) {
				t.Errorf("%s lacks %q:\n%s", tc.mg.name(), w, def)
			}
		}
	}
}

// systemd expands a specifier (%h, %u, %g …) in every one of these settings and a variable
// ($HOME) in ExecStart: what is written doubles each, so that it reads as typed. Reading
// the unit back cannot tell, since it leaves a specifier it does not know as it is.
func TestTheUnitIsNotExpandedBySystemd(t *testing.T) {
	m := &Machine{Home: t.TempDir(), Config: t.TempDir()}
	s := Setup{Binary: "/opt/100%/asq", Args: []string{"serve", "--path", "claude=/srv/$HOME %h"},
		Env: []Var{{"ASQ_TOKEN", "a$b%gc"}}, Writable: []string{"/c/50%u"}}
	unit := string((&systemd{m: m}).render(s))
	for _, want := range []string{
		"\nExecStart=/opt/100%%/asq serve --path \"claude=/srv/$$HOME %%h\"\n",
		"\nEnvironment=ASQ_TOKEN=a$b%%gc\n",
		"\nReadWritePaths=/c/50%%u\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("lacks %q:\n%s", want, unit)
		}
	}
}

// A home with a space in it is common on Windows: the program is quoted, as Task
// Scheduler's own window writes it, and read back without the quotes.
func TestTheTaskQuotesAProgramWithASpace(t *testing.T) {
	tk := &scheduler{m: &Machine{User: `PC\a b`}}
	s := Setup{Binary: `C:\Users\a b\AppData\Local\Programs\asq\asq.exe`, Args: []string{"serve"}}
	def := string(tk.render(s))
	if !strings.Contains(def, `<Command>&#34;C:\Users\a b\AppData\Local\Programs\asq\asq.exe&#34;</Command>`) {
		t.Errorf("the program is not quoted:\n%s", def)
	}
	if got := tk.parse([]byte(def)); got.Binary != s.Binary {
		t.Errorf("read back as %q", got.Binary)
	}
}

func TestTheTaskIsGivenToSchtasksInUTF16(t *testing.T) {
	f := newFake(t, "windows")
	if _, err := f.m.Install(f.setup(), false); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(f.stdin, []byte{0xFF, 0xFE, '<', 0, '?', 0}) {
		t.Errorf("schtasks was given % x…", f.stdin[:min(8, len(f.stdin))])
	}
	if !bytes.Contains(f.task, []byte(`<UserId>PC\me</UserId>`)) {
		t.Errorf("the task does not read back:\n%s", f.task)
	}
	if left, _ := filepath.Glob(filepath.Join(f.m.Temp, "*")); len(left) > 0 {
		t.Errorf("left behind: %q", left)
	}
}

func TestInstallRefuses(t *testing.T) {
	t.Run("a port something else answers on", func(t *testing.T) {
		f := newFake(t, "darwin")
		f.taken = true
		if _, err := f.m.Install(f.setup(), false); err == nil || !strings.Contains(err.Error(), "already answers at http://127.0.0.1:7920") {
			t.Errorf("%v", err)
		}
		if _, there := (&launchd{m: f.m}).current(); there {
			t.Error("written anyway")
		}
	})
	// The manager holds the service, but it has stopped: what answers is something else
	t.Run("a port something else answers on, beside a service that has stopped", func(t *testing.T) {
		for _, goos := range []string{"darwin", "linux", "windows"} {
			f := newFake(t, goos)
			if _, err := f.m.Install(f.setup(), false); err != nil {
				t.Fatal(err)
			}
			f.running, f.taken = false, true
			if _, err := f.m.Install(f.setup(), false); err == nil || !strings.Contains(err.Error(), "already answers at http://127.0.0.1:7920") {
				t.Errorf("%s: %v", goos, err)
			}
		}
	})
	t.Run("beside Homebrew's", func(t *testing.T) {
		// The name Homebrew gives a service, and the one an older Homebrew gave it
		for _, file := range [][]string{
			{"darwin", "Library", "LaunchAgents", "sh.brew.asq.plist"},
			{"darwin", "Library", "LaunchAgents", "homebrew.mxcl.asq.plist"},
			{"linux", ".config", "systemd", "user", "sh.brew.asq.service"},
			{"linux", ".config", "systemd", "user", "homebrew.asq.service"},
		} {
			goos := file[0]
			f := newFake(t, goos)
			brew := filepath.Join(append([]string{f.m.Home}, file[1:]...)...)
			if err := writeFile(brew, []byte("brew")); err != nil {
				t.Fatal(err)
			}
			if _, err := f.m.Install(f.setup(), false); err == nil || !strings.Contains(err.Error(), "brew services stop asq") {
				t.Errorf("%s: %v", brew, err)
			}
			if st, _ := f.m.Status(""); len(st.Others) != 1 || !strings.Contains(st.Others[0], brew) {
				t.Errorf("%s: status says %q", brew, st.Others)
			}
			if _, err := f.m.Install(f.setup(), true); err != nil {
				t.Errorf("%s: --force: %v", brew, err)
			}
		}
	})
	t.Run("root", func(t *testing.T) {
		f := newFake(t, "linux")
		f.m.UID = 0
		if _, err := f.m.Install(f.setup(), false); err == nil || !strings.Contains(err.Error(), "root") {
			t.Errorf("%v", err)
		}
	})
	t.Run("a system with none of the three", func(t *testing.T) {
		f := newFake(t, "freebsd")
		if _, err := f.m.Install(f.setup(), false); err == nil || !strings.Contains(err.Error(), "docs/deploy.md") {
			t.Errorf("%v", err)
		}
	})
}

func TestInstallSaysWhatTheServiceSaidWhenItDoesNotAnswer(t *testing.T) {
	f := newFake(t, "darwin")
	logFile := (&launchd{m: f.m}).log()
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte("an earlier run's line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.starts = func() {
		lf, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(lf, "asq serve: listening on 0.0.0.0 would let anyone who can reach this machine read every session")
		lf.Close()
	}
	done, err := f.m.Install(f.setup(), false)
	if err != nil {
		t.Fatal(err)
	}
	if done.Answers || !reflect.DeepEqual(done.Said, []string{"asq serve: listening on 0.0.0.0 would let anyone who can reach this machine read every session"}) {
		t.Errorf("%+v", done)
	}
	st, _ := f.m.Status("")
	if st.State != "not running, last exit code 2" || st.PID != 0 {
		t.Errorf("state %q pid %d", st.State, st.PID)
	}
}

// A task has no environment of its own: where it listens is in the user's, as install
// took it from the shell's, and status asks there.
func TestTaskStatusAsksWhereTheEnvironmentSays(t *testing.T) {
	f := newFake(t, "windows")
	f.m.Getenv = func(name string) string { return map[string]string{"ASQ_PORT": "7921", "ASQ_HOST": "0.0.0.0"}[name] }
	f.place(byHand["windows"])
	st, err := f.m.Status("")
	if err != nil {
		t.Fatal(err)
	}
	if st.Address != "http://127.0.0.1:7921" || len(st.Env) != 0 {
		t.Errorf("address %s, env %q", st.Address, st.Env)
	}
	f.task = []byte(strings.Replace(byHand["windows"], "<Arguments>serve<", "<Arguments>serve --port 7922<", 1))
	if st, _ := f.m.Status(""); st.Address != "http://127.0.0.1:7922" {
		t.Errorf("the flag is not what counts: %s", st.Address)
	}
}

func TestStatusSaysTheBinaryIsGone(t *testing.T) {
	f := newFake(t, "linux")
	s := f.setup()
	if _, err := f.m.Install(s, false); err != nil {
		t.Fatal(err)
	}
	os.Remove(s.Binary)
	st, _ := f.m.Status("")
	if !st.Missing {
		t.Errorf("%+v", st)
	}
}

func TestThePlanHidesTheToken(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		f := newFake(t, goos)
		p, err := f.m.Plan(f.setup())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p.Content, "0123456789abcdef") || !strings.Contains(p.Content, "<ASQ_TOKEN>") && !strings.Contains(p.Content, "&lt;ASQ_TOKEN&gt;") {
			t.Errorf("%s:\n%s", goos, p.Content)
		}
		if len(f.ran) > 0 {
			t.Errorf("%s: a plan ran %q", goos, f.ran)
		}
		if _, there := func() ([]byte, bool) { mg, _ := f.m.manager(); return mg.current() }(); there {
			t.Errorf("%s: a plan wrote", goos)
		}
	}
}

func TestLaunchdState(t *testing.T) {
	f := newFake(t, "darwin")
	f.loaded, f.running = true, true
	if loaded, state, pid := (&launchd{m: f.m}).state(); !loaded || state != "running" || pid != 4242 {
		t.Errorf("%v %q %d", loaded, state, pid)
	}
}

func TestAddress(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  []Var
		want string
	}{
		{[]string{"serve"}, nil, "127.0.0.1:7717"},
		{[]string{"serve", "--port", "7920"}, nil, "127.0.0.1:7920"},
		{[]string{"serve", "-port=7921"}, nil, "127.0.0.1:7921"},
		{[]string{"serve"}, []Var{{"ASQ_PORT", "7922"}, {"ASQ_HOST", "0.0.0.0"}}, "127.0.0.1:7922"},
		{[]string{"serve", "--port", "7923"}, []Var{{"ASQ_PORT", "7922"}}, "127.0.0.1:7923"},
		{[]string{"serve", "--host", "192.0.2.4"}, nil, "192.0.2.4:7717"},
		{[]string{"serve", "--host", "::"}, nil, "127.0.0.1:7717"},
		{[]string{"serve", "--host", "[::1]", "--port", "x"}, nil, "[::1]:7717"},
		{[]string{"serve", "--path", "claude=--port"}, nil, "127.0.0.1:7717"},
		{[]string{"serve", "--port"}, nil, "127.0.0.1:7717"},
	} {
		if got := Address(Setup{Args: tc.args, Env: tc.env}, 7717); got != tc.want {
			t.Errorf("%q %v: %s, want %s", tc.args, tc.env, got, tc.want)
		}
	}
}

func TestStable(t *testing.T) {
	// The temporary directory may itself be reached through a link, as /var is on macOS
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cellar := mk("Cellar", "asq", "1.6.0", "bin", "asq")
	opt := mk("opt", "asq", "bin", "asq")
	version := mk("scoop", "apps", "asq", "1.6.0", "asq.exe")
	current := mk("scoop", "apps", "asq", "current", "asq.exe")
	plain := mk(".local", "bin", "asq")
	lonely := mk("other", "Cellar", "asq", "1.6.0", "bin", "asq") // no opt beside it
	cases := map[string]string{cellar: opt, version: current, current: current, plain: plain, lonely: lonely}
	// Homebrew's bin/asq is a link into the version's directory; a link of one's own is kept
	brewBin, own := filepath.Join(root, "bin", "asq"), filepath.Join(root, "own", "asq")
	os.MkdirAll(filepath.Dir(brewBin), 0o755)
	os.MkdirAll(filepath.Dir(own), 0o755)
	if os.Symlink(cellar, brewBin) == nil && os.Symlink(plain, own) == nil {
		cases[brewBin], cases[own] = opt, own
	} else {
		t.Log("no symbolic links here: links not tried")
	}
	for in, want := range cases {
		if got := Stable(in); got != want {
			t.Errorf("Stable(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestWindowsArguments(t *testing.T) {
	for _, args := range [][]string{
		{"serve"},
		{"serve", "--path", `claude=C:\Users\a b\.claude\projects`},
		{`a"b`, `c\"d`, `trailing\`, `trailing space\ `, "", "tab\there", `\\server\share`},
	} {
		var quoted []string
		for _, a := range args {
			quoted = append(quoted, winQuote(a))
		}
		line := strings.Join(quoted, " ")
		if got := winSplit(line); !reflect.DeepEqual(got, args) {
			t.Errorf("%q → %s → %q", args, line, got)
		}
	}
	if got := winSplit(`"C:\Program Files\asq\asq.exe" serve  --port 7920`); !reflect.DeepEqual(got, []string{`C:\Program Files\asq\asq.exe`, "serve", "--port", "7920"}) {
		t.Errorf("%q", got)
	}
}

// asq service status --json names its fields as every other answer asq gives does, in
// camelCase, as the record's fields are named.
func TestStatusNamesItsFieldsAsTheRecordDoes(t *testing.T) {
	b, err := json.Marshal(Status{SetUp: true, Ours: true, Missing: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`"setUp":true`, `"writtenByAsq":true`, `"binaryMissing":true`} {
		if !bytes.Contains(b, []byte(name)) {
			t.Errorf("status = %s, want %s in it", b, name)
		}
	}
	if bytes.Contains(b, []byte("_")) {
		t.Errorf("status = %s: a field is named in snake_case", b)
	}
}
