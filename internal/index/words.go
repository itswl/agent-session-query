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
// a quoted phrase — match as written.
func (t Term) expr() string {
	toks := tokens(t.Text)
	if len(toks) == 0 {
		return ""
	}
	for i, tok := range toks {
		toks[i] = strings.ReplaceAll(tok, `"`, `""`)
	}
	expr := `"` + strings.Join(toks, " ") + `"`
	// The last word of a bare term matches what it begins — unless it is a single
	// character of a spaceless script, which is already as short as a word gets
	if last := toks[len(toks)-1]; !t.Exact && !(utf8.RuneCountInString(last) == 1 && spaced([]rune(last)[0])) {
		expr += "*"
	}
	if w := toks[0]; !t.Exact && len(toks) == 1 && stemmable(w) {
		s := stem(w)
		forms := []string{expr}
		if s != w {
			forms = append(forms, `"`+s+`"`)
		}
		expr = "(" + strings.Join(append(forms, `"`+stemMark+s+`"`), " OR ") + ")"
	}
	if t.Column != "" {
		expr = "{" + t.Column + "}: " + expr
	}
	return expr
}

// needle is a term of a query as it is looked for in a turn's text, to cut a snippet
// around: as typed, and, for an English word alone, in any of its forms.
type needle struct {
	text string // lowercased
	stem string // the word's stem; "" when the term is not a word alone
	term string // the term it is of, as typed
}

// needles are the query's positive terms as text to look for when cutting a snippet: the
// term as typed, and its first word should the typed form not appear verbatim.
func (q Query) needles() []needle {
	var out []needle
	for _, t := range q.Terms {
		if t.Not {
			continue
		}
		n := needle{text: strings.ToLower(t.Text), term: t.String()}
		if toks := tokens(t.Text); !t.Exact && len(toks) == 1 && stemmable(toks[0]) {
			n.stem = stem(toks[0])
		}
		out = append(out, n)
	}
	for _, t := range q.Terms {
		if t.Not {
			continue
		}
		if toks := tokens(t.Text); len(toks) > 1 {
			out = append(out, needle{text: toks[0], term: t.String()})
		}
	}
	return out
}

// find is where in text the needle first is: the typed form anywhere, or failing that a
// word whose stem is the needle's. word is the form found, when it is not the typed one.
func (n needle) find(text string) (at, size int, word string) {
	if at := indexFold(text, n.text); at >= 0 {
		return at, len(n.text), ""
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
