package index

import (
	"context"
	"math"
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
	Tools   int       `json:"tools,omitempty"`
	Failed  int       `json:"failures,omitempty"`
	// Matches are where in the turn the words were found, at most a few.
	Matches []Match `json:"matches"`
	Score   float64 `json:"score"`
}

// Match is one place a search's words were found.
type Match struct {
	Kind    model.Kind `json:"kind"`           // which kind of item it is in
	Part    string     `json:"part,omitempty"` // for a tool: command, input or output
	Tool    string     `json:"tool,omitempty"`
	Seq     int        `json:"seq"`
	Snippet string     `json:"snippet"`
}

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
}

// The weight of a match by where it was found: a word in what the person asked says
// more about a turn than the same word somewhere in a log.
const bm25Weights = `8.0, 4.0, 1.0, 2.0, 1.0`

// halfLife is how long it takes a turn's age to halve the lift recency gives it. Two
// turns that match alike are ordered newest first; a much better match still wins from
// further back.
const halfLife = 45 * 24 * time.Hour

// Search finds the turns that hold a query's words, among the sessions a filter selects.
//
// The unit is the turn, not the session and not the line: the words of a query may be
// spread over what was asked, what was run and what came back, and it is the exchange
// they add up to that someone is looking for.
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
	where, args := f.where()
	from := ` FROM words JOIN turns t ON t.sid = (words.rowid >> 20) AND t.idx = (words.rowid & 1048575)
JOIN sessions s ON s.sid = t.sid WHERE words MATCH ? AND ` + where
	args = append([]any{match}, args...)

	if err := ix.ro.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT t.sid)`+from, args...).Scan(&res.Total, &res.Sessions); err != nil {
		return res, err
	}
	if res.Total == 0 {
		return res, nil
	}

	// Rank in SQL, then let recency break near-ties here: enough candidates are taken
	// that the page asked for is filled after one session's surplus is set aside
	want := (o.Offset+o.Limit)*4 + 200
	order := `rank`
	if o.Recent {
		order = `t.started_at DESC, t.last_at DESC`
	}
	rows, err := ix.ro.QueryContext(ctx, `SELECT t.sid, t.idx, bm25(words, `+bm25Weights+`) AS rank, t.started_at, t.last_at, t.head`+
		from+` ORDER BY `+order+` LIMIT ?`, append(args, want)...)
	if err != nil {
		return res, err
	}
	type cand struct {
		sid   int64
		idx   int
		score float64
		at    int64
		head  string
	}
	var cands []cand
	now := time.Now()
	for rows.Next() {
		var c cand
		var rank float64
		var started, last int64
		if err := rows.Scan(&c.sid, &c.idx, &rank, &started, &last, &c.head); err != nil {
			rows.Close()
			return res, err
		}
		c.at = started
		if c.at == 0 {
			c.at = last
		}
		age := now.Sub(time.UnixMilli(c.at))
		c.score = -rank * (1 + 0.6*math.Pow(0.5, float64(age)/float64(halfLife)))
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	if !o.Recent {
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	}

	perSession := map[int64]int{}
	sessions := map[int64]Session{}
	needles := q.Needles()
	skipped := 0
	for _, c := range cands {
		if len(res.Hits) >= o.Limit {
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
			if s, err = ix.bySID(ctx, c.sid); err != nil {
				continue
			}
			sessions[c.sid] = s
		}
		turns, err := ix.Turns(ctx, &s, TurnRange{First: c.idx, Last: c.idx})
		if err != nil || len(turns) == 0 {
			continue
		}
		t := &turns[0]
		res.Hits = append(res.Hits, Hit{
			Session: s, Turn: c.idx, Ref: s.Ref + "#" + itoa(c.idx), At: fromMs(c.at),
			Ask: t.Ask, Answer: t.Answer, Tools: t.Tools, Failed: t.Failures,
			Matches: ix.matches(ctx, &s, t, q, needles), Score: c.score,
		})
	}
	return res, nil
}

// snippetRadius is how many characters a snippet keeps on each side of a match.
const snippetRadius = 90

// maxMatches is how many places one hit shows.
const maxMatches = 3

// matches finds where in a turn the query's words are: first in what was asked and
// said, then in what was run, then in what came back.
func (ix *Index) matches(ctx context.Context, s *Session, t *model.Turn, q Query, needles []string) []Match {
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
	// What a match is worth by where it is, the same order the ranking weighs them in
	weight := map[string]int{"ask": 0, "reply": 1, "tool": 2, "output": 3, "thinking": 4}
	sort.SliceStable(fields, func(i, j int) bool { return weight[fields[i].column] < weight[fields[j].column] })

	wanted := map[string]bool{}
	for _, term := range q.Terms {
		if term.Column != "" && !term.Not {
			wanted[term.Column] = true
		}
	}
	var out []Match
	used := map[int]bool{}
	for _, needle := range needles {
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
			at := indexFold(text, needle)
			if at < 0 && f.whole != nil {
				if text = f.whole(); text != "" {
					at = indexFold(text, needle)
				}
			}
			if at < 0 {
				continue
			}
			out = append(out, Match{Kind: f.kind, Part: f.part, Tool: f.tool, Seq: f.seq, Snippet: snippet(text, at, len(needle))})
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
