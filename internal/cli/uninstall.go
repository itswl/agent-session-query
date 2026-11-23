package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/keep"
	"github.com/itswl/agent-session-query/internal/service"
)

// cmdUninstall takes asq off the machine: what it wrote and only what it wrote — the index,
// a cache, and its state. The copies asq keep made it leaves, wherever they are: they may
// be all there is of what a CLI deleted. What it should not do itself it says as commands:
// taking down a service that keeps serve running, and removing this very binary — its
// package manager's to do, when one installed it. The CLIs' stores are not asq's to touch,
// and it never does.
func cmdUninstall(ctx context.Context, args []string, io *streams) error {
	c := newCommand("uninstall", io.err)
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef("asq uninstall takes nothing else")
	}
	cfg, err := c.config()
	if err != nil {
		return usageError{err}
	}
	done := uninstalled{Removed: []string{}, Left: []leftBehind{}, Run: []string{}}
	var notes []string
	removed := func(p, note string) {
		done.Removed, notes = append(done.Removed, p), append(notes, note)
	}

	// The index, and what an earlier asq left beside it — the same taking `asq index rm` does.
	path := cfg.IndexPath
	if path == "" {
		path = index.DefaultPath()
	}
	take := func(p, note string) error {
		if _, err := os.Stat(p); err != nil {
			return nil // not here: nothing to take, nothing to say
		}
		if err := index.Remove(p); err != nil {
			return err
		}
		removed(p, note)
		return nil
	}
	if err := take(path, "the index, rebuilt from the stores on the next run"); err != nil {
		return err
	}
	if cfg.IndexPath == "" {
		for _, old := range index.Older() {
			if err := take(old, "the index an earlier asq left"); err != nil {
				return err
			}
		}
	}

	m := newMachine(DefaultPort)
	self := thisFile()

	// The state: the sign-in key, what a collector has been sent, what asq keep was asked to
	// keep — but for the copies asq keep made, which stay where they are, in it or not. The
	// folder asq keeps it in by default is asq's own and goes whole; of one ASQ_STATE names,
	// only what asq writes in it goes.
	state := app.StateDir()
	const held = ": the sign-in key, what a collector was sent, what asq keep was asked to keep"
	if _, err := os.Stat(state); err == nil {
		st, kerr := keep.Load(state)
		switch {
		case tooWide(state):
			done.Left = append(done.Left, leftBehind{Path: state, What: "state",
				Why: "a home, or a folder that holds one, which ASQ_STATE names: not asq's to remove"})
		case kerr != nil:
			// Where the copies are is what keep.json says: unread, none of it is taken
			done.Left = append(done.Left, leftBehind{Path: state, What: "state",
				Why: "its keep.json, which says where the copies asq keep made are, does not read: " + kerr.Error()})
		default:
			spare := ""
			if copies := keptCopies(ctx, cfg.Stores, st.Folder(state), m.OS); copies != nil {
				spare = copies.Path
				done.Left = append(done.Left, *copies)
			}
			if os.Getenv("ASQ_STATE") != "" {
				// A folder a person named may hold more than asq's: only what asq writes goes
				took, others, whole, err := removeOurs(state, spare)
				if err != nil {
					return err
				}
				switch {
				case whole:
					removed(state, "the state"+held)
				case len(others) == 0 && len(took) > 0:
					removed(state, "the state but for the copies in it"+held)
				case len(others) > 0:
					for _, p := range took {
						removed(p, "asq's, in the folder ASQ_STATE names")
					}
					done.Left = append(done.Left, leftBehind{Path: state, What: "state",
						Why: "the folder ASQ_STATE names, which holds what asq did not write: " + some(others)})
				}
				break
			}
			took, whole, err := removeBut(state, spare)
			if err != nil {
				return err
			}
			switch {
			case whole:
				removed(state, "the state"+held)
			case took:
				removed(state, "the state but for the copies in it"+held)
			}
		}
	}

	done.Run = finishCommands(m, self)
	if c.json {
		return printJSON(io.out, done)
	}
	for i, p := range done.Removed {
		fmt.Fprintf(io.out, "removed %s — %s\n", p, notes[i])
	}
	if len(done.Removed) == 0 {
		fmt.Fprintln(io.out, "nothing of asq's was here to remove")
	}
	for _, l := range done.Left {
		fmt.Fprintf(io.out, "left %s — %s\n", l.Path, l.Why)
		if l.Remove != "" {
			fmt.Fprintf(io.out, "  %s removes them too, if they are not wanted\n", l.Remove)
		}
	}
	if len(done.Run) > 0 {
		fmt.Fprintln(io.out, "\nto finish, run:")
		for _, cmd := range done.Run {
			fmt.Fprintln(io.out, "  "+cmd)
		}
	}
	if manager, _ := service.Package(self); manager == "" {
		fmt.Fprintln(io.out, "\na line that put asq on the PATH, added when installing, can go too")
	}
	return nil
}

type uninstalled struct {
	Removed []string     `json:"removed"`
	Left    []leftBehind `json:"left"`
	Run     []string     `json:"run"`
}

// leftBehind is what uninstall left, and why.
type leftBehind struct {
	Path string `json:"path"`
	What string `json:"what"` // "copies", what asq keep made; "state", what was not asq's to take
	Why  string `json:"why"`
	// Of the copies: how many transcripts, how many of those their CLI has deleted since —
	// which are kept nowhere else —, and the command that removes them, for a person who
	// wants that too
	Transcripts int    `json:"transcripts,omitempty"`
	Gone        int    `json:"gone,omitempty"`
	Remove      string `json:"remove,omitempty"`
}

// keptCopies is the folder of the copies asq keep made, said as what is left: where it is,
// what it holds — counted as asq keep counts it, by each CLI's adapter on its folder of
// copies, beside the CLI's own store — and the command that would remove it. Nil when the
// folder holds no file.
func keptCopies(ctx context.Context, stores catalog.Config, folder, goos string) *leftBehind {
	if files, _ := keep.Size(folder); files == 0 {
		return nil
	}
	l := &leftBehind{Path: folder, What: "copies", Why: "the copies asq keep made"}
	stores.Only, stores.Kept = nil, ""
	if set, err := catalog.New(stores, catalog.Env()); err == nil {
		set.Keep(folder)
		for _, st := range set.All() {
			if keep.IsKept(st.Source) && st.Source.Exists() {
				n, gone, _ := keep.Held(ctx, st.Source)
				l.Transcripts += n
				l.Gone += gone
			}
		}
	}
	switch {
	case l.Gone > 0:
		l.Why += fmt.Sprintf(": %s, %d of them deleted by their CLI since and kept nowhere else", plural(l.Transcripts, "transcript"), l.Gone)
	case l.Transcripts > 0:
		l.Why += ": " + plural(l.Transcripts, "transcript")
	}
	l.Remove = "rm -rf " + app.ShellArg(folder)
	if goos == "windows" {
		l.Remove = "Remove-Item -Recurse -Force " + app.ShellArg(folder)
	}
	return l
}

// tooWide says whether a state directory is not asq's to remove: the root an empty name
// stood for, or a home — or a folder that holds one — that an ASQ_STATE gone wrong names.
func tooWide(dir string) bool {
	if clean := filepath.Clean(dir); dir == "" || filepath.Dir(clean) == clean {
		return true
	}
	home, err := os.UserHomeDir()
	return err == nil && home != "" && inside(home, dir)
}

// removeBut removes the directory dir and what it holds, but for the folder spare when that
// is in it: then what is beside spare, and beside each folder on the way down to it, is
// removed, and they stay. A link on the way to spare is left as it is, never followed into
// what it leads to. It reports whether it removed anything, and whether it removed dir
// whole; a dir in spare, or spare itself, it leaves.
func removeBut(dir, spare string) (took, whole bool, err error) {
	switch {
	case spare == "" || !inside(spare, dir):
		return true, true, os.RemoveAll(dir)
	case inside(dir, spare):
		return false, false, nil
	}
	took, err = removeAround(dir, spare)
	return took, false, err
}

// asqWrites says whether asq writes a file of this name in its state directory: the key
// sign-ins are signed with and the callers a server knows (internal/server), what asq keep
// was asked to keep (internal/keep), the export ledger and the files SQLite keeps beside it
// (internal/otel), each file written whole through a temporary one named after it, and
// kept, the folder for the copies when keep.json names no other.
func asqWrites(name string) bool {
	switch name {
	case "session.key", "known.json", "keep.json", "otel-ledger.db", "kept":
		return true
	}
	for _, prefix := range []string{"known.json.", "keep.json.", "otel-ledger.db-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// removeOurs removes what asq writes in a directory ASQ_STATE names, and nothing else: a
// folder a person named may hold what is not asq's — a CLI's store, their documents — and
// that stays. The folder of copies spare, and what holds it, stays too, and kept goes only
// empty of files. It reports what it removed and the names of what it left that asq does
// not write, and removes the directory itself, whole, once nothing is left in it.
func removeOurs(dir, spare string) (took, others []string, whole bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, false, err
	}
	copies := false
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if spare != "" && inside(spare, p) {
			copies = true // they, or what holds them: their own line says where they are
			continue
		}
		if !asqWrites(e.Name()) || e.Name() == "kept" && hasFiles(p) {
			others = append(others, e.Name())
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return took, others, false, err
		}
		took = append(took, p)
	}
	if copies || len(others) > 0 {
		return took, others, false, nil
	}
	return took, others, true, os.Remove(dir)
}

func hasFiles(dir string) bool {
	files, _ := keep.Size(dir)
	return files > 0
}

// some names a few of a list, and how many more there are.
func some(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:3], ", "), len(names)-3)
}

func removeAround(dir, spare string) (took bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if inside(spare, p) {
			if !e.IsDir() || inside(p, spare) {
				continue // spare, or a link to it or on the way to it
			}
			t, err := removeAround(p, spare)
			if took = took || t; err != nil {
				return took, err
			}
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return took, err
		}
		took = true
	}
	return took, nil
}

// inside reports whether path is dir or beneath it as the file system has them: by name,
// through a link — at path or on the way to it — and in another case where the file system
// ignores it. Each folder of path, as named and as its links lead, is compared with dir by
// what it is (os.SameFile).
func inside(path, dir string) bool {
	if rel, err := filepath.Rel(dir, path); err == nil && filepath.IsLocal(rel) {
		return true
	}
	at, err := os.Stat(dir)
	if err != nil {
		return false
	}
	paths := []string{filepath.Clean(path)}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		paths = append(paths, real)
	}
	for _, p := range paths {
		for {
			if fi, err := os.Stat(p); err == nil && os.SameFile(fi, at) {
				return true
			}
			up := filepath.Dir(p)
			if up == p {
				break
			}
			p = up
		}
	}
	return false
}

// thisFile is this very binary, by the path of its file: a link to it followed.
func thisFile() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		return real
	}
	return self
}

// finishCommands says what uninstall did not do itself, in the order to run it. First the
// services that keep asq serve running are taken down — asq service uninstall its own,
// which needs this binary —; then uninstall again, for the index a running server writes
// anew until it is down; then this binary is removed. Taking a service down touches more
// of the machine than a directory of asq's, and removing the binary is this command's own
// process: both are said, for a person to run.
func finishCommands(m *service.Machine, self string) []string {
	var cmds []string
	add := func(lines ...string) {
		for _, line := range lines {
			if !slices.Contains(cmds, line) {
				cmds = append(cmds, line)
			}
		}
	}
	asq := asqCommand(m.OS, self)
	found := m.Find(self)
	for _, f := range found {
		if f.By == service.ByAsq {
			add(asq + " service uninstall")
		}
		add(f.Down...)
	}
	if len(found) > 0 {
		add(asq + " uninstall")
	}
	add(binaryCommands(m.OS, self)...)
	return cmds
}

// asqCommand is how a shell runs this asq: by its name when the PATH finds this one so, by
// its path otherwise.
func asqCommand(goos, self string) string {
	if self == "" {
		return "asq"
	}
	if p, err := exec.LookPath("asq"); err == nil {
		if a, err := os.Stat(p); err == nil {
			if b, err := os.Stat(self); err == nil && os.SameFile(a, b) {
				return "asq"
			}
		}
	}
	cmd := app.ShellArg(self)
	if goos == "windows" && strings.HasPrefix(cmd, `"`) {
		cmd = "& " + cmd // PowerShell runs a quoted path given its call operator
	}
	return cmd
}

// binaryCommands removes this very binary: by its package manager when one installed it —
// Homebrew, and the tap it came from when that is this repository; Scoop —, or else the
// file, by sudo when its directory is not this account's to write. On Windows the
// installer puts the exe in a directory of its own, which goes with it.
func binaryCommands(goos, self string) []string {
	if self == "" {
		return nil
	}
	switch manager, dir := service.Package(self); manager {
	case service.Homebrew:
		cmds := []string{"brew uninstall asq"}
		if tap := brewTap(dir); tap != "" {
			cmds = append(cmds, "brew untap "+app.ShellArg(tap))
		}
		return cmds
	case service.Scoop:
		return []string{"scoop uninstall asq"}
	}
	if goos == "windows" {
		target := self
		if filepath.Base(filepath.Dir(self)) == "asq" {
			target = filepath.Dir(self)
		}
		return []string{"Remove-Item -Recurse -Force " + app.ShellArg(target)}
	}
	rm := "rm " + app.ShellArg(self)
	if !canWrite(filepath.Dir(self)) {
		rm = "sudo " + rm
	}
	return []string{rm}
}

// module is this repository's Go module, which a clone of it names in its go.mod.
const module = "github.com/itswl/agent-session-query"

// brewTap is the tap Homebrew installed asq from, when that tap is this repository — a
// clone of it, which holds asq's formula alone and goes with it; "" for homebrew/core, or
// a tap of another's, which may hold more. Homebrew writes where a formula came from in the
// receipt it leaves in the version's directory: the tap, and the formula's file, which is
// in the tap's clone, <repository>/Library/Taps/<user>/homebrew-<name>.
func brewTap(keg string) string {
	b, err := os.ReadFile(filepath.Join(keg, "INSTALL_RECEIPT.json"))
	if err != nil {
		return ""
	}
	var receipt struct {
		Source struct {
			Tap  string `json:"tap"`
			Path string `json:"path"`
		} `json:"source"`
	}
	if json.Unmarshal(b, &receipt) != nil || receipt.Source.Tap == "" || receipt.Source.Path == "" {
		return ""
	}
	for dir := filepath.Dir(receipt.Source.Path); filepath.Dir(dir) != dir; dir = filepath.Dir(dir) {
		if filepath.Base(filepath.Dir(filepath.Dir(dir))) != "Taps" {
			continue
		}
		if goModule(filepath.Join(dir, "go.mod")) == module {
			return receipt.Source.Tap
		}
		break
	}
	return ""
}

// goModule is the module a go.mod names.
func goModule(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := bufio.NewScanner(bytes.NewReader(b))
	for lines.Scan() {
		if name, ok := strings.CutPrefix(strings.TrimSpace(lines.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(name), `"`)
		}
	}
	return ""
}
