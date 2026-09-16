package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Pi：~/.pi/agent/sessions/<项目>/<时间>_<uuid>.jsonl
type PiSource struct {
	root string
}

func newPiSource(root string) *PiSource { return &PiSource{root: root} }

func (s *PiSource) Mode() string     { return "pi" }
func (s *PiSource) Location() string { return s.root }

func (s *PiSource) Exists() bool {
	_, err := os.Stat(s.root)
	return err == nil
}

func (s *PiSource) files() []string {
	files, err := filepath.Glob(filepath.Join(s.root, "*", "*.jsonl"))
	if err != nil {
		return nil
	}
	return files
}

func (s *PiSource) List() []record {
	out := []record{}
	for _, path := range s.files() {
		head := readJSONL(path, 1)
		var meta map[string]any
		if len(head) > 0 {
			meta = head[0]
		} else {
			meta = map[string]any{}
		}
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		updated := mtimeISO(path)
		out = append(out, record{
			fields: map[string]any{
				"source":    "pi",
				"key":       path,
				"shortKey":  stem,
				"sessionId": strOr(meta["id"], stem),
				"file":      path,
				"hasFile":   true,
				"status":    "done",
				"cwd":       getOr(meta, "cwd", ""),
				"updatedAt": updated,
			},
			sortKey: updated,
		})
	}
	return out
}

func (s *PiSource) Messages(r record, limit int) []map[string]any {
	out := []map[string]any{}
	path := r.str("file")
	if path == "" {
		return out
	}
	eachJSONL(path, func(obj map[string]any) bool {
		if len(out) >= limit {
			return false
		}
		if obj["type"] != "message" {
			return true
		}
		msg := getMap(obj, "message")
		parts := []map[string]any{}
		for _, item := range getSlice(msg, "content") {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			kind := strField(m, "type")
			_, hasText := m["text"]
			switch {
			case kind == "text" || hasText:
				parts = append(parts, map[string]any{"type": "text", "content": strField(m, "text")})
			case kind == "thinking":
				parts = append(parts, map[string]any{"type": "thinking", "content": truncate(strField(m, "thinking"), 1000, "...[truncated]")})
			default:
				if kind == "" {
					kind = "unknown"
				}
				parts = append(parts, map[string]any{"type": kind, "content": truncate(contentText(m), 500, "...[truncated]")})
			}
		}
		out = append(out, map[string]any{
			"id":        getOr(obj, "id", ""),
			"role":      strOr(msg["role"], "unknown"),
			"timestamp": getOr(msg, "timestamp", getOr(obj, "timestamp", "")),
			"content":   parts,
		})
		return true
	})
	return out
}

func (s *PiSource) Final(r record) map[string]any {
	path := r.str("file")
	if path == "" {
		return nil
	}
	// 整文件扫描这条路径只关心 type / message.role：先按结构体「探测」，
	// 大字段（content、toolResult）不物化成 map，实测比全量解析快 3 倍以上
	var rawLast []byte
	count := 0
	eachJSONLLine(path, func(line []byte) bool {
		var probe struct {
			Type    string `json:"type"`
			Message struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &probe) != nil {
			return true
		}
		if probe.Type != "message" {
			return true
		}
		count++
		if probe.Message.Role == "assistant" {
			// 缓冲会被复用，留用必须拷贝
			rawLast = append(rawLast[:0], line...)
		}
		return true
	})
	if len(rawLast) == 0 {
		return map[string]any{
			"status":       "done",
			"isFinal":      false,
			"isProcessing": false,
			"messageCount": count,
			"source":       "pi",
			"text":         "",
			"thinking":     "",
		}
	}
	var lastAssistant map[string]any
	_ = json.Unmarshal(rawLast, &lastAssistant)
	if lastAssistant == nil {
		return map[string]any{
			"status":       "done",
			"isFinal":      false,
			"isProcessing": false,
			"messageCount": count,
			"source":       "pi",
			"text":         "",
			"thinking":     "",
		}
	}

	msg := getMap(lastAssistant, "message")
	textParts := []string{}
	thinkParts := []string{}
	for _, item := range getSlice(msg, "content") {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "thinking" {
			thinkParts = append(thinkParts, strField(m, "thinking"))
		} else {
			textParts = append(textParts, contentText(m))
		}
	}
	stopReason := strField(msg, "stopReason")
	return map[string]any{
		"status":       "done",
		"isFinal":      stopReason == "stop",
		"isProcessing": false,
		"messageCount": count,
		"source":       "pi",
		"id":           getOr(lastAssistant, "id", ""),
		"timestamp":    getOr(msg, "timestamp", getOr(lastAssistant, "timestamp", "")),
		"stopReason":   stopReason,
		"model":        getOr(msg, "model", ""),
		"text":         strings.Join(nonEmpty(textParts), "\n"),
		"thinking":     strings.Join(nonEmpty(thinkParts), "\n"),
		"toolCalls":    []any{},
		"usage":        getOr(msg, "usage", map[string]any{}),
	}
}

func nonEmpty(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
