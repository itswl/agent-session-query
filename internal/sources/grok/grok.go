// Package grok reads Grok CLI's sessions.
//
//	<root>/<url-encoded cwd>/<session>/summary.json        the index entry: id, cwd, titles, times
//	<root>/<url-encoded cwd>/<session>/updates.jsonl       the transcript, as a stream of updates
//	<root>/<url-encoded cwd>/<session>/chat_history.jsonl  whole messages, written as a turn runs
//	<root>/<url-encoded cwd>/<session>/usage.json          the session's token counts so far
//	<root>/<group>/.cwd                                    the cwd, when it is too long to be a name
//	<root>/../archived_sessions/<group>/<session>/…        the sessions the user archived
//
// A session is a directory, not a file, and it is there from the moment Grok writes its
// summary.json — before anything is said. The transcript is updates.jsonl: one JSON-RPC
// notification per line, {timestamp, method, params}, the timestamp in epoch seconds and
// the update under params.update. Two methods share the file: session/update is the Agent
// Client Protocol stream, and _x.ai/session/update is Grok's own — hook runs, and the
// turn_completed row that says what a turn cost.
//
// When the stream is written has changed, twice. An earlier Grok was measured writing it
// while a turn ran: sampled every 8 seconds through a multi-tool turn, its modification
// time advanced five times, each time together with summary.json's. Grok 1.0.52 flushed it
// when the session ended, and until then kept chat_history.jsonl — one whole message per
// line — and usage.json. Grok 1.0.77 writes it as the turn runs again: a session read
// mid-turn had its prompt, reasoning and open call in the stream, and no usage.json until
// the turn completed. A session with no stream yet is read from the other two, and says
// it is running. One with a stream does not: a turn Grok was killed in (SIGKILL) reads the
// same as one it is still at work on, so the files do not say which. One it was stopped in
// with Ctrl+C does differ — a stop hook's row ends the stream, and resume_status.json
// lists what was left — but is not read as running either.
package grok

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Spec describes Grok CLI.
var Spec = sources.Spec{
	Name:  "grok",
	Title: "Grok CLI",
	Locate: func(env sources.Env) string {
		// GROK_HOME moves the whole ~/.grok — sessions, config and credentials together
		if d := env.Getenv("GROK_HOME"); d != "" {
			return filepath.Join(d, "sessions")
		}
		return filepath.Join(env.Home, ".grok", "sessions")
	},
	Open:    func(location string) sources.Source { return &Source{root: location} },
	Movable: true,
	Resume:  "grok -r",
	Reads:   2,
	Store:   "~/.grok/sessions/<url-encoded-cwd>/<session-id>/updates.jsonl",
}

// The files of a session directory that a read depends on.
const (
	summaryFile = "summary.json"
	updatesFile = "updates.jsonl"
	historyFile = "chat_history.jsonl"
	usageFile   = "usage.json"
)

// Source is a Grok sessions directory, and the archive beside it.
type Source struct{ root string }

func (s *Source) Location() string { return s.root }
func (s *Source) Exists() bool     { return kit.Exists(s.root) }

// archivedRoot is where Grok moves a session the user archives: archived_sessions beside
// sessions, in the same layout. A relocated root that is not named sessions has no such
// sibling to look beside.
func (s *Source) archivedRoot() string {
	root := filepath.Clean(s.root)
	if filepath.Base(root) != "sessions" {
		return ""
	}
	return filepath.Join(filepath.Dir(root), "archived_sessions")
}

// Scan lists every session directory, the archived ones included: they are still history.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	var refs []sources.Ref
	var unread error
	for _, root := range []string{s.root, s.archivedRoot()} {
		if root == "" {
			continue
		}
		groups, err := subdirs(root)
		if err != nil {
			return nil, err
		}
		for _, group := range groups {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			dirs, err := subdirs(group)
			if err != nil {
				// One directory that cannot be read is not the store that cannot: the rest
				// is listed, and the error says the list is short
				unread = errors.Join(unread, err)
				continue
			}
			for _, dir := range dirs {
				if ref, ok := refOf(dir, group); ok {
					refs = append(refs, ref)
				}
			}
		}
	}
	return refs, unread
}

// subdirs lists the directories inside dir. One that is not there holds nothing; one that
// cannot be read is an error, so that a store nobody could look into is not taken for an
// empty one and its sessions for deleted.
func subdirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			out = append(out, path)
		case e.Type()&fs.ModeSymlink != 0:
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				out = append(out, path)
			}
		}
	}
	return out, nil
}

// refOf names a session directory and fingerprints it, or reports that the directory is
// not a session: one without a summary.json is something Grok has not made a session of.
//
// The stamp covers every file a read opens. summary.json alone would nearly do — Grok
// rewrites it on every turn, together with the transcript — but only nearly: a crash
// between the two writes, or anything else appending, would leave the session one turn
// behind until the summary next moved.
func refOf(dir, group string) (sources.Ref, bool) {
	info, err := os.Stat(filepath.Join(dir, summaryFile))
	if err != nil {
		return sources.Ref{}, false
	}
	// The group's .cwd is read too, when the summary names no directory
	rest, mod := kit.Stamp(filepath.Join(dir, updatesFile), filepath.Join(dir, historyFile), filepath.Join(dir, usageFile), filepath.Join(group, ".cwd"))
	if info.ModTime().After(mod) {
		mod = info.ModTime()
	}
	return sources.Ref{Key: dir, Stamp: kit.StampInfo(info) + ";" + rest, ModTime: mod}, true
}

// Read parses one session directory.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	dir := ref.Key
	raw, err := os.ReadFile(filepath.Join(dir, summaryFile))
	if err != nil {
		return err
	}
	// A summary caught half-written still leaves the session named by where it is
	var sum map[string]any
	_ = json.Unmarshal(raw, &sum)
	info := kit.Map(sum["info"])

	w.S.Path = dir
	w.S.ID = kit.First(kit.Str(info["id"]), filepath.Base(dir))
	w.S.Cwd = cwdOf(kit.Str(info["cwd"]), filepath.Dir(dir))
	w.S.Model = kit.Str(sum["current_model_id"])
	w.S.Title = storedTitle(kit.Str(sum["generated_title"]), kit.Str(sum["session_summary"]))
	w.S.StartedAt = kit.Time(sum["created_at"])
	// last_active_at follows the conversation and updated_at the file. They agree in the
	// normal case, and last_active_at is the one that stays true when something rewrites
	// the summary without a turn having happened.
	if w.S.UpdatedAt = kit.Time(sum["last_active_at"]); w.S.UpdatedAt.IsZero() {
		w.S.UpdatedAt = kit.Time(sum["updated_at"])
	}
	if archived := s.archivedRoot(); archived != "" && filepath.Dir(filepath.Dir(dir)) == archived {
		w.S.Archived = true
	}

	f := &folder{w: w, model: w.S.Model}
	updates, history := filepath.Join(dir, updatesFile), filepath.Join(dir, historyFile)
	switch {
	case kit.Exists(updates):
		err = readUpdates(ctx, updates, f)
	case kit.Exists(history):
		// No stream yet: the session has not ended. The whole-message log is what it has
		// said so far, and usage.json — which Grok keeps current per turn — what it has
		// cost, for the log carries no counters of its own.
		w.S.Running = true
		if u := usageOf(kit.Map(kit.Map(kit.ReadJSON(filepath.Join(dir, usageFile)))["session"])); u != nil {
			w.S.Recorded = &model.Recorded{Usage: u}
		}
		err = readHistory(ctx, history, f)
	}
	// With neither, the session was created and never prompted: it has a summary and
	// nothing to say
	f.flush()
	if err != nil {
		return err
	}
	return ctx.Err()
}

// cwdOf is the session's working directory.
//
// summary.json states it outright. The fallbacks exist because the group directory is
// named after that same path, URL-encoded, and Grok swaps the encoded name for a slug plus
// a hash once it passes 255 bytes, recording the real path in a .cwd file beside the
// sessions.
//
// Decoding uses PathUnescape, not QueryUnescape: the latter also turns "+" into a space,
// which would corrupt every working directory with a plus in its name.
func cwdOf(recorded, groupDir string) string {
	if recorded != "" {
		return recorded
	}
	if raw, err := os.ReadFile(filepath.Join(groupDir, ".cwd")); err == nil {
		if cwd := strings.TrimSpace(string(raw)); cwd != "" {
			return cwd
		}
	}
	if decoded, err := url.PathUnescape(filepath.Base(groupDir)); err == nil {
		return decoded
	}
	return ""
}

// storedTitle is the first of the titles Grok keeps that reads as one: the title it
// generated, then its summary of the session. Either can be the opening prompt's words
// cut short, and a prompt a harness extended can be cut inside the harness's block or be
// nothing but plumbing — a title that comes out empty, or as something no person asked,
// is passed over, and with both passed over the session is titled by its first ask.
func storedTitle(candidates ...string) string {
	for _, c := range candidates {
		if _, ok := transcript.AskOf(transcript.CleanTitle(c)); ok {
			return c
		}
	}
	return ""
}

// folder builds messages out of pieces.
//
// Grok streams a turn as many small updates: text arrives in chunks, and a tool call
// opens on one line and completes on a later one. One message per update would turn a
// single reply into dozens of one-word rows, so consecutive text chunks of one kind
// become one block, and consecutive pieces from one speaker one response. A message is
// handed over when the speaker changes, when the turn completes, or when the transcript
// ends.
//
// The speakers are three: the user, the agent, and the tool whose result a finished call
// reports. The tool being one is what keeps a turn's times — a result is filed when it
// arrived, not when the agent began — and it makes a response of each stretch the agent
// spoke between results, which is as near as the stream comes to one per model call: in
// the turns measured, Grok's own modelCalls is the number of responses this makes of
// them.
//
// A block that begins later than the message it would join is a message of its own, in
// the same response: each piece keeps its own time. A call is made when its line says,
// not when the reasoning before it began — which could be most of a minute earlier — and
// of two calls made together and finishing apart, the slower ends when it ended.
type folder struct {
	w         *transcript.Writer
	model     string             // the model that answers, as far as the transcript has said
	msg       transcript.Message // the message being built; it exists once it has a block
	tail      strings.Builder    // the text of its last block, while chunks still add to it
	responses int                // the agent's responses so far, which name each one
}

// speak makes role the speaker: whoever spoke before has finished.
func (f *folder) speak(role transcript.Role, at time.Time) {
	if len(f.msg.Blocks) > 0 && f.msg.Role == role {
		return
	}
	f.flush()
	f.msg = transcript.Message{Role: role, At: at}
	if role == transcript.Assistant {
		f.responses++
		f.msg.Model, f.msg.Response = f.model, "response-"+strconv.Itoa(f.responses)
	}
}

// add appends a whole block to role's message.
func (f *folder) add(role transcript.Role, at time.Time, b transcript.Block) {
	f.speak(role, at)
	f.open(at, b)
}

// chunk appends streamed text, joining it to a block of the same kind it follows: a
// reader wants the sentence, not the pieces it was sent in.
func (f *folder) chunk(role transcript.Role, at time.Time, kind transcript.BlockType, text string) {
	if text == "" {
		return
	}
	f.speak(role, at)
	if n := len(f.msg.Blocks); n == 0 || f.tail.Len() == 0 || f.msg.Blocks[n-1].Type != kind {
		f.open(at, transcript.Block{Type: kind})
	}
	f.tail.WriteString(text)
}

// open begins a block in the speaker's message, or in a message of its own beside it
// when it begins later than the message did.
func (f *folder) open(at time.Time, b transcript.Block) {
	f.seal()
	if len(f.msg.Blocks) > 0 && !at.Equal(f.msg.At) {
		next := transcript.Message{Role: f.msg.Role, At: at, Model: f.msg.Model, Response: f.msg.Response}
		f.w.Add(f.msg)
		f.msg = next
	}
	f.msg.Blocks = append(f.msg.Blocks, b)
}

// seal closes the block chunks were adding to. Its text is gathered once, here: a long
// reply is thousands of chunks, and joining them one at a time copies it thousands of
// times.
func (f *folder) seal() {
	if f.tail.Len() > 0 {
		f.msg.Blocks[len(f.msg.Blocks)-1].Text = f.tail.String()
		f.tail.Reset()
	}
}

// flush hands over the message being built, if there is one.
func (f *folder) flush() {
	f.seal()
	if len(f.msg.Blocks) > 0 || f.msg.Usage != nil {
		f.w.Add(f.msg)
	}
	f.msg = transcript.Message{}
}

// complete ends a turn. Grok counts tokens per turn, not per response, and says so after
// the answer's last chunk: the counters go on the turn's last message from the agent,
// which is the one still being built. A turn that closed on anything else — a tool's
// result, or nothing at all — still cost what it cost, and gets a message with no words to
// carry it, so that no turn's tokens are counted into the turn after it.
func (f *folder) complete(at time.Time, usage *model.Usage, stop string) {
	if f.msg.Role != transcript.Assistant || len(f.msg.Blocks) == 0 {
		f.flush()
		if usage == nil {
			return
		}
		f.msg = transcript.Message{Role: transcript.Assistant, At: at, Model: f.model}
	}
	f.msg.Usage, f.msg.Stop = usage, stop
	f.flush()
}

// line is one line of updates.jsonl.
type line struct {
	Timestamp any    `json:"timestamp"` // epoch seconds
	Method    string `json:"method"`
	Params    struct {
		Update update `json:"update"`
		Meta   struct {
			At any `json:"agentTimestampMs"`
		} `json:"_meta"`
	} `json:"params"`
}

// at is when the line's update happened.
//
// The timestamp is when the line was written, in whole seconds, and Grok 1.0.77 writes
// some lines well after what they record: an edit that completed 56 ms after it was
// called was stamped four seconds on, and a call re-titled as it began was stamped eight
// seconds on, when the session was interrupted and its stream flushed. The milliseconds
// Grok's agent stamped the update with are when it happened, and agree with
// events.jsonl's to the millisecond; a Grok that writes none is read by the timestamp.
func (l *line) at() time.Time {
	if at := kit.Time(l.Params.Meta.At); !at.IsZero() {
		return at
	}
	return kit.Time(l.Timestamp)
}

// update is a session update, reduced to the parts a transcript needs. rawOutput, a
// tool's own struct whose shape differs per tool, is kept whole for the few fields a
// finished call is read by.
type update struct {
	Kind       string          `json:"sessionUpdate"`
	Content    any             `json:"content"` // a chunk's block, or a finished call's list of them
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
	Meta       struct {
		ModelID string `json:"modelId"`
		Tool    struct {
			Name string `json:"name"`
		} `json:"x.ai/tool"`
	} `json:"_meta"`

	// turn_completed
	StopReason string         `json:"stop_reason"`
	Usage      map[string]any `json:"usage"`
}

// decode reads a line into v. A field whose type changed between versions costs that
// field and not the line: the decoder skips it and fills in the rest.
func decode(raw []byte, v any) bool {
	err := json.Unmarshal(raw, v)
	var mismatch *json.UnmarshalTypeError
	return err == nil || errors.As(err, &mismatch)
}

// readUpdates folds the update stream into messages.
func readUpdates(ctx context.Context, path string, f *folder) error {
	n := 0
	return kit.Lines(path, func(raw []byte) bool {
		if n++; n&1023 == 0 && ctx.Err() != nil {
			return false
		}
		var l line
		if !decode(raw, &l) {
			return true
		}
		u, at := &l.Params.Update, l.at()
		switch {
		case u.Kind == "turn_completed":
			f.complete(at, usageOf(u.Usage), u.StopReason)
		case l.Method == "session/update":
			f.update(u, at)
		case l.Method == "_x.ai/session/update":
			// Grok's own events, beside the turn_completed row read above
			switch {
			case u.Kind == "hook_execution":
				// A hook Grok ran at a point of the session, and what it returned: Grok's
				// own plumbing, not conversation
			case u.Kind == "background_tasks":
				// The commands and agents still running in the background, listed as a
				// resumed session's next prompt begins: a status, not something said
			case aside(u.Kind):
				// One of the protocol's updates that say nothing of the conversation, which
				// is no more conversation for being written under Grok's own method
			default:
				f.unread("event", u.Kind)
			}
		default:
			// A method beside the two the stream has been written with
			f.unread("method", l.Method)
		}
		return true
	})
}

// update reads one update of the Agent Client Protocol stream.
func (f *folder) update(u *update, at time.Time) {
	switch u.Kind {
	case "user_message_chunk":
		// A prompt names the model it was sent to, which is the one that answers it;
		// the summary only knows the model the session ended on
		if u.Meta.ModelID != "" {
			f.model = u.Meta.ModelID
		}
		f.chunk(transcript.User, at, transcript.Text, kit.ContentText(u.Content))
	case "agent_message_chunk":
		f.chunk(transcript.Assistant, at, transcript.Text, kit.ContentText(u.Content))
	case "agent_thought_chunk":
		f.chunk(transcript.Assistant, at, transcript.Thinking, kit.ContentText(u.Content))
	case "tool_call":
		f.add(transcript.Assistant, at, transcript.CallOf(u.ToolCallID, toolName(u), u.RawInput))
	case "tool_call_update":
		// Only a finished call carries output. How it went is which of the two ways it
		// finished, and for a command its exit code too: Grok marks a command that ran
		// and exited non-zero completed, as it does one that succeeded — the call worked,
		// the command did not — and its own events.jsonl counts that call's outcome an
		// error.
		switch u.Status {
		case "completed", "failed":
			exit := exitCode(u.RawOutput)
			failed := u.Status == "failed" || (exit != nil && *exit != 0)
			outcome := transcript.Outcome{Status: transcript.StatusOf(failed), Exit: exit}
			f.add(transcript.ToolRole, at, transcript.ResultOf(u.ToolCallID, "", output(u), outcome))
		case "", "pending", "in_progress":
			// An update with no status is the call being re-titled while it runs, and one
			// that has not finished has nothing to show yet
		default:
			f.unread("status", u.Status)
		}
	default:
		if !aside(u.Kind) {
			f.unread("event", u.Kind)
		}
	}
}

// aside reports whether kind is one of the Agent Client Protocol's updates that say
// nothing of the conversation, each of which is passed over on purpose.
//
// They are passed over under either method. The protocol sends them as session/update,
// and Grok 1.0.77 was seen to write a plan and a mode change there; this adapter was
// first written taking Grok to put them among its own events, and a kind known under one
// method is no news under the other.
func aside(kind string) bool {
	switch kind {
	case "plan":
		// The agent's list of steps, restated whole each time one changes: its working
		// notes, which the calls and replies around it carry out
	case "current_mode_update", "config_option_update":
		// The session switched mode (plan, agent…) or changed a setting: a control the
		// person moved, not something said
	case "available_commands_update":
		// The slash commands on offer, announced whenever they change
	case "session_info_update":
		// The session's title and times, which summary.json keeps
	case "usage_update":
		// How much of the context window is in use, of how much, and perhaps what the
		// session has cost so far. The tokens a turn spent are turn_completed's to say.
		// The window's fill and the running cost would be worth reading once a Grok
		// session is seen to write them.
	default:
		return false
	}
	return true
}

// unread notes a record of a kind this adapter has never heard of, as what:kind. A line
// that names no kind at all is not a record of a new kind, and is not counted.
func (f *folder) unread(what, kind string) {
	if kind != "" {
		f.w.Unread(what + ":" + kind)
	}
}

// toolName is the tool's own name. Grok keeps it in its _meta extension; the title starts
// out as that same name but is rewritten into a sentence once the call is under way, so
// _meta is the stable one.
func toolName(u *update) string {
	return kit.First(u.Meta.Tool.Name, u.Title)
}

// resultText is the readable output of a finished call. The Agent Client Protocol content
// array is the presentable form, each entry wrapping a block as
// {"type":"content","content":{…}}.
func resultText(content any) string {
	var texts []string
	for _, item := range kit.List(content) {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		inner := entry["content"]
		if inner == nil {
			inner = entry
		}
		if text := kit.ContentText(inner); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// output is a finished call's output: what the tool gave the model, as near as the
// stream keeps it. The content array is that for most tools; a search's content is a
// count, and a file written or edited, a listing or a todo list has none to show.
func output(u *update) string {
	if s := matches(u.RawOutput); s != "" {
		return s
	}
	if s := resultText(u.Content); strings.TrimSpace(s) != "" {
		return s
	}
	return toldModel(u.RawOutput)
}

// matches is what a search gave the model. Grok's grep shows a count for content —
// "found 1 matches" — and keeps the matching lines, as the model read them, in rawOutput's
// stdout: the bytes, written as a list of numbers.
func matches(raw json.RawMessage) string {
	var v struct {
		Type   string `json:"type"`
		Stdout []int  `json:"stdout"`
	}
	if len(raw) == 0 || !decode(raw, &v) || v.Type != "GrepSearch" {
		return ""
	}
	b := make([]byte, len(v.Stdout))
	for i, n := range v.Stdout {
		if n < 0 || n > 255 {
			return ""
		}
		b[i] = byte(n)
	}
	return strings.ToValidUTF8(string(b), "\uFFFD")
}

// exitCode is the exit code a finished command's rawOutput carries, if it carries one.
//
// Only a command's: Grok's grep keeps the exit code of the search it ran too, and a search
// exits 1 when it matched nothing, which is an answer and not a failure.
func exitCode(raw json.RawMessage) *int {
	var v struct {
		Type     string `json:"type"`
		ExitCode *int   `json:"exit_code"`
	}
	if len(raw) == 0 || !decode(raw, &v) || v.Type != "Bash" {
		return nil
	}
	return v.ExitCode
}

// toldModel is the words a finished call's rawOutput says the tool gave the model, for the
// calls whose content has none to show.
//
// Grok 1.0.77 gives a file written or edited a diff for content, and a directory listed or
// a todo list updated no content at all. What the model was told — the session's
// chat_history.jsonl has the same words — sits in rawOutput, under a name that differs
// per tool, one level down beneath the name of how the call went:
// {"type":"SearchReplace","EditsApplied":{"tool_output_for_prompt":"The file … has been
// updated successfully.",…}}, {"type":"ListDir","Content":{"content":"- /w/\n"}},
// {"type":"Todo","TodosUpdated":{"summary_for_prompt":"- [pending] 1: …"}}.
func toldModel(raw json.RawMessage) string {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || !decode(raw, &fields) {
		return ""
	}
	if s := said(raw); s != "" {
		return s
	}
	// A map keeps no order: the names are sorted, so that a struct with two such objects
	// gives the same words every read
	names := make([]string, 0, len(fields))
	for name, v := range fields {
		if len(v) > 0 && v[0] == '{' {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if s := said(fields[name]); s != "" {
			return s
		}
	}
	return ""
}

// said is the words one object of a rawOutput gave the model, under the names Grok's
// tools put them.
func said(raw json.RawMessage) string {
	var v struct {
		ToolOutput string `json:"tool_output_for_prompt"`
		Summary    string `json:"summary_for_prompt"`
		Content    string `json:"content"`
	}
	if !decode(raw, &v) {
		return ""
	}
	for _, s := range []string{v.ToolOutput, v.Summary, v.Content} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// usageOf converts Grok's counters — a turn's in turn_completed, the session's in
// usage.json, under the same names — to the canonical ones: input without the cache,
// output with the reasoning.
//
// Whether Grok's inputTokens holds the cache has changed, and its totalTokens says which.
// A Grok 1.0.41 turn had 31344 total, exactly 15667 in + 53 out + 15624 read from cache:
// the input left the cache out. Grok 1.0.77 counts the whole prompt — a resumed turn's
// 11374 in, of which 11264 were read from cache, with 11405 total, in + out — so a
// reader that added the cache to it counted every cached token twice. What the counters
// add up to past the total is what the input already holds, and is taken out of it.
// Without a total, or with one that no part of the cache explains, the input is read as
// it stands. Under either, the reasoning is part of the output and added to nothing.
//
// cachedReadTokens is Grok's spelling, and cacheCreationTokens its name for writing the
// cache, which has been seen only at zero; the names read beside Grok's are the ones the
// same shape goes by elsewhere.
//
// A counter is read by the one rule for what a count is (kit.Count): one below zero or
// past model.MaxTokens is a number gone wrong, and reads as no counter at all.
func usageOf(raw map[string]any) *model.Usage {
	count := func(names ...string) int64 {
		for _, name := range names {
			if n := kit.Count(raw[name]); n > 0 {
				return n
			}
		}
		return 0
	}
	u := model.Usage{
		Input:      count("inputTokens"),
		Output:     count("outputTokens"),
		CacheRead:  count("cachedReadTokens", "cacheReadTokens"),
		CacheWrite: count("cacheCreationTokens", "cacheWriteTokens"),
		Reasoning:  count("reasoningTokens"),
	}
	if total := count("totalTokens"); total > 0 {
		cache := u.CacheRead + u.CacheWrite
		if twice := u.Input + u.Output + cache - total; twice > 0 && twice <= cache && twice <= u.Input {
			u.Input -= twice
		}
	}
	if u.IsZero() {
		return nil
	}
	return &u
}

// historyLine is one line of chat_history.jsonl: a whole message.
type historyLine struct {
	Type    string `json:"type"`    // system, user, reasoning, assistant, tool_result
	Content any    `json:"content"` // a string, or an array of blocks
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"` // a reasoning line's blocks, {type: summary_text, text}
	ToolCalls []struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"tool_calls"`
	ToolCallID  string `json:"tool_call_id"`
	ModelID     string `json:"model_id"`
	PromptIndex *int   `json:"prompt_index"` // on a prompt the person sent, and on no other user line
}

// readHistory reads the whole-message log of a session still running, into the messages
// its stream will fold into once it is flushed: a reasoning line belongs to the assistant
// line after it — together they are one response — and a tool's result is the tool's.
//
// The lines carry no times (those live in events.jsonl), so neither do the messages; the
// file's order is the conversation's. The system line, Grok's own prompt, is not
// conversation.
func readHistory(ctx context.Context, path string, f *folder) error {
	n := 0
	var none time.Time
	return kit.Lines(path, func(raw []byte) bool {
		if n++; n&1023 == 0 && ctx.Err() != nil {
			return false
		}
		var l historyLine
		if !decode(raw, &l) {
			return true
		}
		switch l.Type {
		case "user":
			f.flush() // a line is a whole message: a second prompt is a second one
			text := kit.ContentText(l.Content)
			if l.PromptIndex == nil && !strings.Contains(text, "<user_query>") {
				// Grok's own context in the user's seat: Grok 1.0.77 opens a session's log
				// with the workspace and its rules, and a reminder of the workflows on offer,
				// before the first prompt. Neither carries a prompt_index or a user_query,
				// and the stream has neither: a session reads the same from either file.
				return true
			}
			if text = userQuery(text); strings.TrimSpace(text) != "" {
				f.add(transcript.User, none, transcript.TextOf(text))
				f.flush()
			}
		case "reasoning":
			var texts []string
			for _, s := range l.Summary {
				if s.Text != "" {
					texts = append(texts, s.Text)
				}
			}
			if len(texts) > 0 {
				f.add(transcript.Assistant, none, transcript.ThinkingOf(strings.Join(texts, "\n")))
			}
		case "assistant":
			if l.ModelID != "" {
				f.model = l.ModelID
			}
			if text := kit.ContentText(l.Content); text != "" {
				f.add(transcript.Assistant, none, transcript.TextOf(text))
			}
			for _, call := range l.ToolCalls {
				f.add(transcript.Assistant, none, transcript.CallOf(call.ID, call.Name, arguments(call.Arguments)))
			}
			// The reasoning before it opened this message before the line named its model
			if f.msg.Role == transcript.Assistant {
				f.msg.Model = f.model
			}
			f.flush()
		case "tool_result":
			// The log names no status. A command's result opens on its exit code, which is
			// the outcome; any other result carries none
			text, outcome := kit.ContentText(l.Content), transcript.Outcome{}
			if exit, rest, ok := exitLine(text); ok {
				text, outcome = rest, transcript.Outcome{Status: transcript.StatusOf(exit != 0), Exit: &exit}
			}
			f.add(transcript.ToolRole, none, transcript.ResultOf(l.ToolCallID, "", text, outcome))
		case "system":
			// Grok's own prompt, the log's first line: not conversation
		default:
			f.unread("row", l.Type)
		}
		return true
	})
}

// exitLine splits a command's result as the log keeps it — "exit: 2\n" and then the
// output, which is how Grok tells the model how a command ended — into the code and the
// output alone, which is what the stream's content holds.
func exitLine(text string) (code int, rest string, ok bool) {
	first, rest, _ := strings.Cut(text, "\n")
	digits, cut := strings.CutPrefix(first, "exit: ")
	if !cut {
		return 0, "", false
	}
	code, err := strconv.Atoi(digits)
	if err != nil {
		return 0, "", false
	}
	return code, rest, true
}

// arguments are a call's arguments as the log carries them: a JSON object inside a string.
func arguments(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return raw
}

// userQuery is what the person typed. The whole-message log keeps a prompt as the model
// was sent it, inside <user_query>…</user_query>, where the stream keeps the words alone.
// Unwrapped, a session reads the same from either — and reads as asked at all: an element
// that wraps a whole message is how a CLI marks what it injected.
func userQuery(text string) string {
	// The element may not be alone: what Grok attached to the prompt rides beside it, in
	// elements of its own. The words are what the first user_query holds.
	_, rest, ok := strings.Cut(text, "<user_query>")
	if !ok {
		return text
	}
	inner, _, ok := strings.Cut(rest, "</user_query>")
	if !ok {
		return text
	}
	return strings.TrimSpace(inner)
}
