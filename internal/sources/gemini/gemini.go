// Package gemini reads Gemini CLI's sessions.
//
//	<root>/<project>/.project_root                         the directory the project is
//	<root>/<project>/chats/session-<time>-<id>.jsonl       one file of a conversation
//	<root>/<project>/chats/<session id>/<agent id>.jsonl   a subagent the session ran
//
// A file is an append log: a metadata first line (sessionId, startTime, kind …), message
// rows (type "user" and "gemini"; the others are bookkeeping), {"$set": …} rows, which move
// the metadata's lastUpdated on and may carry messages of their own, {"$rewindTo": id}
// rows, which /rewind writes, and {"$patch": …} rows, which bring the messages in line
// with the model's history.
//
// --resume appends to the file it continues (measured, 0.68). A conversation can still be
// more than one file: earlier versions continued a session in a new file that opens with
// the same sessionId (measured: two files a minute apart), so the conversation — not the
// file — is what a Ref names, and Read walks every file of it in order.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Spec describes Gemini CLI.
var Spec = sources.Spec{
	Name:  "gemini",
	Title: "Gemini CLI",
	// Gemini CLI has no variable that moves its data: ~/.gemini is where it always writes
	Locate:  func(env sources.Env) string { return filepath.Join(env.Home, ".gemini", "tmp") },
	Open:    func(location string) sources.Source { return &Source{root: location} },
	Movable: true,
	// --resume takes a session's id as well as "latest" or an index, and says so on exit
	// ("To resume this session: gemini --resume <id>"). It looks for the id among the
	// sessions of the directory it is started in, which is why Read gives the session the
	// project's directory as its own.
	Resume:     "gemini --resume",
	ResumeHere: true,
	// A file with no metadata line goes by its own name (session-<time>-<id>), which is
	// not an id --resume takes
	Resumable: func(id string) bool { return !strings.HasPrefix(id, "session-") },
	Reads:     2,
	Store:     "~/.gemini/tmp/<project>/chats/session-*.jsonl",
	// Gemini CLI sweeps the chats of the project it starts in, by the time each was last
	// updated (packages/cli/src/utils/sessionCleanup.ts): a project it is not started in
	// again keeps its old sessions
	Retention: "Gemini CLI deletes a project's sessions not used for 30 days when it next starts in that project (general.sessionRetention in ~/.gemini/settings.json)",
}

// Source is a Gemini CLI tmp directory.
type Source struct {
	root string

	mu sync.Mutex
	// heads remembers each file's session id at the fingerprint it was read at. Which files
	// belong together takes every file's first line, and Scan runs every second or two: an
	// untouched store costs a stat per file, not an open.
	heads map[string]head
	// groups is the files of each conversation, in order, as the last Scan found them, by
	// the Key of the conversation's Ref — a Read is handed the Ref and nothing else.
	groups map[string][]string
}

type head struct{ stamp, id string }

func (s *Source) Location() string { return s.root }
func (s *Source) Exists() bool     { return kit.Exists(s.root) }

// file is one session file as a scan saw it.
type file struct {
	path  string
	stamp string
	mod   time.Time
}

// conversation is the files that share a session id.
type conversation struct {
	id    string
	files []file
}

// Scan lists conversations, one Ref each, whatever number of files they are in. The Key is
// the conversation's first file; the Stamp is the stamps of all of them, so a line appended
// to any moves it.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	convs, unread, err := s.walk(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]sources.Ref, 0, len(convs))
	for _, c := range convs {
		stamps := make([]string, len(c.files))
		var mod time.Time
		for i, f := range c.files {
			stamps[i] = f.stamp
			if f.mod.After(mod) {
				mod = f.mod
			}
		}
		refs = append(refs, sources.Ref{Key: c.files[0].path, Stamp: strings.Join(stamps, "|"), ModTime: mod})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Key < refs[j].Key })
	return refs, unread
}

// walk globs the store and groups its files by session id. Its second result names the
// project directories it could not read, whose sessions are missing from the first.
//
// A subagent's transcript is a conversation of its own, one file under the directory of
// the session that ran it, and is grouped with nothing: its id is new each time an agent
// runs, so there is no other file to look for.
func (s *Source) walk(ctx context.Context) ([]*conversation, error, error) {
	matches, unread := kit.Glob(filepath.Join(s.root, "*", "chats", "session-*.jsonl"))
	agents, agentsUnread := kit.Glob(filepath.Join(s.root, "*", "chats", "*", "*.jsonl"))
	unread = errors.Join(unread, agentsUnread)
	s.mu.Lock()
	known := s.heads
	s.mu.Unlock()

	heads := make(map[string]head, len(matches))
	byID := map[string]*conversation{}
	var convs []*conversation
	for i, path := range agents {
		if i&63 == 63 && ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if stamp, mod := kit.Stamp(path); stamp != "-" {
			convs = append(convs, &conversation{id: path, files: []file{{path: path, stamp: stamp, mod: mod}}})
		}
	}
	for i, path := range matches {
		if i&63 == 63 && ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		stamp, mod := kit.Stamp(path)
		if stamp == "-" {
			continue // gone since the glob
		}
		h, ok := known[path]
		if !ok || h.stamp != stamp {
			// The fingerprint is the one taken before the read: a line appended meanwhile
			// moves it, and the next scan reads again
			h = head{stamp: stamp, id: sessionIDOf(path)}
		}
		heads[path] = h
		// A file with no metadata line (2 of 33 measured: its first line is a message) has
		// nothing to join on, and is its own session under its own name
		id := kit.First(h.id, stemOf(path))
		c := byID[id]
		if c == nil {
			c = &conversation{id: id}
			byID[id] = c
			convs = append(convs, c)
		}
		c.files = append(c.files, file{path: path, stamp: stamp, mod: mod})
	}

	groups := make(map[string][]string, len(convs))
	for _, c := range convs {
		// The file name opens with the time it was started, so by name is by time — and
		// stays so for a conversation whose files sit under different projects
		sort.Slice(c.files, func(i, j int) bool {
			a, b := c.files[i].path, c.files[j].path
			if x, y := filepath.Base(a), filepath.Base(b); x != y {
				return x < y
			}
			return a < b
		})
		paths := make([]string, len(c.files))
		for i, f := range c.files {
			paths[i] = f.path
		}
		groups[paths[0]] = paths
	}
	s.mu.Lock()
	s.heads, s.groups = heads, groups
	s.mu.Unlock()
	return convs, unread, nil
}

// filesOf is the files of the conversation whose Ref has this Key. A scan has just said so;
// a Read without one — or after the store moved — looks again, and a file that belongs to
// no group is its own conversation.
func (s *Source) filesOf(ctx context.Context, key string) ([]string, error) {
	s.mu.Lock()
	paths, ok := s.groups[key]
	s.mu.Unlock()
	if ok {
		return paths, nil
	}
	if _, _, err := s.walk(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	paths, ok = s.groups[key]
	s.mu.Unlock()
	if ok {
		return paths, nil
	}
	return []string{key}, nil
}

func stemOf(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// headLimit bounds how much of a file's start is looked at for its metadata line. The
// line is a few hundred bytes; one that does not fit is a message, and says no session id.
const headLimit = 16 << 10

// sessionIDOf is the sessionId a file's metadata names, and "" when it names none. The
// metadata is nearly always the first line, but not always: a file that continues a
// conversation can open with a patch and name its session on the line after. So the first
// few lines are looked at, and no more — without a bound, one file missing its metadata
// would turn "list the sessions" into a read of the whole file.
func sessionIDOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, headLimit)
	for looked := 0; looked < headLines; {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// A line that does not fit is a message; the metadata, if any, came before it
			return ""
		}
		if line = bytes.TrimSpace(line); len(line) > 0 {
			looked++
			var meta struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(line, &meta) == nil && meta.SessionID != "" {
				return meta.SessionID
			}
		}
		if err != nil {
			return ""
		}
	}
	return ""
}

// headLines is how many lines of a file are looked at for its session id.
const headLines = 5

// projectDir is the project directory a file sits under: the one that holds chats/,
// whether the file is a session's or, a level down, a subagent's.
func projectDir(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "chats" {
		dir = filepath.Dir(dir)
	}
	return filepath.Dir(dir)
}

// parentOf is the id of the session a subagent's file belongs to — the directory it sits
// in — and "" for a session's own file.
func parentOf(path string) string {
	if dir := filepath.Base(filepath.Dir(path)); dir != "chats" {
		return dir
	}
	return ""
}

// rootLimit bounds how much of a .project_root is read: it holds one path.
const rootLimit = 4 << 10

// cwdOf is the directory a project's sessions ran in. Gemini CLI names a project's
// directory after the directory's base name (or a hash of its path, in earlier versions)
// and writes the path itself into .project_root beside chats/, to tell projects of the
// same name apart. Without that file, or with something in it that is not a path, the
// project's own name is what there is.
func cwdOf(project string) string {
	if f, err := os.Open(filepath.Join(project, ".project_root")); err == nil {
		defer f.Close()
		b, _ := io.ReadAll(io.LimitReader(f, rootLimit))
		if root := strings.TrimSpace(string(b)); !strings.ContainsAny(root, "\n\x00") && absolute(root) {
			return root
		}
	}
	return filepath.Base(project)
}

// absolute says whether a path written on any system is absolute: a store may have been
// copied from another.
func absolute(path string) bool {
	switch {
	case strings.HasPrefix(path, "/"), strings.HasPrefix(path, `\\`):
		return true
	case len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/'):
		c := path[0] | 0x20
		return 'a' <= c && c <= 'z'
	}
	return false
}

// row is one line of a session file. It is a message when it has a type, the metadata when
// it has a sessionId, a patch when it has $set — a patch's messages are rows of their own,
// written inside it — a rewind when it has $rewindTo, and a message patch when it has
// $patch.
type row struct {
	message
	SessionID   string `json:"sessionId"`
	StartTime   string `json:"startTime"`
	LastUpdated string `json:"lastUpdated"`
	Set         *struct {
		LastUpdated string            `json:"lastUpdated"`
		Messages    []json.RawMessage `json:"messages"`
	} `json:"$set"`
	RewindTo string          `json:"$rewindTo"`
	Patch    json.RawMessage `json:"$patch"`
}

// unknownRow names a line that is none of the rows above, for Writer.Unread: by its
// operator when it is a patch of a kind not known here ({"$truncate": …}), and as
// "(untyped)" when it is a record that does not say what it is — a message without a type.
// A row of a known kind whose fields say nothing (a $set of null, an empty session id) is
// not of an unknown kind, and neither is a line with nothing in it; both are "".
func unknownRow(line []byte) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil || len(fields) == 0 {
		return ""
	}
	for _, known := range []string{"$set", "$rewindTo", "$patch", "sessionId", "type"} {
		if _, ok := fields[known]; ok {
			return ""
		}
	}
	var ops []string
	for key := range fields {
		if strings.HasPrefix(key, "$") {
			ops = append(ops, key)
		}
	}
	if len(ops) > 0 {
		return slices.Min(ops)
	}
	return "(untyped)"
}

// message is a user or gemini row. content is an array of Gemini API parts ({text},
// {functionResponse}, {inlineData} …) on a user row and a string on a gemini row — parts
// too, where the row was rewritten from the model's history. Gemini CLI also takes a lone
// part for the array, and a string among the parts for a text part, and so does Read.
// thoughts, tokens and toolCalls have changed shape between Gemini CLI's versions and are
// read loosely. displayContent, on a user row, is what the person typed, where the CLI sent
// the model more than that.
type message struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	Timestamp      string `json:"timestamp"`
	Model          string `json:"model"`
	Content        any    `json:"content"`
	DisplayContent any    `json:"displayContent"`
	Thoughts       any    `json:"thoughts"`
	Tokens         any    `json:"tokens"`
	ToolCalls      any    `json:"toolCalls"`
}

// Read parses one conversation, every file of it in order.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	files, err := s.filesOf(ctx, ref.Key)
	if err != nil {
		return err
	}
	w.S.Path = files[0]
	// A session file records no working directory: its project does, beside it
	w.S.Cwd = cwdOf(projectDir(files[0]))
	if parent := parentOf(files[0]); parent != "" {
		// What kind of agent it was is in the call that started it, in the parent's file:
		// the subagent's own says only whose it is
		w.S.Parent = &model.Parent{Session: parent}
	}

	rd := &reader{w: w, answered: map[string]bool{}, issued: map[string]bool{}, statuses: map[string]string{}}
	var meta time.Time // the latest time the metadata lines claim, the last resort for "updated"
	// The file is a log of writes, and a message can be written more than once: a patch
	// that sets the messages again, a row rewritten when its tool call finishes. A
	// message is what its id last said, where its id first stood — read any other way,
	// a response written twice is two responses and its tokens are spent twice.
	var msgs []*message
	at := map[string]int{}
	keep := func(m *message) {
		if i, seen := at[m.ID]; seen && m.ID != "" {
			msgs[i] = m
			return
		}
		at[m.ID] = len(msgs)
		msgs = append(msgs, m)
	}
	unknown := func(record []byte) {
		if kind := unknownRow(record); kind != "" {
			w.Unread("row:" + kind)
		}
	}
	n := 0
	for _, path := range files {
		err := kit.Lines(path, func(line []byte) bool {
			if n++; n&1023 == 0 && ctx.Err() != nil {
				return false
			}
			var r row
			if json.Unmarshal(line, &r) != nil {
				return true
			}
			switch {
			case r.Set != nil:
				if at := kit.Time(r.Set.LastUpdated); !at.IsZero() {
					rd.last = at
				}
				// The rest of what a patch sets — the summary the session list shows, the
				// directories added to the workspace, the memory scratchpad — is not
				// something said in the conversation
				for _, raw := range r.Set.Messages {
					var m message
					switch {
					case json.Unmarshal(raw, &m) != nil:
					case m.Type != "":
						keep(&m)
					default:
						unknown(raw)
					}
				}
			case r.SessionID != "":
				if w.S.ID == "" {
					w.S.ID = r.SessionID
				}
				if start := kit.Time(r.StartTime); !start.IsZero() {
					if w.S.StartedAt.IsZero() {
						w.S.StartedAt = start
					}
					meta = laterOf(meta, start)
				}
				meta = laterOf(meta, kit.Time(r.LastUpdated))
			case r.Type != "":
				m := r.message
				keep(&m)
			case r.RewindTo != "":
				// /rewind took the conversation back to before the message it names. What
				// came after is read all the same: it was said and done, its tools ran and
				// its tokens were spent, and without it the history cannot say how the
				// session came to be where it is
			case len(r.Patch) > 0:
				// Gemini CLI brings the messages in line with the model's history: on
				// --resume and after /rewind it writes each model row again as the parts
				// the history holds (its thinking and its words, said once more), a
				// rollback or a compression drops rows, and a call's result can be set to
				// what the history kept of it. None of it is something said: the rows say
				// what was, and are read as they were first written
			default:
				unknown(line)
			}
			return true
		})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	patched := rd.last // a patch's own time, where it carried one
	for _, m := range msgs {
		rd.add(m)
	}
	rd.last = laterOf(rd.last, patched)
	if w.S.ID == "" {
		w.S.ID = stemOf(files[0])
	}
	// The metadata's lastUpdated is its value when the file was opened; the patch rows
	// move it on afterwards (measured: 29 of 33 sessions had a stale first-line time, off
	// by as much as 45 minutes). The time on the last row that carries one is the
	// session's; the metadata is only what is left when no row has any.
	w.S.UpdatedAt = rd.last
	if w.S.UpdatedAt.IsZero() {
		w.S.UpdatedAt = meta
	}
	return nil
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// reader folds a conversation's rows into messages. What it remembers across rows — and
// across the files of one conversation — is which calls have been issued and answered.
//
// Tool-driven sessions carry very little prose: when a call is issued, content is the
// empty string and the substance sits in toolCalls (id, name, args). Where the result
// lands has changed over Gemini CLI's versions. Older files answer on a later user row, as
// a functionResponse entry in its content array, and echo that same response under the
// call's result field without the real output — so a result field alone is not a result.
// Newer files write a status onto the toolCalls entry, with the output as resultDisplay (a
// string, or a file-diff object) or under result. A status is the signal: with one, the
// call is answered in place and the user row's copy, if any, is skipped; without one, the
// user row is the answer. statuses carries a status that arrived without any output to the
// user row that brings it.
type reader struct {
	w        *transcript.Writer
	answered map[string]bool   // call id → answered on the call itself
	issued   map[string]bool   // call id → its call is in the transcript already
	statuses map[string]string // call id → a status that came without its output
	last     time.Time         // the time on the latest row that had one
}

// add files one message row. Only the two sides' rows are the conversation, and the notice
// that the person stopped a turn.
func (rd *reader) add(m *message) {
	at := kit.Time(m.Timestamp)
	if !at.IsZero() {
		rd.last = at
	}
	switch m.Type {
	case "user":
		blocks, answers := rd.blocks(m)
		for _, a := range answers {
			blocks = append(blocks, a.block)
		}
		// Where the CLI sent the model more than the person typed — the files an @path
		// named, a command's prompt — the row keeps both. The ask is what was typed; the
		// rest the model read too, and is the CLI's
		var added string
		if typed := partsText(m.DisplayContent); strings.TrimSpace(typed) != "" {
			if sent := partsText(m.Content); sent != typed {
				blocks = withText(blocks, typed)
				added = strings.TrimSpace(strings.TrimPrefix(sent, typed))
			}
		}
		if len(blocks) > 0 {
			rd.w.Add(transcript.Message{ID: m.ID, Role: transcript.User, At: at, Blocks: blocks})
		}
		if added != "" {
			rd.w.Add(transcript.Message{ID: m.ID, Role: transcript.User, At: at, Injected: true,
				Blocks: []transcript.Block{transcript.TextOf(added)}})
		}
	case "gemini":
		blocks, answers := rd.blocks(m)
		// A row of only tokens is still a model call, and has to be counted. A row with
		// neither tokens nor anything said is not one: it is the CLI writing calls again —
		// a call the person cancelled is, on a row of its own — and brings at most their
		// outcome
		if usage := usageOf(m.Tokens); usage != nil || says(blocks) {
			rd.w.Add(transcript.Message{ID: m.ID, Role: transcript.Assistant, At: at, Model: m.Model,
				Usage: usage, Blocks: blocks})
		}
		for _, a := range answers {
			// A call answered in place says when it finished: the row it is on was written
			// when the model asked for it
			done := a.at
			if done.IsZero() {
				done = at
			}
			rd.w.Add(transcript.Message{ID: m.ID, Role: transcript.ToolRole, At: done, Blocks: []transcript.Block{a.block}})
		}
	case "info":
		// What the CLI showed the person beside the conversation — a notice, the bytes of
		// an image or PDF a tool read — is not something either side said. The notice that
		// the person stopped the turn is how the turn ended
		if text := strings.TrimSpace(kit.ContentText(m.Content)); cancelNotices[text] {
			rd.w.Add(transcript.Message{ID: m.ID, Role: transcript.System, At: at,
				Blocks: []transcript.Block{transcript.EventOf(model.EventInterrupted, text)}})
		}
	case "warning", "error":
		// A warning or an error the CLI showed the person: neither side said it
	default:
		rd.w.Unread("row:" + m.Type)
	}
}

// cancelNotices are the info rows Gemini CLI writes when the person stops a turn: Esc or
// Ctrl+C while the model or a tool is at work.
var cancelNotices = map[string]bool{"Request cancelled.": true, "User cancelled the request.": true}

// says reports whether a model row's blocks hold anything the model produced: words,
// thinking, a call.
func says(blocks []transcript.Block) bool {
	for _, b := range blocks {
		switch b.Type {
		case transcript.Text, transcript.Thinking:
			if b.Text != "" {
				return true
			}
		case transcript.Call:
			return true
		}
	}
	return false
}

// partsText is the text of a content value as Gemini CLI flattens it: the text parts
// joined as they stand, without the thinking.
func partsText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		return partsText([]any{t})
	case []any:
		var b strings.Builder
		for _, item := range t {
			if s, ok := item.(string); ok {
				b.WriteString(s)
				continue
			}
			if part := kit.Map(item); part["thought"] != true {
				b.WriteString(kit.Str(part["text"]))
			}
		}
		return b.String()
	}
	return ""
}

// withText puts text in place of a row's text blocks, where the first of them stood.
func withText(blocks []transcript.Block, text string) []transcript.Block {
	out := make([]transcript.Block, 0, len(blocks)+1)
	placed := false
	for _, b := range blocks {
		if b.Type != transcript.Text {
			out = append(out, b)
			continue
		}
		if !placed {
			out = append(out, transcript.TextOf(text))
			placed = true
		}
	}
	if !placed {
		out = append([]transcript.Block{transcript.TextOf(text)}, out...)
	}
	return out
}

// answer is the outcome a toolCalls entry carries, and when the call finished.
type answer struct {
	at    time.Time
	block transcript.Block
}

// blocks lays a row out in the order it happened: the thinking that came first, the
// words — with any call only the model's history holds, where it stands among them — then
// each call. The answers the row's calls carry come apart, each with its own time.
func (rd *reader) blocks(m *message) ([]transcript.Block, []answer) {
	var out []transcript.Block
	var answers []answer
	thoughts := thoughtText(m.Thoughts)
	if thoughts != "" {
		out = append(out, transcript.ThinkingOf(thoughts))
	}
	var listed *entries // the row's toolCalls, once a call part asks after them
	var parts []any
	switch content := m.Content.(type) {
	case string:
		if content != "" {
			out = append(out, transcript.TextOf(content))
		}
	case []any:
		// An array is a list of Gemini API parts: what the person sent, or a row Gemini CLI
		// rewrote from the model's own history after compressing or trimming it
		parts = content
	case map[string]any:
		// One part on its own, which Gemini CLI reads as a list of one
		parts = []any{content}
	}
	for _, item := range parts {
		entry := kit.Map(item)
		if text, ok := item.(string); ok {
			// A string among the parts is a text part, as Gemini CLI reads it
			entry = map[string]any{"text": text}
		}
		switch kind := partKind(entry); kind {
		case "text":
			text := kit.Str(entry["text"])
			switch {
			case text == "":
			case entry["thought"] != true:
				out = append(out, transcript.TextOf(text))
			case thoughts == "":
				// The model's thinking as the API returned it. A row with thoughts of
				// its own says the same there, and is read from them instead
				out = append(out, transcript.ThinkingOf(text))
			}
		case "functionResponse":
			fr, ok := entry["functionResponse"].(map[string]any)
			if !ok {
				continue
			}
			id := kit.Str(fr["id"])
			if id != "" && rd.answered[id] {
				continue // already answered on the call itself
			}
			out = append(out, result(id, kit.Str(fr["name"]), kit.Map(fr["response"]), "", rd.statuses[id]))
		case "functionCall":
			// A call as the model's history holds it, in a row rewritten from that
			// history. Where the row's toolCalls has it, that entry is the same call
			// with its outcome, and is read instead, and a call an earlier row issued
			// under the same id is not issued twice. A row Gemini CLI made from the
			// history alone has no toolCalls: the part is the only record of the call
			fc, ok := entry["functionCall"].(map[string]any)
			if !ok {
				continue
			}
			id, name := kit.Str(fc["id"]), kit.Str(fc["name"])
			if listed == nil {
				listed = entriesOf(m.ToolCalls)
			}
			if listed.has(id, name) || id != "" && rd.issued[id] {
				continue
			}
			args, ok := fc["args"]
			if !ok {
				args = map[string]any{}
			}
			out = append(out, transcript.CallOf(id, name, args))
			if id != "" {
				rd.issued[id] = true
			}
		case "inlineData", "fileData":
			// An image, a PDF or another file sent with the message, by value or by
			// reference: there is no text in it to read
		case "":
			// Nothing but what qualifies a part — a thought signature on its own — or
			// nothing that is a part at all: an empty object, a null, a number
		default:
			rd.w.Unread("part:" + kind)
		}
	}
	for _, tc := range kit.List(m.ToolCalls) {
		call := kit.Map(tc)
		id, name := kit.Str(call["id"]), kit.Str(call["name"])
		if id != "" && rd.answered[id] {
			// Written again once its outcome was read: a call the person cancelled is
			// written on the row that asked for it and once more on a row of its own
			continue
		}
		if id == "" || !rd.issued[id] {
			args, ok := call["args"]
			if !ok {
				args = map[string]any{}
			}
			out = append(out, transcript.CallOf(id, name, args))
			if id != "" {
				rd.issued[id] = true
			}
		}

		// The newer shape answers the call in place
		status := kit.Str(call["status"])
		switch status {
		case "":
			continue // the older shape: the user row that follows carries the answer
		case "executing", "pending", "scheduled", "validating", "awaiting_approval":
			continue // still running when the file was written: no result yet
		}
		display := ""
		switch d := call["resultDisplay"].(type) {
		case string:
			display = d
		case map[string]any:
			display = kit.Str(d["fileDiff"])
		}
		var response map[string]any
		for _, r := range kit.List(call["result"]) {
			if rm, ok := r.(map[string]any); ok {
				response = kit.Map(kit.Map(rm["functionResponse"])["response"])
				break
			}
		}
		if display == "" && response == nil {
			// A verdict without output: keep it for the user row that brings the output
			if id != "" {
				rd.statuses[id] = status
			}
			continue
		}
		answers = append(answers, answer{at: kit.Time(call["timestamp"]), block: result(id, name, response, display, status)})
		if id != "" {
			rd.answered[id] = true
		}
	}
	return out, answers
}

// entries is what a row's toolCalls say of the calls in it: the ids of the entries that
// have one, the names of those that have none, and every entry's name. It is gathered once
// a row and each call part is looked up in it: a part compared with every entry in turn
// makes a read grow with the square of the row (measured: twenty seconds for a row of a
// megabyte made of little else).
type entries struct {
	ids, idless, names map[string]bool
}

func entriesOf(toolCalls any) *entries {
	e := &entries{ids: map[string]bool{}, idless: map[string]bool{}, names: map[string]bool{}}
	for _, tc := range kit.List(toolCalls) {
		call := kit.Map(tc)
		id, name := kit.Str(call["id"]), kit.Str(call["name"])
		if id != "" {
			e.ids[id] = true
		} else {
			e.idless[name] = true
		}
		e.names[name] = true
	}
	return e
}

// has says whether a call part is one of the entries: the entry with its id, or, where the
// part or the entry has none, one of its name.
func (e *entries) has(id, name string) bool {
	if id == "" {
		return e.names[name]
	}
	return e.ids[id] || e.idless[name]
}

// partKind is what a content part is: the one field of a Gemini API part that holds its
// data. The others say something about that data — that it is thinking, the signature
// that goes with it, which frames of a video are meant, at what resolution — and are no
// kind of their own. A part with no data field is "".
func partKind(part map[string]any) string {
	for _, kind := range []string{"text", "functionResponse", "functionCall", "inlineData", "fileData"} {
		if _, ok := part[kind]; ok {
			return kind
		}
	}
	var other []string
	for key := range part {
		switch key {
		case "thought", "thoughtSignature", "videoMetadata", "mediaResolution":
		default:
			other = append(other, key)
		}
	}
	if len(other) == 0 {
		return ""
	}
	return slices.Min(other)
}

// result builds a call's result from whichever of the two shapes supplied it: the
// response object's output or error, or the display text. An error field is the verdict
// when the entry carries no status of its own; a response that says nothing against it is
// a success.
//
// A command's exit code is not on the entry: Gemini CLI marks a command that ran a success
// whatever it exited with (measured: ls of a missing path, exit 2, "success"), and tells
// the model the code in the response. A command that exited non-zero failed.
func result(id, name string, response map[string]any, display, status string) transcript.Block {
	output := kit.ContentText(response["output"])
	errText := kit.ContentText(response["error"])
	content := kit.First(display, output, errText)
	var outcome transcript.Outcome
	switch status {
	case "success", "completed":
		outcome.Status = model.StatusOK
	case "error":
		outcome.Status = model.StatusError
	case "cancelled", "canceled":
		outcome.Status = model.StatusInterrupted
	default:
		if errText != "" {
			outcome.Status = model.StatusError
		} else if response != nil {
			outcome.Status = model.StatusOK
		}
	}
	if name == shellTool {
		code, exited, signalled := shellEnd(output, display)
		if exited {
			outcome.Exit = transcript.ExitOf(code)
		}
		switch {
		case outcome.Status == model.StatusInterrupted:
		case signalled:
			outcome.Status = model.StatusInterrupted // ended by a signal before it finished
		case exited && code != 0:
			outcome.Status = model.StatusError
		}
	}
	return transcript.ResultOf(id, name, content, outcome)
}

// shellTool is Gemini CLI's tool for running a command.
const shellTool = "run_shell_command"

// shellEnd is how a command ended, from what the shell tool told the model of it:
//
//	Output: <what it printed>
//	Exit Code: <n>                       0.68: only when not 0
//	Signal: <n>                          when a signal ended it
//	Background PIDs: <pids>
//	Process Group PGID: <pid>
//
// wrapped in <untrusted_context>; and before 0.68 every line, a "(none)" where there was
// nothing to say, after a Command: and a Directory: line. The lines after the output are
// read from the end. The code is known only where the tool wrote one: a 0 goes unsaid in
// 0.68, and so does the code of a command cancelled, sent to the background or summarised.
// A signal that ended it is a command that did not finish.
//
// What the command printed may end in lines that read like the tool's ("Exit Code: 0").
// display, what the user was shown, is the output alone (measured: every command of 0.68),
// and where the text opens with it, only what follows is the tool's. Where it does not,
// the tool's lines are read from the end in the order it writes them, each once.
func shellEnd(output, display string) (code int, exited, signalled bool) {
	text := strings.TrimSpace(output)
	if rest, ok := strings.CutPrefix(text, "<untrusted_context"); ok {
		if _, body, found := strings.Cut(rest, ">"); found {
			text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), "</untrusted_context>"))
		}
	}
	if !strings.HasPrefix(text, "Output:") && !strings.HasPrefix(text, "Command:") {
		return 0, false, false
	}
	lines, first := strings.Split(text, "\n"), 1 // the first line is the command's, or its output's
	if display != "" {
		if rest, ok := strings.CutPrefix(text, "Output: "+display); ok && (rest == "" || rest[0] == '\n') {
			lines, first = strings.Split(strings.TrimPrefix(rest, "\n"), "\n"), 0
		}
	}
	at := -1
	for i := len(lines) - 1; i >= first; i-- {
		key, value, _ := strings.Cut(strings.TrimSpace(lines[i]), ":")
		rank, ok := trailer[key]
		if !ok || rank <= at {
			break
		}
		at = rank
		value = strings.TrimSpace(value)
		switch key {
		case "Signal":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				signalled = true
			}
		case "Exit Code":
			if n, err := strconv.Atoi(value); err == nil {
				code, exited = n, true
			}
		}
	}
	return code, exited, signalled
}

// trailer is the lines the shell tool writes after a command's output, by how far from
// the end each comes.
var trailer = map[string]int{"Process Group PGID": 0, "Background PIDs": 1, "Signal": 2, "Exit Code": 3, "Error": 4}

// thoughtText flattens thoughts into text. Gemini CLI has two generations of the format: a
// plain string, or a [{subject, description, timestamp}] array, of which the description
// is taken — the subject only where there is none.
//
// The array holds the thinking as it streamed in, one entry per piece, each trimmed. A
// piece that opens with a bold subject starts a thought; one without continues the thought
// before it. Whole thoughts come one piece each, a line apart. Some models, behind some
// gateways, stream their thinking a token at a time (measured: 268 entries for 268 thought
// tokens), and those pieces are one text, a space apart — the space each piece lost to the
// trimming — rather than a column of words.
func thoughtText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			m := kit.Map(item)
			subject := kit.Str(m["subject"])
			text := kit.First(kit.Str(m["description"]), subject)
			if text == "" {
				continue
			}
			if b.Len() > 0 {
				if subject != "" {
					b.WriteByte('\n')
				} else {
					b.WriteByte(' ')
				}
			}
			b.WriteString(text)
		}
		return b.String()
	}
	return ""
}

// usageOf converts a gemini row's tokens to the canonical shape. The counters are the API's
// usage metadata under short names, and they overlap in two ways the canonical shape does
// not allow:
//
//   - input (promptTokenCount) includes cached (cachedContentTokenCount): the prompt is what
//     was sent, and part of it was served from cache. Input is what is left of it.
//   - output (candidatesTokenCount) leaves out thoughts (thoughtsTokenCount), which are
//     counted apart — so on a model that thinks, output alone understates what it
//     produced. Output here is the two together, and Reasoning is the thinking part.
//
// tool (toolUsePromptTokenCount) is what a tool's result — a search, say — added to the
// prompt. It sits in neither input nor cached, and is part of the total the row records:
// 15597 input + 133 output + 396 thoughts + 0 tool = 16126 total, as measured. It is
// prompt, so it is added to Input.
//
// nil when the row has none of these counters.
func usageOf(tokens any) *model.Usage {
	counters := map[string]int64{}
	for key, v := range kit.Map(tokens) {
		if n, ok := kit.Num(v); ok {
			// A figure below zero or past any model's reach is no count, and counts 0
			// (model.TokensOf): added to the others, it would take the totals with it
			counters[strings.ToLower(strings.ReplaceAll(key, "_", ""))] = model.TokensOf(n)
		}
	}
	found := false
	get := func(names ...string) int64 {
		for _, name := range names {
			if v, ok := counters[name]; ok {
				found = true
				return v
			}
		}
		return 0
	}
	input := get("input", "inputtokens", "prompttokens")
	cached := get("cached", "cachedtokens", "cacheread")
	output := get("output", "outputtokens", "completiontokens")
	thoughts := get("thoughts", "reasoning", "reasoningtokens")
	tool := get("tool")
	if !found {
		return nil
	}
	return &model.Usage{
		Input:     max(input-cached, 0) + tool,
		Output:    output + thoughts,
		CacheRead: cached,
		Reasoning: thoughts,
	}
}
