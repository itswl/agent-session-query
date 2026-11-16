package transcript

import (
	"strings"
	"unicode"

	"github.com/itswl/agent-session-query/internal/txt"
)

// Which user message is a person asking?
//
// Every CLI writes more into the user's seat than the user typed: command plumbing, hook
// output, task notifications, the instructions a harness appends. A turn starts at a
// person's words and nowhere else, so these rules are the difference between a session
// that reads as its exchanges and one cut at every notification.

// machinePrefixes open user-role text a CLI assembled. Each was seen in real transcripts.
var machinePrefixes = []string{
	// Claude Code: slash-command plumbing, local command output, caveats, hooks, background
	// tasks reporting in
	"<command-name>", "<command-message>", "<command-args>", "<command-contents>",
	"<local-command-", "<bash-", "<caveat", "Caveat:", "<task-notification", "<system-reminder",
	// Codex: the instruction rows it opens a session with
	"<environment_context", "<user_instructions", "<permissions instructions", "<INSTRUCTIONS",
	"<turn_aborted", "# AGENTS.md",
	// An aborted turn's notice
	"[Request interrupted",
}

// harnessBlocks are blocks a harness appends to the person's words before a model reads
// them: T3 Code closes every prompt with these, glued to the last word. They belong to
// the transcript — the model read them — but not to the ask.
var harnessBlocks = []string{"runtime_info", "pull_request_linking"}

// StripHarness drops the harness blocks that close a message, however many, with the
// whitespace between them. Only blocks that close it: the same tag written mid-question
// is the person's, and stays.
func StripHarness(text string) string {
	t := strings.TrimRight(text, " \t\r\n")
	for {
		stripped := false
		for _, tag := range harnessBlocks {
			if !strings.HasSuffix(t, "</"+tag+">") {
				continue
			}
			open := strings.LastIndex(t, "<"+tag+">")
			if open < 0 {
				continue
			}
			t = strings.TrimRight(t[:open], " \t\r\n")
			stripped = true
		}
		if !stripped {
			return t
		}
	}
}

// cutOpenHarness cuts a title where a harness block opens and never closes — the shape of
// a title a CLI made from the head of a prompt the harness had already extended.
func cutOpenHarness(title string) string {
	for _, tag := range harnessBlocks {
		open := strings.Index(title, "<"+tag+">")
		if open >= 0 && !strings.Contains(title[open:], "</"+tag+">") {
			title = title[:open]
		}
	}
	return title
}

// WrappedInTag reports whether text is wholly one XML-style element — <name …>…</name>,
// the same name at both ends and nothing outside. That is how CLIs wrap the context they
// inject and never how a person writes a question.
func WrappedInTag(t string) bool {
	t = strings.TrimSpace(t)
	if len(t) < 5 || t[0] != '<' || t[len(t)-1] != '>' {
		return false
	}
	end := strings.IndexAny(t, " >\n\t")
	if end < 2 {
		return false
	}
	name := t[1:end]
	for _, r := range name {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') {
			return false
		}
	}
	return strings.HasSuffix(t, "</"+name+">")
}

// teammateLeadPrefix opens what an agent team's lead says to one of its members. Claude
// Code hands a member its work, and every later instruction, wrapped this way; in the
// lead's own transcript the members' replies arrive under their own ids and stay what
// they are: news mid-turn.
const teammateLeadPrefix = `<teammate-message teammate_id="team-lead"`

// teammateLeadAsk is the body of a message from a team's lead, and whether text is one.
func teammateLeadAsk(text string) (string, bool) {
	if !strings.HasPrefix(text, teammateLeadPrefix) {
		return "", false
	}
	_, body, ok := strings.Cut(text, ">") // attributes are escaped: the first > ends the tag
	if !ok {
		return "", true
	}
	if end := strings.LastIndex(body, "</teammate-message>"); end >= 0 {
		body = body[:end]
	}
	return strings.TrimSpace(body), true
}

// forkBoilerplateEnd closes the instructions Claude Code puts before a fork's directive.
const forkBoilerplateEnd = "</fork-boilerplate>"

// AskOf decides whether a user message's text is a person asking, and returns the ask as
// it was meant: without a harness's closing blocks, a team lead's instruction without the
// element it arrives in, a fork's directive without the boilerplate before it.
func AskOf(text string) (ask string, ok bool) {
	t := strings.TrimSpace(StripHarness(text))
	if t == "" {
		return "", false
	}
	if body, lead := teammateLeadAsk(t); lead {
		// The lead's words are the member's ask; a protocol message (a shutdown request)
		// is JSON and asks nothing
		return body, body != "" && !strings.HasPrefix(body, "{")
	}
	if strings.HasPrefix(t, "<fork-boilerplate>") {
		if _, directive, cut := strings.Cut(t, forkBoilerplateEnd); cut {
			directive = strings.TrimSpace(directive)
			return directive, directive != ""
		}
	}
	for _, prefix := range machinePrefixes {
		if strings.HasPrefix(t, prefix) {
			return "", false
		}
	}
	if WrappedInTag(t) {
		return "", false
	}
	return t, true
}

// titleLength caps a title made from an ask.
const titleLength = 80

// TitleOf folds text into a one-line title: cleaned, its first line, cut to length. Empty
// when the text is not something a person would recognise as what they asked.
func TitleOf(text string) string {
	ask, ok := AskOf(text)
	if !ok {
		return ""
	}
	return txt.Truncate(txt.FirstLine(txt.Clean(ask)), titleLength, "…")
}

// CleanTitle prepares a title a CLI wrote itself for display.
func CleanTitle(title string) string {
	return txt.Truncate(txt.FirstLine(txt.Clean(cutOpenHarness(StripHarness(title)))), titleLength, "…")
}
