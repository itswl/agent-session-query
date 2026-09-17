package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Claude Code: ~/.claude/projects/<project>/<session-uuid>.jsonl
type ClaudeCodeSource struct {
	root  string
	cache *fileRecordCache
}

func newClaudeSource(root string) *ClaudeCodeSource {
	return &ClaudeCodeSource{root: root, cache: newFileRecordCache()}
}

func (s *ClaudeCodeSource) Mode() string     { return "claude" }
func (s *ClaudeCodeSource) Location() string { return s.root }

func (s *ClaudeCodeSource) Exists() bool { return fileExists(s.root) }

func (s *ClaudeCodeSource) files() []string {
	files, err := filepath.Glob(filepath.Join(s.root, "*", "*.jsonl"))
	if err != nil {
		return nil
	}
	return files
}

// claudeHeadLines caps how far down the file the metadata scan will go.
//
// cwd is not on the first line — sessions open with non-conversation rows such as
// queue-operation. The original code read a fixed first 5 lines; measured locally, cwd
// lands on lines 2-5 with a good number sitting exactly on line 5. That is luck, not a
// guarantee: one more preamble row from Claude Code and cwd slides out of the window,
// after which cwd / project silently go empty and project grouping breaks, without any
// error. So the scan now runs until it has what it needs, and this cap is only a
// defensive backstop.
//
// It is usually faster too: stopping as soon as both fields are in place ends most files
// at line 3, fewer than the fixed 5 it used to read.
const claudeHeadLines = 50

// List reads only the head of each session file for metadata; unchanged files come
// straight from the cache (see fileRecordCache)
func (s *ClaudeCodeSource) List() []record {
	return s.cache.records(s.files(), func(path, modISO string) record {
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		// Use the time on the last record in the file, not the file's mtime (see updatedAtOf)
		updated := updatedAtOf(path, modISO)
		sid := stem
		var cwd any = ""
		var haveSID, haveCWD bool
		seen := 0
		eachJSONL(path, func(obj map[string]any) bool {
			if !haveSID && truthy(obj["sessionId"]) {
				sid, haveSID = toStr(obj["sessionId"]), true
			}
			if !haveCWD && truthy(obj["cwd"]) {
				cwd, haveCWD = obj["cwd"], true
			}
			seen++
			return !(haveSID && haveCWD) && seen < claudeHeadLines
		})
		return newRecord(map[string]any{
			"source":    "claude",
			"key":       path,
			"shortKey":  stem,
			"sessionId": sid,
			"file":      path,
			"hasFile":   true,
			"status":    "done",
			"cwd":       cwd,
			"updatedAt": updated,
		}, updated)
	})
}

// parts folds Claude Code's content into the shared block array shape
func (s *ClaudeCodeSource) parts(content any) []map[string]any {
	parts := []map[string]any{}
	switch c := content.(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"type": "text", "content": c})
		}
		return parts
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
			case "tool_use":
				parts = append(parts, map[string]any{"type": "toolCall", "name": strField(m, "name"), "arguments": getOr(m, "input", map[string]any{})})
			case "tool_result":
				parts = append(parts, map[string]any{"type": "toolResult", "toolName": "", "content": truncate(contentText(m["content"]), 500, "...[truncated]")})
			}
		}
	}
	return parts
}

func (s *ClaudeCodeSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := r.str("file")
	if path == "" {
		return sink.result()
	}
	eachJSONL(path, func(obj map[string]any) bool {
		if obj["type"] != "user" && obj["type"] != "assistant" {
			return true
		}
		if truthy(obj["isSidechain"]) { // subagent messages are not part of the main thread
			return true
		}
		msg := getMap(obj, "message")
		return sink.add(map[string]any{
			"id":        getOr(obj, "uuid", ""),
			"role":      strOr(msg["role"], strOr(obj["type"], "")),
			"timestamp": getOr(obj, "timestamp", ""),
			"content":   s.parts(msg["content"]),
		})
	})
	return sink.result()
}

func (s *ClaudeCodeSource) Final(r record) map[string]any {
	path := r.str("file")
	if path == "" {
		return nil
	}
	// Only type / isSidechain matter here; big fields like content wait until the last
	// assistant message, which is the only one fully decoded
	var rawLast []byte
	count := 0
	eachJSONLLine(path, func(line []byte) bool {
		var probe struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
		}
		if json.Unmarshal(line, &probe) != nil {
			return true
		}
		if probe.Type != "user" && probe.Type != "assistant" {
			return true
		}
		if probe.IsSidechain {
			return true
		}
		count++
		if probe.Type == "assistant" {
			rawLast = append(rawLast[:0], line...)
		}
		return true
	})
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
			"source":       "claude",
			"text":         "",
			"thinking":     "",
		}
	}

	msg := getMap(lastAssistant, "message")
	parts := s.parts(msg["content"])
	texts := []string{}
	thoughts := []string{}
	toolCalls := []any{}
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
	stopReason := strOr(msg["stop_reason"], "")
	isFinal := stopReason == "end_turn" || stopReason == "stop" || stopReason == "stop_sequence"
	return map[string]any{
		"status":       "done",
		"isFinal":      isFinal,
		"isProcessing": false,
		"messageCount": count,
		"source":       "claude",
		"id":           getOr(msg, "id", ""),
		"timestamp":    getOr(lastAssistant, "timestamp", ""),
		"stopReason":   stopReason,
		"model":        getOr(msg, "model", ""),
		"text":         strings.Join(texts, "\n"),
		"thinking":     strings.Join(thoughts, "\n"),
		"toolCalls":    toolCalls,
		"usage":        getOr(msg, "usage", map[string]any{}),
	}
}
