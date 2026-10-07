package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Claude Code: ~/.claude/projects/<project>/<session-uuid>.jsonl
type ClaudeCodeSource struct {
	root  string
	cache *fileRecordCache
}

func newClaudeSource(root string) *ClaudeCodeSource {
	return &ClaudeCodeSource{root: root, cache: newFileRecordCache(claudeCountMessages)}
}

func (s *ClaudeCodeSource) Mode() string     { return "claude" }
func (s *ClaudeCodeSource) Location() string { return s.root }

func (s *ClaudeCodeSource) Exists() bool { return fileExists(s.root) }

func (s *ClaudeCodeSource) files() []string {
	files, err := filepath.Glob(filepath.Join(s.root, "*", "*.jsonl"))
	if err != nil {
		return nil
	}
	return files
}

// claudeHeadLines caps how far down the file the metadata scan will go.
//
// cwd is not on the first line — sessions open with non-conversation rows such as
// queue-operation. The original code read a fixed first 5 lines; measured locally, cwd
// lands on lines 2-5 with a good number sitting exactly on line 5. That is luck, not a
// guarantee: one more preamble row from Claude Code and cwd slides out of the window,
// after which cwd / project silently go empty and project grouping breaks, without any
// error. So the scan now runs until it has what it needs, and this cap is only a
// defensive backstop.
//
// It is usually faster too: stopping as soon as both fields are in place ends most files
// at line 3, fewer than the fixed 5 it used to read.
const claudeHeadLines = 50

// List reads only the head of each session file for metadata; unchanged files come
// straight from the cache (see fileRecordCache)
func (s *ClaudeCodeSource) List() []record {
	return s.cache.records(s.files(), func(path, modISO string) record {
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		// Use the time on the last record in the file, not the file's mtime (see updatedAtOf)
		updated := updatedAtOf(path, modISO)
		sid := stem
		var cwd any = ""
		branch := ""
		var haveSID, haveCWD, haveBranch bool
		seen := 0
		eachJSONL(path, func(obj map[string]any) bool {
			if !haveSID && truthy(obj["sessionId"]) {
				sid, haveSID = toStr(obj["sessionId"]), true
			}
			if !haveCWD && truthy(obj["cwd"]) {
				cwd, haveCWD = obj["cwd"], true
			}
			// The branch the session opened on. Claude Code writes gitBranch on nearly
			// every row, so the head scan already passes several; a session started
			// outside a repository has none, and that is the one case the scan runs to
			// the cap, which is why the cap is there.
			if !haveBranch && truthy(obj["gitBranch"]) {
				branch, haveBranch = toStr(obj["gitBranch"]), true
			}
			seen++
			return !(haveSID && haveCWD && haveBranch) && seen < claudeHeadLines
		})
		name := firstNonEmpty(claudeUserTitle(path), stem)
		return newRecord(record{
			Source:    "claude",
			Key:       path,
			ShortKey:  name,
			SessionID: sid,
			File:      path,
			HasFile:   true,
			Status:    "done",
			Cwd:       toStr(cwd),
			Branch:    branch,
			UpdatedAt: updated,
		}, updated)
	})
}

// claudeUserTitle is the first real user message of a Claude Code session, as a title.
// A user row's content is a string or a block array; text blocks concatenate. Command
// plumbing and caveat rows open with a tag and are rejected inside titleFromUserText.
func claudeUserTitle(path string) string {
	return firstUserTitle(path, claudeHeadLines, func(obj map[string]any) (string, bool) {
		if obj["type"] != "user" {
			return "", false
		}
		msg, _ := obj["message"].(map[string]any)
		if msg == nil {
			return "", false
		}
		switch content := msg["content"].(type) {
		case string:
			return content, true
		case []any:
			texts := []string{}
			for _, item := range content {
				if m, ok := item.(map[string]any); ok && m["type"] == "text" {
					if t := strField(m, "text"); t != "" {
						texts = append(texts, t)
					}
				}
			}
			return strings.Join(texts, "\n"), len(texts) > 0
		}
		return "", false
	})
}

// claudeInterruptedPrefix opens the text Claude Code writes when the user stops a turn:
// "[Request interrupted by user]" as a user text block, and "[Request interrupted by user
// for tool use]" as the content of the tool_result the stopped call never produced.
const claudeInterruptedPrefix = "[Request interrupted by user"

// claudeRow folds one user or assistant row into the shared block array shape.
//
// names carries tool_use id → tool name across the scan: a tool_result row names only the
// call it answers (tool_use_id), and the name that lets a reader see "the Bash call failed"
// rather than "a tool failed" lives on the assistant row before it.
type claudeRow struct {
	names map[string]string
	full  bool
}

func (c claudeRow) parts(obj map[string]any) []map[string]any {
	msg := getMap(obj, "message")
	parts := []map[string]any{}
	switch content := msg["content"].(type) {
	case string:
		if content != "" {
			parts = append(parts, c.textOrEvent(content))
		}
		return parts
	case []any:
		for _, item := range content {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				if text := strField(m, "text"); text != "" {
					parts = append(parts, c.textOrEvent(text))
				}
			case "thinking":
				parts = append(parts, thinkingBlock(strField(m, "thinking"), c.full))
			case "tool_use":
				id, name := strField(m, "id"), strField(m, "name")
				if id != "" && c.names != nil {
					c.names[id] = name
				}
				parts = append(parts, toolCallBlock(id, name, getOr(m, "input", map[string]any{})))
			case "tool_result":
				parts = append(parts, c.toolResult(obj, m))
			}
		}
	}
	return parts
}

// textOrEvent: the interruption notice is not something the user said, it is something
// that happened, so it becomes an event rather than a user text block (and so does not
// start a round)
func (c claudeRow) textOrEvent(text string) map[string]any {
	if strings.HasPrefix(strings.TrimSpace(text), claudeInterruptedPrefix) {
		return eventBlock(eventInterrupted, strings.TrimSpace(text))
	}
	return textBlock(text)
}

// toolResult pairs the result with its call and records how the call ended. is_error is
// Claude's own verdict; the row-level toolUseResult says when the user interrupted the
// command, and so does the placeholder text a stopped call gets as its result.
func (c claudeRow) toolResult(obj, m map[string]any) map[string]any {
	callID := strField(m, "tool_use_id")
	text := contentText(m["content"])
	block := toolResultBlock(callID, c.names[callID], text, c.full)
	outcome := toolOutcome{status: statusFromError(truthy(m["is_error"]))}
	if truthy(getMap(obj, "toolUseResult")["interrupted"]) || strings.HasPrefix(strings.TrimSpace(text), claudeInterruptedPrefix) {
		outcome.status = statusInterrupted
	}
	return outcome.apply(block)
}

// claudeSystemEvent turns the system rows worth a reader's attention into event blocks:
// a compaction boundary (the context the model sees was rewritten here) and a stop hook
// that failed. The rest — turn_duration, informational, local_command — is bookkeeping
// and yields nothing. Measured locally over 60 sessions: 562 system rows, of which 12
// compactions and 289 hook summaries, most of the latter with no errors.
func claudeSystemEvent(obj map[string]any) map[string]any {
	switch obj["subtype"] {
	case "compact_boundary":
		return eventBlock(eventCompaction, strOr(obj["content"], "Context compacted"))
	case "stop_hook_summary":
		errs := []string{}
		for _, raw := range getSlice(obj, "hookErrors") {
			if text := contentText(raw); text != "" {
				errs = append(errs, text)
			}
		}
		if len(errs) == 0 {
			return nil
		}
		return eventBlock(eventHookError, strings.Join(errs, "\n"))
	}
	return nil
}

// claudeProbe is the small decode every whole-file scan does per row: enough to decide
// whether the row counts as a message and to pick up usage, nothing more. counts is the
// one rule Messages, Final and the background counter all follow, so the number in the
// list is the number of rows the reader will get.
type claudeProbe struct {
	Type        string            `json:"type"`
	Subtype     string            `json:"subtype"`
	IsSidechain bool              `json:"isSidechain"`
	HookErrors  []json.RawMessage `json:"hookErrors"`
	Message     struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
}

func (p claudeProbe) counts() bool {
	if p.IsSidechain {
		return false
	}
	switch p.Type {
	case "user", "assistant":
		return true
	case "system":
		return p.Subtype == "compact_boundary" || (p.Subtype == "stop_hook_summary" && len(p.HookErrors) > 0)
	}
	return false
}

func (s *ClaudeCodeSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := r.File
	if path == "" {
		return sink.result()
	}
	row := claudeRow{names: map[string]string{}, full: q.full}
	eachJSONL(path, func(obj map[string]any) bool {
		if truthy(obj["isSidechain"]) { // subagent messages are not part of the main thread
			return true
		}
		switch obj["type"] {
		case "system":
			event := claudeSystemEvent(obj)
			if event == nil {
				return true
			}
			return sink.add(map[string]any{
				"id":        getOr(obj, "uuid", ""),
				"role":      "system",
				"timestamp": getOr(obj, "timestamp", ""),
				"content":   []map[string]any{event},
			})
		case "user", "assistant":
		default:
			return true
		}
		msg := getMap(obj, "message")
		return sink.add(map[string]any{
			"id":        getOr(obj, "uuid", ""),
			"role":      strOr(msg["role"], strOr(obj["type"], "")),
			"timestamp": getOr(obj, "timestamp", ""),
			"content":   row.parts(obj),
		})
	})
	return sink.result()
}

func (s *ClaudeCodeSource) Final(r record) map[string]any {
	path := r.File
	if path == "" {
		return nil
	}
	// Only the probe fields matter here; big fields like content wait until the last
	// assistant message, which is the only one fully decoded
	var rawLast []byte
	count := 0
	// Usage is summed over the whole session rather than taken from the last message: the
	// last message answers "what did the final answer cost", which is not what a Usage
	// card is read as. The scan already walks every line, so this costs one small decode
	// per assistant message.
	var totals usageTotals
	eachJSONLLine(path, func(line []byte) bool {
		var probe claudeProbe
		if json.Unmarshal(line, &probe) != nil || !probe.counts() {
			return true
		}
		count++
		if probe.Type == "assistant" {
			rawLast = append(rawLast[:0], line...)
			if len(probe.Message.Usage) > 0 {
				var u map[string]any
				if json.Unmarshal(probe.Message.Usage, &u) == nil {
					totals.add(u)
				}
			}
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
			"source":       "claude",
			"text":         "",
			"thinking":     "",
		}
	}

	msg := getMap(lastAssistant, "message")
	parts := claudeRow{}.parts(lastAssistant)
	texts := []string{}
	thoughts := []string{}
	toolCalls := []any{}
	for _, p := range parts {
		switch p["type"] {
		case "text":
			if c := strField(p, "content"); c != "" {
				texts = append(texts, c)
			}
		case "thinking":
			if c := strField(p, "content"); c != "" {
				thoughts = append(thoughts, c)
			}
		case "toolCall":
			call := map[string]any{
				"name":      strField(p, "name"),
				"arguments": getOr(p, "arguments", map[string]any{}),
			}
			if id := strField(p, "id"); id != "" {
				call["id"] = id
			}
			toolCalls = append(toolCalls, call)
		}
	}
	stopReason := strOr(msg["stop_reason"], "")
	isFinal := stopReason == "end_turn" || stopReason == "stop" || stopReason == "stop_sequence"
	return map[string]any{
		"status":       "done",
		"isFinal":      isFinal,
		"isProcessing": false,
		"messageCount": count,
		"source":       "claude",
		"id":           getOr(msg, "id", ""),
		"timestamp":    getOr(lastAssistant, "timestamp", ""),
		"stopReason":   stopReason,
		"model":        getOr(msg, "model", ""),
		"text":         strings.Join(texts, "\n"),
		"thinking":     strings.Join(thoughts, "\n"),
		"toolCalls":    toolCalls,
		"usage":        totals.result(),
	}
}
