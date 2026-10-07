package source

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Text this layer assembles.
//
// The rule the whole service follows: a message body is the data and goes back as stored,
// but a title, a snippet, a brief — anything built here for someone to read — is cleaned
// first. Terminal control sequences come off, then secret-shaped runs are redacted (in
// that order: a key with a colour code in the middle is one string only once the escapes
// are gone).
//
// These live in the source layer because both sides need them: sources clean the titles
// and display names their CLIs write, and the app layer cleans every snippet a search
// returns.

// IndexFold finds needleLower case-insensitively (needleLower must already be lowercase)
// and returns -1 when absent. It does not copy all of s first, saving an allocation on
// long lines.
func IndexFold(s, needleLower string) int {
	n := len(needleLower)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(s); i++ {
		hit := true
		for j := 0; j < n; j++ {
			c := s[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needleLower[j] {
				hit = false
				break
			}
		}
		if hit {
			return i
		}
	}
	return -1
}

// stripTerminalControls removes ANSI escape sequences and other control characters.
//
// A transcript records what a tool printed, and tool output is full of colour codes.
// Measured on this machine: 24 of 157 Claude sessions carry escape sequences, all of them
// on the rows that hold tool results, and searching a word inside that output returned 5
// snippets in 111 with a raw ESC still in them.
//
// Two things go wrong when that reaches a caller. The mild one is display: the snippet
// renders as mojibake anywhere that is not a terminal. The other is that a snippet is
// content the caller never chose to run. An OSC 52 in a tool log drives the clipboard of
// whoever prints it, and a cursor sequence moves their cursor.
//
// Only snippets are cleaned. A message body is returned exactly as stored, because that is
// the data and what to do with it is the caller's policy; a snippet is ours, cut for
// display, so it is ours to make safe.
func StripTerminalControls(s string) string {
	if strings.IndexFunc(s, isTerminalControl) < 0 {
		return s // the overwhelmingly common case, and it allocates nothing
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == 0x1b:
			i += escapeSequenceLen(s[i:])
		case c < utf8.RuneSelf && isTerminalControl(rune(c)):
			i++
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			out = append(out, s[i:i+size]...)
			i += size
		}
	}
	return string(out)
}

// isTerminalControl reports whether a rune is a control character worth removing.
// Tab and newline stay: they are layout inside the text, not instructions to a terminal.
// Carriage return goes, because on a terminal it rewrites the line already printed.
func isTerminalControl(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f
}

// Secrets in text this service assembles.
//
// The rule stripTerminalControls follows applies here too: a message body is the data and
// goes back as stored, but a snippet, a title and a brief are built here for someone to
// read, and a brief exists to be copied and handed to another agent. Measured on this
// machine, one real brief carried a live 51-character API key, a private hostname and two
// home paths, because the user had pasted the key into a prompt and the brief quotes the
// prompt.
//
// This is best effort and is documented as such. It recognises the shapes that announce
// themselves — a known prefix followed by a long opaque run, a JWT, a PEM header — and
// nothing else. A password in prose is not detectable and is not claimed to be.
var secretPatterns = []*regexp.Regexp{
	// Provider keys: a known prefix and a long tail. The tail is checked for entropy
	// below, because a kebab-case name can start with sk- too.
	regexp.MustCompile(`\b[sprk]k-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{30,}`),
	// AWS key ids are a fixed shape, so they need no entropy check
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	// A JWT: three base64url runs, the first one starting with the encoded "{"
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

// RedactSecrets replaces the secret-shaped runs in text this service assembled.
//
// Run after stripTerminalControls, never before: a key with a colour code in the middle of
// it is one string only once the escapes are gone.
func RedactSecrets(s string) string {
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

// secretLike rejects what a prefix alone would let through. A generated key packs digits
// and mixed case into one unbroken run; an identifier that happens to start with sk- is
// words joined by dashes and has neither.
func secretLike(match string) bool {
	if !strings.HasPrefix(match, "sk-") && !strings.HasPrefix(match, "pk-") &&
		!strings.HasPrefix(match, "rk-") && !strings.HasPrefix(match, "kk-") {
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

// escapeSequenceLen is the length of the escape sequence starting at s[0], which the
// caller has already established is ESC.
//
// The shape of what follows the ESC decides the length, and getting this wrong is not
// cosmetic: returning too few leaves the tail of a sequence in the output as text, and
// returning too many eats text that was never part of one.
//
//	[                 CSI: parameters and intermediates, then one final byte in @..~
//	] P X ^ _         a string sequence, run to BEL or to ST (ESC backslash)
//	0x20..0x2f        nF: more intermediates, then one final byte in 0..~
//	0x30..0x7e        a complete two-byte escape (ESC M, ESC 7, ESC c, ...)
//	anything else     not a sequence at all
//
// The nF row is the one this got wrong at first, and it is not exotic: ESC ( B is what
// tput sgr0 writes, so it is in anything ncurses, less, vim or git coloured. Treating it
// as a two-byte escape left its final byte behind as a stray "B".
//
// The last row matters as much. A second ESC starts a new sequence rather than ending
// this one, and a byte at or above 0x80 is the lead byte of a rune. Consuming either
// along with the ESC would swallow a real sequence, or cut a rune in half and leave its
// continuation bytes behind as invalid UTF-8. Consuming the ESC alone lets the next pass
// see the byte whole.
//
// An unterminated sequence still takes the rest of the string, since leaving the tail of
// a half-written CSI behind would put back the bytes this removes.
func escapeSequenceLen(s string) int {
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
				return i // not a continuation; whatever this is, it is not part of the sequence
			}
		}
		return len(s)
	case c >= 0x30 && c <= 0x7e:
		return 2
	default:
		return 1
	}
}

// snippetAround cuts radius characters either side of the hit, marking each end it had
// to trim.

// SearchSnippetRadius is how many characters a snippet keeps on each side of a hit
const SearchSnippetRadius = 70

func SnippetAround(text, needleLower string, radius int) string {
	// Cleaned before the window is cut, not after: an escape sequence counted toward the
	// radius would spend the snippet's budget on bytes nobody sees, and cutting inside one
	// would leave its tail behind.
	text = RedactSecrets(StripTerminalControls(text))
	idx := IndexFold(text, needleLower)
	if idx < 0 {
		return Truncate(text, radius*2, "…")
	}

	// Open a generous byte window first (UTF-8 is at most 4 bytes per character), then
	// narrow it down to a character count
	lo, hi := idx-radius*4, idx+len(needleLower)+radius*4
	if lo < 0 {
		lo = 0
	}
	if hi > len(text) {
		hi = len(text)
	}
	for lo > 0 && !utf8.RuneStart(text[lo]) { // align to a character boundary
		lo--
	}
	for hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi++
	}

	head := trimRunesFromLeft(text[lo:idx], radius)
	tail := trimRunesFromRight(text[idx:hi], radius+utf8.RuneCountInString(needleLower))
	out := head + tail
	if lo > 0 || len(head) < idx-lo {
		out = "…" + out
	}
	if hi < len(text) || len(tail) < hi-idx {
		out += "…"
	}
	return out
}

// trimRunesFromLeft keeps only the last n characters
func trimRunesFromLeft(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	drop := utf8.RuneCountInString(s) - n
	for i := range s {
		if drop == 0 {
			return s[i:]
		}
		drop--
	}
	return ""
}

// trimRunesFromRight keeps only the first n characters
func trimRunesFromRight(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
