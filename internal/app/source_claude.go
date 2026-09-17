package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Claude Code：~/.claude/projects/<项目>/<session-uuid>.jsonl
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

// List 只读每个会话文件的头几行拿元数据；文件没变过就直接用缓存（见 fileRecordCache）
func (s *ClaudeCodeSource) List() []record {
	return s.cache.records(s.files(), func(path, modISO string) record {
		head := readJSONL(path, 5)
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		sid := stem
		var cwd any = ""
		for _, obj := range head {
			if truthy(obj["sessionId"]) {
				sid = toStr(obj["sessionId"])
			}
			if truthy(obj["cwd"]) {
				cwd = obj["cwd"]
			}
		}
		return newRecord(map[string]any{
			"source":    "claude",
			"key":       path,
			"shortKey":  stem,
			"sessionId": sid,
			"file":      path,
			"hasFile":   true,
			"status":    "done",
			"cwd":       cwd,
			"updatedAt": modISO,
		}, modISO)
	})
}

// parts 把 Claude Code 的 content 收敛成统一的块数组
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
		if truthy(obj["isSidechain"]) { // 子代理的消息不计入主线
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
	// 只关心 type / isSidechain；content 这类大字段留到最后一条助手消息再完整解析
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
