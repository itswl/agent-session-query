package app

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A pack: several sessions as one document.
//
// The problem it answers is handing a stretch of work to something that was not there for
// it — another model, another person, yourself in a month. Pasting a session is too much
// and pasting nothing is too little; what carries is "this was asked, this is what came of
// it", once per session, in the order it happened.
//
// The pair is not a summary and nothing here generates one. Both halves already exist in
// the records: the opening ask is the session's display name (shortKey), and the outcome
// is the final result every source already computes. Assembling them is deterministic and
// replayable, which is the property a written summary lacks — a summary decides what
// matters before anyone knows what will be asked of it.
//
// What a pack deliberately does not do is decide what is still true. Two sessions a week
// apart can conclude opposite things, and the reader has to see both, in order. The header
// says so rather than the document pretending to be current.
// A pack costs one Final call per session, and Final scans the file: ~18 ms for a small
// session, a few hundred for a large one. The defaults keep a pack to a few seconds and
// the header always says how many of the matching sessions it actually holds.
const (
	defaultPackSessions = 20
	maxPackSessions     = 100
)

const (
	packModeIndex = "index" // the ask and the outcome, one block per session
	packModeFull  = "full"  // the same, with each session's transcript inline
)

// packQuery selects what a pack covers. The filters are the ones /sessions already
// understands, so a pack can be "everything for this project since last month".
type packQuery struct {
	source  string
	project string
	since   time.Time
	until   time.Time
	limit   int
	mode    string
	format  string
}

// parsePackQuery reads the pack selection. The filters are /sessions' own, so the
// vocabulary is one the caller already knows.
func (s *apiServer) parsePackQuery(r *http.Request) (packQuery, error) {
	values := r.URL.Query()
	q := packQuery{
		source:  strings.TrimSpace(values.Get("source")),
		project: strings.TrimSpace(values.Get("project")),
		mode:    strings.TrimSpace(values.Get("mode")),
		format:  strings.ToLower(strings.TrimSpace(values.Get("format"))),
		limit:   defaultPackSessions,
	}
	// Both default to the cheap, common case: an index of Markdown. Asking for every
	// transcript is a deliberate act, since that is the one that can be enormous.
	if q.mode == "" {
		q.mode = packModeIndex
	}
	if q.format == "" {
		q.format = exportFormatMarkdown
	}
	if q.source != "" && !slices.Contains(knownModes, q.source) {
		return q, fmt.Errorf("unknown source %q (choose from: %s)", q.source, strings.Join(knownModes, " / "))
	}
	if q.mode != packModeIndex && q.mode != packModeFull {
		return q, fmt.Errorf("mode must be %s or %s", packModeIndex, packModeFull)
	}
	if q.format != exportFormatMarkdown && q.format != exportFormatJSONL {
		return q, fmt.Errorf("format must be %s or %s", exportFormatMarkdown, exportFormatJSONL)
	}
	if raw := strings.TrimSpace(values.Get("since")); raw != "" {
		since, err := parseSince(raw)
		if err != nil {
			return q, err
		}
		q.since = since
	}
	if raw := strings.TrimSpace(values.Get("until")); raw != "" {
		until, err := parseSince(raw)
		if err != nil {
			return q, err
		}
		q.until = until
	}
	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return q, fmt.Errorf("limit must be a positive number, got %q", raw)
		}
		if n > maxPackSessions {
			n = maxPackSessions
		}
		q.limit = n
	}
	return q, nil
}

// packEntry is one session in a pack, with the source that owns it
type packEntry struct {
	source SessionSource
	rec    record
}

// packEntries resolves the selection, oldest first.
//
// Oldest first is the opposite of the session list, and deliberately: a list answers "what
// was I doing", which is a question about now; a pack answers "how did this get here",
// which is a question about sequence.
func (a *SessionQueryAPI) packEntries(q packQuery) (entries []packEntry, matching int) {
	for _, source := range a.sources {
		if q.source != "" && source.Mode() != q.source {
			continue
		}
		for _, rec := range a.recordsOf(source) {
			if q.project != "" && !strings.Contains(rec.project(), q.project) {
				continue
			}
			at := rec.sortAt
			if !q.since.IsZero() && (at.IsZero() || at.Before(q.since)) {
				continue
			}
			if !q.until.IsZero() && (at.IsZero() || at.After(q.until)) {
				continue
			}
			entries = append(entries, packEntry{source: source, rec: rec})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[j].rec.newerThan(entries[i].rec)
	})

	matching = len(entries)
	if q.limit > 0 && len(entries) > q.limit {
		// Keep the most recent: a pack is read forwards, but the part that matters most is
		// the part nearest to now, and the header says what was dropped
		entries = entries[len(entries)-q.limit:]
	}
	return entries, matching
}

// packSummary is what every pack states about itself before anything else
type packSummary struct {
	sessions  int
	matching  int
	from, to  time.Time
	sources   map[string]int
	generated time.Time
	mode      string
}

func summarizePack(entries []packEntry, matching int, mode string) packSummary {
	summary := packSummary{
		sessions: len(entries), matching: matching, sources: map[string]int{},
		generated: time.Now().UTC(), mode: mode,
	}
	for i, entry := range entries {
		summary.sources[entry.rec.str("source")]++
		at := entry.rec.sortAt
		if at.IsZero() {
			continue
		}
		if i == 0 || at.Before(summary.from) {
			summary.from = at
		}
		if i == 0 || at.After(summary.to) {
			summary.to = at
		}
	}
	return summary
}

// packSourcesLine reads "claude (12), codex (2)", most used first
func (s packSummary) sourcesLine() string {
	type count struct {
		name string
		n    int
	}
	counts := make([]count, 0, len(s.sources))
	for name, n := range s.sources {
		counts = append(counts, count{name, n})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].n != counts[j].n {
			return counts[i].n > counts[j].n
		}
		return counts[i].name < counts[j].name
	})
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		parts = append(parts, fmt.Sprintf("%s (%d)", c.name, c.n))
	}
	return strings.Join(parts, ", ")
}

// packStamps format the header's dates; a zero range prints as a dash
func packDay(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02")
}

func packMoment(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// packNotice is the paragraph every pack opens with. It is the one part of the design that
// is about the reader rather than the data: two sessions can conclude opposite things, and
// a document that lists both without saying so invites the reader to take the last one, or
// the first, as the current truth.
const packNotice = "> This is a record, not a summary. Each entry is what was asked and what that\n" +
	"> session concluded, with its id and time so the transcript can be checked. Nothing here\n" +
	"> states what is *currently* true: where two entries disagree, the later one was said\n" +
	"> later, and that is all this document can tell you."

// renderPackMarkdown writes the index form: one block per session, ask and outcome.
//
// The outcome costs a Final call per session, and Final scans the session file — 18 ms for
// a small session, a few hundred for a large one. That is why a pack takes a limit.
func renderPackMarkdown(a *SessionQueryAPI, entries []packEntry, summary packSummary) string {
	var b strings.Builder
	scope := "every session"
	if summary.matching != summary.sessions {
		scope = fmt.Sprintf("the %d most recent of %d matching", summary.sessions, summary.matching)
	}
	b.WriteString("# Session pack\n\n")
	fmt.Fprintf(&b, "- **Sessions**: %d (%s)\n", summary.sessions, scope)
	fmt.Fprintf(&b, "- **Range**: %s → %s\n", packDay(summary.from), packDay(summary.to))
	if line := summary.sourcesLine(); line != "" {
		fmt.Fprintf(&b, "- **Sources**: %s\n", line)
	}
	fmt.Fprintf(&b, "- **Generated**: %s\n\n", summary.generated.Format(time.RFC3339))
	b.WriteString(packNotice + "\n\n")

	for i, entry := range entries {
		item := entry.rec.public()
		final := safeParse(entry.source.Mode(), "the final result", func() map[string]any {
			return entry.source.Final(entry.rec)
		})

		fmt.Fprintf(&b, "## %d · %s · %s · %s\n",
			i+1, packMoment(entry.rec.sortAt), item["source"], shortID(item["sessionId"]))
		// project() rather than cwd: the SQLite and gemini sources have no cwd but do have a
		// project, and showing "—" for a session that knows where it belongs is a lie of
		// omission
		if project := entry.rec.project(); project != "" {
			fmt.Fprintf(&b, "- **Project**: %s", project)
		} else {
			b.WriteString("- **Project**: —")
		}
		if n, ok := toFloat(final["messageCount"]); ok {
			fmt.Fprintf(&b, " · **Messages**: %d", int(n))
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "- **Asked**: %s\n", packAsk(entry.source, entry.rec, item))

		if final != nil {
			text := strings.TrimSpace(toStr(final["text"]))
			switch {
			case text != "":
				fmt.Fprintf(&b, "- **Concluded**: %s\n", packQuote(text))
			case truthy(final["isFinal"]):
				b.WriteString("- **Concluded**: (the session finished without a written answer)\n")
			default:
				b.WriteString("- **Concluded**: (no final result)\n")
			}
			if reason := toStr(final["stopReason"]); reason != "" && reason != "stop" {
				fmt.Fprintf(&b, "- **Stopped**: `%s`\n", reason)
			}
		}
		fmt.Fprintf(&b, "- **Transcript**: `/sessions/%s/export?format=jsonl`\n\n",
			url.PathEscape(toStr(item["sessionId"])))

		if summary.mode == packModeFull {
			// The whole session inline, in the same shape a single export uses, so a reader
			// that has the pack does not need the server
			messages := safeParse(entry.source.Mode(), "messages", func() []map[string]any {
				return entry.source.Messages(entry.rec, messageQuery{limit: exportMaxMessages})
			})
			fmt.Fprintf(&b, "### Transcript (%d messages)\n", len(messages))
			for _, message := range messages {
				fmt.Fprintf(&b, "\n#### %s", toStr(message["role"]))
				if ts := toStr(message["timestamp"]); ts != "" {
					fmt.Fprintf(&b, " · %s", ts)
				}
				b.WriteString("\n\n")
				writeBlocks(&b, message["content"])
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderPackJSONL is the index as data: a header line, then one record per session. A
// program that wants a transcript follows the sessionId; inlining every transcript would
// make the pack unqueryable, which is the one thing this form is for.
func renderPackJSONL(entries []packEntry, summary packSummary) string {
	var b strings.Builder
	writeJSONLine(&b, map[string]any{
		"type":        "pack",
		"sessions":    summary.sessions,
		"matching":    summary.matching,
		"from":        packDay(summary.from),
		"to":          packDay(summary.to),
		"sources":     summary.sources,
		"generatedAt": summary.generated.Format(time.RFC3339),
		"mode":        summary.mode,
		"notice":      "a record, not a summary: nothing here states what is currently true",
	})
	for i, entry := range entries {
		item := entry.rec.public()
		final := safeParse(entry.source.Mode(), "the final result", func() map[string]any {
			return entry.source.Final(entry.rec)
		})
		record := map[string]any{
			"type":       "session",
			"index":      i + 1,
			"sessionId":  item["sessionId"],
			"source":     item["source"],
			"cwd":        item["cwd"],
			"updatedAt":  item["updatedAt"],
			"asked":      packAsk(entry.source, entry.rec, item),
			"transcript": "/sessions/" + url.PathEscape(toStr(item["sessionId"])) + "/export?format=jsonl",
		}
		if final != nil {
			record["concluded"] = strings.TrimSpace(toStr(final["text"]))
			record["stopReason"] = final["stopReason"]
			record["messageCount"] = final["messageCount"]
		}
		writeJSONLine(&b, record)
	}
	return b.String()
}

// A pack entry leads with the opening message, and an opening message is not always a
// statement of intent: "参考评审一下" says nothing about what is being reviewed, because it
// was said into a conversation that already had context. When the opening is too short to
// stand on its own, the next user turns usually say what it was about, so they are appended
// until there is enough to read.
//
// The threshold is length, which is a proxy — a long, vague opening goes unextended. It is
// the cheap proxy: reading intent would mean a model, and a pack is assembled without one.
const (
	// 24, not 40: a rune count means different things per script. Twenty-four Chinese
	// characters is a sentence with a subject and an object — "把 auth 模块的重试逻辑改成指数
	// 退避，最大 30 秒" is 26 and complete — while forty would let a bare "帮我看看" through.
	packAskMinChars     = 24
	packAskBudget       = 200 // stop once the ask line has this much
	packAskMaxParts     = 3   // ...or this many messages, whichever comes first
	packAskScanMessages = 12  // how far in to look for user turns
)

// packAsk builds an entry's "asked" line, extending a short opening with what followed
func packAsk(source SessionSource, rec record, item map[string]any) string {
	opening := firstLine(toStr(item["shortKey"]))
	if len([]rune(opening)) >= packAskMinChars {
		return opening
	}
	// Reading the head of the session is cheap — the source stops once it has enough
	messages := safeParse(source.Mode(), "messages", func() []map[string]any {
		return source.Messages(rec, messageQuery{limit: packAskScanMessages})
	})

	parts := []string{opening}
	length := len([]rune(opening))
	for _, message := range messages {
		if len(parts) >= packAskMaxParts {
			break
		}
		if toStr(message["role"]) != "user" {
			continue
		}
		// blockText, not contentText: a message's blocks are []map[string]any, which
		// contentText does not walk, so it returned "" and firstLine turned that into
		// "(no opening message)" — appended as if it were a turn.
		//
		// titleFromUserText then does the two jobs it already does for a session's display
		// name: it rejects machine-assembled rows by their opening (a caveat row, Codex's
		// AGENTS.md instructions) and folds the rest to one line.
		text := titleFromUserText(blockText(message["content"]))
		if text == "" {
			continue
		}
		if len([]rune(text)) < packAskMinChars {
			// A terse first line is not always the whole turn: "参考评审一下" can be
			// followed, in the same message, by the material it refers to
			text = packSnippet(blockText(message["content"]))
		}
		// The opening is itself the first user message, so the same turn comes round again
		// here and must not be repeated. A candidate that merely *contains* it is a
		// different matter — that one has more to say, which is the whole point.
		if text == opening || strings.Contains(strings.Join(parts, " "), text) {
			continue
		}
		// The candidate usually opens with the same words as the ask line, because it is
		// that message; print what it adds rather than saying it twice
		if rest := strings.TrimSpace(strings.TrimPrefix(text, opening)); rest != "" && strings.HasPrefix(text, opening) {
			text = rest
		}
		if strings.Contains(strings.Join(parts, " "), text) {
			continue
		}
		parts = append(parts, text)
		length += len([]rune(text))
		if length >= packAskBudget {
			break
		}
	}
	return strings.Join(parts, " \u00b7 ")
}

// packSnippet is a message as one line, capped. The first line is preferred when it stands
// on its own; when it does not, the start of the whole message is used instead, because the
// sentence that identifies the work may be the second paragraph.
func packSnippet(text string) string {
	folded := strings.Join(strings.Fields(text), " ")
	const limit = 120
	if len([]rune(folded)) <= limit {
		return folded
	}
	return string([]rune(folded)[:limit]) + " \u2026"
}

// blockText is the first text block of a message, or the value itself when a source hands
// back a plain string. The empty return means "nothing said here" and callers skip it.
func blockText(content any) string {
	if blocks, ok := content.([]map[string]any); ok {
		for _, block := range blocks {
			if toStr(block["type"]) != "text" {
				continue
			}
			if text := strings.TrimSpace(toStr(block["content"])); text != "" {
				return text
			}
		}
		return ""
	}
	return strings.TrimSpace(contentText(content))
}

// packQuote folds an outcome onto one line and caps it: a conclusion is a paragraph or a
// page, and the pack gives the first of it while the transcript keeps the rest.
func packQuote(text string) string {
	one := strings.Join(strings.Fields(text), " ")
	const limit = 240
	if len([]rune(one)) <= limit {
		return one
	}
	return string([]rune(one)[:limit]) + " …"
}

// firstLine is the first non-empty line of a session's opening ask
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return packQuote(trimmed)
		}
	}
	return "(no opening message)"
}

// shortID is enough of an id to tell two sessions apart on screen, which is not the same
// as the first eight characters of it. Ids differ per source: claude's are uuids, gemini's
// are "session-<timestamp>-<uuid prefix>", hermes's are "<date>_<time>_<counter>". Taking
// the first segment yields "session" for every gemini session — the same label for all of
// them — so a uuid keeps its first group and anything else gives up its last segment, which
// is the part that varies.
func shortID(id any) string {
	s := toStr(id)
	if isUUID(s) {
		return s[:8]
	}
	if i := strings.LastIndexAny(s, "-_"); i >= 0 && len(s)-i-1 >= 4 {
		return s[i+1:]
	}
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
