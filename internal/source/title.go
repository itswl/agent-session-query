package source

import "strings"

// Session titles.
//
// opencode writes a real title for every session; the other sources write none, and their
// lists show a sessionId or a filename instead. The closest equivalent those sources have
// is the first real user message, which is also where opencode's own titles come from —
// it just has a model summarise the line instead of showing it.
//
// Coverage measured locally before building this: 190/192 Claude sessions, 31/33 Gemini,
// 2/2 Pi, 2/2 Codex have a usable first user message within the head-scan window. The
// misses fall back to what the list showed before, so a noisy file loses nothing.

// titleLength caps the extracted title.
const titleLength = 80

// titleNoisePrefixes are machine-assembled openings that look like user messages but are
// not a question. Each was seen in real local data, not invented:
//
//   - `<command…` / `<local-command…` / `<bash…` — Claude Code slash-command plumbing;
//     the caveat row it emits wraps the actual user text, and digging that out would
//     surface command output as a title, so the whole row is skipped instead
//   - `<system-reminder` — hooks and context injected as a user turn
//   - `<environment_context` / `<user_instructions` / "# AGENTS.md" — Codex's opening
//     instruction rows, role=user but assembled by the CLI
//   - "[Request interrupted" — an aborted turn
//   - "Caveat:" — the standalone form of the Claude Code caveat row
var titleNoisePrefixes = []string{
	"<command", "<local-command", "<bash", "<system-reminder", "<task-notification",
	"<environment_context", "<user_instructions", "# AGENTS.md",
	"[Request interrupted", "Caveat:",
}

// TitleFromUserText folds one user message into a single-line title: noise rows are
// rejected, the first non-empty line is taken, and the result is rune-safely truncated.
// An empty return means "nothing usable here, keep looking / fall back".
func TitleFromUserText(text string) string {
	// Cleaned before anything else looks at it. A title is synthesised for display, like a
	// snippet, so the same rule applies (see stripTerminalControls). Doing it first also
	// means a noise prefix cannot hide behind an escape sequence, and that the rune cut
	// below cannot land inside one — a half-written CSI is worse than a whole one, because
	// a terminal swallows what follows while it waits for the final byte.
	t := strings.TrimSpace(RedactSecrets(StripTerminalControls(text)))
	if t == "" {
		return ""
	}
	for _, prefix := range titleNoisePrefixes {
		if strings.HasPrefix(t, prefix) {
			return ""
		}
	}
	// The first line carries the intent; later lines are usually detail or pasted output
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	return Truncate(t, titleLength, "…")
}

// firstUserTitle scans a session file's head for the first real user message and returns
// its title. userText decides whether a row is a user message and extracts its text; the
// sources differ in exactly that shape, everything else is shared.
func firstUserTitle(path string, headLines int, userText func(obj map[string]any) (string, bool)) string {
	title := ""
	seen := 0
	eachJSONL(path, func(obj map[string]any) bool {
		seen++
		if title == "" {
			if text, ok := userText(obj); ok {
				title = TitleFromUserText(text)
			}
		}
		return title == "" && seen < headLines
	})
	return title
}
