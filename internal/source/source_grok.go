package source

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Grok CLI: ~/.grok/sessions/<url-encoded-cwd>/<session-id>/
//
// A Grok session is a directory, not a file. summary.json is the index entry, and
// updates.jsonl is the conversation: one JSON-RPC notification per line, each carrying an
// Agent Client Protocol session update.
//
// The list is keyed on summary.json rather than on the transcript because that is the
// file Grok keeps current — it holds last_active_at and num_messages — so its mtime and
// size answer "did this session change" for the directory as a whole, which is what
// fileRecordCache needs. The transcript beside it is what Messages, Final and search read.
//
// Keying on it is only safe because Grok writes the two together. Measured on a live
// multi-tool turn, sampling both files every 8 seconds: their mtimes advanced five times
// during the turn and were equal at every sample. So a running session's time is current
// rather than frozen at the last completed turn, which matters because isActive only
// looks back two minutes and a turn with tool calls in it routinely runs longer.
type GrokSource struct {
	root  string
	cache *fileRecordCache
}

func NewGrokSource(root string) *GrokSource {
	return &GrokSource{root: root, cache: newFileRecordCache(grokCountMessages)}
}

func (s *GrokSource) Mode() string     { return "grok" }
func (s *GrokSource) Location() string { return s.root }

func (s *GrokSource) Exists() bool { return fileExists(s.root) }

// files lists the session index files: <root>/<group>/<session-id>/summary.json, plus
// the same layout under the archived_sessions directory beside the root, where Grok
// moves a session the user archives. An archived session is still history — the list
// carries it with archived:true rather than losing it.
func (s *GrokSource) files() []string {
	files, err := filepath.Glob(filepath.Join(s.root, "*", "*", "summary.json"))
	if err != nil {
		return nil
	}
	if archived := s.archivedRoot(); archived != "" {
		more, _ := filepath.Glob(filepath.Join(archived, "*", "*", "summary.json"))
		files = append(files, more...)
	}
	return files
}

// archivedRoot is ~/.grok/archived_sessions when the root is the default sessions
// directory; a relocated root (--path grok=/x) has no sibling to look beside.
func (s *GrokSource) archivedRoot() string {
	if filepath.Base(s.root) != "sessions" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.root), "archived_sessions")
}

// grokUpdatesPath is the conversation log beside a session's summary.json.
func grokUpdatesPath(summaryPath string) string {
	return filepath.Join(filepath.Dir(summaryPath), "updates.jsonl")
}

// readJSONObject reads one small whole-file JSON object. Only the per-session state files
// go through here; a conversation is never read this way.
func readJSONObject(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// grokCwd is the session's working directory.
//
// summary.json states it outright; the fallbacks exist because the group directory is
// named after that same path, URL-encoded, and Grok swaps the encoded name for a slug
// plus a hash once it passes 255 bytes, recording the real path in a .cwd file beside it.
//
// Decoding uses PathUnescape, not QueryUnescape: the latter also turns "+" into a space,
// which would corrupt every working directory with a plus in its name.
func grokCwd(info map[string]any, groupDir string) string {
	if cwd := StrOr(info["cwd"], ""); cwd != "" {
		return cwd
	}
	if raw, err := os.ReadFile(filepath.Join(groupDir, ".cwd")); err == nil {
		if cwd := strings.TrimSpace(string(raw)); cwd != "" {
			return cwd
		}
	}
	if decoded, err := url.PathUnescape(filepath.Base(groupDir)); err == nil {
		return decoded
	}
	return ""
}

// List reads each session's summary.json, which is small and complete; unchanged sessions
// come straight from the cache (see fileRecordCache).
func (s *GrokSource) List() []Record {
	return s.cache.records(s.files(), func(path, modISO string) Record {
		dir := filepath.Dir(path)
		meta := readJSONObject(path)
		info := getMap(meta, "info")
		sid := StrOr(info["id"], filepath.Base(dir))
		updates := grokUpdatesPath(path)
		// last_active_at tracks the conversation, updated_at the file; they agree in the
		// normal case and last_active_at is the one that stays true when something
		// rewrites the summary without a turn having happened.
		updated := FirstNonEmpty(
			ToStr(meta["last_active_at"]),
			ToStr(meta["updated_at"]),
			ToStr(meta["created_at"]),
			modISO,
		)
		// Grok titles a session itself, so prefer its title over the opening prompt and
		// fall back through the same path the other sources take.
		title := FirstNonEmpty(
			TitleFromUserText(ToStr(meta["generated_title"])),
			TitleFromUserText(ToStr(meta["session_summary"])),
			grokUserTitle(updates),
			sid,
		)
		rec := Record{
			Source:    "grok",
			Key:       dir,
			ShortKey:  title,
			SessionID: sid,
			File:      updates,
			HasFile:   fileExists(updates),
			Status:    "done",
			Cwd:       grokCwd(info, filepath.Dir(dir)),
			Model:     StrOr(meta["current_model_id"], ""),
			UpdatedAt: updated,
		}
		if archived := s.archivedRoot(); archived != "" && strings.HasPrefix(path, archived+string(filepath.Separator)) {
			rec.Archived = true
		}
		return NewRecord(rec, updated)
	})
}

// grokHeadLines caps how far the title scan reads, matching the other file sources. The
// first prompt sits within the first few updates; session_start hook rows precede it.
const grokHeadLines = 50

// grokUserTitle is the first real user message of a session, as a title. It is only
// reached when Grok has not generated one of its own yet.
// It cannot use firstUserTitle, which takes the first row that yields text: a Grok prompt
// arrives in chunks, so that would title the session with however much of the first
// sentence happened to land in chunk one. The chunks are joined first, the same way the
// grouper joins them, and only then turned into a title.
func grokUserTitle(path string) string {
	prompt := ""
	seen := 0
	eachGrokUpdate(path, func(u grokUpdate) bool {
		seen++
		if grokRole(u) == "user" {
			prompt += ContentText(u.body["content"])
			return seen < grokHeadLines
		}
		// The prompt is over once anyone else speaks, and until it starts there is
		// nothing to add
		return prompt == "" && seen < grokHeadLines
	})
	return TitleFromUserText(prompt)
}

// grokUpdate is one line of updates.jsonl, reduced to the parts a transcript needs.
type grokUpdate struct {
	timestamp any            // epoch seconds, as written
	method    string         // session/update, or _x.ai/session/update for xAI's own events
	kind      string         // the sessionUpdate discriminator
	body      map[string]any // the update object itself
}

// eachGrokUpdate walks updates.jsonl.
//
// Every line is {timestamp, method, params}, with the update under params.update. Two
// methods share the file: session/update is the Agent Client Protocol stream, and
// _x.ai/session/update is Grok's own (hook runs, per-turn accounting). Both are yielded,
// because the transcript comes from the first and the usage total from the second.
func eachGrokUpdate(path string, fn func(u grokUpdate) bool) {
	eachJSONL(path, func(obj map[string]any) bool {
		body := getMap(getMap(obj, "params"), "update")
		if len(body) == 0 {
			return true
		}
		return fn(grokUpdate{
			timestamp: obj["timestamp"],
			method:    ToStr(obj["method"]),
			kind:      ToStr(body["sessionUpdate"]),
			body:      body,
		})
	})
}

// grokTimestamp renders a line's time the way every other source spells one. Grok writes
// epoch seconds, which parseTimestampValue would accept as-is, but messages go straight
// out to clients and a bare integer there would not be the field the other sources return.
func GrokTimestamp(v any) string {
	sec, ok := ToFloat(v)
	if !ok {
		return ""
	}
	_, iso, ok := utcFromSeconds(sec)
	if !ok {
		return ""
	}
	return iso
}

// grokRole is the speaker an update belongs to. An empty role means the update carries no
// transcript content: hook runs, plan changes, mode switches, turn accounting.
func grokRole(u grokUpdate) string {
	if u.method != "session/update" {
		return ""
	}
	switch u.kind {
	case "user_message_chunk":
		return "user"
	case "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update":
		return "assistant"
	}
	return ""
}

// grokHitRole is the speaker of one raw updates.jsonl line.
//
// The generic search path scans lines, not the messages the grouper folds them into, so it
// meets Grok's JSON-RPC envelope rather than a row with a role on it. Without this every
// hit came back with an empty role, which quietly removed the whole source from any
// role-filtered search — including find_decisions and find_similar_question, which ask for
// assistant hits and so could never surface a Grok session at all.
//
// It answers through grokRole so the search path and the transcript cannot drift apart.
func GrokHitRole(obj map[string]any) string {
	body := getMap(getMap(obj, "params"), "update")
	if len(body) == 0 {
		return ""
	}
	return grokRole(grokUpdate{method: ToStr(obj["method"]), kind: ToStr(body["sessionUpdate"])})
}

// grokToolName is the tool's own name. Grok keeps it in its _meta extension; the title
// field starts out as that same name but is rewritten into a human sentence once the call
// is under way, so _meta is the stable one.
func grokToolName(body map[string]any) string {
	if name := StrOr(getMap(getMap(body, "_meta"), "x.ai/tool")["name"], ""); name != "" {
		return name
	}
	return StrOr(body["title"], "")
}

// grokToolResultText pulls readable output off a finished tool call. The Agent Client
// Protocol content array is the presentable form, each entry wrapping a block as
// {"type":"content","content":{...}}; the rawOutput beside it is the tool's own struct,
// whose shape differs per tool and is not worth rendering.
func grokToolResultText(body map[string]any) string {
	texts := []string{}
	for _, item := range getSlice(body, "content") {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		inner := entry["content"]
		if inner == nil {
			inner = entry
		}
		if text := ContentText(inner); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// grokGrouper folds the update stream into messages.
//
// Grok streams a turn as many small updates: text arrives in chunks, and a tool call
// opens on one line then completes on a later one. One message per update would turn a
// single reply into dozens of one-word rows, so consecutive updates from the same speaker
// become one message and consecutive text chunks inside it become one block. A message is
// emitted when the speaker changes or the stream ends.
type grokGrouper struct {
	emit    func(m map[string]any) bool
	full    bool
	role    string
	at      any
	parts   []map[string]any
	names   map[string]string // toolCallId -> tool name, so a result can be labelled by its call
	count   int
	stopped bool
}

func newGrokGrouper(emit func(m map[string]any) bool, full bool) *grokGrouper {
	return &grokGrouper{emit: emit, full: full, names: map[string]string{}}
}

// add takes one update; false means the consumer has enough and the scan may stop.
func (g *grokGrouper) add(u grokUpdate) bool {
	if g.stopped {
		return false
	}
	role := grokRole(u)
	if role == "" {
		return true
	}
	if role != g.role {
		if !g.flush() {
			return false
		}
		g.role, g.at = role, u.timestamp
	}
	g.append(u)
	return true
}

// append folds one update into the message being built.
func (g *grokGrouper) append(u grokUpdate) {
	switch u.kind {
	case "user_message_chunk", "agent_message_chunk":
		g.text("text", ContentText(u.body["content"]))
	case "agent_thought_chunk":
		g.text("thinking", ContentText(u.body["content"]))
	case "tool_call":
		name := grokToolName(u.body)
		id := ToStr(u.body["toolCallId"])
		if id != "" && name != "" {
			g.names[id] = name
		}
		g.parts = append(g.parts, ToolCallBlock(id, name, GetOr(u.body, "rawInput", map[string]any{})))
	case "tool_call_update":
		// An update with no status is the call being re-titled while it runs. Only a
		// finished one carries output, and only that is worth a block of its own; the
		// status it finished with is the one bit Grok records about how it went.
		status := ToStr(u.body["status"])
		switch status {
		case "completed", "failed":
		default:
			return
		}
		id := ToStr(u.body["toolCallId"])
		block := ToolResultBlock(id, g.names[id], grokToolResultText(u.body), g.full)
		g.parts = append(g.parts, ToolOutcome{Status: statusFromError(status == "failed")}.Apply(block))
	}
}

// text appends text, merging into the trailing block of the same kind: streamed text
// arrives a chunk at a time and a reader wants the sentence, not the chunks.
func (g *grokGrouper) text(kind, content string) {
	if content == "" {
		return
	}
	if n := len(g.parts); n > 0 && g.parts[n-1]["type"] == kind {
		g.parts[n-1]["content"] = ToStr(g.parts[n-1]["content"]) + content
		return
	}
	g.parts = append(g.parts, map[string]any{"type": kind, "content": content})
}

// flush emits the message being built, if there is one. False means the consumer has
// enough; every later call is then a no-op.
func (g *grokGrouper) flush() bool {
	if g.stopped {
		return false
	}
	if g.role == "" || len(g.parts) == 0 {
		g.reset()
		return true
	}
	// Thinking is capped here rather than per chunk: the cap belongs to the block a
	// reader sees, and the chunks it was streamed in are an accident of transport.
	for i, p := range g.parts {
		if p["type"] == "thinking" {
			g.parts[i] = ThinkingBlock(ToStr(p["content"]), g.full)
		}
	}
	message := map[string]any{
		// The ordinal is the session's own numbering, stable because the log is only ever
		// appended to; a Grok message has no id of its own to use instead.
		"id":        strconv.Itoa(g.count),
		"role":      g.role,
		"timestamp": GrokTimestamp(g.at),
		"content":   g.parts,
	}
	g.count++
	ok := g.emit(message)
	g.reset()
	if !ok {
		g.stopped = true
	}
	return ok
}

func (g *grokGrouper) reset() {
	g.role, g.at, g.parts = "", nil, nil
}

func (s *GrokSource) Messages(r Record, q MessageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := r.File
	if path == "" {
		return sink.result()
	}
	g := newGrokGrouper(sink.add, q.Full)
	eachGrokUpdate(path, g.add)
	g.flush()
	return sink.result()
}

// grokCountMessages counts a session's messages by the same grouping Messages uses, so the
// two cannot disagree about what a message is.
//
// They can still disagree about when. The count is cached against summary.json while it
// reads updates.jsonl, so a transcript that grew without its summary being rewritten keeps
// the old number until the summary moves. Measured on a live turn the two advance together,
// which is what makes the key sound in practice; a crash between the two writes, or another
// tool appending, would leave the list one turn behind until the next write.
//
// It is handed the summary.json path because that is what the list is keyed on. Unlike
// the other counters it cannot pre-filter on raw bytes: grouping is stateful across
// lines, so every update has to be decoded in order.
func grokCountMessages(summaryPath string, from int64) (int, bool) {
	if from > 0 {
		// The grouper joins several update rows into one message, so its state spans
		// lines: resuming at an offset could split one group across the seam and count
		// it twice (or not at all). Recount the file and say so, rather than resume
		// with a number that disagrees with Final.
		n, _ := grokCountMessages(summaryPath, 0)
		return n, false
	}
	n := 0
	g := newGrokGrouper(func(map[string]any) bool { n++; return true }, false)
	eachGrokUpdate(grokUpdatesPath(summaryPath), g.add)
	g.flush()
	return n, true
}

func (s *GrokSource) Final(r Record) map[string]any {
	path := r.File
	// A session that has been created but never prompted has a summary.json and no
	// transcript at all; that is the "no session file yet" the interface means.
	if path == "" || !fileExists(path) {
		return nil
	}

	count := 0
	var last map[string]any
	// Usage is summed over the session rather than taken from the closing turn: the last
	// turn answers what the final reply cost, which is not what a usage total reads as
	// (see source_claude).
	var totals usageTotals
	stopReason := ""
	g := newGrokGrouper(func(m map[string]any) bool {
		count++
		if m["role"] == "assistant" {
			last = m
		}
		return true
	}, false)
	eachGrokUpdate(path, func(u grokUpdate) bool {
		// turn_completed is Grok's own event, so it never reaches the transcript grouping;
		// it is where the per-turn token totals and the stop reason live.
		if u.kind == "turn_completed" {
			totals.add(getMap(u.body, "usage"))
			if reason := StrOr(u.body["stop_reason"], ""); reason != "" {
				stopReason = reason
			}
		}
		return g.add(u)
	})
	g.flush()

	model := r.Model
	if model == "" {
		model = StrOr(readJSONObject(filepath.Join(filepath.Dir(path), "summary.json"))["current_model_id"], "")
	}
	if last == nil {
		return map[string]any{
			"status":       "done",
			"isFinal":      false,
			"isProcessing": false,
			"messageCount": count,
			"source":       "grok",
			"text":         "",
			"thinking":     "",
		}
	}

	texts := []string{}
	thoughts := []string{}
	toolCalls := []any{}
	parts, _ := last["content"].([]map[string]any)
	for _, p := range parts {
		switch p["type"] {
		case "text":
			if c := strField(p, "content"); c != "" {
				texts = append(texts, c)
			}
		case "thinking":
			if c := strField(p, "content"); c != "" {
				thoughts = append(thoughts, c)
			}
		case "toolCall":
			toolCalls = append(toolCalls, map[string]any{
				"name":      strField(p, "name"),
				"arguments": GetOr(p, "arguments", map[string]any{}),
			})
		}
	}
	return map[string]any{
		"status":       "done",
		"isFinal":      stopReason == "end_turn" || stopReason == "stop",
		"isProcessing": false,
		"messageCount": count,
		"source":       "grok",
		"id":           GetOr(last, "id", ""),
		"timestamp":    GetOr(last, "timestamp", ""),
		"stopReason":   stopReason,
		"model":        model,
		"text":         strings.Join(texts, "\n"),
		"thinking":     strings.Join(thoughts, "\n"),
		"toolCalls":    toolCalls,
		"usage":        totals.result(),
	}
}
