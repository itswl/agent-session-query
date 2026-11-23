package transcript

import (
	"bytes"
	"encoding/json"
	"path"
	"strings"
	"unicode"

	"github.com/itswl/agent-session-query/internal/model"
)

// ToolKind sorts a tool by what it is for. Matching is whole-token, so publish does not
// read as run and runbook does not read as sh.
//
// A name that ends in an agent names what the tool acts on, and it is the agents' work
// whatever it does to one: write_agent messages an agent and read_agent reads what one
// said; they write and read no file.
func ToolKind(name string) string {
	tokens := nameTokens(name)
	if n := len(tokens); n > 0 && agentNouns[tokens[n-1]] {
		return model.ToolAgent
	}
	for _, kind := range kindOrder {
		for _, token := range tokens {
			if kindWords[kind][token] {
				return kind
			}
		}
	}
	return model.ToolOther
}

var kindOrder = []string{model.ToolExec, model.ToolWrite, model.ToolRead, model.ToolSearch, model.ToolNet, model.ToolAgent}

var kindWords = map[string]map[string]bool{
	model.ToolExec:   setOf("bash", "sh", "zsh", "shell", "powershell", "pwsh", "terminal", "exec", "run", "cmd", "command", "process", "container", "docker"),
	model.ToolWrite:  setOf("write", "edit", "patch", "replace", "create", "delete", "remove", "move", "rename", "apply", "save", "notebook"),
	model.ToolRead:   setOf("read", "view", "cat", "open", "show"),
	model.ToolSearch: setOf("grep", "rg", "glob", "search", "find", "list", "scan", "ls"),
	model.ToolNet:    setOf("fetch", "web", "url", "download", "browser", "http", "curl"),
	model.ToolAgent:  setOf("task", "agent", "dispatch", "delegate", "subagent", "workflow"),
}

// agentNouns: the words a tool's name ends in when what it acts on is an agent.
var agentNouns = setOf("agent", "agents", "subagent", "subagents")

func setOf(words ...string) map[string]bool {
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}

// nameTokens splits a tool name into lowercase words: on separators, and on camelCase
// boundaries — NotebookEdit → [notebook edit].
func nameTokens(name string) []string {
	fields := strings.FieldsFunc(name, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == ':' || r == ' ' || (r >= '0' && r <= '9')
	})
	out := []string{}
	for _, field := range fields {
		rs := []rune(field)
		start := 0
		for i := 1; i < len(rs); i++ {
			if unicode.IsLower(rs[i-1]) && unicode.IsUpper(rs[i]) {
				out = append(out, strings.ToLower(string(rs[start:i])))
				start = i
			}
		}
		out = append(out, strings.ToLower(string(rs[start:])))
	}
	return out
}

// inputJSON renders a call's arguments as one JSON value, whatever shape the adapter
// held them in. A string that is itself a JSON object is that object; any other string
// is kept under "raw", so the input is always an object a reader can walk.
func inputJSON(v any) json.RawMessage {
	switch t := v.(type) {
	case nil:
		return nil
	case json.RawMessage:
		if trimmed := bytes.TrimSpace(t); len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed) {
			return trimmed
		}
		if len(bytes.TrimSpace(t)) == 0 {
			return nil
		}
		return rawObject(string(t))
	case []byte:
		return inputJSON(json.RawMessage(t))
	case string:
		if s := strings.TrimSpace(t); strings.HasPrefix(s, "{") && json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return rawObject(t)
	case map[string]any:
		if len(t) == 0 {
			return nil
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if len(raw) > 0 && raw[0] != '{' {
		return rawObject(string(raw))
	}
	return raw
}

func rawObject(s string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"raw": s})
	return raw
}

// fileKeys: the argument keys a tool names a file by, across the CLIs, compared with
// case, underscores and dashes folded away. The first that is present wins.
var fileKeys = []string{"filepath", "path", "notebookpath", "file", "targetfile", "absolutepath"}

// commandKeys: the argument keys a tool carries a command line under.
var commandKeys = []string{"command", "cmd", "script", "commandline"} // CommandLine: Antigravity's run_command

func foldKey(key string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
}

// callFacts reads what a call's arguments say it will touch and run: the files it names
// (one argument, or every file of a patch), and its command line.
func callFacts(kind string, input json.RawMessage) (files []string, command string) {
	if len(input) == 0 {
		return nil, ""
	}
	var args map[string]any
	if json.Unmarshal(input, &args) != nil {
		return nil, ""
	}
	folded := make(map[string]any, len(args))
	for k, v := range args {
		folded[foldKey(k)] = v
	}
	for _, key := range fileKeys {
		if s, ok := folded[key].(string); ok && strings.TrimSpace(s) != "" {
			files = []string{strings.TrimSpace(s)}
			break
		}
	}
	if files == nil {
		// Codex edits with apply_patch, whose one argument is the patch text
		for _, key := range []string{"input", "patch", "raw"} {
			if text, ok := args[key].(string); ok && strings.Contains(text, "*** Begin Patch") {
				files = patchFiles(text)
				break
			}
		}
	}
	if files == nil {
		if text, ok := args["diff"].(string); ok {
			files = diffFiles(text)
		}
	}
	keys := commandKeys
	if kind == model.ToolExec {
		keys = append(append([]string{}, commandKeys...), "input", "raw")
	}
	for _, key := range keys {
		switch v := folded[key].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return files, v
			}
		case []any: // the argv Codex's shell takes
			parts := make([]string, 0, len(v))
			for _, part := range v {
				if s, ok := part.(string); ok {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return files, strings.Join(parts, " ")
			}
		}
	}
	return files, ""
}

// patchFiles are the files a patch in Codex's apply_patch format names.
func patchFiles(text string) []string {
	var files []string
	for line := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: ", "*** Move to: "} {
			if rest, ok := strings.CutPrefix(line, prefix); ok && strings.TrimSpace(rest) != "" {
				files = appendUnique(files, strings.TrimSpace(rest))
			}
		}
	}
	return files
}

// diffFiles are the files a unified diff names on its +++ lines (the --- side when a
// file was deleted).
func diffFiles(text string) []string {
	var files []string
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		rest, ok := strings.CutPrefix(line, "+++ ")
		if !ok {
			continue
		}
		name := strings.TrimSpace(rest)
		if name == "/dev/null" && i > 0 {
			name = strings.TrimSpace(strings.TrimPrefix(lines[i-1], "--- "))
		}
		name = strings.TrimPrefix(strings.TrimPrefix(name, "b/"), "a/")
		if name != "" && name != "/dev/null" {
			files = appendUnique(files, name)
		}
	}
	return files
}

// within names a file a call named relative to dir, the directory the call ran in, by its
// whole path. A file named whole, or from the home directory (~/notes), is named as it
// was; so is any file when dir is not a whole path itself. The path is written the way
// dir is — a Unix one with /, a Windows one with \ — whichever system reads it: dir is
// the CLI's record of where it ran, which may be another machine's.
func within(dir, file string) string {
	if dir == "" || file == "" || rooted(file) || strings.HasPrefix(file, "~") {
		return file
	}
	switch {
	case strings.HasPrefix(dir, "/"):
		return path.Join(dir, file)
	case rooted(dir):
		slashed := path.Join(strings.ReplaceAll(dir, "\\", "/"), strings.ReplaceAll(file, "\\", "/"))
		if strings.HasPrefix(dir, `\\`) {
			slashed = "/" + slashed // \\server\share: Join keeps one of its two slashes
		}
		return strings.ReplaceAll(slashed, "/", "\\")
	}
	return file
}

// rooted reports whether p is a whole path on some system: /work/api, C:\work\api, C:/work,
// \\server\share.
func rooted(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		('a' <= p[0]|0x20 && p[0]|0x20 <= 'z')
}

func appendUnique(list []string, s string) []string {
	for _, item := range list {
		if item == s {
			return list
		}
	}
	return append(list, s)
}

// File kinds: what a change was made of, by path alone. Convention, not content — the
// rules are the mainstream layouts, and a miss shows a wrong word, nothing more.
const (
	FileCode   = "code"
	FileTest   = "test"
	FileDocs   = "docs"
	FileConfig = "config"
)

// FileKind classifies one path: tests first (a *_test.go is a test, not code), then
// docs, then configuration, and everything else is code.
func FileKind(p string) string {
	p = strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
	base := path.Base(p)
	segments := strings.Split(p, "/")
	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasSuffix(base, "_test.py"),
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.Contains(base, ".test."),
		strings.Contains(base, ".spec."),
		hasSegment(segments, "test", "tests", "__tests__", "spec", "specs"):
		return FileTest
	case strings.HasSuffix(base, ".md") || strings.HasSuffix(base, ".mdx") || strings.HasSuffix(base, ".rst"),
		hasSegment(segments, "docs", "doc", "documentation"),
		strings.HasPrefix(base, "readme"), strings.HasPrefix(base, "changelog"),
		strings.HasPrefix(base, "license"), strings.HasPrefix(base, "contributing"):
		return FileDocs
	case strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml") ||
		strings.HasSuffix(base, ".toml") || strings.HasSuffix(base, ".ini") || strings.HasSuffix(base, ".cfg") ||
		strings.HasSuffix(base, ".conf") || strings.HasSuffix(base, ".lock") ||
		base == "go.mod" || base == "go.sum" || base == "go.work" || base == "dockerfile" ||
		base == ".gitignore" || base == ".dockerignore" ||
		strings.HasPrefix(base, ".") && strings.HasSuffix(base, "rc"):
		return FileConfig
	}
	return FileCode
}

func hasSegment(segments []string, names ...string) bool {
	for _, segment := range segments[:len(segments)-1] { // the base is the file
		for _, name := range names {
			if segment == name {
				return true
			}
		}
	}
	return false
}
