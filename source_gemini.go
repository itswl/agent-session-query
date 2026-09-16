package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Gemini CLI：~/.gemini/tmp/<项目>/chats/session-*.jsonl
type GeminiSource struct {
	root string
}

func newGeminiSource(root string) *GeminiSource { return &GeminiSource{root: root} }

func (s *GeminiSource) Mode() string     { return "gemini" }
func (s *GeminiSource) Location() string { return s.root }

func (s *GeminiSource) Exists() bool {
	_, err := os.Stat(s.root)
	return err == nil
}

func (s *GeminiSource) files() []string {
	files, err := filepath.Glob(filepath.Join(s.root, "*", "chats", "session-*.jsonl"))
	if err != nil {
		return nil
	}
	return files
}

// metaOf 只读首行拿元数据（列表用，不扫全文件）
func (s *GeminiSource) metaOf(path string) map[string]any {
	meta := map[string]any{}
	eachJSONL(path, func(obj map[string]any) bool {
		if truthy(obj["sessionId"]) {
			meta = obj
			return false
		}
		return true
	})
	return meta
}

// geminiThoughts 把 thoughts 收敛成文本：Gemini CLI 有两代格式——字符串，或
// [{subject, description, timestamp}] 数组（做计划/自我汇报时的分条思考）。
func geminiThoughts(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := []string{}
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if text := strOr(m["description"], strOr(m["subject"], "")); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// geminiParts 把一条 Gemini 消息收敛成统一的块数组。工具调用型会话里正文很稀：
// 发起调用时 content 是空串、真正内容在 toolCalls（name + args，结果不带——
// 后续 user 行的 functionResponse 才是执行结果）；user 行的 content 数组里
// functionResponse 项回传工具输出。thoughts 插在最前面，与其它源一致。
func geminiParts(m map[string]any) []map[string]any {
	parts := []map[string]any{}
	switch content := m["content"].(type) {
	case string:
		if content != "" {
			parts = append(parts, map[string]any{"type": "text", "content": content})
		}
	case []any:
		for _, item := range content {
			mm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := mm["text"].(string); ok && text != "" {
				parts = append(parts, map[string]any{"type": "text", "content": text})
			}
			if fr, ok := mm["functionResponse"].(map[string]any); ok {
				output := contentText(getMap(fr, "response")["output"])
				parts = append(parts, map[string]any{
					"type":     "toolResult",
					"toolName": strOr(fr["name"], ""),
					"content":  truncate(output, 500, "...[truncated]"),
				})
			}
		}
	}
	for _, tc := range getSlice(m, "toolCalls") {
		mm, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		parts = append(parts, map[string]any{
			"type":      "toolCall",
			"name":      strOr(mm["name"], ""),
			"arguments": getOr(mm, "args", map[string]any{}),
		})
	}
	if thoughts := geminiThoughts(m["thoughts"]); thoughts != "" {
		parts = append([]map[string]any{{"type": "thinking", "content": truncate(thoughts, 1000, "...[truncated]")}}, parts...)
	}
	return parts
}

// eachEntry 逐行产出消息；Gemini 的 jsonl 是「首行元数据 + $set 补丁 + 消息行」的追加日志
func eachGeminiEntry(path string, fn func(entry map[string]any) bool) {
	eachJSONL(path, func(obj map[string]any) bool {
		if set, ok := obj["$set"].(map[string]any); ok && set != nil {
			for _, m := range getSlice(set, "messages") {
				if mm, ok := m.(map[string]any); ok {
					if _, has := mm["type"]; has {
						if !fn(mm) {
							return false
						}
					}
				}
			}
			return true
		}
		if _, has := obj["type"]; has && !truthy(obj["sessionId"]) {
			return fn(obj)
		}
		return true
	})
}

func (s *GeminiSource) List() []record {
	out := []record{}
	for _, path := range s.files() {
		meta := s.metaOf(path)
		// 用元数据里的时间；没有就退回文件修改时间（都不需要扫全文件）
		lastTs := meta["lastUpdated"]
		if !truthy(lastTs) {
			lastTs = meta["startTime"]
		}
		if !truthy(lastTs) {
			lastTs = mtimeISO(path)
		}
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		out = append(out, record{
			fields: map[string]any{
				"source":    "gemini",
				"key":       path,
				"shortKey":  stem,
				"sessionId": strOr(meta["sessionId"], stem),
				"file":      path,
				"hasFile":   true,
				"status":    "done",
				"project":   filepath.Base(filepath.Dir(filepath.Dir(path))),
				"updatedAt": lastTs,
			},
			sortKey: toStr(lastTs),
		})
	}
	return out
}

func (s *GeminiSource) Messages(r record, limit int) []map[string]any {
	path := r.str("file")
	if path == "" {
		return []map[string]any{}
	}
	out := []map[string]any{}
	eachGeminiEntry(path, func(m map[string]any) bool {
		if len(out) >= limit {
			return false
		}
		if m["type"] != "user" && m["type"] != "gemini" {
			return true
		}
		role := "user"
		if m["type"] == "gemini" {
			role = "assistant"
		}
		out = append(out, map[string]any{
			"id":        getOr(m, "id", ""),
			"role":      role,
			"timestamp": getOr(m, "timestamp", ""),
			"content":   geminiParts(m),
		})
		return true
	})
	return out
}

func (s *GeminiSource) Final(r record) map[string]any {
	path := r.str("file")
	if path == "" {
		return nil
	}
	var lastGemini map[string]any
	count := 0
	eachGeminiEntry(path, func(m map[string]any) bool {
		if m["type"] != "user" && m["type"] != "gemini" {
			return true
		}
		count++
		if m["type"] == "gemini" {
			lastGemini = m
		}
		return true
	})
	if lastGemini == nil {
		return map[string]any{
			"status":       "done",
			"isFinal":      false,
			"isProcessing": false,
			"messageCount": count,
			"source":       "gemini",
			"text":         "",
			"thinking":     "",
		}
	}

	texts := []string{}
	toolCalls := []any{}
	for _, p := range geminiParts(lastGemini) {
		switch p["type"] {
		case "text":
			if c := strField(p, "content"); c != "" {
				texts = append(texts, c)
			}
		case "toolCall":
			toolCalls = append(toolCalls, map[string]any{
				"name":      strField(p, "name"),
				"arguments": getOr(p, "arguments", map[string]any{}),
			})
		}
	}
	thinking := truncate(geminiThoughts(lastGemini["thoughts"]), 1000, "...[truncated]")
	return map[string]any{
		"status":       "done",
		"isFinal":      true,
		"isProcessing": false,
		"messageCount": count,
		"source":       "gemini",
		"id":           getOr(lastGemini, "id", ""),
		"timestamp":    getOr(lastGemini, "timestamp", ""),
		"stopReason":   "stop",
		"model":        getOr(lastGemini, "model", ""),
		"text":         strings.Join(texts, "\n"),
		"thinking":     thinking,
		"toolCalls":    toolCalls,
		"usage":        getOr(lastGemini, "tokens", map[string]any{}),
	}
}
