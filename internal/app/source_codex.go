package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Codex: ~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl
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

// codexHeadLines caps how far the metadata scan goes (a defensive backstop; normally
// the very first line is the one)
const codexHeadLines = 50

// codexSalvageKeys are the fields that can be picked up piecemeal from other rows when
// there is no session_meta.
//
// Measured against a real rollout: session_meta carries the full set, turn_context only
// cwd, token_usage_record only session_id — they complement each other, so salvaging
// what is available is worth it.
//
// Note there is no bare `id` here. A response_item (message row) payload carries
// id = "msg_...", and picking that up would report a message ID as the session ID —
// exactly the trap Pi fell into.
var codexSalvageKeys = []string{"session_id", "cwd", "cli_version"}

// List finds the metadata row; unchanged files come straight from the cache
// (see fileRecordCache)
// codexUserTitle is the first real user message of a Codex session, as a title. The
// opening instruction rows are role=user too but assembled by the CLI (AGENTS.md
// instructions, environment context); titleFromUserText rejects them by their openings.
func codexUserTitle(path string) string {
	return firstUserTitle(path, codexHeadLines, func(obj map[string]any) (string, bool) {
		if obj["type"] != "response_item" {
			return "", false
		}
		payload, _ := obj["payload"].(map[string]any)
		if payload == nil || payload["type"] != "message" || payload["role"] != "user" {
			return "", false
		}
		return blockArrayText(payload["content"])
	})
}

func (s *CodexSource) List() []record {
	return s.cache.records(s.files(), func(path, modISO string) record {
		// This used to read only the first line, losing everything when that line was not
		// the metadata (truncated file, or a new preamble row upstream). Now: session_meta
		// is the authoritative and complete row, so stop as soon as it appears; only when
		// it never does fall back to salvaging recognisable fields from later rows.
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
		// Use the time on the last record in the file, not the file's mtime (see updatedAtOf)
		updated := updatedAtOf(path, modISO)
		return newRecord(map[string]any{
			"source":     "codex",
			"key":        path,
			"shortKey":   firstNonEmpty(codexUserTitle(path), stem),
			"sessionId":  strOr(payload["session_id"], strOr(payload["id"], stem)),
			"file":       path,
			"hasFile":    true,
			"status":     "done",
			"cwd":        getOr(payload, "cwd", ""),
			"cliVersion": getOr(payload, "cli_version", ""),
			"updatedAt":  updated,
		}, updated)
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
		if role == "developer" { // machine-assembled instructions, not conversation
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
	// Only type / payload.type / payload.role matter (plus the small usage object)
	var rawLast []byte
	count := 0
	// Every token_usage_record is a turn's usage; summing them gives the session total,
	// where keeping only the last gave the final turn's (see source_claude)
	var totals usageTotals
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
					totals.add(u)
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
		"usage":        totals.result(),
	}
}
