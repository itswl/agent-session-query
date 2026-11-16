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

// tokens splits text the way the words table does: runs of letters and digits, each
// character of a spaceless script alone, all lowercased.
func tokens(s string) []string {
	var out []string
	start := -1
	flush := func(end int) {
		if start >= 0 {
			out = append(out, strings.ToLower(s[start:end]))
			start = -1
		}
	}
	for i, r := range s {
		switch {
		case spaced(r):
			flush(i)
			out = append(out, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if start < 0 {
				start = i
			}
		default:
			flush(i)
		}
	}
	flush(len(s))
	return out
}

// Query is a search as a person types it, parsed.
//
//	retry backoff            both words, in any part of the turn
//	"exit status 1"          the phrase
//	-flaky                   not this word
//	ask:deploy               the word, in what the person asked
//	                         (also reply: thinking: tool: output:)
//
// A bare word also matches the words it begins: rebind finds rebinding. A quoted phrase
// matches as written.
type Query struct {
	Terms []Term
}

// Term is one condition of a query.
type Term struct {
	Text   string // as typed, without quotes or prefix
	Column string // ask, reply, thinking, tool, output; empty for any
	Not    bool
	Exact  bool // a quoted phrase: no prefix matching
}

var columns = map[string]bool{"ask": true, "reply": true, "thinking": true, "tool": true, "output": true}

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
		toks := tokens(t.Text)
		if len(toks) == 0 {
			continue
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
		if t.Column != "" {
			expr = "{" + t.Column + "}: " + expr
		}
		if t.Not {
			not = append(not, expr)
		} else {
			must = append(must, expr)
		}
	}
	if len(must) == 0 {
		return ""
	}
	out := strings.Join(must, " AND ")
	for _, n := range not {
		out = "(" + out + ") NOT " + n
	}
	return out
}

// Needles are the query's positive terms as text to look for when cutting a snippet:
// the term as typed, and its first word should the typed form not appear verbatim.
func (q Query) Needles() []string {
	var out []string
	for _, t := range q.Terms {
		if t.Not {
			continue
		}
		out = append(out, strings.ToLower(t.Text))
	}
	for _, t := range q.Terms {
		if t.Not {
			continue
		}
		if toks := tokens(t.Text); len(toks) > 1 {
			out = append(out, toks[0])
		}
	}
	return out
}
