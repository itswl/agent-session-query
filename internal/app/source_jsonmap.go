package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// JsonMapSource：OpenClaw / Hermes —— 一个 sessions.json 索引 + 每会话一个 jsonl。
type JsonMapSource struct {
	def jsonMapDef
}

func newJsonMapSource(def jsonMapDef) *JsonMapSource { return &JsonMapSource{def: def} }

func (s *JsonMapSource) Mode() string { return s.def.mode }

// Location 返回实际存在的那份索引（sessions.json 优先；新版 Hermes 只有 state.db）
func (s *JsonMapSource) Location() string {
	if !fileExists(s.def.sessionsJSON) && s.def.stateDB != "" && fileExists(s.def.stateDB) {
		return s.def.stateDB
	}
	return s.def.sessionsJSON
}

func (s *JsonMapSource) Exists() bool {
	return fileExists(s.def.sessionsJSON) || (s.def.stateDB != "" && fileExists(s.def.stateDB))
}

// kv 保留 sessions.json 里的原始顺序（Go map 不保序，而顺序会影响同分记录的先后）
type kv struct {
	Key string
	Val any
}

// load 读取 sessions.json；不是 JSON 对象时返回空。
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
		var updatedRaw any // 交给 newRecord 解析成排序用的时间

		if isOpenClaw {
			updated := info["updatedAt"] // epoch 毫秒
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

	// 新版 Hermes：会话全部在 state.db 里，sessions.json 可能根本不存在；
	// 已列出的 sessionId 跳过，避免双重列出。
	// 先 stat 一下：库不存在时没必要每次列表都去开一个连接、再报一行错。
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

// Messages：OpenClaw / Hermes 的消息格式由行内容自辨（type=message 或 role=user/assistant）。
func (s *JsonMapSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := s.fileOf(r)
	if path == "" {
		// 没有会话文件（新版 Hermes 全 SQLite）：消息也直接查 state.db
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

	// 第一个 stop 消息之后还有指向它的 toolResult，说明可能仍在处理中
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

// hermesSQLiteFallback：Hermes 的 webhook 会话有时只把最终消息落在 state.db，
// jsonl 里什么都没有，这时只能去 SQLite 里捞（见 hermes_sqlite.go）。
//
// 用数据源自己配置的 stateDB 路径——List / Messages 一直是这么做的，
// 这里也一样，不再自己从 $HOME 重推一遍（两条路径算出来不一样时会静默查错库）。
func hermesSQLiteFallback(def jsonMapDef, sessionID, status string) map[string]any {
	if def.stateDB == "" {
		return nil
	}
	return hermesSQLiteFinal(def.stateDB, def.mode, sessionID, status)
}
