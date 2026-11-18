// Package codex reads Codex's sessions.
//
//	<root>/<year>/<month>/<day>/rollout-<time>-<id>.jsonl[.zst]   a session ("rollout")
//	<root>/../archived_sessions/…                                 the ones Codex put away
//	<root>/../session_index.jsonl                                 the names threads were given
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
//	response_item/tool_search_call          a search for tools to load, and its _output
//	response_item/web_search_call           a search, which has no output row
//	response_item/image_generation_call     an image made, which has none either
//	response_item/*_call_output             a result, by call_id
//	response_item/agent_message             one agent of a team speaking to another
//	response_item/context_compaction        a compaction the API made inside a response
//	token_usage_record                      what the model response just written cost
//	event_msg/turn_aborted, compacted       what happened between the words
//	event_msg/thread_rolled_back            turns taken back out of the thread
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/jsonmap"
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
	Reads:   2,
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
	// The names threads were given, read once per version of session_index.jsonl
	names threadNames
}

type turnIDs struct {
	stamp string
	ids   map[string]bool
}

type threadNames struct {
	read  bool
	stamp string
	byID  map[string]string
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

// threadOf is the id of the thread a rollout is named for: rollout-<time>-<thread>.jsonl,
// and rollout-<time>-<thread>_<rollout>.jsonl for a later rollout of the same thread
// (codex-rs rollout/src/rollout_file_name.rs). "" for a name of another shape.
func threadOf(path string) string {
	rest, ok := strings.CutPrefix(stem(path), "rollout-")
	if !ok || len(rest) < 21 || rest[19] != '-' {
		return ""
	}
	id, _, _ := strings.Cut(rest[20:], "_")
	return id
}

// threadNames is the name of each thread that has one, by thread id. Codex appends a
// line to $CODEX_HOME/session_index.jsonl whenever a thread is named (/rename, or a
// client's thread/name/set) and never writes the name into the rollout. The newest line
// for a thread is its name, and a blank one — the name cleared — leaves it unnamed, as
// Codex reads it for a thread (codex-rs rollout/src/session_index.rs, find_thread_name_by_id).
// The file is read once per version of it.
func (s *Source) threadNames() map[string]string {
	if filepath.Base(s.root) != "sessions" {
		return nil // the index is beside the default sessions directory, in CODEX_HOME
	}
	path := filepath.Join(filepath.Dir(s.root), "session_index.jsonl")
	stamp, _ := kit.Stamp(path)
	s.mu.Lock()
	if s.names.read && s.names.stamp == stamp {
		names := s.names.byID
		s.mu.Unlock()
		return names
	}
	s.mu.Unlock()

	names := map[string]string{}
	err := kit.Lines(path, func(line []byte) bool {
		var entry struct {
			ID   string  `json:"id"`
			Name *string `json:"thread_name"`
		}
		// A line Codex cannot parse either — a name that is not a string, the end of a
		// line still being written — it passes over for the one before
		if json.Unmarshal(line, &entry) == nil && entry.ID != "" && entry.Name != nil {
			names[entry.ID] = strings.TrimSpace(*entry.Name)
		}
		return true
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return names // read again next time
	}
	s.mu.Lock()
	s.names = threadNames{read: true, stamp: stamp, byID: names}
	s.mu.Unlock()
	return names
}

// Scan lists every rollout. A thread's name is in session_index.jsonl and not in its
// rollout, so it is part of the stamp: a thread renamed is read again.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	files, unread := s.files()
	names := s.threadNames()
	refs := make([]sources.Ref, 0, len(files))
	for _, path := range files {
		stamp, mod := kit.Stamp(path)
		if name := names[threadOf(path)]; name != "" {
			stamp += "|" + jsonmap.StampOf(name)
		}
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
		Execution string          `json:"execution"`
		Tools     json.RawMessage `json:"tools"`
		Revised   string          `json:"revised_prompt"`

		// token_usage_record, event_msg
		ResponseID string          `json:"response_id"`
		Usage      *usage          `json:"usage"`
		Info       json.RawMessage `json:"info"`
		Reason     string          `json:"reason"`
		Message    json.RawMessage `json:"message"`
		NumTurns   json.Number     `json:"num_turns"`
	} `json:"payload"`
}

// usage is Codex's token counters: OpenAI's shape, where the input counts the cached
// tokens and the output counts the reasoning. Each is a count by the one rule for what a
// count is from the moment it is read: see UnmarshalJSON.
type usage struct {
	Input, Cached, CacheWrite, Output, Reasoning int64
}

// UnmarshalJSON reads the counters of a usage object. A number that is not a count —
// below zero, past model.MaxTokens, or too large for a float64 — is 0 and the rest of the
// row is read: a corrupt counter adds nothing, and moves no high mark a later snapshot is
// measured against (see spentAt). A counter that is not a number at all, like a usage
// that is not an object, is a shape the adapter does not know, and fails the row.
func (u *usage) UnmarshalJSON(data []byte) error {
	var counts struct {
		Input      json.Number `json:"input_tokens"`
		Cached     json.Number `json:"cached_input_tokens"`
		CacheWrite json.Number `json:"cache_write_input_tokens"`
		Output     json.Number `json:"output_tokens"`
		Reasoning  json.Number `json:"reasoning_output_tokens"`
	}
	if err := json.Unmarshal(data, &counts); err != nil {
		return err
	}
	*u = usage{Input: kit.Count(counts.Input), Cached: kit.Count(counts.Cached), CacheWrite: kit.Count(counts.CacheWrite),
		Output: kit.Count(counts.Output), Reasoning: kit.Count(counts.Reasoning)}
	return nil
}

// canonical takes the cached tokens back out of the input, so the parts do not overlap.
// The counters are counts already, so none of this can go below zero or wrap.
func (u usage) canonical() *model.Usage {
	return &model.Usage{
		Input:      max(u.Input-u.Cached-u.CacheWrite, 0),
		Output:     u.Output,
		CacheRead:  u.Cached,
		CacheWrite: u.CacheWrite,
		Reasoning:  u.Reasoning,
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
			// A row with a field of another type than the adapter reads it as is passed
			// over whole, and counted, so that a change in the format shows
			if kind := undecodable(line); kind != "" {
				w.Unread(kind)
			}
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
			case "thread_rolled_back":
				// The thread was taken back: its last num_turns user turns were dropped from
				// what the model is given, and the turns after this go on from before them
				// (codex-rs app-server-protocol thread_history.rs, handle_thread_rollback).
				// The rows of the turns taken back stay in the rollout and are read: they
				// were said and done, their tools ran and their tokens were spent. The
				// person takes the thread back between turns, so the last turn's work had
				// ended before it
				flush(nil)
				rollback := transcript.EventOf(eventRollback, rolledBack(p.NumTurns))
				rollback.After = true
				w.Add(transcript.Message{Role: transcript.System, At: at, Inherited: inherited,
					Blocks: []transcript.Block{rollback}})
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
		case "tool_search_call":
			// A search among the tools the model is not given until it asks for them —
			// arguments {query, limit} — run by Codex ("client") or by the API ("server")
			names[callID] = toolSearch
			assistant(transcript.CallOf(callID, toolSearch, arguments(p.Arguments)))
		case "tool_search_output":
			// What the search found: the tools it loaded. A search Codex ran answers after the
			// response that asked for it, as any output does; one the API ran is part of that
			// response, as its call is. A search that failed is written as one that found
			// nothing (codex-rs core/src/tools/parallel.rs, failure_response), so the two
			// cannot be told apart
			result := transcript.ResultOf(p.CallID, toolSearch, toolsFound(p.Tools), itemOutcome(p.Status))
			if p.Execution == "server" {
				assistant(result)
			} else {
				flush(nil)
				w.Add(transcript.Message{ID: p.ID, Role: transcript.ToolRole, At: at, Inherited: inherited,
					Blocks: []transcript.Block{result}})
			}
		case "image_generation_call":
			// An image the API's image tool made inside the response: like a web search, the
			// call is the whole record and carries its own result. The image (result, in
			// base64) is not text a reader is shown; what was asked for is the prompt as the
			// API revised it
			var input any
			if p.Revised != "" {
				input = map[string]any{"revised_prompt": p.Revised}
			}
			assistant(transcript.CallOf(callID, imageGeneration, input),
				transcript.ResultOf(callID, imageGeneration, "", itemOutcome(p.Status)))
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
		case "context_compaction":
			// A compaction the API made of the context as an item of a response, encrypted.
			// Nothing else says it happened: a compaction Codex runs itself writes a compacted
			// row, with its item inside the replacement history, which is not read
			// (codex-rs core/src/session/mod.rs, replace_compacted_history)
			assistant(transcript.EventOf(model.EventCompaction, "Context compacted"))
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
	// The name the thread was given, by the thread the file is named for: the key Scan
	// stamps it under. A thread with none is titled by its first ask
	w.S.Title = s.threadNames()[threadOf(path)]
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

// The names the adapter gives what Codex records without one: the tools the API runs
// inside a response, as Codex names them (codex-rs core/src/tools/router.rs, tool_search),
// and the event a rollback is.
const (
	toolSearch      = "tool_search"
	imageGeneration = "image_generation"
	eventRollback   = "rollback"
)

// rolledBack says what a rollback of n turns did. n is a count Codex writes as an
// unsigned number; anything else is a rollback that did not say of how many turns.
func rolledBack(n json.Number) string {
	const after = ": the turns after this go on from before "
	switch count, err := strconv.ParseUint(n.String(), 10, 32); {
	case err != nil:
		return "Rolled back the last turns" + after + "them"
	case count == 0:
		return "Rolled back no turns"
	case count == 1:
		return "Rolled back the last turn" + after + "it"
	default:
		return "Rolled back the last " + strconv.FormatUint(count, 10) + " turns" + after + "them"
	}
}

// itemOutcome is how a call the API ran inside a response ended, by the status its item
// carries (the Responses API's: in_progress, searching or generating while it runs, then
// completed, failed or incomplete). One that did not end, or was cut off with its
// response, has no outcome recorded.
func itemOutcome(status string) transcript.Outcome {
	switch status {
	case "completed":
		return transcript.Outcome{Status: model.StatusOK}
	case "failed":
		return transcript.Outcome{Status: model.StatusError}
	}
	return transcript.Outcome{}
}

// toolsFound is what a tool search found, one tool a line: a function by its name, and
// a namespace's tools by the name a call gives them, namespace.name — the way a
// function_call is named here. The list holds the tools' whole definitions, of which the
// names are what a reader needs to know what was loaded.
func toolsFound(raw json.RawMessage) string {
	var specs []struct {
		Name  string `json:"name"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if json.Unmarshal(raw, &specs) != nil {
		return ""
	}
	var names []string
	for _, spec := range specs {
		if len(spec.Tools) == 0 {
			if spec.Name != "" {
				names = append(names, spec.Name)
			}
			continue
		}
		for _, tool := range spec.Tools {
			name := tool.Name
			if spec.Name != "" {
				name = spec.Name + "." + name
			}
			names = append(names, name)
		}
	}
	return strings.Join(names, "\n")
}

// undecodable is the kind, for Writer.Unread, of a line that is JSON but will not decode
// as a row: "undecodable:" and the kind it would be counted as if it were unknown —
// row:<type>, or event:<type> and item:<type> for an event_msg's or a response_item's
// payload. Known or not, a record that cannot be decoded is passed over. "" for a line
// that does not say what it is, or is not JSON at all, such as the end of a rollout that
// is still being written.
func undecodable(line []byte) string {
	var envelope struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
		} `json:"payload"`
	}
	// A field of the wrong type is left empty and the rest decoded: a payload that is not
	// an object still leaves the row's type
	err := json.Unmarshal(line, &envelope)
	var mistyped *json.UnmarshalTypeError
	if (err != nil && !errors.As(err, &mistyped)) || envelope.Type == "" {
		return ""
	}
	kind := "row:" + envelope.Type
	if p := envelope.Payload.Type; p != "" {
		switch envelope.Type {
		case "event_msg":
			kind = "event:" + p
		case "response_item":
			kind = "item:" + p
		}
	}
	return "undecodable:" + kind
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
	running := false
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
		switch found.Status {
		case model.StatusError:
			outcome.Status = model.StatusError
		case "":
			running = true // the command runs on: see stripHeader
		}
		if body != "" {
			bodies = append(bodies, body)
		}
	}
	if outcome.Status == "" && !running {
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
		case strings.HasPrefix(line, "Process running with session ID"):
			// The command outlasted the wait and runs on: it has not ended, so how it ended
			// is not recorded — not "ok", which would say it succeeded
			outcome.Status = ""
		case strings.HasPrefix(line, "Chunk ID:"), strings.HasPrefix(line, "Original token count:"):
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
