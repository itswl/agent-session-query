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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	// How many turns after a failure are looked through for the same command succeeding
	laterTurns = 3
)

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
		list[i].Now = look.fileNow(ctx, &list[i])
	}
	return list, total, nil
}

// Failures is the index's Failures, each command with where it next succeeded in its
// session (see index.Failure.Later).
func (a *App) Failures(ctx context.Context, f index.Filter, w index.Window, tool string, p index.Page) (index.FailureReport, error) {
	rep, err := a.Index.Failures(ctx, f, w, tool, p)
	if err != nil {
		return rep, err
	}
	look := newLooker(a)
	for i := range rep.Failures {
		if ctx.Err() != nil {
			break
		}
		rep.Failures[i].Later = look.later(ctx, &rep.Failures[i])
	}
	return rep, nil
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
	err  error
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
		f.info, f.err = os.Stat(path)
		l.files[path] = f
	}
	return f
}

// content is a file's text to compare with, CRLFs as LFs; false when it is larger than
// maxCompared or cannot be read. However large its size said it was: a file can grow,
// and some (/proc's) say nothing of their size.
func (l *looker) content(path string) ([]byte, bool) {
	if path != l.lastPath {
		l.lastPath = path
		l.lastData, l.lastOK = readAtMost(path, maxCompared)
	}
	return l.lastData, l.lastOK
}

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

// fileNow says how the file a turn named stands now; nil when that cannot be said.
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
func (l *looker) fileNow(ctx context.Context, t *index.FileTouch) *index.FileNow {
	if s := l.session(ctx, t.Session); s == nil || strings.Contains(s.Source, "@") {
		return nil
	}
	path := t.Path
	if t.Cwd != "" {
		// A directory this machine does not have — or names another way: a Unix path on
		// Windows — is another machine's
		if dir := l.stat(t.Cwd); dir.err != nil || !dir.info.IsDir() {
			return nil
		}
		if !filepath.IsAbs(path) && filepath.IsAbs(t.Cwd) {
			path = filepath.Join(t.Cwd, path)
		}
	}
	if !filepath.IsAbs(path) {
		return nil
	}
	f := l.stat(path)
	if errors.Is(f.err, fs.ErrNotExist) {
		// Gone from a directory that is here; from one that is not, nothing can be said
		if dir := l.stat(filepath.Dir(path)); dir.err == nil && dir.info.IsDir() {
			return &index.FileNow{State: index.FileDeleted}
		}
		return nil
	}
	if f.err != nil || !f.info.Mode().IsRegular() {
		return nil
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
					return &index.FileNow{State: index.FileSame, By: index.ByContent}
				case held:
					return &index.FileNow{State: index.FileWritten, By: index.ByContent}
				case timed && !since:
					return &index.FileNow{State: index.FileSame, By: index.ByTime}
				default:
					return &index.FileNow{State: index.FileChanged, By: index.ByContent}
				}
			}
		}
	}
	switch {
	case !timed:
		return nil
	case since:
		return &index.FileNow{State: index.FileWritten, By: index.ByTime}
	}
	return &index.FileNow{State: index.FileSame, By: index.ByTime}
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

// later finds where the command that failed next succeeded in its session.
//
// "The command" is the program and what it was asked to do — go test, pnpm build, git
// push — read past what prepares for it (see commandKey). An agent rarely runs a failed
// command again word for word: it narrows the test, adds a flag. Measured on a real
// history, the same command word for word came back in none of 37 failures and the same
// program and subcommand in 28 of 40, all within three turns. The match is loose, so
// what passed is shown whole beside it: the reader judges whether "go test ./internal/x"
// passing says anything about "go test ./..." failing.
//
// Succeeded is taken strictly (see passed). A run begun before the failure was known —
// beside it, in the same response — is not later than it.
func (l *looker) later(ctx context.Context, f *index.Failure) *index.Later {
	key := commandKey(f.Command)
	if key == "" {
		return nil
	}
	turns := l.turns(ctx, f.Session, f.Turn, f.Turn+laterTurns)
	call := 0 // the response the failed call was part of
	for _, turn := range turns {
		for _, item := range turn.Items {
			if turn.Index == f.Turn && item.Seq == f.Seq {
				call = item.Call
			}
		}
	}
	for _, turn := range turns {
		for _, item := range turn.Items {
			t := item.Tool
			if item.Seq <= f.Seq || t == nil || t.Name != f.Tool || commandKey(t.Command) != key {
				continue
			}
			if turn.Index == f.Turn && call != 0 && item.Call == call {
				continue // asked for in the same response: run beside the failure, not after it
			}
			if !f.At.IsZero() && !item.At.IsZero() && item.At.Before(f.At) {
				continue // begun before the failure was known
			}
			if !t.Done {
				return nil // run again, and never answered: nothing is known of it
			}
			if !passed(t) {
				continue // failed again, or ended in a way that says nothing: the next run may be the one
			}
			return &index.Later{Turn: turn.Index, Seq: item.Seq, At: item.At, Command: oneSpace(t.Command)}
		}
	}
	return nil
}

// passed reports whether a call says its command succeeded: it ended well — an exit
// status of 0 where one was recorded, an "ok" where none was — the line's status was the
// command's own, at every level the line was read through (nothing after it could hide a
// failure: see resolve), and what came back was its end, not word that it runs on in the
// background. A command that outlasted a CLI's wait — Codex's — has no status, and so
// did not pass.
func passed(t *model.Tool) bool {
	switch {
	case t.Exit != nil:
		if *t.Exit != 0 {
			return false
		}
	case t.Status != model.StatusOK:
		return false
	}
	if _, decides := resolve(t.Command, 0); !decides {
		return false
	}
	return !stillRunning(t)
}

// stillRunning reports whether a call's answer is that its command was started and runs
// on: Claude Code's run_in_background, which answers ok at once.
func stillRunning(t *model.Tool) bool {
	var in struct {
		Background bool `json:"run_in_background"`
	}
	return json.Unmarshal(t.Input, &in) == nil && in.Background
}

func oneSpace(s string) string { return strings.Join(strings.Fields(s), " ") }
