// Package pi reads Pi's sessions.
//
//	<root>/<project>/<time>_<id>.jsonl    a session, in a directory per working directory
//	<root>/<time>_<id>.jsonl              a session, flat: what --session-dir leaves
//
// A transcript is one JSON object per line: a header (type "session": id, cwd, when it
// was created) and then entries, each with an id of its own and the time it was written.
// The conversation is the "message" entries — message.role and message.content, a block
// array — and around them pi keeps what it needs to rebuild its state: model and
// thinking-level changes, labels, extension state, the prompt and tool loadout as system
// messages. What is read here is the two sides' messages, the tools' results, what an
// extension or the person's own shell put in front of the model, and the entries that
// mark an event: a compaction, a branch left behind.
//
// Entries form a tree (id / parentId): going back to an earlier point with /tree starts
// another branch in the same file. They are read in the order they were written, every
// branch — which is also how pi totals a session: all of it was work, and all of it was
// billed.
package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/jsonmap"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Spec describes Pi.
var Spec = sources.Spec{
	Name:  "pi",
	Title: "Pi",
	Locate: func(env sources.Env) string {
		// pi's own resolution: PI_CODING_AGENT_SESSION_DIR replaces the sessions directory
		// outright — the flat --session-dir layout — and PI_CODING_AGENT_DIR moves the
		// whole agent directory off ~/.pi/agent
		if d := env.Getenv("PI_CODING_AGENT_SESSION_DIR"); d != "" {
			return d
		}
		agent := filepath.Join(env.Home, ".pi", "agent")
		if d := env.Getenv("PI_CODING_AGENT_DIR"); d != "" {
			agent = d
		}
		return filepath.Join(agent, "sessions")
	},
	Open:    func(location string) sources.Source { return &Source{root: location} },
	Movable: true,
	// --session takes an id; pi's own --resume is an interactive picker
	Resume: "pi --session",
	Store:  "~/.pi/agent/sessions/<project>/<time>_<id>.jsonl",
}

// Source is a Pi sessions directory.
type Source struct{ root string }

func (s *Source) Location() string { return s.root }
func (s *Source) Exists() bool     { return kit.Exists(s.root) }

// Scan lists every transcript. Two layouts, both pi's own: the interactive default nests
// a directory per project, and a host's --session-dir stores flat in the directory it was
// given. The two patterns are one path level apart, so no file is listed twice.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	var refs []sources.Ref
	var unread error
	for _, pattern := range []string{
		filepath.Join(s.root, "*", "*.jsonl"),
		filepath.Join(s.root, "*.jsonl"),
	} {
		// A directory that could not be read is reported beside what was found
		matches, err := kit.Glob(pattern)
		unread = errors.Join(unread, err)
		for _, path := range matches {
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue // a directory that happens to be named like a transcript
			}
			refs = append(refs, sources.Ref{Key: path, Stamp: kit.StampInfo(info), ModTime: info.ModTime()})
		}
	}
	return refs, unread
}

// row is one line of a transcript: the header, or an entry.
type row struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp any    `json:"timestamp"` // when the row was written: ISO 8601

	// The header
	Cwd           string `json:"cwd"`
	ParentSession string `json:"parentSession"` // the file a fork was made from

	// The entries that are not messages
	Name    string          `json:"name"`    // session_info
	Summary string          `json:"summary"` // compaction, branch_summary
	Content json.RawMessage `json:"content"` // custom_message
	Usage   *usage          `json:"usage"`   // compaction, branch_summary: what the summary cost

	Message message `json:"message"`
}

// message is what a "message" entry holds. Which fields are there depends on the role.
type message struct {
	Role      string          `json:"role"`
	Timestamp any             `json:"timestamp"` // when the message was created: epoch milliseconds
	Content   json.RawMessage `json:"content"`

	// assistant
	Model        string `json:"model"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`

	// assistant, toolResult
	DurationMs float64 `json:"durationMs"`
	Usage      *usage  `json:"usage"`

	// toolResult
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`

	// bashExecution
	Command        string `json:"command"`
	Output         string `json:"output"`
	ExitCode       *int   `json:"exitCode"`
	Cancelled      bool   `json:"cancelled"`
	Truncated      bool   `json:"truncated"`
	FullOutputPath string `json:"fullOutputPath"`
}

// usage is the pi-ai shape. Its parts are already apart — input excludes what was read
// from or written to cache, and pi's own total is input + output + cacheRead + cacheWrite
// (measured: it held on all 511 assistant messages of one machine) — and reasoning is
// counted inside output, so the canonical record takes the counters as they are.
type usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
	Cost       struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// canonical is the call's counters, each read by model.Tokens: one below zero or past
// what any response counts is damage and counts 0, and the rest of the call is kept.
func (u *usage) canonical() *model.Usage {
	return &model.Usage{Input: model.Tokens(u.Input), Output: model.Tokens(u.Output), CacheRead: model.Tokens(u.CacheRead),
		CacheWrite: model.Tokens(u.CacheWrite), Reasoning: model.Tokens(u.Reasoning)}
}

// costUSD is what the call cost, read by jsonmap.Cost — the rule for a price in pi-ai's
// shape wherever it is written: one below zero, or past what any response has cost, is
// damage and is none.
func (u *usage) costUSD() float64 { return jsonmap.Cost(u.Cost.Total) }

// maxDurMs is a hundred and fifteen days, longer than any model call or tool run lasts.
// A duration below zero, or past it, is not a measure but damage, and is read as none:
// added into a session's totals it would take them out of all proportion.
const maxDurMs = 1e10

// millis is a duration in milliseconds, which pi writes as a number that need not be whole.
func millis(ms float64) int64 {
	if !(ms > 0 && ms <= maxDurMs) { // NaN fails both
		return 0
	}
	return int64(ms)
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

// branchSummary is pi's own name for the entry a branch left behind is summarized in.
const branchSummary = "branch_summary"

// Read parses one transcript.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	path := ref.Key
	w.S.Path = path
	// Until a session row says otherwise, the id is the one in the file's name
	w.S.ID = idOf(strings.TrimSuffix(filepath.Base(path), ".jsonl"))

	var (
		r         row
		header    bool      // the session row has been read
		forkedAt  time.Time // when a fork was made, while the rows being read are its copy of its parent's
		inherited bool
		latest    time.Time // when the file was last written, by its own account
		name      string
	)
	add := func(m transcript.Message) {
		m.ID, m.Inherited = r.ID, inherited
		w.Add(m)
	}
	// injected adds text the CLI put in the user's seat.
	injected := func(at time.Time, text string) {
		if text != "" {
			add(transcript.Message{Role: transcript.User, At: at, Injected: true, Blocks: []transcript.Block{transcript.TextOf(text)}})
		}
	}
	// spend enters model work the transcript accounts for outside an assistant message —
	// the summary a compaction is made of, what a tool spent on models of its own — as
	// what it was: a model call that said nothing in this conversation. pi counts these
	// in a session's totals, and so they are counted here. Such a row names no model, so
	// the call reads as the model that answered last.
	spend := func(at time.Time, u *usage) {
		if u == nil || inherited {
			return
		}
		if c, cost := u.canonical(), u.costUSD(); !c.IsZero() || cost > 0 {
			add(transcript.Message{Role: transcript.Assistant, At: at, Usage: c, CostUSD: cost})
		}
	}

	n := 0
	err := kit.Lines(path, func(line []byte) bool {
		if n++; n&1023 == 0 && ctx.Err() != nil {
			return false
		}
		r = row{}
		if !decode(line, &r) {
			return true
		}
		written := kit.Time(r.Timestamp)
		if written.After(latest) {
			latest = written
		}

		if r.Type == "session" {
			if header {
				return true
			}
			header = true
			// The id is the session row's and no other's. A model_change row carries an id
			// of its own, and reading "the first line's id" reported an event's id as the
			// session's (measured: "e74f2cff" in place of the uuid in the file's name).
			// The row's version is the file format's, not pi's, and is not the session's.
			w.S.ID = kit.First(r.ID, w.S.ID)
			w.S.Cwd = r.Cwd
			w.S.StartedAt = written
			// /fork, /clone and --fork write a new file: a header with the moment of the
			// fork and the file it came from, then the parent's rows as they were — the
			// same ids, the same times, the same usage. The copy is the parent's work,
			// read in the parent's transcript; here it is inherited, so a fork's turns
			// and counters are its own. The copied rows are those written before the fork
			// was made.
			if r.ParentSession != "" {
				forkedAt = written
			}
			return true
		}
		if !forkedAt.IsZero() && !written.Before(forkedAt) {
			forkedAt = time.Time{} // the fork's own rows from here
		}
		inherited = !forkedAt.IsZero()

		switch r.Type {
		case "message":
			m := &r.Message
			// A row is written once its message is complete, and the message's own time is
			// when it was created — for a response, when it began. The two part by 3 ms
			// at the median, 17 s at p90 and nine minutes at worst (measured over 1,111
			// messages), and the message's time is the one that says when it happened.
			at := kit.Time(m.Timestamp)
			if at.IsZero() {
				at = written
			}
			switch m.Role {
			case "user":
				if text := textOf(m.Content, w.Unread); text != "" {
					add(transcript.Message{Role: transcript.User, At: at, Blocks: []transcript.Block{transcript.TextOf(text)}})
				}
			case "assistant":
				// One message is one response, timed from its creation to its last token:
				// the call ends that long after the message's time.
				out := transcript.Message{Role: transcript.Assistant, At: at, Model: m.Model, Stop: m.StopReason,
					DurMs: millis(m.DurationMs), Blocks: assistantBlocks(m, w.Unread)}
				if m.Usage != nil && !inherited {
					out.Usage, out.CostUSD = m.Usage.canonical(), m.Usage.costUSD()
				}
				add(out)
			case "toolResult":
				// A result is a message of its own: the call it answers, the tool's name and
				// whether it failed ride on the message, and the output is its content
				// (measured over the newest 40 sessions: 554 results, 49 of them failures).
				// Its time is when the tool finished, its duration how long the tool ran.
				outcome := transcript.Outcome{Status: transcript.StatusOf(m.IsError), DurMs: millis(m.DurationMs)}
				add(transcript.Message{Role: transcript.ToolRole, At: at, Blocks: []transcript.Block{
					transcript.ResultOf(m.ToolCallID, m.ToolName, textOf(m.Content, w.Unread), outcome)}})
				spend(at, m.Usage)
			case "bashExecution":
				// A command the person ran themselves ("!ls"): not a tool call of the
				// agent's, and not an ask. It reaches the model as text in the user's seat.
				injected(at, shellText(m))
			case "system":
				// The prompt and tool loadout, and later patches to them: one at the head of
				// every session, 7–11 KB of sections that are the same text in each. It is
				// what pi rebuilds its state from, not something that was said.
			case "custom", "hookMessage":
				// What an extension put in front of the model, from files older than the
				// custom_message row ("hookMessage" before the format's version 3)
				injected(at, textOf(m.Content, w.Unread))
			case "compactionSummary", "branchSummary":
				// What pi makes of a compaction or branch_summary row when it builds the
				// model's context. The row is what is read; a copy of it as a message
				// would be the same event twice.
			default:
				// A role pi had not written when this was made: a host's own, or one added
				// since. What it is is not known, and it is counted so. The text it holds,
				// if any, is read as context in the user's seat, which is where pi puts
				// every role of its own beyond the two sides', the tools' and the prompt.
				w.Unread(unknown("role", m.Role))
				injected(at, textOf(m.Content, w.Unread))
			}
		case "custom_message":
			injected(written, textOf(r.Content, w.Unread))
		case "compaction":
			// The context the model sees was rewritten here: what came before is now this
			// summary
			add(transcript.Message{Role: transcript.System, At: written,
				Blocks: []transcript.Block{transcript.EventOf(model.EventCompaction, r.Summary)}})
			spend(written, r.Usage)
		case branchSummary:
			// /tree moved to another branch, and this is what the one left behind came to
			add(transcript.Message{Role: transcript.System, At: written,
				Blocks: []transcript.Block{transcript.EventOf(branchSummary, r.Summary)}})
			spend(written, r.Usage)
		case "session_info":
			name = r.Name // the name /name gave the session; the latest stands
		case "usage":
			// A refresh of an idle prompt cache: billed like a response, and counted in
			// pi's own totals. It is left out — a model call can only be entered as an
			// assistant message, and that would stretch the turn before it to the moment
			// of the refresh.
		case "model_change":
			// Every session opens with one, and each response names the model that
			// answered it
		case "thinking_level_change":
			// A setting changed, not something said
		case "label":
			// A bookmark /tree put on an earlier entry
		case "custom":
			// An extension's own state, kept for it to rebuild from; it never reaches the
			// model
		case "context_edit":
			// A later change to what an earlier entry puts in front of the model, or its
			// removal from it: the entry as it was written is what was said
		default:
			w.Unread(unknown("row", r.Type))
		}
		return true
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.S.Title = name
	// When the session was last written is what its last row says, not the file's
	// modification time — and not the last message's own time, which for a response is
	// when it began. A fork nobody has continued was last written when it was made.
	w.S.UpdatedAt = latest
	return nil
}

// idOf is the session id a file's name carries: the name is <time>_<id>, so the part
// after the last underscore. Only a file without a session row needs it — a truncated
// one, or one whose head was lost.
func idOf(stem string) string {
	if i := strings.LastIndexByte(stem, '_'); i >= 0 && i+1 < len(stem) {
		return stem[i+1:]
	}
	return stem
}

// decode reads a record — a line, or a block of a message's content — into v. A field
// whose value is not of the type expected is left empty and the rest of the record is
// still read: one odd field does not cost a message. A value that is not a JSON object
// at all (null, a number, a string, an array) is not a record of any kind, known or
// not: it is damage, and is passed over as a line that is not JSON is.
func decode(data []byte, v any) bool {
	if data = bytes.TrimLeft(data, " \t\r\n"); len(data) == 0 || data[0] != '{' {
		return false
	}
	err := json.Unmarshal(data, v)
	var mismatch *json.UnmarshalTypeError
	return err == nil || errors.As(err, &mismatch)
}

// unknown names a record of a kind the adapter does not know, for Writer.Unread. One that
// does not say what it is — a line of JSON with no type, a message with no role — is such
// a record too.
func unknown(what, kind string) string {
	if kind == "" {
		kind = "(untyped)"
	}
	return what + ":" + kind
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
	_ = json.Unmarshal(raw, &list)
	for _, p := range list {
		if p.Type == "" {
			// A block that does not say what it is may be no block at all — null, a
			// string, a number where an object belongs — which is damage, not a kind. pi
			// types every block it writes, so only then is the content read again, block
			// by block, to keep the objects and pass over the rest.
			return objects(raw)
		}
	}
	return list
}

// objects is the blocks of a content array that are JSON objects, each read by decode.
func objects(raw json.RawMessage) []part {
	var list []json.RawMessage
	_ = json.Unmarshal(raw, &list)
	out := make([]part, 0, len(list))
	for _, el := range list {
		var p part
		if decode(el, &p) {
			out = append(out, p)
		}
	}
	return out
}

// textOf is the text of a message's content, its blocks joined. unread is told of every
// block of a type pi does not put in such content.
func textOf(raw json.RawMessage, unread func(kind string)) string {
	var texts []string
	for _, p := range parts(raw) {
		switch p.Type {
		case "text": // read below
		case "image":
			continue // a picture the person or a tool attached, which has no text
		default:
			// Its text, if it has any, is still what the block said
			unread(unknown("block", p.Type))
		}
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// assistantBlocks is what a response said and did. unread is told of every block of a
// type pi does not put in a response.
func assistantBlocks(m *message, unread func(kind string)) []transcript.Block {
	var out []transcript.Block
	for _, p := range parts(m.Content) {
		switch p.Type {
		case "thinking":
			if p.Thinking != "" { // a redacted block keeps only its signature
				out = append(out, transcript.ThinkingOf(p.Thinking))
			}
		case "toolCall":
			out = append(out, transcript.CallOf(p.ID, p.Name, p.Arguments))
		case "text":
			if p.Text != "" {
				out = append(out, transcript.TextOf(p.Text))
			}
		default:
			// Its text, if it has any, is still what the response said
			unread(unknown("block", p.Type))
			if p.Text != "" {
				out = append(out, transcript.TextOf(p.Text))
			}
		}
	}
	// How the response ended, where that is something that happened rather than something
	// said: stopped by the person (measured: such a message has no content, only
	// "Operation aborted"), or failed at the provider.
	switch m.StopReason {
	case "aborted":
		out = append(out, transcript.EventOf(model.EventInterrupted, m.ErrorMessage))
	case "error":
		out = append(out, transcript.EventOf("error", m.ErrorMessage))
	}
	return out
}

// shellText renders a command the person ran as pi hands it to the model.
func shellText(m *message) string {
	var b strings.Builder
	b.WriteString("Ran `" + m.Command + "`\n")
	if m.Output != "" {
		b.WriteString("```\n" + m.Output + "\n```")
	} else {
		b.WriteString("(no output)")
	}
	switch {
	case m.Cancelled:
		b.WriteString("\n\n(command cancelled)")
	case m.ExitCode != nil && *m.ExitCode != 0:
		fmt.Fprintf(&b, "\n\nCommand exited with code %d", *m.ExitCode)
	}
	if m.Truncated && m.FullOutputPath != "" {
		b.WriteString("\n\n[Output truncated. Full output: " + m.FullOutputPath + "]")
	}
	return b.String()
}
