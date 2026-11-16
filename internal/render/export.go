package render

import (
	"encoding/json"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/txt"
)

// Export formats. A session is exported whole: every turn, every output in full.
const (
	FormatMarkdown = "md"    // for reading, and pasting into an issue
	FormatJSON     = "json"  // one document
	FormatJSONL    = "jsonl" // a tagged object per line: a jq query is a filter, not a parse
	FormatHTML     = "html"  // a page that stands on its own: no scripts, no requests
)

// Formats lists the export formats.
var Formats = []string{FormatMarkdown, FormatJSON, FormatJSONL, FormatHTML}

// ContentType is the media type a format is served as.
func ContentType(format string) string {
	switch format {
	case FormatJSON:
		return "application/json; charset=utf-8"
	case FormatJSONL:
		return "application/x-ndjson; charset=utf-8"
	case FormatHTML:
		return "text/html; charset=utf-8"
	}
	return "text/markdown; charset=utf-8"
}

// Export renders a session whole in one of the formats.
func Export(s *index.Session, turns []model.Turn, format string) (string, error) {
	switch format {
	case "", FormatMarkdown:
		return exportMarkdown(s, turns), nil
	case FormatJSON:
		b, err := json.MarshalIndent(struct {
			Session *index.Session `json:"session"`
			Turns   []model.Turn   `json:"turns"`
		}{s, turns}, "", "  ")
		return string(b) + "\n", err
	case FormatJSONL:
		return exportJSONL(s, turns)
	case FormatHTML:
		return exportHTML(s, turns), nil
	}
	return "", fmt.Errorf("unknown format %q (one of: %s)", format, strings.Join(Formats, ", "))
}

func exportJSONL(s *index.Session, turns []model.Turn) (string, error) {
	var b strings.Builder
	line := func(kind string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		// {"type":"turn", …the value's own fields}
		b.WriteString(`{"type":"` + kind + `",` + string(raw[1:]) + "\n")
		return nil
	}
	if err := line("session", s); err != nil {
		return "", err
	}
	for i := range turns {
		head := turns[i]
		head.Items = nil
		if err := line("turn", head); err != nil {
			return "", err
		}
		for j := range turns[i].Items {
			if err := line("item", struct {
				Turn int `json:"turn"`
				*model.Item
			}{turns[i].Index, &turns[i].Items[j]}); err != nil {
				return "", err
			}
		}
	}
	return b.String(), nil
}

// header is the lines every export opens with.
func header(s *index.Session, turns []model.Turn) []string {
	lines := []string{"source " + s.Source + " · project " + orDash(s.Cwd)}
	if s.Branch != "" {
		lines = append(lines, "branch: "+s.Branch+" (when the session opened)")
	}
	if s.Model != "" {
		lines = append(lines, "model: "+s.Model)
	}
	lines = append(lines, "ref: "+s.Ref)
	if s.Path != "" {
		lines = append(lines, "path: "+s.Path)
	}
	sums := []string{plural(s.Stats.Turns, "turn"), plural(s.Stats.Tools, "tool call")}
	if s.Stats.Failures > 0 {
		sums = append(sums, strconv.Itoa(s.Stats.Failures)+" failed")
	}
	if t := TokenLine(s.Stats.Usage); t != "" {
		sums = append(sums, t)
	}
	if !s.StartedAt.IsZero() {
		sums = append(sums, s.StartedAt.Local().Format("2006-01-02 15:04")+" → "+s.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	lines = append(lines, strings.Join(sums, " · "))
	if s.Resume != "" {
		lines = append(lines, "resume: "+s.Resume)
	}
	return lines
}

// outcomeText says how a tool call ended: "failed · exit 1 · 2.3s".
func outcomeText(tool *model.Tool) string {
	var parts []string
	switch {
	case !tool.Done:
		parts = append(parts, "no result")
	case tool.Status == model.StatusInterrupted:
		parts = append(parts, "interrupted")
	case tool.Failed():
		parts = append(parts, "failed")
	}
	if tool.Exit != nil && (*tool.Exit != 0 || len(parts) > 0) {
		parts = append(parts, "exit "+strconv.Itoa(*tool.Exit))
	}
	if tool.DurMs > 0 {
		parts = append(parts, txt.Duration(time.Duration(tool.DurMs)*time.Millisecond))
	}
	return strings.Join(parts, " · ")
}

// fence wraps text in a code fence longer than any run of backticks inside it.
func fence(text string) string {
	ticks := "```"
	for strings.Contains(text, ticks) {
		ticks += "`"
	}
	return ticks + "\n" + strings.TrimRight(text, "\n") + "\n" + ticks + "\n"
}

func exportMarkdown(s *index.Session, turns []model.Turn) string {
	var b strings.Builder
	// A document is not a terminal: what a tool printed keeps its words and loses its
	// colour codes. The JSON formats are the data, and keep everything.
	plain := func(text string) string { return strings.TrimSpace(txt.StripControls(text)) }
	title := txt.OneLine(s.Title, 0)
	if title == "" {
		title = s.ID
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	for _, line := range header(s, turns) {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	for i := range turns {
		t := &turns[i]
		if t.Index == 0 {
			b.WriteString("\n## Before the first ask\n")
		} else {
			fmt.Fprintf(&b, "\n## Turn %d — %s", t.Index, t.StartedAt.Local().Format("2006-01-02 15:04"))
			if t.Interrupted {
				b.WriteString(" — interrupted")
			}
			b.WriteString("\n")
		}
		for j := range t.Items {
			it := &t.Items[j]
			copied := ""
			if it.Inherited {
				copied = " (copied from the parent session)"
			}
			switch it.Kind {
			case model.Prompt:
				label := "Asked"
				if it.Queued {
					label = "Asked while the agent worked"
				}
				fmt.Fprintf(&b, "\n### %s%s\n\n%s\n", label, copied, plain(it.Text))
			case model.Context:
				fmt.Fprintf(&b, "\n<details><summary>context%s</summary>\n\n%s\n</details>\n", copied, fence(plain(it.Text)))
			case model.Reply:
				fmt.Fprintf(&b, "\n%s\n", plain(it.Text))
			case model.Thinking:
				// Fenced: reasoning is quoted, and text that closes a tag must not close this one
				fmt.Fprintf(&b, "\n<details><summary>thinking</summary>\n\n%s\n</details>\n", fence(plain(it.Text)))
			case model.Event:
				fmt.Fprintf(&b, "\n> **%s** %s\n", it.Event, strings.ReplaceAll(plain(it.Text), "\n", "\n> "))
			case model.ToolUse:
				tool := it.Tool
				head := "**" + tool.Name + "**"
				if tool.Command != "" {
					head += " `" + txt.OneLine(tool.Command, 200) + "`"
				} else if len(tool.Files) > 0 {
					head += " " + strings.Join(tool.Files, ", ")
				}
				if o := outcomeText(tool); o != "" {
					head += " — " + o
				}
				fmt.Fprintf(&b, "\n↳ %s\n", head)
				if tool.Command == "" || strings.Contains(tool.Command, "\n") {
					if len(tool.Input) > 0 {
						fmt.Fprintf(&b, "\n%s", fence(indentJSON(tool.Input)))
					}
				}
				if tool.Output != "" {
					fmt.Fprintf(&b, "\n%s", fence(plain(tool.Output)))
				}
			}
		}
	}
	return b.String()
}

const exportCSS = `
:root{color-scheme:light dark;--fg:#1c1c1e;--mut:#6b6b70;--bg:#fff;--card:#f5f5f7;--line:#e2e2e6;--bad:#c62828;--acc:#3457d5}
@media(prefers-color-scheme:dark){:root{--fg:#ececf0;--mut:#9a9aa2;--bg:#16161a;--card:#202026;--line:#2e2e36;--bad:#ff6b6b;--acc:#8fa6ff}}
*{box-sizing:border-box}body{font:15px/1.55 system-ui,-apple-system,"Segoe UI",sans-serif;color:var(--fg);background:var(--bg);max-width:880px;margin:0 auto;padding:32px 20px 80px}
h1{font-size:24px;margin:0 0 8px}h2{font-size:16px;margin:36px 0 8px;padding-top:16px;border-top:1px solid var(--line)}
ul.meta{list-style:none;padding:0;margin:0;color:var(--mut);font-size:13px}
.ask{background:var(--card);border-radius:10px;padding:12px 14px;margin:12px 0;white-space:pre-wrap;overflow-wrap:anywhere}
.reply{white-space:pre-wrap;overflow-wrap:anywhere;margin:12px 0}
details{margin:6px 0;border-left:2px solid var(--line);padding-left:10px}summary{cursor:pointer;color:var(--mut);font-size:13px;overflow-wrap:anywhere}
summary b{color:var(--acc);font-weight:600}summary .bad{color:var(--bad)}
pre{background:var(--card);border-radius:8px;padding:10px 12px;overflow:auto;font:12.5px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace;white-space:pre-wrap;overflow-wrap:anywhere;max-height:480px}
.event{color:var(--mut);font-size:13px;margin:8px 0}.copied{opacity:.6}`

func exportHTML(s *index.Session, turns []model.Turn) string {
	var b strings.Builder
	esc := func(text string) string { return html.EscapeString(txt.StripControls(text)) }
	title := s.Title
	if title == "" {
		title = s.ID
	}
	fmt.Fprintf(&b, "<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\">\n<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">\n<title>%s</title>\n<style>%s</style></head><body>\n", esc(title), exportCSS)
	fmt.Fprintf(&b, "<h1>%s</h1>\n<ul class=\"meta\">\n", esc(title))
	for _, line := range header(s, turns) {
		fmt.Fprintf(&b, "<li>%s</li>\n", esc(line))
	}
	b.WriteString("</ul>\n")
	for i := range turns {
		t := &turns[i]
		if t.Index == 0 {
			b.WriteString("<h2>Before the first ask</h2>\n")
		} else {
			note := ""
			if t.Interrupted {
				note = " — interrupted"
			}
			fmt.Fprintf(&b, "<h2>Turn %d · %s%s</h2>\n", t.Index, esc(t.StartedAt.Local().Format("2006-01-02 15:04")), note)
		}
		for j := range t.Items {
			it := &t.Items[j]
			class := ""
			if it.Inherited {
				class = " copied"
			}
			switch it.Kind {
			case model.Prompt:
				fmt.Fprintf(&b, "<div class=\"ask%s\">%s</div>\n", class, esc(strings.TrimSpace(it.Text)))
			case model.Context:
				fmt.Fprintf(&b, "<details class=\"%s\"><summary>context</summary><pre>%s</pre></details>\n", strings.TrimSpace(class), esc(it.Text))
			case model.Reply:
				fmt.Fprintf(&b, "<div class=\"reply%s\">%s</div>\n", class, esc(strings.TrimSpace(it.Text)))
			case model.Thinking:
				fmt.Fprintf(&b, "<details><summary>thinking</summary><pre>%s</pre></details>\n", esc(it.Text))
			case model.Event:
				fmt.Fprintf(&b, "<div class=\"event\"><b>%s</b> %s</div>\n", esc(it.Event), esc(txt.OneLine(it.Text, 600)))
			case model.ToolUse:
				tool := it.Tool
				what := tool.Command
				if what == "" {
					what = strings.Join(tool.Files, ", ")
				}
				outcome := outcomeText(tool)
				if outcome != "" {
					cls := ""
					if tool.Failed() {
						cls = " class=\"bad\""
					}
					outcome = fmt.Sprintf(" <span%s>— %s</span>", cls, esc(outcome))
				}
				fmt.Fprintf(&b, "<details><summary><b>%s</b> %s%s</summary>\n", esc(tool.Name), esc(txt.OneLine(what, 160)), outcome)
				if len(tool.Input) > 0 {
					fmt.Fprintf(&b, "<pre>%s</pre>\n", esc(indentJSON(tool.Input)))
				}
				if tool.Output != "" {
					fmt.Fprintf(&b, "<pre>%s</pre>\n", esc(tool.Output))
				}
				b.WriteString("</details>\n")
			}
		}
	}
	b.WriteString("</body></html>\n")
	return b.String()
}
