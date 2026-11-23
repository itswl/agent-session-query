package app

// What the record says against how things are now.
//
// A session three days old says a turn changed auth.go. An agent that takes that account
// and acts on it may be acting on a file someone has since rewritten: the most common way
// for a recollection to mislead is to be out of date. The index knows only what the
// transcripts say; this side of the program can look at the file. So the files a turn
// changed carry how they stand now, and the evidence for it.
//
// The evidence is the content where there is one. A turn's last write to a file records
// what it put there — the text an edit inserted, the whole of a file it wrote, the lines a
// patch added — and either that text is still in the file or it is not. A file's
// modification time is the fallback, and says less: a checkout or a formatter writes a
// file as surely as an edit does. Git's history would say less still: the edit an agent
// made is committed after it, and would read as a change.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
)

const (
	// A file larger than this is judged by its time alone: comparing text is for source
	// files, and a turn's account of a 50 MB log is not what anyone checks
	maxCompared = 8 << 20
	// A fragment shorter than this, in characters that are not space, proves nothing by
	// being found: "}" is in every file
	minFragment = 4
	// Writes within this long of the turn's end are the turn's own: a modification time
	// and a transcript's timestamp are not taken from one clock to the millisecond
	slack = 2 * time.Second
)

// lookWait is how long one look at a file is waited for.
//
// A look can take as long as something else decides. On a Mac, the first look at a file in
// a folder the system guards — Documents, Desktop, Downloads, iCloud Drive, another disk —
// asks the person at the Mac whether asq may read it, and the look waits for the answer: a
// question asked from a phone gets none. A network disk that went away waits as long. An
// answer does not wait with them: a look not back by lookWait is left to finish on its own,
// its file is not judged, and every other look in the same folder is skipped until it does
// (see guarded) — one look waits, however many questions are asked meanwhile.
var lookWait = 400 * time.Millisecond

// osStat is how a file is looked at: os.Stat, or a stand-in that waits, in a test.
var osStat = os.Stat

// errWaiting is a look not back in time, or not made since another in its folder is not.
var errWaiting = errors.New("a look in this folder is waiting")

// waits are the folders a look is waiting in, with how many are.
type waits struct {
	mu     sync.Mutex
	folder map[string]int
}

// look runs see on path within lookWait, unless a look in its folder is waiting already.
func look[T any](a *App, path string, see func() (T, error)) (T, error) {
	var zero T
	folder := guarded(path)
	a.waits.mu.Lock()
	busy := a.waits.folder[folder] > 0
	a.waits.mu.Unlock()
	if busy {
		return zero, errWaiting
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := see()
		done <- result{v, err}
	}()
	timer := time.NewTimer(lookWait)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.v, r.err
	case <-timer.C:
	}
	a.waits.mu.Lock()
	if a.waits.folder == nil {
		a.waits.folder = map[string]int{}
	}
	a.waits.folder[folder]++
	first := a.waits.folder[folder] == 1
	a.waits.mu.Unlock()
	if first {
		fmt.Fprintf(os.Stderr, "asq: looking at a file in %s is waiting: on a Mac, macOS may be asking on its screen whether asq may read that folder; its files are not judged until it is answered\n", folder)
	}
	go func() {
		<-done
		a.waits.mu.Lock()
		if a.waits.folder[folder]--; a.waits.folder[folder] <= 0 {
			delete(a.waits.folder, folder)
		}
		a.waits.mu.Unlock()
	}()
	return zero, errWaiting
}

// guarded is the folder a look at path waits in: the one a privacy question names — under
// the home directory its first folder (in Library, the second: ~/Library/Mobile Documents),
// elsewhere the first two (/Volumes/Disk) — so that one question holds back the looks it is
// about, and not the rest.
func guarded(path string) string {
	clean := filepath.Clean(path)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, clean); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			parts := strings.Split(rel, string(filepath.Separator))
			n := 1
			if parts[0] == "Library" && len(parts) > 2 {
				n = 2
			}
			return filepath.Join(append([]string{home}, parts[:n]...)...)
		}
	}
	vol := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean[len(vol):], string(filepath.Separator))
	parts := strings.SplitN(rest, string(filepath.Separator), 3)
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return vol + string(filepath.Separator) + filepath.Join(parts...)
}

// Files is the index's Files, each file with how it stands now (see index.FileNow).
func (a *App) Files(ctx context.Context, f index.Filter, w index.Window, path string, changedOnly bool, p index.Page) ([]index.FileTouch, int, error) {
	list, total, err := a.Index.Files(ctx, f, w, path, changedOnly, p)
	if err != nil {
		return list, total, err
	}
	look := newLooker(a)
	for i := range list {
		if ctx.Err() != nil {
			break
		}
		list[i].Now, list[i].Waiting = look.fileNow(ctx, &list[i])
	}
	return list, total, nil
}

// Search is the index's Search, each hit whose turn had calls fail saying how many of them
// were followed by the same command succeeding (index.Hit.PassedLater): what a reader
// weighing which turn to open wants to know, and would otherwise open each to learn.
func (a *App) Search(ctx context.Context, q index.Query, f index.Filter, o index.SearchOptions) (index.SearchResult, error) {
	res, err := a.Index.Search(ctx, q, f, o)
	if err != nil {
		return res, err
	}
	for i := range res.Hits {
		h := &res.Hits[i]
		if h.Failed == 0 {
			continue
		}
		if h.PassedLater, err = a.Index.PassedLater(ctx, &h.Session, h.Turn); err != nil {
			return res, err
		}
	}
	return res, nil
}

// looker reads what one answer needs once: a session resolved, a turn read, a file
// looked at, however many rows name it. Turns are read as the index keeps them, with
// previews; what one row needs whole is read alone.
type looker struct {
	a        *App
	sessions map[string]*index.Session
	read     map[turnKey]*model.Turn
	files    map[string]*fileSeen
	// The last file read, for the rows of one file that follow one another
	lastPath string
	lastData []byte
	lastOK   bool
}

type turnKey struct {
	session string
	idx     int
}

type fileSeen struct {
	info os.FileInfo
	err  error // errWaiting: the look did not come back in time
}

func newLooker(a *App) *looker {
	return &looker{a: a, sessions: map[string]*index.Session{}, read: map[turnKey]*model.Turn{}, files: map[string]*fileSeen{}}
}

func (l *looker) session(ctx context.Context, ref string) *index.Session {
	if s, ok := l.sessions[ref]; ok {
		return s
	}
	var found *index.Session
	if s, _, err := l.a.Resolve(ctx, ref); err == nil {
		found = &s
	}
	l.sessions[ref] = found
	return found
}

// turns reads turns first..last of a session, from what it has already read where it can.
func (l *looker) turns(ctx context.Context, ref string, first, last int) []*model.Turn {
	s := l.session(ctx, ref)
	if s == nil {
		return nil
	}
	missing := false
	for i := first; i <= last && !missing; i++ {
		_, ok := l.read[turnKey{ref, i}]
		missing = !ok
	}
	if missing {
		got, err := l.a.Index.Turns(ctx, s, index.TurnRange{First: first, Last: last})
		if err != nil {
			return nil
		}
		for i := first; i <= last; i++ {
			l.read[turnKey{ref, i}] = nil // a turn the session does not have is not asked for again
		}
		for i := range got {
			l.read[turnKey{ref, got[i].Index}] = &got[i]
		}
	}
	var out []*model.Turn
	for i := first; i <= last; i++ {
		if t := l.read[turnKey{ref, i}]; t != nil {
			out = append(out, t)
		}
	}
	return out
}

func (l *looker) stat(path string) *fileSeen {
	f := l.files[path]
	if f == nil {
		f = &fileSeen{}
		f.info, f.err = look(l.a, path, func() (os.FileInfo, error) { return osStat(path) })
		l.files[path] = f
	}
	return f
}

// content is a file's text to compare with, CRLFs as LFs; false when it is larger than
// maxCompared or cannot be read in time. However large its size said it was: a file can
// grow, and some (/proc's) say nothing of their size.
func (l *looker) content(path string) ([]byte, bool) {
	if path != l.lastPath {
		l.lastPath = path
		data, err := look(l.a, path, func() ([]byte, error) {
			if data, ok := readAtMost(path, maxCompared); ok {
				return data, nil
			}
			return nil, errUnread
		})
		l.lastData, l.lastOK = data, err == nil
	}
	return l.lastData, l.lastOK
}

var errUnread = errors.New("not read")

func readAtMost(path string, most int64) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, most+1))
	if err != nil || int64(len(data)) > most {
		return nil, false
	}
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), true
}

// fileNow says how the file a turn named stands now; nil when that cannot be said, with the
// folder a look is waiting in when that is why.
//
// Both kinds of evidence are weighed. What the turn's last write put there is either
// still in the file or not; and the file has been written since the turn ended, or not.
// Text still there in a file not written since is as the turn left it. Text gone from a
// file not written since was changed within the turn — by the turn, or a formatter it ran
// — and the file is as the turn ended it. A file written since is "written" while its
// text is all there, and "changed" once it is not; it is as the turn left it only when
// the turn wrote it whole and it is that, to the byte.
//
// Only this machine's files are judged: not those of a labelled store, which is as a rule
// another machine's, nor those of a session whose directory is not here.
func (l *looker) fileNow(ctx context.Context, t *index.FileTouch) (*index.FileNow, string) {
	if s := l.session(ctx, t.Session); s == nil || strings.Contains(s.Source, "@") {
		return nil, ""
	}
	path := t.Path
	if t.Cwd != "" {
		// A directory this machine does not have — or names another way: a Unix path on
		// Windows — is another machine's
		dir := l.stat(t.Cwd)
		if errors.Is(dir.err, errWaiting) {
			return nil, guarded(t.Cwd)
		}
		if dir.err != nil || !dir.info.IsDir() {
			return nil, ""
		}
		if !filepath.IsAbs(path) && filepath.IsAbs(t.Cwd) {
			path = filepath.Join(t.Cwd, path)
		}
	}
	if !filepath.IsAbs(path) {
		return nil, ""
	}
	f := l.stat(path)
	if errors.Is(f.err, errWaiting) {
		return nil, guarded(path)
	}
	if errors.Is(f.err, fs.ErrNotExist) {
		// Gone from a directory that is here; from one that is not, nothing can be said
		dir := l.stat(filepath.Dir(path))
		if errors.Is(dir.err, errWaiting) {
			return nil, guarded(path)
		}
		if dir.err == nil && dir.info.IsDir() {
			return &index.FileNow{State: index.FileDeleted}, ""
		}
		return nil, ""
	}
	if f.err != nil || !f.info.Mode().IsRegular() {
		return nil, ""
	}
	end := t.Ended
	if end.IsZero() {
		end = t.At
	}
	timed := !end.IsZero()
	since := timed && f.info.ModTime().After(end.Add(slack))
	if t.Changed {
		if whole, fragments, ok := l.leftBy(ctx, t); ok {
			if data, ok := l.content(path); ok {
				held := holds(data, whole, fragments)
				switch {
				case held && (whole != nil || !since):
					return &index.FileNow{State: index.FileSame, By: index.ByContent}, ""
				case held:
					return &index.FileNow{State: index.FileWritten, By: index.ByContent}, ""
				case timed && !since:
					return &index.FileNow{State: index.FileSame, By: index.ByTime}, ""
				default:
					return &index.FileNow{State: index.FileChanged, By: index.ByContent}, ""
				}
			}
		}
	}
	switch {
	case !timed:
		return nil, ""
	case since:
		return &index.FileNow{State: index.FileWritten, By: index.ByTime}, ""
	}
	return &index.FileNow{State: index.FileSame, By: index.ByTime}, ""
}

// holds says whether the file still holds what the turn left in it.
func holds(data []byte, whole *string, fragments []string) bool {
	if whole != nil {
		return bytes.Equal(bytes.TrimRight(data, "\n"), []byte(strings.TrimRight(*whole, "\n")))
	}
	for _, frag := range fragments {
		if !bytes.Contains(data, []byte(frag)) {
			return false
		}
	}
	return true
}

// leftBy is what a turn's last write to a file put there: the whole file, or the
// fragments it inserted. ok is false when that write said nothing that can be looked
// for — it only removed text, or its input is in a shape not known here.
func (l *looker) leftBy(ctx context.Context, t *index.FileTouch) (whole *string, fragments []string, ok bool) {
	turns := l.turns(ctx, t.Session, t.Turn, t.Turn)
	if len(turns) == 0 {
		return nil, nil, false
	}
	items := turns[0].Items
	for i := len(items) - 1; i >= 0; i-- {
		tool := items[i].Tool
		if tool == nil || tool.Kind != model.ToolWrite || tool.Failed() || !names(tool.Files, t.Path) {
			continue
		}
		// The last write is what the turn left; whatever an earlier one put there, the
		// last may have replaced. Its input is read whole when the turn keeps a preview.
		input := tool.Input
		if tool.InBytes > 0 {
			s := l.session(ctx, t.Session)
			if s == nil {
				return nil, nil, false
			}
			whole, ok, err := l.a.Index.Input(ctx, s, items[i].Seq)
			if err != nil || !ok {
				return nil, nil, false
			}
			input = whole
		}
		return written(input, t.Path)
	}
	return nil, nil, false
}

func names(files []string, path string) bool {
	for _, f := range files {
		if f == path {
			return true
		}
	}
	return false
}

// The keys a write's input puts its text under, across the CLIs: the text an edit
// inserts, and the whole of a file written.
var (
	fragmentKeys = []string{"new_string", "newString", "new_str", "newText", "new_text", "replacement", "newContent"}
	listKeys     = []string{"edits", "changes"}
	wholeKeys    = []string{"content", "contents", "file_text", "fileText", "text"}
)

// written reads what a write put in the file from the call's input.
func written(input json.RawMessage, path string) (whole *string, fragments []string, ok bool) {
	var v any
	if json.Unmarshal(input, &v) != nil {
		return nil, nil, false
	}
	// A patch names its files itself, wherever in the input it sits
	if patch := findPatch(v); patch != "" {
		return fromPatch(patch, path)
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return nil, nil, false
	}
	add := func(s string) {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		if len(strings.Join(strings.Fields(s), "")) >= minFragment {
			fragments = append(fragments, s)
		}
	}
	seen := false
	for _, k := range fragmentKeys {
		if s, ok := m[k].(string); ok {
			seen = true
			add(s)
		}
	}
	for _, k := range listKeys {
		list, _ := m[k].([]any)
		for _, e := range list {
			if em, ok := e.(map[string]any); ok {
				for _, fk := range fragmentKeys {
					if s, ok := em[fk].(string); ok {
						seen = true
						add(s)
					}
				}
			}
		}
	}
	if seen {
		return nil, fragments, len(fragments) > 0
	}
	for _, k := range wholeKeys {
		if s, ok := m[k].(string); ok {
			s = strings.ReplaceAll(s, "\r\n", "\n")
			return &s, nil, true
		}
	}
	return nil, nil, false
}

// findPatch finds a patch in the form Codex writes it, anywhere in a call's input: the
// input itself, a field of it, or the here-document of a shell command.
func findPatch(v any) string {
	switch v := v.(type) {
	case string:
		if i := strings.Index(v, "*** Begin Patch"); i >= 0 {
			return v[i:]
		}
	case []any:
		for _, e := range v {
			if p := findPatch(e); p != "" {
				return p
			}
		}
	case map[string]any:
		for _, e := range v {
			if p := findPatch(e); p != "" {
				return p
			}
		}
	}
	return ""
}

// fromPatch is what a patch left in one of its files: the whole of a file it added, or
// the runs of lines it added to one it updated.
func fromPatch(patch, path string) (whole *string, fragments []string, ok bool) {
	var (
		in    bool
		added bool
		lines []string
		run   []string
	)
	flush := func() {
		if len(run) > 0 {
			s := strings.Join(run, "\n")
			if len(strings.Join(strings.Fields(s), "")) >= minFragment {
				fragments = append(fragments, s)
			}
			run = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "*** ") {
			flush()
			if in {
				if strings.HasPrefix(line, "*** Move to: ") {
					return nil, nil, false // the file was moved: what is at the old path is not this
				}
				if !strings.HasPrefix(line, "*** End of File") {
					break
				}
				continue
			}
			for _, head := range []string{"*** Update File: ", "*** Add File: "} {
				if name, found := strings.CutPrefix(line, head); found && samePath(strings.TrimSpace(name), path) {
					in, added = true, head == "*** Add File: "
				}
			}
			continue
		}
		if !in {
			continue
		}
		if rest, plus := strings.CutPrefix(line, "+"); plus {
			run = append(run, rest)
			lines = append(lines, rest)
			continue
		}
		flush()
	}
	flush()
	if !in {
		return nil, nil, false
	}
	if added {
		s := strings.Join(lines, "\n")
		return &s, nil, true
	}
	return nil, fragments, len(fragments) > 0
}

// samePath reports whether a patch's name for a file is the path the index holds: the
// same, or the end of it (a patch names files relative to where it was applied).
func samePath(name, path string) bool {
	if name == path {
		return true
	}
	name = filepath.ToSlash(filepath.Clean(name))
	return strings.HasSuffix(filepath.ToSlash(path), "/"+strings.TrimPrefix(name, "./"))
}
