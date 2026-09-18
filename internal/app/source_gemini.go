package app

import (
	"context"
	"path/filepath"
	"sort"
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
// geminiUserTitle is the first real user message of a Gemini CLI session, as a title.
// content is a string or a block array; the first metadata line has no type and is not a
// user row, so the shared head scan skips it naturally.
// geminiHeadLines caps how far the title scan goes, matching the other file sources.
const geminiHeadLines = 50

func geminiUserTitle(path string) string {
	return firstUserTitle(path, geminiHeadLines, func(obj map[string]any) (string, bool) {
		if obj["type"] != "user" {
			return "", false
		}
		return blockArrayText(obj["content"])
	})
}

func (s *GeminiSource) List() []record {
	records := s.cache.records(s.files(), func(path, modISO string) record {
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
			"shortKey":  firstNonEmpty(geminiUserTitle(path), stem),
			"sessionId": strOr(meta["sessionId"], stem),
			"file":      path,
			"files":     []string{path},
			"hasFile":   true,
			"status":    "done",
			"project":   filepath.Base(filepath.Dir(filepath.Dir(path))),
			"updatedAt": lastTs,
		}, lastTs)
	})
	return mergeGeminiSessions(records)
}

// mergeGeminiSessions folds files that share a sessionId into one record.
//
// Gemini CLI continues a session in a new file (measured locally: two files one minute
// apart, same sessionId) — one conversation split across files, not two sessions.
// Before this merge each file was its own record, and the damage was twofold: the list
// showed two rows that clicked to the same place (the UI keys rows by sessionId, so the
// second row's click looked like re-selecting the first and did nothing), and the other
// file's messages were unreachable — matching by sessionId returns one record.
func mergeGeminiSessions(records []record) []record {
	out := make([]record, 0, len(records))
	index := map[string]int{}
	for _, rec := range records {
		id := rec.str("sessionId")
		if id == "" {
			out = append(out, rec)
			continue
		}
		if at, ok := index[id]; ok {
			out[at] = mergeGeminiRecord(out[at], rec)
			continue
		}
		index[id] = len(out)
		out = append(out, rec)
	}
	return out
}

// mergeGeminiRecord combines two files of one session: files keep filename order (the
// filename carries the timestamp, so that is chronological), the title comes from
// whichever file has one, and updatedAt from whichever is newer.
func mergeGeminiRecord(a, b record) record {
	files := append(geminiFilesOf(a), geminiFilesOf(b)...)
	sort.Strings(files)
	fields := map[string]any{}
	for k, v := range a.fields {
		fields[k] = v
	}
	fields["files"] = files
	fields["file"] = files[0]
	if toStr(fields["shortKey"]) == "" {
		fields["shortKey"] = b.str("shortKey")
	}
	updatedAt := a.str("updatedAt")
	if b.sortAt.After(a.sortAt) {
		updatedAt = b.str("updatedAt")
	}
	return newRecord(fields, updatedAt)
}

// geminiFilesOf lists the files behind one record: the merged set, or the single file.
func geminiFilesOf(r record) []string {
	if raw, ok := r.fields["files"].([]string); ok && len(raw) > 0 {
		return raw
	}
	if path := r.str("file"); path != "" {
		return []string{path}
	}
	return nil
}

func (s *GeminiSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	// One session may span several files (see mergeGeminiSessions); filename order is
	// chronological, so reading them in sequence reconstructs the conversation
	for _, path := range geminiFilesOf(r) {
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
	}
	return sink.result()
}

// Search covers every file of a session; the generic path only reads rec's single
// file field and would miss the continuation files.
func (s *GeminiSource) Search(ctx context.Context, r record, q searchQuery) []map[string]any {
	var out []map[string]any
	for _, path := range geminiFilesOf(r) {
		out = append(out, searchFile(ctx, path, q)...)
		if len(out) >= q.perSession {
			break
		}
	}
	return out
}

func (s *GeminiSource) Final(r record) map[string]any {
	if len(geminiFilesOf(r)) == 0 {
		return nil
	}
	var lastGemini map[string]any
	count := 0
	// Usage is summed over the session, not read off the last row (see source_claude)
	var totals usageTotals
	// Files are chronological, so the last gemini row of the last file is the final
	// answer, while the count covers the whole session
	for _, path := range geminiFilesOf(r) {
		eachGeminiEntry(path, func(m map[string]any) bool {
			if m["type"] != "user" && m["type"] != "gemini" {
				return true
			}
			count++
			if m["type"] == "gemini" {
				lastGemini = m
				if tokens, ok := m["tokens"].(map[string]any); ok {
					totals.add(tokens)
				}
			}
			return true
		})
	}
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
		"usage":        totals.result(),
	}
}
