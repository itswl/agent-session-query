package app

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Rounds and briefs.
//
// A session is not a document with a beginning and a conclusion: it is a working
// notebook — topics come and go, the human doubles back, the CLI closes mid-thought.
// Summarizing one is lossy and quietly wrong, so the unit of extraction here is the
// round: everything between two real user messages. A round is bounded and factual —
// the ask in the human's own words, the files the tools touched, how the exchange
// ended — and the session-level brief is only metadata, the rounds index and the state
// at the end. It describes; it never claims a conclusion.

// roundPlumbingPrefixes marks command-plumbing rows: user role and real text, but not a
// human turn. They never start a round.
var roundPlumbingPrefixes = []string{
	"<command-name>", "<command-message>", "<command-args>", "<command-contents>",
	"<local-command-", "<caveat", "Caveat:",
}

// errNoSession is what the brief endpoints return when the pattern matches nothing; the
// HTTP layer maps it to a 404 like the neighbouring endpoints
var errNoSession = errors.New("no session matches the pattern")

// round is one exchange: a real user message and everything that happened until the
// next one.
type round struct {
	index       int
	startAt     string
	endAt       string
	messages    int
	toolCalls   int
	files       []string
	kinds       map[string]int
	asked       string
	outcome     string
	interrupted bool
}

// isRoundStart: a user message carrying human words that is not command plumbing
func isRoundStart(m map[string]any) bool {
	if toStr(m["role"]) != "user" {
		return false
	}
	text := strings.TrimSpace(messageText(m["content"]))
	if text == "" {
		return false
	}
	for _, prefix := range roundPlumbingPrefixes {
		if strings.HasPrefix(text, prefix) {
			return false
		}
	}
	return true
}

// contentBlocks folds a message's content field into blocks, whatever the transport did
// to it on the way here
func contentBlocks(content any) []map[string]any {
	switch c := content.(type) {
	case []map[string]any:
		return c
	case []any:
		out := make([]map[string]any, 0, len(c))
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// messageText concatenates a message's text blocks — the words, without tool traffic.
// (pack.go's blockText is deliberately different: it takes the first block only.)
func messageText(content any) string {
	if s, ok := content.(string); ok {
		return s // some sources hand a plain string back instead of blocks
	}
	var b strings.Builder
	for _, block := range contentBlocks(content) {
		if toStr(block["type"]) == "text" {
			b.WriteString(toStr(block["content"]))
		}
	}
	return b.String()
}

// filePathKeys: the argument keys a tool names a file by, across the CLIs
var filePathKeys = []string{"file_path", "path", "notebook_path", "file"}

// appendFilePath pulls the file a tool call names out of its arguments, if any
func appendFilePath(files []string, arguments any) []string {
	args, ok := arguments.(map[string]any)
	if !ok {
		return files
	}
	for _, want := range filePathKeys {
		for key, value := range args {
			if !strings.EqualFold(key, want) {
				continue
			}
			if s := toStr(value); s != "" && !containsString(files, s) {
				return append(files, s)
			}
		}
	}
	return files
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// splitRounds segments messages into rounds. The rule is the page's table of contents
// moved server-side: a round starts at a user message with words of its own, and
// everything up to the next such message belongs to it. Messages before the first real
// ask belong to no round.
func splitRounds(messages []map[string]any) []round {
	rounds := []round{}
	var cur *round
	humanLast := false
	for _, m := range messages {
		role := toStr(m["role"])
		starting := isRoundStart(m)
		if starting {
			if cur != nil {
				cur.interrupted = humanLast
				rounds = append(rounds, *cur)
			}
			cur = &round{
				index:   len(rounds) + 1,
				startAt: toStr(m["timestamp"]),
				asked:   strings.TrimSpace(messageText(m["content"])),
				kinds:   map[string]int{},
			}
		} else if cur == nil {
			continue
		}
		cur.endAt = toStr(m["timestamp"])
		cur.messages++
		humanLast = starting
		for _, block := range contentBlocks(m["content"]) {
			if toStr(block["type"]) != "toolCall" {
				continue
			}
			cur.toolCalls++
			cur.files = appendFilePath(cur.files, block["arguments"])
			cur.kinds[toolKindOf(toStr(block["name"]))]++
		}
		if role == "assistant" {
			if text := strings.TrimSpace(messageText(m["content"])); text != "" {
				cur.outcome = text
			}
		}
	}
	if cur != nil {
		cur.interrupted = humanLast
		rounds = append(rounds, *cur)
	}
	return rounds
}

// capText shortens s to about n characters, cutting at a space and marking the cut
func capText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return cut + " …"
}

func hasAnyWord(n string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// toolKindOf sorts a tool by what it is for — the same categories the page colours tool
// blocks by, kept coarse because a brief reads categories, not names
func toolKindOf(name string) string {
	n := strings.ToLower(name)
	switch {
	case hasAnyWord(n, "bash", "shell", "terminal", "exec", "sh", "run", "command", "process", "container", "docker"):
		return "exec"
	case hasAnyWord(n, "write", "edit", "patch", "replace", "create", "delete", "remove", "move", "rename", "apply", "notebook"):
		return "write"
	case hasAnyWord(n, "read", "view", "cat", "open", "head", "tail"):
		return "read"
	case hasAnyWord(n, "grep", "glob", "search", "find", "list", "scan"):
		return "search"
	case hasAnyWord(n, "fetch", "http", "curl", "web", "url", "download", "browser"):
		return "net"
	case hasAnyWord(n, "task", "agent", "dispatch", "delegate", "subagent"):
		return "agent"
	}
	return "other"
}

func shortTime(ts string) string {
	if t, ok := parseTimestamp(ts); ok {
		return t.Format("01-02 15:04")
	}
	if len(ts) > 16 {
		return ts[:16]
	}
	return ts
}

// roundsOf fetches a whole session and segments it. Chronological and complete: the
// export path's whole-session read, not the page's 200-message window.
func (a *SessionQueryAPI) roundsOf(pattern, sourceWanted string) (SessionSource, record, []round, error) {
	source, item, found := a.findSession(pattern, sourceWanted)
	if !found {
		return nil, record{}, nil, errNoSession
	}
	messages := safeParse(source.Mode(), "messages", func() []map[string]any {
		return source.Messages(item, messageQuery{limit: exportMaxMessages})
	})
	return source, item, splitRounds(messages), nil
}

// sessionRounds is the rounds index as JSON: one row per exchange.
func (a *SessionQueryAPI) sessionRounds(pattern, sourceWanted string) (map[string]any, error) {
	_, item, rounds, err := a.roundsOf(pattern, sourceWanted)
	if err != nil {
		return nil, err
	}
	list := make([]map[string]any, 0, len(rounds))
	for _, r := range rounds {
		list = append(list, map[string]any{
			"round":       r.index,
			"startAt":     r.startAt,
			"endAt":       r.endAt,
			"messages":    r.messages,
			"toolCalls":   r.toolCalls,
			"files":       r.files,
			"asked":       capText(r.asked, 160),
			"outcome":     capText(r.outcome, 240),
			"interrupted": r.interrupted,
		})
	}
	return map[string]any{
		"sessionId": item.str("sessionId"),
		"source":    item.str("source"),
		"total":     len(rounds),
		"rounds":    list,
	}, nil
}

// sessionBrief renders one round as a markdown handoff brief. roundNo 0 means the
// latest round; atParam, when given, selects the round the timestamp falls in — how a
// search hit becomes the thing being briefed.
func (a *SessionQueryAPI) sessionBrief(pattern, sourceWanted string, roundNo int, atParam string) (string, error) {
	_, item, rounds, err := a.roundsOf(pattern, sourceWanted)
	if err != nil {
		return "", err
	}
	selected := 0
	switch {
	case atParam != "":
		at, ok := parseTimestamp(atParam)
		if !ok {
			return "", fmt.Errorf("at: unreadable time %q", atParam)
		}
		for _, r := range rounds {
			start, okStart := parseTimestamp(r.startAt)
			end, okEnd := parseTimestamp(r.endAt)
			if okStart && okEnd && !at.Before(start) && !at.After(end) {
				selected = r.index
				break
			}
		}
		if selected == 0 {
			return "", fmt.Errorf("at %q falls in no round of this session", atParam)
		}
	case roundNo != 0:
		if roundNo < 1 || roundNo > len(rounds) {
			return "", fmt.Errorf("round must be 1..%d, got %d", len(rounds), roundNo)
		}
		selected = roundNo
	case len(rounds) > 0:
		selected = rounds[len(rounds)-1].index
	}
	return renderBrief(item, rounds, selected), nil
}

// renderBrief lays the brief out in fixed sections: the headers are the interface the
// receiving side prompts around, not decoration.
func renderBrief(item record, rounds []round, selected int) string {
	var b strings.Builder
	name := item.str("shortKey")
	if name == "" {
		name = item.str("sessionId")
	}
	fmt.Fprintf(&b, "# Session brief: %s\n\n", name)
	fmt.Fprintf(&b, "- source %s · project %s\n", item.str("source"), item.str("cwd"))
	fmt.Fprintf(&b, "- sessionId: %s\n", item.str("sessionId"))
	if file := item.str("file"); file != "" {
		fmt.Fprintf(&b, "- file: %s\n", file)
	}
	if len(rounds) > 0 {
		span := rounds[len(rounds)-1].endAt
		if span == "" {
			span = rounds[0].startAt
		}
		fmt.Fprintf(&b, "- %d rounds · %s → %s\n", len(rounds), shortTime(rounds[0].startAt), shortTime(span))
	}

	if len(rounds) > 1 {
		b.WriteString("\n## Rounds\n\n")
		shown := rounds
		if len(shown) > 20 {
			shown = append(append([]round{}, rounds[:5]...), rounds[len(rounds)-15:]...)
		}
		for _, r := range shown {
			mark := "✓"
			if r.interrupted {
				mark = "⚠"
			}
			fmt.Fprintf(&b, "- #%d %s %s %s\n", r.index, shortTime(r.startAt), mark, capText(r.asked, 100))
		}
		if len(shown) < len(rounds) {
			fmt.Fprintf(&b, "- … %d rounds not shown\n", len(rounds)-len(shown))
		}
	}

	for _, r := range rounds {
		if r.index != selected {
			continue
		}
		b.WriteString("\n## Round " + strconv.Itoa(r.index))
		if r.interrupted {
			b.WriteString(" — interrupted")
		}
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "- Asked: %s\n", capText(r.asked, 300))
		if len(r.files) > 0 {
			files, more := r.files, ""
			if len(files) > 20 {
				more = fmt.Sprintf(" … and %d more", len(files)-20)
				files = files[:20]
			}
			fmt.Fprintf(&b, "- Files: %s%s\n", strings.Join(files, ", "), more)
		}
		if len(r.kinds) > 0 {
			kinds := make([]string, 0, len(r.kinds))
			for kind := range r.kinds {
				kinds = append(kinds, kind)
			}
			sort.Strings(kinds)
			parts := make([]string, 0, len(kinds))
			for _, kind := range kinds {
				parts = append(parts, fmt.Sprintf("%s %d", kind, r.kinds[kind]))
			}
			fmt.Fprintf(&b, "- Tools: %s\n", strings.Join(parts, " · "))
		} else if r.toolCalls > 0 {
			fmt.Fprintf(&b, "- Tools: %d\n", r.toolCalls)
		}
		ended := r.outcome
		if ended == "" {
			ended = "(no assistant reply — the round was interrupted)"
		} else {
			ended = capText(ended, 500)
		}
		fmt.Fprintf(&b, "- Ended: %s\n", ended)
	}

	b.WriteString("\n## State at the end\n\n")
	state := ""
	for i := len(rounds) - 1; i >= 0; i-- {
		if rounds[i].outcome != "" {
			state = rounds[i].outcome
			break
		}
	}
	if state == "" {
		state = "(nothing concluded — no assistant text in this session)"
	}
	b.WriteString(capText(state, 400) + "\n")

	if id := item.str("sessionId"); id != "" {
		fmt.Fprintf(&b, "\nDig deeper: GET /sessions/%s/messages?limit=200 — or MCP get_messages with pattern=%q. session_brief takes round=/at= for another round.\n", id, id)
	}
	return b.String()
}

// briefHTTPError maps a brief-layer error onto a status and payload. Only the miss is a
// 404 — everything else is the caller's malformed request.
func briefHTTPError(err error) (int, map[string]any) {
	if errors.Is(err, errNoSession) {
		return 404, map[string]any{"error": "Session not found"}
	}
	return 400, map[string]any{"error": err.Error()}
}
