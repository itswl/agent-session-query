package app

import "unicode/utf8"

// Blocks: the one content shape every source converges on.
//
// A message's content is an array of blocks. Eight sources spell their transcripts eight
// ways — Claude nests tool_use inside an assistant row and tool_result inside a user row,
// Codex writes each call and each output as a row of its own, OpenCode keeps a call and its
// result in one part, Hermes puts the result in a role=tool row — and the page, the brief
// and the MCP tools want to read one shape. These helpers build it, so the fields a block
// may carry are declared once rather than spelled by hand in every parser:
//
//	text        content
//	thinking    content, truncated
//	toolCall    id, name, arguments
//	toolResult  callId, toolName, content, status, exitCode, durationMs, truncated
//	event       kind, content — something that happened between the turns: a context
//	            compaction, the user interrupting, a hook that failed, a model change
//
// id / callId pair a result with its call, which is what lets a reader say which command
// failed rather than only that one did. status is one of ok / error / interrupted and is
// present only when the source said so; a result without it is a result whose outcome the
// writer did not record (Hermes, for instance), which is not the same as success.
//
// Tool output and thinking are cut for the ordinary read — a 200-message window carrying
// every full build log would be megabytes — and the cut is marked with truncated:true so a
// reader knows there is more. A full read (?full=1, the export, MCP's full) keeps every
// byte: an agent reading the output of the command that failed needs the end of it, which
// is exactly the part a preview drops.

const (
	toolResultLimit = 500
	thinkingLimit   = 1000
	truncationMark  = "...[truncated]"

	statusOK          = "ok"
	statusError       = "error"
	statusInterrupted = "interrupted"

	eventCompaction  = "compaction"
	eventInterrupted = "interrupted"
	eventHookError   = "hook_error"
	eventModelChange = "model_change"
)

// clip shortens text to limit runes unless the read is a full one. The second result says
// whether anything was cut.
func clip(text string, limit int, full bool) (string, bool) {
	if full || utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	return truncate(text, limit, truncationMark), true
}

func textBlock(content string) map[string]any {
	return map[string]any{"type": "text", "content": content}
}

func thinkingBlock(content string, full bool) map[string]any {
	cut, truncated := clip(content, thinkingLimit, full)
	block := map[string]any{"type": "thinking", "content": cut}
	if truncated {
		block["truncated"] = true
	}
	return block
}

// toolCallBlock: id is omitted rather than empty when the source has none, so a reader can
// tell "unpaired" from "paired with the call whose id is the empty string".
func toolCallBlock(id, name string, arguments any) map[string]any {
	if arguments == nil {
		arguments = map[string]any{}
	}
	block := map[string]any{"type": "toolCall", "name": name, "arguments": arguments}
	if id != "" {
		block["id"] = id
	}
	return block
}

func toolResultBlock(callID, toolName, content string, full bool) map[string]any {
	cut, truncated := clip(content, toolResultLimit, full)
	block := map[string]any{"type": "toolResult", "toolName": toolName, "content": cut}
	if callID != "" {
		block["callId"] = callID
	}
	if truncated {
		block["truncated"] = true
	}
	return block
}

// toolOutcome is what a source learned about how a tool call ended: a status when it said
// one, an exit code when the tool was a command, a duration when it was timed. Zero values
// mean "not recorded" and write nothing.
type toolOutcome struct {
	status     string
	exitCode   int
	hasExit    bool
	durationMs int64
}

// apply writes the outcome onto a toolResult block. A non-zero exit code with no explicit
// status is an error: the tool said so in the only way a command can.
func (o toolOutcome) apply(block map[string]any) map[string]any {
	status := o.status
	if status == "" && o.hasExit && o.exitCode != 0 {
		status = statusError
	}
	if status != "" {
		block["status"] = status
	}
	if o.hasExit {
		block["exitCode"] = o.exitCode
	}
	if o.durationMs > 0 {
		block["durationMs"] = o.durationMs
	}
	return block
}

// statusFromError folds the one bit most sources record — did it fail — into a status.
func statusFromError(isError bool) string {
	if isError {
		return statusError
	}
	return statusOK
}

func eventBlock(kind, content string) map[string]any {
	return map[string]any{"type": "event", "kind": kind, "content": content}
}

// isFailedResult reports whether a toolResult block records a failure or an interruption.
func isFailedResult(block map[string]any) bool {
	if toStr(block["type"]) != "toolResult" {
		return false
	}
	status := toStr(block["status"])
	return status == statusError || status == statusInterrupted
}
