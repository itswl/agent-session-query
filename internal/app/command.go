package app

import (
	"path/filepath"
	"strings"
)

// What a command line runs, read the way a shell would read it — closely enough to tell
// which program a line is about, and whether its exit status is the line's.

// segment is one simple command of a line, and the operator that follows it: "&&",
// "||", ";", "|", "&", or "" at the end.
type segment struct {
	words []string
	then  string
}

// parseLine splits a command line into simple commands. Quotes and backslashes are
// honoured, a newline separates commands as ";" does, and a word is taken as written,
// quotes removed. What a shell does beyond that — expansions, subshells, here-documents
// — is not needed to tell which program runs, and is not done.
func parseLine(line string) []segment {
	var (
		out   []segment
		words []string
		word  strings.Builder
		in    bool // a word has begun, even an empty quoted one
	)
	endWord := func() {
		if in {
			words = append(words, word.String())
			word.Reset()
			in = false
		}
	}
	endSegment := func(op string) {
		endWord()
		if len(words) > 0 || op != "" {
			out = append(out, segment{words: words, then: op})
		}
		words = nil
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line):
			i++
			if line[i] != '\n' { // a backslash before a newline joins the lines
				word.WriteByte(line[i])
				in = true
			}
		case c == '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				j = len(line) - i - 1
			}
			word.WriteString(line[i+1 : i+1+j])
			in = true
			i += j + 1
		case c == '"':
			i++
			for ; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) && strings.IndexByte("\"\\$`\n", line[i+1]) >= 0 {
					i++
				}
				word.WriteByte(line[i])
			}
			in = true
		case c == '$' && i+1 < len(line) && line[i+1] == '(', c == '`':
			// A command substituted is part of the word it is in, operators and all
			j := i + 1
			if c == '`' {
				for j < len(line) && line[j] != '`' {
					j++
				}
			} else {
				for depth := 0; j < len(line); j++ {
					if line[j] == '(' {
						depth++
					} else if line[j] == ')' {
						if depth--; depth == 0 {
							break
						}
					}
				}
			}
			j = min(j, len(line)-1)
			word.WriteString(line[i : j+1])
			in = true
			i = j
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == '\n' || c == ';':
			endSegment(";")
		case c == '&' && i > 0 && (line[i-1] == '>' || line[i-1] == '<'):
			word.WriteByte(c) // 2>&1 is a redirection
			in = true
		case c == '&' || c == '|':
			op := string(c)
			if i+1 < len(line) && line[i+1] == c {
				op += op
				i++
			} else if c == '|' && i+1 < len(line) && line[i+1] == '&' {
				i++ // |& pipes stderr too
			} else if c == '&' && i+1 < len(line) && line[i+1] == '>' {
				word.WriteByte(c) // &> is a redirection
				in = true
				continue
			}
			endSegment(op)
		default:
			word.WriteByte(c)
			in = true
		}
	}
	endSegment("")
	return out
}

// mainCommand is the simple command a line is about, past what only prepares for it — a
// cd, a source of an environment, an export, a set — and whether its exit status is the
// line's: whether nothing after it can hide a failure (a pipe without pipefail, a ";",
// an "||", running it in the background).
func mainCommand(line string) (words []string, decides bool) {
	segs := parseLine(line)
	pipefail := false
	for i, s := range segs {
		if len(s.words) == 0 {
			continue
		}
		if preparing(s.words) && (s.then == "&&" || s.then == ";") {
			if s.words[0] == "set" && strings.Contains(strings.Join(s.words, " "), "pipefail") {
				pipefail = true
			}
			continue
		}
		// Its own status is the line's when what follows it runs only if it succeeded:
		// an "&&", and after that pipes and more "&&"s, which it guards. A pipe straight
		// after it hands the line the status of the last command in the pipe, and an
		// "||", a ";" or an "&" anywhere after it the status of something else.
		decides = s.then == "" || s.then == "&&" || s.then == "|" && pipefail
		for _, rest := range segs[i+1:] {
			if rest.then == "||" || rest.then == ";" || rest.then == "&" {
				decides = false
			}
		}
		return s.words, decides
	}
	return nil, false
}

// preparing reports whether a simple command only sets the scene for the next.
func preparing(words []string) bool {
	switch words[0] {
	case "cd", "pushd", "source", ".", "export", "set", "unset", "alias", "ulimit", "umask":
		return true
	}
	for _, w := range words {
		if !assignment(w) {
			return false
		}
	}
	return true // FOO=1 on a line of its own
}

func assignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for _, r := range w[:eq] {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// commandKey is what a command line asks of which program: the program and its first
// argument when that is a word and not a flag — "go test", "git push", "make",
// "./run.sh" — past what prepares for it, the variables set in front of it, and the
// programs that only run another (sudo, env, time, timeout, nice, nohup). A shell's -c
// is the script it runs; a runner's run is what it runs ("npm run build", "uv run
// pytest", "npx tsc"). Code given inline (python -c, node -e) is no command that can be
// recognised again, and has no key; nor has a line with none.
func commandKey(line string) string {
	words, _ := mainCommand(line)
	return keyOf(words, 0)
}

func keyOf(words []string, depth int) string {
	if depth > 3 {
		return ""
	}
	for len(words) > 0 && assignment(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	prog := words[0]
	if strings.ContainsAny(prog, "$`") {
		return "" // a program named by what an expansion yields is not known here
	}
	if filepath.IsAbs(prog) {
		prog = filepath.Base(prog) // /usr/bin/make is make
	}
	args := words[1:]
	switch prog {
	case "sudo", "doas":
		return keyOf(skipOptions(args, "ugCDhprtT"), depth+1)
	case "env":
		return keyOf(skipOptions(args, "uSCP"), depth+1)
	case "time", "nohup", "command", "exec", "builtin", "caffeinate", "stdbuf":
		return keyOf(skipOptions(args, ""), depth+1)
	case "nice", "ionice":
		return keyOf(skipOptions(args, "nc"), depth+1)
	case "timeout", "gtimeout":
		rest := skipOptions(args, "sk")
		if len(rest) > 0 {
			rest = rest[1:] // the duration
		}
		return keyOf(rest, depth+1)
	case "bash", "sh", "zsh", "dash", "ksh", "fish":
		for i, a := range args {
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
				if i+1 < len(args) {
					inner, _ := mainCommand(args[i+1])
					return keyOf(inner, depth+1)
				}
				return ""
			}
			if !strings.HasPrefix(a, "-") {
				break
			}
		}
	case "python", "python3", "node", "deno", "bun", "ruby", "perl", "php":
		for _, a := range args {
			if a == "-c" || a == "-e" || a == "--eval" || a == "-p" || a == "--print" || a == "eval" {
				return "" // inline code: the next run is other code
			}
			if a == "-m" || !strings.HasPrefix(a, "-") {
				break // a module, or a script and its own arguments
			}
		}
		if prog == "bun" && len(args) > 1 && args[0] == "run" && !strings.HasPrefix(args[1], "-") {
			return "bun run " + args[1]
		}
	case "git":
		args = skipOptions(args, "Cc") // git -C dir push is git push
	case "npx", "bunx", "pnpx":
		if inner := keyOf(skipOptions(args, "p"), depth+1); inner != "" {
			return prog + " " + inner
		}
		return ""
	case "uv", "poetry", "pipenv", "pdm", "rye", "hatch":
		if len(args) > 0 && args[0] == "run" {
			if inner := keyOf(skipOptions(args[1:], ""), depth+1); inner != "" {
				return prog + " run " + inner
			}
			return ""
		}
	case "npm", "pnpm", "yarn":
		if len(args) > 1 && (args[0] == "run" || args[0] == "run-script" || args[0] == "exec" || args[0] == "dlx") && !strings.HasPrefix(args[1], "-") {
			return prog + " " + args[0] + " " + args[1]
		}
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && !strings.ContainsAny(args[0], "<>") {
		return prog + " " + args[0]
	}
	if (prog == "python" || prog == "python3") && len(args) > 1 && args[0] == "-m" {
		return prog + " -m " + args[1]
	}
	return prog
}

// skipOptions passes over a program's options: "-x", "--long", "--long=value", and for
// the single letters in withValue, the word after them too.
func skipOptions(args []string, withValue string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		a := args[0]
		args = args[1:]
		if a == "--" {
			break
		}
		if len(a) == 2 && strings.IndexByte(withValue, a[1]) >= 0 && len(args) > 0 {
			args = args[1:]
		}
	}
	return args
}
