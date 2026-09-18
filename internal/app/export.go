package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Session export to Markdown.
//
// Pasting a debugging session into an issue, a document, or a message to a colleague
// otherwise means copying it out block by block. The output reuses the Messages / Final
// block structures rather than introducing a second way to parse a session.

// Export formats. Markdown is the document you read or paste somewhere; JSONL is one
// JSON object per line, with a header line naming the session and a final line carrying
// the result — the shape a program (or a model writing a program) queries with jq or a
// dozen lines of Python, rather than reading.
const (
	exportFormatMarkdown = "md"
	exportFormatJSONL    = "jsonl"
)

// exportSession resolves one session and renders it in the requested format.
func (a *SessionQueryAPI) exportSession(pattern string, q messageQuery, format string) (body, filename, contentType string, ok bool) {
	source, item, found := a.findSession(pattern, "")
	if !found {
		return "", "", "", false
	}
	messages := safeParse(source.Mode(), "messages", func() []map[string]any {
		return source.Messages(item, q)
	})
	final := safeParse(source.Mode(), "the final result", func() map[string]any {
		return source.Final(item)
	})

	name := strOr(item.get("shortKey"), item.str("sessionId"))
	stem := sanitizeFilename(name)

	if format == exportFormatJSONL {
		return renderExportJSONL(item, messages, final, q),
			stem + ".jsonl", "application/x-ndjson; charset=utf-8", true
	}
	return renderExportMarkdown(item, messages, final, q),
		stem + ".md", "text/markdown; charset=utf-8", true
}

// exportCoverage says how much of the session a document holds.
//
// The record's count comes from the background pass and may not have arrived yet; the
// final result carries a true count of its own, computed by the scan that produced it.
// Falling back to that means the document can always state its coverage, and it costs
// nothing since Final has already run.
//
// Completeness is only claimed when the count written matches the count reported. If they
// disagree — a source that counts a message differently from the way it lists one — the
// document says how many it holds and no more, rather than announcing a total it cannot
// back.
func exportCoverage(item record, final map[string]any, written int, which string) (coverage string, complete bool) {
	total := int64(0)
	if n, ok := toFloat(item.get("messageCount")); ok && n > 0 {
		total = int64(n)
	} else if final != nil {
		if n, ok := toFloat(final["messageCount"]); ok && n > 0 {
			total = int64(n)
		}
	}
	switch {
	case total > 0 && int64(written) < total:
		return fmt.Sprintf("%d of %d messages (the %s %d; the rest is not in this document)",
			written, total, which, written), false
	case total > 0 && int64(written) == total:
		return fmt.Sprintf("all %d messages", total), true
	}
	return fmt.Sprintf("%d messages", written), false
}

// renderExportMarkdown is the document form: something to read, or to paste into an issue.
func renderExportMarkdown(item record, messages []map[string]any, final map[string]any, q messageQuery) string {
	which := "earliest"
	if q.fromEnd {
		which = "latest"
	}
	coverage, _ := exportCoverage(item, final, len(messages), which)

	var b strings.Builder
	name := strOr(item.get("shortKey"), item.str("sessionId"))
	fmt.Fprintf(&b, "# %s\n\n", name)

	// Metadata: only fields that actually have a value
	for _, kv := range [][2]string{
		{"Source", item.str("source")},
		{"sessionId", item.str("sessionId")},
		{"Updated", item.str("updatedAt")},
		{"cwd", item.str("cwd")},
		{"Model", item.str("model")},
		{"File", item.str("file")},
		{"Messages", coverage},
	} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "- **%s**: %s\n", kv[0], kv[1])
		}
	}

	if final != nil {
		b.WriteString("\n## Final result\n\n")
		if truthy(final["isFinal"]) {
			b.WriteString("> Complete")
		} else {
			b.WriteString("> Incomplete")
		}
		if reason := strOr(final["stopReason"], ""); reason != "" {
			fmt.Fprintf(&b, " · `%s`", reason)
		}
		b.WriteString("\n\n")
		if text := strOr(final["text"], ""); text != "" {
			b.WriteString(text + "\n")
		}
	}

	fmt.Fprintf(&b, "\n## Messages (%s %d)\n", which, len(messages))
	for _, message := range messages {
		fmt.Fprintf(&b, "\n### %s", strOr(message["role"], "unknown"))
		if ts := strOr(message["timestamp"], ""); ts != "" {
			fmt.Fprintf(&b, " · %s", ts)
		}
		b.WriteString("\n\n")
		writeBlocks(&b, message["content"])
	}
	return b.String()
}

// renderExportJSONL is the data form: one object per line, each tagged with a type, so a
// query is a filter rather than a parse.
//
//	{"type":"session", ...}   the session, and how much of it this file holds
//	{"type":"message", ...}   one message, in the shape /messages returns
//	{"type":"final",   ...}   the session's final result
func renderExportJSONL(item record, messages []map[string]any, final map[string]any, q messageQuery) string {
	which := "earliest"
	if q.fromEnd {
		which = "latest"
	}
	coverage, complete := exportCoverage(item, final, len(messages), which)

	var b strings.Builder
	writeJSONLine(&b, map[string]any{
		"type":      "session",
		"source":    item.str("source"),
		"sessionId": item.str("sessionId"),
		"title":     item.str("shortKey"),
		"cwd":       item.str("cwd"),
		"model":     item.str("model"),
		"updatedAt": item.str("updatedAt"),
		"order":     which,
		"exported":  len(messages),
		"coverage":  coverage,
		"complete":  complete,
	})
	for _, message := range messages {
		line := map[string]any{"type": "message"}
		for k, v := range message {
			line[k] = v
		}
		writeJSONLine(&b, line)
	}
	if final != nil {
		line := map[string]any{"type": "final"}
		for k, v := range final {
			line[k] = v
		}
		writeJSONLine(&b, line)
	}
	return b.String()
}

// writeJSONLine emits one JSONL record. A value that cannot be marshalled — a session file
// holds whatever its writer put there — becomes an error record rather than a half-written
// line, so the file stays parseable.
func writeJSONLine(b *strings.Builder, record map[string]any) {
	encoded, err := json.Marshal(record)
	if err != nil {
		encoded, _ = json.Marshal(map[string]any{"type": "error", "error": err.Error()})
	}
	b.Write(encoded)
	b.WriteByte('\n')
}

// writeBlocks renders a message's block array as Markdown
func writeBlocks(b *strings.Builder, content any) {
	blocks, ok := content.([]map[string]any)
	if !ok || len(blocks) == 0 {
		b.WriteString("_(nothing to display)_\n")
		return
	}
	for _, block := range blocks {
		switch block["type"] {
		case "text":
			if text := strOr(block["content"], ""); text != "" {
				b.WriteString(text + "\n\n")
			}
		case "thinking":
			if text := strOr(block["content"], ""); text != "" {
				// Fold the thinking away so it does not drown the actual answer
				fmt.Fprintf(b, "<details><summary>Thinking</summary>\n\n%s\n\n</details>\n\n", text)
			}
		case "toolCall":
			args, _ := json.MarshalIndent(getOr(block, "arguments", map[string]any{}), "", "  ")
			fmt.Fprintf(b, "**⚙ %s**\n\n```json\n%s\n```\n\n", strOr(block["name"], "(unnamed tool)"), args)
		case "toolResult":
			label := strOr(block["toolName"], "result")
			fmt.Fprintf(b, "↳ %s\n\n```\n%s\n```\n\n", label, strOr(block["content"], ""))
		}
	}
}

// sanitizeFilename folds a session identifier into a safe filename: path separators and
// the characters Windows rejects all become -, so Content-Disposition cannot carry a
// directory out with it.
func sanitizeFilename(name string) string {
	if name == "" {
		return "session"
	}
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			return '-'
		}
		if r < 0x20 {
			return '-'
		}
		return r
	}, name)
	cleaned = strings.Trim(cleaned, ". ")
	if cleaned == "" {
		return "session"
	}
	return truncate(cleaned, 100, "")
}
