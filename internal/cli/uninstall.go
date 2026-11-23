package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/itswl/agent-session-query/internal/sources"
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
	// What could not be removed is said as left, and the rest goes on: on Windows a file an
	// asq serve still running holds open is not removed until it is down, and what takes it
	// down is among what is said to run
	couldNot := func(p, what string, err error) {
		done.Left = append(done.Left, leftBehind{Path: p, What: what,
			Why: "could not be removed (" + err.Error() + "): an asq serve still running may hold it; asq uninstall again, once that is down, takes it"})
	}
	take := func(p, note string) {
		if _, err := os.Stat(p); err != nil {
			return // not here: nothing to take, nothing to say
		}
		if err := index.Remove(p); err != nil {
			couldNot(p, "index", err)
			return
		}
		removed(p, note)
	}
	why := ""
	if cfg.IndexPath != "" || os.Getenv("ASQ_INDEX") != "" {
		// A path a person named may be a file asq never wrote: it is left, not taken for one
		why = notAnIndex(ctx, path)
	}
	if why != "" {
		done.Left = append(done.Left, leftBehind{Path: path, What: "index", Why: why})
	} else {
		take(path, "the index, rebuilt from the stores on the next run")
	}
	if cfg.IndexPath == "" {
		for _, old := range index.Older() {
			take(old, "the index an earlier asq left")
		}
	}
	if cfg.IndexPath == "" && os.Getenv("ASQ_INDEX") == "" {
		// The folder of the index at the default path is asq's own, and goes once empty
		if dir := filepath.Dir(path); filepath.Base(dir) == "asq" {
			if info, err := os.Lstat(dir); err == nil && info.IsDir() {
				os.Remove(dir)
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
			// The copies are in the folder keep.json names — and in kept, the one asq keeps
			// them in by default, when they were made there before asq keep --dir named
			// another, or by an asq that had not read keep.json again since
			folders := []string{st.Folder(state)}
			if st.Dir != "" {
				folders = append(folders, filepath.Join(state, "kept"))
			}
			// What stays is each CLI's folder of copies; around them the state goes as it
			// would without them, even when asq keep --dir named the state itself
			var spares []string
			for i, folder := range folders {
				copies, held := keptCopies(ctx, cfg.Stores, folder)
				if copies == nil || i > 0 && inside(copies.Path, folders[0]) && inside(folders[0], copies.Path) {
					continue
				}
				spares = append(spares, held...)
				done.Left = append(done.Left, *copies)
			}
			if os.Getenv("ASQ_STATE") != "" {
				// A folder a person named may hold more than asq's: only what asq writes goes
				took, others, whole, err := removeOurs(state, spares)
				switch {
				case err != nil:
					for _, p := range took {
						removed(p, "asq's, in the folder ASQ_STATE names")
					}
					couldNot(state, "state", err)
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
			took, whole, err := removeBut(state, spares)
			switch {
			case err != nil:
				couldNot(state, "state", err)
			case whole:
				removed(state, "the state"+held)
			case took:
				removed(state, "the state but for the copies in it"+held)
			}
		}
	}

	for i, l := range done.Left {
		if l.What == "copies" {
			done.Left[i].Remove = removeCopies(m.OS, l.Path)
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
	switch manager, _ := packageOf(m.OS, self); {
	case manager != "":
	case m.OS == "windows":
		// The installer adds the binary's folder to the user's PATH, which no file holds
		fmt.Fprintln(io.out, "\nthe asq entry in your user PATH, added when installing, can go too")
	default:
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
	What string `json:"what"` // "copies", what asq keep made; "state" or "index", what was not asq's to take
	Why  string `json:"why"`
	// Of the copies: how many transcripts, how many of those their CLI has deleted since —
	// which are kept nowhere else —, and the command that removes them, for a person who
	// wants that too
	Transcripts int    `json:"transcripts,omitempty"`
	Gone        int    `json:"gone,omitempty"`
	Remove      string `json:"remove,omitempty"`
}

// notAnIndex says why the file at path is not an index asq wrote, or "" when it may be: a
// SQLite database that holds an index's tables alone, or an empty file, as an index is the
// moment it is made — or nothing there. A CLI's store that ASQ_INDEX was pointed at holds
// asq's tables too, which asq added to it, and tables of its own beside them.
func notAnIndex(ctx context.Context, path string) string {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return ""
	case info.IsDir():
		return "a folder, which is no index: not asq's to remove"
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, len(sqliteHeader))
	n, _ := io.ReadFull(f, head)
	switch {
	case n == 0:
		return ""
	case string(head[:n]) != sqliteHeader:
		return "not a SQLite database, which an index is: not asq's to remove"
	}
	switch foreign, err := index.Foreign(ctx, path); {
	case err != nil:
		return "a SQLite database that does not read (" + err.Error() + "): not asq's to remove"
	case len(foreign) > 0:
		return "a SQLite database with tables no index has — " + some(foreign) + " —, another program's: not asq's to remove"
	}
	return ""
}

// sqliteHeader is how every SQLite database begins.
const sqliteHeader = "SQLite format 3\x00"

// keptCopies is the folder of the copies asq keep made, said as what is left: where it is,
// what it holds — counted as asq keep counts it, by each CLI's adapter on its folder of
// copies, beside the CLI's own store —; and the folders in it that hold them, one for each
// CLI. Nil when it holds none.
func keptCopies(ctx context.Context, stores catalog.Config, folder string) (*leftBehind, []string) {
	// A folder that is a link holds what it leads to: that is where the copies are, and what
	// stays when the link goes with the state
	if info, err := os.Lstat(folder); err == nil && !info.IsDir() {
		if real, err := filepath.EvalSymlinks(folder); err == nil {
			folder = real
		}
	}
	dirs := keptIn(folder)
	var held []string
	for _, dir := range dirs {
		if n, _ := keep.Size(dir); n > 0 {
			held = append(held, dir)
		}
	}
	if len(held) == 0 {
		return nil, nil
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
	return l, held
}

// removeCopies is the command that removes the copies in folder, said once the state around
// them has gone: the folder when they are all it holds; else the CLIs' folders in it, which
// hold the copies alone — one asq keep --dir named may be a folder a person keeps more in,
// their home or a synced folder.
func removeCopies(goos, folder string) string {
	dirs := keptIn(folder)
	names := make([]string, len(dirs))
	for i, dir := range dirs {
		names[i] = filepath.Base(dir)
	}
	if holdsOnly(folder, names...) {
		dirs = []string{folder}
	}
	return removeAll(goos, dirs...)
}

// keptIn is the folders in folder that asq keep writes copies in: one for each CLI whose
// sessions it copies, named after the CLI, as the store of its copies is (catalog.Set.Keep).
func keptIn(folder string) []string {
	var dirs []string
	for _, spec := range catalog.All {
		if _, ok := spec.Open("").(sources.Keeper); !ok {
			continue
		}
		if dir := filepath.Join(folder, spec.Name); exists(dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// removeAll is the command that removes files and folders whole: rm -rf, or on Windows
// PowerShell's Remove-Item.
func removeAll(goos string, paths ...string) string {
	if goos == "windows" {
		return removeItem(paths...)
	}
	words := []string{"rm -rf"}
	for _, p := range paths {
		words = append(words, app.ShellArg(p))
	}
	return strings.Join(words, " ")
}

// removeItem is PowerShell's command that removes files or folders: by their names as they
// are, which -Path would read as patterns — a [ in one matching another folder's name.
func removeItem(paths ...string) string {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = psArg(p)
	}
	return "Remove-Item -Recurse -Force -LiteralPath " + strings.Join(quoted, ",")
}

// psArg is a word as PowerShell reads it, which runs every command said for Windows: as it
// is when it holds nothing PowerShell reads otherwise, or else in single quotes, which
// expand nothing. In double quotes, as cmd.exe has a path quoted (app.ShellArg), a $name
// in it is read as a variable and a ` as an escape, and the path names another folder. A
// quote in it is doubled; PowerShell takes the typographic ones for quotes too.
func psArg(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(`-_./\:`, r)) {
			safe = false
		}
	}
	if safe {
		return s
	}
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if strings.ContainsRune("'\u2018\u2019\u201a\u201b", r) {
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
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

// removeBut removes the directory dir and what it holds, but for the folders of copies
// spares that are in it: then what is beside them, and beside each folder on the way down
// to them, is removed, and they stay. A link on the way to one is left as it is, never
// followed into what it leads to. It reports whether it removed anything, and whether it
// removed dir whole; dir itself, when it is a folder of copies, it leaves.
func removeBut(dir string, spares []string) (took, whole bool, err error) {
	var in []string
	for _, spare := range spares {
		if inside(spare, dir) {
			if inside(dir, spare) {
				return false, false, nil
			}
			in = append(in, spare)
		}
	}
	if len(in) == 0 {
		return true, true, os.RemoveAll(dir)
	}
	took, err = removeAround(dir, in)
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
// that stays. The folders of copies spares, and what holds them, stay too, and kept goes only
// empty of files. It reports what it removed and the names of what it left that asq does
// not write, and removes the directory itself, whole, once nothing is left in it.
func removeOurs(dir string, spares []string) (took, others []string, whole bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, false, err
	}
	copies := false
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if slices.ContainsFunc(spares, func(spare string) bool { return inside(spare, p) }) {
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

func removeAround(dir string, spares []string) (took bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		var on []string // the folders of copies that are p, or in it
		for _, spare := range spares {
			if inside(spare, p) {
				on = append(on, spare)
			}
		}
		if len(on) > 0 {
			if !e.IsDir() || slices.ContainsFunc(on, func(spare string) bool { return inside(p, spare) }) {
				continue // a folder of copies, or a link to one or on the way to one
			}
			t, err := removeAround(p, on)
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
	cmds := []string{}
	add := func(lines ...string) {
		for _, line := range lines {
			// A line two services need is said once, where the last needs it: systemctl
			// daemon-reload after every unit it is to forget is removed
			cmds = append(slices.DeleteFunc(cmds, func(c string) bool { return c == line }), line)
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
	if goos != "windows" {
		return app.ShellArg(self)
	}
	cmd := psArg(self)
	if strings.HasPrefix(cmd, "'") {
		cmd = "& " + cmd // PowerShell runs a quoted path given its call operator
	}
	return cmd
}

// binaryCommands removes this very binary: by its package manager when one installed it —
// Homebrew, and the tap it came from when that is this repository; Scoop —, or else the
// file, by sudo when its directory is not this account's to write. On Windows the
// installer puts the exe in a folder of its own, asq, which goes with it when it holds the
// exe alone: one that holds more — a checkout it was built in, the copies asq keep made —
// keeps the rest.
func binaryCommands(goos, self string) []string {
	if self == "" {
		return nil
	}
	switch manager, dir := packageOf(goos, self); manager {
	case service.Homebrew:
		cmds := []string{"brew uninstall asq"}
		if tap := brewTap(dir); tap != "" {
			cmds = append(cmds, "brew untap "+app.ShellArg(tap))
		}
		return cmds
	case service.Scoop:
		if scoopGlobal(dir) {
			return []string{"scoop uninstall asq --global"}
		}
		return []string{"scoop uninstall asq"}
	}
	if goos == "windows" {
		target := self
		if dir := filepath.Dir(self); strings.EqualFold(filepath.Base(dir), "asq") && holdsOnly(dir, filepath.Base(self)) {
			target = dir
		}
		return []string{removeItem(target)}
	}
	rm := "rm " + app.ShellArg(self)
	if !canWrite(filepath.Dir(self)) {
		rm = "sudo " + rm
	}
	return []string{rm}
}

// packageOf is the package manager that installed this binary, of those goos has: Homebrew
// on macOS and Linux, Scoop on Windows. A folder of one's own may be laid out as the other's.
func packageOf(goos, self string) (manager, dir string) {
	switch manager, dir = service.Package(self); {
	case manager == service.Homebrew && goos != "windows", manager == service.Scoop && goos == "windows":
		return manager, dir
	}
	return "", ""
}

// scoopGlobal says whether Scoop installed the version in dir for every user — in its
// global folder, SCOOP_GLOBAL or %ProgramData%\scoop —, which scoop uninstall finds only
// told so, and run as an administrator.
func scoopGlobal(dir string) bool {
	global := os.Getenv("SCOOP_GLOBAL")
	if data := os.Getenv("ProgramData"); global == "" && data != "" {
		global = filepath.Join(data, "scoop")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(dir))) // <root>\apps\asq\<version>
	return global != "" && strings.EqualFold(filepath.Clean(root), filepath.Clean(global))
}

// holdsOnly says whether a directory holds something, and nothing but what is named so.
func holdsOnly(dir string, names ...string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return false
	}
	for _, e := range entries {
		if !slices.ContainsFunc(names, func(name string) bool { return strings.EqualFold(e.Name(), name) }) {
			return false
		}
	}
	return true
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
