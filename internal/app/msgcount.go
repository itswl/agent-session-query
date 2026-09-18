package app

import (
	"bytes"
	"encoding/json"
)

// Counting a session file's messages, for the list.
//
// The list shows a message count without the session being opened, and the number has to
// be the one the session itself reports — a count that disagreed with the final result
// would just be a second wrong number. So each rule here mirrors the corresponding
// source's Final: claude counts user/assistant rows that are not sidechains, codex counts
// response_item messages that are not the developer's instructions, and so on.
//
// Counting means reading the whole file, which is exactly what the list avoids, so this
// runs in the background once per file version (see fileRecordCache). Each counter filters
// on the raw bytes before decoding: a decode per line would cost several times more than
// the byte scan, and most lines are not messages.

// lineHasAll is a cheap pre-filter: every token must appear in the line before it is worth
// decoding. Order is irrelevant, only presence.
func lineHasAll(line []byte, tokens ...string) bool {
	for _, t := range tokens {
		if !bytes.Contains(line, []byte(t)) {
			return false
		}
	}
	return true
}

// claudeCountMessages: user/assistant rows, sidechains excluded (see ClaudeCodeSource.Final)
func claudeCountMessages(path string) int {
	n := 0
	eachJSONLLine(path, func(line []byte) bool {
		if !lineHasAll(line, `"type"`, `"message"`) {
			return true
		}
		var probe struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
		}
		if json.Unmarshal(line, &probe) != nil {
			return true
		}
		if (probe.Type == "user" || probe.Type == "assistant") && !probe.IsSidechain {
			n++
		}
		return true
	})
	return n
}

// piCountMessages: type=message rows (see PiSource.Final)
func piCountMessages(path string) int {
	n := 0
	eachJSONLLine(path, func(line []byte) bool {
		if !bytes.Contains(line, []byte(`"type":"message"`)) {
			return true
		}
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.Type == "message" {
			n++
		}
		return true
	})
	return n
}

// codexCountMessages: response_item messages, the developer's assembled instructions
// excluded (see CodexSource.Final)
func codexCountMessages(path string) int {
	n := 0
	eachJSONLLine(path, func(line []byte) bool {
		if !lineHasAll(line, `"response_item"`, `"message"`) {
			return true
		}
		var probe struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
				Role string `json:"role"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &probe) != nil {
			return true
		}
		if probe.Type == "response_item" && probe.Payload.Type == "message" && probe.Payload.Role != "developer" {
			n++
		}
		return true
	})
	return n
}

// geminiCountMessages counts the conversation rows; eachGeminiEntry already reduces the
// append log to its entries, so the rule matches GeminiSource.Final exactly.
func geminiCountMessages(path string) int {
	n := 0
	eachGeminiEntry(path, func(m map[string]any) bool {
		if m["type"] == "user" || m["type"] == "gemini" {
			n++
		}
		return true
	})
	return n
}
