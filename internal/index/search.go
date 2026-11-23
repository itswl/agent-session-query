package index

import (
	"context"
	"database/sql"
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
	// Missing are the words of the query no turn the filter selects holds at all, as
	// typed — said when no turn holds every word.
	Missing []string `json:"missing,omitempty"`
	// Outside is what the filters a person set left out of a search no turn they select
	// matches whole — or in part only — and what it would take to see it.
	Outside *Outside `json:"outside,omitempty"`
}

// Outside is how many turns would match a search whole without the filters that kept them
// out: the ones a person set and, should that find none, the sessions their CLI archived,
// which a search leaves out unless asked.
type Outside struct {
	Turns    int `json:"turns"`    // 0: not even without them
	Sessions int `json:"sessions"` // the sessions those turns are in
	// Filters names the filters set, as the API's parameters and the command line's flags
	// are named: source, project, branch, text, since, until.
	Filters []string `json:"filters,omitempty"`
	// Archived says those turns are in archived sessions alone: they take including those,
	// as well as dropping the filters.
	Archived bool `json:"archived,omitempty"`
}

// Partial is how near the hits of a partial match come to the query.
type Partial struct {
	Words int `json:"words"` // the words the query asks for
	Held  int `json:"held"`  // how many of them the best of the hits hold
}

// The weight of a match by where it was found: a word in what the person asked says
// more about a turn than the same word somewhere in a log. The session's title weighs
// nothing: it lets the session be found, at its first turn, and where that turn comes is
// for its own words to say.
const bm25Weights = `8.0, 4.0, 1.0, 2.0, 1.0, 0.0`

// halfLife is how long it takes a turn's age to halve the lift recency gives it. Two
// turns that match alike are ordered newest first; a much better match still wins from
// further back.
const halfLife = 45 * 24 * time.Hour

// Search finds the turns that hold a query's words, among the sessions a filter selects.
//
// The unit is the turn, not the session and not the line: the words of a query may be
// spread over what was asked, what was run and what came back, and it is the exchange they
// add up to that someone is looking for. The session's title is searched with its first
// ask (see titleTurn), except in a search of one session alone (see inTurns).
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
	total, sessions, cands, err := ix.candidates(ctx, wordsQuery{match: inTurns(match, f)}, f, o)
	if err != nil {
		return res, err
	}
	if total == 0 {
		// Turns that hold every word, each with a word the query excludes: none matches, and
		// none is called close for holding fewer
		excluded := false
		if w := q.words(); len(w.Terms) < len(q.Terms) {
			if excluded, err = ix.holds(ctx, inTurns(w.match(), f), f); err != nil {
				return res, err
			}
		}
		if !excluded {
			if res, err = ix.closest(ctx, q, f, o); err != nil {
				return res, err
			}
		}
		res.Outside, err = ix.outside(ctx, match, f)
		return res, err
	}
	res.Total, res.Sessions = total, sessions
	res.Hits = ix.hits(ctx, cands, q, o, nil, f.SID == 0)
	return res, nil
}

// outside counts the turns an expression matches that the filter leaves out: with the
// filters a person set dropped — the store, the directory, the branch, the text, the time —
// and, should that find none, with archived sessions too. A search no turn the filter
// selects matches would otherwise answer that there is none, and be taken at its word, when
// a week, a project or an archive was all that hid it. Nil when there is nothing to say: no
// filter was set and no archived session holds it, or the search is of one session.
func (ix *Index) outside(ctx context.Context, match string, f Filter) (*Outside, error) {
	if f.SID != 0 {
		return nil, nil
	}
	o := &Outside{}
	for _, c := range []struct {
		name string
		set  bool
	}{
		{"source", f.Source != ""},
		{"project", f.Project != "" || len(f.Dirs) > 0 || len(f.In) > 0},
		{"branch", f.Branch != ""},
		{"text", f.Text != ""},
		{"since", !f.Since.IsZero()},
		{"until", !f.Until.IsZero()},
	} {
		if c.set {
			o.Filters = append(o.Filters, c.name)
		}
	}
	if len(o.Filters) == 0 && f.Archived {
		return nil, nil
	}
	wide := f
	wide.Source, wide.Project, wide.Dirs, wide.In, wide.Branch, wide.Text = "", "", nil, nil, "", ""
	wide.Since, wide.Until, wide.Archived = time.Time{}, time.Time{}, true
	where, args := wide.where()
	var turns, sessions, kept, keptSessions int
	err := ix.ro.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT t.sid), coalesce(sum(s.archived = 0), 0), count(DISTINCT CASE WHEN s.archived = 0 THEN t.sid END)`+
		wordsFrom+` WHERE words MATCH ? AND `+where, append([]any{match}, args...)...).Scan(&turns, &sessions, &kept, &keptSessions)
	if err != nil {
		return nil, err
	}
	switch {
	case f.Archived:
		o.Turns, o.Sessions = turns, sessions
	case kept > 0:
		o.Turns, o.Sessions = kept, keptSessions
	case turns > 0:
		o.Turns, o.Sessions, o.Archived = turns, sessions, true
	}
	if len(o.Filters) == 0 && o.Turns == 0 {
		return nil, nil
	}
	return o, nil
}

// turnColumns are the columns of the words table that hold a turn's own words.
const turnColumns = "{ask reply thinking tool output}"

// inTurns confines an expression to the turns' own words in a search of one session alone
// (Filter.SID), which is how the page's Find in a session asks: the title is the same for
// every turn of it, and would list the first, which holds it, for a word no turn says.
func inTurns(expr string, f Filter) string {
	if f.SID == 0 || expr == "" {
		return expr
	}
	return turnColumns + ": (" + expr + ")"
}

// cand is a turn a search found, before it is a hit.
type cand struct {
	sid   int64
	idx   int
	score float64
	at    int64
}

// wordsFrom joins each row of the words table to its turn, as t, and the turn's session, as
// s. Of the turn, a search takes its sid, idx, started_at and last_at, which turns_at holds:
// a word most turns hold is otherwise a read of most of the index.
const wordsFrom = ` FROM words JOIN turns t INDEXED BY turns_at ON t.sid = (words.rowid >> 20) AND t.idx = (words.rowid & 1048575)
JOIN sessions s ON s.sid = t.sid`

// wordsQuery is what a search asks of the words table: the rows an expression matches —
// and, for a partial match of more words than are spelled out (see atLeast), how many of
// terms each of those rows must match as well.
type wordsQuery struct {
	match string
	terms []string
	least int
}

// where renders the query as conditions on the words table, with their arguments.
func (w wordsQuery) where() (string, []any) {
	cond, args := `words MATCH ?`, []any{w.match}
	if w.least > 0 {
		n, more := heldCount(w.terms)
		cond += ` AND ` + n + ` >= ?`
		args = append(append(args, more...), w.least)
	}
	return cond, args
}

// candidates counts the turns a query matches, and ranks enough of them that the page
// asked for is filled after one session's surplus is set aside.
func (ix *Index) candidates(ctx context.Context, w wordsQuery, f Filter, o SearchOptions) (total, sessions int, cands []cand, err error) {
	cond, args := w.where()
	where, more := f.where()
	from := wordsFrom + ` WHERE ` + cond + ` AND ` + where
	args = append(args, more...)

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
// for a partial match, is what each turn lacks, by its rowid in the words table. titled
// says the words were looked for in the session's title too, in the turn that holds it.
func (ix *Index) hits(ctx context.Context, cands []cand, q Query, o SearchOptions, lacks map[int64][]string, titled bool) []Hit {
	out := []Hit{}
	perSession := map[int64]int{}
	sessions := map[int64]Session{}
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
		}
		turns, err := ix.Turns(ctx, &s, TurnRange{First: c.idx, Last: c.idx})
		if err != nil || len(turns) == 0 {
			continue
		}
		t := &turns[0]
		title := ""
		if titled && c.idx == titleTurn {
			title = s.Title
		}
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
			Matches: ix.matches(ctx, &s, title, t, q, held), Score: c.score,
		})
	}
	return out
}

// maxChoices is how many ways of choosing some of a query's words closest spells out in
// one expression. Past it, the turns that hold any of the words are taken, and each is
// counted the words it holds (see heldCount).
const maxChoices = 16

// closest answers a search no turn matched whole: with the turns that hold the most of
// its words, best first, each saying which words it lacks — and with the words no turn
// holds at all. The words a query excludes stay excluded.
//
// How many words those turns hold is found first, leaving out from the start any word no
// turn holds (see most); the turns that hold that many are then ranked as any search's
// are, and paged alike, however many words the query has.
func (ix *Index) closest(ctx context.Context, q Query, f Filter, o SearchOptions) (SearchResult, error) {
	res := SearchResult{Hits: []Hit{}}
	var words []Term
	var not []string
	for _, t := range q.Terms {
		switch e := inTurns(t.expr(), f); {
		case e == "":
		case t.Not:
			not = append(not, e)
		default:
			words = append(words, t)
		}
	}
	var present []Term
	var exprs []string // the present words' expressions
	for _, t := range words {
		e := inTurns(t.expr(), f)
		ok, err := ix.holds(ctx, e, f)
		if err != nil {
			return res, err
		}
		if ok {
			present, exprs = append(present, t), append(exprs, e)
		} else {
			res.Missing = append(res.Missing, t.String())
		}
	}
	if len(words) < 2 || len(present) == 0 {
		return res, nil
	}
	k, err := ix.most(ctx, exprs, not, f, min(len(present), len(words)-1))
	if err != nil || k == 0 {
		return res, err
	}
	total, sessions, cands, err := ix.candidates(ctx, atLeast(exprs, k, not), f, o)
	if err != nil || total == 0 {
		return res, err
	}
	held, err := ix.held(ctx, exprs, cands)
	if err != nil {
		return res, err
	}
	lacks := map[int64][]string{}
	for _, c := range cands {
		id := turnRowID(c.sid, c.idx)
		var l []string
		for i, t := range present {
			if !held[id][i] {
				l = append(l, t.String())
			}
		}
		lacks[id] = append(l, res.Missing...)
	}
	res.Total, res.Sessions = total, sessions
	res.Partial = &Partial{Words: len(words), Held: k}
	res.Hits = ix.hits(ctx, cands, q, o, lacks, f.SID == 0)
	return res, nil
}

// most is how many of the terms' expressions the turns that hold the most of them hold,
// among those the filter selects and none of not matches: at most k, which no turn holds
// more than, and 0 when none holds any. It asks for k of them together, then k-1, and so
// on, while there are few ways to choose them; past that, it counts them in each turn
// that holds any.
func (ix *Index) most(ctx context.Context, exprs, not []string, f Filter, k int) (int, error) {
	for ; k >= 1; k-- {
		w := atLeast(exprs, k, not)
		if w.least == 0 {
			ok, err := ix.holds(ctx, w.match, f)
			if err != nil || ok {
				return k, err
			}
			continue
		}
		n, args := heldCount(exprs)
		where, more := f.where()
		var most sql.NullInt64
		err := ix.ro.QueryRowContext(ctx, `SELECT max(`+n+`)`+wordsFrom+` WHERE words MATCH ? AND `+where,
			append(append(args, w.match), more...)...).Scan(&most)
		return min(int(most.Int64), k), err
	}
	return 0, nil
}

// atLeast is what a search asks of the words table for the turns that hold at least k of
// the terms' expressions and none of not: any k of them together — or, where there are
// more than maxChoices ways to choose them, any of them, each turn counted (see heldCount).
func atLeast(exprs []string, k int, not []string) wordsQuery {
	switch {
	case k >= len(exprs):
		return wordsQuery{match: excluding(strings.Join(exprs, " AND "), not)}
	case k <= 1:
		return wordsQuery{match: excluding(strings.Join(exprs, " OR "), not)}
	case choices(len(exprs), k) > maxChoices:
		return wordsQuery{match: excluding(strings.Join(exprs, " OR "), not), terms: exprs, least: k}
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
		for i := from; i <= len(exprs)-(k-at); i++ {
			pick[at] = i
			choose(i+1, at+1)
		}
	}
	choose(0, 0)
	return wordsQuery{match: excluding(strings.Join(groups, " OR "), not)}
}

// heldCount is SQL for how many of the expressions a row of the words table matches, with
// its arguments. The rows each expression matches are looked up once for the statement.
func heldCount(exprs []string) (string, []any) {
	parts := make([]string, len(exprs))
	args := make([]any, len(exprs))
	for i, e := range exprs {
		parts[i] = `(words.rowid IN (SELECT rowid FROM words WHERE words MATCH ?))`
		args[i] = e
	}
	return "(" + strings.Join(parts, " + ") + ")", args
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
	err := ix.ro.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1`+wordsFrom+` WHERE words MATCH ? AND `+where+` LIMIT 1)`,
		append([]any{match}, args...)...).Scan(&n)
	return n > 0, err
}

// held says which of the terms' expressions each candidate matches, by its rowid in the
// words table.
func (ix *Index) held(ctx context.Context, exprs []string, cands []cand) (map[int64][]bool, error) {
	out := make(map[int64][]bool, len(cands))
	ids := make([]any, 0, len(cands)+1)
	ids = append(ids, "")
	for _, c := range cands {
		id := turnRowID(c.sid, c.idx)
		out[id] = make([]bool, len(exprs))
		ids = append(ids, id)
	}
	for i, e := range exprs {
		ids[0] = e
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
// said, then in what was run, then in what came back — and in the session's title, given
// for the turn that holds it (see titleTurn), for a word the turn does not hold itself.
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
