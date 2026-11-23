package index

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The words table tokenises on anything that is not a letter or a digit, which is right
// for prose, code and paths alike: session_query, session-query and "session query" are
// the same two words. It is wrong for the scripts that write no spaces — a run of Han
// characters would be one token, findable only whole. So each such character is set
// apart as a word of its own, on the way in and in a query alike, and a query for 数据库
// becomes the phrase "数 据 库": an exact substring match, at no cost in index size.
//
// English words are found in any of their forms. Beside the words of each column the table
// holds the stems of those that have one other than themselves (see stemsOf), and a word
// searched for alone matches by its stem as well as by how it begins (see Term.expr).

// spaced reports whether a rune belongs to a script written without spaces between words.
func spaced(r rune) bool {
	switch {
	case r < 0x0E00: // nothing below Thai is: the test that every Latin, Greek and Cyrillic letter takes
		return false
	case r < 0x2E80:
		return unicode.In(r, unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar) || (r >= 0x1100 && r <= 0x11FF) // … and Hangul's jamo
	}
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// segment sets every character of a spaceless script apart. Text with none comes back as
// it was, without a copy.
func segment(s string) string {
	i := 0
	for i < len(s) {
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if spaced(r) {
			break
		}
		i += size
	}
	if i == len(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(s)/2)
	b.WriteString(s[:i])
	for _, r := range s[i:] {
		if spaced(r) {
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// eachToken calls fn with where each word of text starts and ends, the way the words table
// splits it: runs of letters and digits, and each character of a spaceless script alone.
// It stops when fn returns false.
func eachToken(s string, fn func(start, end int) bool) {
	start := -1
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
				if start < 0 {
					start = i
				}
			} else if start >= 0 {
				if !fn(start, i) {
					return
				}
				start = -1
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case spaced(r):
			if start >= 0 {
				if !fn(start, i) {
					return
				}
				start = -1
			}
			if !fn(i, i+size) {
				return
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if start < 0 {
				start = i
			}
		default:
			if start >= 0 {
				if !fn(start, i) {
					return
				}
				start = -1
			}
		}
		i += size
	}
	if start >= 0 {
		fn(start, len(s))
	}
}

// tokens splits text the way the words table does, all lowercased.
func tokens(s string) []string {
	var out []string
	eachToken(s, func(start, end int) bool {
		out = append(out, strings.ToLower(s[start:end]))
		return true
	})
	return out
}

// lowerWord is a token as the stemmer takes it — lowercased into buf — or false when it is
// no word the stemmer knows: shorter than three letters or longer than sixty-four, or with
// anything in it but the letters of the English alphabet.
func lowerWord(tok string, buf *[maxStemmed]byte) ([]byte, bool) {
	if len(tok) < 3 || len(tok) > maxStemmed {
		return nil, false
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		default:
			return nil, false
		}
		buf[i] = c
	}
	return buf[:len(tok)], true
}

// stemsOf is what the words table holds beside a column's own words: the stem of each
// English word in text that has one other than itself, marked, once each, in the order
// they first appear — "retried, retries" adds "ǂretri" once. Only a word's stem is added,
// so that a search for one form finds the others while the words keep their own forms
// for phrases and prefixes. "" when there is none.
func stemsOf(text string) string {
	var b strings.Builder
	words := map[string]bool{} // the words already stemmed
	stems := map[string]bool{} // the stems already added
	var buf [maxStemmed]byte
	eachToken(text, func(start, end int) bool {
		w, ok := lowerWord(text[start:end], &buf)
		if !ok || words[string(w)] {
			return true
		}
		word := string(w)
		words[word] = true
		s := stem(word)
		if s == word || stems[s] {
			return true
		}
		stems[s] = true
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(stemMark)
		b.WriteString(s)
		return true
	})
	return b.String()
}

// withStems is a column's text as the words table takes it: followed by the stems of its
// words, on a line of their own.
func withStems(text string) string {
	if text == "" {
		return ""
	}
	if stems := stemsOf(text); stems != "" {
		return text + "\n" + stems
	}
	return text
}

// Query is a search as a person types it, parsed.
//
//	retry backoff            both words, in any part of the turn or the session's title
//	"exit status 1"          the phrase
//	-flaky                   not this word
//	ask:deploy               the word, in what the person asked
//	                         (also reply: thinking: tool: output: title:)
//
// A bare word also matches the words it begins — rebind finds rebinding — and the other
// forms of the same English word: retries finds retried and retry. A quoted phrase
// matches as written.
type Query struct {
	Terms []Term
}

// Term is one condition of a query.
type Term struct {
	Text   string // as typed, without quotes or prefix
	Column string // ask, reply, thinking, tool, output, title; empty for any
	Not    bool
	Exact  bool // a quoted phrase: no prefix matching, no other forms
}

// String is the term as it would be typed.
func (t Term) String() string {
	s := t.Text
	if t.Exact {
		s = `"` + s + `"`
	}
	if t.Column != "" {
		s = t.Column + ":" + s
	}
	if t.Not {
		s = "-" + s
	}
	return s
}

var columns = map[string]bool{"ask": true, "reply": true, "thinking": true, "tool": true, "output": true, "title": true}

// ParseQuery reads a query string.
func ParseQuery(s string) Query {
	var q Query
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t\n")
		if s == "" {
			break
		}
		var t Term
		if s[0] == '-' && len(s) > 1 && s[1] != ' ' {
			t.Not = true
			s = s[1:]
		}
		if i := strings.IndexByte(s, ':'); i > 0 && columns[strings.ToLower(s[:i])] && i+1 < len(s) && s[i+1] != ' ' {
			t.Column = strings.ToLower(s[:i])
			s = s[i+1:]
		}
		if s[0] == '"' {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				t.Text, s = s[1:], ""
			} else {
				t.Text, s = s[1:1+end], s[end+2:]
			}
			t.Exact = true
		} else {
			end := strings.IndexAny(s, " \t\n")
			if end < 0 {
				end = len(s)
			}
			t.Text, s = s[:end], s[end:]
		}
		if len(tokens(t.Text)) > 0 {
			q.Terms = append(q.Terms, t)
		}
	}
	return q
}

// Empty reports whether the query asks for nothing.
func (q Query) Empty() bool {
	for _, t := range q.Terms {
		if !t.Not {
			return false
		}
	}
	return true
}

// match renders the query in the words table's own syntax.
func (q Query) match() string {
	var must, not []string
	for _, t := range q.Terms {
		expr := t.expr()
		switch {
		case expr == "":
		case t.Not:
			not = append(not, expr)
		default:
			must = append(must, expr)
		}
	}
	if len(must) == 0 {
		return ""
	}
	return excluding(strings.Join(must, " AND "), not)
}

// words is the query without the words it excludes.
func (q Query) words() Query {
	var out Query
	for _, t := range q.Terms {
		if !t.Not {
			out.Terms = append(out.Terms, t)
		}
	}
	return out
}

// excluding is an expression less the turns any of not matches.
func excluding(expr string, not []string) string {
	for _, n := range not {
		expr = "(" + expr + ") NOT " + n
	}
	return expr
}

// expr renders one term in the words table's syntax, without its -.
//
// A word alone, not quoted, matches three ways: by how a word begins, as it always has
// ("retry"*: retry, retrying, retryable); by being the stem itself, for the words that are
// their own stem ("log", for logs); and by the stem the table holds beside the words that
// have another ("ǂretri": retried, retries). Terms of more than one word — session_query,
// a quoted phrase — match as written, and so does a word excluded, by how a word begins:
// -windows leaves out a turn that says windows, and not one that says only window.
func (t Term) expr() string {
	toks := tokens(t.Text)
	if len(toks) == 0 {
		return ""
	}
	for i, tok := range toks {
		toks[i] = strings.ReplaceAll(tok, `"`, `""`)
	}
	expr := `"` + strings.Join(toks, " ") + `"`
	if t.prefix(toks) {
		expr += "*"
	}
	if w := toks[0]; !t.Exact && !t.Not && len(toks) == 1 && stemmable(w) {
		s := stem(w)
		forms := []string{expr}
		if s != w {
			forms = append(forms, `"`+s+`"`)
		}
		expr = "(" + strings.Join(append(forms, `"`+stemMark+s+`"`), " OR ") + ")"
	}
	if w := toks[0]; !t.Exact && !t.Not && len(toks) == 1 && commitID(w) {
		// A commit's id also finds it cut short, as git commit and git log --oneline print
		// it: the full id from GitHub or git log finds the turn whose commit printed seven
		// characters of it
		forms := []string{expr}
		for n := shortestID; n < len(w); n++ {
			forms = append(forms, `"`+w[:n]+`"`)
		}
		expr = "(" + strings.Join(forms, " OR ") + ")"
	}
	if t.Column != "" {
		expr = "{" + t.Column + "}: " + expr
	}
	return expr
}

// prefix reports whether the last of a term's words, toks, matches the words it begins: in
// a bare term it does — unless it is a single character of a spaceless script, which is
// already as short as a word gets, or the term goes on past it: what is typed after a word
// says where it ends, and c++ is the word c, not every word that begins with c. A * says
// the opposite, as it does in most searches: retr* is every word that begins with retr.
func (t Term) prefix(toks []string) bool {
	last := toks[len(toks)-1]
	if t.Exact || utf8.RuneCountInString(last) == 1 && spaced([]rune(last)[0]) {
		return false
	}
	r := lastRune(t.Text)
	return r == '*' || wordRune(r)
}

// wordRune reports whether a rune is part of a word as the words table splits text, beside
// others: a letter or a digit, of a script that writes spaces between words.
func wordRune(r rune) bool {
	return (unicode.IsLetter(r) || unicode.IsDigit(r)) && !spaced(r)
}

// lastRune is the last character of s; utf8.RuneError when there is none.
func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

// shortestID is the fewest characters git cuts a commit's id to: git commit and git log
// --oneline print seven, or more where seven do not tell it from another.
const shortestID = 7

// commitID reports whether a word may be a commit's id: eight to forty hexadecimal digits,
// a letter among them and a digit — a word of digits alone is a number, and one of letters
// alone a word, far more often than an id.
func commitID(w string) bool {
	if len(w) <= shortestID || len(w) > 40 {
		return false
	}
	var digit, letter bool
	for i := 0; i < len(w); i++ {
		switch c := w[i]; {
		case '0' <= c && c <= '9':
			digit = true
		case 'a' <= c && c <= 'f':
			letter = true
		default:
			return false
		}
	}
	return digit && letter
}

// needle is a term of a query as it is looked for in a turn's text, to cut a snippet
// around: as typed, as the words table splits it, and, for an English word alone, in any of
// its forms; for a commit's id, cut short.
type needle struct {
	text   string   // lowercased
	toks   []string // its words, lowercased, as the words table splits them
	prefix bool     // the last of them matches the words it begins
	stem   string   // the word's stem; "" when the term is not a word alone
	id     bool     // a commit's id, found too by a word that is the start of it
	term   string   // the term it is of, as typed
}

// needles are the query's positive terms as text to look for when cutting a snippet.
func (q Query) needles() []needle {
	var out []needle
	for _, t := range q.Terms {
		if t.Not {
			continue
		}
		toks := tokens(t.Text)
		n := needle{text: strings.ToLower(t.Text), toks: toks, prefix: t.prefix(toks), term: t.String()}
		if !t.Exact && len(toks) == 1 && stemmable(toks[0]) {
			n.stem = stem(toks[0])
		}
		if !t.Exact && len(toks) == 1 && commitID(toks[0]) {
			n.text, n.id = toks[0], true
		}
		out = append(out, n)
	}
	return out
}

// findWords is where in text the needle's words first come one after another, as the
// words table splits text, and how far they reach.
func (n needle) findWords(text string) (at, size int) {
	k := len(n.toks)
	if k == 0 {
		return -1, 0
	}
	type span struct{ start, end int }
	last := make([]span, 0, k) // the latest k words
	at = -1
	eachToken(text, func(start, end int) bool {
		if len(last) == k {
			last = append(last[:0], last[1:]...)
		}
		last = append(last, span{start, end})
		if len(last) < k {
			return true
		}
		for i, sp := range last {
			w, want := text[sp.start:sp.end], n.toks[i]
			if i == k-1 && n.prefix && len(w) > len(want) {
				w = w[:len(want)]
			}
			if !strings.EqualFold(w, want) {
				return true
			}
		}
		at, size = last[0].start, last[k-1].end-last[0].start
		return false
	})
	return at, size
}

// findTyped is where in text the needle first stands as typed, without regard to case, and
// how far it reaches there: where the words table would find it, at the start of a word, and
// at its end unless its last word is a prefix — go is not in multipart.go, nor a quoted
// "exit status" in exit statuses. A needle that begins or ends with what is not part of a
// word, or with a character of a spaceless script, stands wherever it is found.
func (n needle) findTyped(text string) (at, size int) {
	if n.text == "" {
		return -1, 0
	}
	// Lowering a character gives one character, whose length in bytes may differ: offsets in
	// the lowered text are mapped back by counting characters, when the lengths differ
	lower := strings.ToLower(text)
	first, _ := utf8.DecodeRuneInString(n.text)
	starts, ends := wordRune(first), !n.prefix && wordRune(lastRune(n.text))
	for off := 0; off < len(lower); {
		i := strings.Index(lower[off:], n.text)
		if i < 0 {
			return -1, 0
		}
		i += off
		end := i + len(n.text)
		after, _ := utf8.DecodeRuneInString(lower[end:])
		if !(starts && wordRune(lastRune(lower[:i]))) && !(ends && wordRune(after)) {
			if len(lower) == len(text) {
				return i, len(n.text)
			}
			at = runeOffset(text, utf8.RuneCountInString(lower[:i]))
			return at, runeOffset(text[at:], utf8.RuneCountInString(n.text))
		}
		_, step := utf8.DecodeRuneInString(lower[i:])
		off = i + step
	}
	return -1, 0
}

// runeOffset is where in s its nth character begins.
func runeOffset(s string, n int) int {
	off := 0
	for range n {
		if off >= len(s) {
			break
		}
		_, size := utf8.DecodeRuneInString(s[off:])
		off += size
	}
	return off
}

// find is where in text the needle first is: the typed form, where it stands as the words
// table would find it, or failing that its words one after another, whatever is between
// them — session_query is session-query — or a word whose stem is the needle's. word is the
// form found, when it is not the typed one.
func (n needle) find(text string) (at, size int, word string) {
	if at, size := n.findTyped(text); at >= 0 {
		return at, size, ""
	}
	if at, size := n.findWords(text); at >= 0 {
		return at, size, ""
	}
	if n.id {
		// The id cut short: a word of at least seven characters that it begins with
		at = -1
		eachToken(text, func(start, end int) bool {
			if end-start >= shortestID && end-start < len(n.text) && strings.HasPrefix(n.text, strings.ToLower(text[start:end])) {
				at, size, word = start, end-start, text[start:end]
				return false
			}
			return true
		})
		return at, size, word
	}
	if n.stem == "" {
		return -1, 0, ""
	}
	at = -1
	var buf [maxStemmed]byte
	eachToken(text, func(start, end int) bool {
		// A stem keeps the first letter of its word: most words are passed over on that
		if text[start]|0x20 != n.stem[0] {
			return true
		}
		if w, ok := lowerWord(text[start:end], &buf); ok && stem(string(w)) == n.stem {
			at, size, word = start, end-start, text[start:end]
			return false
		}
		return true
	})
	return at, size, word
}
