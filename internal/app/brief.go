package app

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
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

// capText shortens s to about n characters — runes, not bytes: asks are often CJK and
// a byte cut would split one — preferring a space inside the cut, and marking it
func capText(s string, n int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if n <= 0 || len(runes) <= n {
		return s
	}
	cut := runes[:n]
	for i := len(cut) - 1; i > n/2; i-- {
		if cut[i] == ' ' {
			cut = cut[:i]
			break
		}
	}
	return string(cut) + " …"
}

// toolKindOf sorts a tool by what it is for — the same categories the page colours tool
// blocks by. Matching is whole-token, so publish does not read as run and runbook does
// not read as sh: a brief's tool counts should survive a skeptical reader.
func toolKindOf(name string) string {
	tokens := toolNameTokens(name)
	for _, kind := range []string{"exec", "write", "read", "search", "net", "agent"} {
		for _, token := range tokens {
			if toolKindWords[kind][token] {
				return kind
			}
		}
	}
	return "other"
}

var toolKindWords = map[string]map[string]bool{
	"exec":   setOf("bash", "sh", "zsh", "shell", "terminal", "exec", "run", "cmd", "command", "process", "container", "docker"),
	"write":  setOf("write", "edit", "patch", "replace", "create", "delete", "remove", "move", "rename", "apply", "save", "notebook"),
	"read":   setOf("read", "view", "cat", "open", "show"),
	"search": setOf("grep", "glob", "search", "find", "list", "scan", "ls"),
	"net":    setOf("fetch", "web", "url", "download", "browser", "http", "curl"),
	"agent":  setOf("task", "agent", "dispatch", "delegate", "subagent"),
}

func setOf(words ...string) map[string]bool {
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}

// toolNameTokens splits a tool name into lowercase word tokens: on separators, and on
// camelCase boundaries — NotebookEdit → [notebook edit], WebFetch → [web fetch].
// CamelCase is split before lowercasing, or the boundary is gone.
func toolNameTokens(name string) []string {
	fields := strings.FieldsFunc(name, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == ':' || r == ' ' || (r >= '0' && r <= '9')
	})
	out := []string{}
	for _, field := range fields {
		rs := []rune(field)
		start := 0
		for i := 1; i < len(rs); i++ {
			if unicode.IsLower(rs[i-1]) && unicode.IsUpper(rs[i]) {
				out = append(out, strings.ToLower(string(rs[start:i])))
				start = i
			}
		}
		out = append(out, strings.ToLower(string(rs[start:])))
	}
	return out
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

// sessionRoundsRead carries what a whole-session read produced. scanned and total differ
// when the final result knows the session ran longer than the export cap: the caller
// says so (partial / messagesScanned) instead of quietly claiming completeness.
type sessionRoundsRead struct {
	source  SessionSource
	item    record
	rounds  []round
	scanned int
	total   int
}

// roundsOf fetches a whole session and segments it. The read is capped at
// exportMaxMessages like an export; when the final result knows the session ran longer,
// total exceeds scanned and everything downstream says partial.
func (a *SessionQueryAPI) roundsOf(pattern, sourceWanted string) (sessionRoundsRead, error) {
	source, item, found := a.findSession(pattern, sourceWanted)
	if !found {
		return sessionRoundsRead{}, errNoSession
	}
	messages := safeParse(source.Mode(), "messages", func() []map[string]any {
		return source.Messages(item, messageQuery{limit: exportMaxMessages})
	})
	final := safeParse(source.Mode(), "the final result", func() map[string]any {
		return source.Final(item)
	})
	scanned, total := len(messages), len(messages)
	if final != nil {
		if n, ok := asCount(final["messageCount"]); ok && n > scanned {
			total = n
		}
	}
	return sessionRoundsRead{source, item, splitRounds(messages), scanned, total}, nil
}

// asCount reads a message count without assuming which integer shape a source used
func asCount(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// sessionRounds is the rounds index as JSON: one row per exchange.
func (a *SessionQueryAPI) sessionRounds(pattern, sourceWanted string) (map[string]any, error) {
	sr, err := a.roundsOf(pattern, sourceWanted)
	if err != nil {
		return nil, err
	}
	item, rounds := sr.item, sr.rounds
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
		"sessionId":       item.str("sessionId"),
		"source":          item.str("source"),
		"total":           len(rounds),
		"rounds":          list,
		"messagesScanned": sr.scanned,
		"messagesTotal":   sr.total,
		"partial":         sr.total > sr.scanned,
	}, nil
}

// sessionBrief renders one round as a markdown handoff brief. roundNo 0 means the
// latest round; atParam, when given, selects the round the timestamp falls in — how a
// search hit becomes the thing being briefed.
func (a *SessionQueryAPI) sessionBrief(pattern, sourceWanted string, roundNo int, atParam string) (string, error) {
	sr, err := a.roundsOf(pattern, sourceWanted)
	if err != nil {
		return "", err
	}
	item, rounds := sr.item, sr.rounds
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
	return renderBrief(item, rounds, selected, sr.scanned, sr.total), nil
}

// renderBrief lays the brief out in fixed sections: the headers are the interface the
// receiving side prompts around, not decoration.
func renderBrief(item record, rounds []round, selected, scanned, total int) string {
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
	if total > scanned {
		fmt.Fprintf(&b, "- scanned the latest %d of %d messages (partial)\n", scanned, total)
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
		header := "\n## Round " + strconv.Itoa(r.index)
		if r.interrupted {
			header += " — interrupted"
		}
		b.WriteString(header + "\n\n")
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
