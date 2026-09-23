package app

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

func newGrokSource(root string) *GrokSource {
	return &GrokSource{root: root, cache: newFileRecordCache(grokCountMessages)}
}

func (s *GrokSource) Mode() string     { return "grok" }
func (s *GrokSource) Location() string { return s.root }

func (s *GrokSource) Exists() bool { return fileExists(s.root) }

// files lists the session index files: <root>/<group>/<session-id>/summary.json.
func (s *GrokSource) files() []string {
	files, err := filepath.Glob(filepath.Join(s.root, "*", "*", "summary.json"))
	if err != nil {
		return nil
	}
	return files
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
	if cwd := strOr(info["cwd"], ""); cwd != "" {
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
func (s *GrokSource) List() []record {
	return s.cache.records(s.files(), func(path, modISO string) record {
		dir := filepath.Dir(path)
		meta := readJSONObject(path)
		info := getMap(meta, "info")
		sid := strOr(info["id"], filepath.Base(dir))
		updates := grokUpdatesPath(path)
		// last_active_at tracks the conversation, updated_at the file; they agree in the
		// normal case and last_active_at is the one that stays true when something
		// rewrites the summary without a turn having happened.
		updated := firstNonEmpty(
			toStr(meta["last_active_at"]),
			toStr(meta["updated_at"]),
			toStr(meta["created_at"]),
			modISO,
		)
		// Grok titles a session itself, so prefer its title over the opening prompt and
		// fall back through the same path the other sources take.
		title := firstNonEmpty(
			titleFromUserText(toStr(meta["generated_title"])),
			titleFromUserText(toStr(meta["session_summary"])),
			grokUserTitle(updates),
			sid,
		)
		return newRecord(map[string]any{
			"source":    "grok",
			"key":       dir,
			"shortKey":  title,
			"sessionId": sid,
			"file":      updates,
			"hasFile":   fileExists(updates),
			"status":    "done",
			"cwd":       grokCwd(info, filepath.Dir(dir)),
			"model":     strOr(meta["current_model_id"], ""),
			"updatedAt": updated,
		}, updated)
	})
}

// grokHeadLines caps how far the title scan reads, matching the other file sources. The
// first prompt sits within the first few updates; session_start hook rows precede it.
const grokHeadLines = 50

// grokUserTitle is the first real user message of a session, as a title. It is only
// reached when Grok has not generated one of its own yet.
func grokUserTitle(path string) string {
	return firstUserTitle(path, grokHeadLines, func(obj map[string]any) (string, bool) {
		update := getMap(getMap(obj, "params"), "update")
		if toStr(update["sessionUpdate"]) != "user_message_chunk" {
			return "", false
		}
		return contentText(update["content"]), true
	})
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
			method:    toStr(obj["method"]),
			kind:      toStr(body["sessionUpdate"]),
			body:      body,
		})
	})
}

// grokTimestamp renders a line's time the way every other source spells one. Grok writes
// epoch seconds, which parseTimestampValue would accept as-is, but messages go straight
// out to clients and a bare integer there would not be the field the other sources return.
func grokTimestamp(v any) string {
	sec, ok := toFloat(v)
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

// grokToolName is the tool's own name. Grok keeps it in its _meta extension; the title
// field starts out as that same name but is rewritten into a human sentence once the call
// is under way, so _meta is the stable one.
func grokToolName(body map[string]any) string {
	if name := strOr(getMap(getMap(body, "_meta"), "x.ai/tool")["name"], ""); name != "" {
		return name
	}
	return strOr(body["title"], "")
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
		if text := contentText(inner); text != "" {
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
	role    string
	at      any
	parts   []map[string]any
	names   map[string]string // toolCallId -> tool name, so a result can be labelled by its call
	count   int
	stopped bool
}

func newGrokGrouper(emit func(m map[string]any) bool) *grokGrouper {
	return &grokGrouper{emit: emit, names: map[string]string{}}
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
		g.text("text", contentText(u.body["content"]))
	case "agent_thought_chunk":
		g.text("thinking", contentText(u.body["content"]))
	case "tool_call":
		name := grokToolName(u.body)
		if id := toStr(u.body["toolCallId"]); id != "" && name != "" {
			g.names[id] = name
		}
		g.parts = append(g.parts, map[string]any{
			"type":      "toolCall",
			"name":      name,
			"arguments": getOr(u.body, "rawInput", map[string]any{}),
		})
	case "tool_call_update":
		// An update with no status is the call being re-titled while it runs. Only a
		// finished one carries output, and only that is worth a block of its own.
		switch toStr(u.body["status"]) {
		case "completed", "failed":
		default:
			return
		}
		g.parts = append(g.parts, map[string]any{
			"type":     "toolResult",
			"toolName": g.names[toStr(u.body["toolCallId"])],
			"content":  truncate(grokToolResultText(u.body), 500, "...[truncated]"),
		})
	}
}

// text appends text, merging into the trailing block of the same kind: streamed text
// arrives a chunk at a time and a reader wants the sentence, not the chunks.
func (g *grokGrouper) text(kind, content string) {
	if content == "" {
		return
	}
	if n := len(g.parts); n > 0 && g.parts[n-1]["type"] == kind {
		g.parts[n-1]["content"] = toStr(g.parts[n-1]["content"]) + content
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
	for _, p := range g.parts {
		if p["type"] == "thinking" {
			p["content"] = truncate(toStr(p["content"]), 1000, "...[truncated]")
		}
	}
	message := map[string]any{
		// The ordinal is the session's own numbering, stable because the log is only ever
		// appended to; a Grok message has no id of its own to use instead.
		"id":        strconv.Itoa(g.count),
		"role":      g.role,
		"timestamp": grokTimestamp(g.at),
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

func (s *GrokSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := r.str("file")
	if path == "" {
		return sink.result()
	}
	g := newGrokGrouper(sink.add)
	eachGrokUpdate(path, g.add)
	g.flush()
	return sink.result()
}

// grokCountMessages counts a session's messages by the same grouping Messages uses, so
// the list and the opened session never report different numbers.
//
// It is handed the summary.json path because that is what the list is keyed on. Unlike
// the other counters it cannot pre-filter on raw bytes: grouping is stateful across
// lines, so every update has to be decoded in order.
func grokCountMessages(summaryPath string) int {
	n := 0
	g := newGrokGrouper(func(map[string]any) bool { n++; return true })
	eachGrokUpdate(grokUpdatesPath(summaryPath), g.add)
	g.flush()
	return n
}

func (s *GrokSource) Final(r record) map[string]any {
	path := r.str("file")
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
	})
	eachGrokUpdate(path, func(u grokUpdate) bool {
		// turn_completed is Grok's own event, so it never reaches the transcript grouping;
		// it is where the per-turn token totals and the stop reason live.
		if u.kind == "turn_completed" {
			totals.add(getMap(u.body, "usage"))
			if reason := strOr(u.body["stop_reason"], ""); reason != "" {
				stopReason = reason
			}
		}
		return g.add(u)
	})
	g.flush()

	model := r.str("model")
	if model == "" {
		model = strOr(readJSONObject(filepath.Join(filepath.Dir(path), "summary.json"))["current_model_id"], "")
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
				"arguments": getOr(p, "arguments", map[string]any{}),
			})
		}
	}
	return map[string]any{
		"status":       "done",
		"isFinal":      stopReason == "end_turn" || stopReason == "stop",
		"isProcessing": false,
		"messageCount": count,
		"source":       "grok",
		"id":           getOr(last, "id", ""),
		"timestamp":    getOr(last, "timestamp", ""),
		"stopReason":   stopReason,
		"model":        model,
		"text":         strings.Join(texts, "\n"),
		"thinking":     strings.Join(thoughts, "\n"),
		"toolCalls":    toolCalls,
		"usage":        totals.result(),
	}
}
