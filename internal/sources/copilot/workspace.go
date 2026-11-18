package copilot

import (
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// workspace is what workspace.yaml, beside a session's log, says of the session that its
// events do not: the name the session goes by.
type workspace struct {
	// Name is the session's name — one the person gave it (/rename, --name) or one the CLI
	// or its agent chose — and Summary the short summary the CLI derived, which it falls back
	// to without a name: its own list of sessions shows name ?? summary.
	Name, Summary string
	Cwd           string // the directory the session started in
}

// readWorkspace reads a session's workspace.yaml; a file that is not there, or not
// readable, says nothing.
func readWorkspace(path string) workspace {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > maxWorkspace {
		return workspace{}
	}
	v := yamlScalars(string(raw))
	return workspace{Name: v["name"], Summary: v["summary"], Cwd: v["cwd"]}
}

// maxWorkspace bounds what is read of a workspace.yaml: the CLI writes a dozen short
// fields, and a file of megabytes is not one of its.
const maxWorkspace = 1 << 20

// yamlScalars reads the top-level scalars of a YAML mapping — the shape of workspace.yaml,
// which Copilot CLI writes with serde_yaml from its runtime and with the yaml package from
// its JavaScript: one "key: value" per line, values plain, quoted or block scalars, a long
// one folded onto indented lines. A key whose value is a nested collection, or that is not
// written as a plain word, is left out; the rest of the file is still read.
func yamlScalars(doc string) map[string]string {
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	out := map[string]string{}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "...") {
			continue
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok || key == "" || strings.ContainsAny(key, `"'{}[],&*!|>%@`+"`") || rest != "" && rest[0] != ' ' && rest[0] != '\t' {
			continue
		}
		// The value goes on for as long as the lines after it are indented, or blank
		var more []string
		for i+1 < len(lines) && (lines[i+1] == "" || lines[i+1][0] == ' ' || lines[i+1][0] == '\t') {
			i++
			more = append(more, lines[i])
		}
		if value, ok := scalar(strings.TrimSpace(rest), more); ok {
			out[strings.TrimSpace(key)] = value
		}
	}
	return out
}

// scalar reads one value: its first line's text after the key, and the indented lines
// that continue it.
func scalar(first string, more []string) (string, bool) {
	switch {
	case first == "":
		// A nested collection, or a value on the lines below (rare, and plain)
		for _, l := range more {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "- ") || t == "-" || strings.Contains(t, ": ") || strings.HasSuffix(t, ":") {
				return "", false
			}
		}
		return fold(more), true
	case first[0] == '|' || first[0] == '>':
		return block(first, more), true
	case first[0] == '"':
		return doubleQuoted(strings.Join(append([]string{first}, more...), "\n"))
	case first[0] == '\'':
		return singleQuoted(strings.Join(append([]string{first}, more...), "\n"))
	case first[0] == '{' || first[0] == '[' || first[0] == '&' || first[0] == '*' || first[0] == '!':
		return "", false // a flow collection, an anchor, an alias or a tag: no plain string
	}
	if c := strings.Index(first, " #"); c >= 0 {
		first = strings.TrimSpace(first[:c])
	}
	if first == "~" || first == "null" {
		return "", true
	}
	return fold(append([]string{first}, more...)), true
}

// fold joins the lines of a plain scalar as YAML does: one line break is a space, and each
// blank line between two lines is a line break of its own.
func fold(lines []string) string {
	var b strings.Builder
	blank := 0
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			blank++
			continue
		}
		if b.Len() > 0 {
			if blank > 0 {
				b.WriteString(strings.Repeat("\n", blank))
			} else {
				b.WriteByte(' ')
			}
		}
		blank = 0
		b.WriteString(t)
	}
	return b.String()
}

// block reads a literal (|) or folded (>) block scalar: the indented lines below its
// header, their common indentation taken off. Trailing line breaks are dropped, whatever
// the header asks of them: a name or a directory does not end in one.
func block(header string, lines []string) string {
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	body := make([]string, len(lines))
	for i, l := range lines {
		if len(l) >= indent && indent >= 0 {
			body[i] = l[indent:]
		}
	}
	text := strings.Join(body, "\n")
	if header[0] == '>' {
		text = fold(body)
	}
	return strings.TrimRight(text, "\n")
}

// singleQuoted reads a single-quoted scalar: two quotes in a row stand for one, and lines
// fold as a plain scalar's do. Text after the closing quote is a comment, or not this shape.
func singleQuoted(s string) (string, bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return fold(strings.Split(b.String(), "\n")), true
	}
	return "", false // never closed
}

// doubleQuoted reads a double-quoted scalar: its backslash escapes, and its lines folded
// — except where a line ends in a backslash, which joins it to the next with nothing
// between.
func doubleQuoted(s string) (string, bool) {
	var lines []string
	var cur strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			lines = append(lines, cur.String())
			return foldQuoted(lines), true
		case '\n':
			lines = append(lines, cur.String())
			cur.Reset()
		case '\\':
			if i+1 >= len(s) {
				return "", false
			}
			i++
			if s[i] == '\n' {
				// An escaped line break: the next line carries on this one, its indentation
				// not part of the text
				for i+1 < len(s) && (s[i+1] == ' ' || s[i+1] == '\t') {
					i++
				}
				continue
			}
			r, width, ok := unescape(s[i:])
			if !ok {
				return "", false
			}
			cur.WriteRune(r)
			i += width - 1
		default:
			cur.WriteByte(c)
		}
	}
	return "", false // never closed
}

// foldQuoted folds the lines of a quoted scalar: each line keeps its words, not the
// indentation it was written at, and one break is a space.
func foldQuoted(lines []string) string {
	if len(lines) == 1 {
		return lines[0]
	}
	trimmed := make([]string, len(lines))
	for i, l := range lines {
		switch i {
		case 0:
			trimmed[i] = strings.TrimRight(l, " \t")
		case len(lines) - 1:
			trimmed[i] = strings.TrimLeft(l, " \t")
		default:
			trimmed[i] = strings.TrimSpace(l)
		}
	}
	return fold(trimmed)
}

// unescape reads the escape after a backslash: the rune it stands for and how many bytes
// of s it took.
func unescape(s string) (rune, int, bool) {
	simple := map[byte]rune{
		'0': 0, 'a': '\a', 'b': '\b', 't': '\t', '\t': '\t', 'n': '\n', 'v': '\v', 'f': '\f', 'r': '\r',
		'e': 0x1b, ' ': ' ', '"': '"', '/': '/', '\\': '\\', 'N': 0x85, '_': 0xa0, 'L': 0x2028, 'P': 0x2029,
	}
	if r, ok := simple[s[0]]; ok {
		return r, 1, true
	}
	digits := map[byte]int{'x': 2, 'u': 4, 'U': 8}[s[0]]
	if digits == 0 || len(s) < 1+digits {
		return 0, 0, false
	}
	n, err := strconv.ParseUint(s[1:1+digits], 16, 32)
	if err != nil || !utf8.ValidRune(rune(n)) {
		return 0, 0, false
	}
	return rune(n), 1 + digits, true
}
