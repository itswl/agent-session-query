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
// opened, however many rows name it.
type looker struct {
	a        *App
	sessions map[string]*index.Session
	read     map[turnKey]*model.Turn
	files    map[string]*fileSeen
}

type turnKey struct {
	session string
	idx     int
	full    bool
}

type fileSeen struct {
	info os.FileInfo
	err  error
	data []byte
	read bool
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
func (l *looker) turns(ctx context.Context, ref string, first, last int, full bool) []*model.Turn {
	s := l.session(ctx, ref)
	if s == nil {
		return nil
	}
	missing := false
	for i := first; i <= last && !missing; i++ {
		_, ok := l.read[turnKey{ref, i, full}]
		missing = !ok
	}
	if missing {
		got, err := l.a.Index.Turns(ctx, s, index.TurnRange{First: first, Last: last, Full: full})
		if err != nil {
			return nil
		}
		for i := first; i <= last; i++ {
			l.read[turnKey{ref, i, full}] = nil // a turn the session does not have is not asked for again
		}
		for i := range got {
			l.read[turnKey{ref, got[i].Index, full}] = &got[i]
		}
	}
	var out []*model.Turn
	for i := first; i <= last; i++ {
		if t := l.read[turnKey{ref, i, full}]; t != nil {
			out = append(out, t)
		}
	}
	return out
}

func (l *looker) file(path string, data bool) *fileSeen {
	f := l.files[path]
	if f == nil {
		f = &fileSeen{}
		f.info, f.err = os.Stat(path)
		l.files[path] = f
	}
	if data && !f.read && f.err == nil && f.info.Mode().IsRegular() && f.info.Size() <= maxCompared {
		f.data, f.err = os.ReadFile(path)
		f.data = bytes.ReplaceAll(f.data, []byte("\r\n"), []byte("\n"))
		f.read = true
	}
	return f
}

// fileNow says how the file a turn named stands now; nil when that cannot be said.
func (l *looker) fileNow(ctx context.Context, t *index.FileTouch) *index.FileNow {
	path := t.Path
	if !filepath.IsAbs(path) {
		if t.Cwd == "" || !filepath.IsAbs(t.Cwd) {
			return nil
		}
		path = filepath.Join(t.Cwd, path)
	}
	f := l.file(path, false)
	if errors.Is(f.err, fs.ErrNotExist) {
		// Gone from a directory that is here; from one that is not, it is another
		// machine's file, or a container's, and nothing can be said of it
		if dir := l.file(filepath.Dir(path), false); dir.err == nil && dir.info.IsDir() {
			return &index.FileNow{State: index.FileDeleted}
		}
		return nil
	}
	if f.err != nil || !f.info.Mode().IsRegular() {
		return nil
	}
	if t.Changed {
		if whole, fragments, ok := l.leftBy(ctx, t); ok {
			if f = l.file(path, true); f.read && f.err == nil {
				return &index.FileNow{State: compare(f.data, whole, fragments), By: index.ByContent}
			}
		}
	}
	end := t.Ended
	if end.IsZero() {
		end = t.At
	}
	if end.IsZero() {
		return nil
	}
	if f.info.ModTime().After(end.Add(slack)) {
		return &index.FileNow{State: index.FileWritten, By: index.ByTime}
	}
	return &index.FileNow{State: index.FileSame, By: index.ByTime}
}

// compare says whether the file still holds what the turn left in it.
func compare(data []byte, whole *string, fragments []string) string {
	text := string(data)
	if whole != nil {
		if strings.TrimRight(text, "\n") == strings.TrimRight(*whole, "\n") {
			return index.FileSame
		}
		return index.FileChanged
	}
	for _, frag := range fragments {
		if !strings.Contains(text, frag) {
			return index.FileChanged
		}
	}
	return index.FileSame
}

// leftBy is what a turn's last write to a file put there: the whole file, or the
// fragments it inserted. ok is false when that write said nothing that can be looked
// for — it only removed text, or its input is in a shape not known here.
func (l *looker) leftBy(ctx context.Context, t *index.FileTouch) (whole *string, fragments []string, ok bool) {
	turns := l.turns(ctx, t.Session, t.Turn, t.Turn, true)
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
		// last may have replaced
		return written(tool.Input, t.Path)
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
// push — past a cd and the variables set in front of it. An agent rarely runs a failed
// command again word for word: it narrows the test, adds a flag, pipes the output to
// tail. Measured on a real history, the same command word for word came back in none of
// 37 failures and the same program and subcommand in 28 of 40, all within three turns.
// The match is loose, so what passed is shown whole beside it: the reader judges whether
// "go test ./internal/x" passing says anything about "go test ./..." failing.
func (l *looker) later(ctx context.Context, f *index.Failure) *index.Later {
	key := commandKey(f.Command)
	if key == "" {
		return nil
	}
	for _, turn := range l.turns(ctx, f.Session, f.Turn, f.Turn+laterTurns, false) {
		for _, item := range turn.Items {
			t := item.Tool
			if item.Seq <= f.Seq || t == nil || t.Name != f.Tool || commandKey(t.Command) != key {
				continue
			}
			if !t.Done {
				return nil // run again, and never answered: nothing is known of it
			}
			if t.Failed() {
				continue // failed again: the next run may be the one
			}
			return &index.Later{Turn: turn.Index, Seq: item.Seq, At: item.At, Command: oneSpace(t.Command)}
		}
	}
	return nil
}

// commandKey is the program a command line runs and its first argument, when that is a
// word and not a flag: "go test", "git push", "make", "./run.sh". A cd and variables
// set in front are passed over; a flag, a pipe or a redirection ends it.
func commandKey(command string) string {
	words := strings.Fields(command)
	for len(words) > 0 {
		switch {
		case words[0] == "cd" && len(words) > 2 && (words[2] == "&&" || words[2] == ";"):
			words = words[3:]
			continue
		case words[0] == "cd" && len(words) > 1 && strings.HasSuffix(words[1], ";"):
			words = words[2:] // cd /work/api; make lint
			continue
		case strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "-"):
			words = words[1:] // FOO=1 go test
			continue
		}
		break
	}
	var out []string
	for _, w := range words {
		if strings.HasPrefix(w, "-") || strings.ContainsAny(w, "|;&<>()$`") {
			break
		}
		out = append(out, w)
		if len(out) == 2 {
			break
		}
	}
	return strings.Join(out, " ")
}

func oneSpace(s string) string { return strings.Join(strings.Fields(s), " ") }
