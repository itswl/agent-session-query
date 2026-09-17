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

// exportMarkdown renders one session as Markdown, returning the body and a suggested
// filename.
func (a *SessionQueryAPI) exportMarkdown(pattern string, q messageQuery) (body string, filename string, ok bool) {
	source, item, found := a.findSession(pattern)
	if !found {
		return "", "", false
	}
	messages := safeParse(source.Mode(), "messages", func() []map[string]any {
		return source.Messages(item, q)
	})
	final := safeParse(source.Mode(), "the final result", func() map[string]any {
		return source.Final(item)
	})

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

	which := "earliest"
	if q.fromEnd {
		which = "latest"
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

	return b.String(), sanitizeFilename(name) + ".md", true
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
