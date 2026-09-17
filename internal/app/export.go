package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 会话导出成 Markdown。
//
// 想把一段调试过程贴进 issue、文档或者发给同事时，现在只能一块块手抄。
// 输出直接复用 Messages / Final 那套块结构，不另起一套解析。

// exportMarkdown 把一个会话渲染成 Markdown，返回正文与建议文件名。
func (a *SessionQueryAPI) exportMarkdown(pattern string, q messageQuery) (body string, filename string, ok bool) {
	source, item, found := a.findSession(pattern)
	if !found {
		return "", "", false
	}
	messages := safeParse(source.Mode(), "消息", func() []map[string]any {
		return source.Messages(item, q)
	})
	final := safeParse(source.Mode(), "最终结果", func() map[string]any {
		return source.Final(item)
	})

	var b strings.Builder
	name := strOr(item.get("shortKey"), item.str("sessionId"))
	fmt.Fprintf(&b, "# %s\n\n", name)

	// 元信息：只写有值的
	for _, kv := range [][2]string{
		{"数据源", item.str("source")},
		{"sessionId", item.str("sessionId")},
		{"更新时间", item.str("updatedAt")},
		{"cwd", item.str("cwd")},
		{"模型", item.str("model")},
		{"文件", item.str("file")},
	} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "- **%s**：%s\n", kv[0], kv[1])
		}
	}

	if final != nil {
		b.WriteString("\n## 最终结果\n\n")
		if truthy(final["isFinal"]) {
			b.WriteString("> 已完成")
		} else {
			b.WriteString("> 未完成")
		}
		if reason := strOr(final["stopReason"], ""); reason != "" {
			fmt.Fprintf(&b, " · `%s`", reason)
		}
		b.WriteString("\n\n")
		if text := strOr(final["text"], ""); text != "" {
			b.WriteString(text + "\n")
		}
	}

	which := "最早"
	if q.fromEnd {
		which = "最新"
	}
	fmt.Fprintf(&b, "\n## 消息（%s %d 条）\n", which, len(messages))
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

// writeBlocks 把消息的块数组写成 Markdown
func writeBlocks(b *strings.Builder, content any) {
	blocks, ok := content.([]map[string]any)
	if !ok || len(blocks) == 0 {
		b.WriteString("_（无可显示内容）_\n")
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
				// 思考过程折起来，免得淹没正文
				fmt.Fprintf(b, "<details><summary>思考过程</summary>\n\n%s\n\n</details>\n\n", text)
			}
		case "toolCall":
			args, _ := json.MarshalIndent(getOr(block, "arguments", map[string]any{}), "", "  ")
			fmt.Fprintf(b, "**⚙ %s**\n\n```json\n%s\n```\n\n", strOr(block["name"], "(未命名工具)"), args)
		case "toolResult":
			label := strOr(block["toolName"], "结果")
			fmt.Fprintf(b, "↳ %s\n\n```\n%s\n```\n\n", label, strOr(block["content"], ""))
		}
	}
}

// sanitizeFilename 把会话标识收敛成安全的文件名：
// 路径分隔符和 Windows 不收的字符都换成 -，别让 Content-Disposition 带出目录。
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
