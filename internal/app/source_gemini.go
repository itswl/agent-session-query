package app

import (
	"path/filepath"
	"strings"
)

// Gemini CLI: ~/.gemini/tmp/<project>/chats/session-*.jsonl
type GeminiSource struct {
	root  string
	cache *fileRecordCache
}

func newGeminiSource(root string) *GeminiSource {
	return &GeminiSource{root: root, cache: newFileRecordCache()}
}

func (s *GeminiSource) Mode() string     { return "gemini" }
func (s *GeminiSource) Location() string { return s.root }

func (s *GeminiSource) Exists() bool { return fileExists(s.root) }

func (s *GeminiSource) files() []string {
	files, err := filepath.Glob(filepath.Join(s.root, "*", "chats", "session-*.jsonl"))
	if err != nil {
		return nil
	}
	return files
}

// metaHeadLines caps how far metaOf looks. The metadata is on the first line, but
// without a hard cap one file missing it would turn "list the sessions" into a
// whole-file scan.
const metaHeadLines = 5

// metaOf reads the file head for metadata (for listing; never scans the whole file)
func (s *GeminiSource) metaOf(path string) map[string]any {
	meta := map[string]any{}
	seen := 0
	eachJSONL(path, func(obj map[string]any) bool {
		if truthy(obj["sessionId"]) {
			meta = obj
			return false
		}
		seen++
		return seen < metaHeadLines
	})
	return meta
}

// geminiThoughts flattens thoughts into text. Gemini CLI has two generations of the
// format: a plain string, or a [{subject, description, timestamp}] array (itemised
// thinking used when planning or reporting on itself).
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

// geminiParts folds one Gemini message into the shared block array. Tool-driven
// sessions carry very little prose: when a call is issued, content is the empty string
// and the substance sits in toolCalls (name + args, no result — the result arrives as a
// functionResponse on a later user row). A user row's content array carries those
// functionResponse entries back. thoughts goes first, matching the other sources.
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

// eachGeminiEntry yields messages line by line. A Gemini jsonl is an append log shaped
// as "metadata first line + $set patches + message rows".
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

// List reads only the file head for metadata; unchanged files come straight from the
// cache (see fileRecordCache)
func (s *GeminiSource) List() []record {
	return s.cache.records(s.files(), func(path, modISO string) record {
		meta := s.metaOf(path)
		// The first line's lastUpdated is its value at session start; later $set patch rows
		// update it. Measured locally, 29 of 33 sessions had a stale first-line time, off by
		// as much as 45 minutes. So look at the last record in the tail first, then fall back
		// to the head metadata, and only then to the file's mtime.
		var lastTs any = updatedAtOf(path, "")
		if !truthy(lastTs) {
			lastTs = meta["lastUpdated"]
		}
		if !truthy(lastTs) {
			lastTs = meta["startTime"]
		}
		if !truthy(lastTs) {
			lastTs = modISO
		}
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		return newRecord(map[string]any{
			"source":    "gemini",
			"key":       path,
			"shortKey":  stem,
			"sessionId": strOr(meta["sessionId"], stem),
			"file":      path,
			"hasFile":   true,
			"status":    "done",
			"project":   filepath.Base(filepath.Dir(filepath.Dir(path))),
			"updatedAt": lastTs,
		}, lastTs)
	})
}

func (s *GeminiSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := r.str("file")
	if path == "" {
		return sink.result()
	}
	eachGeminiEntry(path, func(m map[string]any) bool {
		if m["type"] != "user" && m["type"] != "gemini" {
			return true
		}
		role := "user"
		if m["type"] == "gemini" {
			role = "assistant"
		}
		return sink.add(map[string]any{
			"id":        getOr(m, "id", ""),
			"role":      role,
			"timestamp": getOr(m, "timestamp", ""),
			"content":   geminiParts(m),
		})
	})
	return sink.result()
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
