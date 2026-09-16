package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// JsonMapSource：OpenClaw / Hermes —— 一个 sessions.json 索引 + 每会话一个 jsonl。
type JsonMapSource struct {
	def jsonMapDef
}

func newJsonMapSource(def jsonMapDef) *JsonMapSource { return &JsonMapSource{def: def} }

func (s *JsonMapSource) Mode() string     { return s.def.mode }
func (s *JsonMapSource) Location() string { return s.def.sessionsJSON }

func (s *JsonMapSource) Exists() bool {
	_, err := os.Stat(s.def.sessionsJSON)
	return err == nil
}

// kv 保留 sessions.json 里的原始顺序（Go map 不保序，而顺序会影响同分记录的先后）
type kv struct {
	Key string
	Val any
}

// load 读取 sessions.json；不是 JSON 对象时返回空（与 Python 的失败形态一致）。
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

// fileOf 取会话记录对应的 jsonl 文件路径。
func (s *JsonMapSource) fileOf(r record) string {
	if path := r.str("file"); path != "" {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	if sid := r.str("sessionId"); sid != "" {
		alt := filepath.Join(s.def.sessionsDir, sid+".jsonl")
		if _, err := os.Stat(alt); err == nil {
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
		hasFile := false
		if file != "" {
			if _, err := os.Stat(file); err == nil {
				hasFile = true
			}
		}
		if !hasFile && sid != "" {
			alt := filepath.Join(s.def.sessionsDir, sid+".jsonl")
			if _, err := os.Stat(alt); err == nil {
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
		sortKey := ""

		if isOpenClaw {
			updated := info["updatedAt"]
			updatedStr := ""
			if truthy(updated) {
				if ms, ok := toFloat(updated); ok {
					if dashed, iso, ok := utcFromSeconds(ms / 1000); ok {
						updatedStr, sortKey = dashed, iso
					} else {
						updatedStr = toStr(updated)
					}
				} else {
					updatedStr = toStr(updated)
				}
			}
			fields["status"] = getOr(info, "status", "unknown")
			fields["updatedAt"] = updatedStr
			fields["model"] = getOr(info, "model", "")
			fields["runtimeMs"] = getOr(info, "runtimeMs", float64(0))
			fields["totalTokens"] = getOr(info, "totalTokens", float64(0))
		} else { // hermes
			sortKey = toStr(info["updated_at"])
			fields["status"] = "done"
			fields["updatedAt"] = getOr(info, "updated_at", "")
			fields["createdAt"] = getOr(info, "created_at", "")
			fields["displayName"] = getOr(info, "display_name", "")
			fields["platform"] = getOr(info, "platform", "")
			fields["totalTokens"] = getOr(info, "total_tokens", float64(0))
			fields["estimatedCostUsd"] = getOr(info, "estimated_cost_usd", float64(0))
		}

		out = append(out, record{fields: fields, sortKey: sortKey})
	}
	return out
}

// Messages：OpenClaw / Hermes 的消息格式由行内容自辨（type=message 或 role=user/assistant）。
func (s *JsonMapSource) Messages(r record, limit int) []map[string]any {
	out := []map[string]any{}
	path := s.fileOf(r)
	if path == "" {
		return out
	}
	eachJSONL(path, func(obj map[string]any) bool {
		if len(out) >= limit {
			return false
		}
		if obj["type"] == "message" {
			out = append(out, s.formatMessage(obj))
		} else if role := obj["role"]; role == "user" || role == "assistant" {
			out = append(out, s.formatMessage(obj))
		}
		return true
	})
	return out
}

// formatMessage 格式化单条消息（OpenClaw content 数组 / Hermes 字符串）。
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

// stopMessage 记录「第一条 stopReason=stop 的助手消息」及其解析出的 stopReason
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
		if result := hermesSQLiteFallback(s.def.mode, sessionID); result != nil {
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

	// 第一个 stop 消息之后还有指向它的 toolResult，说明可能仍在处理中
	isProcessing := false
	if firstStop != nil && status == "running" && toolResultParents[strField(firstStop.line, "id")] {
		isProcessing = true
	}

	if firstStop == nil {
		if result := hermesSQLiteFallback(s.def.mode, sessionID); result != nil {
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

// hermesSQLiteFallback：Hermes 的 webhook 会话有时只把最终消息落在 ~/.hermes/state.db。
// Go 版不含 SQLite 依赖（保持零依赖单二进制），这里只提示一次行为差异。
func hermesSQLiteFallback(mode, sessionID string) map[string]any {
	if mode != "hermes" || sessionID == "" {
		return nil
	}
	dbPath := filepath.Join(defaultHome(), ".hermes", "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "[WARN] hermes 会话 %s 没有 jsonl 文件；Go 版未实现 state.db 回退（Python 版有），详见 README\n", sessionID)
	return nil
}
