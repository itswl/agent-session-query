package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Codex：~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl
type CodexSource struct {
	root  string
	cache *fileRecordCache
}

func newCodexSource(root string) *CodexSource {
	return &CodexSource{root: root, cache: newFileRecordCache()}
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

// codexHeadLines 找元数据行时最多往下读几行（防御性上限，正常第一行就是）
const codexHeadLines = 50

// codexSalvageKeys 没有 session_meta 时，可以从别的行零散捡回来的字段。
//
// 实测一个真实 rollout 的行类型分布：session_meta 带全套，turn_context 只带 cwd，
// token_usage_record 只带 session_id——它们是互补的，能凑一点是一点。
//
// 注意这里**没有裸 `id`**：response_item（消息行）的 payload 带 id = "msg_…"，
// 顺手捡的话就会把消息 ID 当成会话 ID 报出去，和 Pi 踩过的是同一个坑。
var codexSalvageKeys = []string{"session_id", "cwd", "cli_version"}

// List 找元数据行；文件没变过就直接用缓存（见 fileRecordCache）
func (s *CodexSource) List() []record {
	return s.cache.records(s.files(), func(path, modISO string) record {
		// 原先只读第一行，首行不是元数据（被截断、或上游加了前导行）就全丢。
		// 现在：session_meta 是权威且完整的那一行，拿到就停；找不到才退而求其次，
		// 从后面的行里把认得出来的字段逐个补上。
		payload := map[string]any{}
		seen := 0
		eachJSONL(path, func(obj map[string]any) bool {
			row := getMap(obj, "payload")
			if obj["type"] == "session_meta" {
				payload = row
				return false
			}
			for _, key := range codexSalvageKeys {
				if !truthy(payload[key]) && truthy(row[key]) {
					payload[key] = row[key]
				}
			}
			seen++
			return seen < codexHeadLines
		})
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		return newRecord(map[string]any{
			"source":     "codex",
			"key":        path,
			"shortKey":   stem,
			"sessionId":  strOr(payload["session_id"], strOr(payload["id"], stem)),
			"file":       path,
			"hasFile":    true,
			"status":     "done",
			"cwd":        getOr(payload, "cwd", ""),
			"cliVersion": getOr(payload, "cli_version", ""),
			"updatedAt":  modISO,
		}, modISO)
	})
}

func (s *CodexSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := r.str("file")
	if path == "" {
		return sink.result()
	}
	eachJSONL(path, func(obj map[string]any) bool {
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
		return sink.add(map[string]any{
			"id":        getOr(payload, "id", ""),
			"role":      role,
			"timestamp": getOr(obj, "timestamp", ""),
			"content":   parts,
		})
	})
	return sink.result()
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
