package jsonmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// row is one line of a transcript — or one event of OpenClaw's database, which keeps each
// row as the JSON the jsonl held. Two shapes share it.
//
// OpenClaw writes pi's: a typed row (`session`, `message`, `model_change`, `compaction`)
// whose message sits under "message", its content a block array — `text`, `thinking`,
// `toolCall` (id, name, arguments) — and a tool's result a message of its own, role
// `toolResult`, carrying toolCallId, toolName and isError.
//
// Hermes writes the message itself, the way a chat API takes it: a role, a string of
// content, and beside it an assistant's `reasoning` and its OpenAI-shaped `tool_calls`; a
// tool's result is a row of role `tool` naming its call in `tool_call_id`.
//
// The fields a message carries are embedded for the flat shape and repeated under Message
// for the typed one. A typed row may still say some of them itself — the time, and in
// some versions the stop reason and the usage — and those are read from wherever they are.
type row struct {
	Type string `json:"type"`
	ID   any    `json:"id"`
	Cwd  string `json:"cwd"` // a session row

	Provider string `json:"provider"` // a model_change row
	ModelID  string `json:"modelId"`
	Summary  string `json:"summary"` // a compaction or branch_summary row

	Message *message `json:"message"`
	message
}

type message struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Timestamp any             `json:"timestamp"`

	Model        string         `json:"model"`
	StopReason   string         `json:"stopReason"`
	FinishReason string         `json:"finish_reason"`
	ErrorMessage string         `json:"errorMessage"`
	Usage        map[string]any `json:"usage"`

	// pi: a tool's result
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    any    `json:"isError"`
	// OpenClaw: what a result says beside its text (an exit code, a duration), and on a
	// message of its own kind, what kind and the whole of what it records
	CustomType string          `json:"customType"`
	Details    json.RawMessage `json:"details"`

	// Hermes: an assistant's reasoning and calls, and a tool's result
	Reasoning string          `json:"reasoning"`
	ToolCalls json.RawMessage `json:"tool_calls"`
	CallID    string          `json:"tool_call_id"`
	Tool      string          `json:"tool_name"`
	Name      string          `json:"name"`

	Platform string `json:"platform"` // Hermes's session_meta row
}

// part is one block of a typed message's content.
type part struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`

	// a result written as a block of the message it came back in
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Content    json.RawMessage `json:"content"`
	IsError    *bool           `json:"isError"`
}

// Reader turns a transcript's rows into messages, and keeps what the rows say about the
// session itself.
type Reader struct {
	// Model is the model an assistant message is given when its row names none: Hermes
	// records the model once, for the session, and not on any message.
	Model string

	SessionID string // the id on the session row
	Cwd       string // the directory on the session row
	Platform  string // the channel Hermes's session_meta row names

	// Unread is told of every row, message and block passed over without knowing what it
	// was, named the way transcript.Writer.Unread takes it. Nil when no one asks.
	Unread func(kind string)

	// spent is what a row cost beyond the message it was read as: a tool that called a
	// model of its own reports those tokens on its result. Take hands it over.
	spent *transcript.Message
}

// note tells Unread, if anyone listens, of a record of a kind the reader does not know.
func (r *Reader) note(kind string) {
	if r.Unread != nil {
		r.Unread(kind)
	}
}

// unknown names a record of a kind the reader does not know, for Writer.Unread. One that
// does not say what it is — a line of JSON with no type, a block with no type — is such a
// record too.
func unknown(what, kind string) string {
	if kind == "" {
		kind = "(untyped)"
	}
	return what + ":" + kind
}

// Take returns the message the last Row left behind, if it left one: tokens a tool's
// result says the tool itself spent, as a response with no words.
func (r *Reader) Take() (transcript.Message, bool) {
	if r.spent == nil {
		return transcript.Message{}, false
	}
	m := *r.spent
	r.spent = nil
	return m, true
}

// ReadFile adds every message of a transcript file to w.
func (r *Reader) ReadFile(ctx context.Context, path string, w *transcript.Writer) error {
	n := 0
	err := kit.Lines(path, func(line []byte) bool {
		if n++; n&1023 == 0 && ctx.Err() != nil {
			return false
		}
		if m, ok := r.Row(line); ok {
			w.Add(m)
		}
		if m, ok := r.Take(); ok {
			w.Add(m)
		}
		return true
	})
	if err != nil {
		return err
	}
	return ctx.Err()
}

// Row reads one row. The second result is false for a row that adds no message: one that
// only describes the session, bookkeeping, or a line that is not JSON.
func (r *Reader) Row(line []byte) (transcript.Message, bool) {
	var x row
	if !decode(line, &x) {
		return transcript.Message{}, false
	}
	switch x.Type {
	case "message":
		if x.Message != nil {
			return r.typed(&x)
		}
		// One that holds its message beside the type rather than under it — a role and a
		// content, the flat shape — is read as that shape is; one that holds no message
		// at all names no role, and flat counts it as such rather than let it vanish
		return r.flat(&x)
	case "session":
		// The header, wherever it turns up — and only the first: other rows carry an id
		// of their own, which is theirs and not the session's
		if r.SessionID == "" && r.Cwd == "" {
			r.SessionID, r.Cwd = kit.Text(x.ID), x.Cwd
		}
		return transcript.Message{}, false
	case "model_change":
		if x.ModelID == "" {
			return transcript.Message{}, false
		}
		name := x.ModelID
		if x.Provider != "" {
			name = x.Provider + "/" + name
		}
		return r.event(&x, model.EventModelChange, name), true
	case "compaction":
		// The context the model sees was rewritten here; the row carries the summary that
		// took the place of what was folded away
		return r.event(&x, model.EventCompaction, kit.First(x.Summary, "Context compacted")), true
	case "branch_summary":
		// The conversation moved to another branch, and this is what the one left behind
		// came to — read as pi reads it
		return r.event(&x, x.Type, x.Summary), true
	case "custom":
		// An extension's own state, kept for it to rebuild from (OpenClaw's model
		// snapshot); it never reaches the model
	case "custom_message":
		// What an extension put in front of the model — OpenClaw's runtime context — and
		// nobody typed or was shown
	case "thinking_level_change":
		// A setting changed, not something said
	case "label":
		// A bookmark put on an earlier row
	case "session_info":
		// The name given to the session; OpenClaw keeps its own beside the transcript
	case "context_edit":
		// A later change to what an earlier row puts in front of the model, or its removal
		// from it: the row as it was written is what was said
	case "":
		// Hermes's rows have no type: each is the message itself
		return r.flat(&x)
	default:
		r.note(unknown("row", x.Type))
	}
	return transcript.Message{}, false
}

func (r *Reader) event(x *row, kind, text string) transcript.Message {
	return transcript.Message{ID: kit.Text(x.ID), Role: transcript.System, At: kit.Time(x.Timestamp),
		Blocks: []transcript.Block{transcript.EventOf(kind, text)}}
}

// typed reads pi's message row.
func (r *Reader) typed(x *row) (transcript.Message, bool) {
	msg := x.Message
	// Two times: the message's own, in epoch milliseconds, is when it happened; the row's,
	// ISO text, is when it was appended — which for rows flushed together is the same
	// moment for all of them, seconds or minutes after the first. The message's is read,
	// and the row's stands in where there is none.
	at := kit.Time(msg.Timestamp)
	if at.IsZero() {
		at = kit.Time(x.Timestamp)
	}
	m := transcript.Message{ID: kit.Text(x.ID), At: at}
	switch msg.Role {
	case "user":
		m.Role = transcript.User
		m.Blocks = wordBlocks(msg.Content, r.note)
		return m, len(m.Blocks) > 0
	case "assistant":
		m.Role = transcript.Assistant
		m.Model = kit.First(msg.Model, r.Model)
		// The stop reason sits in the message or on the row, depending on the version
		// that wrote it; so does the usage
		m.Stop = kit.First(msg.StopReason, x.StopReason, msg.FinishReason, x.FinishReason)
		usage := msg.Usage
		if len(usage) == 0 {
			usage = x.Usage
		}
		u, cost := Usage(usage)
		m.CostUSD = cost
		if !u.IsZero() {
			m.Usage = &u
		}
		m.Blocks = assistantBlocks(msg.Content, r.note)
		// How a response ended, where that is something that happened rather than
		// something said: stopped by the person, or failed at the provider — a 429, a
		// dropped connection — with errorMessage carrying the reason either way
		switch m.Stop {
		case "aborted":
			m.Blocks = append(m.Blocks, transcript.EventOf(model.EventInterrupted, kit.First(msg.ErrorMessage, x.ErrorMessage)))
		case "error":
			m.Blocks = append(m.Blocks, transcript.EventOf("error", kit.First(msg.ErrorMessage, x.ErrorMessage)))
		}
		// A message with no words can still be a response that cost tokens: it is added
		// whatever it holds
		return m, true
	case "toolResult":
		m.Role = transcript.ToolRole
		// isError is what every version records about how a tool went; OpenClaw adds, for
		// a command, how it exited and how long it ran
		var details map[string]any
		_ = json.Unmarshal(msg.Details, &details)
		m.Blocks = []transcript.Block{transcript.ResultOf(msg.ToolCallID, msg.ToolName, partsText(msg.Content, r.note), outcomeOf(kit.Truthy(msg.IsError), details))}
		// A tool that called a model of its own says what that cost on its result
		if u, cost := Usage(msg.Usage); !u.IsZero() || cost > 0 {
			r.spent = &transcript.Message{Role: transcript.Assistant, At: at, Model: kit.First(msg.Model, r.Model), Usage: &u, CostUSD: cost}
		}
		return m, true
	case "custom":
		if msg.CustomType == nestedToolType {
			return nestedTool(m, msg, r.note)
		}
		// Any other is what an extension of the CLI's put in front of the model, read
		// below as the CLI's own words
	default:
		// A role the CLI had not written when this was made. What it is is not known, and
		// it is counted so; the words it holds, if any, are still read as the CLI's own.
		r.note(unknown("role", msg.Role))
	}
	m.Role = transcript.System
	m.Blocks = wordBlocks(msg.Content, r.note)
	return m, len(m.Blocks) > 0
}

// nestedToolType marks the row OpenClaw writes for a tool run from inside another: its
// agents call one tool, `exec`, with a program, and the program calls the real ones —
// read this file, run that command. The model is shown only what the program returned;
// what was read and run, how each ended and how long it took is in these rows, and
// nowhere else in the transcript.
const nestedToolType = "openclaw.nested-tool.v1"

// nestedTool reads such a row as what it records: a call, and the result that answered it.
// unread is told of every block of its result of a type a result does not hold.
func nestedTool(m transcript.Message, msg *message, unread func(kind string)) (transcript.Message, bool) {
	var d struct {
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"toolName"`
		Input      json.RawMessage `json:"input"`
		Result     struct {
			Content json.RawMessage `json:"content"`
			Details map[string]any  `json:"details"`
		} `json:"result"`
		IsError   any `json:"isError"`
		StartedAt any `json:"startedAt"`
		Timestamp any `json:"timestamp"`
	}
	if json.Unmarshal(msg.Details, &d) != nil || d.ToolName == "" {
		return m, false
	}
	outcome := outcomeOf(kit.Truthy(d.IsError), d.Result.Details)
	started, ended := kit.Time(d.StartedAt), kit.Time(d.Timestamp)
	if !started.IsZero() {
		m.At = started
		if outcome.DurMs == 0 && ended.After(started) {
			outcome.DurMs = model.DurMs(ended.Sub(started).Milliseconds())
		}
	}
	output := partsText(d.Result.Content, unread)
	if output == "" {
		output = kit.First(kit.Str(d.Result.Details["aggregated"]), kit.Str(d.Result.Details["content"]))
	}
	var input any
	if len(d.Input) > 0 {
		input = d.Input
	}
	m.Role = transcript.ToolRole
	m.Blocks = []transcript.Block{
		transcript.CallOf(d.ToolCallID, d.ToolName, input),
		transcript.ResultOf(d.ToolCallID, d.ToolName, output, outcome),
	}
	return m, true
}

// outcomeOf reads how a tool went from its error flag and from what its result details
// say: a command that exited non-zero failed, whether or not anything flagged it.
//
// A figure no exit code or duration can be is not read: Go leaves the conversion of a
// float64 past an integer's range to the platform, so 1e300 would read as one code on
// arm64 and another on amd64 — and as a negative duration on the second. An exit code is
// a 32-bit figure, signed where a process returns one and unsigned on Windows, whose
// codes for a crash run past 2^31.
func outcomeOf(failed bool, details map[string]any) transcript.Outcome {
	o := transcript.Outcome{}
	if code, ok := kit.Num(details["exitCode"]); ok && code >= math.MinInt32 && code <= math.MaxUint32 {
		o.Exit = transcript.ExitOf(int(code))
		failed = failed || code != 0
	}
	if ms, ok := kit.Num(details["durationMs"]); ok && ms > 0 && ms <= maxDurMs {
		o.DurMs = int64(ms)
	}
	o.Status = transcript.StatusOf(failed)
	switch strings.ToLower(kit.Str(details["status"])) {
	case "running", "started", "backgrounded", "background", "pending", "yielded":
		if !failed {
			o.Status = "" // it runs on: how it ends is not recorded, and is no success
		}
	}
	return o
}

// maxDurMs is a hundred and fifteen days: longer than any call runs.
const maxDurMs = 1e10

// flat reads Hermes's row: the message itself. Only the two sides' rows and the tools'
// are conversation; the system prompt and the session's own bookkeeping are not.
func (r *Reader) flat(x *row) (transcript.Message, bool) {
	m := transcript.Message{ID: kit.Text(x.ID), At: kit.Time(x.Timestamp)}
	switch x.Role {
	case "session_meta":
		// Where Hermes notes the model and the channel a session opened with
		r.Platform = kit.First(r.Platform, x.Platform)
		r.Model = kit.First(r.Model, x.Model)
	case "user":
		m.Role = transcript.User
		if text := contentText(x.Content); text != "" {
			m.Blocks = []transcript.Block{transcript.TextOf(text)}
		}
		return m, len(m.Blocks) > 0
	case "assistant":
		m.Role = transcript.Assistant
		m.Model = kit.First(x.Model, r.Model)
		m.Stop = kit.First(x.FinishReason, x.StopReason)
		u, cost := Usage(x.Usage)
		m.CostUSD = cost
		if !u.IsZero() {
			m.Usage = &u
		}
		m.Blocks = Assistant(x.Reasoning, contentText(x.Content), x.ToolCalls)
		return m, len(m.Blocks) > 0 || m.Usage != nil
	case "tool":
		m.Role = transcript.ToolRole
		m.Blocks = []transcript.Block{Result(x.CallID, kit.First(x.Tool, x.Name), contentText(x.Content))}
		return m, true
	case "system":
		// The system prompt: what the model was told to be, not something said
	default:
		r.note(unknown("role", x.Role))
	}
	return m, false
}

// Assistant is the blocks of a Hermes assistant message: what it thought, what it said,
// and the calls it made — in that order, which is the order they happened in. The
// message holds them as three fields, not as a sequence.
func Assistant(reasoning, text string, toolCalls []byte) []transcript.Block {
	var out []transcript.Block
	if reasoning != "" {
		out = append(out, transcript.ThinkingOf(reasoning))
	}
	if text != "" {
		out = append(out, transcript.TextOf(text))
	}
	return append(out, Calls(toolCalls)...)
}

// Result is a Hermes tool row as a result block: the output, the call it answers and the
// tool that ran. Hermes does not record how a call ended, so the block carries no status
// — which is "not recorded", not "fine".
func Result(callID, tool, output string) transcript.Block {
	return transcript.ResultOf(callID, tool, output, transcript.Outcome{})
}

// Calls reads OpenAI-shaped tool calls — [{id, function: {name, arguments}}], the
// arguments a string holding JSON — as call blocks. raw is the array, or a string that
// holds one.
func Calls(raw []byte) []transcript.Block {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return nil
		}
		raw = []byte(text)
	}
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var calls []struct {
		ID       string `json:"id"`
		CallID   string `json:"call_id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	_ = json.Unmarshal(raw, &calls) // what decoded before an oddly typed call stays
	var out []transcript.Block
	for _, call := range calls {
		var input any
		if args := bytes.TrimSpace(call.Function.Arguments); len(args) > 0 && !bytes.Equal(args, []byte("null")) {
			input = json.RawMessage(args)
			var text string
			if args[0] == '"' && json.Unmarshal(args, &text) == nil {
				input = text // the JSON the string holds is the arguments
			}
		}
		// The result names its call by id; call_id says the same in the versions that
		// write both (measured: equal on every one of 3 279 calls)
		out = append(out, transcript.CallOf(kit.First(call.ID, call.CallID), call.Function.Name, input))
	}
	return out
}

// Answered reports whether a transcript file holds an answer: an assistant row that
// stopped because it was done.
func Answered(path string) bool {
	found := false
	_ = kit.Lines(path, func(line []byte) bool {
		if !bytes.Contains(line, []byte("assistant")) {
			return true
		}
		var x row
		if !decode(line, &x) {
			return true
		}
		msg := &x.message
		stop := kit.First(x.FinishReason, x.StopReason)
		if x.Type == "message" && x.Message != nil {
			msg = x.Message
			stop = kit.First(msg.StopReason, x.StopReason, msg.FinishReason, x.FinishReason)
		}
		found = msg.Role == "assistant" && stop == "stop"
		return !found
	})
	return found
}

// Usage converts the counters a message carries to the canonical shape, and reads the
// cost beside them. The shape is pi-ai's, which OpenClaw records: input + output +
// cacheRead + cacheWrite = totalTokens, so the input already leaves the cache out and
// nothing is subtracted; the output includes the reasoning. The cost is `cost.total`,
// the figure the response was priced at.
//
// Keys are folded to lowercase without underscores before they are matched, so input,
// inputTokens and input_tokens are one counter. A prompt or completion count is not
// read: those are the other shape, whose prompt count holds the cache, and taken as
// apart they would count it twice.
//
// Each counter is a count by model.TokensOf's rule (kit.Count), and the cost a price by
// Cost's. Two keys that fold to one counter — input beside input_tokens — give the larger:
// the map is walked in no fixed order, and the same row must read the same every time.
func Usage(raw map[string]any) (u model.Usage, cost float64) {
	for key, v := range raw {
		switch strings.ReplaceAll(strings.ToLower(key), "_", "") {
		case "input", "inputtokens":
			u.Input = max(u.Input, kit.Count(v))
		case "output", "outputtokens":
			u.Output = max(u.Output, kit.Count(v))
		case "cacheread", "cachereadtokens", "cachereadinputtokens":
			u.CacheRead = max(u.CacheRead, kit.Count(v))
		case "cachewrite", "cachewritetokens", "cachecreationinputtokens":
			u.CacheWrite = max(u.CacheWrite, kit.Count(v))
		case "reasoning", "reasoningtokens":
			u.Reasoning = max(u.Reasoning, kit.Count(v))
		case "cost":
			// {"cost": {"total": 0.0021, …}}; a writer that gives the number alone is read too
			if nested, ok := v.(map[string]any); ok {
				v = nested["total"]
			}
			cost = max(cost, Cost(v))
		}
	}
	return u, cost
}

// maxCost is more than any response, or any session, has cost by far: a price past it is
// a number gone wrong — a doctored or damaged record — and not a price.
const maxCost = 1e9

// Cost reads a price in dollars, from a response or from a session's totals. One that is
// not a number, is below zero, is not finite, or is past maxCost is not a price, and is
// read as none: added to others it would take a total below zero or to infinity.
func Cost(v any) float64 {
	if f, ok := kit.Num(v); ok && f > 0 && f <= maxCost { // NaN fails both
		return f
	}
	return 0
}

// decode reads one line into v. A field of the wrong type is left empty and the rest of
// the row still counts: a transcript is another program's JSON, and one oddly typed
// field must not lose the message around it. Only a line that is not JSON is skipped.
func decode(line []byte, v any) bool {
	err := json.Unmarshal(line, v)
	var typeErr *json.UnmarshalTypeError
	return err == nil || errors.As(err, &typeErr)
}

// parts decodes a typed message's content: a bare string is one text block.
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

// partText is the text a block carries, whatever the block calls itself.
func partText(p part) string {
	if p.Text != "" {
		return p.Text
	}
	return contentText(p.Content)
}

// partsText is the text of a content array, one block per line. An image has none and
// adds nothing. unread is told of every block of a type such content does not hold.
func partsText(raw json.RawMessage, unread func(kind string)) string {
	var texts []string
	for _, p := range parts(raw) {
		switch p.Type {
		case "text": // read below
		case "image":
			continue // a picture a tool returned, which has no text
		default:
			// Its text, if it has any, is still what the block said
			unread(unknown("block", p.Type))
		}
		if text := partText(p); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// resultBlock is a result written as a block of a message's content, the shape older
// transcripts used. Its status is written only when the block says one.
func resultBlock(p part, unread func(kind string)) transcript.Block {
	var outcome transcript.Outcome
	if p.IsError != nil {
		outcome.Status = transcript.StatusOf(*p.IsError)
	}
	return transcript.ResultOf(p.ToolCallID, p.ToolName, partsText(p.Content, unread), outcome)
}

// wordBlocks is a message's words: the blocks that carry text, and any result among them.
// unread is told of every block of a type such a message does not hold.
func wordBlocks(raw json.RawMessage, unread func(kind string)) []transcript.Block {
	var out []transcript.Block
	for _, p := range parts(raw) {
		switch p.Type {
		case "toolResult":
			out = append(out, resultBlock(p, unread))
			continue
		case "text": // read below
		case "image":
			continue // a picture attached to the message, which has no words
		default:
			// Its text, if it has any, is still what the message said
			unread(unknown("block", p.Type))
		}
		if text := partText(p); text != "" {
			out = append(out, transcript.TextOf(text))
		}
	}
	return out
}

// assistantBlocks is what a response said and did. unread is told of every block of a
// type a response does not hold.
func assistantBlocks(raw json.RawMessage, unread func(kind string)) []transcript.Block {
	var out []transcript.Block
	for _, p := range parts(raw) {
		switch p.Type {
		case "thinking":
			if p.Thinking != "" { // a redacted block keeps only its signature
				out = append(out, transcript.ThinkingOf(p.Thinking))
			}
		case "toolCall":
			var args any
			if a := bytes.TrimSpace(p.Arguments); len(a) > 0 && !bytes.Equal(a, []byte("null")) {
				args = json.RawMessage(a)
			}
			out = append(out, transcript.CallOf(p.ID, p.Name, args))
		case "toolResult":
			out = append(out, resultBlock(p, unread))
		case "text":
			if text := partText(p); text != "" {
				out = append(out, transcript.TextOf(text))
			}
		default:
			// Its text, if it has any, is still what the response said
			unread(unknown("block", p.Type))
			if text := partText(p); text != "" {
				out = append(out, transcript.TextOf(text))
			}
		}
	}
	return out
}

// contentText flattens a flat message's content: a string, or the block array a chat API
// also takes.
func contentText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return kit.ContentText(v)
}
