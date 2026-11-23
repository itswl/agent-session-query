package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/service"
)

// userSystemd stands in for a user's systemd: it keeps what it is told, and the service it
// starts answers at 7920. Nothing here runs a real service manager.
type userSystemd struct {
	unit             string
	loaded, running  bool
	ran              []string
	answersElsewhere bool
}

func (f *userSystemd) run(name string, args ...string) ([]byte, error) {
	f.ran = append(f.ran, name+" "+strings.Join(args, " "))
	switch strings.Join(args, " ") {
	case "--user daemon-reload":
		_, err := os.Stat(f.unit)
		f.loaded = err == nil
	case "--user restart asq.service":
		f.running = true
	case "--user disable --now asq.service":
		f.running = false
	case "--user show asq.service --property=LoadState,ActiveState,SubState,MainPID":
		if !f.loaded {
			return []byte("LoadState=not-found\n"), nil
		}
		return []byte("LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=7\n"), nil
	}
	return nil, nil
}

// withSystemd has asq service set up on a Linux of the test's own, whatever runs it.
func withSystemd(t *testing.T) *userSystemd {
	t.Helper()
	home := t.TempDir()
	for _, name := range []string{"ASQ_HOST", "ASQ_PORT", "ASQ_TOKEN", "ASQ_ONLY", "ASQ_PATH", "ASQ_PROXY_HEADER", "ASQ_CORS_ORIGIN", "CODEX_HOME"} {
		t.Setenv(name, "")
	}
	t.Setenv("ASQ_INDEX", filepath.Join(home, "cache", "asq", "index.db"))
	t.Setenv("ASQ_STATE", filepath.Join(home, "state", "asq"))
	f := &userSystemd{unit: filepath.Join(home, ".config", "systemd", "user", "asq.service")}
	old := newMachine
	newMachine = func(port int) *service.Machine {
		return &service.Machine{OS: "linux", Home: home, Config: filepath.Join(home, ".config"), UID: 1000, User: "me", Port: port,
			Run:     f.run,
			Answers: func(addr string) bool { return f.answersElsewhere || f.running && addr == "127.0.0.1:7920" },
			Lingers: func() bool { return true }}
	}
	t.Cleanup(func() { newMachine = old })
	return f
}

func runAsq(args ...string) (stdout, stderr string, code int) {
	var out, errs bytes.Buffer
	code = Run(args, strings.NewReader(""), &out, &errs)
	return out.String(), errs.String(), code
}

func TestServiceInstallsWhatServeHereWouldServe(t *testing.T) {
	f := withSystemd(t)
	token := "0123456789abcdef0123456789abcdef"
	t.Setenv("ASQ_TOKEN", token)
	codex := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", codex)
	rel := filepath.Join("some", "store")
	abs, _ := filepath.Abs(rel)

	out, errs, code := runAsq("service", "install", "--port", "7920", "--only", "claude,codex", "--path", "claude@box="+rel)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"systemd: wrote " + f.unit, "  runs     ", " serve --port 7920 --only ",
		"  with     ASQ_TOKEN, ", "CODEX_HOME", "  ran      systemctl --user restart asq.service", "  answers  http://127.0.0.1:7920", "  log      journalctl --user -u asq"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, token) {
		t.Errorf("the token is said:\n%s", out)
	}
	unit, err := os.ReadFile(f.unit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "Environment=ASQ_TOKEN="+token+"\n") || !strings.Contains(string(unit), service.Mark) {
		t.Errorf("the unit:\n%s", unit)
	}
	if info, _ := os.Stat(f.unit); info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("a unit holding the token is %v", info.Mode().Perm())
	}

	out, _, code = runAsq("service", "status", "--json")
	var st service.Status
	if err := json.Unmarshal([]byte(out), &st); code != 0 || err != nil {
		t.Fatalf("exit %d %v\n%s", code, err, out)
	}
	want := []string{thisBinary(), "serve", "--port", "7920", "--only", "claude,codex", "--path", "claude@box=" + abs}
	if !st.SetUp || !st.Ours || !st.Answers || st.State != "active (running)" || st.PID != 7 || !reflect.DeepEqual(st.Command, want) {
		t.Errorf("status %+v\nwant command %q", st, want)
	}
	for _, name := range []string{"ASQ_TOKEN", "ASQ_INDEX", "ASQ_STATE", "CODEX_HOME"} {
		if !slices.Contains(st.Env, name) {
			t.Errorf("env %q lacks %s", st.Env, name)
		}
	}
	if strings.Contains(out, token) {
		t.Errorf("status says the token:\n%s", out)
	}

	out, _, code = runAsq("service")
	if code != 0 || !strings.Contains(out, "written by asq service install") || !strings.Contains(out, "  state    active (running), pid 7") {
		t.Errorf("exit %d\n%s", code, out)
	}

	out, _, code = runAsq("service", "uninstall")
	if code != 0 || !strings.Contains(out, "systemd: removed "+f.unit) || !strings.Contains(out, "  ran      systemctl --user disable --now asq.service") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(f.unit); err == nil {
		t.Error("the unit is still there")
	}
	out, _, _ = runAsq("service", "status")
	if !strings.Contains(out, "systemd: no service for asq — asq service install sets one up") {
		t.Errorf("%s", out)
	}
}

// An index named from here is named from wherever the service starts: by its full path.
func TestServiceNamesTheIndexByItsFullPath(t *testing.T) {
	withSystemd(t)
	t.Chdir(t.TempDir())
	rel := filepath.Join("elsewhere", "index.db")
	abs, _ := filepath.Abs(rel)
	if out, errs, code := runAsq("service", "install", "--port", "7920", "--index", rel); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	out, _, _ := runAsq("service", "status", "--json")
	var st service.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(st.Command, "--index"); i < 0 || i+1 >= len(st.Command) || st.Command[i+1] != abs {
		t.Errorf("runs %q, want --index %s", st.Command, abs)
	}
}

func TestServiceDryRunWritesAndRunsNothing(t *testing.T) {
	f := withSystemd(t)
	t.Setenv("ASQ_TOKEN", "0123456789abcdef0123456789abcdef")
	out, errs, code := runAsq("service", "install", "--dry-run", "--host", "0.0.0.0")
	if code != 0 {
		t.Fatalf("exit %d %s", code, errs)
	}
	for _, want := range []string{"systemd: would write " + f.unit, "Environment=ASQ_TOKEN=<ASQ_TOKEN>\n", "serve --host 0.0.0.0", "and run\n  systemctl --user daemon-reload\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0123456789abcdef") || len(f.ran) > 0 {
		t.Errorf("ran %q\n%s", f.ran, out)
	}
	if _, err := os.Stat(f.unit); err == nil {
		t.Error("a dry run wrote the unit")
	}
	out, _, _ = runAsq("service", "install", "--dry-run", "--json")
	var p service.Plan
	if err := json.Unmarshal([]byte(out), &p); err != nil || p.Manager != "systemd" || p.Definition != f.unit || len(p.Commands) != 2 {
		t.Errorf("%v %+v", err, p)
	}
}

func TestServiceRefuses(t *testing.T) {
	f := withSystemd(t)
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"service", "start"}, 2, "asq service [install | status | uninstall]"},
		{[]string{"service", "install", "now"}, 2, "asq service [install | status | uninstall]"},
		{[]string{"service", "status", "--port", "7920"}, 2, "--port is for asq service install"},
		{[]string{"service", "uninstall", "--only", "claude"}, 2, "--only is for asq service install"},
		{[]string{"service", "status", "--force"}, 2, "--force is for asq service install and uninstall"},
		{[]string{"service", "install", "--host", "0.0.0.0"}, 2, "listening on 0.0.0.0 needs a token"},
		{[]string{"service", "install", "--only", "nosuchcli"}, 2, "unknown source"},
	} {
		out, errs, code := runAsq(tc.args...)
		if code != tc.code || !strings.Contains(errs, tc.want) {
			t.Errorf("%q: exit %d, want %d and %q\n%s%s", tc.args, code, tc.code, tc.want, out, errs)
		}
	}
	if _, err := os.Stat(f.unit); err == nil || len(f.ran) > 0 {
		t.Errorf("something was set up: ran %q", f.ran)
	}
}

func TestServiceThatDoesNotAnswerIsAFailure(t *testing.T) {
	f := withSystemd(t)
	out, _, code := runAsq("service", "install", "--port", "7921")
	if code != 1 || !strings.Contains(out, "✗ nothing answers at http://127.0.0.1:7921") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(f.unit); err != nil {
		t.Errorf("the unit was not written: %v", err)
	}
	f.answersElsewhere = true
	if _, errs, code := runAsq("service", "install", "--port", "7922"); code != 0 {
		t.Errorf("installing again over one asq wrote: exit %d %s", code, errs)
	}
}

func TestServiceEnvIsWhatAsqReads(t *testing.T) {
	set := map[string]string{
		"ASQ_TOKEN": "t", "ASQ_INDEX": "/i", "CLAUDE_CONFIG_DIR": "/c", "CODEX_HOME": "/x", "OPENCODE_DB": "/o.db",
		"XDG_DATA_HOME": "/d", "PI_CODING_AGENT_SESSION_DIR": "/p", "TZ": "Europe/Berlin", "PATH": "/bin", "SECRET_OTHER": "s",
	}
	var got []string
	for _, v := range serviceEnv(func(name string) string { return set[name] }) {
		if v.Value != set[v.Name] {
			t.Errorf("%s=%s", v.Name, v.Value)
		}
		got = append(got, v.Name)
	}
	slices.Sort(got)
	want := []string{"ASQ_INDEX", "ASQ_TOKEN", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "OPENCODE_DB", "PI_CODING_AGENT_SESSION_DIR", "TZ", "XDG_DATA_HOME"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
}
