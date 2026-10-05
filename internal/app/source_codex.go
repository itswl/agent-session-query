package app

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// Codex: ~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl
//
// A rollout is one JSON object per line, each {timestamp, type, payload}. The conversation
// is spread over several row types, and for a long time only one of them was read here —
// the response_item message rows — which left a Codex session looking like two people
// talking with nothing in between: no commands, no files, no thinking, and a brief whose
// tool list was always empty. The rest of the rows are:
//
//	response_item/reasoning                 the model's thinking (summary_text entries; the
//	                                        content itself is usually encrypted and absent)
//	response_item/function_call             a tool call: name, arguments (a JSON string), call_id
//	response_item/custom_tool_call          the same for the freeform tools: exec (a script),
//	                                        apply_patch (a patch text), input rather than arguments
//	response_item/local_shell_call          an older shell call: action.command
//	response_item/web_search_call           a search: action.query
//	response_item/function_call_output      the result, by call_id; output is a string or a
//	response_item/custom_tool_call_output   list of {type, text} items, often opening with a
//	                                        header (exit code, wall time) this strips off
//	event_msg/turn_aborted                  the user stopped the turn
//	token_usage_record / event_msg/token_count   usage, see Final
//	turn_context                            the model and cwd of a turn
//
// Each of those yields one message here, so a Codex transcript has the shape every other
// source's does: calls ride on assistant messages, a result is a role=tool message paired
// with its call by id, an abort is a system event.
type CodexSource struct {
	root  string
	cache *fileRecordCache
}

func newCodexSource(root string) *CodexSource {
	return &CodexSource{root: root, cache: newFileRecordCache(codexCountMessages)}
}

func (s *CodexSource) Mode() string     { return "codex" }
func (s *CodexSource) Location() string { return s.root }

func (s *CodexSource) Exists() bool { return fileExists(s.root) }

func (s *CodexSource) files() []string {
	files, err := filepath.Glob(filepath.Join(s.root, "*", "*", "*", "rollout-*.jsonl"))
	if err != nil {
		return nil
	}
	return files
}

// codexHeadLines caps how far the metadata scan goes (a defensive backstop; normally
// the very first line is the one)
const codexHeadLines = 50

// codexSalvageKeys are the fields that can be picked up piecemeal from other rows when
// there is no session_meta.
//
// Measured against a real rollout: session_meta carries the full set, turn_context only
// cwd, token_usage_record only session_id — they complement each other, so salvaging
// what is available is worth it.
//
// Note there is no bare `id` here. A response_item (message row) payload carries
// id = "msg_...", and picking that up would report a message ID as the session ID —
// exactly the trap Pi fell into.
var codexSalvageKeys = []string{"session_id", "cwd", "cli_version"}

// codexUserTitle is the first real user message of a Codex session, as a title. The
// opening instruction rows are role=user too but assembled by the CLI (AGENTS.md
// instructions, environment context); titleFromUserText rejects them by their openings.
func codexUserTitle(path string) string {
	return firstUserTitle(path, codexHeadLines, func(obj map[string]any) (string, bool) {
		if obj["type"] != "response_item" {
			return "", false
		}
		payload, _ := obj["payload"].(map[string]any)
		if payload == nil || payload["type"] != "message" || payload["role"] != "user" {
			return "", false
		}
		return blockArrayText(payload["content"])
	})
}

// List finds the metadata row; unchanged files come straight from the cache
// (see fileRecordCache)
func (s *CodexSource) List() []record {
	return s.cache.records(s.files(), func(path, modISO string) record {
		// This used to read only the first line, losing everything when that line was not
		// the metadata (truncated file, or a new preamble row upstream). Now: session_meta
		// is the authoritative and complete row, so stop as soon as it appears; only when
		// it never does fall back to salvaging recognisable fields from later rows.
		//
		// The model is not in session_meta at all: it is on the turn_context row that
		// follows the first turn's start, so the scan runs on past the metadata until it
		// has seen one (still inside the head cap).
		payload := map[string]any{}
		model := ""
		hasMeta := false
		seen := 0
		eachJSONL(path, func(obj map[string]any) bool {
			row := getMap(obj, "payload")
			if obj["type"] == "turn_context" && model == "" {
				model = strOr(row["model"], "")
			}
			if obj["type"] == "session_meta" {
				payload = row
				hasMeta = true
			} else if !hasMeta {
				for _, key := range codexSalvageKeys {
					if !truthy(payload[key]) && truthy(row[key]) {
						payload[key] = row[key]
					}
				}
			}
			seen++
			return !(hasMeta && model != "") && seen < codexHeadLines
		})
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		// Use the time on the last record in the file, not the file's mtime (see updatedAtOf)
		updated := updatedAtOf(path, modISO)
		return newRecord(map[string]any{
			"source":     "codex",
			"key":        path,
			"shortKey":   firstNonEmpty(codexUserTitle(path), stem),
			"sessionId":  codexSessionID(payload, hasMeta, stem),
			"file":       path,
			"hasFile":    true,
			"status":     "done",
			"cwd":        getOr(payload, "cwd", ""),
			"model":      model,
			"cliVersion": getOr(payload, "cli_version", ""),
			"updatedAt":  updated,
		}, updated)
	})
}

// codexSessionID is the id of the session a rollout file holds.
//
// A forked — subagent — rollout's first session_meta says which session it was forked
// from: `session_id` there is the *parent's* id, and the file's own is in `id`. Verified
// against every rollout on a real install: `id` equals the UUID in the filename and
// `session_id` equals the parent's, on all seven subagent files, while a normal rollout
// carries the same value in both. Reading `session_id` first therefore listed every
// subagent as its parent, which is what put several rows sharing one sessionId in the
// list — the UI keys sessions by id, so those rows could not be told apart, and the
// duplicates outlived every filter that should have dropped them.
//
// Only a session_meta id counts. A message payload carries `id` as well and that one is a
// message id (see codexSalvageKeys), which is what hasMeta gates. With no session_meta at
// all there is nothing authoritative to read, so a salvaged session_id, or the filename,
// has to do.
func codexSessionID(payload map[string]any, hasMeta bool, stem string) string {
	salvaged := strOr(payload["session_id"], stem)
	if !hasMeta {
		return salvaged
	}
	if own := strOr(payload["id"], ""); own != "" {
		return own
	}
	// Forked with no id of its own: session_id names the parent here, so the filename is
	// the only thing left that belongs to this file
	if truthy(payload["forked_from_id"]) || truthy(payload["parent_thread_id"]) {
		return stem
	}
	return salvaged
}

// ---------------------------------------------------------------------------
// Rows → messages
// ---------------------------------------------------------------------------

// codexInjectedPrefixes open the user-role rows the CLI assembles rather than the person
// types: the AGENTS.md instructions, the environment context, the permissions block. They
// are kept in the transcript (the model did read them) but flagged injected, so the page
// can fold them and a round does not start at one.
var codexInjectedPrefixes = []string{
	"<environment_context", "<user_instructions", "# AGENTS.md", "<permissions instructions",
	"<INSTRUCTIONS", "<turn_aborted",
}

// codexInjected reports whether a user message was assembled by the CLI: it opens with a
// known preamble, or the whole of it is one XML-style element, which is how Codex wraps
// every piece of context it injects and never how a person writes a question.
func codexInjected(text string) bool {
	t := strings.TrimSpace(text)
	for _, prefix := range codexInjectedPrefixes {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return wrappedInTag(t)
}

// wrappedInTag: <name ...>…</name>, the same name at both ends and nothing outside them
func wrappedInTag(t string) bool {
	if len(t) < 5 || t[0] != '<' || t[len(t)-1] != '>' {
		return false
	}
	end := strings.IndexAny(t, " >\n\t")
	if end < 2 {
		return false
	}
	name := t[1:end]
	for _, r := range name {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') {
			return false
		}
	}
	return strings.HasSuffix(t, "</"+name+">")
}

// codexWalker folds rollout rows into messages. names carries call_id → tool name across
// the scan, because an output row names the call it answers and nothing else.
type codexWalker struct {
	names map[string]string
	full  bool
}

func newCodexWalker(full bool) *codexWalker {
	return &codexWalker{names: map[string]string{}, full: full}
}

// message turns one row into a message, or nil when the row carries no conversation
// (session_meta, turn_context, usage, the task bookkeeping events).
func (w *codexWalker) message(obj map[string]any) map[string]any {
	payload := getMap(obj, "payload")
	kind := toStr(payload["type"])
	timestamp := getOr(obj, "timestamp", "")
	assistant := func(blocks ...map[string]any) map[string]any {
		return map[string]any{"id": getOr(payload, "id", ""), "role": "assistant", "timestamp": timestamp, "content": blocks}
	}
	switch obj["type"] {
	case "event_msg":
		if kind != "turn_aborted" {
			return nil
		}
		reason := strOr(payload["reason"], "interrupted")
		return map[string]any{
			"id": "", "role": "system", "timestamp": timestamp,
			"content": []map[string]any{eventBlock(eventInterrupted, "Turn aborted: "+reason)},
		}
	case "response_item":
	default:
		return nil
	}

	switch kind {
	case "message":
		role := strOr(payload["role"], "unknown")
		if role == "developer" { // machine-assembled instructions, not conversation
			return nil
		}
		parts := []map[string]any{}
		texts := []string{}
		for _, item := range getSlice(payload, "content") {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := m["text"].(string); ok {
				parts = append(parts, textBlock(text))
				texts = append(texts, text)
			}
		}
		message := map[string]any{"id": getOr(payload, "id", ""), "role": role, "timestamp": timestamp, "content": parts}
		if role == "user" && codexInjected(strings.Join(texts, "\n")) {
			message["injected"] = true
		}
		return message

	case "reasoning":
		text := codexReasoningText(payload["summary"], payload["content"])
		if text == "" { // encrypted content only: there is nothing a reader could see
			return nil
		}
		return assistant(thinkingBlock(text, w.full))

	case "function_call":
		id := strOr(payload["call_id"], strOr(payload["id"], ""))
		name := strOr(payload["name"], "unknown")
		w.remember(id, name)
		return assistant(toolCallBlock(id, name, codexArguments(payload["arguments"])))

	case "custom_tool_call":
		id := strOr(payload["call_id"], strOr(payload["id"], ""))
		name := strOr(payload["name"], "unknown")
		w.remember(id, name)
		return assistant(toolCallBlock(id, name, map[string]any{"input": toStr(payload["input"])}))

	case "local_shell_call":
		id := strOr(payload["call_id"], strOr(payload["id"], ""))
		w.remember(id, "local_shell")
		return assistant(toolCallBlock(id, "local_shell", getOr(payload, "action", map[string]any{})))

	case "web_search_call":
		// No call_id and no output row: the call is the whole record, so it carries its
		// own (empty) result, marked by the status the row reports
		call := toolCallBlock("", "web_search", getOr(payload, "action", map[string]any{}))
		result := toolResultBlock("", "web_search", "", w.full)
		if payload["status"] == "completed" {
			result["status"] = statusOK
		}
		return assistant(call, result)

	case "function_call_output", "custom_tool_call_output":
		id := toStr(payload["call_id"])
		name := w.names[id]
		text, outcome := codexOutput(payload["output"])
		if name == "apply_patch" && codexPatchRejected(text) {
			// A rejected patch has no exit code and would otherwise read as applied
			outcome.status = statusError
		}
		return map[string]any{
			"id": getOr(payload, "id", ""), "role": "tool", "timestamp": timestamp,
			"content": []map[string]any{outcome.apply(toolResultBlock(id, name, text, w.full))},
		}
	}
	return nil
}

func (w *codexWalker) remember(id, name string) {
	if id != "" {
		w.names[id] = name
	}
}

// codexReasoningText joins what a reasoning row shows: the summary entries, then the
// content entries when they are not encrypted. Either list is [{type, text}].
func codexReasoningText(summary, content any) string {
	texts := []string{}
	for _, list := range []any{summary, content} {
		for _, item := range getSliceAny(list) {
			if m, ok := item.(map[string]any); ok {
				if text := strings.TrimSpace(strField(m, "text")); text != "" {
					texts = append(texts, text)
				}
			}
		}
	}
	return strings.Join(texts, "\n\n")
}

// getSliceAny is getSlice for a value that is already in hand
func getSliceAny(v any) []any {
	list, _ := v.([]any)
	return list
}

// codexArguments decodes a function call's arguments, which Codex writes as a JSON string.
// One that will not decode is kept whole under raw rather than dropped.
func codexArguments(v any) any {
	text, ok := v.(string)
	if !ok {
		if v == nil {
			return map[string]any{}
		}
		return v
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(text), &decoded) == nil && decoded != nil {
		return decoded
	}
	return map[string]any{"raw": text}
}

// codexOutput reads a tool output: the text a reader wants and what the header said about
// how the command ended.
//
// Two shapes, both seen in real rollouts. The older shell tool wrote the output as a JSON
// document, {"output": "...", "metadata": {"exit_code": 0, "duration_seconds": 0.1}}.
// Newer tools write plain text, or a list of {type, text} items, that opens with a header
// of known lines — "Exit code: N", "Wall time: 0.3 seconds", "Script completed",
// "Process exited with code N" — and ends the header with a line reading "Output:". The
// header is information, not output, so it is read and removed.
func codexOutput(output any) (string, toolOutcome) {
	switch v := output.(type) {
	case string:
		if body, outcome, ok := codexLegacyOutput(v); ok {
			return body, outcome
		}
		return codexStripHeader(v)
	case []any:
		var outcome toolOutcome
		bodies := []string{}
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "input_image" || m["type"] == "image" {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			body, found := codexStripHeader(text)
			if !outcome.hasExit && found.hasExit {
				outcome.exitCode, outcome.hasExit = found.exitCode, true
			}
			if outcome.durationMs == 0 {
				outcome.durationMs = found.durationMs
			}
			if found.status == statusError {
				outcome.status = statusError
			}
			if body != "" {
				bodies = append(bodies, body)
			}
		}
		if outcome.status == "" {
			outcome.status = statusOK
		}
		return strings.Join(bodies, ""), outcome
	}
	return "", toolOutcome{status: statusOK}
}

// codexLegacyOutput decodes the JSON form of a shell output
func codexLegacyOutput(text string) (string, toolOutcome, bool) {
	if !strings.HasPrefix(strings.TrimSpace(text), `{"output"`) {
		return "", toolOutcome{}, false
	}
	var legacy struct {
		Output   string `json:"output"`
		Metadata *struct {
			ExitCode        *int     `json:"exit_code"`
			DurationSeconds *float64 `json:"duration_seconds"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(text), &legacy) != nil {
		return "", toolOutcome{}, false
	}
	outcome := toolOutcome{status: statusOK}
	if legacy.Metadata != nil {
		if legacy.Metadata.ExitCode != nil {
			outcome.exitCode, outcome.hasExit = *legacy.Metadata.ExitCode, true
			if outcome.exitCode != 0 {
				outcome.status = statusError
			}
		}
		if legacy.Metadata.DurationSeconds != nil {
			outcome.durationMs = int64(*legacy.Metadata.DurationSeconds*1000 + 0.5)
		}
	}
	return legacy.Output, outcome, true
}

// codexStripHeader removes an output header when the text has one. Only a run of known
// header lines closed by an "Output:" line counts; anything else is output and is kept
// whole, header-looking first line included.
func codexStripHeader(text string) (string, toolOutcome) {
	outcome := toolOutcome{status: statusOK}
	rest := text
	for i := 0; i < 8; i++ {
		line, after, hasMore := strings.Cut(rest, "\n")
		line = strings.TrimRight(line, "\r")
		if line == "Output:" {
			return after, outcome
		}
		switch {
		case strings.HasPrefix(line, "Script "):
			status := strings.ToLower(line[len("Script "):])
			if strings.Contains(status, "fail") || strings.Contains(status, "error") {
				outcome.status = statusError
			}
		case strings.HasPrefix(line, "Wall time"):
			secs := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[len("Wall time"):]), ":"))
			secs = strings.TrimSpace(strings.TrimSuffix(secs, "seconds"))
			if f, err := strconv.ParseFloat(secs, 64); err == nil && f >= 0 {
				outcome.durationMs = int64(f*1000 + 0.5)
			}
		case strings.HasPrefix(line, "Process exited with code "), strings.HasPrefix(line, "Exit code: "):
			code := strings.TrimSpace(line[strings.LastIndexByte(line, ' ')+1:])
			if n, err := strconv.Atoi(code); err == nil {
				outcome.exitCode, outcome.hasExit = n, true
				if n != 0 {
					outcome.status = statusError
				}
			}
		case strings.HasPrefix(line, "Chunk ID:"), strings.HasPrefix(line, "Original token count:"),
			strings.HasPrefix(line, "Process running with session ID"):
			// known header lines that carry nothing worth keeping
		default:
			return text, toolOutcome{status: statusOK}
		}
		if !hasMore {
			break
		}
		rest = after
	}
	return text, toolOutcome{status: statusOK}
}

// codexPatchRejected: apply_patch's output when the patch did not land — the model wrote
// one that fails verification, or the user refused it. There is no exit code, so without
// this the step would read as a successful edit.
func codexPatchRejected(output string) bool {
	t := strings.TrimSpace(output)
	return strings.HasPrefix(t, "apply_patch verification failed") || strings.HasPrefix(t, "patch rejected")
}

// codexHitRole is the speaker of one raw rollout line, for search: the call rows carry no
// role of their own, and without this a hit in a command or its output came back with the
// row type in the role field.
func codexHitRole(obj map[string]any) string {
	if obj["type"] != "response_item" {
		return ""
	}
	switch toStr(getMap(obj, "payload")["type"]) {
	case "reasoning", "function_call", "custom_tool_call", "local_shell_call", "web_search_call":
		return "assistant"
	case "function_call_output", "custom_tool_call_output":
		return "tool"
	}
	return ""
}

func (s *CodexSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := r.str("file")
	if path == "" {
		return sink.result()
	}
	w := newCodexWalker(q.full)
	eachJSONL(path, func(obj map[string]any) bool {
		m := w.message(obj)
		if m == nil {
			return true
		}
		return sink.add(m)
	})
	return sink.result()
}

// codexProbe is the small decode every whole-file scan does per row. counts mirrors
// codexWalker.message — a row counts when it yields a message — so the list's number,
// Final's and the reader's are one number.
type codexProbe struct {
	Type    string `json:"type"`
	Payload struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Summary json.RawMessage `json:"summary"`
		Content json.RawMessage `json:"content"`
		Usage   json.RawMessage `json:"usage"`
		Info    json.RawMessage `json:"info"`
	} `json:"payload"`
}

func (p codexProbe) counts() bool {
	switch p.Type {
	case "event_msg":
		return p.Payload.Type == "turn_aborted"
	case "response_item":
	default:
		return false
	}
	switch p.Payload.Type {
	case "message":
		return p.Payload.Role != "developer"
	case "reasoning":
		return codexReasoningHasText(p.Payload.Summary) || codexReasoningHasText(p.Payload.Content)
	case "function_call", "custom_tool_call", "local_shell_call", "web_search_call",
		"function_call_output", "custom_tool_call_output":
		return true
	}
	return false
}

func codexReasoningHasText(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var items []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return false
	}
	for _, item := range items {
		if strings.TrimSpace(item.Text) != "" {
			return true
		}
	}
	return false
}

func (s *CodexSource) Final(r record) map[string]any {
	path := r.str("file")
	if path == "" {
		return nil
	}
	var rawLast []byte
	count := 0
	model := ""
	// Every token_usage_record is a turn's usage; summing them gives the session total,
	// where keeping only the last gave the final turn's (see source_claude). A rollout
	// without those records has token_count events instead, whose info.total_token_usage
	// is already cumulative, so the last one is the total.
	var totals usageTotals
	var lastTokenCount map[string]any
	eachJSONLLine(path, func(line []byte) bool {
		var probe codexProbe
		if json.Unmarshal(line, &probe) != nil {
			return true
		}
		switch {
		case probe.Type == "token_usage_record":
			if len(probe.Payload.Usage) > 0 {
				var u map[string]any
				if json.Unmarshal(probe.Payload.Usage, &u) == nil && len(u) > 0 {
					totals.add(u)
				}
			}
			return true
		case probe.Type == "turn_context":
			if probe.Payload.Model != "" {
				model = probe.Payload.Model
			}
			return true
		case probe.Type == "event_msg" && probe.Payload.Type == "token_count":
			var info map[string]any
			if json.Unmarshal(probe.Payload.Info, &info) == nil {
				if total, ok := info["total_token_usage"].(map[string]any); ok && len(total) > 0 {
					lastTokenCount = total
				}
			}
			return true
		}
		if !probe.counts() {
			return true
		}
		count++
		if probe.Type == "response_item" && probe.Payload.Type == "message" && probe.Payload.Role == "assistant" {
			rawLast = append(rawLast[:0], line...)
		}
		return true
	})
	if !totals.seen && lastTokenCount != nil {
		totals.add(lastTokenCount)
	}
	var lastAssistant map[string]any
	if len(rawLast) > 0 {
		_ = json.Unmarshal(rawLast, &lastAssistant)
	}
	if lastAssistant == nil {
		return map[string]any{
			"status":       "done",
			"isFinal":      false,
			"isProcessing": false,
			"messageCount": count,
			"source":       "codex",
			"model":        model,
			"text":         "",
			"thinking":     "",
			"usage":        totals.result(),
		}
	}

	payload := getMap(lastAssistant, "payload")
	texts := []string{}
	for _, item := range getSlice(payload, "content") {
		if m, ok := item.(map[string]any); ok {
			if text, ok := m["text"].(string); ok {
				texts = append(texts, text)
			}
		}
	}
	return map[string]any{
		"status":       "done",
		"isFinal":      true,
		"isProcessing": false,
		"messageCount": count,
		"source":       "codex",
		"id":           getOr(payload, "id", ""),
		"timestamp":    getOr(lastAssistant, "timestamp", ""),
		"stopReason":   getOr(payload, "stop_reason", ""),
		"model":        model,
		"text":         strings.Join(texts, "\n"),
		"thinking":     "",
		"toolCalls":    []any{},
		"usage":        totals.result(),
	}
}
