package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Pi: ~/.pi/agent/sessions/<project>/<time>_<uuid>.jsonl
type PiSource struct {
	root  string
	cache *fileRecordCache
}

func newPiSource(root string) *PiSource {
	return &PiSource{root: root, cache: newFileRecordCache()}
}

func (s *PiSource) Mode() string     { return "pi" }
func (s *PiSource) Location() string { return s.root }

func (s *PiSource) Exists() bool { return fileExists(s.root) }

func (s *PiSource) files() []string {
	files, err := filepath.Glob(filepath.Join(s.root, "*", "*.jsonl"))
	if err != nil {
		return nil
	}
	return files
}

// piHeadLines caps how far the scan for the session row goes (a defensive backstop;
// normally the very first line is the one)
const piHeadLines = 50

// piSessionID is the fallback when there is no session row: the filename is shaped
// <time>_<uuid>, so take the last segment. Normal sessions never need this — only
// truncated or resumed files lack the session row.
func piSessionID(stem string) string {
	if i := strings.LastIndexByte(stem, '_'); i >= 0 && i+1 < len(stem) {
		return stem[i+1:]
	}
	return stem
}

// piUserTitle is the first real user message of a Pi session, as a title. A message
// row's content is a block array or a plain string.
func piUserTitle(path string) string {
	return firstUserTitle(path, piHeadLines, func(obj map[string]any) (string, bool) {
		if obj["type"] != "message" {
			return "", false
		}
		msg, _ := obj["message"].(map[string]any)
		if msg == nil || msg["role"] != "user" {
			return "", false
		}
		return blockArrayText(msg["content"])
	})
}

// blockArrayText concatenates the text of a content block array, or returns the value
// itself when it is a plain string.
func blockArrayText(content any) (string, bool) {
	switch c := content.(type) {
	case string:
		return c, true
	case []any:
		texts := []string{}
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				if t := strField(m, "text"); t != "" {
					texts = append(texts, t)
				}
			}
		}
		return strings.Join(texts, "\n"), len(texts) > 0
	}
	return "", false
}

// List finds the session row for metadata; unchanged files come straight from the
// cache (see fileRecordCache)
func (s *PiSource) List() []record {
	return s.cache.records(s.files(), func(path, modISO string) record {
		// It has to be the type=session row specifically. The original code took the first
		// line's id unconditionally — but a model_change record carries its own id field, so
		// whenever the first line was not the session row it reported an event id as the
		// session id. (Measured locally, this really happened: it reported "e74f2cff"
		// instead of the uuid in the filename.)
		meta := map[string]any{}
		seen := 0
		eachJSONL(path, func(obj map[string]any) bool {
			if obj["type"] == "session" {
				meta = obj
				return false
			}
			seen++
			return seen < piHeadLines
		})
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		// Use the time on the last record in the file, not the file's mtime (see updatedAtOf)
		updated := updatedAtOf(path, modISO)
		return newRecord(map[string]any{
			"source":    "pi",
			"key":       path,
			"shortKey":  firstNonEmpty(piUserTitle(path), stem),
			"sessionId": strOr(meta["id"], piSessionID(stem)),
			"file":      path,
			"hasFile":   true,
			"status":    "done",
			"cwd":       getOr(meta, "cwd", ""),
			"updatedAt": updated,
		}, updated)
	})
}

func (s *PiSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := r.str("file")
	if path == "" {
		return sink.result()
	}
	eachJSONL(path, func(obj map[string]any) bool {
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
		return sink.add(map[string]any{
			"id":        getOr(obj, "id", ""),
			"role":      strOr(msg["role"], "unknown"),
			"timestamp": getOr(msg, "timestamp", getOr(obj, "timestamp", "")),
			"content":   parts,
		})
	})
	return sink.result()
}

func (s *PiSource) Final(r record) map[string]any {
	path := r.str("file")
	if path == "" {
		return nil
	}
	// This whole-file scan only cares about type / message.role, so probe with a struct
	// first and never materialise the big fields (content, toolResult) into a map —
	// measured over 3x faster than decoding everything
	var rawLast []byte
	count := 0
	// Usage is summed over the session, not read off the last message (see source_claude)
	var totals usageTotals
	eachJSONLLine(path, func(line []byte) bool {
		var probe struct {
			Type    string `json:"type"`
			Message struct {
				Role  string          `json:"role"`
				Usage json.RawMessage `json:"usage"`
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
			// the buffer is reused; copy anything kept
			rawLast = append(rawLast[:0], line...)
		}
		if len(probe.Message.Usage) > 0 {
			var u map[string]any
			if json.Unmarshal(probe.Message.Usage, &u) == nil {
				totals.add(u)
			}
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
		"usage":        totals.result(),
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
