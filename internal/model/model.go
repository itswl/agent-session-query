// Package model is the one record every agent CLI's transcript converges on.
//
// Eleven CLIs write eleven shapes. What they all describe is the same thing: a person
// asked, an agent worked, the agent answered. That exchange is the unit here — a Turn — and
// a Session is its turns in order. Everything the program offers (the list, search, briefs,
// usage, failures, traces) is a projection of this record and of nothing else.
//
//	Session ─┬─ Turn 0   what came before the first ask: injected context, a fork's copy
//	         ├─ Turn 1   Ask → Items (reply, thinking, tool, event …) → Answer
//	         └─ Turn n
//
// A tool call and its result are one Item: they are paired when the transcript is read,
// once, so no reader has to pair them again.
package model

import (
	"encoding/json"
	"math"
	"time"
)

// Session is one conversation with an agent CLI.
type Session struct {
	// Source is the store the session was read from: "claude", or a labeled instance such
	// as "claude@box2". Agent is the CLI that wrote it ("claude"), which is what decides
	// how it resumes.
	Source string `json:"source"`
	Agent  string `json:"agent"`
	// ID is the CLI's own id for the session; Source and ID together name it.
	ID   string `json:"id"`
	Path string `json:"path,omitempty"` // where it lives: a file, a directory, a database

	Title    string `json:"title,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Branch   string `json:"branch,omitempty"` // the branch it opened on, as recorded then
	Model    string `json:"model,omitempty"`
	Version  string `json:"version,omitempty"`  // the CLI's version
	Platform string `json:"platform,omitempty"` // the channel a chat-borne session came through

	StartedAt time.Time `json:"startedAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`

	Archived bool `json:"archived,omitempty"` // put away by its CLI
	Running  bool `json:"running,omitempty"`  // the CLI itself says a turn is in flight

	// Live is where the latest turn stands, as the last rows of the transcript say: for
	// the CLIs that record enough to tell, and nil for the others.
	Live *Live `json:"live,omitempty"`

	Parent *Parent `json:"parent,omitempty"`

	// Recorded is what the CLI itself keeps at session level. Where it keeps no counters
	// per response (Hermes) these totals are all there is. Where it keeps both, the
	// totals can hold more than the responses add up to — OpenCode names a session with a
	// model call that leaves no message — and the difference is spending too.
	Recorded *Recorded `json:"recorded,omitempty"`
}

// Live is where a session's latest turn stands, as its transcript's last rows say. It is
// what the transcript says, not a look at a process: a CLI closed in the middle of a turn
// leaves it in flight for good, and a reader weighs it by how long ago the transcript was
// last written.
type Live struct {
	// State is LiveWorking, LiveWaiting or LiveIdle.
	State string `json:"state"`
	// Since is when the session came to stand so: the turn's ask, or the moment it went
	// back to work; the call waited on; the end of the turn.
	Since time.Time `json:"since,omitzero"`

	// What a waiting session waits on: the latest call with no result yet, the command it
	// runs or else the first file it names, and its step.
	Tool    string `json:"tool,omitempty"`
	Command string `json:"command,omitempty"`
	File    string `json:"file,omitempty"`
	Seq     int    `json:"seq,omitempty"`
}

// Where a turn stands.
const (
	// LiveWorking: the agent is at its turn — the model answering, or a result on its way
	// back to it.
	LiveWorking = "working"
	// LiveWaiting: a tool call it made has no result yet — a command still running, or a
	// prompt asking the person's permission to run it, which transcripts do not tell apart.
	LiveWaiting = "waiting"
	// LiveIdle: its turn is over — it answered, gave up or was stopped — and the next move
	// is the person's.
	LiveIdle = "idle"
)

// Parent ties a subagent's transcript to the session it ran in.
type Parent struct {
	Session string `json:"session"`         // the session it ran in
	Agent   string `json:"agent,omitempty"` // the agent that spawned it, when not the main thread
	Call    string `json:"call,omitempty"`  // the tool call that started it
	Run     string `json:"run,omitempty"`   // the workflow run it belongs to
	Type    string `json:"type,omitempty"`  // what kind of agent it is
	Depth   int    `json:"depth,omitempty"` // 1 for one the session spawned
	Fork    bool   `json:"fork,omitempty"`  // it opened with a copy of its parent's conversation
}

// Recorded holds session-level figures a CLI wrote down itself.
type Recorded struct {
	Usage   *Usage  `json:"usage,omitempty"`
	CostUSD float64 `json:"costUsd,omitempty"`
}

// Usage is token counts in one shape, whatever the provider called them.
//
// The parts never overlap: Input is the prompt tokens that were not served from or
// written to cache, and CacheRead and CacheWrite are those that were. Output is everything
// the model produced, reasoning included; Reasoning is the part of Output that was
// thinking, where the provider says. An adapter converts its CLI's counters to this
// shape, so a sum across CLIs adds like with like.
type Usage struct {
	Input      int64 `json:"input,omitempty"`
	Output     int64 `json:"output,omitempty"`
	CacheRead  int64 `json:"cacheRead,omitempty"`
	CacheWrite int64 `json:"cacheWrite,omitempty"`
	Reasoning  int64 `json:"reasoning,omitempty"`
}

// Add sums o into u.
func (u *Usage) Add(o Usage) {
	u.Input = sum(u.Input, o.Input)
	u.Output = sum(u.Output, o.Output)
	u.CacheRead = sum(u.CacheRead, o.CacheRead)
	u.CacheWrite = sum(u.CacheWrite, o.CacheWrite)
	u.Reasoning = sum(u.Reasoning, o.Reasoning)
}

// sum adds two counts without wrapping: a total past what an int64 holds stays at the
// most it holds, rather than turning negative.
func sum(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// MaxTokens is more than one response of any model counts, by far: a context of some
// millions of tokens is a million times smaller. A figure past it, or below zero, is a
// number gone wrong — a doctored or damaged transcript — and not a count.
const MaxTokens = 1 << 40

// Tokens is a count as an adapter read it, if it is one: the count, or 0 for a figure
// below zero or past MaxTokens. Every adapter reads its token counts through it, so that
// no transcript can make a total go below zero or wrap.
func Tokens(n int64) int64 {
	if n < 0 || n > MaxTokens {
		return 0
	}
	return n
}

// TokensOf is Tokens for a count that arrived as a JSON number, checked before it is
// converted: a float64 outside an int64's range converts to a value Go leaves to the
// platform.
func TokensOf(f float64) int64 {
	if !(f >= 0 && f <= MaxTokens) { // NaN fails both
		return 0
	}
	return int64(f)
}

// MaxDurMs is a hundred and fifteen days: longer than any model call or tool run lasts,
// and far short of what a time.Duration holds. A duration past it, or not above zero, is a
// number gone wrong, and not a duration.
const MaxDurMs = 1e10

// DurMs is a duration in milliseconds as an adapter worked it out, if it is one: the
// duration, or 0. A call's end is its start plus its duration; one past what a
// time.Duration holds would end before it began.
func DurMs(ms int64) int64 {
	if ms <= 0 || ms > MaxDurMs {
		return 0
	}
	return ms
}

// Raise lifts each counter to o's where o's is larger: the rows of one response report
// running figures, never less than a row before them.
func (u *Usage) Raise(o Usage) {
	u.Input = max(u.Input, o.Input)
	u.Output = max(u.Output, o.Output)
	u.CacheRead = max(u.CacheRead, o.CacheRead)
	u.CacheWrite = max(u.CacheWrite, o.CacheWrite)
	u.Reasoning = max(u.Reasoning, o.Reasoning)
}

// Covers reports whether every counter of u is at least o's: whether u can be a total
// that o is part of.
func (u Usage) Covers(o Usage) bool {
	return u.Input >= o.Input && u.Output >= o.Output && u.CacheRead >= o.CacheRead &&
		u.CacheWrite >= o.CacheWrite && u.Reasoning >= o.Reasoning
}

// Minus is u without o, counter by counter.
func (u Usage) Minus(o Usage) Usage {
	return Usage{Input: u.Input - o.Input, Output: u.Output - o.Output, CacheRead: u.CacheRead - o.CacheRead,
		CacheWrite: u.CacheWrite - o.CacheWrite, Reasoning: u.Reasoning - o.Reasoning}
}

// Total is every token that went in or came out.
func (u Usage) Total() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// Prompt is every token the model was shown.
func (u Usage) Prompt() int64 { return u.Input + u.CacheRead + u.CacheWrite }

// IsZero reports whether no counter is set.
func (u Usage) IsZero() bool { return u == Usage{} }

// CacheHit is the share of the prompt that was served from cache. What was written to
// cache counts against it: those tokens were sent in full.
func (u Usage) CacheHit() float64 {
	if d := u.Prompt(); d > 0 {
		return float64(u.CacheRead) / float64(d)
	}
	return 0
}

// Turn is one exchange: an ask, the work it set off, and how it ended.
//
// Index 0 is not an exchange: it holds whatever precedes the first ask.
type Turn struct {
	Index int `json:"index"`

	// StartedAt is the ask. EndedAt is the last thing the agent did — the end of the work,
	// not of the rows: a notification the CLI drops in hours later belongs to whichever
	// turn was last and must not stretch it. LastAt is that final row, kept because a
	// moment inside it still has to resolve to this turn.
	StartedAt time.Time `json:"startedAt,omitzero"`
	EndedAt   time.Time `json:"endedAt,omitzero"`
	LastAt    time.Time `json:"lastAt,omitzero"`

	Ask    string `json:"ask,omitempty"`    // the person's words, without what a harness added
	Answer string `json:"answer,omitempty"` // the last thing the agent said

	Interrupted bool `json:"interrupted,omitempty"`

	Tools     int            `json:"tools,omitempty"`     // tool calls
	Failures  int            `json:"failures,omitempty"`  // of which failed or were interrupted
	ToolKinds map[string]int `json:"toolKinds,omitempty"` // calls by what they were for
	// PassedLater is how many of the failures the same command passed later, in this turn or
	// the three after it: what the index judges of a turn read from it, and no transcript says
	PassedLater int `json:"passedLater,omitempty"`

	Files []File `json:"files,omitempty"` // every file a tool named, and whether it was changed

	Calls   []ModelCall `json:"calls,omitempty"` // the model responses, in order
	Usage   Usage       `json:"usage,omitzero"`
	CostUSD float64     `json:"costUsd,omitempty"`

	// Steps is how many items the turn holds, for a turn read without them: an outline
	// can say "31 steps" before any is fetched.
	Steps int `json:"steps,omitempty"`

	Items []Item `json:"items,omitempty"`
}

// File is a file a turn's tools named.
type File struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed,omitempty"` // a write that did not fail
}

// ModelCall is one response from a model: what a trace calls a generation.
type ModelCall struct {
	ID      string    `json:"id,omitempty"` // the provider's response id, when the CLI kept it
	Model   string    `json:"model,omitempty"`
	At      time.Time `json:"at,omitzero"`    // when its first row was written
	EndAt   time.Time `json:"endAt,omitzero"` // when its last was
	Usage   *Usage    `json:"usage,omitempty"`
	CostUSD float64   `json:"costUsd,omitempty"`
	// DurMs and TTFTMs are the call's own measured time and time to first token, for the
	// CLIs that measure them; zero means not recorded, never "instant".
	DurMs  int64  `json:"durMs,omitempty"`
	TTFTMs int64  `json:"ttftMs,omitempty"`
	Stop   string `json:"stop,omitempty"`
}

// Kind says what an Item is.
type Kind string

const (
	// Prompt is what the person typed. The one that opens a turn is its ask; one typed
	// while the agent worked is Queued and opens nothing.
	Prompt Kind = "prompt"
	// Context is text that reached the model in the user's place but was not typed by a
	// person: injected instructions, command plumbing, a notification.
	Context Kind = "context"
	// Reply is the agent's words; Thinking its reasoning, where the CLI kept it.
	Reply    Kind = "reply"
	Thinking Kind = "thinking"
	// ToolUse is a tool call together with its result.
	ToolUse Kind = "tool"
	// Event is something that happened between the words: a compaction, an interruption.
	Event Kind = "event"
)

// Item is one thing that happened in a turn, in the order it happened.
type Item struct {
	Seq  int       `json:"seq"` // position in the session, stable while the transcript only grows
	Kind Kind      `json:"kind"`
	At   time.Time `json:"at,omitzero"`
	ID   string    `json:"id,omitempty"` // the CLI's own id for the row

	Text string `json:"text,omitempty"`
	// Bytes is the size of the whole text when Text holds only a preview of it.
	Bytes int `json:"bytes,omitempty"`

	Queued    bool `json:"queued,omitempty"`    // typed while the agent was working
	Inherited bool `json:"inherited,omitempty"` // a fork's copy of its parent's row

	// Call is the 1-based index into the turn's Calls of the response this item is part
	// of; zero when no model produced it.
	Call int `json:"call,omitempty"`

	Tool  *Tool  `json:"tool,omitempty"`
	Event string `json:"event,omitempty"` // the event's kind

	// Attachments are the files the item carried — a picture pasted into a prompt, a
	// screenshot a tool returned — each marked in its text where it came.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is a file an item carried rather than said. The record keeps what it is; its
// bytes stay in the transcript, and are read from there when asked for.
type Attachment struct {
	Mime  string `json:"mime,omitempty"`  // image/png
	Bytes int    `json:"bytes,omitempty"` // its size; 0 when not known
	Name  string `json:"name,omitempty"`  // the file it named, where it named one rather than holding it
	// Inline says the transcript holds its bytes, and not only its name
	Inline bool `json:"inline,omitempty"`
}

// Tool outcomes. An empty Status is a result whose writer did not record how it ended,
// which is not the same as success.
const (
	StatusOK          = "ok"
	StatusError       = "error"
	StatusInterrupted = "interrupted"
)

// Event kinds every adapter shares; any other name is the CLI's own.
const (
	EventCompaction  = "compaction"
	EventInterrupted = "interrupted"
	EventHookError   = "hook_error"
	EventModelChange = "model_change"
	// EventColdStorage is the adapter's own word on a session part of which its CLI moved
	// where it is not read (OpenClaw's cold storage): what was moved is not in it.
	EventColdStorage = "cold_storage"
)

// Tool kinds: what a call was for.
const (
	ToolExec   = "exec"
	ToolWrite  = "write"
	ToolRead   = "read"
	ToolSearch = "search"
	ToolNet    = "net"
	ToolAgent  = "agent"
	ToolOther  = "other"
)

// Tool is a call and the result that answered it.
type Tool struct {
	CallID string `json:"callId,omitempty"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`

	Input json.RawMessage `json:"input,omitempty"`
	// InBytes is the size of the whole input when Input had its long strings cut.
	InBytes int `json:"inBytes,omitempty"`

	Command string   `json:"command,omitempty"` // the command line, for a call that ran one
	Files   []string `json:"files,omitempty"`   // the files the call names

	// Done says a result arrived. Output is that result; OutBytes its whole size when
	// Output holds only a preview.
	Done     bool   `json:"done,omitempty"`
	Output   string `json:"output,omitempty"`
	OutBytes int    `json:"outBytes,omitempty"`

	Status string    `json:"status,omitempty"`
	Exit   *int      `json:"exit,omitempty"`
	DurMs  int64     `json:"durMs,omitempty"`
	EndAt  time.Time `json:"endAt,omitzero"`

	// Run is the workflow run a call started, which is how its agents find their way back.
	Run string `json:"run,omitempty"`
}

// Failed reports whether the call ended in an error or was interrupted.
func (t *Tool) Failed() bool {
	return t != nil && (t.Status == StatusError || t.Status == StatusInterrupted)
}

// Stats is what a session adds up to.
type Stats struct {
	Turns    int     `json:"turns"`
	Items    int     `json:"items"`
	Tools    int     `json:"tools"`
	Failures int     `json:"failures"`
	Calls    int     `json:"calls"`
	Usage    Usage   `json:"usage,omitzero"`
	CostUSD  float64 `json:"costUsd,omitempty"`

	// PassedLater is how many of the failures the same command passed later, as the index
	// judges it (see Turn.PassedLater)
	PassedLater int `json:"passedLater,omitempty"`
}

// Ref names a session the way a person or an agent passes it around: "claude/3f2a9c1e-…".
// A turn of it is that with "#4" appended.
func (s *Session) Ref() string { return s.Source + "/" + s.ID }
