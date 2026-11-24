// Package dsh reads the DeepSeek Harness's sessions.
//
//	<root>/<project>/<session>/session.v<N>.jsonl[.zstd]
//
// A session is a directory, and the directory holds one transcript per rotating
// generation: the highest N is the current one. The transcript is JSONL — a header row
// (`session` or `header`, with the id, the cwd and when it was created), then events,
// each with `time` in epoch milliseconds. The conversation is `user/message`,
// `assistant/message` and `tool/result`, and how a command exited is on the `job/settled`
// that ran it. Beside it the harness writes rows of its own work, which are read as
// events of the turn they fall in:
//
//   - `step/start` and `step/end` bound each step — a call to the model and the tools it
//     asked for — and a step that ended is a `step` event, at its end, saying how long it
//     took.
//   - `permission/preset`, `sandbox/mode` and `approval/policy` are the settings a session
//     runs with, written as a run of rows: the first run is a `settings` event naming all
//     three, and a later one is an event only where a value changed.
//   - `request/context` names the model each request is made to: a model other than the
//     last request's is a `model_change` event.
//   - `assistant/attempt` is a call to the model that came to no message, and `turn/end`
//     says how a turn ended: a call or a turn that failed is an `error` event, and a turn
//     stopped before it was done is interrupted.
//   - `turn/start` and `turn/end` bound every turn, so a turn without its end is in flight.
//
// The rest of the harness's rows say nothing a reader of the transcript was shown — the
// system prompt, the request's tools, the title model's prompt, a log's receipts, a call
// as it was run — and are passed over. Three things about the transcript are not obvious:
//
//   - A `.zstd` transcript is a chain of independently decodable frames, one appended per
//     batch of events, and the last is often half-written. The frames are decoded in order,
//     the complete ones are kept, and the truncated tail is dropped with the incomplete JSON
//     line it would have held.
//   - A row with `surfaceOp.op == "replace"` is the model-visible replacement of earlier
//     rows — a compaction — and not something a reader of the transcript was shown. It is
//     skipped.
//   - An `agent/inbox/spliced` row puts a prompt waiting in the harness's inbox into the
//     conversation, and the prompt is then written again, under the same id, as the
//     `user/message` after it. The splice is passed over, or the prompt would be read twice.
package dsh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
	"github.com/itswl/agent-session-query/internal/txt"
)

// Spec describes the DeepSeek Harness.
var Spec = sources.Spec{
	Name:  "dsh",
	Title: "DeepSeek Harness",
	Locate: func(env sources.Env) string {
		return filepath.Join(home(env), "sessions")
	},
	Open:    func(location string) sources.Source { return &Source{root: location} },
	Movable: true,
	Resume:  "", // the harness has no by-id resume: its own Web UI owns resume
	Reads:   2,
	Store:   "~/.dsh/sessions/<project>/<session>/session.v<N>.jsonl[.zstd]",
}

// home follows the harness's own resolution: DSH_HOME moves the whole ~/.dsh.
func home(env sources.Env) string {
	if d := env.Getenv("DSH_HOME"); d != "" {
		return d
	}
	return filepath.Join(env.Home, ".dsh")
}

// Source is a DeepSeek Harness sessions directory.
type Source struct{ root string }

func (s *Source) Location() string { return s.root }
func (s *Source) Exists() bool     { return kit.Exists(s.root) }

// generation reads the version out of session.v<N>.jsonl[.zstd].
func generation(name string) (int, bool) {
	name = strings.TrimSuffix(name, ".zstd")
	name = strings.TrimSuffix(name, ".jsonl")
	rest, ok := strings.CutPrefix(name, "session.v")
	if !ok {
		return 0, false
	}
	version, err := strconv.Atoi(rest)
	if err != nil || version < 0 {
		return 0, false
	}
	return version, true
}

// current is a session directory's current transcript: the highest generation in it. Two
// files of one generation — the harness compressing it — are told apart by name, which puts
// the .zstd one last.
func current(dir string) (path string, ok bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false, err
	}
	best, bestVersion := "", -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		version, ok := generation(e.Name())
		if !ok {
			continue
		}
		if version > bestVersion || (version == bestVersion && e.Name() > best) {
			best, bestVersion = e.Name(), version
		}
	}
	if best == "" {
		return "", false, nil
	}
	return filepath.Join(dir, best), true, nil
}

// Scan lists a session per directory. Its stamp is the current generation's file — name
// as well as size and time, so a rotation moves it even to a file that looks the same.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	projects, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var refs []sources.Ref
	var unread error
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !project.IsDir() {
			continue
		}
		projectDir := filepath.Join(s.root, project.Name())
		sessions, err := os.ReadDir(projectDir)
		if err != nil {
			// One project that cannot be read is not the store that cannot: the rest is
			// listed, and the error says the list is short
			unread = errors.Join(unread, err)
			continue
		}
		for _, session := range sessions {
			if !session.IsDir() {
				continue
			}
			dir := filepath.Join(projectDir, session.Name())
			path, ok, err := current(dir)
			if err != nil || !ok {
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			refs = append(refs, sources.Ref{
				Key:     dir,
				Stamp:   filepath.Base(path) + "@" + kit.StampInfo(info),
				ModTime: info.ModTime(),
			})
		}
	}
	return refs, unread
}

// row is one line of a transcript.
type row struct {
	Type string `json:"type"`
	Time any    `json:"time"`
	// "append" or "replace": a bare string in the current format, {"op": …} before it
	SurfaceOp json.RawMessage `json:"surfaceOp"`
	Data      json.RawMessage `json:"data"`

	// the header
	ID        string `json:"id"`
	Cwd       string `json:"cwd"`
	CreatedAt any    `json:"createdAt"` // epoch seconds
}

// replaces reports whether a row is the model-visible replacement of earlier rows.
func (r *row) replaces() bool {
	var op string
	if json.Unmarshal(r.SurfaceOp, &op) != nil {
		var obj struct {
			Op string `json:"op"`
		}
		_ = json.Unmarshal(r.SurfaceOp, &obj)
		op = obj.Op
	}
	return op == "replace"
}

// job is a job/settled event: how a command the harness ran in the background of a tool
// call ended. A bash call's result says only what it printed, and its isError stays false
// whatever the command returned; the exit code is here.
type job struct {
	ToolCall struct {
		CallID string `json:"callId"`
	} `json:"toolCall"`
	StartedAt  any    `json:"startedAt"`
	FinishedAt any    `json:"finishedAt"`
	Detail     string `json:"detail"` // "exit code: 1"
}

// ended is what a settled job says about its call.
type ended struct {
	exit  *int
	durMs int64
}

// data is an event's payload. A user row's data is the message itself; an assistant's and
// a tool result's nest it under "message", and an assistant's usage rides beside it.
type data struct {
	body
	Message body           `json:"message"`
	Usage   map[string]any `json:"usage"`
}

type body struct {
	ID     string `json:"id"`
	Source struct {
		Kind  string `json:"kind"`
		Model string `json:"model"`
	} `json:"source"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	IsError    any             `json:"isError"`
}

// part is one block of a message's content.
type part struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Read parses a session's current transcript.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	dir := ref.Key
	// The generation is looked up again: it may have rotated since the scan, and the one
	// that is current now is the one to read
	path, ok, err := current(dir)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s: no transcript: %w", dir, fs.ErrNotExist)
	}
	w.S.Path = dir

	var headerID string
	sawHeader := false
	seen := map[string]bool{}   // ids of messages added
	looked := map[string]bool{} // ids of messages whose content was looked at, added or not
	jobs := map[string]ended{}  // by call id
	title, titled := "", false  // the harness's own title; one a model wrote outranks the stopgap
	h := &harness{w: w, begun: map[string]time.Time{}}
	n := 0
	// kit.Lines walks a .zstd transcript's chain of frames in order, hands over every line
	// that was completed, and ends without an error where the half-written last frame
	// begins — wherever in that frame the file was cut. What follows the last newline of the
	// decoded stream is a partial append: it is not JSON, and decode skips it.
	err = kit.Lines(path, func(line []byte) bool {
		if n++; n&1023 == 0 && ctx.Err() != nil {
			return false
		}
		var r row
		if !decode(line, &r) {
			return true
		}
		at := kit.Time(r.Time)
		// The settings come as a run of rows, which is drawn as one event once a row of
		// another kind ends it
		if i, ok := settingOf[r.Type]; ok {
			h.setting(i, r.Data, at)
			return true
		}
		h.settle()
		if r.Type != "session" && r.Type != "header" {
			h.onward = true
		}
		switch r.Type {
		case "session", "header":
			if !sawHeader {
				sawHeader = true
				headerID, w.S.Cwd = r.ID, r.Cwd
				w.S.StartedAt = kit.Time(r.CreatedAt)
			}
			return true
		case "session/title":
			var t struct {
				Title  string `json:"title"`
				Source struct {
					Kind string `json:"kind"`
				} `json:"source"`
			}
			if decode(r.Data, &t) && t.Title != "" && (t.Source.Kind != "fallback" || !titled) {
				title, titled = t.Title, titled || t.Source.Kind != "fallback"
			}
			return true
		case "job/settled":
			var j job
			if decode(r.Data, &j) && j.ToolCall.CallID != "" {
				var e ended
				if code, ok := strings.CutPrefix(j.Detail, "exit code: "); ok {
					if n, err := strconv.Atoi(strings.TrimSpace(code)); err == nil {
						e.exit = &n
					}
				}
				if from, to := kit.Time(j.StartedAt), kit.Time(j.FinishedAt); !from.IsZero() && to.After(from) {
					e.durMs = model.DurMs(to.Sub(from).Milliseconds())
				}
				jobs[j.ToolCall.CallID] = e
			}
			return true
		case "user/message", "assistant/message", "tool/result":
		case "system/message":
			return true // the system prompt: what the model is told, no part of a turn
		case "tool/call":
			// The harness running a call, which the assistant's message holds already as
			// a tool-call part
			return true
		case "job/registered":
			return true // a job beginning; how it ended is its job/settled
		case "turn/start":
			// A turn opens at its prompt, a few rows on. What this row says is that the
			// transcript bounds its turns, and so marks where each of them ends
			h.marked = true
			h.failedTurn, h.failed = "", ""
			return true
		case "turn/end":
			h.turnEnded(r.Data, at)
			return true
		case "step/start":
			h.stepStarted(r.Data, at)
			return true
		case "step/end":
			h.stepEnded(r.Data, at)
			return true
		case "request/context":
			h.request(r.Data, at)
			return true
		case "assistant/attempt":
			h.attempted(r.Data, at)
			return true
		case "agent/inbox/spliced":
			// A prompt taken from the harness's inbox into the conversation, or taken out of
			// the inbox: what it puts in is written again as the user/message that follows,
			// and is read there
			return true
		case "request/header":
			return true // the tools and the configuration a request was made with
		case "session/title-llm-request":
			return true // the prompt the title model was given; the title is session/title
		case "session-log-deepseek/delivery-accepted":
			return true // a receipt for the rows a log took delivery of
		default:
			w.Unread(unknown("row", r.Type))
			return true
		}
		if r.replaces() {
			return true // a model-visible rewrite, not a row of the transcript
		}
		var d data
		if !decode(r.Data, &d) {
			return true
		}
		// A repeated id is the same message written again: the first copy that adds something
		// stands, and so does its usage, and the copies after it are passed over before their
		// content is looked at. A copy that adds nothing leaves the next one to stand, but its
		// content was looked at all the same, so a part in it the adapter does not know is not
		// counted again for the copies after it: one message is counted once, however many
		// times it was written
		id := d.Message.ID
		if r.Type == "user/message" {
			id = d.ID
		}
		if id != "" && seen[id] {
			return true
		}
		unread := w.Unread
		if id != "" {
			if looked[id] {
				unread = func(string) {}
			}
			looked[id] = true
		}
		if m, ok := messageOf(&r, &d, jobs, unread); ok {
			if id != "" {
				seen[id] = true
			}
			w.Add(m)
		}
		return true
	})
	// A run of settings the transcript ends on is drawn too — but not the opening run of a
	// session with nothing after it. The harness writes the settings as it opens a session,
	// before anything is asked, and a session opened and left holds nothing: drawn, its run
	// would make it a session with something in it, listed with those that have. Should it
	// go on, the run is drawn then, at the place it would have had now, which nothing took
	if h.onward {
		h.settle()
	}
	// Set whatever the read ended on: a transcript damaged partway is kept as far as it
	// went, and is still the session its header says it is
	w.S.ID = kit.First(headerID, filepath.Base(dir))
	w.S.Title = title
	// A harness that bounds its turns writes the end of every one, and a turn without it is
	// in flight, whatever its last rows were. One that does not says nothing of it that has
	// been checked against a real session, and is left to say nothing
	if h.marked {
		w.Tells = transcript.TellsByMarks
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

// decode reads one line into v. A field of the wrong type is left empty and the rest of
// the row still counts; only a line that is not JSON is skipped.
func decode(line []byte, v any) bool {
	if len(line) == 0 {
		return false
	}
	err := json.Unmarshal(line, v)
	var typeErr *json.UnmarshalTypeError
	return err == nil || errors.As(err, &typeErr)
}

// unknown names a record of a kind the adapter does not know, for Writer.Unread. One that
// does not say what it is — a line of JSON with no type — is such a record too.
func unknown(what, kind string) string {
	if kind == "" {
		kind = "(untyped)"
	}
	return what + ":" + kind
}

// The events the harness's own rows are read as, beside the kinds every adapter shares.
const (
	eventStep     = "step"     // a call to the model and the tools it asked for, once it is over
	eventSettings = "settings" // the permissions, sandbox and approval a session runs with
	eventError    = "error"    // a call or a turn that failed: the kind the adapters give an error
)

// settings are the rows that each set one of the settings a session runs with: the name
// an event gives it, and the field of the row's data that holds it.
var settings = [...]struct{ row, name, field string }{
	{"permission/preset", "permissions", "preset"},
	{"sandbox/mode", "sandbox", "mode"},
	{"approval/policy", "approval", "policy"},
}

// settingOf finds a row among the settings, by its type.
var settingOf = func() map[string]int {
	m := map[string]int{}
	for i, s := range settings {
		m[s.row] = i
	}
	return m
}()

// harness reads the rows the harness writes of its own work — its steps, its settings, the
// requests it makes, how an attempt and a turn ended — and carries what they say from one
// row to the next. What they come to is added to w as events.
type harness struct {
	w *transcript.Writer

	marked bool                 // a turn's bounds were written: so is the end of every turn
	onward bool                 // a row other than the header and the settings was read
	begun  map[string]time.Time // the steps begun and not yet ended, by turn and step

	set, drawn [len(settings)]string // the settings as they stand, and as an event last said them
	setAt      time.Time             // when the run of settings not yet drawn was written
	pending    bool                  // a run of settings is not drawn yet

	model string // the model the latest request was made to

	// The latest attempt that failed, of the turn being read: which turn, and what it said
	failedTurn, failed string
}

// setting notes one of the settings. It is drawn with the rest of its run, by settle.
func (h *harness) setting(i int, raw json.RawMessage, at time.Time) {
	var data map[string]any
	if !decode(raw, &data) {
		return
	}
	if value := strings.TrimSpace(kit.Str(data[settings[i].field])); value != "" {
		h.set[i], h.setAt, h.pending = value, at, true
	}
}

// settle draws the run of settings written since the last was drawn, as one event: every
// value the first time, and after that the ones that changed, from what to what. A run
// that changed nothing — the harness writing them all again as they were — draws nothing.
// A setting is something the person chose, not work of the agent's: it does not move
// where the turn's work ended.
func (h *harness) settle() {
	if !h.pending {
		return
	}
	h.pending = false
	var parts []string
	for i, s := range settings {
		switch now, was := h.set[i], h.drawn[i]; {
		case now == was:
		case was == "":
			parts = append(parts, s.name+" "+now)
		default:
			parts = append(parts, s.name+" "+was+" → "+now)
		}
	}
	h.drawn = h.set
	if len(parts) == 0 {
		return
	}
	text := strings.Join(parts, " · ")
	event := transcript.EventOf(eventSettings, strings.ToUpper(text[:1])+text[1:])
	event.After = true
	h.w.Add(transcript.Message{Role: transcript.System, At: h.setAt, Blocks: []transcript.Block{event}})
}

// step is a step/start or step/end row's data: which step of which turn.
type step struct {
	Turn any `json:"turn"`
	Step any `json:"step"`
}

func (s *step) key() string { return kit.Text(s.Turn) + "/" + kit.Text(s.Step) }

func (h *harness) stepStarted(raw json.RawMessage, at time.Time) {
	var s step
	if decode(raw, &s) && !at.IsZero() {
		h.begun[s.key()] = at
	}
}

// stepEnded draws a step, at its end: only then is how long it took known, and an event
// is added where it falls among the rows. A step still under way draws nothing until it
// ends — the session says it is at work meanwhile — and one that never ends, the harness
// stopped in the middle of it, never does: its work is in the turn all the same.
func (h *harness) stepEnded(raw json.RawMessage, at time.Time) {
	var s step
	if !decode(raw, &s) {
		return
	}
	text := "Step"
	if n := kit.Text(s.Step); n != "" {
		text += " " + n
	}
	if from, ok := h.begun[s.key()]; ok {
		delete(h.begun, s.key())
		if ms := model.DurMs(at.Sub(from).Milliseconds()); ms > 0 {
			text += " · " + txt.Duration(time.Duration(ms)*time.Millisecond)
		}
	}
	h.w.Add(transcript.Message{Role: transcript.System, At: at, Blocks: []transcript.Block{transcript.EventOf(eventStep, text)}})
}

// request reads which model a request was made to. The first names the model the session
// began with, which its replies name too, and draws nothing; one after it that names
// another model is a model change, where that request was made.
func (h *harness) request(raw json.RawMessage, at time.Time) {
	var c struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if !decode(raw, &c) || c.Model == "" {
		return
	}
	if h.model != "" && c.Model != h.model {
		name := c.Model
		if c.Provider != "" {
			name = c.Provider + "/" + c.Model
		}
		h.w.Add(transcript.Message{Role: transcript.System, At: at, Blocks: []transcript.Block{transcript.EventOf(model.EventModelChange, name)}})
	}
	h.model = c.Model
}

// reason is how an attempt or a turn ended: its kind ("completed", "error"), and what
// went wrong when it failed — an attempt's failure, a turn's error.
type reason struct {
	Kind    string  `json:"kind"`
	Failure failure `json:"failure"`
	Error   failure `json:"error"`
}

type failure struct {
	Message string `json:"message"`
	Code    any    `json:"code"`
}

// text is what a failure says: its message, and its code where the message does not hold
// it.
func (f *failure) text() string {
	msg, code := strings.TrimSpace(f.Message), strings.TrimSpace(kit.Text(f.Code))
	switch {
	case code == "" || strings.Contains(msg, code):
		return msg
	case msg == "":
		return code
	}
	return msg + " (" + code + ")"
}

// attempted reads an assistant/attempt: a call to the model as it streamed, which the
// harness writes where the call came to no assistant/message. The last finish in its
// stream says how it ended; one that failed is an error event, and one that ended any
// other way draws nothing.
func (h *harness) attempted(raw json.RawMessage, at time.Time) {
	var a struct {
		Turn   any `json:"turn"`
		Stream []struct {
			Chunk struct {
				Type   string `json:"type"`
				Reason reason `json:"reason"`
			} `json:"chunk"`
		} `json:"stream"`
	}
	if !decode(raw, &a) {
		return
	}
	var end *reason
	for i := range a.Stream {
		if c := &a.Stream[i].Chunk; c.Type == "finish" {
			end = &c.Reason
		}
	}
	if end == nil || end.Kind != "error" {
		return
	}
	text := kit.First(end.Failure.text(), end.Error.text(), "The call to the model failed")
	h.failedTurn, h.failed = kit.Text(a.Turn), text
	h.w.Add(transcript.Message{Role: transcript.System, At: at, Blocks: []transcript.Block{transcript.EventOf(eventError, text)}})
}

// turnEnded reads a turn/end: the turn is over, and the next move is the person's. A turn
// that failed is an error event — unless the turn's last failed attempt said the same,
// which the harness writes again here — and, like the error of every CLI but Gemini CLI,
// does not mark the turn interrupted: that is for a turn stopped before it was done, as
// one the person stopped is. A reason of a kind not known ends the turn all the same, and
// is counted as not understood.
func (h *harness) turnEnded(raw json.RawMessage, at time.Time) {
	h.marked = true
	var e struct {
		Turn   any    `json:"turn"`
		Reason reason `json:"reason"`
	}
	decode(raw, &e)
	m := transcript.Message{Role: transcript.System, At: at, Ends: true}
	switch kind := e.Reason.Kind; {
	case kind == "" || kind == "completed":
	case kind == "error":
		text := kit.First(e.Reason.Error.text(), e.Reason.Failure.text(), "The turn failed")
		if text != h.failed || kit.Text(e.Turn) != h.failedTurn {
			m.Blocks = []transcript.Block{transcript.EventOf(eventError, text)}
		}
	case stopped(kind):
		m.Blocks = []transcript.Block{transcript.EventOf(model.EventInterrupted, "Turn "+kind)}
	default:
		h.w.Unread(unknown("reason", kind))
	}
	h.failedTurn, h.failed = "", ""
	h.w.Add(m)
}

// stopped reports whether a turn's end says it was stopped before it was done. The turns
// recorded so far ended completed or in an error, and none was stopped: a kind is taken by
// the words a harness would stop a turn with, however it spells them.
func stopped(kind string) bool {
	kind = strings.ToLower(kind)
	return strings.Contains(kind, "abort") || strings.Contains(kind, "cancel") || strings.Contains(kind, "interrupt")
}

// messageOf turns an event into what the Writer takes; false for one with nothing to add.
// unread is told of every part of the message's content it passed over not knowing what
// it was.
func messageOf(r *row, d *data, jobs map[string]ended, unread func(kind string)) (transcript.Message, bool) {
	at := kit.Time(r.Time)
	switch r.Type {
	case "user/message":
		m := transcript.Message{ID: d.ID, Role: transcript.User, At: at, Blocks: blocks(d.Content, unread)}
		// A user row the harness wrote itself (context, a summary) is not something the
		// person typed, so a turn does not start at it
		if k := d.Source.Kind; k != "" && k != "user" {
			m.Injected = true
		}
		return m, len(m.Blocks) > 0
	case "assistant/message":
		m := d.Message
		out := transcript.Message{ID: m.ID, Role: transcript.Assistant, At: at, Model: m.Source.Model, Blocks: blocks(m.Content, unread)}
		if u := usageOf(d.Usage); !u.IsZero() {
			out.Usage = &u
		}
		// A response with no words still cost what it cost
		return out, len(out.Blocks) > 0 || out.Usage != nil
	case "tool/result":
		m := d.Message
		if m.ID == "" && m.ToolCallID == "" && len(m.Content) == 0 {
			return transcript.Message{}, false
		}
		// A result names its call, not its tool: pairing gives it the call's name. How a
		// command exited is in the job that ran it, settled just before this row
		failed := kit.Truthy(m.IsError)
		job := jobs[m.ToolCallID]
		if job.exit != nil && *job.exit != 0 {
			failed = true
		}
		outcome := transcript.Outcome{Status: transcript.StatusOf(failed), Exit: job.exit, DurMs: job.durMs}
		return transcript.Message{ID: m.ID, Role: transcript.ToolRole, At: at, Blocks: []transcript.Block{
			transcript.ResultOf(m.ToolCallID, "", resultText(m.Content, unread), outcome),
		}}, true
	}
	return transcript.Message{}, false
}

// parts decodes a message's content: a bare string is one text block.
func parts(raw json.RawMessage) []part {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return nil
		}
		return []part{{Type: "text", Text: s}}
	}
	var list []part
	_ = json.Unmarshal(raw, &list) // what decoded before an oddly typed block stays
	return list
}

// blocks is a message's content as the Writer takes it; unread is told of every part it
// passed over not knowing what it was.
func blocks(raw json.RawMessage, unread func(kind string)) []transcript.Block {
	var out []transcript.Block
	for _, p := range parts(raw) {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, transcript.TextOf(p.Text))
			}
		case "thinking", "reasoning":
			if text := kit.First(p.Text, p.Thinking); text != "" {
				out = append(out, transcript.ThinkingOf(text))
			}
		case "tool-call":
			out = append(out, transcript.CallOf(p.ID, p.Name, arguments(p.Arguments)))
		default:
			unread(unknown("part", p.Type))
		}
	}
	return out
}

// arguments are a call's input: an object, or — as the harness writes them — a string
// holding the object's JSON.
func arguments(raw json.RawMessage) any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return raw
}

// resultText is a tool result's output: the text of its blocks, one per line. A block of
// any type gives the text it holds; one that holds none and is no text block at all — an
// image, say — is passed over, and unread is told.
func resultText(raw json.RawMessage, unread func(kind string)) string {
	var texts []string
	for _, p := range parts(raw) {
		switch {
		case p.Text != "":
			texts = append(texts, p.Text)
		case p.Type != "text":
			unread(unknown("part", p.Type))
		}
	}
	return strings.Join(texts, "\n")
}

// usageOf converts the harness's counters to the canonical shape. inputTokens excludes the
// cache — cacheReadTokens and cacheWriteTokens are counted apart, and totalTokens is the
// four added up — so it is the canonical Input as it stands.
//
// Keys are folded to lowercase without underscores before they are matched. A counter is
// read by the one rule for a token count (kit.Count): a figure below zero, or past what one
// response could use, is no count and is read as none.
func usageOf(raw map[string]any) (u model.Usage) {
	// Two keys that mean one counter — input_tokens and prompt_tokens — are read as the
	// larger, so that a row reads the same however its map is walked
	for key, v := range raw {
		switch strings.ReplaceAll(strings.ToLower(key), "_", "") {
		case "inputtokens", "input", "prompttokens":
			u.Input = max(u.Input, kit.Count(v))
		case "outputtokens", "output", "completiontokens":
			u.Output = max(u.Output, kit.Count(v))
		case "cachereadtokens", "cacheread", "cachereadinputtokens":
			u.CacheRead = max(u.CacheRead, kit.Count(v))
		case "cachewritetokens", "cachewrite", "cachecreationinputtokens":
			u.CacheWrite = max(u.CacheWrite, kit.Count(v))
		case "reasoningtokens", "reasoning":
			u.Reasoning = max(u.Reasoning, kit.Count(v))
		}
	}
	return u
}
