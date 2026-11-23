package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/keep"
	"github.com/itswl/agent-session-query/internal/service"
)

// uninstallHome gives uninstall a machine of the test's own: a home and every directory Go
// finds the cache, the config and the state by, on any system; an index at the default path
// and a state in it; the CLIs' stores in it, empty; and a service manager of goos that reads
// the definitions in it and is never run.
func uninstallHome(t *testing.T, goos string, run func(name string, args ...string) ([]byte, error)) (home, state string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)        // what UserHomeDir reads on unix…
	t.Setenv("USERPROFILE", home) // …and on Windows
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xdg-state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg-data"))
	t.Setenv("LocalAppData", filepath.Join(home, "local"))
	t.Setenv("AppData", filepath.Join(home, "roaming"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("ASQ_INDEX", "")
	t.Setenv("ASQ_ONLY", "")
	t.Setenv("ASQ_PATH", "")
	state = filepath.Join(home, "state")
	t.Setenv("ASQ_STATE", state)
	put(t, filepath.Join(state, "session.key"), "k")
	put(t, index.DefaultPath(), "i")
	if run == nil {
		run = func(string, ...string) ([]byte, error) { return nil, errors.New("no service manager here") }
	}
	old := newMachine
	newMachine = func(port int) *service.Machine {
		return &service.Machine{OS: goos, Home: home, Config: filepath.Join(home, "config"), UID: 501, User: "me", Port: port,
			Run: run, Answers: func(string) bool { return false }, Lingers: func() bool { return true }}
	}
	t.Cleanup(func() { newMachine = old })
	return home, state
}

func put(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func uninstallJSON(t *testing.T) uninstalled {
	t.Helper()
	out, errs, code := runAsq("uninstall", "--json")
	if code != 0 {
		t.Fatalf("asq uninstall --json: exit %d\n%s", code, errs)
	}
	var got uninstalled
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return got
}

func gone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s is still there", path)
	}
}

func there(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s was taken: %v", path, err)
	}
}

// uninstall takes what asq wrote — the index, the state — and only that; the CLIs' stores it
// never touches. The rest is said as commands: this very binary, and a service found by what
// its file names.
func TestUninstallTakesWhatAsqWrote(t *testing.T) {
	home, state := uninstallHome(t, runtime.GOOS, nil)
	store := put(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"), "{}\n")

	// A service file naming this very binary, where this machine's service manager reads it
	self := thisFile()
	switch runtime.GOOS {
	case "darwin":
		put(t, filepath.Join(home, "Library", "LaunchAgents", "x.plist"), "<string>"+self+"</string>")
	case "linux":
		put(t, filepath.Join(home, "config", "systemd", "user", "asq.service"), "ExecStart="+self+" serve")
	}

	said, errs, code := runAsq("uninstall")
	if code != 0 {
		t.Fatalf("asq uninstall: exit %d\n%s", code, errs)
	}
	gone(t, index.DefaultPath())
	gone(t, state)
	there(t, store)
	if !strings.Contains(said, "to finish, run:") {
		t.Errorf("asq uninstall said:\n%s", said)
	}
	if runtime.GOOS != "windows" && !strings.Contains(said, "rm ") {
		t.Errorf("no command said to remove the binary:\n%s", said)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(said, "launchctl bootout") {
		t.Errorf("the LaunchAgent was not said:\n%s", said)
	}
	if runtime.GOOS == "linux" && !strings.Contains(said, "systemctl --user disable --now asq") {
		t.Errorf("the unit was not said:\n%s", said)
	}
}

// --json answers what was removed, what was left and what to run, for a script or an agent
// to finish.
func TestUninstallJSON(t *testing.T) {
	_, state := uninstallHome(t, runtime.GOOS, nil)
	got := uninstallJSON(t)
	if len(got.Removed) != 2 || got.Removed[0] != index.DefaultPath() || got.Removed[1] != state {
		t.Errorf("removed %v", got.Removed)
	}
	if got.Left == nil || len(got.Left) != 0 {
		t.Errorf("left %#v, want []", got.Left)
	}
	if len(got.Run) == 0 {
		t.Error("nothing to run was said")
	}
}

// A state dir that is a home, or holds one — an ASQ_STATE pointed too wide — is refused, not
// removed, and said as left.
func TestUninstallRefusesAHomeForState(t *testing.T) {
	home, _ := uninstallHome(t, runtime.GOOS, nil)
	for _, state := range []string{home, filepath.Dir(home)} {
		t.Setenv("ASQ_STATE", state)
		out, errs, code := runAsq("uninstall")
		if code != 0 {
			t.Fatalf("asq uninstall: exit %d\n%s", code, errs)
		}
		there(t, home)
		there(t, filepath.Join(home, "state", "session.key"))
		if strings.Contains(out, "removed "+state+" ") {
			t.Errorf("the home was said as removed:\n%s", out)
		}
		if !strings.Contains(out, "left "+state+" — a home, or a folder that holds one") {
			t.Errorf("the home was not said as left:\n%s", out)
		}
	}
}

// An index ASQ_INDEX names is removed when it is one — a SQLite database, or the empty file
// it is when just made —, and left when it is a file asq never wrote, or a folder.
func TestUninstallTakesANamedIndexOnlyWhenItIsOne(t *testing.T) {
	home, _ := uninstallHome(t, runtime.GOOS, nil)
	for _, tc := range []struct {
		name, content string
		taken         bool
	}{
		{"index.db", "SQLite format 3\x00 and the rest", true},
		{"new.db", "", true},
		{"s.jsonl", "{}\n", false},
		{"folder", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(home, "elsewhere", tc.name)
			if tc.name == "folder" {
				put(t, filepath.Join(path, "mine"), "m")
			} else {
				put(t, path, tc.content)
			}
			t.Setenv("ASQ_INDEX", path)
			got := uninstallJSON(t)
			if tc.taken {
				gone(t, path)
				if len(got.Removed) == 0 || got.Removed[0] != path {
					t.Errorf("removed %q, want %s first", got.Removed, path)
				}
				return
			}
			there(t, path)
			if len(got.Left) == 0 || got.Left[0].Path != path || got.Left[0].What != "index" || slices.Contains(got.Removed, path) {
				t.Errorf("removed %q, left %+v; want %s left", got.Removed, got.Left, path)
			}
		})
	}
}

// keep.json says where the copies are: one that does not read leaves the state whole, the
// copies wherever they are with it, and says why — in the state asq keeps by default too.
func TestUninstallLeavesAStateWhoseKeepJSONDoesNotRead(t *testing.T) {
	home, _ := uninstallHome(t, runtime.GOOS, nil)
	state := defaultState(t, home)
	put(t, filepath.Join(state, "keep.json"), `{"dir": `)
	copies := keptHere(t, home, filepath.Join(state, "kept"))

	got := uninstallJSON(t)
	for _, p := range append(copies, filepath.Join(state, "session.key")) {
		there(t, p)
	}
	if len(got.Removed) != 1 || len(got.Left) != 1 || got.Left[0].Path != state || got.Left[0].What != "state" ||
		!strings.Contains(got.Left[0].Why, "keep.json") {
		t.Errorf("removed %q, left %+v; want the index removed, and %s left for its keep.json", got.Removed, got.Left, state)
	}
}

// keptHere puts copies asq keep made into a folder: two of Claude Code's transcripts, one
// of which Claude Code still holds and one it has deleted, which is kept nowhere else.
func keptHere(t *testing.T, home, folder string) []string {
	t.Helper()
	put(t, filepath.Join(home, ".claude", "projects", "p", "held.jsonl"), "{}\n")
	return []string{
		put(t, filepath.Join(folder, "claude", "p", "held.jsonl"), "{}\n"),
		put(t, filepath.Join(folder, "claude", "p", "deleted.jsonl"), "{}\n"),
	}
}

func removeCommand(folder string) string {
	if runtime.GOOS == "windows" {
		return "Remove-Item -Recurse -Force -LiteralPath " + app.ShellArg(folder)
	}
	return "rm -rf " + app.ShellArg(folder)
}

// defaultState has uninstall find the state where asq keeps it when ASQ_STATE names none —
// a folder of asq's own, under XDG_STATE_HOME —, and puts the sign-in key in it.
func defaultState(t *testing.T, home string) string {
	t.Helper()
	t.Setenv("ASQ_STATE", "")
	state := filepath.Join(home, "xdg-state", "asq")
	put(t, filepath.Join(state, "session.key"), "k")
	return state
}

// The copies asq keep made may be all there is of what a CLI deleted: uninstall takes the
// rest of the state and leaves them, and says where they are, what they hold, and how to
// remove them should that be wanted — in the state asq keeps by default, and in a folder
// ASQ_STATE names.
func TestUninstallLeavesTheKeptCopies(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "ASQ_STATE"}[named], func(t *testing.T) {
			home, state := uninstallHome(t, runtime.GOOS, nil)
			if !named {
				state = defaultState(t, home)
			}
			if err := keep.Save(state, keep.Settings{All: true}); err != nil {
				t.Fatal(err)
			}
			kept := filepath.Join(state, "kept")
			copies := keptHere(t, home, kept)
			ledger := put(t, filepath.Join(state, "otel-ledger.db"), "l")
			put(t, filepath.Join(state, "otel-ledger.db-wal"), "w")
			put(t, filepath.Join(state, "known.json"), "{}")

			out, errs, code := runAsq("uninstall")
			if code != 0 {
				t.Fatalf("asq uninstall: exit %d\n%s", code, errs)
			}
			for _, p := range []string{filepath.Join(state, "session.key"), filepath.Join(state, "keep.json"), ledger, ledger + "-wal", filepath.Join(state, "known.json")} {
				gone(t, p)
			}
			for _, p := range copies {
				there(t, p)
			}
			there(t, filepath.Join(home, ".claude", "projects", "p", "held.jsonl"))
			for _, want := range []string{
				"removed " + state + " — the state but for the copies in it: the sign-in key",
				"left " + kept + " — the copies asq keep made: 2 transcripts, 1 of them deleted by their CLI since and kept nowhere else\n",
				"  " + removeCommand(kept) + " removes them too",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("lacks %q:\n%s", want, out)
				}
			}

			// Run again, it takes nothing more, and says the copies again
			got := uninstallJSON(t)
			want := []leftBehind{{Path: kept, What: "copies", Why: "the copies asq keep made: 2 transcripts, 1 of them deleted by their CLI since and kept nowhere else",
				Transcripts: 2, Gone: 1, Remove: removeCommand(kept)}}
			if len(got.Removed) != 0 || !reflect.DeepEqual(got.Left, want) {
				t.Errorf("removed %q, left %+v; want nothing removed, and left %+v", got.Removed, got.Left, want)
			}
		})
	}
}

// A folder ASQ_STATE names may hold more than asq's — a CLI's store, a person's files —: of
// it uninstall takes what asq writes there and nothing else, and says the folder is left,
// and what is in it.
func TestUninstallTakesOnlyAsqsFilesFromAFolderASQ_STATENames(t *testing.T) {
	home, _ := uninstallHome(t, runtime.GOOS, nil)
	state := filepath.Join(home, ".claude")
	t.Setenv("ASQ_STATE", state)
	transcript := put(t, filepath.Join(state, "projects", "p", "s.jsonl"), "{}\n")
	settings := put(t, filepath.Join(state, "settings.json"), "{}")
	// A kept beside copies asq keep --dir sent elsewhere is not theirs: it goes only empty
	notes := put(t, filepath.Join(state, "kept", "notes.txt"), "mine")
	if err := keep.Save(state, keep.Settings{Dir: filepath.Join(home, "elsewhere")}); err != nil {
		t.Fatal(err)
	}
	ours := []string{filepath.Join(state, "keep.json")}
	for _, name := range []string{"session.key", "known.json", "known.json.123", "keep.json.456", "otel-ledger.db", "otel-ledger.db-shm"} {
		ours = append(ours, put(t, filepath.Join(state, name), "x"))
	}

	got := uninstallJSON(t)
	for _, p := range []string{transcript, settings, notes} {
		there(t, p)
	}
	for _, p := range ours {
		gone(t, p)
	}
	if len(got.Removed) != 1+len(ours) || got.Removed[0] != index.DefaultPath() {
		t.Errorf("removed %q, want the index and asq's %d files", got.Removed, len(ours))
	}
	for _, p := range got.Removed {
		if p == state {
			t.Errorf("%s was said as removed: %q", state, got.Removed)
		}
	}
	want := []leftBehind{{Path: state, What: "state", Why: "the folder ASQ_STATE names, which holds what asq did not write: kept, projects, settings.json"}}
	if !reflect.DeepEqual(got.Left, want) {
		t.Errorf("left %+v, want %+v", got.Left, want)
	}
}

// Copies in a folder asq keep --dir named outside the state are left too, and named.
func TestUninstallNamesAKeptFolderElsewhere(t *testing.T) {
	home, state := uninstallHome(t, runtime.GOOS, nil)
	folder := filepath.Join(home, "backup", "asq copies")
	if err := keep.Save(state, keep.Settings{Dir: folder}); err != nil {
		t.Fatal(err)
	}
	copies := keptHere(t, home, folder)

	got := uninstallJSON(t)
	gone(t, state)
	for _, p := range copies {
		there(t, p)
	}
	if len(got.Removed) != 2 || got.Removed[1] != state {
		t.Errorf("removed %q, want the index and %s", got.Removed, state)
	}
	if len(got.Left) != 1 || got.Left[0].Path != folder || got.Left[0].Transcripts != 2 || got.Left[0].Gone != 1 || got.Left[0].Remove != removeCommand(folder) {
		t.Errorf("left %+v, want %s and its two transcripts", got.Left, folder)
	}
}

// The copies are spared as the file system has them: a folder named by another way to it
// than the state's — through a link — and deeper in it, is still the folder; and a link in
// the state is removed, not followed into what it leads to.
func TestUninstallSparesTheCopiesWhateverTheyAreNamedBy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links take a privilege on Windows")
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	home, _ := uninstallHome(t, runtime.GOOS, nil)
	t.Setenv("ASQ_STATE", "")
	t.Setenv("XDG_STATE_HOME", link)
	state := filepath.Join(link, "asq")
	folder := filepath.Join(real, "asq", "deep", "copies")
	if err := keep.Save(state, keep.Settings{Dir: folder}); err != nil {
		t.Fatal(err)
	}
	copies := keptHere(t, home, folder)
	beside := put(t, filepath.Join(state, "deep", "beside"), "b")
	elsewhere := put(t, filepath.Join(t.TempDir(), "mine"), "m")
	if err := os.Symlink(filepath.Dir(elsewhere), filepath.Join(state, "out")); err != nil {
		t.Fatal(err)
	}

	got := uninstallJSON(t)
	for _, p := range copies {
		there(t, p)
	}
	there(t, elsewhere)
	gone(t, beside)
	gone(t, filepath.Join(state, "out"))
	gone(t, filepath.Join(state, "keep.json"))
	if len(got.Left) != 1 || got.Left[0].Path != folder {
		t.Errorf("left %+v, want %s", got.Left, folder)
	}
}

// A link in the state on the way to the copies — asq keep --dir named them through it — is
// left as it is: what it leads to is not the state's, and nothing beside the copies there is
// taken.
func TestUninstallFollowsNoLinkOnTheWayToTheCopies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links take a privilege on Windows")
	}
	home, _ := uninstallHome(t, runtime.GOOS, nil)
	state := defaultState(t, home)
	outside := t.TempDir()
	mine := put(t, filepath.Join(outside, "mine"), "m")
	via := filepath.Join(state, "via")
	if err := os.Symlink(outside, via); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(via, "copies")
	if err := keep.Save(state, keep.Settings{Dir: folder}); err != nil {
		t.Fatal(err)
	}
	copies := keptHere(t, home, folder)

	got := uninstallJSON(t)
	for _, p := range append(copies, mine, via) {
		there(t, p)
	}
	gone(t, filepath.Join(state, "session.key"))
	if len(got.Left) != 1 || got.Left[0].Path != folder {
		t.Errorf("left %+v, want %s", got.Left, folder)
	}
}

// A folder of copies that is itself a link holds what it leads to: those are named, by
// where they are, and stay when the link goes with the state.
func TestUninstallNamesTheCopiesAKeptLinkLeadsTo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links take a privilege on Windows")
	}
	home, _ := uninstallHome(t, runtime.GOOS, nil)
	state := defaultState(t, home)
	if err := keep.Save(state, keep.Settings{All: true}); err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	copies := keptHere(t, home, outside)
	if err := os.Symlink(outside, filepath.Join(state, "kept")); err != nil {
		t.Fatal(err)
	}

	got := uninstallJSON(t)
	gone(t, state)
	for _, p := range copies {
		there(t, p)
	}
	if len(got.Left) != 1 || got.Left[0].Path != outside || got.Left[0].Transcripts != 2 {
		t.Errorf("left %+v, want %s and its two transcripts", got.Left, outside)
	}
}

// What keeps asq serve running is taken down first: asq service uninstall for the service
// asq service install wrote — it needs the binary, which goes last —, brew services stop for
// Homebrew's, and the manager's own commands for one set up by hand; never both for one
// definition. Uninstall again then takes what a running server wrote meanwhile.
func TestUninstallTakesTheServicesDownFirst(t *testing.T) {
	self := thisFile()
	for _, tc := range []struct {
		os   string
		ours bool
		// place puts the definitions in a home, and says what is run, in order, before
		// uninstall again and the binary; "ours" stands for asq service uninstall
		place   func(t *testing.T, home string) []string
		notSaid []string
	}{
		{os: "darwin", ours: true, place: func(t *testing.T, home string) []string {
			dir := filepath.Join(home, "Library", "LaunchAgents")
			put(t, filepath.Join(dir, service.Label+".plist"), "<!-- "+service.Mark+" --><string>/x/asq</string><string>serve</string>")
			put(t, filepath.Join(dir, "sh.brew.asq.plist"), "<string>/opt/homebrew/opt/asq/bin/asq</string><string>serve</string>")
			put(t, filepath.Join(dir, "unrelated.plist"), "<string>/usr/bin/true</string>")
			return []string{"ours", "brew services stop asq"}
		}, notSaid: []string{service.Label, "sh.brew.asq", "unrelated"}},
		{os: "darwin", place: func(t *testing.T, home string) []string {
			mine := put(t, filepath.Join(home, "Library", "LaunchAgents", "mine.plist"), "<string>"+self+"</string>")
			return []string{"launchctl bootout gui/501 " + app.ShellArg(mine), "rm " + app.ShellArg(mine)}
		}},
		{os: "linux", ours: true, place: func(t *testing.T, home string) []string {
			dir := filepath.Join(home, "config", "systemd", "user")
			put(t, filepath.Join(dir, "asq.service"), "# "+service.Mark+"\n[Service]\nExecStart=/x/asq serve\n")
			put(t, filepath.Join(dir, "homebrew.asq.service"), "[Service]\nExecStart=/home/linuxbrew/.linuxbrew/opt/asq/bin/asq serve\n")
			mine := put(t, filepath.Join(dir, "mine.service"), "[Service]\nExecStart="+self+"\n")
			return []string{"ours", "brew services stop asq", "systemctl --user disable --now mine.service", "rm " + app.ShellArg(mine), "systemctl --user daemon-reload"}
		}, notSaid: []string{"disable --now asq.service", "homebrew.asq.service"}},
		{os: "linux", place: func(t *testing.T, home string) []string {
			// Two units by hand: daemon-reload once, after both are removed
			dir := filepath.Join(home, "config", "systemd", "user")
			unit := put(t, filepath.Join(dir, "asq.service"), "[Service]\nExecStart=/x/asq serve\n")
			mine := put(t, filepath.Join(dir, "mine.service"), "[Service]\nExecStart="+self+"\n")
			return []string{"systemctl --user disable --now asq.service", "rm " + app.ShellArg(unit),
				"systemctl --user disable --now mine.service", "rm " + app.ShellArg(mine), "systemctl --user daemon-reload"}
		}},
		{os: "windows", ours: true, place: func(*testing.T, string) []string { return []string{"ours"} }, notSaid: []string{"schtasks"}},
		{os: "windows", place: func(*testing.T, string) []string {
			return []string{"schtasks /end /tn asq", "schtasks /delete /tn asq /f"}
		}},
	} {
		t.Run(tc.os, func(t *testing.T) {
			task := []byte(`<Task><RegistrationInfo><URI>\asq</URI></RegistrationInfo><Actions><Exec><Command>C:\x\asq.exe</Command><Arguments>serve</Arguments></Exec></Actions></Task>`)
			if tc.ours {
				task = bytes.Replace(task, []byte("<URI>"), []byte("<Description>"+service.Mark+"</Description><URI>"), 1)
			}
			run := func(name string, args ...string) ([]byte, error) {
				if tc.os == "windows" && name == "powershell.exe" && strings.Contains(args[len(args)-1], "Export-ScheduledTask") {
					return task, nil
				}
				return nil, errors.New("no service manager here")
			}
			home, _ := uninstallHome(t, tc.os, run)
			asq := asqCommand(tc.os, self)
			want := tc.place(t, home)
			for i, w := range want {
				if w == "ours" {
					want[i] = asq + " service uninstall"
				}
			}
			want = append(want, asq+" uninstall")
			got := uninstallJSON(t).Run
			binary := "rm "
			if tc.os == "windows" {
				binary = "Remove-Item "
			}
			if len(got) != len(want)+1 || !reflect.DeepEqual(got[:len(want)], want) || !strings.HasPrefix(got[len(want)], binary) {
				t.Errorf("run\n  %s\nwant\n  %s\n  %s…", strings.Join(got, "\n  "), strings.Join(want, "\n  "), binary)
			}
			for _, not := range tc.notSaid {
				if strings.Contains(strings.Join(got, "\n"), not) {
					t.Errorf("%q is said: %q", not, got)
				}
			}
		})
	}
}

// Nothing set up, nothing is said of services, nor is uninstall said again.
func TestUninstallSaysNoServiceWhereThereIsNone(t *testing.T) {
	uninstallHome(t, "darwin", nil)
	if got := uninstallJSON(t).Run; len(got) != 1 || !strings.HasPrefix(got[0], "rm ") && !strings.HasPrefix(got[0], "sudo rm ") {
		t.Errorf("run %q, want the binary alone", got)
	}
}

// A binary a package manager installed is its to remove: brew uninstall, and untap when the
// tap is this repository; scoop uninstall. Any other is a file to remove — on Windows with
// the installer's folder, which holds it alone, and never a folder that holds more.
func TestUninstallLeavesTheBinaryToItsPackageManager(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keg := filepath.Join(root, "Cellar", "asq", "1.6.0")
	brewed := put(t, filepath.Join(keg, "bin", "asq"), "")
	tap := filepath.Join(root, "Library", "Taps", "someone", "homebrew-asq")
	receipt := func(tap, path string) {
		b, _ := json.Marshal(map[string]any{"source": map[string]any{"tap": tap, "path": path}})
		put(t, filepath.Join(keg, "INSTALL_RECEIPT.json"), string(b))
	}
	formula := put(t, filepath.Join(tap, "Formula", "asq.rb"), "class Asq < Formula\nend\n")
	scoop := put(t, filepath.Join(root, "scoop", "apps", "asq", "1.6.0", "asq.exe"), "")
	current := put(t, filepath.Join(root, "scoop", "apps", "asq", "current", "asq.exe"), "")
	installed := put(t, filepath.Join(root, "Programs", "asq", "asq.exe"), "")
	built := put(t, filepath.Join(root, "src", "asq", "asq.exe"), "") // in the checkout it was built in
	put(t, filepath.Join(root, "src", "asq", "go.mod"), "module "+module+"\n")
	beside := put(t, filepath.Join(root, "tools", "asq", "asq.exe"), "") // beside the copies asq keep made
	put(t, filepath.Join(root, "tools", "asq", "kept", "claude", "p", "s.jsonl"), "{}\n")
	plain := put(t, filepath.Join(root, "bin", "asq"), "")

	check := func(goos, self string, want ...string) {
		t.Helper()
		if got := binaryCommands(goos, self); !reflect.DeepEqual(got, want) {
			t.Errorf("binaryCommands(%s, %s) = %q, want %q", goos, self, got, want)
		}
	}
	put(t, filepath.Join(tap, "go.mod"), "module "+module+"\n\ngo 1.26\n")
	receipt("someone/asq", formula)
	check("darwin", brewed, "brew uninstall asq", "brew untap someone/asq")
	put(t, filepath.Join(tap, "go.mod"), "module example.com/another\n")
	check("linux", brewed, "brew uninstall asq")
	receipt("homebrew/core", filepath.Join(root, "Caches", "api-source", "Formula", "a", "asq.rb"))
	check("darwin", brewed, "brew uninstall asq")
	os.Remove(filepath.Join(keg, "INSTALL_RECEIPT.json"))
	check("darwin", brewed, "brew uninstall asq")

	check("windows", scoop, "scoop uninstall asq")
	check("windows", current, "scoop uninstall asq")
	check("windows", installed, "Remove-Item -Recurse -Force -LiteralPath "+app.ShellArg(filepath.Dir(installed)))
	check("windows", built, "Remove-Item -Recurse -Force -LiteralPath "+app.ShellArg(built))
	check("windows", beside, "Remove-Item -Recurse -Force -LiteralPath "+app.ShellArg(beside))
	check("linux", plain, "rm "+app.ShellArg(plain))
	// Each package manager where it runs: a folder laid out as the other's is a folder
	check("linux", scoop, "rm "+app.ShellArg(scoop))
	check("windows", brewed, "Remove-Item -Recurse -Force -LiteralPath "+app.ShellArg(brewed))
}
