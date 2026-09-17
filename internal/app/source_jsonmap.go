package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// JsonMapSource: OpenClaw / Hermes — one sessions.json index plus one jsonl per session.
type JsonMapSource struct {
	def jsonMapDef
}

func newJsonMapSource(def jsonMapDef) *JsonMapSource { return &JsonMapSource{def: def} }

func (s *JsonMapSource) Mode() string { return s.def.mode }

// Location returns whichever index actually exists (sessions.json wins; newer Hermes
// has only state.db)
func (s *JsonMapSource) Location() string {
	if !fileExists(s.def.sessionsJSON) && s.def.stateDB != "" && fileExists(s.def.stateDB) {
		return s.def.stateDB
	}
	return s.def.sessionsJSON
}

func (s *JsonMapSource) Exists() bool {
	return fileExists(s.def.sessionsJSON) || (s.def.stateDB != "" && fileExists(s.def.stateDB))
}

// kv preserves sessions.json's original order (Go maps do not, and order decides which
// of two equally ranked records wins)
type kv struct {
	Key string
	Val any
}

// load reads sessions.json; anything that is not a JSON object yields nothing.
func (s *JsonMapSource) load() []kv {
	raw, err := os.ReadFile(s.def.sessionsJSON)
	if err != nil || !json.Valid(raw) {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil
	}
	out := []kv{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := keyTok.(string)
		if !ok {
			break
		}
		var val any
		if err := dec.Decode(&val); err != nil {
			break
		}
		out = append(out, kv{Key: key, Val: val})
	}
	return out
}

// fileOf resolves the jsonl path belonging to a session record.
func (s *JsonMapSource) fileOf(r record) string {
	if path := r.str("file"); path != "" && fileExists(path) {
		return path
	}
	if sid := r.str("sessionId"); sid != "" {
		if alt := filepath.Join(s.def.sessionsDir, sid+".jsonl"); fileExists(alt) {
			return alt
		}
	}
	return ""
}

func (s *JsonMapSource) List() []record {
	entries := s.load()
	isOpenClaw := s.def.mode == "openclaw"
	out := []record{}

	for _, entry := range entries {
		info, ok := entry.Val.(map[string]any)
		if !ok || info == nil {
			continue
		}
		sid := strOr(info[s.def.sessionIDField], "")
		if sid == "" {
			sid = strOr(info["sessionId"], strOr(info["session_id"], ""))
		}

		file := strOr(info["sessionFile"], "")
		var filePublic any
		hasFile := file != "" && fileExists(file)
		if !hasFile && sid != "" {
			if alt := filepath.Join(s.def.sessionsDir, sid+".jsonl"); fileExists(alt) {
				file, hasFile = alt, true
			}
		}
		if hasFile {
			filePublic = file
		}

		shortKey := entry.Key
		if isOpenClaw {
			shortKey = strings.ReplaceAll(entry.Key, "agent:default:", "")
		}

		fields := map[string]any{
			"source":    s.def.mode,
			"key":       entry.Key,
			"shortKey":  shortKey,
			"sessionId": sid,
			"file":      filePublic,
			"hasFile":   hasFile,
		}
		var updatedRaw any // handed to newRecord, which parses it into the sort time

		if isOpenClaw {
			updated := info["updatedAt"] // epoch milliseconds
			updatedStr := ""
			if truthy(updated) {
				updatedStr = toStr(updated)
				if ms, ok := toFloat(updated); ok {
					if dashed, _, ok := utcFromSeconds(ms / 1000); ok {
						updatedStr = dashed
					}
				}
			}
			updatedRaw = updated
			fields["status"] = getOr(info, "status", "unknown")
			fields["updatedAt"] = updatedStr
			fields["model"] = getOr(info, "model", "")
			fields["runtimeMs"] = getOr(info, "runtimeMs", float64(0))
			fields["totalTokens"] = getOr(info, "totalTokens", float64(0))
		} else { // hermes
			updatedRaw = info["updated_at"]
			fields["status"] = "done"
			fields["updatedAt"] = getOr(info, "updated_at", "")
			fields["createdAt"] = getOr(info, "created_at", "")
			fields["displayName"] = getOr(info, "display_name", "")
			fields["platform"] = getOr(info, "platform", "")
			fields["totalTokens"] = getOr(info, "total_tokens", float64(0))
			fields["estimatedCostUsd"] = getOr(info, "estimated_cost_usd", float64(0))
		}

		out = append(out, newRecord(fields, updatedRaw))
	}

	// Newer Hermes keeps every session in state.db and may have no sessions.json at all.
	// Session IDs already listed are skipped so nothing shows up twice.
	// stat first: with no database there is no point opening a connection and logging an
	// error on every single list call.
	if s.def.stateDB != "" && fileExists(s.def.stateDB) {
		seen := map[string]bool{}
		for _, r := range out {
			if sid := r.str("sessionId"); sid != "" {
				seen[sid] = true
			}
		}
		out = append(out, hermesSQLiteList(s.def.stateDB, s.def.mode, seen)...)
	}
	return out
}

// Messages: OpenClaw / Hermes message rows identify themselves by content
// (type=message, or role=user/assistant).
func (s *JsonMapSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := s.fileOf(r)
	if path == "" {
		// No session file (newer Hermes is all SQLite): read the messages from state.db too
		if s.def.stateDB != "" && fileExists(s.def.stateDB) {
			return hermesSQLiteMessages(s.def.stateDB, r.str("sessionId"), q)
		}
		return sink.result()
	}
	eachJSONL(path, func(obj map[string]any) bool {
		if obj["type"] == "message" {
			return sink.add(s.formatMessage(obj))
		}
		if role := obj["role"]; role == "user" || role == "assistant" {
			return sink.add(s.formatMessage(obj))
		}
		return true
	})
	return sink.result()
}

// Search scans the jsonl when there is one; without it (newer Hermes is all SQLite) it
// queries the database. This is the only searchableSource implementation — every other
// source has nothing but files, so the generic path suffices.
func (s *JsonMapSource) Search(ctx context.Context, r record, q searchQuery) []map[string]any {
	if path := s.fileOf(r); path != "" {
		return searchFile(ctx, path, q)
	}
	if s.def.stateDB != "" && fileExists(s.def.stateDB) {
		return hermesSQLiteSearch(ctx, s.def.stateDB, r.str("sessionId"), q)
	}
	return nil
}

// formatMessage renders one message (OpenClaw's content array / Hermes's string).
func (s *JsonMapSource) formatMessage(msg map[string]any) map[string]any {
	var content any
	var role string
	if msg["type"] == "message" {
		inner := getMap(msg, "message")
		content = inner["content"]
		if content == nil {
			content = []any{}
		}
		role = strOr(inner["role"], "unknown")
	} else {
		content = msg["content"]
		if content == nil {
			content = ""
		}
		role = strOr(msg["role"], "unknown")
	}

	parts := []map[string]any{}
	switch c := content.(type) {
	case []any:
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "content": strField(m, "text")})
			case "thinking":
				parts = append(parts, map[string]any{"type": "thinking", "content": truncate(strField(m, "thinking"), 1000, "...[truncated]")})
			case "toolCall":
				args := m["arguments"]
				if args == nil {
					args = map[string]any{}
				}
				parts = append(parts, map[string]any{"type": "toolCall", "name": strField(m, "name"), "arguments": args})
			case "toolResult":
				resultText := ""
				for _, raw := range getSlice(m, "content") {
					if rm, ok := raw.(map[string]any); ok && rm["type"] == "text" {
						resultText = truncate(strField(rm, "text"), 500, "...[truncated]")
					}
				}
				parts = append(parts, map[string]any{"type": "toolResult", "toolName": strField(m, "toolName"), "content": resultText})
			}
		}
	case string:
		if role == "assistant" {
			if reasoning := strField(msg, "reasoning"); reasoning != "" {
				parts = append(parts, map[string]any{"type": "thinking", "content": truncate(reasoning, 1000, "...[truncated]")})
			}
		}
		parts = append(parts, map[string]any{"type": "text", "content": c})
	}

	return map[string]any{
		"id":        getOr(msg, "id", ""),
		"role":      role,
		"timestamp": getOr(msg, "timestamp", ""),
		"content":   parts,
	}
}

// stopMessage holds the first assistant message with stopReason=stop, plus the
// stopReason parsed out of it
type stopMessage struct {
	line   map[string]any
	reason string
}

func (s *JsonMapSource) Final(r record) map[string]any {
	status := r.str("status")
	if status == "" {
		status = "done"
	}
	sessionID := r.str("sessionId")
	path := s.fileOf(r)

	if path == "" {
		if result := hermesSQLiteFallback(s.def, sessionID, status); result != nil {
			return result
		}
		return map[string]any{
			"status":       status,
			"isFinal":      false,
			"isProcessing": status == "running",
			"messageCount": 0,
			"source":       s.def.mode,
			"error":        "Session file not available yet (session may be still initializing)",
		}
	}

	allMessages := []map[string]any{}
	var firstStop *stopMessage
	toolResultParents := map[string]bool{}

	eachJSONL(path, func(obj map[string]any) bool {
		stopField := s.def.stopReasonField
		if obj["type"] == "message" {
			msg := getMap(obj, "message")
			switch msg["role"] {
			case "assistant":
				stopReason := strOr(msg[stopField], strOr(obj[stopField], ""))
				allMessages = append(allMessages, obj)
				if firstStop == nil && stopReason == "stop" {
					firstStop = &stopMessage{line: obj, reason: stopReason}
				}
			case "toolResult":
				if parent := strOr(obj["parentId"], ""); parent != "" {
					toolResultParents[parent] = true
				}
			}
		} else if obj["role"] == "assistant" {
			stopReason := strOr(obj[stopField], "")
			allMessages = append(allMessages, obj)
			if firstStop == nil && stopReason == "stop" {
				firstStop = &stopMessage{line: obj, reason: stopReason}
			}
		}
		return true
	})

	// A toolResult pointing back at the first stop message means work may still be running
	isProcessing := false
	if firstStop != nil && status == "running" && toolResultParents[strField(firstStop.line, "id")] {
		isProcessing = true
	}

	if firstStop == nil {
		if result := hermesSQLiteFallback(s.def, sessionID, status); result != nil {
			return result
		}
		return map[string]any{
			"status":       status,
			"isFinal":      false,
			"isProcessing": status == "running",
			"messageCount": len(allMessages),
			"source":       s.def.mode,
			"text":         "",
			"thinking":     "",
		}
	}

	last := firstStop
	var content any
	stopReason := ""
	if last.line["type"] == "message" {
		msgData := getMap(last.line, "message")
		content = msgData["content"]
		stopReason = strOr(last.reason, strOr(msgData[s.def.stopReasonField], strOr(last.line[s.def.stopReasonField], "")))
	} else {
		content = last.line["content"]
		stopReason = last.reason
	}

	isStopped := stopReason == "stop"
	isDone := status == "done" || isStopped

	usage := getOr(last.line, "usage", map[string]any{})
	result := map[string]any{
		"status":       status,
		"isFinal":      isDone && !isProcessing,
		"isProcessing": isProcessing || (status == "running" && !isStopped),
		"messageCount": len(allMessages),
		"source":       s.def.mode,
		"id":           getOr(last.line, "id", ""),
		"timestamp":    getOr(last.line, "timestamp", ""),
		"stopReason":   stopReason,
		"text":         "",
		"thinking":     "",
		"toolCalls":    []any{},
		"usage":        usage,
	}

	switch c := content.(type) {
	case []any:
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				result["text"] = strField(m, "text")
			case "thinking":
				result["thinking"] = strField(m, "thinking")
			case "toolCall":
				args := m["arguments"]
				if args == nil {
					args = map[string]any{}
				}
				result["toolCalls"] = append(result["toolCalls"].([]any), map[string]any{
					"name": strField(m, "name"), "arguments": args,
				})
			}
		}
	case string:
		result["text"] = c
		result["thinking"] = strField(last.line, "reasoning")
	}

	return result
}

// hermesSQLiteFallback: Hermes webhook sessions sometimes land their final message only
// in state.db with nothing in the jsonl, leaving SQLite as the only place to find it
// (see hermes_sqlite.go).
//
// It uses the stateDB path the source was configured with — the same one List and
// Messages have always used — rather than re-deriving it from $HOME, which would
// silently query the wrong database whenever the two disagree.
func hermesSQLiteFallback(def jsonMapDef, sessionID, status string) map[string]any {
	if def.stateDB == "" {
		return nil
	}
	return hermesSQLiteFinal(def.stateDB, def.mode, sessionID, status)
}
