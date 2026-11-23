// Package transcript turns what an adapter reads into the canonical record.
//
// An adapter knows one CLI's files and nothing about turns. It hands over messages in
// the order they were written — who spoke, when, and a list of blocks — and the Writer
// does the rest once, for every CLI alike: it decides which user message is a person's
// ask, pairs each tool result with its call, groups the rows of one model response, and
// derives what a turn adds up to (the files it changed, what failed, what it cost).
package transcript

import (
	"time"

	"github.com/itswl/agent-session-query/internal/model"
)

// Role is who a message is from.
type Role int

const (
	User Role = iota
	Assistant
	System   // the CLI itself: events between the turns
	ToolRole // a tool's result, for the CLIs that write it as a message of its own
)

// Message is one row of a transcript as an adapter read it.
type Message struct {
	ID   string
	Role Role
	At   time.Time

	// What the CLI recorded about the model response an assistant message is part of.
	// Response groups the rows of one response when the CLI splits it (Claude Code writes
	// a row per content block); left empty, the message is a response of its own.
	Response string
	Model    string
	Usage    *model.Usage
	CostUSD  float64
	DurMs    int64 // the call's measured duration, when the CLI measures it
	TTFTMs   int64 // time to the first token, likewise
	Stop     string

	// What an adapter knows about a user message that its text does not say.
	Queued    bool // typed while the agent was working: it opens no turn
	Injected  bool // assembled by the CLI, not typed by a person
	Inherited bool // a fork's copy of its parent's row: the parent's, not this session's

	// What an adapter knows of where the agent's turn stands, for a CLI that records it
	// (see Writer.Tells). Ends says the CLI marked the turn over here: the agent answered,
	// gave up or was stopped, and the next move is the person's. A message that holds
	// nothing but the mark adds nothing to the turn. Unfinished, on an assistant message,
	// says its response is not over: the CLI writes it as it streams, and more is to come.
	Ends       bool
	Unfinished bool

	// Dir is the directory the message's calls ran in, for a CLI whose tools take a path
	// relative to it: a file a call names that way is named by its whole path. Empty, a
	// file is named as the call named it.
	Dir string

	Blocks []Block
}

// BlockType says what a Block is.
type BlockType int

const (
	Text BlockType = iota
	Thinking
	Call
	Result
	EventBlock
)

// Block is one piece of a message.
type Block struct {
	Type BlockType
	Text string // the words, the reasoning, a result's output, an event's text

	// Call and Result
	CallID string
	Name   string // the tool; for an event, its kind
	Input  any    // a call's arguments: a JSON object, json.RawMessage, or a string
	Outcome
	Run string // the workflow run a call's result names

	// Stops, on an event, says the turn ended there before the agent was done: an error
	// that cut the work off. The turn is marked interrupted, as one the person stopped is.
	Stops bool
	// After, on an event the CLI wrote of its own, says it is not where the turn's work
	// got to — something the person did between turns, an error the work went on past or
	// that came once it was done — and does not move the turn's end.
	After bool
}

// Outcome is what a CLI recorded about how a tool call ended. Zero values mean "not
// recorded" — a result without a status is not a success.
type Outcome struct {
	Status string // model.StatusOK / StatusError / StatusInterrupted
	Exit   *int
	DurMs  int64
}

// TextOf builds a text block.
func TextOf(s string) Block { return Block{Type: Text, Text: s} }

// ThinkingOf builds a thinking block.
func ThinkingOf(s string) Block { return Block{Type: Thinking, Text: s} }

// CallOf builds a tool-call block. id may be empty when the CLI gives calls none.
func CallOf(id, name string, input any) Block {
	return Block{Type: Call, CallID: id, Name: name, Input: input}
}

// ResultOf builds a tool-result block answering the call with that id. name is only
// needed when the call itself may not be in the transcript.
func ResultOf(callID, name, output string, o Outcome) Block {
	return Block{Type: Result, CallID: callID, Name: name, Text: output, Outcome: o}
}

// EventOf builds an event block: something that happened between the words.
func EventOf(kind, text string) Block { return Block{Type: EventBlock, Name: kind, Text: text} }

// StatusOf folds the one bit most CLIs record — did it fail — into a status.
func StatusOf(failed bool) string {
	if failed {
		return model.StatusError
	}
	return model.StatusOK
}

// ExitOf is an exit code as an Outcome carries it.
func ExitOf(code int) *int { return &code }
