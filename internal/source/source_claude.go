package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Claude Code: ~/.claude/projects/<project>/<session-uuid>.jsonl, and beside each session a
// directory of the same name, where its subagents' transcripts are kept
// (<session-uuid>/subagents/agent-<id>.jsonl, with an agent-<id>.meta.json describing each;
// a workflow's agents one level further down, in subagents/workflows/<run>/)
type ClaudeCodeSource struct {
	root  string
	cache *fileRecordCache
}

func NewClaudeSource(root string) *ClaudeCodeSource {
	return &ClaudeCodeSource{root: root, cache: newFileRecordCache(claudeCountMessages)}
}

// claudeProjectsDir follows Claude Code's own resolution: CLAUDE_CONFIG_DIR moves the
// whole configuration directory off ~/.claude
func claudeProjectsDir(home string) string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

func (s *ClaudeCodeSource) Mode() string     { return "claude" }
func (s *ClaudeCodeSource) Location() string { return s.root }

func (s *ClaudeCodeSource) Exists() bool { return fileExists(s.root) }

func (s *ClaudeCodeSource) files() []string {
	files := []string{}
	for _, pattern := range []string{
		filepath.Join(s.root, "*", "*.jsonl"),
		filepath.Join(s.root, "*", "*", "subagents", "agent-*.jsonl"),
		filepath.Join(s.root, "*", "*", "subagents", "workflows", "*", "agent-*.jsonl"),
	} {
		matches, err := filepath.Glob(pattern)
		if err == nil {
			files = append(files, matches...)
		}
	}
	return files
}

// claudeSubagentPath reports whether a transcript is a subagent's: agent-<id>.jsonl in a
// session's subagents directory. Every row of one is marked isSidechain, which in a session's
// own transcript means "not this thread" and is skipped; in a subagent's it is the thread.
func claudeSubagentPath(path string) bool {
	return strings.HasPrefix(filepath.Base(path), "agent-") &&
		strings.Contains(filepath.ToSlash(path), "/subagents/")
}

// claudeHeadLines caps how far down the file the metadata scan will go.
//
// cwd is not on the first line — sessions open with non-conversation rows such as
// queue-operation. The original code read a fixed first 5 lines; measured locally, cwd
// lands on lines 2-5 with a good number sitting exactly on line 5. That is luck, not a
// guarantee: one more preamble row from Claude Code and cwd slides out of the window,
// after which cwd / project silently go empty and project grouping breaks, without any
// error. So the scan now runs until it has what it needs, and this cap is only a
// defensive backstop.
//
// It is usually faster too: stopping as soon as both fields are in place ends most files
// at line 3, fewer than the fixed 5 it used to read.
const claudeHeadLines = 50

// List reads only the head of each session file for metadata; unchanged files come
// straight from the cache (see fileRecordCache)
func (s *ClaudeCodeSource) List() []Record {
	return claudeLinkSubagents(s.cache.records(s.files(), func(path, modISO string) Record {
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		// Use the time on the last record in the file, not the file's mtime (see updatedAtOf)
		updated := updatedAtOf(path, modISO)
		head := claudeHeadOf(path)
		if claudeSubagentPath(path) {
			return claudeSubagentRecord(path, stem, head, updated)
		}
		name := FirstNonEmpty(claudeUserTitle(path), stem)
		return NewRecord(Record{
			Source:    "claude",
			Key:       path,
			ShortKey:  name,
			SessionID: FirstNonEmpty(head.sid, stem),
			File:      path,
			HasFile:   true,
			Status:    "done",
			Cwd:       head.cwd,
			Branch:    head.branch,
			UpdatedAt: updated,
		}, updated)
	}))
}

// claudeHead is what a transcript's first rows say about it
type claudeHead struct {
	sid, cwd, branch string
	firstAt          string // the first row's time: when a subagent was started
	agentID          string
}

// claudeHeadOf decodes only the fields it reads: the first row of a subagent's transcript
// is its prompt, often pages long, and decoding it whole into a map was the larger part of
// listing one
func claudeHeadOf(path string) claudeHead {
	var h claudeHead
	var haveSID, haveCWD, haveBranch bool
	seen := 0
	EachJSONLLine(path, func(line []byte) bool {
		seen++
		var row struct {
			SessionID any `json:"sessionId"`
			Cwd       any `json:"cwd"`
			GitBranch any `json:"gitBranch"`
			Timestamp any `json:"timestamp"`
			AgentID   any `json:"agentId"`
		}
		if json.Unmarshal(line, &row) != nil {
			return seen < claudeHeadLines
		}
		if h.firstAt == "" {
			h.firstAt = ToStr(row.Timestamp)
		}
		if h.agentID == "" {
			h.agentID = ToStr(row.AgentID)
		}
		if !haveSID && Truthy(row.SessionID) {
			h.sid, haveSID = ToStr(row.SessionID), true
		}
		if !haveCWD && Truthy(row.Cwd) {
			h.cwd, haveCWD = ToStr(row.Cwd), true
		}
		// The branch the session opened on. Claude Code writes gitBranch on nearly every
		// row, so the head scan already passes several; a session started outside a
		// repository has none, and that is the one case the scan runs to the cap, which is
		// why the cap is there.
		if !haveBranch && Truthy(row.GitBranch) {
			h.branch, haveBranch = ToStr(row.GitBranch), true
		}
		return !(haveSID && haveCWD && haveBranch) && seen < claudeHeadLines
	})
	return h
}

// claudeSubagentRecord lists a subagent's transcript as a session of its own, tied to the
// session it ran in. Its rows carry the parent's sessionId; its own id is the agent id, which
// is also how the parent's records name it. The meta file beside it says what kind of agent
// it was, what it was asked to do (the title — the prompt itself is often pages long), which
// agent spawned it, and the call that did.
//
// Measured locally: 291 subagent transcripts in 18 sessions, 528 MB between them — more than
// the sessions' own transcripts — none of it listed or searched until now.
func claudeSubagentRecord(path, stem string, head claudeHead, updated string) Record {
	meta := map[string]any{}
	if data, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json"); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	// The session directory is the parent of subagents/, however deep the transcript sits
	parent := head.sid
	if parent == "" {
		for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if filepath.Base(dir) == "subagents" {
				parent = filepath.Base(filepath.Dir(dir))
				break
			}
		}
	}
	// The prompt is read for a title only when the meta file has none: it is a second pass
	// over the head of a file whose first row can be pages long
	title := FirstNonEmpty(TitleFromUserText(ToStr(meta["description"])), TitleFromUserText(ToStr(meta["name"])))
	if title == "" {
		title = FirstNonEmpty(claudeUserTitle(path), stem)
	}
	return NewRecord(Record{
		Source:          "claude",
		Key:             path,
		ShortKey:        title,
		SessionID:       FirstNonEmpty(head.agentID, strings.TrimPrefix(stem, "agent-")),
		File:            path,
		HasFile:         true,
		Status:          "done",
		Cwd:             head.cwd,
		Branch:          head.branch,
		UpdatedAt:       updated,
		CreatedAt:       head.firstAt,
		ParentSessionID: parent,
		ParentAgentID:   ToStr(meta["parentAgentId"]),
		AgentType:       ToStr(meta["agentType"]),
		ParentToolUseID: ToStr(meta["toolUseId"]),
		AgentDepth:      1,
	}, updated)
}

// claudeLinkSubagents fills in what no single transcript knows: how many subagents ran in
// each session, and how deep each subagent sits. Depth follows the parentAgentId links
// rather than the spawnDepth the meta file records, which starts from 0 for a team's
// members and from 1 for an agent the main thread spawned with a tool call.
func claudeLinkSubagents(records []Record) []Record {
	agents := map[string]int{}   // parent session + agent id → index
	children := map[string]int{} // parent session → subagents
	for i, r := range records {
		if r.IsSubagent() {
			agents[r.ParentSessionID+"\x00"+r.SessionID] = i
			children[r.ParentSessionID]++
		}
	}
	for i := range records {
		r := &records[i]
		if !r.IsSubagent() {
			r.Subagents = children[r.SessionID]
			continue
		}
		depth, at := 1, r
		for hops := 0; at.ParentAgentID != "" && hops < len(records); hops++ {
			j, ok := agents[at.ParentSessionID+"\x00"+at.ParentAgentID]
			if !ok {
				break
			}
			depth, at = depth+1, &records[j]
		}
		r.AgentDepth = depth
	}
	return records
}

// claudeUserTitle is the first real user message of a Claude Code session, as a title.
// A user row's content is a string or a block array; text blocks concatenate. Command
// plumbing and caveat rows open with a tag and are rejected inside TitleFromUserText.
func claudeUserTitle(path string) string {
	return firstUserTitle(path, claudeHeadLines, func(obj map[string]any) (string, bool) {
		if obj["type"] != "user" {
			return "", false
		}
		msg, _ := obj["message"].(map[string]any)
		if msg == nil {
			return "", false
		}
		switch content := msg["content"].(type) {
		case string:
			return content, true
		case []any:
			texts := []string{}
			for _, item := range content {
				if m, ok := item.(map[string]any); ok && m["type"] == "text" {
					if t := strField(m, "text"); t != "" {
						texts = append(texts, t)
					}
				}
			}
			return strings.Join(texts, "\n"), len(texts) > 0
		}
		return "", false
	})
}

// claudeInterruptedPrefix opens the text Claude Code writes when the user stops a turn:
// "[Request interrupted by user]" as a user text block, and "[Request interrupted by user
// for tool use]" as the content of the tool_result the stopped call never produced.
const claudeInterruptedPrefix = "[Request interrupted by user"

// claudeRow folds one user or assistant row into the shared block array shape.
//
// names carries tool_use id → tool name across the scan: a tool_result row names only the
// call it answers (tool_use_id), and the name that lets a reader see "the Bash call failed"
// rather than "a tool failed" lives on the assistant row before it. root is the source's
// projects directory, where the outputs Claude Code saved beside the transcript are looked
// for (see claudePersistedPath).
type claudeRow struct {
	names map[string]string
	full  bool
	root  string
}

func (c claudeRow) parts(obj map[string]any) []map[string]any {
	msg := getMap(obj, "message")
	parts := []map[string]any{}
	switch content := msg["content"].(type) {
	case string:
		if content != "" {
			parts = append(parts, c.textOrEvent(content))
		}
		return parts
	case []any:
		for _, item := range content {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				if text := strField(m, "text"); text != "" {
					parts = append(parts, c.textOrEvent(text))
				}
			case "thinking":
				parts = append(parts, ThinkingBlock(strField(m, "thinking"), c.full))
			case "tool_use":
				id, name := strField(m, "id"), strField(m, "name")
				if id != "" && c.names != nil {
					c.names[id] = name
				}
				parts = append(parts, ToolCallBlock(id, name, GetOr(m, "input", map[string]any{})))
			case "tool_result":
				parts = append(parts, c.toolResult(obj, m))
			}
		}
	}
	return parts
}

// textOrEvent: the interruption notice is not something the user said, it is something
// that happened, so it becomes an event rather than a user text block (and so does not
// start a round)
func (c claudeRow) textOrEvent(text string) map[string]any {
	if strings.HasPrefix(strings.TrimSpace(text), claudeInterruptedPrefix) {
		return EventBlock(EventInterrupted, strings.TrimSpace(text))
	}
	return TextBlock(text)
}

// toolResult pairs the result with its call and records how the call ended. is_error is
// Claude's own verdict; the row-level toolUseResult says when the user interrupted the
// command, and so does the placeholder text a stopped call gets as its result.
//
// An output too large for the transcript is saved to a file beside it, and the transcript
// keeps a stub: a line naming the file and a 2 KB preview. A full read returns the file's
// content in the stub's place — the end of a build log is exactly what the preview drops.
// An ordinary read keeps the stub and marks it truncated however short it is, because the
// output is not what it holds.
func (c claudeRow) toolResult(obj, m map[string]any) map[string]any {
	callID := strField(m, "tool_use_id")
	text := ContentText(m["content"])
	content := text
	saved := claudePersistedPath(c.root, obj, text)
	if saved != "" && c.full {
		if whole, ok := readPersistedOutput(saved); ok {
			content = whole
		}
	}
	block := ToolResultBlock(callID, c.names[callID], content, c.full)
	if saved != "" && !c.full {
		block["truncated"] = true
	}
	outcome := ToolOutcome{Status: statusFromError(Truthy(m["is_error"]))}
	if Truthy(getMap(obj, "toolUseResult")["interrupted"]) || strings.HasPrefix(strings.TrimSpace(text), claudeInterruptedPrefix) {
		outcome.Status = StatusInterrupted
	}
	return outcome.Apply(block)
}

// claudeStubPath is the path a stub names on its first line, or "" when the result is not
// a stub. Claude Code writes two: a command's output inside <persisted-output>, "Output too
// large (30.5KB). Full output saved to: <path>", and an MCP tool's result over the token
// limit, "Error: result (56,846 characters) exceeds maximum allowed tokens. Output has been
// saved to <path>." Both are recognised by how they open, not by a path somewhere in the
// text: a command whose output merely mentions a saved file is not a stub.
func claudeStubPath(text string) string {
	body := text
	if rest, ok := strings.CutPrefix(text, "<persisted-output>"); ok {
		body = strings.TrimLeft(rest, "\r\n")
	} else if !strings.HasPrefix(text, "Error: result (") {
		return ""
	}
	first, _, _ := strings.Cut(body, "\n")
	_, path, ok := strings.Cut(first, "saved to")
	if !ok {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(path, ":"))
}

// claudeSavedOutput takes the parts of a saved output's path from the project directory on,
// which is all claudePersistedPath uses
var claudeSavedOutput = regexp.MustCompile(`([^/\s]+)/([^/\s]+)/tool-results/([^/\s]+)$`)

// claudePersistedPath is where a stub's output is on this machine, or "" when the result
// is not a stub or its file is not there.
//
// Only the last parts of the recorded path are used — project, session, file — and they
// are looked up under this source's own root. The recorded path is whatever the transcript
// says: for a projects directory copied or mounted from another machine it names a path
// there, and in a doctored transcript it could name any file on this one. Measured
// locally: 77 stubs in transcripts and 155 in subagent transcripts, every one of them under
// <project>/<session>/tool-results/, the session being the one the transcript belongs to
// or, for a subagent, its parent.
func claudePersistedPath(root string, obj map[string]any, text string) string {
	if root == "" {
		return ""
	}
	recorded := claudeStubPath(text)
	if recorded == "" && strings.HasPrefix(text, "<persisted-output>") {
		// The command's stub says the same on its row, which is the fallback when the
		// stub's own wording has moved
		recorded = ToStr(getMap(obj, "toolUseResult")["persistedOutputPath"])
	}
	if recorded == "" {
		return ""
	}
	m := claudeSavedOutput.FindStringSubmatch(strings.ReplaceAll(recorded, "\\", "/"))
	if m == nil {
		return ""
	}
	project, session, file := m[1], m[2], strings.TrimSuffix(m[3], ".")
	for _, part := range []string{project, session, file} {
		if part == "." || part == ".." || part == "" {
			return ""
		}
	}
	path := filepath.Join(root, project, session, "tool-results", file)
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

// persistedOutputLimit caps what a full read returns of one saved output, and
// persistedOutputHead is how much of it comes from the start; the rest is the end, where a
// build log says how it failed. Measured locally: 144 saved outputs, median 39 KB, nine in
// ten under 130 KB — and one of 52 MB, which is what the cap is for.
var (
	persistedOutputLimit int64 = 8 << 20
	persistedOutputHead  int64 = 1 << 20
)

// readPersistedOutput reads a saved output whole, or its start and its end with a line
// between them saying how much of the middle was left out
func readPersistedOutput(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	size := info.Size()
	if size <= persistedOutputLimit {
		data, err := io.ReadAll(io.LimitReader(f, persistedOutputLimit))
		if err != nil {
			return "", false
		}
		return string(data), true
	}
	head := make([]byte, persistedOutputHead)
	if _, err := io.ReadFull(f, head); err != nil {
		return "", false
	}
	tail := make([]byte, persistedOutputLimit-persistedOutputHead)
	if _, err := f.ReadAt(tail, size-int64(len(tail))); err != nil {
		return "", false
	}
	// A cut can land inside a character; the bytes of half a character are dropped
	gap := size - int64(len(head)) - int64(len(tail))
	return strings.ToValidUTF8(string(head), "") +
		fmt.Sprintf("\n\n… %d bytes of the middle left out; the whole output is %s …\n\n", gap, filepath.Base(path)) +
		strings.ToValidUTF8(string(tail), ""), true
}

// claudeSystemEvent turns the system rows worth a reader's attention into event blocks:
// a compaction boundary (the context the model sees was rewritten here) and a stop hook
// that failed. The rest — turn_duration, informational, local_command — is bookkeeping
// and yields nothing. Measured locally over 60 sessions: 562 system rows, of which 12
// compactions and 289 hook summaries, most of the latter with no errors.
func claudeSystemEvent(obj map[string]any) map[string]any {
	switch obj["subtype"] {
	case "compact_boundary":
		return EventBlock(EventCompaction, StrOr(obj["content"], "Context compacted"))
	case "stop_hook_summary":
		errs := []string{}
		for _, raw := range getSlice(obj, "hookErrors") {
			if text := ContentText(raw); text != "" {
				errs = append(errs, text)
			}
		}
		if len(errs) == 0 {
			return nil
		}
		return EventBlock(EventHookError, strings.Join(errs, "\n"))
	}
	return nil
}

// claudeProbe is the small decode every whole-file scan does per row: enough to decide
// whether the row counts as a message and to pick up usage, nothing more. counts is the
// one rule Messages, Final and the background counter all follow, so the number in the
// list is the number of rows the reader will get.
type claudeProbe struct {
	Type        string            `json:"type"`
	Subtype     string            `json:"subtype"`
	IsSidechain bool              `json:"isSidechain"`
	HookErrors  []json.RawMessage `json:"hookErrors"`
	Message     struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	Attachment struct {
		Type        string `json:"type"`
		CommandMode string `json:"commandMode"`
		IsMeta      bool   `json:"isMeta"`
		Prompt      any    `json:"prompt"`
	} `json:"attachment"`
}

func (p claudeProbe) counts(subagent bool) bool {
	if p.IsSidechain && !subagent {
		return false
	}
	switch p.Type {
	case "user", "assistant":
		return true
	case "system":
		return p.Subtype == "compact_boundary" || (p.Subtype == "stop_hook_summary" && len(p.HookErrors) > 0)
	case "attachment":
		a := p.Attachment
		return a.Type == "queued_command" && a.CommandMode == "prompt" && !a.IsMeta &&
			strings.TrimSpace(ContentText(a.Prompt)) != ""
	}
	return false
}

// Search scans the rows the reader draws, not the whole file. A Claude Code transcript
// carries more bookkeeping than conversation: measured locally, 94 547 attachment rows
// (hook output alone was 203 MB of them) against 106 318 user and assistant rows, beside
// title, mode, snapshot and queue rows. None of them is drawn, and they matched first:
// across eight common words, the first hit in 332 of 400 results was an attachment, so the
// list showed injected context where the conversation should have been, and the
// per_session slots were spent on it.
func (s *ClaudeCodeSource) Search(ctx context.Context, r Record, q SearchQuery) []map[string]any {
	keep := claudeSearchable
	if r.IsSubagent() {
		keep = claudeRowSearchable
	}
	return searchFileWith(ctx, r.File, q, lineRules{stamp: rowTime, keep: keep})
}

// claudeSearchable is the row rule Messages follows — the main thread's user and assistant
// rows, and the system rows it draws as events — with one addition: what the user typed
// while the agent was working. It reaches the model as a queued-command attachment, and for
// some versions of Claude Code that row is the only record of it; dropped here, the user's
// own words would not be found.
func claudeSearchable(obj map[string]any) bool {
	if Truthy(obj["isSidechain"]) {
		return false
	}
	return claudeRowSearchable(obj)
}

// claudeRowSearchable is claudeSearchable without the thread check, for a subagent's
// transcript, where every row is the sidechain
func claudeRowSearchable(obj map[string]any) bool {
	switch obj["type"] {
	case "user", "assistant":
		return true
	case "system":
		return claudeSystemEvent(obj) != nil
	case "attachment":
		return claudeQueuedPrompt(obj)
	}
	return false
}

// claudeQueuedPrompt: an attachment carrying a prompt the user typed while the agent worked.
// The same attachment type also carries background-task notifications (commandMode
// task-notification) and prompts the CLI wrote itself (isMeta); neither is the user's.
func claudeQueuedPrompt(obj map[string]any) bool {
	a := getMap(obj, "attachment")
	return a["type"] == "queued_command" && a["commandMode"] == "prompt" && !Truthy(a["isMeta"])
}

// claudeQueuedText is the words of a queued prompt: a string, or blocks when an image was
// pasted with it
func claudeQueuedText(obj map[string]any) string {
	return strings.TrimSpace(ContentText(getMap(obj, "attachment")["prompt"]))
}

// claudeHitRole names who spoke in a queued prompt: the user, not "attachment"
func claudeHitRole(obj map[string]any) string {
	if obj["type"] == "attachment" && claudeQueuedPrompt(obj) {
		return "user"
	}
	return ""
}

func (s *ClaudeCodeSource) Messages(r Record, q MessageQuery) []map[string]any {
	sink := newMessageSink(q)
	path := r.File
	if path == "" {
		return sink.result()
	}
	row := claudeRow{names: map[string]string{}, full: q.Full, root: s.root}
	subagent := r.IsSubagent()
	eachJSONL(path, func(obj map[string]any) bool {
		// A subagent's rows are not part of the main thread — except in its own transcript
		if Truthy(obj["isSidechain"]) && !subagent {
			return true
		}
		switch obj["type"] {
		case "system":
			event := claudeSystemEvent(obj)
			if event == nil {
				return true
			}
			return sink.add(map[string]any{
				"id":        GetOr(obj, "uuid", ""),
				"role":      "system",
				"timestamp": GetOr(obj, "timestamp", ""),
				"content":   []map[string]any{event},
			})
		case "attachment":
			// What the person typed while the agent was working reaches the model as an
			// attachment, at the time it was typed, and for most such prompts nothing else
			// records it: of 78 here, 45 appear nowhere else and 31 more only where a later
			// compaction summary quotes them. It is the person's words, marked queued: it does
			// not start a round, because the agent did not stop to answer it.
			text := claudeQueuedText(obj)
			if !claudeQueuedPrompt(obj) || text == "" {
				return true
			}
			return sink.add(map[string]any{
				"id":        GetOr(obj, "uuid", ""),
				"role":      "user",
				"timestamp": GetOr(obj, "timestamp", ""),
				"queued":    true,
				"content":   []map[string]any{TextBlock(text)},
			})
		case "user", "assistant":
		default:
			return true
		}
		msg := getMap(obj, "message")
		return sink.add(map[string]any{
			"id":        GetOr(obj, "uuid", ""),
			"role":      StrOr(msg["role"], StrOr(obj["type"], "")),
			"timestamp": GetOr(obj, "timestamp", ""),
			"content":   row.parts(obj),
		})
	})
	return sink.result()
}

func (s *ClaudeCodeSource) Final(r Record) map[string]any {
	path := r.File
	if path == "" {
		return nil
	}
	// Only the probe fields matter here; big fields like content wait until the last
	// assistant message, which is the only one fully decoded
	var rawLast []byte
	count := 0
	// Usage is summed over the whole session rather than taken from the last message: the
	// last message answers "what did the final answer cost", which is not what a Usage
	// card is read as. The scan already walks every line, so this costs one small decode
	// per assistant message.
	var totals usageTotals
	subagent := r.IsSubagent()
	EachJSONLLine(path, func(line []byte) bool {
		var probe claudeProbe
		if json.Unmarshal(line, &probe) != nil || !probe.counts(subagent) {
			return true
		}
		count++
		if probe.Type == "assistant" {
			rawLast = append(rawLast[:0], line...)
			if len(probe.Message.Usage) > 0 {
				var u map[string]any
				if json.Unmarshal(probe.Message.Usage, &u) == nil {
					totals.add(u)
				}
			}
		}
		return true
	})
	var lastAssistant map[string]any
	if len(rawLast) > 0 {
		_ = json.Unmarshal(rawLast, &lastAssistant)
	}
	if lastAssistant == nil {
		return map[string]any{
			"status":       "done",
			"isFinal":      false,
			"isProcessing": false,
			"messageCount": count,
			"source":       "claude",
			"text":         "",
			"thinking":     "",
		}
	}

	msg := getMap(lastAssistant, "message")
	parts := claudeRow{}.parts(lastAssistant)
	texts := []string{}
	thoughts := []string{}
	toolCalls := []any{}
	for _, p := range parts {
		switch p["type"] {
		case "text":
			if c := strField(p, "content"); c != "" {
				texts = append(texts, c)
			}
		case "thinking":
			if c := strField(p, "content"); c != "" {
				thoughts = append(thoughts, c)
			}
		case "toolCall":
			call := map[string]any{
				"name":      strField(p, "name"),
				"arguments": GetOr(p, "arguments", map[string]any{}),
			}
			if id := strField(p, "id"); id != "" {
				call["id"] = id
			}
			toolCalls = append(toolCalls, call)
		}
	}
	stopReason := StrOr(msg["stop_reason"], "")
	isFinal := stopReason == "end_turn" || stopReason == "stop" || stopReason == "stop_sequence"
	return map[string]any{
		"status":       "done",
		"isFinal":      isFinal,
		"isProcessing": false,
		"messageCount": count,
		"source":       "claude",
		"id":           GetOr(msg, "id", ""),
		"timestamp":    GetOr(lastAssistant, "timestamp", ""),
		"stopReason":   stopReason,
		"model":        GetOr(msg, "model", ""),
		"text":         strings.Join(texts, "\n"),
		"thinking":     strings.Join(thoughts, "\n"),
		"toolCalls":    toolCalls,
		"usage":        totals.result(),
	}
}
