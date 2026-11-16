// Package txt cleans and cuts the text this program assembles for someone to read.
//
// The rule the whole program follows: a transcript is the data and goes back as stored,
// but a title, a snippet, a brief — anything built here — is cleaned first. Terminal
// control sequences come off, then secret-shaped runs are redacted, in that order: a key
// with a colour code in the middle is one string only once the escapes are gone.
package txt

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Clean is the two steps every assembled text goes through.
func Clean(s string) string { return Redact(StripControls(s)) }

// StripControls removes ANSI escape sequences and other control characters.
//
// A transcript records what a tool printed, and tool output is full of colour codes.
// Two things go wrong when that reaches a reader. The mild one is display: it renders as
// mojibake anywhere that is not a terminal. The other is that it is content the reader
// never chose to run — an OSC 52 in a tool log drives the clipboard of whoever prints it.
func StripControls(s string) string {
	if strings.IndexFunc(s, isControl) < 0 {
		return s // the overwhelmingly common case, and it allocates nothing
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == 0x1b:
			i += escapeLen(s[i:])
		case c < utf8.RuneSelf && isControl(rune(c)):
			i++
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			out = append(out, s[i:i+size]...)
			i += size
		}
	}
	return string(out)
}

// isControl reports whether a rune is a control character worth removing. Tab and newline
// stay: they are layout inside the text. Carriage return goes, because on a terminal it
// rewrites the line already printed.
func isControl(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f
}

// escapeLen is the length of the escape sequence starting at s[0], which is ESC.
//
// The shape of what follows the ESC decides the length, and getting it wrong is not
// cosmetic: too few leaves the tail of a sequence in the output as text, too many eats
// text that was never part of one.
//
//	[                 CSI: parameters and intermediates, then one final byte in @..~
//	] P X ^ _         a string sequence, run to BEL or to ST (ESC backslash)
//	0x20..0x2f        nF: more intermediates, then one final byte in 0..~  (ESC ( B)
//	0x30..0x7e        a complete two-byte escape (ESC M, ESC 7, ESC c, ...)
//	anything else     not a sequence at all: the ESC alone is dropped
//
// An unterminated sequence takes the rest of the string, since leaving the tail of a
// half-written CSI behind would put back the bytes this removes.
func escapeLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch c := s[1]; {
	case c == '[':
		for i := 2; i < len(s); i++ {
			if b := s[i]; b >= 0x40 && b <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case c == ']', c == 'P', c == 'X', c == '^', c == '_':
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	case c >= 0x20 && c <= 0x2f:
		for i := 2; i < len(s); i++ {
			b := s[i]
			if b >= 0x30 && b <= 0x7e {
				return i + 1
			}
			if b < 0x20 || b > 0x2f {
				return i
			}
		}
		return len(s)
	case c >= 0x30 && c <= 0x7e:
		return 2
	default:
		return 1
	}
}

// Secrets in assembled text. Best effort, and documented as such: this recognises the
// shapes that announce themselves — a known prefix followed by a long opaque run, a JWT,
// a PEM header — and nothing else. A password in prose is not detectable.
var secretPatterns = []*regexp.Regexp{
	// Provider keys: a known prefix and a long tail. The tail is checked below, because
	// a kebab-case name can start with sk- too.
	regexp.MustCompile(`\b[sprk]k-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{30,}`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

// Redact replaces secret-shaped runs with "[redacted]". Run it after StripControls.
func Redact(s string) string {
	if s == "" {
		return s
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(match string) string {
			if !secretLike(match) {
				return match
			}
			return "[redacted]"
		})
	}
	return s
}

// secretLike rejects what a prefix alone would let through: a generated key packs digits
// and mixed case into one run; an identifier that happens to start with sk- has neither.
func secretLike(match string) bool {
	if len(match) < 3 || match[2] != '-' || !strings.ContainsRune("sprk", rune(match[0])) || match[1] != 'k' {
		return true // the other shapes are distinctive enough on their own
	}
	digit, upper := false, false
	for _, r := range match[3:] {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'A' && r <= 'Z':
			upper = true
		}
	}
	return digit && upper
}

// Truncate cuts to limit characters, appending mark when it cuts.
func Truncate(s string, limit int, mark string) string {
	if limit <= 0 {
		return s
	}
	if len(s) <= limit { // bytes ≥ runes: short in bytes is short in runes
		return s
	}
	count := 0
	for i := range s {
		if count == limit {
			return s[:i] + mark
		}
		count++
	}
	return s
}

// Cap cleans s and shortens it to about n characters, preferring to cut at a space.
// It is how an ask or an answer is quoted in a line.
func Cap(s string, n int) string {
	s = strings.TrimSpace(Clean(s))
	runes := []rune(s)
	if n <= 0 || len(runes) <= n {
		return s
	}
	cut := runes[:n]
	for i := len(cut) - 1; i > n/2; i-- {
		if cut[i] == ' ' || cut[i] == '\n' {
			cut = cut[:i]
			break
		}
	}
	return strings.TrimRight(string(cut), " \n") + " …"
}

// OneLine folds text to a single cleaned line of at most n characters.
func OneLine(s string, n int) string {
	return Cap(strings.Join(strings.Fields(Clean(s)), " "), n)
}

// FirstLine is the first non-empty line of s, trimmed.
func FirstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// Preview keeps the head and the tail of a long text and says how much it left out. The
// tail gets the larger share: the end of an output is where a command says how it failed.
func Preview(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	head, tail := limit/4, limit-limit/4
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	from := len(s) - tail
	for from < len(s) && !utf8.RuneStart(s[from]) {
		from++
	}
	return s[:head] + "\n… " + bytesHuman(from-head) + " left out …\n" + s[from:], true
}

func bytesHuman(n int) string {
	switch {
	case n >= 1<<20:
		return trimFloat(float64(n)/(1<<20)) + " MB"
	case n >= 1<<10:
		return trimFloat(float64(n)/(1<<10)) + " KB"
	}
	return itoa(n) + " bytes"
}
