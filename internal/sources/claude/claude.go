// Package claude reads Claude Code's sessions.
//
//	<root>/<project>/<session>.jsonl                          a session
//	<root>/<project>/<session>/subagents/agent-<id>.jsonl     a subagent's transcript,
//	<root>/<project>/<session>/subagents/agent-<id>.meta.json   and what it was
//	<root>/<project>/<session>/subagents/workflows/<run>/…    a workflow's agents
//	<root>/<project>/<session>/tool-results/<file>            outputs too large for a row
//
// A transcript is one JSON object per line. Most lines are not conversation: of the rows
// on one machine, attachments (hook output, reminders) outnumbered user and assistant
// rows, beside titles, modes, snapshots and queue operations. What is read here is what
// a person would recognise as the session: the two sides' rows, the system rows that
// mark an event, a prompt typed while the agent was busy, and the session's titles.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/jsonmap"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Spec describes Claude Code.
var Spec = sources.Spec{
	Name:  "claude",
	Title: "Claude Code",
	Locate: func(env sources.Env) string {
		// CLAUDE_CONFIG_DIR moves the whole configuration directory off ~/.claude
		if d := env.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
			return filepath.Join(d, "projects")
		}
		return filepath.Join(env.Home, ".claude", "projects")
	},
	Open:       func(location string) sources.Source { return &Source{root: location} },
	Movable:    true,
	Resume:     "claude --resume",
	ResumeHere: true,
	Reads:      4,
	Store:      "~/.claude/projects/<project>/<session>.jsonl",
	// Claude Code sweeps every project after a session starts; from v2.1.248 it keeps the
	// sessions started or last continued in Claude Desktop or Cowork, unless
	// desktopSessionCleanupPeriodDays or a managed cleanupPeriodDays limits them: "Cleaned
	// up automatically" in code.claude.com/docs/en/claude-directory
	Retention: "Claude Code deletes the transcripts of sessions not used for 30 days by default (cleanupPeriodDays in its settings.json, under ~/.claude or CLAUDE_CONFIG_DIR)",
}

// Source is a Claude Code projects directory.
type Source struct{ root string }

func (s *Source) Location() string { return s.root }
func (s *Source) Exists() bool     { return kit.Exists(s.root) }

// Scan lists every transcript: sessions, and the subagents that ran inside them.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	var refs []sources.Ref
	var unread error
	for _, pattern := range []string{
		filepath.Join(s.root, "*", "*.jsonl"),
		filepath.Join(s.root, "*", "*", "subagents", "agent-*.jsonl"),
		filepath.Join(s.root, "*", "*", "subagents", "workflows", "*", "agent-*.jsonl"),
	} {
		// A directory that could not be read is reported beside what was found: its
		// sessions are missing from this list, not from the store
		matches, err := kit.Glob(pattern)
		unread = errors.Join(unread, err)
		for _, path := range matches {
			paths := []string{path}
			if isSubagent(path) {
				paths = append(paths, metaPath(path)) // the meta file can be written after
			}
			stamp, mod := kit.Stamp(paths...)
			refs = append(refs, sources.Ref{Key: path, Stamp: stamp, ModTime: mod})
		}
	}
	return refs, unread
}

// isSubagent reports whether a transcript is a subagent's: agent-<id>.jsonl somewhere
// under a session's subagents directory. Every row of one is marked isSidechain, which in
// a session's own transcript means "not this thread" and is skipped; here it is the thread.
func isSubagent(path string) bool {
	return strings.HasPrefix(filepath.Base(path), "agent-") &&
		strings.Contains(filepath.ToSlash(path), "/subagents/")
}

func metaPath(path string) string { return strings.TrimSuffix(path, ".jsonl") + ".meta.json" }

// Files is what a transcript is made of (sources.Keeper): a subagent's, and the meta file
// beside it; a session's, and the outputs saved beside it, which its subagents' rows name
// too. No other session reads any of them.
func (s *Source) Files(ref sources.Ref) (own, shared []string) {
	if isSubagent(ref.Key) {
		return []string{ref.Key, metaPath(ref.Key)}, nil
	}
	own = []string{ref.Key}
	saved := filepath.Join(strings.TrimSuffix(ref.Key, ".jsonl"), "tool-results")
	entries, _ := os.ReadDir(saved)
	for _, e := range entries {
		if e.Type().IsRegular() {
			own = append(own, filepath.Join(saved, e.Name()))
		}
	}
	return own, nil
}

// row is the part of a transcript line that decides what it is. The rest — the content —
// is decoded only for the rows that are read.
type row struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	UUID        string          `json:"uuid"`
	Timestamp   string          `json:"timestamp"`
	SessionID   string          `json:"sessionId"`
	AgentID     string          `json:"agentId"`
	Cwd         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	Version     string          `json:"version"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	IsCompact   bool            `json:"isCompactSummary"`
	Content     json.RawMessage `json:"content"` // a system row's text

	AITitle     string `json:"aiTitle"`
	CustomTitle string `json:"customTitle"`

	// Figures are read as whatever JSON holds (see jsonmap.Cost, kit.Count): a count
	// written as 1e19 or 12.0, or a price as a string, is no figure, and does not cost the
	// row it is in — a reply and its tool calls with it
	TotalCostUSD any `json:"totalCostUSD"`

	HookErrors []json.RawMessage `json:"hookErrors"`

	// A system row saying another model took over: which, from which
	OriginalModel string `json:"originalModel"`
	FallbackModel string `json:"fallbackModel"`

	Message struct {
		ID         string          `json:"id"`
		Role       string          `json:"role"`
		Model      string          `json:"model"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
		CostUSD    any             `json:"costUSD"`
		Usage      *struct {
			Input      any `json:"input_tokens"`
			Output     any `json:"output_tokens"`
			CacheRead  any `json:"cache_read_input_tokens"`
			CacheWrite any `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`

	ToolUseResult json.RawMessage `json:"toolUseResult"`
	// ToolDenialKind is why a call did not run as asked, where the CLI says: see stopped
	ToolDenialKind string `json:"toolDenialKind"`

	Attachment struct {
		Type        string          `json:"type"`
		CommandMode string          `json:"commandMode"`
		IsMeta      bool            `json:"isMeta"`
		Prompt      json.RawMessage `json:"prompt"`
	} `json:"attachment"`
}

// maxDurMs is a hundred and fifteen days: longer than any call runs, and far short of the
// 292 years past which a duration in milliseconds overflows a time.Duration.
const maxDurMs = 1e10

// millis is a call's duration in milliseconds as a row gives it, or 0 when it is not one:
// a damaged or doctored row can say anything, and a figure below zero, or past maxDurMs,
// is no duration. Go leaves the conversion of a float64 past an int64's range to the
// platform, so the figure is checked before it is converted.
func millis(ms float64) int64 {
	if !(ms > 0 && ms <= maxDurMs) {
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
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *fileSource     `json:"source"` // an image's or a document's
}

// fileSource is where an image or a document block has its file: in the block, as base64
// (or, for a document, as text), or at an address it names.
type fileSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
	FileID    string `json:"file_id"`
}

// fileOf is the file an image or a document block carried, marked as what it is.
func fileOf(kind string, src *fileSource) transcript.Attachment {
	mime := ""
	if kind == "image" {
		mime = "image/*" // the bytes, if there are any, say which
	}
	if src == nil {
		return transcript.Embedded(mime, "")
	}
	mime = kit.First(src.MediaType, mime)
	switch src.Type {
	case "base64":
		return transcript.Embedded(mime, src.Data)
	case "text":
		// A document given as plain text: its words are the model's to read, and not the
		// person's; it is marked, and not shown
		a := transcript.Embedded(kit.First(src.MediaType, "text/plain"), "")
		a.Size = len(src.Data)
		return a
	case "url":
		return transcript.Referenced(mime, src.URL)
	case "file":
		return transcript.Referenced(mime, src.FileID)
	}
	return transcript.Embedded(mime, "")
}

// useResult is the row-level record of how a tool call went.
type useResult struct {
	Interrupted         bool    `json:"interrupted"`
	RunID               string  `json:"runId"`
	DurationMs          float64 `json:"durationMs"`
	PersistedOutputPath string  `json:"persistedOutputPath"`
}

// interruptedPrefix opens the text Claude Code writes when the user stops a turn:
// "[Request interrupted by user]" as a user text block, and "[Request interrupted by user
// for tool use]" as the content of the tool_result the stopped call never produced.
const interruptedPrefix = "[Request interrupted by user"

// declinedPrefixes open the words Claude Code gives the model in place of a call's output
// when the person said no to it at the permission prompt, or stopped the turn before it
// ran. In a session's own thread: "The user doesn't want to proceed with this tool use. The
// tool use was rejected (eg. …). STOP what you are doing…", or, where the person typed why,
// "…To tell you how to proceed, the user said:" and what they typed; and "The user doesn't
// want to take this action right now." for a call the stop caught before it started. In a
// subagent's, whose prompts the person answers for it: "Permission for this tool use was
// denied. The tool use was rejected (eg. …)", with "The user said:" and what they typed, or
// not. A call a rule refused is told in other words ("Permission to use Bash has been
// denied.", "Permission for this tool use was denied. It requires approval…"), and failed.
var declinedPrefixes = []string{
	"The user doesn't want to proceed with this tool use.",
	"The user doesn't want to take this action right now.",
	"Permission for this tool use was denied. The tool use was rejected",
}

// stopped reports whether a tool result is a call the person stopped or said no to: an
// interruption, not a failure.
//
// Newer versions of Claude Code say which on the row, in toolDenialKind: "user-rejected"
// for a call declined at the permission prompt, "interrupted" and "cancelled" for one
// stopped while it ran or before it started, "permission-rule" and the "automode-" kinds
// for one a rule, a hook or auto mode's classifier refused, which failed. Where the row
// says, that decides. Where it does not, the result's own words do: the notices above,
// which Claude Code writes as a failed result, and a stopped command's, which is its exit
// code and the interruption notice on the line after it.
func stopped(text string, failed bool, kind string, unread func(kind string)) bool {
	known := true
	switch kind {
	case "", "user-rejected", "interrupted", "cancelled",
		"permission-rule", "automode-blocked", "automode-unavailable", "automode-parsing-error":
	default:
		// A kind not known here is counted whatever the result says, so that doctor tells
		// of it: a new one is how a change in Claude Code's records shows
		unread("toolDenialKind:" + kind)
		known = false
	}
	if strings.HasPrefix(text, interruptedPrefix) {
		return true
	}
	if known {
		switch kind {
		case "user-rejected", "interrupted", "cancelled":
			return true
		case "permission-rule", "automode-blocked", "automode-unavailable", "automode-parsing-error":
			return false
		}
	}
	if !failed {
		return false
	}
	if _, rest, ok := strings.Cut(text, "\n"); ok && exitCode(text) != nil && strings.HasPrefix(rest, interruptedPrefix) {
		return true
	}
	for _, prefix := range declinedPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// exitCode is the exit code a failed command's result opens with, or nil. Claude Code
// writes a command that ended in failure as "Exit code N", then, when the person stopped
// it, "[Request interrupted by user for tool use]", then what it printed:
//
//	Exit code 1
//	--- FAIL: TestOrder
//
// A command that passed, or whose code says no failure (grep finding nothing exits 1), has
// no code recorded: its toolUseResult holds what it printed and whether it was
// interrupted, not how it exited.
func exitCode(text string) *int {
	first, _, _ := strings.Cut(text, "\n")
	digits, ok := strings.CutPrefix(first, "Exit code ")
	if !ok {
		return nil
	}
	code, err := strconv.Atoi(digits)
	if err != nil {
		return nil
	}
	return transcript.ExitOf(code)
}

// Read parses one transcript.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	path := ref.Key
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	sub := isSubagent(path)

	w.S.Path = path
	var meta map[string]any
	forkStart := ""
	if sub {
		meta = kit.Map(kit.ReadJSON(metaPath(path)))
		// A fork opens with a copy of its parent's conversation — the parent's rows as they
		// were, up to the call that started the fork — and then the row that hands it its
		// directive. The copy is the parent's work, read in the parent's transcript: here
		// its rows are inherited and carry no usage, so a fork's turns and counters are its
		// own.
		if kit.Str(meta["agentType"]) == "fork" {
			forkStart = forkStartOf(path, kit.Str(meta["toolUseId"]))
		}
	}

	// A session's own transcript is only ever added to at its end, and a read is taken up
	// where an earlier one marked a turn's first row, when the bytes before it are the
	// ones it was found after. A subagent's is read whole: its meta file, written after it
	// began, can change how its first rows are read
	var c carried
	from := kit.Place{}
	if m := w.From; m != nil && !sub {
		var p place
		if json.Unmarshal(m.Source, &p) == nil && p.At > 0 && kit.Before(path, p.Place) {
			w.Resume(m)
			from, c = p.Place, p.carried
		}
	}
	// add adds a message from the row at origin, and marks the turn it begins, if it does,
	// as a place a later read can take the transcript up again. A command held before
	// that row is let go before anything of the row is added, into the turn before
	add := func(m transcript.Message, origin place) {
		began := w.Turns()
		w.Add(m)
		if w.Turns() != began && origin.At > 0 && !sub {
			w.MarkTurn(origin)
		}
	}

	var held *transcript.Message // a slash command waiting to learn what it set off
	var heldAt place             // the row it is from
	release := func(asAsk bool) {
		if held == nil {
			return
		}
		m := *held
		held = nil
		if asAsk {
			if text := commandAsk(m.Blocks[0].Text); text != "" {
				m.Blocks = []transcript.Block{transcript.TextOf(text)}
			}
		}
		add(m, heldAt)
	}

	n := 0
	err := kit.LinesFrom(path, from, func(line []byte, here kit.Place) bool {
		if n++; n&1023 == 0 && ctx.Err() != nil {
			return false
		}
		origin := place{here, c}
		var r row
		if json.Unmarshal(line, &r) != nil {
			if kind := undecodable(line); kind != "" {
				w.Unread(kind) // a row of a known shape with a field of another: not read, and said so
			}
			return true
		}
		if c.SessionID == "" {
			c.SessionID = r.SessionID
		}
		if c.AgentID == "" {
			c.AgentID = r.AgentID
		}
		if w.S.Cwd == "" {
			w.S.Cwd = r.Cwd
		}
		// The branch the session opened on, as written then: the checkout has moved on
		// since, and resolving it live would answer a question nobody asked
		if w.S.Branch == "" {
			w.S.Branch = r.GitBranch
		}
		if r.Version != "" {
			w.S.Version = r.Version
		}
		if forkStart != "" && r.UUID == forkStart {
			forkStart = "" // the fork's own rows from here
		}

		switch r.Type {
		case "ai-title":
			c.AITitle = r.AITitle
			return true
		case "custom-title":
			c.CustomTitle = r.CustomTitle
			return true
		case "cost-state":
			// The closing row carries the session's own cost total, which no assistant row
			// knows; it is the authoritative number when it is there and is a price
			if total := jsonmap.Cost(r.TotalCostUSD); total > 0 {
				w.S.Recorded = &model.Recorded{CostUSD: total}
			}
			return true

		// The rows below are not conversation, and are passed over knowing what they are.
		case "last-prompt", "queue-operation", "progress":
			// What the conversation's own rows record again: the prompt typed last, the
			// queue a prompt typed mid-turn waits in (the prompt is a queued_command
			// attachment), a call or hook still running (its result says how it ended)
			return true
		case "api-request", "api-request-shape", "api-request-blob", "api-request-resumed":
			// The CLI's log of the requests it sent, and of one taken up again after a
			// restart: each answer is read from its own rows
			return true
		case "file-history-snapshot", "file-history-delta", "attribution-snapshot", "content-replacement":
			// Checkpoints of the files the session edited, kept for a rewind and for saying
			// who wrote what, and what the CLI put in the model's context in place of a
			// large output
			return true
		case "mode", "permission-mode", "repl-mode", "memory-mode", "session-owner", "isolation-latch",
			"atis-latch", "dev-mods", "worktree-state", "bridge-session", "tools-used", "agent-name",
			"agent-color", "agent-setting", "relocated", "history-suppression", "derived-from",
			"data-flow-records", "data-flow-incomplete", "frame-link", "observer-ref", "resume-head-vouch",
			"artifact-comment-monitor", "artifact-autoreact-ledger", "hybrid-organization":
			// The session's settings and state as the CLI keeps them for itself: its modes,
			// its owner and the organizations it ran under, its worktree, the tools it has
			// used, how it is tied to other views
			return true
		case "host-row":
			// What Code Mode kept of a script's calls for showing them: what a call's row was
			// shown as, or that the script's rows were held back. The calls themselves, and
			// their results, are rows of their own, read as any other's
			return true
		case "summary", "tag", "pr-link", "continued-in", "ended-by-model":
			// What the CLI notes about the session for its own lists: the title older
			// versions gave it, a tag, the pull request it opened, the session it went on
			// in, that the model ended it
			return true
		case "fork-context-ref":
			// A fork's pointer to the conversation it starts from, which newer versions write
			// in place of a copy: the parent's rows are read in the parent's transcript
			return true
		}
		// A subagent's rows are not part of the main thread — except in its own transcript
		if r.IsSidechain && !sub {
			return true
		}
		at := kit.Time(r.Timestamp)
		inherited := forkStart != ""

		switch r.Type {
		case "system":
			if r.Subtype == "turn_duration" {
				// How long a turn took, written as it ends: the mark that it is over. The
				// turn's own rows say when it ran
				add(transcript.Message{ID: r.UUID, Role: transcript.System, At: at, Inherited: inherited, Ends: true}, origin)
				return true
			}
			if event, ok := systemEvent(&r, w.Unread); ok {
				release(false)
				add(transcript.Message{ID: r.UUID, Role: transcript.System, At: at, Inherited: inherited, Blocks: []transcript.Block{event}}, origin)
			}
		case "attachment":
			// What the person typed while the agent was working reaches the model as an
			// attachment, at the time it was typed, and for most such prompts nothing else
			// records it. Every other attachment is context the CLI gives the model.
			a := r.Attachment
			if a.Type != "queued_command" {
				if a.Type != "" && !contextAttachment(a.Type) {
					w.Unread("attachment:" + a.Type)
				}
				return true
			}
			switch a.CommandMode {
			case "prompt":
				if a.IsMeta {
					return true // a prompt the CLI wrote itself, or a peer sent: not the person's
				}
			case "task-notification":
				return true // a background task saying how it ended: the CLI's, not the person's
			case "":
				return true // one that names no mode: Claude Code does not take it for the person's either
			default:
				w.Unread("queued_command:" + a.CommandMode)
				return true
			}
			if blocks := promptBlocks(a.Prompt); len(blocks) > 0 {
				release(false)
				add(transcript.Message{ID: r.UUID, Role: transcript.User, At: at, Queued: true, Inherited: inherited,
					Blocks: blocks}, origin)
			}
		case "user":
			m := transcript.Message{ID: r.UUID, Role: transcript.User, At: at, Inherited: inherited,
				// A row the CLI wrote in the user's seat: a skill's expansion, the summary a
				// compaction leaves behind
				Injected: r.IsMeta || r.IsCompact,
				Blocks:   s.userBlocks(&r, w.Unread)}
			if len(m.Blocks) == 0 {
				return true
			}
			text := ""
			if m.Blocks[0].Type == transcript.Text {
				text = strings.TrimSpace(m.Blocks[0].Text)
			}
			// A slash command is plumbing when the CLI answers it itself (/model, /compact:
			// local output follows) and the person's ask when it sets a skill off (the
			// skill's expansion follows, written by the CLI). Which, the next row says.
			if held != nil {
				release(r.IsMeta && !strings.HasPrefix(text, "<local-command-"))
			}
			if !m.Injected && !inherited && strings.HasPrefix(text, "<command-") && len(m.Blocks) == 1 {
				held, heldAt = &m, origin
				return true
			}
			add(m, origin)
		case "assistant":
			release(false)
			// A row can say anything of its cost and its counts: each is read by the rule
			// every adapter's is (jsonmap.Cost, model.Tokens), so that no transcript can
			// take a session's totals below zero, or its cost to infinity
			m := transcript.Message{ID: r.UUID, Role: transcript.Assistant, At: at, Inherited: inherited,
				Response: r.Message.ID, Model: r.Message.Model, Stop: r.Message.StopReason,
				CostUSD: jsonmap.Cost(r.Message.CostUSD), Blocks: assistantBlocks(&r, w.Unread)}
			if u := r.Message.Usage; u != nil && !inherited {
				m.Usage = &model.Usage{Input: kit.Count(u.Input), Output: kit.Count(u.Output), CacheRead: kit.Count(u.CacheRead), CacheWrite: kit.Count(u.CacheWrite)}
			}
			add(m, origin)
		case "":
			// A line that does not say what it is: no row this CLI writes
		default:
			w.Unread("row:" + r.Type)
		}
		return true
	})
	release(false)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	w.S.Title = kit.First(c.CustomTitle, c.AITitle)
	// Claude Code writes a turn's rows as it goes, the model's a content block at a time,
	// each with why the response stopped once it has; and turn_duration as many turns end
	w.Tells = transcript.TellsByRows
	if !sub {
		w.S.ID = kit.First(c.SessionID, stem)
		return nil
	}
	// A subagent's rows carry the parent's sessionId; its own id is the agent id, which is
	// also how the parent's records name it. The meta file says what kind of agent it was,
	// what it was asked to do (the title — the prompt itself is often pages long), which
	// agent spawned it, and the call that did.
	w.S.ID = kit.First(c.AgentID, strings.TrimPrefix(stem, "agent-"))
	parent, run := parentOf(path)
	w.S.Parent = &model.Parent{
		Session: kit.First(c.SessionID, parent),
		Agent:   kit.Str(meta["parentAgentId"]),
		Call:    kit.Str(meta["toolUseId"]),
		Run:     run,
		Type:    kit.Str(meta["agentType"]),
		Fork:    kit.Str(meta["agentType"]) == "fork",
	}
	w.S.Title = kit.First(kit.Str(meta["description"]), kit.Str(meta["name"]), w.S.Title)
	return nil
}

// carried is what a read of a transcript takes from row to row beyond what it sets on the
// Writer: the session's and agent's ids, as the first rows that carry them give them, and
// its titles, as the last rows that set them do.
type carried struct {
	SessionID   string `json:"sessionId,omitempty"`
	AgentID     string `json:"agentId,omitempty"`
	AITitle     string `json:"aiTitle,omitempty"`
	CustomTitle string `json:"customTitle,omitempty"`
}

// place is where a later read can take a transcript up again (see transcript.Mark): the
// row a turn begins at, and what the read carried to it.
type place struct {
	kit.Place
	carried
}

// parentOf reads a subagent's place off its path: the session directory above
// subagents/, and for a workflow's agent the run directory it sits in — which is what
// ties it to the call that started it, since its meta file names none.
func parentOf(path string) (session, run string) {
	for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if filepath.Base(dir) != "subagents" {
			continue
		}
		session = filepath.Base(filepath.Dir(dir))
		if rel, err := filepath.Rel(dir, filepath.Dir(path)); err == nil {
			if first, rest, ok := strings.Cut(filepath.ToSlash(rel), "/"); ok && first == "workflows" && rest != "" && !strings.Contains(rest, "/") {
				run = rest
			}
		}
		return session, run
	}
	return "", ""
}

// forkStartOf is the uuid of the row that hands a fork its directive: the result of the
// call that started it beside Claude Code's <fork-boilerplate>. Everything before it is
// the parent's. Empty when the transcript has no such row, and then nothing is inherited.
func forkStartOf(path, callID string) string {
	start := ""
	_ = kit.Lines(path, func(line []byte) bool {
		if !bytes.Contains(line, []byte("<fork-boilerplate>")) || (callID != "" && !bytes.Contains(line, []byte(callID))) {
			return true
		}
		var r row
		if json.Unmarshal(line, &r) != nil || r.Type != "user" {
			return true
		}
		start = r.UUID
		return start == ""
	})
	return start
}

// commandAsk renders a slash command row as what the person typed: "/review the diff".
func commandAsk(text string) string {
	name := between(text, "<command-name>", "</command-name>")
	if name == "" {
		return ""
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	if args := strings.TrimSpace(between(text, "<command-args>", "</command-args>")); args != "" {
		return name + " " + args
	}
	return name
}

func between(s, open, end string) string {
	_, rest, ok := strings.Cut(s, open)
	if !ok {
		return ""
	}
	inner, _, ok := strings.Cut(rest, end)
	if !ok {
		return ""
	}
	return strings.TrimSpace(inner)
}

// contextAttachment reports whether an attachment is of a kind Claude Code lays before the
// model in the person's seat, or keeps for itself: files and selections, reminders,
// listings, instructions, modes, what a hook printed. None of it is what either side said
// — the person's words are in their own rows — and all of it is passed over. The kinds are
// the CLI's own: those its code turns into context for the model, or drops.
func contextAttachment(kind string) bool {
	switch kind {
	case "hook_success", "hook_blocking_error", "hook_non_blocking_error", "hook_error_during_execution",
		"hook_cancelled", "hook_additional_context", "hook_stopped_continuation", "hook_system_message",
		"hook_permission_decision", "hook_deferred_tool", "async_hook_response", "async_hook_response_batch",
		"hook_plugin_listing":
		// What a hook printed or decided; a Stop hook's failure is read from its system row
	case "plugin_note":
		// What a plugin put before the model, which Claude Code hands on as a prompt it wrote
		// itself: not the person's
	case "file", "directory", "edited_text_file", "edited_image_file", "compact_file_reference",
		"pdf_reference", "already_read_file", "plan_file_reference", "mcp_resource", "diagnostics",
		"inlined_image_paths", "selected_lines_in_ide", "selected_lines_in_diff", "opened_file_in_ide",
		"at_mention_reference", "agent_mention", "skill_mention", "peer_mention", "unknown_command_fallback",
		"prompt_as_file", "whole_texts_withdrawn":
		// Files, folders and selections laid before the model, and what a prompt mentioned; a
		// long prompt saved to a file for the model, and whole texts taken back out of what it
		// is given: the prompt itself is read from its own row
	case "nested_memory", "relevant_memories", "withheld_memory", "account_memory_recall",
		"cowork_memory_context", "instruction_files_not_loaded", "project_doc_reads", "message_pdf_reads",
		"memory_update":
		// Memory and instruction files, which of them were read, and what changed in them
	case "total_tokens_reminder", "silent_turn_reminder", "elapsed_time_reminder", "batching_reminder",
		"batching_reminder_sent", "secondary_reminder", "secondary_reminder_sent", "critical_system_reminder",
		"todo_reminder", "task_reminder", "tool_search_usage_reminder", "bash_output_audience_note",
		"read_truncation_notice", "output_limit_note", "dir_sync_notice", "claude_app_notice",
		"tool_hosts_notice", "tool_hosts_correction", "linked_plugin_tools_note", "widget_context_hint",
		"token_usage", "budget_usd", "output_token_usage", "attention_budget", "context_efficiency",
		"max_turns_reached", "date", "date_change", "poll_events", "agent_ask_notice", "first_week":
		// Reminders and notices the CLI gives the model: of its budget, the time and the
		// date, what the person sees, the events it was woken by, the agents waiting on a
		// permission prompt, what a person new to it is told
	case "environment", "session_context", "model", "language", "instructions", "sandbox_instructions",
		"output_style", "output_style_instructions", "command_permissions", "skill_listing", "dynamic_skill",
		"invoked_skills", "agent_listing_delta", "deferred_tools_delta", "deferred_tools_record",
		"mcp_instructions_delta", "mcp_dropped_tools_delta", "repl_mcp_needs_auth", "advisor_tool",
		"context_sections", "coordinator_context", "prefix_delta", "thread_state", "fork_briefing",
		"remote_session_change", "workflow_keyword_request", "workflow_size_guideline_change",
		"artifact_opening_prefetch", "proactivity", "goals", "goal_status", "repl_mcp_servers",
		"tool_offer_record":
		// What the model is told it has and how to work: its environment and instructions,
		// the skills, agents and tools it can call — Code Mode's MCP servers among them — and
		// which tools it was offered
	case "plan_mode", "plan_mode_reentry", "plan_mode_exit", "auto_mode", "auto_mode_exit",
		"ultra_effort_enter", "ultra_effort_exit", "ultrathink_effort":
		// A mode the session went into or out of
	case "task_status", "team_context", "teammate_mailbox", "teammate_shutdown_batch", "structured_output":
		// Background tasks and teammates as the model is told of them, and a structured
		// answer, which the call that gave it holds already
	case "prompt_snapshot", "prompt_render_point", "thinking_drop", "thinking_stripped", "advisor_stripped",
		"tool_results_cut", "tool_host_result_lines", "credential_org", "session_cron_carry", "projects_thread_ask":
		// The CLI's records of what it sent the model and what it held back
	case "autocheckpointing", "background_task_status", "todo", "task_progress", "ultramemory",
		"compaction_reminder", "current_session_memory", "thinking_reminder", "companion_intro",
		"pen_mode_enter", "pen_mode_exit", "ultrawork_request", "echo_activities", "verify_plan_reminder",
		"fold_nudge", "context_tip", "audio_transcript":
		// Kinds earlier versions wrote that Claude Code itself now passes over
	default:
		return false
	}
	return true
}

// systemEvent turns the system rows worth a reader's attention into events: a compaction
// boundary (the context the model sees was rewritten here), a stop hook that failed, and
// another model taking over. The rest is bookkeeping, and a subtype it has never heard of
// is told to unread.
func systemEvent(r *row, unread func(kind string)) (transcript.Block, bool) {
	switch r.Subtype {
	case "compact_boundary":
		text := contentText(r.Content)
		if text == "" {
			text = "Context compacted"
		}
		return transcript.EventOf(model.EventCompaction, text), true
	case "model_refusal_fallback", "model_fallback", "model_consent_fallback":
		// Another model answered in the one asked for's place: after it refused (the Agent
		// SDK's SDKModelRefusalFallbackMessage), because it failed or is not available, or
		// because using it needed a consent the person did not give. Claude Code says which
		// in words of its own; the rows that follow name the model that answered
		text := contentText(r.Content)
		if text == "" && r.FallbackModel != "" {
			text = "Switched to " + r.FallbackModel
			if r.OriginalModel != "" {
				text = "Switched from " + r.OriginalModel + " to " + r.FallbackModel
			}
		}
		if text == "" {
			return transcript.Block{}, false
		}
		return transcript.EventOf(model.EventModelChange, text), true
	case "model_refusal_no_fallback":
		// The model refused and no other was asked: the refusal is the assistant's row,
		// which is read
	case "stop_hook_summary":
		var errs []string
		for _, raw := range r.HookErrors {
			if text := contentText(raw); text != "" {
				errs = append(errs, text)
			}
		}
		if len(errs) == 0 {
			return transcript.Block{}, false
		}
		return transcript.EventOf(model.EventHookError, strings.Join(errs, "\n")), true
	case "informational", "local_command":
		// A notice, a local command's output: the turn's own rows say what it did
	case "api_error":
		// A request that failed and is about to be retried; an error that ends a turn is
		// written in the assistant's place, and read there
	case "away_summary":
		// The recap the CLI writes for a person coming back: a summary of what is here
	case "scheduled_task_fire":
		// A scheduled task went off; the prompt it set off follows as a row of its own
	case "memory_saved", "agents_killed", "permission_retry", "file_snapshot", "bridge_status", "cloud_session_status":
		// Notices of the CLI's own doings: what it saved to memory, background agents
		// stopped, commands retried with permission, a copy of the plan file kept with the
		// session, where else the session can be continued
	case "":
		// A row that names no subtype names no kind to count
	default:
		unread("system:" + r.Subtype)
	}
	return transcript.Block{}, false
}

// contentText flattens content that is a string or an array of blocks.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return kit.ContentText(v)
}

// promptBlocks is a prompt typed while the agent worked: its words, and the files pasted
// with it.
func promptBlocks(raw json.RawMessage) []transcript.Block {
	var out []transcript.Block
	if text := strings.TrimSpace(contentText(raw)); text != "" {
		out = append(out, transcript.TextOf(text))
	}
	for _, p := range parts(raw) {
		if p.Type == "image" || p.Type == "document" {
			out = append(out, transcript.AttachmentOf(fileOf(p.Type, p.Source)))
		}
	}
	return out
}

// resultText flattens a tool result's content as contentText does, and tells unread of
// the blocks in it that are of a kind it has never heard of. The files in it — a
// screenshot, a page of a PDF: what the model saw, with no text to read — come apart.
func resultText(raw json.RawMessage, unread func(kind string)) (string, []transcript.Attachment) {
	if len(raw) == 0 {
		return "", nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", nil
	}
	var files []transcript.Attachment
	if list, ok := v.([]any); ok {
		for _, item := range list {
			b, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch kind := kit.Str(b["type"]); kind {
			case "text":
			case "image", "document":
				var src *fileSource
				if m, ok := b["source"].(map[string]any); ok {
					src = &fileSource{Type: kit.Str(m["type"]), MediaType: kit.Str(m["media_type"]), Data: kit.Str(m["data"]),
						URL: kit.Str(m["url"]), FileID: kit.Str(m["file_id"])}
				}
				files = append(files, fileOf(kind, src))
			case "tool_reference":
				// A tool a search loaded, named for the model: the call says what was sought
			case "":
				// A block that names no type names no kind to count
			default:
				unread("block:" + kind)
			}
		}
	}
	return kit.ContentText(v), files
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
	return list
}

func assistantBlocks(r *row, unread func(kind string)) []transcript.Block {
	var out []transcript.Block
	for _, p := range parts(r.Message.Content) {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, transcript.TextOf(p.Text))
			}
		case "thinking":
			if p.Thinking != "" {
				out = append(out, transcript.ThinkingOf(p.Thinking))
			}
		case "tool_use":
			out = append(out, transcript.CallOf(p.ID, p.Name, p.Input))
		case "redacted_thinking":
			// Thinking the API sent encrypted: there is no text in it to read
		case "fallback":
			// The CLI's mark that another model took over the answer: the row's own model
			// says which one gave it
		case "":
			// A block that names no type names no kind to count
		default:
			unread("block:" + p.Type)
		}
	}
	return out
}

func (s *Source) userBlocks(r *row, unread func(kind string)) []transcript.Block {
	list := parts(r.Message.Content)
	results := 0
	for _, p := range list {
		if p.Type == "tool_result" {
			results++
		}
	}
	var use useResult
	if len(r.ToolUseResult) > 0 && bytes.TrimSpace(r.ToolUseResult)[0] == '{' {
		_ = json.Unmarshal(r.ToolUseResult, &use)
	}
	var out []transcript.Block
	for _, p := range list {
		switch p.Type {
		case "text":
			if p.Text == "" {
				continue
			}
			// The interruption notice is not something the person said, it is something
			// that happened
			if strings.HasPrefix(strings.TrimSpace(p.Text), interruptedPrefix) {
				out = append(out, transcript.EventOf(model.EventInterrupted, strings.TrimSpace(p.Text)))
				continue
			}
			out = append(out, transcript.TextOf(p.Text))
		case "tool_result":
			text, files := resultText(p.Content, unread)
			head := strings.TrimSpace(text)
			// is_error is Claude's own verdict, and a failed command's result opens with its
			// exit code. A call the person stopped or declined is interrupted: the row-level
			// record says so of a command they stopped, and stopped reads the rest.
			outcome := transcript.Outcome{Status: transcript.StatusOf(p.IsError)}
			if p.IsError {
				outcome.Exit = exitCode(head)
			}
			// The row-level record speaks for the row's one result
			kind := ""
			if results == 1 {
				outcome.DurMs = millis(use.DurationMs)
				kind = r.ToolDenialKind
			}
			if stopped(head, p.IsError, kind, unread) || use.Interrupted {
				outcome.Status = model.StatusInterrupted
			}
			// An output too large for the transcript is saved beside it, and the row keeps
			// a stub: a line naming the file and a 2 KB preview. The output is what the
			// file holds — the end of a build log is exactly what the preview drops.
			if saved := s.savedOutput(text, use.PersistedOutputPath); saved != "" {
				if whole, ok := readSaved(saved); ok {
					text = whole
				}
			}
			block := transcript.ResultOf(p.ToolUseID, "", text, outcome).With(files...)
			// A Workflow call's result names the run it started; the run's agents are
			// transcripts of their own, and this is the call they hang from
			if results == 1 {
				block.Run = use.RunID
			}
			out = append(out, block)
		case "image", "document":
			// A picture or a document the person attached: no words of theirs, and marked
			// where it came
			out = append(out, transcript.AttachmentOf(fileOf(p.Type, p.Source)))
		case "":
			// A block that names no type names no kind to count
		default:
			unread("block:" + p.Type)
		}
	}
	return out
}

// stubPath is the path a stub names on its first line, or "" when the result is not a
// stub. Claude Code writes two: a command's output inside <persisted-output>, "Output too
// large (30.5KB). Full output saved to: <path>", and an MCP tool's result over the token
// limit, "Error: result (56,846 characters) exceeds maximum allowed tokens. Output has
// been saved to <path>." Both are recognised by how they open, not by a path somewhere in
// the text: a command whose output merely mentions a saved file is not a stub.
func stubPath(text string) string {
	body := text
	if rest, ok := strings.CutPrefix(text, "<persisted-output>"); ok {
		body = strings.TrimLeft(rest, "\r\n")
	} else if !strings.HasPrefix(text, "Error: result (") {
		return ""
	}
	first, _, _ := strings.Cut(body, "\n")
	_, path, ok := strings.Cut(first, "saved to")
	if !ok {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(path, ":"))
}

var savedOutputPath = regexp.MustCompile(`([^/\s]+)/([^/\s]+)/tool-results/([^/\s]+)$`)

// savedOutput is where a stub's output is on this machine, or "" when the result is not a
// stub or its file is not there.
//
// Only the last parts of the recorded path are used — project, session, file — and they
// are looked up under this source's own root. The recorded path is whatever the
// transcript says: for a directory copied from another machine it names a path there,
// and in a doctored transcript it could name any file on this one.
func (s *Source) savedOutput(text, rowPath string) string {
	if s.root == "" {
		return ""
	}
	recorded := stubPath(text)
	if recorded == "" && strings.HasPrefix(text, "<persisted-output>") {
		recorded = rowPath // the row says the same, should the stub's wording move
	}
	if recorded == "" {
		return ""
	}
	m := savedOutputPath.FindStringSubmatch(strings.ReplaceAll(recorded, "\\", "/"))
	if m == nil {
		return ""
	}
	project, session, file := m[1], m[2], strings.TrimSuffix(m[3], ".")
	for _, p := range []string{project, session, file} {
		if p == "." || p == ".." || p == "" {
			return ""
		}
	}
	path := filepath.Join(s.root, project, session, "tool-results", file)
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

// savedLimit caps what is read of one saved output, and savedHead is how much of it comes
// from the start; the rest is the end, where a build log says how it failed. Most saved
// outputs are tens of kilobytes; the cap is for the one of 52 MB.
var (
	savedLimit int64 = 8 << 20
	savedHead  int64 = 1 << 20
)

func readSaved(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	size := info.Size()
	if size <= savedLimit {
		data, err := io.ReadAll(io.LimitReader(f, savedLimit))
		if err != nil {
			return "", false
		}
		return string(data), true
	}
	head := make([]byte, savedHead)
	if _, err := io.ReadFull(f, head); err != nil {
		return "", false
	}
	tail := make([]byte, savedLimit-savedHead)
	if _, err := f.ReadAt(tail, size-int64(len(tail))); err != nil {
		return "", false
	}
	gap := size - int64(len(head)) - int64(len(tail))
	// A cut can land inside a character; the bytes of half a character are dropped
	return strings.ToValidUTF8(string(head), "") +
		fmt.Sprintf("\n\n… %d bytes of the middle left out; the whole output is %s …\n\n", gap, filepath.Base(path)) +
		strings.ToValidUTF8(string(tail), ""), true
}

// undecodable names a line that is a row — a JSON object with a type — but did not decode
// as one; "" for a line that is not a row at all.
func undecodable(line []byte) string {
	var envelope struct {
		Type string `json:"type"`
	}
	err := json.Unmarshal(line, &envelope)
	var mistyped *json.UnmarshalTypeError
	if (err != nil && !errors.As(err, &mistyped)) || envelope.Type == "" {
		return ""
	}
	return "undecodable:row:" + envelope.Type
}
