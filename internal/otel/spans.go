package otel

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/transcript"
	"github.com/itswl/agent-session-query/internal/txt"
)

// What a turn becomes.
//
//	turn                    the trace's root: the ask, how long the work took, how it ended
//	├─ chat <model>         one span per model response, with its tokens
//	│  ├─ execute_tool …    the calls that response made, beneath it
//	│  │  └─ turn           a subagent the call started: its own turns, in this trace
//	│  └─ execute_tool …
//	├─ chat <model>
//	└─ compaction           what happened between the words, as an event
//
// The names are the OpenTelemetry GenAI conventions', plus the few langfuse.* attributes
// Langfuse needs to show a generation as a generation. Ids are derived from what a span
// is — the store, the session, the turn, the step — so a turn sent twice is one turn.
//
// Format moves when the same turn would be sent as spans with other ids or another
// shape. It is part of what the ledger remembers, so a new format sends everything again.
const Format = 1

// Level is how much of the conversation leaves the machine.
type Level int

const (
	// LevelNone sends no text of the conversation: timings, models, token counts, tool
	// names and how each ended. A trace is named after its session and turn number.
	LevelNone Level = iota
	// LevelAsks adds what the person asked and what the agent answered, and names each
	// trace after its ask.
	LevelAsks
	// LevelAll adds everything in between: reasoning, every tool's input and output.
	LevelAll
)

// ParseLevel reads a level as the command line spells it.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none", "off":
		return LevelNone, nil
	case "asks", "ask":
		return LevelAsks, nil
	case "all", "io", "full":
		return LevelAll, nil
	}
	return LevelNone, fmt.Errorf("content is none, asks or all — not %q", s)
}

func (l Level) String() string { return [...]string{"none", "asks", "all"}[l] }

// maxBody caps one body. Past it the head and the tail are kept: a megabyte of build
// log is not what a trace is for, and collectors refuse far less.
const maxBody = 256 << 10

// place is where a session's turns sit among the traces.
type place struct {
	// trace is the trace every turn of the session joins, when it is a subagent drawn
	// beneath the call that started it; empty for a session whose turns are each a trace.
	trace string
	// parent is the span a turn's root hangs beneath: the call that started the subagent.
	parent string
	// group is the session traces are filed under: the one a subagent ran in.
	group string
}

func turnTrace(s *index.Session, turn int) string {
	return traceID(s.Source, s.ID, "turn", strconv.Itoa(turn))
}

func itemSpan(s *index.Session, seq int) string {
	return spanID(s.Source, s.ID, "item", strconv.Itoa(seq))
}

// body prepares text to leave the machine: the shapes that announce themselves as
// secrets masked, control sequences gone, and held to a size.
func body(text string) string {
	out, _ := txt.Preview(txt.Redact(txt.StripControls(text)), maxBody)
	return out
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// provider guesses who served a model from its name, for the backends that price by it.
// A name it does not know is left unsaid rather than guessed at.
func provider(name string) string {
	n := strings.ToLower(name)
	if i := strings.LastIndexByte(n, '/'); i >= 0 {
		n = n[i+1:]
	}
	switch {
	case strings.HasPrefix(n, "claude"):
		return "anthropic"
	case strings.HasPrefix(n, "gpt"), strings.HasPrefix(n, "o1"), strings.HasPrefix(n, "o3"), strings.HasPrefix(n, "o4"), strings.HasPrefix(n, "codex"), strings.HasPrefix(n, "chatgpt"):
		return "openai"
	case strings.HasPrefix(n, "gemini"):
		return "gcp.gemini"
	case strings.HasPrefix(n, "grok"):
		return "x_ai"
	case strings.HasPrefix(n, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(n, "mistral"), strings.HasPrefix(n, "codestral"):
		return "mistral_ai"
	}
	return ""
}

// usageAttrs spells a call's tokens the way the GenAI conventions count them: the input
// is everything the model was shown, cached or not, and the output everything it
// produced, reasoning included — with the cached and the reasoning parts named beside
// them. The record keeps the parts apart, the same way for every CLI, so this is one sum
// and not a table of who counts what.
func usageAttrs(u *model.Usage, cost float64) []Attr {
	var out []Attr
	if u != nil && !u.IsZero() {
		for _, pair := range []struct {
			key string
			n   int64
		}{
			{"gen_ai.usage.input_tokens", u.Prompt()},
			{"gen_ai.usage.output_tokens", u.Output},
			{"gen_ai.usage.cache_read.input_tokens", u.CacheRead},
			{"gen_ai.usage.cache_creation.input_tokens", u.CacheWrite},
			{"gen_ai.usage.reasoning.output_tokens", u.Reasoning},
		} {
			if pair.n > 0 {
				out = append(out, Attr{pair.key, pair.n})
			}
		}
	}
	// A cost the CLI worked out goes as it is; none is left for the backend to price
	if cost > 0 {
		out = append(out, Attr{"langfuse.observation.cost_details", jsonText(map[string]any{"total": cost})})
	}
	return out
}

func project(cwd string) string {
	name := filepath.Base(strings.TrimRight(filepath.ToSlash(cwd), "/"))
	if name == "." || name == "/" || name == "" {
		return ""
	}
	return name
}

// turnName names a trace. With the ask allowed out it is the ask's first line; without,
// only where the turn is.
func turnName(s *index.Session, t *model.Turn, level Level) string {
	if level >= LevelAsks {
		if line := txt.OneLine(txt.FirstLine(t.Ask), 100); line != "" {
			return line
		}
	}
	return s.Agent + " turn " + strconv.Itoa(t.Index)
}

// workEnd is when a turn's work ended, whatever was written into it afterwards.
func workEnd(t *model.Turn) time.Time {
	for _, at := range []time.Time{t.EndedAt, t.LastAt} {
		if !at.IsZero() {
			return at
		}
	}
	return t.StartedAt
}

// spansOf draws one turn.
func spansOf(s *index.Session, t *model.Turn, p place, level Level) []Span {
	trace := p.trace
	if trace == "" {
		trace = turnTrace(s, t.Index)
	}
	rootID := spanID(s.Source, s.ID, "turn", strconv.Itoa(t.Index))
	start, end := t.StartedAt, workEnd(t)
	if start.IsZero() {
		for i := range t.Items {
			if !t.Items[i].At.IsZero() {
				start = t.Items[i].At
				break
			}
		}
	}
	if start.IsZero() {
		return nil // a turn with no moment in it cannot be placed on a timeline
	}
	if end.Before(start) {
		end = start
	}

	name := turnName(s, t, level)
	agent := s.Agent
	tags := []string{s.Source}
	if proj := project(s.Cwd); proj != "" {
		tags = append(tags, "project:"+proj)
	}
	if s.Parent != nil {
		tags = append(tags, "subagent")
		if s.Parent.Type != "" {
			agent = s.Parent.Type
		}
	}
	if t.Failures > 0 {
		tags = append(tags, "tool-failure")
	}
	if t.Interrupted {
		tags = append(tags, "interrupted")
	}
	common := []Attr{
		{"langfuse.session.id", p.group},
		{"session.id", p.group},
		{"gen_ai.conversation.id", s.ID},
		{"asq.source", s.Source},
		{"asq.session", s.Ref},
		{"asq.turn", int64(t.Index)},
	}
	with := func(attrs ...Attr) []Attr { return append(append([]Attr{}, common...), attrs...) }

	root := Span{TraceID: trace, SpanID: rootID, ParentID: p.parent, Name: name, Kind: kindInternal, Start: start, End: end}
	root.Attrs = with(
		Attr{"langfuse.observation.type", "agent"},
		Attr{"langfuse.trace.name", name},
		Attr{"langfuse.trace.tags", tags},
		Attr{"gen_ai.operation.name", "invoke_agent"},
		Attr{"gen_ai.agent.name", agent},
		Attr{"asq.project", project(s.Cwd)},
		Attr{"asq.cli.version", s.Version},
		Attr{"vcs.ref.head.name", s.Branch},
		Attr{"asq.tools", int64(t.Tools)},
		Attr{"asq.failures", int64(t.Failures)},
	)
	if t.Interrupted {
		root.Attrs = append(root.Attrs, Attr{"asq.interrupted", true})
	}
	switch {
	case t.Failures > 0:
		root.Attrs = append(root.Attrs, Attr{"langfuse.observation.level", "WARNING"},
			Attr{"langfuse.observation.status_message", fmt.Sprintf("%d of %d tool calls failed", t.Failures, t.Tools)})
	case t.Interrupted:
		root.Attrs = append(root.Attrs, Attr{"langfuse.observation.level", "WARNING"},
			Attr{"langfuse.observation.status_message", "interrupted"})
	}
	if level >= LevelAsks {
		root.Attrs = append(root.Attrs,
			Attr{"asq.cwd", s.Cwd},
			Attr{"langfuse.observation.input", body(t.Ask)},
			Attr{"langfuse.observation.output", body(t.Answer)})
	}
	spans := []Span{root}

	// The model responses. A CLI writes a response's rows as they finish, so the first row
	// is not when the model was asked: that was when whatever came before it ended — the
	// ask, or the last result of the calls before. A span drawn from there covers the
	// time the model took, which is what a timeline is read for.
	callSpan := make([]string, len(t.Calls)+1)
	said := make([][]int, len(t.Calls)+1) // the items each response produced
	for i := range t.Items {
		if c := t.Items[i].Call; c > 0 && c <= len(t.Calls) && !t.Items[i].Inherited {
			said[c] = append(said[c], i)
		}
	}
	before := start // when the thing before this response ended
	var fed []int   // the tool items whose results this response was shown
	for n := 1; n <= len(t.Calls); n++ {
		c := t.Calls[n-1]
		if c.Model == transcript.SyntheticModel {
			continue // words the CLI put in the model's mouth
		}
		cEnd := c.EndAt
		if cEnd.IsZero() {
			cEnd = c.At
		}
		if cEnd.IsZero() {
			cEnd = before
		}
		cStart, timing := before, "inferred"
		if c.DurMs > 0 {
			cStart, timing = cEnd.Add(-time.Duration(c.DurMs)*time.Millisecond), "recorded"
		}
		if cStart.After(cEnd) || cStart.IsZero() {
			cStart = cEnd
		}
		id := spanID(s.Source, s.ID, "turn", strconv.Itoa(t.Index), "call", strconv.Itoa(n))
		callSpan[n] = id
		name := "chat"
		if c.Model != "" {
			name += " " + c.Model
		}
		g := Span{TraceID: trace, SpanID: id, ParentID: rootID, Name: name, Kind: kindClient, Start: cStart, End: cEnd}
		g.Attrs = with(
			Attr{"langfuse.observation.type", "generation"},
			Attr{"gen_ai.operation.name", "chat"},
			Attr{"gen_ai.provider.name", provider(c.Model)},
			Attr{"gen_ai.request.model", c.Model},
			Attr{"gen_ai.response.model", c.Model},
			Attr{"gen_ai.response.id", c.ID},
			Attr{"asq.timing", timing},
		)
		if c.Stop != "" {
			g.Attrs = append(g.Attrs, Attr{"gen_ai.response.finish_reasons", []string{c.Stop}})
		}
		if c.TTFTMs > 0 {
			g.Attrs = append(g.Attrs, Attr{"langfuse.observation.completion_start_time",
				cStart.Add(time.Duration(c.TTFTMs) * time.Millisecond).UTC().Format(time.RFC3339Nano)})
		}
		g.Attrs = append(g.Attrs, usageAttrs(c.Usage, c.CostUSD)...)
		if level >= LevelAll {
			g.Attrs = append(g.Attrs,
				Attr{"langfuse.observation.input", shown(t, n, fed)},
				Attr{"langfuse.observation.output", produced(t, said[n])})
		}
		spans = append(spans, g)

		// What the next response waits on: this one, and the calls it made
		before, fed = cEnd, fed[:0]
		for _, i := range said[n] {
			it := &t.Items[i]
			if it.Kind != model.ToolUse || it.Tool == nil {
				continue
			}
			fed = append(fed, i)
			if done := toolEnd(it); done.After(before) {
				before = done
			}
		}
	}

	for i := range t.Items {
		it := &t.Items[i]
		if it.Inherited {
			continue // a fork's copy of its parent's rows is the parent's to send
		}
		parent := rootID
		if it.Call > 0 && it.Call <= len(t.Calls) && callSpan[it.Call] != "" {
			parent = callSpan[it.Call]
		}
		at := it.At
		if at.IsZero() {
			at = start
		}
		switch {
		case it.Kind == model.ToolUse && it.Tool != nil:
			tool := it.Tool
			sp := Span{TraceID: trace, SpanID: itemSpan(s, it.Seq), ParentID: parent, Name: "execute_tool " + tool.Name,
				Kind: kindInternal, Start: at, End: toolEnd(it)}
			timing := "derived"
			if tool.EndAt.IsZero() && tool.DurMs > 0 {
				timing = "recorded"
			}
			sp.Attrs = with(
				Attr{"langfuse.observation.type", "tool"},
				Attr{"gen_ai.operation.name", "execute_tool"},
				Attr{"gen_ai.tool.name", tool.Name},
				Attr{"gen_ai.tool.call.id", tool.CallID},
				Attr{"asq.tool.kind", tool.Kind},
				Attr{"asq.tool.status", tool.Status},
				Attr{"asq.seq", int64(it.Seq)},
				Attr{"asq.timing", timing},
			)
			if tool.Exit != nil {
				sp.Attrs = append(sp.Attrs, Attr{"asq.tool.exit_code", int64(*tool.Exit)})
			}
			if tool.DurMs > 0 {
				sp.Attrs = append(sp.Attrs, Attr{"asq.tool.duration_ms", tool.DurMs})
			}
			if !tool.Done {
				sp.Attrs = append(sp.Attrs, Attr{"asq.tool.unanswered", true})
			}
			if tool.Failed() {
				sp.Error, sp.Status = true, tool.Status
				if tool.Exit != nil {
					sp.Status += ", exit " + strconv.Itoa(*tool.Exit)
				}
				sp.Attrs = append(sp.Attrs, Attr{"langfuse.observation.level", "ERROR"}, Attr{"langfuse.observation.status_message", sp.Status})
			}
			if level >= LevelAll {
				sp.Attrs = append(sp.Attrs,
					Attr{"asq.tool.command", body(tool.Command)},
					Attr{"asq.tool.files", tool.Files},
					Attr{"langfuse.observation.input", body(string(tool.Input))},
					Attr{"langfuse.observation.output", body(tool.Output)})
			}
			spans = append(spans, sp)
		case it.Kind == model.Event:
			sp := Span{TraceID: trace, SpanID: itemSpan(s, it.Seq), ParentID: rootID, Name: orElse(it.Event, "event"), Kind: kindInternal, Start: at, End: at}
			sp.Attrs = with(Attr{"langfuse.observation.type", "event"}, Attr{"asq.seq", int64(it.Seq)})
			if level >= LevelAll {
				sp.Attrs = append(sp.Attrs, Attr{"langfuse.observation.output", body(it.Text)})
			}
			spans = append(spans, sp)
		case it.Kind == model.Prompt && it.Queued:
			// Typed while the agent worked: it opened no turn, and it is on the timeline
			sp := Span{TraceID: trace, SpanID: itemSpan(s, it.Seq), ParentID: rootID, Name: "queued prompt", Kind: kindInternal, Start: at, End: at}
			sp.Attrs = with(Attr{"langfuse.observation.type", "event"}, Attr{"asq.seq", int64(it.Seq)})
			if level >= LevelAsks {
				sp.Attrs = append(sp.Attrs, Attr{"langfuse.observation.input", body(it.Text)})
			}
			spans = append(spans, sp)
		}
	}
	return spans
}

func orElse(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// toolEnd is when a call's result arrived: as written, else as measured from the call.
func toolEnd(it *model.Item) time.Time {
	switch {
	case !it.Tool.EndAt.IsZero() && !it.Tool.EndAt.Before(it.At):
		return it.Tool.EndAt
	case it.Tool.DurMs > 0 && !it.At.IsZero():
		return it.At.Add(time.Duration(it.Tool.DurMs) * time.Millisecond)
	}
	return it.At
}

// shown is what a response was given that the one before it was not: the ask, for a
// turn's first; the results of the calls the previous response made, for the rest. It is
// not the whole prompt — no transcript holds that — and does not pretend to be.
func shown(t *model.Turn, n int, fed []int) string {
	var msgs []map[string]any
	if n == 1 && t.Ask != "" {
		msgs = append(msgs, map[string]any{"role": "user", "content": body(t.Ask)})
	}
	for _, i := range fed {
		tool := t.Items[i].Tool
		if !tool.Done {
			continue
		}
		msgs = append(msgs, map[string]any{"role": "tool", "name": tool.Name, "tool_call_id": tool.CallID, "content": body(tool.Output)})
	}
	if len(msgs) == 0 {
		return ""
	}
	return jsonText(msgs)
}

// produced is what a response said: its words, its reasoning, the calls it made.
func produced(t *model.Turn, items []int) string {
	msg := map[string]any{"role": "assistant"}
	var text, thinking []string
	var calls []map[string]any
	for _, i := range items {
		it := &t.Items[i]
		switch it.Kind {
		case model.Reply:
			text = append(text, it.Text)
		case model.Thinking:
			thinking = append(thinking, it.Text)
		case model.ToolUse:
			if it.Tool != nil {
				calls = append(calls, map[string]any{"id": it.Tool.CallID, "type": "function",
					"function": map[string]any{"name": it.Tool.Name, "arguments": body(string(it.Tool.Input))}})
			}
		}
	}
	if len(text) == 0 && len(thinking) == 0 && len(calls) == 0 {
		return ""
	}
	if len(text) > 0 {
		msg["content"] = body(strings.Join(text, "\n\n"))
	}
	if len(thinking) > 0 {
		msg["reasoning"] = body(strings.Join(thinking, "\n\n"))
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	return jsonText(msg)
}
