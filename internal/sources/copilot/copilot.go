// Package copilot reads GitHub Copilot CLI's sessions.
//
//	<root>/<session id>/events.jsonl      the session's event log
//	<root>/<session id>/events.<n>.jsonl  an earlier part of it, once the log was rotated
//	<root>/<session id>/workspace.yaml    what the CLI keeps beside it: the session's name
//
// <root> is session-state under COPILOT_HOME, ~/.copilot on every system by default.
//
// The log is one event per line: an id, the time it was emitted, the event before it, a type
// and the type's data — the event stream the CLI's runtime hands its clients, less the
// events it marks ephemeral (deltas, progress, the per-call usage report, title changes),
// which never reach the file. The CLI ships the schema of every event
// (schemas/session-events.schema.json); this reading follows the one of Copilot CLI 1.0.114.
//
// A session opens with session.start (its id, the CLI's version, the model selected, the
// directory and branch it ran in). A person's message is user.message; a model response is
// assistant.message — its text, the tool calls it requested, and the provider's id for the
// call (apiCallId), which every part of one response shares — and its reasoning, when the
// model gave some, is assistant.reasoning before it. A tool's run is tool.execution_start and
// tool.execution_complete; what a model call used, session.usage_record. A subagent's events
// are written into the same log, each marked with the agent's id.
package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/jsonmap"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Spec describes GitHub Copilot CLI.
var Spec = sources.Spec{
	Name:  "copilot",
	Title: "GitHub Copilot CLI",
	Locate: func(env sources.Env) string {
		// COPILOT_HOME replaces ~/.copilot whole, on every system; XDG_CONFIG_HOME and
		// XDG_STATE_HOME do not move it (copilot help environment)
		home := filepath.Join(env.Home, ".copilot")
		if d := env.Getenv("COPILOT_HOME"); d != "" {
			home = d
		}
		return filepath.Join(home, "session-state")
	},
	Open:    func(location string) sources.Source { return &Source{root: location} },
	Movable: true,
	// --resume takes a session's id (or a prefix of it, or its name) as the word after it,
	// and finds the session in COPILOT_HOME from any directory
	Resume: "copilot --resume",
	Reads:  1,
	Store:  "~/.copilot/session-state/<id>/events.jsonl",
	// No Retention: Copilot CLI keeps a session until someone deletes it ("nothing expires
	// them", copilot help sessions)
}

// Source is a Copilot CLI session-state directory.
type Source struct{ root string }

func (s *Source) Location() string { return s.root }
func (s *Source) Exists() bool     { return kit.Exists(s.root) }

// Scan lists every session's log. A session is a directory; its log is events.jsonl, and
// the workspace.yaml beside it is part of the stamp, as a session renamed is read again.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	matches, unread := kit.Glob(filepath.Join(s.root, "*", "events.jsonl"))
	refs := make([]sources.Ref, 0, len(matches))
	for _, path := range matches {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			continue
		}
		stamp, mod := kit.Stamp(path, filepath.Join(filepath.Dir(path), "workspace.yaml"))
		refs = append(refs, sources.Ref{Key: path, Stamp: stamp, ModTime: mod})
	}
	return refs, unread
}

// event is one line of the log.
type event struct {
	ID        string `json:"id"`
	Timestamp any    `json:"timestamp"`
	Type      string `json:"type"`
	// Ephemeral events are not written to the log; one that is, is not read.
	Ephemeral bool `json:"ephemeral"`
	// AgentID marks a subagent's event.
	AgentID string `json:"agentId"`
	// Checkpoint marks a copy of earlier events at the head of a rotated log, which the
	// CLI replays from: the events themselves are in the log before it.
	Checkpoint bool            `json:"checkpoint"`
	Data       json.RawMessage `json:"data"`
}

// data is every field of an event's data that is read here; which are there depends on
// the event's type.
type data struct {
	// session.start, session.resume
	SessionID      string `json:"sessionId"`
	CopilotVersion string `json:"copilotVersion"`
	StartTime      any    `json:"startTime"`
	SelectedModel  string `json:"selectedModel"`
	Context        *place `json:"context"`
	// A session that carries on the context of another, which started it detached
	DetachedFrom string `json:"detachedFromSpawningParentSessionId"`

	// session.model_change
	NewModel string `json:"newModel"`
	// session.title_changed
	Title string `json:"title"`
	// session.journal_link
	PreviousFile string `json:"previousFile"`

	// user.message, assistant.message, assistant.reasoning, system.notification
	Content string `json:"content"`
	// user.message
	Source                  string `json:"source"`
	Delivery                string `json:"delivery"`
	IsAutopilotContinuation bool   `json:"isAutopilotContinuation"`
	// Which subagent an event is from, as events written before agentId went on every
	// event say it
	AgentID          string `json:"agentId"`
	ParentToolCallID string `json:"parentToolCallId"`

	// assistant.message, assistant.reasoning
	Model         string        `json:"model"`
	APICallID     string        `json:"apiCallId"`
	ReasoningText string        `json:"reasoningText"`
	ToolRequests  []toolRequest `json:"toolRequests"`

	// tool.user_requested, tool.execution_start, tool.execution_complete,
	// permission.completed
	ToolCallID     string          `json:"toolCallId"`
	ToolName       string          `json:"toolName"`
	Arguments      json.RawMessage `json:"arguments"`
	Success        *bool           `json:"success"`
	Result         *result         `json:"result"`
	Error          json.RawMessage `json:"error"` // an object for a tool or a hook, a string for a compaction
	ShellExecution *struct {
		ExitCode *int `json:"exitCode"`
	} `json:"shellExecution"`

	// session.usage_record
	Usage *usage `json:"usage"`
	// abort
	Reason string `json:"reason"`
	// session.error
	Message string `json:"message"`
	// session.compaction_complete
	SummaryContent string `json:"summaryContent"`
	// hook.end
	HookType string `json:"hookType"`
}

// place is where a session ran.
type place struct {
	Cwd    string `json:"cwd"`
	Branch string `json:"branch"`
}

// toolRequest is a tool call a response asked for.
type toolRequest struct {
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
}

// result is a tool's result (tool.execution_complete) or a permission's (permission.completed),
// which share the name.
type result struct {
	// What the model was given, and the fuller text the CLI shows (an edit's diff)
	Content         string `json:"content"`
	DetailedContent string `json:"detailedContent"`
	// How a permission request was settled
	Kind string `json:"kind"`
}

// usage is what one model call used: the shape of assistant.usage, kept in the log as
// session.usage_record.
//
// Copilot CLI's input is every prompt token sent, those read from or written to the cache
// included ("↑ is tokens sent, including those read from or written to the prompt cache",
// copilot help billing), and its output every token generated, the reasoning included
// ("↓ output (N reasoning)"). The cost is in nano AI units: 10⁹ make an AI credit (copilot
// help billing), and an AI credit is $0.01 (docs.github.com, Copilot billing for
// individuals).
type usage struct {
	Model              string  `json:"model"`
	InputTokens        float64 `json:"inputTokens"`
	OutputTokens       float64 `json:"outputTokens"`
	CacheReadTokens    float64 `json:"cacheReadTokens"`
	CacheWriteTokens   float64 `json:"cacheWriteTokens"`
	ReasoningTokens    float64 `json:"reasoningTokens"`
	Duration           float64 `json:"duration"`
	TimeToFirstTokenMs float64 `json:"timeToFirstTokenMs"`
	FinishReason       string  `json:"finishReason"`
	APICallID          string  `json:"apiCallId"`
	CopilotUsage       *struct {
		TotalNanoAiu float64 `json:"totalNanoAiu"`
	} `json:"copilotUsage"`
}

// canonical is the call's counters in the record's shape: the input without what the
// cache served or stored. An input smaller than the cache's counts cannot have held them
// — they were counted apart from it — and it stands as it was given.
func (u *usage) canonical() *model.Usage {
	in, read, write := model.TokensOf(u.InputTokens), model.TokensOf(u.CacheReadTokens), model.TokensOf(u.CacheWriteTokens)
	if read+write <= in {
		in -= read + write
	}
	return &model.Usage{Input: in, Output: model.TokensOf(u.OutputTokens), CacheRead: read, CacheWrite: write,
		Reasoning: model.TokensOf(u.ReasoningTokens)}
}

// costUSD is what the call cost, from the AI credits Copilot counts it at.
func (u *usage) costUSD() float64 {
	if u.CopilotUsage == nil {
		return 0
	}
	return jsonmap.Cost(u.CopilotUsage.TotalNanoAiu / nanoAiuPerUSD)
}

// nanoAiuPerUSD: 10⁹ nano AI units are an AI credit, and a hundred AI credits a dollar.
const nanoAiuPerUSD = 1e11

// millis is a duration in milliseconds, which the CLI writes as a number: one that is not
// above zero, or past what any call lasts, is none.
func millis(ms float64) int64 {
	if !(ms > 0 && ms <= model.MaxDurMs) { // NaN fails both
		return 0
	}
	return int64(ms)
}

// Read parses one session: its log, the earlier parts of it a rotation left, and the name
// workspace.yaml gives it.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	dir := filepath.Dir(ref.Key)
	w.S.Path = ref.Key
	w.S.ID = filepath.Base(dir) // until session.start says
	r := &reader{w: w, calls: map[string]*call{}, reasoned: map[string]bool{}, laneReasoned: map[string]bool{}, declined: map[string]bool{}}
	parts := chain(ref.Key)
	n := 0
	for i, part := range parts {
		// A part whose predecessor was read opens with copies of what the predecessor
		// holds; a part whose predecessor is gone has only them
		linked := i > 0
		err := kit.Lines(part, func(line []byte) bool {
			if n++; n&1023 == 0 && ctx.Err() != nil {
				return false
			}
			r.line(line, linked)
			return true
		})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	r.settleStopped()

	ws := readWorkspace(filepath.Join(dir, "workspace.yaml"))
	w.S.Title = kit.First(ws.Name, ws.Summary, r.title)
	w.S.Cwd = kit.First(r.cwd, ws.Cwd)
	w.S.Branch = r.branch
	w.S.Version = r.version
	w.S.StartedAt = r.started
	// The model that answered last, as the Writer would have it — but not a subagent's,
	// which answered the agent that started it
	w.S.Model = kit.First(r.answered, r.selected)
	w.S.UpdatedAt = r.latest
	return nil
}

// maxParts bounds how far back a rotated log is followed: further than any session is
// rotated, and short of a chain that loops.
const maxParts = 4096

// chain is the parts of a session's log, oldest first, the log itself last.
//
// Copilot CLI rotates a long log: it moves it aside under a numbered name and starts the
// log again with a session.journal_link that names the part before it (previousFile),
// followed by copies of what it needs to resume from, marked checkpoint. Each part it
// moved aside opened the same way, so the parts are found from the newest back.
func chain(head string) []string {
	parts := []string{head}
	seen := map[string]bool{head: true}
	for len(parts) < maxParts {
		prev := predecessor(parts[0])
		if prev == "" {
			break
		}
		// The CLI names a part in the session's own directory, and nothing else is read
		if prev != filepath.Base(prev) || prev == "." || prev == ".." || strings.ContainsAny(prev, `/\`) {
			break
		}
		path := filepath.Join(filepath.Dir(head), prev)
		if seen[path] || !kit.Exists(path) {
			break // a part deleted: the copies at the head of the one after it are all there is
		}
		seen[path] = true
		parts = append([]string{path}, parts...)
	}
	return parts
}

// predecessor is the part a log's first line links to, if it opens with a link.
func predecessor(path string) string {
	var prev string
	_ = kit.Lines(path, func(line []byte) bool {
		var e event
		var d data
		if decode(line, &e) && e.Type == "session.journal_link" && decode(e.Data, &d) {
			prev = d.PreviousFile
		}
		return false // only the first line links
	})
	return prev
}

// call is what is known of one tool call while its result is awaited.
type call struct {
	name    string
	lane    string    // the subagent that made it, "" for the session's own agent
	started time.Time // when it began to run
	done    bool

	// An abort ended the run the call was part of: when, and whether that abort was a
	// fork's copy of its parent's
	stopped          bool
	stoppedAt        time.Time
	stoppedInherited bool
}

// reader is one session being read.
type reader struct {
	w *transcript.Writer

	begun    bool // session.start has been read
	started  time.Time
	latest   time.Time // the last time any event says
	forkedAt time.Time // while set, the events read are a fork's copy of its parent's
	cwd      string
	branch   string
	version  string
	title    string
	selected string // the model selected
	answered string // the model that gave the session's own agent its last response

	calls map[string]*call
	open  []string // the calls without a result, in the order they were made
	// The calls whose response's reasoning came as an event of its own, and the lanes that
	// have had reasoning since their last response: assistant.message repeats it
	reasoned     map[string]bool
	laneReasoned map[string]bool
	// The calls whose permission the person declined, or that were cancelled before they ran
	declined map[string]bool
}

// line reads one event.
func (r *reader) line(raw []byte, linked bool) {
	var e event
	if !decode(raw, &e) {
		return
	}
	if e.Ephemeral || e.Checkpoint && linked || e.Type == "session.journal_link" {
		return // the link was followed before the log was read
	}
	at := kit.Time(e.Timestamp)
	if at.After(r.latest) {
		r.latest = at
	}
	var d data
	if len(e.Data) > 0 {
		decode(e.Data, &d)
	}

	if e.Type == "session.start" {
		if r.begun {
			return
		}
		r.begun = true
		r.w.S.ID = kit.First(d.SessionID, r.w.S.ID)
		r.version, r.selected = d.CopilotVersion, d.SelectedModel
		r.started = kit.Time(d.StartTime)
		if r.started.IsZero() {
			r.started = at
		}
		if d.Context != nil {
			r.cwd, r.branch = d.Context.Cwd, d.Context.Branch
		}
		if d.DetachedFrom != "" {
			// A run the session started to carry on without it — the background agent that
			// consolidates what a session learned — is a session beneath the one it serves
			r.w.S.Parent = &model.Parent{Session: d.DetachedFrom, Depth: 1}
		}
		// /fork copies a session's events into a new log and rewrites only its
		// session.start, to the moment of the fork: the events after it that are older
		// than it are the parent's, as they were
		r.forkedAt = at
		return
	}
	if !r.forkedAt.IsZero() && !at.IsZero() && !at.Before(r.forkedAt) {
		r.forkedAt = time.Time{} // the session's own events from here
	}
	inherited := !r.forkedAt.IsZero()
	add := func(m transcript.Message) {
		m.ID, m.Inherited = e.ID, inherited
		if m.At.IsZero() {
			m.At = at
		}
		r.w.Add(m)
	}
	happened := func(kind, text string) {
		add(transcript.Message{Role: transcript.System, Blocks: []transcript.Block{transcript.EventOf(kind, text)}})
	}
	// Which agent wrote an event: the subagent the CLI marks it with, as the CLI itself
	// tells them apart in its timeline (agentId on the event, or on its data; before
	// that, the call that started the subagent)
	lane := kit.First(e.AgentID, d.AgentID, d.ParentToolCallID)

	switch e.Type {
	case "session.resume":
		r.selected = kit.First(d.SelectedModel, r.selected)
		if r.cwd == "" && d.Context != nil {
			r.cwd, r.branch = d.Context.Cwd, d.Context.Branch
		}
	case "session.model_change":
		if d.NewModel != "" {
			r.selected = d.NewModel
			happened(model.EventModelChange, d.NewModel)
		}
	case "session.title_changed":
		r.title = kit.First(d.Title, r.title)

	case "user.message":
		r.userMessage(&d, lane, add)
	case "system.notification":
		// What the CLI tells the model of its own accord: a background command or agent
		// finished, a message arrived
		if d.Content != "" {
			add(transcript.Message{Role: transcript.User, Injected: true, Blocks: []transcript.Block{transcript.TextOf(d.Content)}})
		}

	case "assistant.reasoning":
		if d.APICallID != "" {
			r.reasoned[d.APICallID] = true
		}
		r.laneReasoned[lane] = true
		if d.Content != "" {
			add(transcript.Message{Role: transcript.Assistant, Response: d.APICallID, Model: d.Model,
				Blocks: []transcript.Block{transcript.ThinkingOf(d.Content)}})
		}
	case "assistant.message":
		r.assistantMessage(&d, lane, inherited, add)
	case "session.usage_record", "assistant.usage":
		// One model call's usage. assistant.usage is ephemeral — this is it kept — and
		// is read too should a log hold it: the two name the same call, whose counters
		// are the largest any of its rows reports, never their sum
		u := d.Usage
		if e.Type == "assistant.usage" {
			u = &usage{}
			decode(e.Data, u)
		}
		if u == nil {
			return
		}
		if lane == "" && u.Model != "" && !inherited {
			r.answered = u.Model
		}
		add(transcript.Message{Role: transcript.Assistant, Response: u.APICallID, Model: u.Model,
			Usage: u.canonical(), CostUSD: u.costUSD(), Stop: u.FinishReason,
			DurMs: millis(u.Duration), TTFTMs: millis(u.TimeToFirstTokenMs)})

	case "tool.user_requested":
		// A tool the person ran themselves (a ! command): a call no response asked for
		if c := r.request(d.ToolCallID, d.ToolName, d.Arguments, lane); c != nil {
			add(transcript.Message{Role: transcript.ToolRole, Blocks: []transcript.Block{transcript.CallOf(d.ToolCallID, c.name, d.Arguments)}})
		}
	case "tool.execution_start", "program.tool_execution_start":
		// The call was asked for in a response; a tool that a program a call ran called
		// in turn was asked for by no response, and is a call of its own here
		known := r.calls[d.ToolCallID] != nil
		c := r.request(d.ToolCallID, d.ToolName, d.Arguments, lane)
		if c == nil {
			return
		}
		c.started = at
		if !known {
			add(transcript.Message{Role: transcript.ToolRole, Blocks: []transcript.Block{transcript.CallOf(d.ToolCallID, c.name, d.Arguments)}})
		}
	case "tool.execution_complete", "program.tool_execution_complete":
		r.complete(&d, at, add)
	case "permission.completed":
		if d.Result != nil && d.ToolCallID != "" {
			switch d.Result.Kind {
			case "denied-interactively-by-user", "cancelled":
				r.declined[d.ToolCallID] = true
			}
		}
	case "abort":
		// The person stopped the run (Ctrl+C, a remote command, an MCP server's abort):
		// every call still running was stopped with it. A subagent's own abort ends its
		// calls and not the turn
		r.stop(lane, at, inherited)
		if lane == "" {
			happened(model.EventInterrupted, d.Reason)
		}

	case "session.compaction_complete":
		if d.Success != nil && *d.Success && d.SummaryContent != "" {
			happened(model.EventCompaction, d.SummaryContent)
		}
	case "session.error":
		if d.Message != "" {
			happened("error", d.Message)
		}
	case "hook.end":
		if d.Success != nil && !*d.Success {
			happened(model.EventHookError, kit.First(errorText(d.Error), d.HookType))
		}

	default:
		if !passedOver[e.Type] {
			r.w.Unread("event:" + kit.First(e.Type, "(untyped)"))
		}
	}
}

// userMessage reads what was said in the user's seat.
func (r *reader) userMessage(d *data, lane string, add func(transcript.Message)) {
	if lane != "" || d.Content == "" {
		// A subagent's task, which the call that started it carries; or a message of
		// attachments alone
		return
	}
	m := transcript.Message{Role: transcript.User, Blocks: []transcript.Block{transcript.TextOf(d.Content)}}
	if typed(d.Source) && !d.IsAutopilotContinuation {
		// Words the person sent while the agent worked go into the run under way
		// ("steering"); words sent to wait for it ("queued") are a run of their own when
		// their turn comes, which is when they are written
		m.Queued = d.Delivery == "steering"
		r.settleStopped()
	} else {
		m.Injected = true
	}
	add(m)
}

// typed reports whether a user message's source is a person's, by the CLI's own rule: what
// it marks "user", or nothing, and what a command, a schedule or an autopilot objective sent
// on their behalf (command-*, schedule-*, autopilot-*: /autopilot <objective> sends
// autopilot-objective). Every other source — a skill's hidden prompt, another agent's,
// autopilot's own — is the CLI's. The CLI counts the prompts the person typed the same way,
// less the continuations it marks isAutopilotContinuation: the person set the objective
// once, and autopilot sends the next step.
func typed(source string) bool {
	for _, prefix := range []string{"command-", "schedule-", "autopilot-"} {
		if strings.HasPrefix(source, prefix) {
			return true
		}
	}
	return source == "" || source == "user"
}

// assistantMessage reads a response, or the part of one a message holds.
func (r *reader) assistantMessage(d *data, lane string, inherited bool, add func(transcript.Message)) {
	var blocks []transcript.Block
	// The reasoning, when no event of its own carried it
	if d.ReasoningText != "" && !r.reasoned[d.APICallID] && !(d.APICallID == "" && r.laneReasoned[lane]) {
		blocks = append(blocks, transcript.ThinkingOf(d.ReasoningText))
	}
	r.laneReasoned[lane] = false
	if d.Content != "" {
		if lane == "" {
			blocks = append(blocks, transcript.TextOf(d.Content))
		} else {
			// A subagent's words are said to the agent that started it, which has them as
			// the call's result: in the person's turn they are that work's reasoning, and
			// the turn's answer stays the session's own agent's
			blocks = append(blocks, transcript.ThinkingOf(d.Content))
		}
	}
	for _, t := range d.ToolRequests {
		if c := r.request(t.ToolCallID, t.Name, t.Arguments, lane); c != nil {
			blocks = append(blocks, transcript.CallOf(t.ToolCallID, c.name, t.Arguments))
		}
	}
	if lane == "" && d.Model != "" && !inherited {
		r.answered = d.Model
	}
	add(transcript.Message{Role: transcript.Assistant, Response: d.APICallID, Model: d.Model, Blocks: blocks})
}

// request notes a tool call, the first time it is named; nil when it names no call.
func (r *reader) request(id, name string, args json.RawMessage, lane string) *call {
	if id == "" {
		return nil
	}
	if c := r.calls[id]; c != nil {
		return c
	}
	c := &call{name: callName(name, args), lane: lane}
	r.calls[id] = c
	r.open = append(r.open, id)
	return c
}

// callName is the name a call is read under: the tool's, but for the one tool whose
// argument says whether it reads or writes. str_replace_editor views a file as well as
// creating and editing one, by its command, and the CLI itself tells them apart by it; a
// view is named for what it did, so that the file does not read as changed.
func callName(name string, args json.RawMessage) string {
	if name == "str_replace_editor" {
		var a struct {
			Command string `json:"command"`
		}
		if decode(args, &a) && a.Command == "view" {
			return "view"
		}
	}
	return name
}

// complete reads a call's result.
func (r *reader) complete(d *data, at time.Time, add func(transcript.Message)) {
	c := r.calls[d.ToolCallID]
	name := ""
	if c != nil {
		name = c.name
		if c.done {
			return // a result for a call that has one: the first stands
		}
		c.done = true
		r.closeCall(d.ToolCallID)
	}
	output := ""
	if d.Result != nil {
		output = kit.First(d.Result.DetailedContent, d.Result.Content)
	}
	if msg := errorText(d.Error); msg != "" {
		output = strings.TrimSpace(msg + "\n\n" + output)
	}
	outcome := transcript.Outcome{}
	if d.ShellExecution != nil && d.ShellExecution.ExitCode != nil {
		outcome.Exit = d.ShellExecution.ExitCode
	} else if shellTool(name) && d.Result != nil {
		outcome.Exit = exitCode(d.Result.Content)
	}
	if d.Success != nil {
		// A command that ran is a success to the tool whatever it exited with: the exit
		// code says how the command went
		failed := !*d.Success || outcome.Exit != nil && *outcome.Exit != 0
		outcome.Status = transcript.StatusOf(failed)
		if failed && (r.declined[d.ToolCallID] || c != nil && c.stopped || declinedText(d.Error)) {
			outcome.Status = model.StatusInterrupted
		}
	}
	if c != nil && !c.started.IsZero() && !at.IsZero() {
		outcome.DurMs = model.DurMs(at.Sub(c.started).Milliseconds())
	}
	add(transcript.Message{Role: transcript.ToolRole, Blocks: []transcript.Block{transcript.ResultOf(d.ToolCallID, name, output, outcome)}})
}

// closeCall takes a call off the list of those awaiting a result.
func (r *reader) closeCall(id string) {
	for i, open := range r.open {
		if open == id {
			r.open = append(r.open[:i], r.open[i+1:]...)
			return
		}
	}
}

// stop marks the calls an abort ended: every call still running, or a subagent's own when
// the abort was its. Their results may yet come — a tool finishes what it was doing — and
// until the person speaks again a result is waited for.
func (r *reader) stop(lane string, at time.Time, inherited bool) {
	for _, id := range r.open {
		if c := r.calls[id]; !c.stopped && (lane == "" || c.lane == lane) {
			c.stopped, c.stoppedAt, c.stoppedInherited = true, at, inherited
		}
	}
}

// settleStopped gives each call an abort ended, and that never had a result, the one it
// would have had: it was interrupted, when the abort came. The CLI shows such a call the
// same way ("Operation aborted by user").
func (r *reader) settleStopped() {
	kept := r.open[:0]
	for _, id := range r.open {
		c := r.calls[id]
		if !c.stopped {
			kept = append(kept, id)
			continue
		}
		c.done = true
		r.w.Add(transcript.Message{Role: transcript.ToolRole, At: c.stoppedAt, Inherited: c.stoppedInherited, Blocks: []transcript.Block{
			transcript.ResultOf(id, c.name, "", transcript.Outcome{Status: model.StatusInterrupted})}})
	}
	r.open = kept
}

// errorText is the message of an error, as an object ({"message": …}) or as a string.
func errorText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var e struct {
		Message string `json:"message"`
	}
	decode(raw, &e)
	return e.Message
}

// declinedText reports whether a tool's error says it never ran because the person said
// no, or the call was cancelled before it ran: the code the CLI gives a rejected call
// ("rejected", beside "denied" for one a rule or a hook refused), or its words for either.
func declinedText(raw json.RawMessage) bool {
	var e struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if !decode(raw, &e) {
		return false
	}
	return e.Code == "rejected" || strings.HasPrefix(e.Message, "The user rejected this tool call") ||
		strings.HasPrefix(e.Message, "Tool call was cancelled")
}

// shellTool reports whether a tool runs commands: bash, or powershell on Windows, their
// companions for a command left running (read_bash, write_bash …), and local_shell, the
// one a person's ! command runs.
func shellTool(name string) bool {
	switch name {
	case "bash", "powershell", "shell", "local_shell":
		return true
	}
	for _, verb := range []string{"read_", "write_"} {
		if rest, ok := strings.CutPrefix(name, verb); ok && (rest == "bash" || rest == "powershell") {
			return true
		}
	}
	return false
}

// exitMark is how a shell tool tells the model a command's exit code, at the end of what it
// printed: "<exited with exit code 1>"; for a command read back by its id, "<command with
// id: 3 exited with exit code 1>" or "<shellId: 3 completed with exit code 1>".
var exitMark = regexp.MustCompile(`<(?:command with id: [^\s>]+ |shellId: [^\s>]+ )?(?:exited|completed) with exit code (-?\d+)>\s*$`)

// exitCode is the exit code a shell tool's result ends with, if it ends with one.
func exitCode(text string) *int {
	if len(text) > 512 {
		text = text[len(text)-512:]
	}
	m := exitMark.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	code, err := strconv.Atoi(m[1])
	if err != nil {
		return nil
	}
	return transcript.ExitOf(code)
}

// decode reads a JSON object into v. A field whose value is not of the type expected is
// left empty and the rest is still read; a value that is not an object at all is not an
// event, or not the part of one sought, and is passed over.
func decode(raw []byte, v any) bool {
	if raw = bytes.TrimLeft(raw, " \t\r\n"); len(raw) == 0 || raw[0] != '{' {
		return false
	}
	err := json.Unmarshal(raw, v)
	var mismatch *json.UnmarshalTypeError
	return err == nil || errors.As(err, &mismatch)
}

// passedOver are the events of Copilot CLI 1.0.114's schema that are read past on purpose.
// The ephemeral ones among them never reach the log; they are named so that a log an
// older or a newer CLI wrote them to reads as what it is.
var passedOver = setOf(
	// The agent loop's bookkeeping — each model call it makes is a turn_start and a
	// turn_end of its own — and the live stream of what the events read here hold whole
	"assistant.turn_start assistant.turn_end assistant.turn_retry assistant.intent assistant.idle",
	"assistant.message_start assistant.message_delta assistant.streaming_delta assistant.reasoning_delta",
	"assistant.tool_call_delta assistant.server_tool_progress agent.interrupted pending_messages.modified",
	"model.call_start model.call_finished model.call_failure model.call_final_result prompt_cache_break session.idle",
	// A tool's progress, and what ran around it; a subagent's start and end, which the call
	// that started it holds
	"tool.execution_progress tool.shell_output hook.start hook.progress sandbox.decision tool_search.activated",
	"program.execution program.permission_requested external_tool.requested external_tool.completed mcp_app.tool_call_complete",
	"subagent.started subagent.configured subagent.completed subagent.failed subagent.selected subagent.deselected",
	// Permissions and questions asked and answered (permission.completed is read)
	"permission.requested permission.carriedForward permission.messageAuthorization permission.messageAuthorizationRead",
	"permission.messageAuthorizationDegraded permission.assentDetected permission.contextualAuthorization",
	"session.permission_recovery human_response.recorded user_input.requested user_input.completed",
	"elicitation.requested elicitation.completed sampling.requested sampling.completed exit_plan_mode.requested exit_plan_mode.completed",
	// The prompt, and what the CLI puts in it: instructions, a skill's text
	"system.message instruction.update instruction.update_delivery skill.invoked skill.invoked_ref",
	"skill.context_delivered skill.context_delivered_ref",
	// The session's settings and state, and the CLI's notices about them
	"session.context_changed session.mode_changed session.mode_notice_delivered session.model_deselected",
	"session.permissions_changed session.plan_changed session.workspace_file_changed session.session_limits_changed",
	"session.remote_steerable_changed session.autopilot_objective_changed session.autopilot_stopped session.auto_mode_resolved",
	"session.schedule_created session.schedule_cancelled session.schedule_rearmed session.handoff session.task_complete",
	"session.completion_receipt session.todos_changed session.memory_changed session.snapshot_rewind session.context_cleared",
	"session.truncation session.compaction_start session.compaction_summarized session.info session.warning",
	"session.binary_asset session.canvas.recorded session.canvas.removed session.request_body_compression_disabled",
	"session.tool_outcomes_recorded session.auto_tier_recommendation session.auto_tier_switch_failed session.indexed_search",
	// The session's totals, which the calls' own usage adds up to, and the account's quota
	"session.shutdown session.usage_checkpoint session.usage_info session.quota_observation session.fusion_change_checkpoint",
	// Experimental routing of one turn across models (HydraFusion)
	"session.fusion_route_started session.fusion_route_failed session.fusion_resolved session.fusion_handoff",
	"session.fusion_commit_started session.fusion_completed assistant.fusion_phase_started assistant.fusion_phase_activity",
	"assistant.fusion_phase_completed assistant.fusion_phase_failed",
	// The CLI's live state: its connections, extensions, servers and screens
	"ide.connection_changed ide.selection_changed ide.add_file_reference ide.elicitation_complete",
	"mcp.oauth_required mcp.oauth_completed mcp.oauth_recovery_completed mcp.headers_refresh_required mcp.headers_refresh_completed",
	"mcp.tools.list_changed mcp.resources.list_changed mcp.prompts.list_changed session.mcp_servers_loaded",
	"session.mcp_server_status_changed session.mcp_server_tools_loaded session.mcp_server_removed session.mcp_server_needs_reconnect",
	"session.custom_notification ui.ephemeral_query command.queued command.execute command.completed commands.changed",
	"capabilities.changed auto_mode_switch.requested auto_mode_switch.completed auto_mode_recovery_consent.requested",
	"auto_mode_recovery_consent.completed auto_mode_recovery.outcome session_limits_exhausted.requested session_limits_exhausted.completed",
	"session.managed_plugin_progress session.managed_settings_resolved session.managed_settings_enforced session.tools_updated",
	"session.background_tasks_changed session.skills_loaded session.custom_agents_updated session.extensions_loaded",
	"session.extensions.attachments_pushed session.canvas.opened session.canvas.registry_changed session.canvas.closed",
	"session.canvas.unavailable workflow.run_updated workflow.run_started workflow.run_settled",
)

func setOf(lists ...string) map[string]bool {
	out := map[string]bool{}
	for _, list := range lists {
		for _, name := range strings.Fields(list) {
			out[name] = true
		}
	}
	return out
}
