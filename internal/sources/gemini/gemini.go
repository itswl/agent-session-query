// Package gemini reads Gemini CLI's sessions.
//
//	<root>/<project>/chats/session-<time>-<id>.jsonl      one file of a conversation
//
// A file is an append log: a metadata first line (sessionId, startTime …), message rows
// (type "user" and "gemini"; the others are bookkeeping), and {"$set": …} patch rows, which
// move the metadata's lastUpdated on and may carry messages of their own.
//
// A conversation is not always one file. Gemini CLI continues a session in a new file that
// opens with the same sessionId (measured: two files a minute apart), so the conversation —
// not the file — is what a Ref names, and Read walks every file of it in order.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
	// Gemini CLI addresses sessions by position rather than identity: --resume takes
	// "latest" or an index, --list-sessions numbers them, and --delete-session takes that
	// same index. A session id cannot be turned into a command at all.
	Resume: "",
	Store:  "~/.gemini/tmp/<project>/chats/session-*.jsonl",
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
func (s *Source) walk(ctx context.Context) ([]*conversation, error, error) {
	matches, unread := kit.Glob(filepath.Join(s.root, "*", "chats", "session-*.jsonl"))
	s.mu.Lock()
	known := s.heads
	s.mu.Unlock()

	heads := make(map[string]head, len(matches))
	byID := map[string]*conversation{}
	var convs []*conversation
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

// project is the directory name a conversation's files sit under: <root>/<project>/chats.
func project(path string) string { return filepath.Base(filepath.Dir(filepath.Dir(path))) }

// row is one line of a session file. It is a message when it has a type, the metadata when
// it has a sessionId, and a patch when it has $set — a patch's messages are rows of their
// own, written inside it.
type row struct {
	message
	SessionID   string `json:"sessionId"`
	StartTime   string `json:"startTime"`
	LastUpdated string `json:"lastUpdated"`
	Set         *struct {
		LastUpdated string            `json:"lastUpdated"`
		Messages    []json.RawMessage `json:"messages"`
	} `json:"$set"`
}

// message is a user or gemini row. content is an array of {text} and {functionResponse}
// entries on a user row and a string on a gemini row; thoughts, tokens and toolCalls
// have changed shape between Gemini CLI's versions and are read loosely.
type message struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Model     string `json:"model"`
	Content   any    `json:"content"`
	Thoughts  any    `json:"thoughts"`
	Tokens    any    `json:"tokens"`
	ToolCalls any    `json:"toolCalls"`
}

// Read parses one conversation, every file of it in order.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	files, err := s.filesOf(ctx, ref.Key)
	if err != nil {
		return err
	}
	w.S.Path = files[0]
	// Gemini CLI records no working directory in a session file: what it has is the project
	// it filed the session under (a hash, or a name, by version). That stands in for the
	// directory, the one grouping the files give.
	w.S.Cwd = project(files[0])

	rd := &reader{w: w, answered: map[string]bool{}, statuses: map[string]string{}}
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
				for _, raw := range r.Set.Messages {
					var m message
					if json.Unmarshal(raw, &m) == nil && m.Type != "" {
						keep(&m)
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
// across the files of one conversation — is which calls have been answered.
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
	statuses map[string]string // call id → a status that came without its output
	last     time.Time         // the time on the latest row that had one
}

// add files one message row. Only the two sides' rows are the conversation; info, error
// and warning rows are the CLI talking to itself.
func (rd *reader) add(m *message) {
	at := kit.Time(m.Timestamp)
	if !at.IsZero() {
		rd.last = at
	}
	switch m.Type {
	case "user":
		if blocks := rd.blocks(m); len(blocks) > 0 {
			rd.w.Add(transcript.Message{ID: m.ID, Role: transcript.User, At: at, Blocks: blocks})
		}
	case "gemini":
		// Added even when it says nothing: a row of only tokens is still a model call, and
		// has to be counted
		rd.w.Add(transcript.Message{ID: m.ID, Role: transcript.Assistant, At: at, Model: m.Model,
			Usage: usageOf(m.Tokens), Blocks: rd.blocks(m)})
	}
}

// blocks lays a row out in the order it happened: the thinking that came first, the
// words, then each call with the answer it carries.
func (rd *reader) blocks(m *message) []transcript.Block {
	var out []transcript.Block
	if thoughts := thoughtText(m.Thoughts); thoughts != "" {
		out = append(out, transcript.ThinkingOf(thoughts))
	}
	switch content := m.Content.(type) {
	case string:
		if content != "" {
			out = append(out, transcript.TextOf(content))
		}
	case []any:
		for _, item := range content {
			entry := kit.Map(item)
			if text := kit.Str(entry["text"]); text != "" {
				out = append(out, transcript.TextOf(text))
			}
			fr, ok := entry["functionResponse"].(map[string]any)
			if !ok {
				continue
			}
			id := kit.Str(fr["id"])
			if id != "" && rd.answered[id] {
				continue // already answered on the call itself
			}
			out = append(out, result(id, kit.Str(fr["name"]), kit.Map(fr["response"]), "", rd.statuses[id]))
		}
	}
	for _, tc := range kit.List(m.ToolCalls) {
		call := kit.Map(tc)
		id, name := kit.Str(call["id"]), kit.Str(call["name"])
		args, ok := call["args"]
		if !ok {
			args = map[string]any{}
		}
		out = append(out, transcript.CallOf(id, name, args))

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
		out = append(out, result(id, name, response, display, status))
		if id != "" {
			rd.answered[id] = true
		}
	}
	return out
}

// result builds a call's result from whichever of the two shapes supplied it: the
// response object's output or error, or the display text. An error field is the verdict
// when the entry carries no status of its own; a response that says nothing against it is
// a success.
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
	return transcript.ResultOf(id, name, content, outcome)
}

// thoughtText flattens thoughts into text. Gemini CLI has two generations of the format: a
// plain string, or a [{subject, description, timestamp}] array (itemised thinking used
// when planning or reporting on itself), of which the description is taken — the subject
// only where there is none.
func thoughtText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			m := kit.Map(item)
			if text := kit.First(kit.Str(m["description"]), kit.Str(m["subject"])); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
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
			counters[strings.ToLower(strings.ReplaceAll(key, "_", ""))] = int64(n)
		}
	}
	found := false
	get := func(names ...string) int64 {
		for _, name := range names {
			if v, ok := counters[name]; ok {
				found = true
				return max(v, 0)
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
