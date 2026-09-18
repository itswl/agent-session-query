package app

import (
	"encoding/json"
	"fmt"
	"html"
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
	exportFormatJSON     = "json"
	exportFormatHTML     = "html"
)

// exportFormats is the whole set, and the one place a format is declared: the route reads
// it, the error message lists it, and the docs test it.
var exportFormats = []string{exportFormatMarkdown, exportFormatJSONL, exportFormatJSON, exportFormatHTML}

// exportContentTypes maps a format to what it is served as
func exportContentType(format string) string {
	switch format {
	case exportFormatJSONL:
		return "application/x-ndjson; charset=utf-8"
	case exportFormatJSON:
		return "application/json; charset=utf-8"
	case exportFormatHTML:
		return "text/html; charset=utf-8"
	}
	return "text/markdown; charset=utf-8"
}

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

	var rendered string
	switch format {
	case exportFormatJSONL:
		rendered = renderExportJSONL(item, messages, final, q)
	case exportFormatJSON:
		rendered = renderExportJSON(item, messages, final, q)
	case exportFormatHTML:
		rendered = renderExportHTML(item, messages, final, q)
	default:
		rendered = renderExportMarkdown(item, messages, final, q)
	}
	return rendered, stem + "." + format, exportContentType(format), true
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
		return fmt.Sprintf("%d of %d messages (the %s %d; the rest is not in this file)",
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

// renderExportJSON is the same data as jsonl, as one document. Some consumers want a
// single parse; the content is identical, only the framing differs.
func renderExportJSON(item record, messages []map[string]any, final map[string]any, q messageQuery) string {
	which := "earliest"
	if q.fromEnd {
		which = "latest"
	}
	coverage, complete := exportCoverage(item, final, len(messages), which)
	document := map[string]any{
		"session":  exportHeader(item, len(messages), coverage, complete, which),
		"messages": messages,
		"final":    final,
		"coverage": coverage,
		"complete": complete,
		"exported": len(messages),
	}
	if messages == nil {
		document["messages"] = []map[string]any{}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		encoded, _ = json.Marshal(map[string]any{"error": err.Error()})
	}
	return string(encoded)
}

// exportHeader is the record both the jsonl and json forms put first
func exportHeader(item record, exported int, coverage string, complete bool, which string) map[string]any {
	return map[string]any{
		"source":    item.str("source"),
		"sessionId": item.str("sessionId"),
		"title":     item.str("shortKey"),
		"cwd":       item.str("cwd"),
		"model":     item.str("model"),
		"updatedAt": item.str("updatedAt"),
		"order":     which,
		"exported":  exported,
		"coverage":  coverage,
		"complete":  complete,
	}
}

// renderExportHTML is a page that stands on its own: no scripts, no external requests, the
// stylesheet inlined, so it opens from disk and can be attached to anything. Session
// content is untrusted input — a session file holds whatever its writer put there — so
// every value goes through template escaping, and the CSP meta says no script may run even
// if one were smuggled in.
func renderExportHTML(item record, messages []map[string]any, final map[string]any, q messageQuery) string {
	which := "earliest"
	if q.fromEnd {
		which = "latest"
	}
	coverage, _ := exportCoverage(item, final, len(messages), which)
	name := strOr(item.get("shortKey"), item.str("sessionId"))

	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString("<meta http-equiv=\"Content-Security-Policy\" content=\"default-src 'none'; style-src 'unsafe-inline'; img-src data:\">\n")
	fmt.Fprintf(&b, "<title>%s</title>\n", html.EscapeString(name))
	b.WriteString(exportStyles)
	b.WriteString("</head>\n<body>\n")
	fmt.Fprintf(&b, "<h1>%s</h1>\n<dl class=\"meta\">\n", html.EscapeString(name))
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
			fmt.Fprintf(&b, "<dt>%s</dt><dd>%s</dd>\n", html.EscapeString(kv[0]), html.EscapeString(kv[1]))
		}
	}
	b.WriteString("</dl>\n")

	if final != nil {
		b.WriteString("<section class=\"final\"><h2>Final result</h2>\n")
		state := "Incomplete"
		if truthy(final["isFinal"]) {
			state = "Complete"
		}
		fmt.Fprintf(&b, "<p class=\"state\">%s", html.EscapeString(state))
		if reason := strOr(final["stopReason"], ""); reason != "" {
			fmt.Fprintf(&b, " · <code>%s</code>", html.EscapeString(reason))
		}
		b.WriteString("</p>\n")
		if text := strOr(final["text"], ""); text != "" {
			fmt.Fprintf(&b, "<p>%s</p>\n", html.EscapeString(text))
		}
		b.WriteString("</section>\n")
	}

	fmt.Fprintf(&b, "<h2>Messages <span class=\"dim\">(%s %d)</span></h2>\n", which, len(messages))
	for _, message := range messages {
		fmt.Fprintf(&b, "<article class=\"msg %s\"><header>%s",
			html.EscapeString(strOr(message["role"], "unknown")),
			html.EscapeString(strOr(message["role"], "unknown")))
		if ts := strOr(message["timestamp"], ""); ts != "" {
			fmt.Fprintf(&b, "<time>%s</time>", html.EscapeString(ts))
		}
		b.WriteString("</header>\n")
		writeHTMLBlocks(&b, message["content"])
		b.WriteString("</article>\n")
	}
	b.WriteString("</body>\n</html>\n")
	return b.String()
}

// writeHTMLBlocks renders the shared block array as HTML. Tool calls and their results are
// folded into <details>: a long session is mostly tool traffic, and the reader wants the
// conversation first.
func writeHTMLBlocks(b *strings.Builder, content any) {
	blocks, ok := content.([]map[string]any)
	if !ok || len(blocks) == 0 {
		b.WriteString("<p class=\"dim\">(nothing to display)</p>\n")
		return
	}
	for _, block := range blocks {
		text := strOr(block["content"], "")
		switch block["type"] {
		case "text":
			if text != "" {
				fmt.Fprintf(b, "<p>%s</p>\n", html.EscapeString(text))
			}
		case "thinking":
			if text != "" {
				fmt.Fprintf(b, "<details class=\"thinking\"><summary>thinking</summary><pre>%s</pre></details>\n",
					html.EscapeString(text))
			}
		case "toolCall":
			args, _ := json.MarshalIndent(block["arguments"], "", "  ")
			fmt.Fprintf(b, "<details class=\"tool\"><summary>⚙ %s</summary><pre>%s</pre></details>\n",
				html.EscapeString(strOr(block["name"], "(unnamed tool)")), html.EscapeString(string(args)))
		case "toolResult":
			fmt.Fprintf(b, "<details class=\"result\"><summary>↳ %s</summary><pre>%s</pre></details>\n",
				html.EscapeString(strOr(block["toolName"], "result")), html.EscapeString(text))
		default:
			if text != "" {
				fmt.Fprintf(b, "<pre>%s</pre>\n", html.EscapeString(text))
			}
		}
	}
}

// exportStyles is inline so the page makes no requests at all
const exportStyles = `<style>
:root { color-scheme: light dark; }
body { margin: 0 auto; padding: 24px 18px 64px; max-width: 900px;
  font: 15px/1.6 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif; }
h1 { font-size: 20px; margin: 0 0 8px; }
h2 { font-size: 15px; text-transform: uppercase; letter-spacing: .04em; margin: 28px 0 10px; }
.meta { display: grid; grid-template-columns: max-content 1fr; gap: 2px 12px; margin: 0 0 8px;
  font-size: 13px; }
.meta dt { color: #6b7280; }
.meta dd { margin: 0; overflow-wrap: anywhere; }
.dim { color: #6b7280; font-weight: 400; }
.final { border: 1px solid #8883; border-radius: 10px; padding: 12px 14px; margin: 18px 0; }
.final .state { font-weight: 650; margin: 0 0 8px; }
.msg { border-left: 2px solid #8884; padding: 6px 0 6px 12px; margin: 0 0 12px; }
.msg.user { border-color: #3b82f6; }
.msg.assistant { border-color: #22c55e; }
.msg > header { display: flex; gap: 10px; font-size: 12px; text-transform: uppercase;
  letter-spacing: .05em; font-weight: 650; }
.msg.user > header { color: #3b82f6; }
.msg.assistant > header { color: #22c55e; }
.msg time { margin-left: auto; color: #6b7280; font-weight: 400; text-transform: none; }
p { margin: 8px 0; white-space: pre-wrap; overflow-wrap: anywhere; }
pre { margin: 0; padding: 8px 10px; overflow-x: auto; font-size: 12.5px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace; white-space: pre-wrap; overflow-wrap: anywhere; }
details { margin: 6px 0; border: 1px solid #8883; border-radius: 8px; overflow: hidden; }
summary { cursor: pointer; padding: 5px 10px; font-size: 13px; }
details.thinking summary { color: #6b7280; font-style: italic; }
details.tool summary { color: #7c3aed; font-weight: 650; }
details.result summary { color: #6b7280; }
</style>
`

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
