package index

import (
	"context"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/txt"
)

// Hit is a turn that matched a search.
type Hit struct {
	Session Session   `json:"session"`
	Turn    int       `json:"turn"`
	Ref     string    `json:"ref"` // the turn's own reference: claude/3f2a…#4
	At      time.Time `json:"at,omitzero"`
	Ask     string    `json:"ask,omitempty"`
	Answer  string    `json:"answer,omitempty"`
	// How the turn ended: its tool calls, how many of them failed or were interrupted, and
	// whether the person stopped it. PassedLater is how many of those failed calls were
	// followed, in that turn or the three after it, by the same command succeeding (see
	// Failure.Later); the program fills it in (see app.Search).
	Tools       int  `json:"tools,omitempty"`
	Failed      int  `json:"failures,omitempty"`
	PassedLater int  `json:"passedLater,omitempty"`
	Interrupted bool `json:"interrupted,omitempty"`
	// Lacks are the words of the query the turn does not hold, as typed: set on the hits
	// of a search no turn matched whole (see SearchResult.Partial).
	Lacks []string `json:"lacks,omitempty"`
	// Matches are where in the turn the words were found, at most a few.
	Matches []Match `json:"matches"`
	Score   float64 `json:"score"`
}

// Match is one place a search's words were found.
type Match struct {
	Kind    model.Kind `json:"kind"`           // which kind of item it is in, or KindTitle
	Part    string     `json:"part,omitempty"` // for a tool: command, input or output
	Tool    string     `json:"tool,omitempty"`
	Seq     int        `json:"seq"`
	Snippet string     `json:"snippet"`
	// Word is the word the snippet was cut around, as it is written there, when it is
	// another form of the one searched for: "retried", for retries.
	Word string `json:"word,omitempty"`
}

// KindTitle is a match in the session's title, which is no item of the turn: its Seq is 0.
const KindTitle model.Kind = "title"

// SearchOptions tune a search.
type SearchOptions struct {
	Limit  int
	Offset int
	// PerSession bounds how many turns of one session a page holds, so that a long
	// session about the very thing searched for does not fill it alone. Zero means 3;
	// negative means no bound.
	PerSession int
	// Recent orders by time, newest first, instead of by how well a turn matches.
	Recent bool
}

// SearchResult is one page of hits.
type SearchResult struct {
	Hits  []Hit `json:"hits"`
	Total int   `json:"total"` // turns that matched, across every page
	// Sessions is how many sessions those turns are in.
	Sessions int `json:"sessions"`
	// Partial is set when no turn holds every word of the query: the hits are then the
	// turns that hold the most of them, each saying which it lacks. A search some turn
	// matches whole is answered with those turns alone.
	Partial *Partial `json:"partial,omitempty"`
	// Missing are the words of the query no turn holds at all, as typed — said when no
	// turn holds every word.
	Missing []string `json:"missing,omitempty"`
}

// Partial is how near the hits of a partial match come to the query.
type Partial struct {
	Words int `json:"words"` // the words the query asks for
	Held  int `json:"held"`  // how many of them the best of the hits hold
}

// The weight of a match by where it was found: a word in what the person asked says
// more about a turn than the same word somewhere in a log. The session's title weighs
// nothing: it lets a session's turns be found by it, and which of them come first is for
// their own words to say. Every turn holds the title, and the ranking counts a word
// against the length of the whole turn, so a turn of a few lines would otherwise rank,
// on the title alone, above the long one that did the work the title names.
const bm25Weights = `8.0, 4.0, 1.0, 2.0, 1.0, 0.0`

// halfLife is how long it takes a turn's age to halve the lift recency gives it. Two
// turns that match alike are ordered newest first; a much better match still wins from
// further back.
const halfLife = 45 * 24 * time.Hour

// Search finds the turns that hold a query's words, among the sessions a filter selects.
//
// The unit is the turn, not the session and not the line: the words of a query may be
// spread over what was asked, what was run and what came back — and the title of the
// session it is in — and it is the exchange they add up to that someone is looking for.
//
// When no turn holds every word, the answer is the turns that hold the most of them,
// marked as such (see closest): a search worded a little differently from the history
// still finds where it is.
func (ix *Index) Search(ctx context.Context, q Query, f Filter, o SearchOptions) (SearchResult, error) {
	res := SearchResult{Hits: []Hit{}}
	match := q.match()
	if match == "" {
		return res, nil
	}
	if o.Limit <= 0 {
		o.Limit = 20
	}
	if o.PerSession == 0 {
		o.PerSession = 3
	}
	total, sessions, cands, err := ix.candidates(ctx, match, f, o)
	if err != nil {
		return res, err
	}
	if total == 0 {
		return ix.closest(ctx, q, f, o)
	}
	res.Total, res.Sessions = total, sessions
	res.Hits = ix.hits(ctx, cands, q, o, nil)
	return res, nil
}

// cand is a turn a search found, before it is a hit.
type cand struct {
	sid   int64
	idx   int
	score float64
	at    int64
}

// candidates counts the turns an expression matches, and ranks enough of them that the
// page asked for is filled after one session's surplus is set aside.
func (ix *Index) candidates(ctx context.Context, match string, f Filter, o SearchOptions) (total, sessions int, cands []cand, err error) {
	where, args := f.where()
	from := ` FROM words JOIN turns t ON t.sid = (words.rowid >> 20) AND t.idx = (words.rowid & 1048575)
JOIN sessions s ON s.sid = t.sid WHERE words MATCH ? AND ` + where
	args = append([]any{match}, args...)

	if err := ix.ro.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT t.sid)`+from, args...).Scan(&total, &sessions); err != nil {
		return 0, 0, nil, err
	}
	if total == 0 {
		return 0, 0, nil, nil
	}

	// Rank in SQL, then let recency break near-ties here
	want := (o.Offset+o.Limit)*4 + 200
	order := `rank`
	if o.Recent {
		order = `t.started_at DESC, t.last_at DESC`
	}
	rows, err := ix.ro.QueryContext(ctx, `SELECT t.sid, t.idx, bm25(words, `+bm25Weights+`) AS rank, t.started_at, t.last_at`+
		from+` ORDER BY `+order+` LIMIT ?`, append(args, want)...)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		var c cand
		var rank float64
		var started, last int64
		if err := rows.Scan(&c.sid, &c.idx, &rank, &started, &last); err != nil {
			return 0, 0, nil, err
		}
		c.at = started
		if c.at == 0 {
			c.at = last
		}
		age := now.Sub(time.UnixMilli(c.at))
		c.score = -rank * (1 + 0.6*math.Pow(0.5, float64(age)/float64(halfLife)))
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, nil, err
	}
	if !o.Recent {
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	}
	return total, sessions, cands, nil
}

// hits makes the page asked for of ranked candidates: each session's surplus set aside,
// and each turn read for what it was asked, how it ended and where the words are. lacks,
// for a partial match, is what each turn lacks, by its rowid in the words table.
func (ix *Index) hits(ctx context.Context, cands []cand, q Query, o SearchOptions, lacks map[int64][]string) []Hit {
	out := []Hit{}
	perSession := map[int64]int{}
	sessions := map[int64]Session{}
	titles := map[int64]string{} // each session's title, as far as the words table holds it
	needles := q.needles()
	skipped := 0
	for _, c := range cands {
		if len(out) >= o.Limit {
			break
		}
		if o.PerSession > 0 && perSession[c.sid] >= o.PerSession {
			continue
		}
		perSession[c.sid]++
		if skipped < o.Offset {
			skipped++
			continue
		}
		s, ok := sessions[c.sid]
		if !ok {
			var err error
			if s, err = ix.bySID(ctx, c.sid); err != nil {
				continue
			}
			sessions[c.sid] = s
			titles[c.sid] = ownTitle(s.Title, firstAsk(ctx, ix.ro, c.sid))
		}
		turns, err := ix.Turns(ctx, &s, TurnRange{First: c.idx, Last: c.idx})
		if err != nil || len(turns) == 0 {
			continue
		}
		t := &turns[0]
		lacking := lacks[turnRowID(c.sid, c.idx)]
		// A word the turn lacks is not looked for: text that holds it only inside another
		// word — queue in enqueue — would say it was found
		held := needles
		if len(lacking) > 0 {
			held = nil
			for _, n := range needles {
				if !slices.Contains(lacking, n.term) {
					held = append(held, n)
				}
			}
		}
		out = append(out, Hit{
			Session: s, Turn: c.idx, Ref: s.Ref + "#" + itoa(c.idx), At: fromMs(c.at),
			Ask: t.Ask, Answer: t.Answer, Tools: t.Tools, Failed: t.Failures, Interrupted: t.Interrupted,
			Lacks:   lacking,
			Matches: ix.matches(ctx, &s, titles[c.sid], t, q, held), Score: c.score,
		})
	}
	return out
}

// maxChoices is how many ways of choosing some of a query's words closest spells out in
// one expression. Past it, the turns that hold any of the words are taken, and ordered
// by how many they hold.
const maxChoices = 16

// closest answers a search no turn matched whole: with the turns that hold the most of
// its words, best first, each saying which words it lacks — and with the words no turn
// holds at all. The words a query excludes stay excluded.
//
// It asks for the turns that hold all the words but one, then all but two, and so on,
// leaving out from the start any word no turn holds: the first that some turn answers is
// the closest there is.
func (ix *Index) closest(ctx context.Context, q Query, f Filter, o SearchOptions) (SearchResult, error) {
	res := SearchResult{Hits: []Hit{}}
	var words []Term
	var not []string
	for _, t := range q.Terms {
		switch e := t.expr(); {
		case e == "":
		case t.Not:
			not = append(not, e)
		default:
			words = append(words, t)
		}
	}
	var present []Term
	for _, t := range words {
		ok, err := ix.holds(ctx, t.expr(), f)
		if err != nil {
			return res, err
		}
		if ok {
			present = append(present, t)
		} else {
			res.Missing = append(res.Missing, t.String())
		}
	}
	if len(words) < 2 {
		return res, nil
	}
	for k := min(len(present), len(words)-1); k >= 1; k-- {
		total, sessions, cands, err := ix.candidates(ctx, excluding(atLeast(present, k), not), f, o)
		if err != nil {
			return res, err
		}
		if total == 0 {
			continue
		}
		held, err := ix.held(ctx, present, cands)
		if err != nil {
			return res, err
		}
		count := func(c cand) int {
			n := 0
			for _, ok := range held[turnRowID(c.sid, c.idx)] {
				if ok {
					n++
				}
			}
			return n
		}
		sort.SliceStable(cands, func(i, j int) bool { return count(cands[i]) > count(cands[j]) })
		lacks := map[int64][]string{}
		for _, c := range cands {
			id := turnRowID(c.sid, c.idx)
			has := held[id]
			var l []string
			for i, t := range present {
				if !has[i] {
					l = append(l, t.String())
				}
			}
			lacks[id] = append(l, res.Missing...)
		}
		res.Total, res.Sessions = total, sessions
		res.Partial = &Partial{Words: len(words), Held: count(cands[0])}
		res.Hits = ix.hits(ctx, cands, q, o, lacks)
		return res, nil
	}
	return res, nil
}

// atLeast is an expression for the turns that hold at least k of the terms: any k of
// them together. Where there are more than maxChoices ways to choose, it is any one.
func atLeast(terms []Term, k int) string {
	exprs := make([]string, len(terms))
	for i, t := range terms {
		exprs[i] = t.expr()
	}
	switch {
	case k >= len(terms):
		return strings.Join(exprs, " AND ")
	case k <= 1 || choices(len(terms), k) > maxChoices:
		return strings.Join(exprs, " OR ")
	}
	var groups []string
	pick := make([]int, k)
	var choose func(from, at int)
	choose = func(from, at int) {
		if at == k {
			parts := make([]string, k)
			for i, j := range pick {
				parts[i] = exprs[j]
			}
			groups = append(groups, "("+strings.Join(parts, " AND ")+")")
			return
		}
		for i := from; i <= len(terms)-(k-at); i++ {
			pick[at] = i
			choose(i+1, at+1)
		}
	}
	choose(0, 0)
	return strings.Join(groups, " OR ")
}

// choices is n choose k.
func choices(n, k int) int {
	c := 1
	for i := 1; i <= k; i++ {
		c = c * (n - k + i) / i
	}
	return c
}

// holds reports whether any turn the filter selects matches an expression.
func (ix *Index) holds(ctx context.Context, match string, f Filter) (bool, error) {
	where, args := f.where()
	var n int
	err := ix.ro.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM words JOIN turns t ON t.sid = (words.rowid >> 20) AND t.idx = (words.rowid & 1048575)
JOIN sessions s ON s.sid = t.sid WHERE words MATCH ? AND `+where+` LIMIT 1)`, append([]any{match}, args...)...).Scan(&n)
	return n > 0, err
}

// held says which of the terms each candidate holds, by its rowid in the words table.
func (ix *Index) held(ctx context.Context, terms []Term, cands []cand) (map[int64][]bool, error) {
	out := make(map[int64][]bool, len(cands))
	ids := make([]any, 0, len(cands)+1)
	ids = append(ids, "")
	for _, c := range cands {
		id := turnRowID(c.sid, c.idx)
		out[id] = make([]bool, len(terms))
		ids = append(ids, id)
	}
	for i, t := range terms {
		ids[0] = t.expr()
		rows, err := ix.ro.QueryContext(ctx, `SELECT rowid FROM words WHERE words MATCH ? AND rowid IN (`+placeholders(len(ids)-1)+`)`, ids...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if has := out[id]; has != nil {
				has[i] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// snippetRadius is how many characters a snippet keeps on each side of a match.
const snippetRadius = 90

// maxMatches is how many places one hit shows.
const maxMatches = 3

// matches finds where in a turn the query's words are: first in what was asked and
// said, then in what was run, then in what came back — and in the session's title, when
// it has one of its own (see ownTitle), for a word the turn does not hold itself.
func (ix *Index) matches(ctx context.Context, s *Session, title string, t *model.Turn, q Query, needles []needle) []Match {
	type field struct {
		kind   model.Kind
		part   string
		tool   string
		seq    int
		text   string
		column string
		whole  func() string // the text in full, when text is a preview of it
	}
	var fields []field
	for i := range t.Items {
		it := &t.Items[i]
		if it.Inherited {
			continue
		}
		switch it.Kind {
		case model.Prompt:
			fields = append(fields, field{kind: it.Kind, seq: it.Seq, text: it.Text, column: "ask"})
		case model.Reply:
			fields = append(fields, field{kind: it.Kind, seq: it.Seq, text: it.Text, column: "reply"})
		case model.Thinking, model.Event:
			fields = append(fields, field{kind: it.Kind, seq: it.Seq, text: it.Text, column: "thinking"})
		case model.ToolUse:
			tool := it.Tool
			if tool.Command != "" {
				fields = append(fields, field{kind: it.Kind, part: "command", tool: tool.Name, seq: it.Seq, text: tool.Command, column: "tool"})
			} else if len(tool.Input) > 0 {
				fields = append(fields, field{kind: it.Kind, part: "input", tool: tool.Name, seq: it.Seq, text: inputText(tool.Input), column: "tool"})
			}
			f := field{kind: it.Kind, part: "output", tool: tool.Name, seq: it.Seq, text: tool.Output, column: "output"}
			if tool.OutBytes > 0 {
				seq := it.Seq
				f.whole = func() string {
					if full, _, err := ix.Item(ctx, s, seq); err == nil && full.Tool != nil {
						return full.Tool.Output
					}
					return ""
				}
			}
			fields = append(fields, f)
		}
	}
	if title != "" {
		fields = append(fields, field{kind: KindTitle, seq: -1, text: title, column: "title"})
	}
	// What a match is worth by where it is, the same order the ranking weighs them in
	weight := map[string]int{"ask": 0, "reply": 1, "tool": 2, "output": 3, "thinking": 4, "title": 5}
	sort.SliceStable(fields, func(i, j int) bool { return weight[fields[i].column] < weight[fields[j].column] })

	wanted := map[string]bool{}
	for _, term := range q.Terms {
		if term.Column != "" && !term.Not {
			wanted[term.Column] = true
		}
	}
	var out []Match
	used := map[int]bool{}
	for _, n := range needles {
		if len(out) >= maxMatches {
			break
		}
		for _, f := range fields {
			if used[f.seq] && f.part != "output" {
				continue
			}
			if len(wanted) > 0 && !wanted[f.column] {
				continue
			}
			text := f.text
			at, size, word := n.find(text)
			if at < 0 && f.whole != nil {
				if text = f.whole(); text != "" {
					at, size, word = n.find(text)
				}
			}
			if at < 0 {
				continue
			}
			out = append(out, Match{Kind: f.kind, Part: f.part, Tool: f.tool, Seq: max(f.seq, 0), Snippet: snippet(text, at, size), Word: word})
			used[f.seq] = true
			break
		}
	}
	return out
}

// indexFold finds needle (already lowercase) in s without regard to case.
func indexFold(s, needle string) int {
	if needle == "" {
		return -1
	}
	if i := strings.Index(s, needle); i >= 0 {
		return i
	}
	// Lowering can change a string's length in bytes, so the search runs over the
	// lowered text and the offset is mapped back by counting characters
	lower := strings.ToLower(s)
	i := strings.Index(lower, needle)
	if i < 0 {
		return -1
	}
	if len(lower) == len(s) {
		return i
	}
	chars := utf8.RuneCountInString(lower[:i])
	off := 0
	for range chars {
		_, size := utf8.DecodeRuneInString(s[off:])
		off += size
	}
	return off
}

// snippet cuts the text around a match, cleaned, on one line.
func snippet(text string, at, n int) string {
	lo, hi := at-snippetRadius*3, at+n+snippetRadius*3
	lo, hi = max(lo, 0), min(hi, len(text))
	for lo > 0 && !utf8.RuneStart(text[lo]) {
		lo--
	}
	for hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi++
	}
	before := []rune(strings.Join(strings.Fields(txt.Clean(text[lo:at])), " "))
	found := strings.Join(strings.Fields(txt.Clean(text[at:min(at+n, len(text))])), " ")
	after := []rune(strings.Join(strings.Fields(txt.Clean(text[min(at+n, len(text)):hi])), " "))
	head, tail := "", ""
	if len(before) > snippetRadius {
		before, head = before[len(before)-snippetRadius:], "…"
	} else if lo > 0 {
		head = "…"
	}
	if len(after) > snippetRadius {
		after, tail = after[:snippetRadius], "…"
	} else if hi < len(text) {
		tail = "…"
	}
	sep1, sep2 := "", ""
	if at > 0 && isSpace(text[at-1]) {
		sep1 = " "
	}
	if at+n < len(text) && isSpace(text[at+n]) {
		sep2 = " "
	}
	return head + string(before) + sep1 + found + sep2 + string(after) + tail
}

func isSpace(b byte) bool { return b == ' ' || b == '\n' || b == '\t' || b == '\r' }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
