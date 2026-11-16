// Package codex reads Codex's sessions.
//
//	<root>/<year>/<month>/<day>/rollout-<time>-<id>.jsonl[.zst]   a session ("rollout")
//	<root>/../archived_sessions/…                                 the ones Codex put away
//
// A rollout is one JSON object per line, each {timestamp, type, payload}, and the
// conversation is spread over several row types:
//
//	session_meta                            who the session is, and whose child
//	turn_context                            the turn's id, model and directory
//	response_item/message                   words; role developer is machine instructions
//	response_item/reasoning                 thinking, when it is not encrypted
//	response_item/function_call             a tool call: name, arguments (a JSON string)
//	response_item/custom_tool_call          the freeform tools: exec, apply_patch
//	response_item/local_shell_call          the older shell
//	response_item/web_search_call           a search, which has no output row
//	response_item/*_call_output             a result, by call_id
//	response_item/agent_message             one agent of a team speaking to another
//	token_usage_record                      what the model response just written cost
//	event_msg/turn_aborted, compacted       what happened between the words
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Spec describes Codex.
var Spec = sources.Spec{
	Name:  "codex",
	Title: "Codex",
	Locate: func(env sources.Env) string {
		// CODEX_HOME moves the whole ~/.codex, sessions and archived_sessions with it
		if d := env.Getenv("CODEX_HOME"); d != "" {
			return filepath.Join(d, "sessions")
		}
		return filepath.Join(env.Home, ".codex", "sessions")
	},
	Open:    func(location string) sources.Source { return &Source{root: location} },
	Movable: true,
	Resume:  "codex resume",
	Store:   "~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl",
}

// Source is a Codex sessions directory.
type Source struct {
	root string

	// A fork's rollout opens with its parent's turns copied in, and the parent's own
	// rollout is what tells a copied turn from the fork's own. Its turn ids are read once
	// per version of the parent's file.
	mu    sync.Mutex
	turns map[string]turnIDs
}

type turnIDs struct {
	stamp string
	ids   map[string]bool
}

func (s *Source) Location() string { return s.root }
func (s *Source) Exists() bool     { return kit.Exists(s.root) }

// archivedRoot is archived_sessions beside the default sessions directory: where Codex
// moves a rollout it archives.
func (s *Source) archivedRoot() string {
	if filepath.Base(s.root) != "sessions" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.root), "archived_sessions")
}

// files lists every rollout: the dated tree, and the archive both flat — the way Codex
// moves a rollout there — and dated.
func (s *Source) files() ([]string, error) {
	prefixes := []string{filepath.Join(s.root, "*", "*", "*", "rollout-*")}
	if archived := s.archivedRoot(); archived != "" {
		prefixes = append(prefixes, filepath.Join(archived, "rollout-*"), filepath.Join(archived, "*", "*", "*", "rollout-*"))
	}
	var files []string
	var unread error
	for _, prefix := range prefixes {
		found, err := rollouts(prefix)
		files, unread = append(files, found...), errors.Join(unread, err)
	}
	return files, unread
}

// rollouts finds plain and compressed rollouts for one glob prefix. While Codex is
// compressing its history both forms of a rollout can exist; it treats the plain file as
// current until compression finishes, so the plain one wins and the pair is listed once.
func rollouts(prefix string) ([]string, error) {
	// A day's directory that could not be read is reported beside what was found
	plain, unread := kit.Glob(prefix + ".jsonl")
	compressed, _ := kit.Glob(prefix + ".jsonl.zst") // the same directories: the same error
	have := make(map[string]bool, len(plain))
	for _, path := range plain {
		have[stem(path)] = true
	}
	out := append([]string(nil), plain...)
	for _, path := range compressed {
		if !have[stem(path)] {
			out = append(out, path)
		}
	}
	return out, unread
}

func stem(path string) string {
	return strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".zst"), ".jsonl")
}

// Scan lists every rollout.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	files, unread := s.files()
	refs := make([]sources.Ref, 0, len(files))
	for _, path := range files {
		stamp, mod := kit.Stamp(path)
		refs = append(refs, sources.Ref{Key: path, Stamp: stamp, ModTime: mod})
	}
	return refs, unread
}

// row is one line of a rollout: the envelope, and the fields of its payload that decide
// what it is. The payload's content is decoded where it is used.
type row struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Role string `json:"role"`

		// session_meta
		SessionID    string          `json:"session_id"`
		Cwd          string          `json:"cwd"`
		CLIVersion   string          `json:"cli_version"`
		Timestamp    string          `json:"timestamp"`
		ForkedFrom   string          `json:"forked_from_id"`
		ParentThread string          `json:"parent_thread_id"`
		ThreadSource string          `json:"thread_source"`
		AgentPath    string          `json:"agent_path"`
		AgentName    string          `json:"agent_nickname"`
		Git          json.RawMessage `json:"git"`

		// turn_context
		TurnID string `json:"turn_id"`
		Model  string `json:"model"`

		// response_item
		Content   json.RawMessage `json:"content"`
		Summary   json.RawMessage `json:"summary"`
		Name      string          `json:"name"`
		Namespace string          `json:"namespace"`
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		Action    json.RawMessage `json:"action"`
		Output    json.RawMessage `json:"output"`
		Status    string          `json:"status"`
		Author    string          `json:"author"`
		Recipient string          `json:"recipient"`

		// token_usage_record, event_msg
		ResponseID string          `json:"response_id"`
		Usage      *usage          `json:"usage"`
		Info       json.RawMessage `json:"info"`
		Reason     string          `json:"reason"`
		Message    json.RawMessage `json:"message"`
	} `json:"payload"`
}

// usage is Codex's token counters: OpenAI's shape, where the input counts the cached
// tokens and the output counts the reasoning.
type usage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
}

// mostTokens is far more than any one model response has spent: a counter past it, like
// one below zero, is not a count.
const mostTokens = 1 << 40

// canonical takes the cached tokens back out of the input, so the parts do not overlap.
// A counter that is not a count reads as none: one corrupt record must not take a
// session's tokens below zero, or carry their sum past what an int64 holds.
func (u usage) canonical() *model.Usage {
	count := func(n int64) int64 {
		if n < 0 || n > mostTokens {
			return 0
		}
		return n
	}
	in, cached, written := count(u.Input), count(u.Cached), count(u.CacheWrite)
	return &model.Usage{
		Input:      max(in-cached-written, 0),
		Output:     count(u.Output),
		CacheRead:  cached,
		CacheWrite: written,
		Reasoning:  count(u.Reasoning),
	}
}

// spentAt reads one token_count snapshot of a rollout that has no records, and returns
// what the response it follows cost — or nil when the snapshot reports no response.
//
// The running total is not the session's total, though it reads like one. Measured on
// real rollouts, and reported upstream from others: Codex writes a snapshot again after
// the fact (the same counters, a later time); a snapshot written after a compaction
// repeats the total with a last usage of zero, as a marker of the new context's size; an
// old snapshot can be re-sent at the end of the file, below the ones before it; a session
// resumed in a new process starts its total again from that request; and a request can
// continue from an earlier total than the newest. So the last snapshot can be far below
// what was spent, and so can the highest. What each response cost is the one figure that
// adds up: a snapshot counts once, for its last usage, and one seen before does not count.
//
// A snapshot from before Codex wrote last_token_usage carries the total alone. There what
// a response cost is how far it moved the total past its highest — and a total below that
// is an old one sent again, which moves nothing.
func spentAt(last, total *usage, seen map[[2]usage]bool, high *usage) *usage {
	if last == nil && total == nil {
		return nil
	}
	var key [2]usage
	if last != nil {
		key[0] = *last
	}
	if total != nil {
		key[1] = *total
	}
	if seen[key] {
		return nil
	}
	seen[key] = true

	cost := last
	if last == nil {
		if total.Input < high.Input || total.Cached < high.Cached || total.CacheWrite < high.CacheWrite ||
			total.Output < high.Output || total.Reasoning < high.Reasoning {
			return nil
		}
		cost = &usage{Input: total.Input - high.Input, Cached: total.Cached - high.Cached, CacheWrite: total.CacheWrite - high.CacheWrite,
			Output: total.Output - high.Output, Reasoning: total.Reasoning - high.Reasoning}
	}
	if total != nil {
		high.Input, high.Cached, high.CacheWrite = max(high.Input, total.Input), max(high.Cached, total.Cached), max(high.CacheWrite, total.CacheWrite)
		high.Output, high.Reasoning = max(high.Output, total.Output), max(high.Reasoning, total.Reasoning)
	}
	if cost.Input+cost.Output == 0 {
		return nil // the size of a context, not a response
	}
	return cost
}

// Read parses one rollout.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	path := ref.Key
	w.S.Path = path
	if archived := s.archivedRoot(); archived != "" && strings.HasPrefix(path, archived+string(filepath.Separator)) {
		w.S.Archived = true
	}

	var (
		hasMeta   bool
		ownID     string // session_meta's id: the file's own
		sessionID string // session_meta's session_id: the parent's, in a child's rollout
		salvaged  string // a session_id picked up from a later row, when there is no meta
		forkedOf  string
		parentID  string
		subagent  bool
		agentPath string
		copied    map[string]bool // the parent's turn ids, once a fork is known
		turn      string
		turnModel string
		names     = map[string]string{} // call id → tool, for the one output that needs it
		pending   []transcript.Message  // a response's rows, held until its usage arrives
		responses int
		anyRecord bool
		// For a rollout too old to have records: the snapshots already read, and the
		// highest each cumulative counter has stood
		snapshots = map[[2]usage]bool{}
		high      usage
	)
	// A model response is written as several rows — reasoning, words, each call — and
	// closed by a token_usage_record saying what the response cost: one record per
	// response, not per turn (measured on real rollouts: 157 records, each with its own
	// response id, over 6 turns — one turn held 65). The rows are held until the record
	// arrives so they go in as one model call carrying that usage; a rollout too old to
	// have records releases them when anything else arrives.
	// end closes the response whose rows are pending: they go in as one model call, with
	// what it cost when that is known.
	//
	// A record is written as its response ends, ahead of the outputs of the calls it
	// made; the snapshot an old rollout has instead is written after them (measured:
	// call, record, output, snapshot). So a snapshot usually finds its response already
	// closed by the first output, and its cost belongs to that response — unpaid is the
	// one that is waiting for it.
	unpaid := ""
	end := func(responseID string, cost *usage, at time.Time) {
		if len(pending) == 0 && cost == nil {
			return
		}
		id := unpaid
		if len(pending) > 0 || responseID != "" || id == "" {
			responses++
			id = "response-" + strconv.Itoa(responses)
			if responseID != "" {
				// The API's own id for the response. A record written twice — the CLI
				// rewriting a finished response — names the same id and is counted once
				id = responseID
			}
		}
		for _, m := range pending {
			m.Response = id
			w.Add(m)
		}
		unpaid = id
		if cost != nil {
			unpaid = ""
			if !copied[turn] {
				w.Add(transcript.Message{Role: transcript.Assistant, At: at, Response: id,
					Model: turnModel, Usage: cost.canonical()})
			}
		}
		pending = pending[:0]
	}
	flush := func(rec *row) {
		if rec == nil {
			end("", nil, time.Time{})
			return
		}
		end(rec.Payload.ResponseID, rec.Payload.Usage, kit.Time(rec.Timestamp))
	}

	n := 0
	err := kit.Lines(path, func(line []byte) bool {
		if n++; n&1023 == 0 && ctx.Err() != nil {
			return false
		}
		var r row
		if json.Unmarshal(line, &r) != nil {
			return true
		}
		p := &r.Payload
		at := kit.Time(r.Timestamp)
		inherited := copied[turn]
		// With no session_meta there is nothing authoritative to read, and the fields are
		// picked up from whichever rows carry them: on a real rollout turn_context has
		// only the cwd and token_usage_record only the session_id. Never a bare id: a
		// message row carries one too, and it is the message's.
		if !hasMeta && r.Type != "session_meta" && r.Type != "response_item" {
			salvaged = kit.First(salvaged, p.SessionID)
			w.S.Cwd, w.S.Version = kit.First(w.S.Cwd, p.Cwd), kit.First(w.S.Version, p.CLIVersion)
		}

		switch r.Type {
		case "session_meta":
			if hasMeta {
				return true // a fork carries its parent's meta further down: the first is its own
			}
			hasMeta = true
			ownID, sessionID = p.ID, p.SessionID
			forkedOf, parentID = p.ForkedFrom, p.ParentThread
			subagent, agentPath = p.ThreadSource == "subagent", p.AgentPath
			w.S.Cwd, w.S.Version = kit.First(p.Cwd, w.S.Cwd), kit.First(p.CLIVersion, w.S.Version)
			w.S.StartedAt = kit.Time(p.Timestamp)
			var git struct {
				Branch string `json:"branch"`
			}
			if json.Unmarshal(p.Git, &git) == nil {
				w.S.Branch = git.Branch
			}
			if from := kit.First(forkedOf, parentID); from != "" {
				copied = s.parentTurns(from)
			}
			return true
		case "turn_context":
			flush(nil)
			turn = p.TurnID
			turnModel = kit.First(p.Model, turnModel)
			if w.S.Cwd == "" {
				w.S.Cwd = p.Cwd
			}
			return true
		case "token_usage_record":
			anyRecord = true
			flush(&r)
			return true
		case "compacted":
			flush(nil)
			text := contentString(p.Message)
			if text == "" {
				text = "Context compacted"
			}
			w.Add(transcript.Message{Role: transcript.System, At: at, Inherited: inherited,
				Blocks: []transcript.Block{transcript.EventOf(model.EventCompaction, text)}})
			return true
		case "event_msg":
			switch p.Type {
			case "turn_aborted":
				flush(nil)
				reason := kit.First(p.Reason, "interrupted")
				w.Add(transcript.Message{Role: transcript.System, At: at, Inherited: inherited,
					Blocks: []transcript.Block{transcript.EventOf(model.EventInterrupted, "Turn aborted: "+reason)}})
			case "token_count":
				// The counters Codex writes after a response: what that response cost
				// (last_token_usage) and the running total (total_token_usage). A rollout
				// with records does not need them — a record says what its response cost,
				// and the snapshots leave out what a compaction spent. A rollout too old
				// to have records has nothing else.
				var info struct {
					Last  *usage `json:"last_token_usage"`
					Total *usage `json:"total_token_usage"`
				}
				if anyRecord || json.Unmarshal(p.Info, &info) != nil {
					return true
				}
				if cost := spentAt(info.Last, info.Total, snapshots, &high); cost != nil {
					end("", cost, at)
				}
			case "user_message", "agent_message", "agent_reasoning", "agent_reasoning_raw_content":
				// The event stream's copy of the words and the thinking, which the
				// response_item rows hold: older versions write both
			case "item_started", "item_completed":
				// The event stream's copy of each item — a message, a thought, a call and
				// its result — which the response_item rows hold
			case "task_started", "task_complete", "thread_settings_applied":
				// A turn starting and ending, and the settings it runs with: turn_context
				// opens a turn, and the rows in it say how it went
			case "context_compacted":
				// The event for a compaction, which the compacted row records
			case "entered_review_mode", "exited_review_mode":
				// /review starting and ending: what the review found is written as
				// messages too
			default:
				if p.Type != "" {
					w.Unread("event:" + p.Type)
				}
			}
			return true
		case "world_state", "dynamic_tools_updated":
			// What the model is given to work with — instructions, tools, skills,
			// permissions — and the tools changing: the CLI's setup, not conversation
			return true
		case "retained_context":
			// What a compaction keeps of the conversation: copies of what was already read
			return true
		case "inter_agent_communication_metadata":
			// Whether a message between agents starts a turn; the message is a row of its own
			return true
		case "security_risk_score":
			// Risk scores sampled for what the session did: the CLI's bookkeeping
			return true
		case "response_item":
		default:
			if r.Type != "" {
				w.Unread("row:" + r.Type)
			}
			return true
		}

		assistant := func(blocks ...transcript.Block) {
			pending = append(pending, transcript.Message{ID: p.ID, Role: transcript.Assistant, At: at,
				Model: turnModel, Inherited: inherited, Blocks: blocks})
		}
		callID := kit.First(p.CallID, p.ID)
		switch p.Type {
		case "message":
			switch p.Role {
			case "assistant":
				if text := messageText(p.Content, w.Unread); text != "" {
					assistant(transcript.TextOf(text))
				}
			case "user":
				flush(nil)
				if text := messageText(p.Content, w.Unread); text != "" {
					w.Add(transcript.Message{ID: p.ID, Role: transcript.User, At: at, Inherited: inherited,
						Blocks: []transcript.Block{transcript.TextOf(text)}})
				}
			case "developer":
				// instructions the CLI assembled: not conversation
			default:
				if p.Role != "" {
					w.Unread("role:" + p.Role)
				}
			}
		case "agent_message":
			// One agent of a team speaking to another. Addressed to a subagent by the agent
			// above it, it is that subagent's ask; anything else is news that arrived
			flush(nil)
			text := messageText(p.Content, w.Unread)
			if text == "" {
				return true
			}
			if subagent && p.Recipient == agentPath && p.Author != agentPath {
				w.Add(transcript.Message{ID: p.ID, Role: transcript.User, At: at, Inherited: inherited,
					Blocks: []transcript.Block{transcript.TextOf(text)}})
			} else {
				w.Add(transcript.Message{ID: p.ID, Role: transcript.System, At: at, Inherited: inherited,
					Blocks: []transcript.Block{transcript.TextOf("[" + p.Author + " → " + p.Recipient + "]\n" + text)}})
			}
		case "reasoning":
			// The summary entries, then the content when it is not encrypted; a row with
			// only encrypted content shows a reader nothing
			if text := reasoningText(w.Unread, p.Summary, p.Content); text != "" {
				assistant(transcript.ThinkingOf(text))
			}
		case "function_call":
			name := kit.First(p.Name, "unknown")
			if p.Namespace != "" {
				name = p.Namespace + "." + name
			}
			names[callID] = name
			assistant(transcript.CallOf(callID, name, arguments(p.Arguments)))
		case "custom_tool_call":
			// The freeform tools take one string: a script for exec, a patch for apply_patch
			name := kit.First(p.Name, "unknown")
			names[callID] = name
			assistant(transcript.CallOf(callID, name, map[string]any{"input": contentString(p.Input)}))
		case "local_shell_call":
			names[callID] = "local_shell"
			assistant(transcript.CallOf(callID, "local_shell", p.Action))
		case "web_search_call":
			// No call id and no output row: the call is the whole record, so it carries its
			// own (empty) result, with the status the row reports
			outcome := transcript.Outcome{}
			if p.Status == "completed" {
				outcome.Status = model.StatusOK
			}
			assistant(transcript.CallOf("", "web_search", p.Action), transcript.ResultOf("", "web_search", "", outcome))
		case "function_call_output", "custom_tool_call_output":
			flush(nil)
			text, outcome := output(p.Output, w.Unread)
			if names[p.CallID] == "apply_patch" && patchRejected(text) {
				// A rejected patch has no exit code and would otherwise read as applied
				outcome.Status = model.StatusError
			}
			w.Add(transcript.Message{ID: p.ID, Role: transcript.ToolRole, At: at, Inherited: inherited,
				Blocks: []transcript.Block{transcript.ResultOf(p.CallID, names[p.CallID], text, outcome)}})
		case "additional_tools", "configuration_update":
			// Tools put before the model mid-session, and its settings changed: the CLI's
			// setup, not conversation
		case "ghost_snapshot":
			// The git snapshot older versions took for /undo: nothing a reader is shown
		case "compaction":
			// What a compaction on the server left, encrypted: the compacted row is what
			// says it happened
		default:
			if p.Type != "" {
				w.Unread("item:" + p.Type)
			}
		}
		return true
	})
	flush(nil)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	w.S.ID = sessionIDOf(ownID, sessionID, salvaged, hasMeta, forkedOf != "" || parentID != "", stem(path))
	w.S.Model = turnModel
	// A subagent's rollout says so itself, and names the thread that spawned it. A fork
	// or a resumed session a person carries on is no one's subagent: it stays a session
	// of its own, its copied turns marked as its parent's.
	if subagent {
		parent := kit.First(parentID, forkedOf)
		if parent == "" && sessionID != ownID {
			parent = sessionID
		}
		if parent != "" {
			w.S.Parent = &model.Parent{Session: parent, Type: kit.First(strings.TrimPrefix(agentPath, "/root/"), agentPath)}
		}
	}
	return nil
}

// sessionIDOf is the id of the session a rollout holds.
//
// A child's rollout says which session it came from: session_id there is the parent's
// id, and the file's own is in id — on a real install id equalled the UUID in the
// filename on every subagent file, while a normal rollout carries the same value in
// both. Reading session_id first lists every subagent as its parent. Only a
// session_meta id counts; with no session_meta at all a salvaged session_id, or the
// filename, has to do.
func sessionIDOf(own, session, salvaged string, hasMeta, child bool, stem string) string {
	if !hasMeta {
		return kit.First(salvaged, stem)
	}
	if own != "" {
		return own
	}
	if child {
		return stem // session_id names the parent here: the filename is all that is this file's
	}
	return kit.First(session, stem)
}

// parentTurns is the turn ids of the rollout a fork was copied from: nil when it is not
// on disk, in which case nothing is taken for copied.
func (s *Source) parentTurns(id string) map[string]bool {
	path := ""
	files, _ := s.files() // a parent that cannot be found is a fork with nothing marked copied
	for _, file := range files {
		if strings.HasSuffix(stem(file), "-"+id) {
			path = file
			break
		}
	}
	if path == "" {
		return nil
	}
	stamp, _ := kit.Stamp(path)
	s.mu.Lock()
	if entry, ok := s.turns[path]; ok && entry.stamp == stamp {
		s.mu.Unlock()
		return entry.ids
	}
	s.mu.Unlock()

	ids := map[string]bool{}
	_ = kit.Lines(path, func(line []byte) bool {
		if !bytes.Contains(line, []byte(`"turn_context"`)) {
			return true
		}
		var r row
		if json.Unmarshal(line, &r) == nil && r.Type == "turn_context" && r.Payload.TurnID != "" {
			ids[r.Payload.TurnID] = true
		}
		return true
	})
	s.mu.Lock()
	if s.turns == nil {
		s.turns = map[string]turnIDs{}
	}
	s.turns[path] = turnIDs{stamp: stamp, ids: ids}
	s.mu.Unlock()
	return ids
}

// part is one entry of a content list: [{type, text}].
type part struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

// text is what an entry shows a reader. The kinds known to hold no text have a case of
// their own; an entry of a kind never seen, with no text to read, is passed over and
// counted as unread. One of a kind never seen that does hold text is read like any other.
func (p part) text(unread func(kind string)) string {
	switch p.Type {
	case "input_image", "image", "input_audio":
		// a picture or a sound: nothing a reader of the words is shown
		return ""
	case "encrypted_content":
		// what Codex keeps encrypted, such as one agent's message to another
		return ""
	}
	if p.Text != nil {
		return *p.Text
	}
	switch p.Type {
	case "", "input_text", "output_text", "summary_text", "reasoning_text", "text":
		// a kind that holds text, with none in it this time
	default:
		unread("part:" + p.Type)
	}
	return ""
}

// messageText joins the text entries of a message's content.
func messageText(raw json.RawMessage, unread func(string)) string {
	var items []part
	if json.Unmarshal(raw, &items) != nil {
		return contentString(raw)
	}
	var texts []string
	for _, item := range items {
		if text := item.text(unread); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// contentString is a JSON string's value, or "" when the value is not a string.
func contentString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// reasoningText joins what a reasoning row shows. Either list is [{type, text}].
func reasoningText(unread func(string), lists ...json.RawMessage) string {
	var texts []string
	for _, raw := range lists {
		var items []part
		if json.Unmarshal(raw, &items) != nil {
			continue
		}
		for _, item := range items {
			if text := strings.TrimSpace(item.text(unread)); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n\n")
}

// arguments decodes a function call's arguments, which Codex writes as a JSON string.
// One that will not decode is kept whole rather than dropped.
func arguments(raw json.RawMessage) any {
	if s := contentString(raw); s != "" {
		return s // the writer reads a string holding an object as that object
	}
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `""` {
		return nil
	}
	return raw
}

// output reads a tool output: the text a reader wants, and what its header said about
// how the command ended.
//
// Two shapes, both seen in real rollouts. The older shell tool wrote the output as a
// JSON document, {"output": "...", "metadata": {"exit_code": 0, "duration_seconds":
// 0.1}}. Newer tools write plain text, or a list of {type, text} items, that opens with
// a header of known lines — "Exit code: N", "Wall time: 0.3 seconds", "Script completed",
// "Process exited with code N" — closed by a line reading "Output:". The header is
// information, not output, so it is read and removed.
func output(raw json.RawMessage, unread func(string)) (string, transcript.Outcome) {
	ok := transcript.Outcome{Status: model.StatusOK}
	if text := contentString(raw); text != "" || bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) {
		if body, outcome, legacy := legacyOutput(text); legacy {
			return body, outcome
		}
		return stripHeader(text)
	}
	var items []part
	if json.Unmarshal(raw, &items) != nil {
		return "", ok
	}
	var outcome transcript.Outcome
	var bodies []string
	for _, item := range items {
		text := item.text(unread)
		if text == "" {
			continue
		}
		body, found := stripHeader(text)
		if outcome.Exit == nil {
			outcome.Exit = found.Exit
		}
		if outcome.DurMs == 0 {
			outcome.DurMs = found.DurMs
		}
		if found.Status == model.StatusError {
			outcome.Status = model.StatusError
		}
		if body != "" {
			bodies = append(bodies, body)
		}
	}
	if outcome.Status == "" {
		outcome.Status = model.StatusOK
	}
	return strings.Join(bodies, ""), outcome
}

// legacyOutput decodes the JSON form of a shell output.
func legacyOutput(text string) (string, transcript.Outcome, bool) {
	if !strings.HasPrefix(strings.TrimSpace(text), `{"output"`) {
		return "", transcript.Outcome{}, false
	}
	var legacy struct {
		Output   string `json:"output"`
		Metadata *struct {
			ExitCode        *int     `json:"exit_code"`
			DurationSeconds *float64 `json:"duration_seconds"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(text), &legacy) != nil {
		return "", transcript.Outcome{}, false
	}
	outcome := transcript.Outcome{Status: model.StatusOK}
	if m := legacy.Metadata; m != nil {
		if m.ExitCode != nil {
			outcome.Exit = m.ExitCode
			if *m.ExitCode != 0 {
				outcome.Status = model.StatusError
			}
		}
		if m.DurationSeconds != nil {
			outcome.DurMs = millis(*m.DurationSeconds)
		}
	}
	return legacy.Output, outcome, true
}

// millis is a duration in seconds as whole milliseconds, or 0 when it is not a duration a
// command could have run for: below zero, not a number, or longer than thirty years — past
// which the conversion, and the time the step ended, would overflow.
func millis(secs float64) int64 {
	if !(secs >= 0 && secs <= 1e9) {
		return 0
	}
	return int64(secs*1000 + 0.5)
}

// stripHeader removes an output header when the text has one. Only a run of known header
// lines closed by an "Output:" line counts; anything else is output and is kept whole,
// header-looking first line included.
func stripHeader(text string) (string, transcript.Outcome) {
	outcome := transcript.Outcome{Status: model.StatusOK}
	rest := text
	for range 8 {
		line, after, more := strings.Cut(rest, "\n")
		line = strings.TrimRight(line, "\r")
		if line == "Output:" {
			return after, outcome
		}
		switch {
		case strings.HasPrefix(line, "Script "):
			status := strings.ToLower(line[len("Script "):])
			if strings.Contains(status, "fail") || strings.Contains(status, "error") {
				outcome.Status = model.StatusError
			}
		case strings.HasPrefix(line, "Wall time"):
			secs := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[len("Wall time"):]), ":"))
			secs = strings.TrimSpace(strings.TrimSuffix(secs, "seconds"))
			if f, err := strconv.ParseFloat(secs, 64); err == nil {
				outcome.DurMs = millis(f)
			}
		case strings.HasPrefix(line, "Process exited with code "), strings.HasPrefix(line, "Exit code: "):
			if code, err := strconv.Atoi(strings.TrimSpace(line[strings.LastIndexByte(line, ' ')+1:])); err == nil {
				outcome.Exit = transcript.ExitOf(code)
				if code != 0 {
					outcome.Status = model.StatusError
				}
			}
		case strings.HasPrefix(line, "Chunk ID:"), strings.HasPrefix(line, "Original token count:"),
			strings.HasPrefix(line, "Process running with session ID"):
			// known header lines that carry nothing worth keeping
		default:
			return text, transcript.Outcome{Status: model.StatusOK}
		}
		if !more {
			break
		}
		rest = after
	}
	return text, transcript.Outcome{Status: model.StatusOK}
}

// patchRejected: apply_patch's output when the patch did not land — the model wrote one
// that fails verification, or the user refused it. There is no exit code, so without
// this the step would read as a successful edit.
func patchRejected(output string) bool {
	t := strings.TrimSpace(output)
	return strings.HasPrefix(t, "apply_patch verification failed") || strings.HasPrefix(t, "patch rejected")
}
