package app

import (
	"errors"
	"fmt"
	"github.com/itswl/agent-session-query/internal/source"
	"sort"
	"strconv"
	"strings"
	"time"
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
// human turn. They never start a round. Besides the slash-command plumbing, Claude Code
// hands the model a background task's completion as a user row wrapped in
// <task-notification>, and hooks' context as <system-reminder>.
var roundPlumbingPrefixes = []string{
	"<command-name>", "<command-message>", "<command-args>", "<command-contents>",
	"<local-command-", "<caveat", "Caveat:", "<task-notification", "<system-reminder",
}

// errNoSession is what the brief endpoints return when the pattern matches nothing; the
// HTTP layer maps it to a 404 like the neighbouring endpoints
var errNoSession = errors.New("no session matches the pattern")

// round is one exchange: a real user message and everything that happened until the
// next one.
//
// files is every file a tool named — read, searched or written — and changed is the
// subset a write-kind call touched and did not fail at. The two used to be one list, which
// read as "the round changed twelve files" when it had read eleven and edited one.
// failures counts the tool results that reported an error or an interruption; it is the
// number a reader scans a session for first, and the one a brief handed to another agent
// must not leave out.
type round struct {
	index int
	// startAt is the ask. endAt is the last thing the agent did — the end of the work,
	// not the end of the rows: a notification or a block of context the CLI dropped in
	// hours later lands in whichever round was last and would otherwise stretch it, which
	// is how a round of four commands came to report ninety-four hours. lastAt is that
	// final row, kept because a search hit in one still has to resolve to this round.
	startAt     string
	endAt       string
	lastAt      string
	messages    int
	toolCalls   int
	failures    int
	files       []string
	changed     []string
	kinds       map[string]int
	asked       string
	outcome     string
	interrupted bool

	// pending holds the write-kind calls still waiting for their result, by call id, so
	// that a failed edit does not count as a change
	pending map[string]string
}

// isRoundStart: a user message carrying human words that is not command plumbing, and
// not one the CLI assembled (a Codex AGENTS.md or environment row carries injected: true)
func isRoundStart(m map[string]any) bool {
	if source.ToStr(m["role"]) != "user" || source.Truthy(m["injected"]) {
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
	// A message that is wholly one XML-style element is context a CLI injected, whatever
	// the tag: a person does not type a question that way (the Codex rule, see wrappedInTag)
	return !source.WrappedInTag(text)
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
		if source.ToStr(block["type"]) == "text" {
			b.WriteString(source.ToStr(block["content"]))
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
			if s := source.ToStr(value); s != "" && !containsString(files, s) {
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

// briefMaxSections caps how many rounds a delta brief spells out in full
const briefMaxSections = 12

// splitRounds segments messages into rounds. The rule is the page's table of contents
// moved server-side: a round starts at a user message with words of its own, and
// everything up to the next such message belongs to it. Messages before the first real
// ask belong to no round.
func splitRounds(messages []map[string]any) []round {
	rounds := []round{}
	var cur *round
	humanLast := false
	for _, m := range messages {
		role := source.ToStr(m["role"])
		starting := isRoundStart(m)
		if starting {
			if cur != nil {
				cur.interrupted = cur.interrupted || humanLast
				rounds = append(rounds, *cur)
			}
			cur = &round{
				index:   len(rounds) + 1,
				startAt: source.ToStr(m["timestamp"]),
				asked:   strings.TrimSpace(messageText(m["content"])),
				kinds:   map[string]int{},
				pending: map[string]string{},
			}
		} else if cur == nil {
			continue
		}
		at := source.ToStr(m["timestamp"])
		if at != "" {
			cur.lastAt = at
		}
		// The ask and the agent's own rows are work; a row that only carries text the CLI
		// put in the user's mouth is not, however late it arrives (see the round comment)
		work := starting || role == "assistant"
		cur.messages++
		humanLast = starting
		for _, block := range contentBlocks(m["content"]) {
			switch source.ToStr(block["type"]) {
			case "toolCall":
				work = true
				cur.toolCalls++
				cur.files = appendFilePath(cur.files, block["arguments"])
				kind := toolKindOf(source.ToStr(block["name"]))
				cur.kinds[kind]++
				if kind == "write" {
					cur.noteWrite(source.ToStr(block["id"]), block["arguments"])
				}
			case "toolResult":
				work = true
				failed := source.IsFailedResult(block)
				if failed {
					cur.failures++
				}
				cur.settleWrite(source.ToStr(block["callId"]), failed)
			case "event":
				work = true
				// The user stopping the turn is recorded as an event by the sources that
				// know it happened; the ask-after-ask heuristic below catches the rest
				if source.ToStr(block["kind"]) == source.EventInterrupted {
					cur.interrupted = true
				}
			}
		}
		if role == "assistant" {
			if text := strings.TrimSpace(messageText(m["content"])); text != "" {
				cur.outcome = text
			}
		}
		if work && at != "" {
			cur.endAt = at
		}
	}
	if cur != nil {
		cur.interrupted = cur.interrupted || humanLast
		rounds = append(rounds, *cur)
	}
	return rounds
}

// noteWrite records a write-kind call. One without an id cannot be paired with a result,
// so it counts as a change on the spot; one with an id waits for its result.
func (r *round) noteWrite(id string, arguments any) {
	files := appendFilePath(nil, arguments)
	if len(files) == 0 {
		return
	}
	if id == "" {
		r.changed = appendUnique(r.changed, files[0])
		return
	}
	r.pending[id] = files[0]
}

// settleWrite pairs a result with its write call: a failure is not a change
func (r *round) settleWrite(callID string, failed bool) {
	file, ok := r.pending[callID]
	if !ok {
		return
	}
	delete(r.pending, callID)
	if !failed {
		r.changed = appendUnique(r.changed, file)
	}
}

func appendUnique(list []string, s string) []string {
	if containsString(list, s) {
		return list
	}
	return append(list, s)
}

// capText shortens s to about n characters — runes, not bytes: asks are often CJK and
// a byte cut would split one — preferring a space inside the cut, and marking it
func capText(s string, n int) string {
	// A brief is assembled for a reader, and it is meant to be handed to another agent or
	// pasted into a terminal, so the text it quotes is cleaned on the way in rather than
	// passed through (see stripTerminalControls). Before the cut, so the cut cannot leave
	// half a sequence behind.
	s = strings.TrimSpace(source.RedactSecrets(source.StripTerminalControls(s)))
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
	if t, ok := source.ParseTimestamp(ts); ok {
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
	source  source.SessionSource
	item    source.Record
	rounds  []round
	scanned int
	total   int
}

// briefScanCap bounds the rounds/brief read; a var so tests can shrink it
var briefScanCap = exportMaxMessages

// roundsOf reads a session's tail and segments it. The tail, deliberately: a handoff
// brief cares about where things ended, not how they started, so the read is fromEnd
// and round numbering runs over the scanned tail. When the final result knows the
// session ran longer than the cap, total exceeds scanned and everything downstream says
// partial; an at= older than the tail then falls in no round, and says so.
func (a *SessionQueryAPI) roundsOf(pattern, sourceWanted string) (sessionRoundsRead, error) {
	src, item, found := a.findSession(pattern, sourceWanted)
	if !found {
		return sessionRoundsRead{}, errNoSession
	}
	messages := safeParse(src.Mode(), "messages", func() []map[string]any {
		return src.Messages(item, source.MessageQuery{Limit: briefScanCap, FromEnd: true})
	})
	final := safeParse(src.Mode(), "the final result", func() map[string]any {
		return src.Final(item)
	})
	scanned, total := len(messages), len(messages)
	if final != nil {
		if n, ok := asCount(final["messageCount"]); ok && n > scanned {
			total = n
		}
	}
	return sessionRoundsRead{src, item, splitRounds(messages), scanned, total}, nil
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
			"round":        r.index,
			"startAt":      r.startAt,
			"endAt":        r.endAt,
			"lastAt":       r.spanEnd(),
			"messages":     r.messages,
			"toolCalls":    r.toolCalls,
			"failures":     r.failures,
			"files":        nonNilStrings(r.files),
			"filesChanged": nonNilStrings(r.changed),
			"asked":        capText(r.asked, 160),
			"outcome":      capText(r.outcome, 240),
			"interrupted":  r.interrupted,
		})
	}
	return map[string]any{
		"sessionId":       item.SessionID,
		"source":          item.Source,
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
func (a *SessionQueryAPI) sessionBrief(pattern, sourceWanted string, roundNo int, atParam, sinceParam string) (string, error) {
	sr, err := a.roundsOf(pattern, sourceWanted)
	if err != nil {
		return "", err
	}
	item, rounds := sr.item, sr.rounds
	selected := []int{}
	since := ""
	switch {
	case atParam != "":
		at, ok := source.ParseTimestamp(atParam)
		if !ok {
			return "", fmt.Errorf("At: unreadable time %q", atParam)
		}
		for _, r := range rounds {
			start, okStart := source.ParseTimestamp(r.startAt)
			end, okEnd := source.ParseTimestamp(r.spanEnd())
			if okStart && okEnd && !at.Before(start) && !at.After(end) {
				selected = []int{r.index}
				break
			}
		}
		if len(selected) == 0 {
			return "", fmt.Errorf("at %q falls in no round of this session (the scan covers the latest %d of %d messages)", atParam, sr.scanned, sr.total)
		}
	case roundNo != 0:
		if roundNo < 1 || roundNo > len(rounds) {
			return "", fmt.Errorf("round must be 1..%d, got %d", len(rounds), roundNo)
		}
		selected = []int{roundNo}
	case sinceParam != "":
		// The delta handoff: everything the session has done since the other side last
		// looked, rather than a round chosen by number. A round that was still running
		// at that moment counts — it is work the receiver has not seen either.
		cut, err := parseSince(sinceParam)
		if err != nil {
			return "", err
		}
		since = cut.UTC().Format(time.RFC3339)
		for _, r := range rounds {
			end, ok := source.ParseTimestamp(r.spanEnd())
			if !ok || !end.Before(cut) {
				selected = append(selected, r.index)
			}
		}
		if len(selected) == 0 {
			return "", fmt.Errorf("no round of this session ran since %s (the scan covers the latest %d of %d messages)", since, sr.scanned, sr.total)
		}
	case len(rounds) > 0:
		selected = []int{rounds[len(rounds)-1].index}
	}
	return renderBrief(item, rounds, selected, sr.scanned, sr.total, since), nil
}

// spanEnd is the last row of the round, work or not: the end of the stretch of session
// the round covers, which is what a timestamp has to fall inside to name it.
func (r round) spanEnd() string {
	if r.lastAt != "" {
		return r.lastAt
	}
	if r.endAt != "" {
		return r.endAt
	}
	return r.startAt
}

// workEnd is the last thing the agent did, falling back to the ask for a round that never
// got one: the end a duration should be measured to.
func (r round) workEnd() string {
	if r.endAt != "" {
		return r.endAt
	}
	return r.startAt
}

// nonNilStrings keeps an empty list an empty JSON array rather than null
func nonNilStrings(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// renderBrief lays the brief out in fixed sections: the headers are the interface the
// receiving side prompts around, not decoration. selected names the rounds to render in
// full — one for a round chosen by number or by timestamp, several for a delta since a
// moment, which is what a second handoff of the same session wants.
func renderBrief(item source.Record, rounds []round, selected []int, scanned, total int, since string) string {
	var b strings.Builder
	// Most titles arrive through titleFromUserText, which already cleans them, but the
	// SQLite sources take theirs straight from a column
	name := source.RedactSecrets(source.StripTerminalControls(item.ShortKey))
	if name == "" {
		name = item.SessionID
	}
	fmt.Fprintf(&b, "# Session brief: %s\n\n", name)
	fmt.Fprintf(&b, "- source %s · project %s\n", item.Source, item.Cwd)
	if branch := item.Branch; branch != "" {
		fmt.Fprintf(&b, "- branch: %s (when the session opened)\n", branch)
	}
	fmt.Fprintf(&b, "- sessionId: %s\n", item.SessionID)
	if file := item.File; file != "" {
		fmt.Fprintf(&b, "- file: %s\n", file)
	}
	// The cheapest handoff of all is to reopen the session in the CLI that wrote it, so
	// the brief says how, with the cd the page prepends for the same reason (see
	// resumeCommand): the agent is found from anywhere, but it works where it is launched
	if resume := item.ResumeCommand(); resume != "" {
		if cwd := item.Cwd; isAbsolutePath(cwd) {
			resume = "cd " + source.ShellArg(cwd) + " && " + resume
		}
		fmt.Fprintf(&b, "- resume: %s\n", resume)
	}
	if len(rounds) > 0 {
		fmt.Fprintf(&b, "- %d rounds · %s → %s\n", len(rounds), shortTime(rounds[0].startAt), shortTime(rounds[len(rounds)-1].workEnd()))
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
			mark, note := "✓", ""
			switch {
			case r.failures > 0:
				mark, note = "✗", fmt.Sprintf(" · %d failed", r.failures)
			case r.interrupted:
				mark = "⚠"
			}
			fmt.Fprintf(&b, "- #%d %s %s %s%s\n", r.index, shortTime(r.startAt), mark, capText(r.asked, 100), note)
		}
		if len(shown) < len(rounds) {
			fmt.Fprintf(&b, "- … %d rounds not shown\n", len(rounds)-len(shown))
		}
	}

	chosen := map[int]bool{}
	for _, index := range selected {
		chosen[index] = true
	}

	// A delta covers a stretch rather than one exchange, so it opens with what the stretch
	// adds up to: the receiving side reads that first and the rounds for the detail.
	if since != "" && len(selected) > 0 {
		var files, changed []string
		calls, failures := 0, 0
		for _, r := range rounds {
			if !chosen[r.index] {
				continue
			}
			for _, f := range r.files {
				files = appendUnique(files, f)
			}
			for _, f := range r.changed {
				changed = appendUnique(changed, f)
			}
			calls += r.toolCalls
			failures += r.failures
		}
		first, last := rounds[selected[0]-1], rounds[selected[len(selected)-1]-1]
		fmt.Fprintf(&b, "\n## Since %s\n\n", since)
		fmt.Fprintf(&b, "- %d rounds: #%d → #%d · %s → %s\n", len(selected), first.index, last.index,
			shortTime(first.startAt), shortTime(last.workEnd()))
		if len(changed) > 0 {
			fmt.Fprintf(&b, "- Changed: %s\n", joinFiles(changed))
		}
		if len(files) > 0 {
			fmt.Fprintf(&b, "- Files: %s\n", joinFiles(files))
		}
		fmt.Fprintf(&b, "- Tools: %d calls · failed %d\n", calls, failures)
	}

	// Enough rounds to follow the work, newest kept: a delta over a long absence would
	// otherwise be the whole session again, which is what the receiver cannot afford
	if len(selected) > briefMaxSections {
		selected = selected[len(selected)-briefMaxSections:]
		chosen = map[int]bool{}
		for _, index := range selected {
			chosen[index] = true
		}
		fmt.Fprintf(&b, "- … only the latest %d rounds are detailed below\n", briefMaxSections)
	}

	for _, r := range rounds {
		if !chosen[r.index] {
			continue
		}
		header := "\n## Round " + strconv.Itoa(r.index)
		if r.interrupted {
			header += " — interrupted"
		}
		b.WriteString(header + "\n\n")
		fmt.Fprintf(&b, "- Asked: %s\n", capText(r.asked, 300))
		if len(r.changed) > 0 {
			fmt.Fprintf(&b, "- Changed: %s\n", joinFiles(r.changed))
		}
		if len(r.files) > 0 {
			fmt.Fprintf(&b, "- Files: %s\n", joinFiles(r.files))
		}
		if len(r.kinds) > 0 || r.toolCalls > 0 {
			kinds := make([]string, 0, len(r.kinds))
			for kind := range r.kinds {
				kinds = append(kinds, kind)
			}
			sort.Strings(kinds)
			parts := make([]string, 0, len(kinds)+1)
			for _, kind := range kinds {
				parts = append(parts, fmt.Sprintf("%s %d", kind, r.kinds[kind]))
			}
			if len(parts) == 0 {
				parts = append(parts, strconv.Itoa(r.toolCalls))
			}
			if r.failures > 0 {
				parts = append(parts, fmt.Sprintf("failed %d", r.failures))
			}
			fmt.Fprintf(&b, "- Tools: %s\n", strings.Join(parts, " · "))
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

	if id := item.SessionID; id != "" {
		fmt.Fprintf(&b, "\nDig deeper: GET /sessions/%s/messages?limit=200 — or MCP get_messages with pattern=%q. session_brief takes round=/at= for another round, and since= for everything after a moment.\n", id, id)
	}
	return b.String()
}

// joinFiles lists up to twenty files and says how many more there were
func joinFiles(files []string) string {
	more := ""
	if len(files) > 20 {
		more = fmt.Sprintf(" … and %d more", len(files)-20)
		files = files[:20]
	}
	return strings.Join(files, ", ") + more
}

// isAbsolutePath: a plain absolute path, Unix or Windows. A labeled --path instance
// prefixes cwd with its label (box2:/srv/proj), and a cd to that fails — and since the two
// halves are joined by &&, the resume would never run.
func isAbsolutePath(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	return len(p) >= 3 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// briefHTTPError maps a brief-layer error onto a status and payload. Only the miss is a
// 404 — everything else is the caller's malformed request.
func briefHTTPError(err error) (int, map[string]any) {
	if errors.Is(err, errNoSession) {
		return 404, map[string]any{"error": "Session not found"}
	}
	return 400, map[string]any{"error": err.Error()}
}
