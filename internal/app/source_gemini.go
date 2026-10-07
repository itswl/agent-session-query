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
	return &GeminiSource{root: root, cache: newFileRecordCache(geminiCountMessages)}
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

// geminiFolder folds Gemini messages into the shared block array across one scan.
//
// Tool-driven sessions carry very little prose: when a call is issued, content is the
// empty string and the substance sits in toolCalls (id + name + args). Where the result
// lands has changed over Gemini CLI's versions. Older files answer on a later user row, as
// a functionResponse entry in its content array, and echo that same response under the
// call's result field without the real output — so a result field alone is not a result.
// Newer files write a status onto the toolCalls entry, with the output as resultDisplay
// (a string, or a file diff object) or under result. A status is the signal: with one,
// the call is answered in place and the user row's copy, if any, is skipped; without one,
// the user row is the answer. statuses carries a status that arrived without any output
// to the user row that brings it. thoughts goes first, matching the other sources.
type geminiFolder struct {
	full     bool
	answered map[string]bool
	statuses map[string]string
}

func (g geminiFolder) parts(m map[string]any) []map[string]any {
	parts := []map[string]any{}
	switch content := m["content"].(type) {
	case string:
		if content != "" {
			parts = append(parts, textBlock(content))
		}
	case []any:
		for _, item := range content {
			mm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := mm["text"].(string); ok && text != "" {
				parts = append(parts, textBlock(text))
			}
			if fr, ok := mm["functionResponse"].(map[string]any); ok {
				id := strOr(fr["id"], "")
				if id != "" && g.answered != nil && g.answered[id] {
					continue // already answered on the call itself
				}
				parts = append(parts, g.result(id, strOr(fr["name"], ""), getMap(fr, "response"), "", g.statusOf(id)))
			}
		}
	}
	for _, tc := range getSlice(m, "toolCalls") {
		mm, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		id, name := strOr(mm["id"], ""), strOr(mm["name"], "")
		parts = append(parts, toolCallBlock(id, name, getOr(mm, "args", map[string]any{})))
		// The newer shape answers the call in place: a status, and the output as
		// resultDisplay (a string, or a file diff object) or under result[]
		status := strOr(mm["status"], "")
		switch status {
		case "":
			continue // the older shape: the user row that follows carries the answer
		case "executing", "pending", "scheduled", "validating", "awaiting_approval":
			continue // still running when the file was written: no result yet
		}
		display := ""
		switch d := mm["resultDisplay"].(type) {
		case string:
			display = d
		case map[string]any:
			display = strOr(d["fileDiff"], "")
		}
		var response map[string]any
		for _, r := range getSlice(mm, "result") {
			if rm, ok := r.(map[string]any); ok {
				response = getMap(getMap(rm, "functionResponse"), "response")
				break
			}
		}
		if display == "" && response == nil {
			// A verdict without output: keep it for the user row that brings the output
			if id != "" && g.statuses != nil {
				g.statuses[id] = status
			}
			continue
		}
		parts = append(parts, g.result(id, name, response, display, status))
		if id != "" && g.answered != nil {
			g.answered[id] = true
		}
	}
	if thoughts := geminiThoughts(m["thoughts"]); thoughts != "" {
		parts = append([]map[string]any{thinkingBlock(thoughts, g.full)}, parts...)
	}
	return parts
}

// statusOf is the status a call's entry recorded without any output, for the user row
func (g geminiFolder) statusOf(id string) string {
	if g.statuses == nil {
		return ""
	}
	return g.statuses[id]
}

// result builds a toolResult from whichever of the two shapes supplied it: the response
// object's output or error, or the display text. An error field is the verdict when the
// entry carries no status of its own.
func (g geminiFolder) result(id, name string, response map[string]any, display, status string) map[string]any {
	output := contentText(response["output"])
	errText := contentText(response["error"])
	content := firstNonEmpty(display, output, errText)
	outcome := toolOutcome{}
	switch status {
	case "success", "completed":
		outcome.status = statusOK
	case "error":
		outcome.status = statusError
	case "cancelled", "canceled":
		outcome.status = statusInterrupted
	default:
		if errText != "" {
			outcome.status = statusError
		} else if response != nil {
			outcome.status = statusOK
		}
	}
	return outcome.apply(toolResultBlock(id, name, content, g.full))
}

// eachGeminiEntry yields messages line by line. A Gemini jsonl is an append log shaped
// as "metadata first line + $set patches + message rows".
func eachGeminiEntry(path string, fn func(entry map[string]any) bool) {
	eachGeminiEntryFrom(path, 0, fn)
}

// eachGeminiEntryFrom is eachGeminiEntry starting at a byte offset. Each row — a plain
// entry or a $set patch — stands on its own, so a tail scan sees exactly what the full
// scan saw for those rows (see eachJSONLLineFrom).
func eachGeminiEntryFrom(path string, from int64, fn func(entry map[string]any) bool) {
	eachJSONLFrom(path, from, func(obj map[string]any) bool {
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
	// A continuation file's messages belong to the same session, so the counts add —
	// otherwise the list would report one file's worth while the session reports both
	if aCount, ok := toFloat(fields["messageCount"]); ok {
		if bCount, ok := toFloat(b.fields["messageCount"]); ok {
			fields["messageCount"] = int(aCount) + int(bCount)
		}
	}
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
	folder := geminiFolder{full: q.full, answered: map[string]bool{}, statuses: map[string]string{}}
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
				"content":   folder.parts(m),
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
		if len(out) >= q.probeLimit() {
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
	for _, p := range (geminiFolder{}).parts(lastGemini) {
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
	thinking, _ := clip(geminiThoughts(lastGemini["thoughts"]), thinkingLimit, false)
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
