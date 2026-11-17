package app

import (
	"regexp"
	"slices"
	"strings"
)

// What a command line runs, read the way a shell would read it — closely enough to tell
// which program a line is about, and whether its exit status is that program's.

// segment is one simple command of a line, and the operator that follows it: "&&",
// "||", ";", "|", "&", or "" at the end.
type segment struct {
	words []string
	then  string
}

// parseLine splits a command line into simple commands. Quotes and backslashes are
// honoured; a newline separates commands as ";" does; a "#" that begins a word begins a
// comment; the parentheses of a
// subshell group what they hold and are otherwise passed over, as are { and }; and a
// command substituted — $(…), `…` — is part of the word it is in. A word is taken as
// written, quotes removed. What a shell does beyond that — expansions, here-documents'
// bodies — is not needed to tell which program runs, and is not done.
func parseLine(line string) []segment {
	var (
		out   []segment
		words []string
		word  strings.Builder
		in    bool // a word has begun, even an empty quoted one
	)
	endWord := func() {
		if in {
			if w := word.String(); w != "{" && w != "}" {
				words = append(words, w)
			}
			word.Reset()
			in = false
		}
	}
	endSegment := func(op string) {
		endWord()
		if len(words) > 0 {
			out = append(out, segment{words: words, then: op})
		} else if op == "&" && len(out) > 0 {
			out[len(out)-1].then = "&" // "( … ) &": the group runs in the background
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
		case c == '#' && !in:
			for i+1 < len(line) && line[i+1] != '\n' {
				i++ // a comment, to the end of the line
			}
		case c == '(' && !in, c == ')':
			endWord() // a subshell's bounds: what is inside is read as it is
			if c == '(' && len(words) == 1 && words[0] == "time" {
				words = nil // time ( … ) times the group, which is what is run
			}
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == '\n' || c == ';':
			endSegment(";") // a segment of no words is none: a newline after "&&" goes on with the list
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
	if n := len(out); n > 0 && out[n-1].then == ";" {
		out[n-1].then = "" // a ";" with nothing after it ends the line and hides nothing
	}
	return out
}

// mainCommand is the simple command a line is about, past what only prepares for it — a
// cd, a source of an environment, an export, a set — and whether its exit status is the
// line's: whether nothing after it can hide a failure (a pipe without pipefail, a ";",
// an "||", running it in the background). A line that begins with a shell's own syntax —
// if, for, while, case, a function, a "!" — is about no one command.
func mainCommand(line string) (words []string, decides bool) {
	segs := parseLine(line)
	pipefail := false
	for i, s := range segs {
		if preparing(s.words) && (s.then == "&&" || s.then == ";") {
			if s.words[0] == "set" && strings.Contains(strings.Join(s.words, " "), "pipefail") {
				pipefail = true
			}
			continue
		}
		if reserved[s.words[0]] {
			return nil, false
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

var reserved = map[string]bool{"if": true, "then": true, "elif": true, "else": true, "fi": true, "for": true,
	"while": true, "until": true, "do": true, "done": true, "case": true, "esac": true, "function": true,
	"select": true, "!": true, "[[": true}

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

// shellArgv is a line that runs a shell's -c on a script given as words of its own rather
// than as one quoted word: how Codex's argv commands, ["bash", "-lc", "cd /p && make"],
// are kept, joined by spaces. The script is all the rest of the line.
//
// What runs the shell may come first — timeout 60, sudo -n, env FOO=1 — and the script
// may run over many lines: a here-document, a preamble of set -euo pipefail.
var shellArgv = regexp.MustCompile(`(?s)^\s*` +
	`(?:(?:sudo|doas|env|time|nohup|nice|timeout|gtimeout)(?:\s+(?:-[^\s'"]+|\d+[smhd]?|[A-Za-z_][A-Za-z0-9_]*=[^\s'"]*))*\s+)*` +
	`(?:\S*/)?(?:bash|sh|zsh|dash|ksh|fish)\s+(?:-[A-Za-z-]+\s+)*-[A-Za-z]*c[A-Za-z]*\s+([^\s'"].*)$`)

// resolve is the simple command a line runs, read through everything that only runs
// another — the programs that wrap one (sudo, env, time, timeout, nice, nohup) and a
// shell's -c, whose script is read in turn — and whether the line's exit status is that
// command's, at every level it was read through.
func resolve(line string, depth int) (words []string, decides bool) {
	if depth > 4 {
		return nil, false
	}
	if m := shellArgv.FindStringSubmatch(line); m != nil {
		return resolve(m[1], depth+1)
	}
	words, decides = mainCommand(line)
	for {
		for len(words) > 0 && assignment(words[0]) {
			words = words[1:]
		}
		if len(words) == 0 {
			return nil, false
		}
		prog, args := progName(words[0]), words[1:]
		switch prog {
		case "sudo", "doas":
			words = skipOptions(args, "ugCDhprtT")
		case "env":
			words = skipOptions(args, "uSCP")
		case "time", "nohup", "command", "exec", "builtin", "caffeinate", "stdbuf":
			words = skipOptions(args, "")
		case "nice", "ionice":
			words = skipOptions(args, "nc")
		case "timeout", "gtimeout":
			words = skipOptions(args, "sk", "--signal", "--kill-after")
			if len(words) > 0 {
				words = words[1:] // the duration
			}
		case "bash", "sh", "zsh", "dash", "ksh", "fish":
			// Options as a shell reads them: letters in clusters, an o or O in one taking
			// the next word (-euo pipefail), c saying that the first word after the
			// options is the script to run. Without a c, that word is a script file.
			script, i := false, 0
		options:
			for ; i < len(args); i++ {
				a := args[i]
				switch {
				case a == "--" || a == "-":
					i++
					break options
				case a == "--command":
					script = true
				case a == "--rcfile" || a == "--init-file" || a == "--init-command":
					i++
				case strings.HasPrefix(a, "--"):
					// --login, --noprofile, --norc
				case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
					for _, r := range a[1:] {
						switch r {
						case 'c':
							script = true
						case 'o', 'O':
							i++
						}
					}
				default:
					break options
				}
			}
			if !script {
				return words, decides
			}
			if i >= len(args) {
				return nil, false
			}
			inner, ok := resolve(args[i], depth+1)
			return inner, decides && ok
		default:
			return words, decides
		}
	}
}

// commandKey is what a command line asks of which program: the program and its first
// argument when that is a word and not a flag — "go test", "git push", "make",
// "./run.sh" — of the command the line resolves to (see resolve), past the options that
// come before a subcommand. A runner's run is what it runs ("npm run build", "uv run
// pytest", "npx tsc"). Code given inline — python -c, node -e, a script fed in by a
// here-document — is no command that can be recognised again, and has no key; nor has a
// line with none.
func commandKey(line string) string {
	words, _ := resolve(line, 0)
	return keyOf(words, 0)
}

func keyOf(words []string, depth int) string {
	if depth > 3 || len(words) == 0 {
		return ""
	}
	prog := words[0]
	if strings.ContainsAny(prog, "$`") {
		return "" // a program named by what an expansion yields is not known here
	}
	for _, w := range words[1:] {
		if strings.HasPrefix(w, "<<") {
			return "" // what it reads is a here-document: inline
		}
	}
	if pathed(prog) {
		prog = progName(prog) // /usr/bin/make is make
	}
	args := words[1:]
	switch prog {
	case "python", "python3", "node", "deno", "bun", "ruby", "perl", "php":
		for _, a := range args {
			if a == "-c" || a == "-e" || a == "--eval" || a == "-p" || a == "--print" || a == "eval" || a == "-" {
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
		args = skipOptions(args, "Cc", "--git-dir", "--work-tree", "--namespace", "--exec-path", "--config-env")
	case "make", "gmake":
		args = makeOptions(args)
	case "cargo":
		args = skipOptions(args, "Z", "--manifest-path", "--config", "--color")
	case "npx", "bunx", "pnpx":
		if inner := keyOf(skipOptions(args, "pc", "--package", "--call"), depth+1); inner != "" {
			return prog + " " + inner
		}
		return ""
	case "uv", "poetry", "pipenv", "pdm", "rye", "hatch":
		args = skipOptions(args, "", "--directory", "--project", "--config-file")
		if len(args) > 0 && args[0] == "run" {
			if inner := keyOf(skipOptions(args[1:], "p", "--with", "--python", "--project", "--directory", "--group", "--extra", "--env-file", "--package"), depth+1); inner != "" {
				return prog + " run " + inner
			}
			return ""
		}
	case "npm", "pnpm", "yarn":
		switch prog {
		case "npm":
			args = skipOptions(args, "Cw", "--prefix", "--workspace")
		case "pnpm":
			args = skipOptions(args, "CF", "--dir", "--filter") // pnpm's -w is a flag: the workspace root
		default:
			args = skipOptions(args, "", "--cwd")
		}
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

// pathed reports whether a program is named by an absolute path — /usr/bin/make,
// C:\tools\make.exe — whatever machine this is: a command line is the machine's it ran on.
func pathed(prog string) bool {
	return strings.HasPrefix(prog, "/") || strings.HasPrefix(prog, `\`) ||
		len(prog) > 2 && prog[1] == ':' && (prog[2] == '\\' || prog[2] == '/')
}

// progName is a program's name, past the directories of a path to it, either way they
// are written, and a Windows .exe.
func progName(prog string) string {
	name := prog[strings.LastIndexAny(prog, `/\`)+1:]
	if len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".exe") {
		name = name[:len(name)-4]
	}
	return name
}

// makeOptions passes over make's options. -j and -l (--jobs, --load-average) take a number
// when one follows — or what will be one, $(nproc) — and are flags otherwise.
func makeOptions(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		a := args[0]
		args = args[1:]
		switch a {
		case "--":
			return args
		case "-C", "-f", "-I", "-W", "-o", "--directory", "--file", "--makefile", "--include-dir", "--what-if", "--new-file", "--assume-new", "--old-file", "--assume-old":
			if len(args) > 0 {
				args = args[1:]
			}
		case "-j", "-l", "--jobs", "--load-average", "--max-load":
			if len(args) > 0 && (strings.Trim(args[0], "0123456789.") == "" || strings.ContainsAny(args[0], "$`")) {
				args = args[1:]
			}
		}
	}
	return args
}

// skipOptions passes over a program's options: "-x", "--long", "--long=value", and the
// word after an option that takes one — the single letters in withValue, and the long
// options listed after it.
func skipOptions(args []string, withValue string, long ...string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		a := args[0]
		args = args[1:]
		if a == "--" {
			break
		}
		takes := len(a) == 2 && strings.IndexByte(withValue, a[1]) >= 0 || slices.Contains(long, a)
		if takes && len(args) > 0 {
			args = args[1:]
		}
	}
	return args
}
