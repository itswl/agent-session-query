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

// claudeHeadLines 找元数据时最多往下读几行。
//
// cwd 不在首行——文件开头常有 queue-operation 之类的非对话行。原先固定只读前 5 行，
// 实测本机的会话 cwd 落在第 2–5 行，其中不少正好卡在第 5 行：这是侥幸不是保证，
// Claude Code 再多一种前导行就会滑出窗口，然后 cwd / project 静默变空、项目聚合失效，
// 而且不报错。所以改成「读到拿齐为止」，这个上限只是防御性的兜底。
//
// 常见情况下反而更快：拿齐就停，多数文件第 3 行就停了，比原来固定读 5 行还少。
const claudeHeadLines = 50

// List 只读每个会话文件的头部拿元数据；文件没变过就直接用缓存（见 fileRecordCache）
func (s *ClaudeCodeSource) List() []record {
	return s.cache.records(s.files(), func(path, modISO string) record {
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		// 用内容里最后一条记录的时间，而不是文件 mtime（见 updatedAtOf）
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
