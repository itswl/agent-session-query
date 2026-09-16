package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Codex：~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl
type CodexSource struct {
	root string
}

func newCodexSource(root string) *CodexSource { return &CodexSource{root: root} }

func (s *CodexSource) Mode() string     { return "codex" }
func (s *CodexSource) Location() string { return s.root }

func (s *CodexSource) Exists() bool {
	_, err := os.Stat(s.root)
	return err == nil
}

func (s *CodexSource) files() []string {
	files, err := filepath.Glob(filepath.Join(s.root, "*", "*", "*", "rollout-*.jsonl"))
	if err != nil {
		return nil
	}
	return files
}

func (s *CodexSource) List() []record {
	out := []record{}
	for _, path := range s.files() {
		head := readJSONL(path, 1)
		payload := map[string]any{}
		if len(head) > 0 {
			payload = getMap(head[0], "payload")
		}
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		updated := mtimeISO(path)
		out = append(out, record{
			fields: map[string]any{
				"source":     "codex",
				"key":        path,
				"shortKey":   stem,
				"sessionId":  strOr(payload["session_id"], strOr(payload["id"], stem)),
				"file":       path,
				"hasFile":    true,
				"status":     "done",
				"cwd":        getOr(payload, "cwd", ""),
				"cliVersion": getOr(payload, "cli_version", ""),
				"updatedAt":  updated,
			},
			sortKey: updated,
		})
	}
	return out
}

func (s *CodexSource) Messages(r record, limit int) []map[string]any {
	out := []map[string]any{}
	path := r.str("file")
	if path == "" {
		return out
	}
	eachJSONL(path, func(obj map[string]any) bool {
		if len(out) >= limit {
			return false
		}
		if obj["type"] != "response_item" {
			return true
		}
		payload := getMap(obj, "payload")
		if payload["type"] != "message" {
			return true
		}
		role := strOr(payload["role"], "unknown")
		if role == "developer" { // 系统拼装的指令，不算对话
			return true
		}
		parts := []map[string]any{}
		for _, item := range getSlice(payload, "content") {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := m["text"].(string); ok {
				parts = append(parts, map[string]any{"type": "text", "content": text})
			}
		}
		out = append(out, map[string]any{
			"id":        getOr(payload, "id", ""),
			"role":      role,
			"timestamp": getOr(obj, "timestamp", ""),
			"content":   parts,
		})
		return true
	})
	return out
}

func (s *CodexSource) Final(r record) map[string]any {
	path := r.str("file")
	if path == "" {
		return nil
	}
	// 只关心 type / payload.type / payload.role（以及用量那一行的小对象）
	var rawLast []byte
	usage := map[string]any{}
	count := 0
	eachJSONLLine(path, func(line []byte) bool {
		var probe struct {
			Type    string `json:"type"`
			Payload struct {
				Type  string          `json:"type"`
				Role  string          `json:"role"`
				Usage json.RawMessage `json:"usage"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &probe) != nil {
			return true
		}
		if probe.Type == "token_usage_record" {
			if len(probe.Payload.Usage) > 0 {
				var u map[string]any
				if json.Unmarshal(probe.Payload.Usage, &u) == nil && len(u) > 0 {
					usage = u
				}
			}
			return true
		}
		if probe.Type != "response_item" {
			return true
		}
		if probe.Payload.Type != "message" || probe.Payload.Role == "developer" {
			return true
		}
		count++
		if probe.Payload.Role == "assistant" {
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
			"source":       "codex",
			"text":         "",
			"thinking":     "",
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
		"text":         strings.Join(texts, "\n"),
		"thinking":     "",
		"toolCalls":    []any{},
		"usage":        usage,
	}
}
