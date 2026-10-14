package source

import (
	"path/filepath"
	"testing"
)

func TestTitleFromUserText(t *testing.T) {
	// Filled with a real letter rather than the zero rune: NUL is a control character and
	// is now stripped, which would leave this case testing an empty string
	long := make([]rune, 120)
	for i := range long {
		long[i] = 'a'
	}

	cases := []struct {
		name, in, want string
	}{
		{"a plain question", "帮我连一下 VPN\n然后看看路由表", "帮我连一下 VPN"},
		{"truncated at 80 runes", string(long), Truncate(string(long), titleLength, "…")},
		// A title is synthesised for display, so it carries no instructions to whatever
		// prints it. Pasting coloured terminal output into a prompt is the ordinary way
		// this happens.
		{"colour codes are stripped", "\x1b[1;32m构建通过\x1b[0m", "构建通过"},
		{"control characters go too", "看下\x00这个\x07报错", "看下这个报错"},
		// Cleaning first is what makes this work: the prefix check runs on the cleaned
		// text, so noise cannot hide behind an escape sequence
		{"a noise prefix cannot hide behind an escape", "\x1b[0m<command-name>/clear</command-name>", ""},
		{"claude command plumbing", "<command-name>/clear</command-name>", ""},
		{"claude caveat row", "<local-command-caveat>Caveat: The messages below were generated…", ""},
		{"claude standalone caveat", "Caveat: since this is a fresh session", ""},
		{"system reminder injection", "<system-reminder>hooks configured</system-reminder>", ""},
		{"codex agents instructions", "# AGENTS.md instructions for /w/proj\n\n<INSTRUCTIONS>", ""},
		{"codex environment context", "<environment_context>cwd=/w</environment_context>", ""},
		{"an interrupted turn", "[Request interrupted by user]", ""},
		{"whitespace only", "  \n  ", ""},
		{"leading and trailing blanks", "  看下这个报错  ", "看下这个报错"},
		{"the first line wins over pasted output", "评审一下这个服务\nError: boom\nError: boom2", "评审一下这个服务"},
		// T3 Code glues its blocks to the last word, so they sat on the first line
		{"a harness's closing blocks are not the ask", "修一下构建<runtime_info>In case you're asked: you run in T3 Code.</runtime_info><pull_request_linking>Link every PR.</pull_request_linking>", "修一下构建"},
	}
	for _, c := range cases {
		if got := TitleFromUserText(c.in); got != c.want {
			t.Errorf("%s: titleFromUserText = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestStripHarnessBlocks: only blocks that close the message go, in either order and with
// the blanks between them; the same tag mid-question is the user's.
func TestStripHarnessBlocks(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"glued to the last word", "fix the build<runtime_info>you run in T3 Code</runtime_info><pull_request_linking>link PRs</pull_request_linking>", "fix the build"},
		{"blank lines between", "fix the build\n\n<runtime_info>x</runtime_info>\n<pull_request_linking>y</pull_request_linking>\n", "fix the build"},
		{"either order", "fix the build<pull_request_linking>y</pull_request_linking><runtime_info>x</runtime_info>", "fix the build"},
		{"one of them", "fix the build\n<runtime_info>x</runtime_info>", "fix the build"},
		{"the tag mid-question stays", "why does <runtime_info>x</runtime_info> show up in my prompts?", "why does <runtime_info>x</runtime_info> show up in my prompts?"},
		{"a closing tag with no opening stays", "it ends with </runtime_info>", "it ends with </runtime_info>"},
		{"other tags are not this rule's", "look <system-reminder>x</system-reminder>", "look <system-reminder>x</system-reminder>"},
		{"nothing else", "<runtime_info>x</runtime_info><pull_request_linking>y</pull_request_linking>", ""},
		{"plain words", "fix the build", "fix the build"},
	}
	for _, c := range cases {
		if got := StripHarnessBlocks(c.in); got != c.want {
			t.Errorf("%s: StripHarnessBlocks = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestCutOpenHarnessBlock: a title made from a prompt's head can stop inside a harness
// block; it is cut where the block opens. A block that closes is not this rule's.
func TestCutOpenHarnessBlock(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"stopped inside the first block", "fix the build<runtime_info>In case you're asked: you are runn", "fix the build"},
		{"stopped inside the second", "fix the build<runtime_info>x</runtime_info><pull_request_linking>When the t3", "fix the build<runtime_info>x</runtime_info>"},
		{"a closed block is left to StripHarnessBlocks", "fix the build<runtime_info>x</runtime_info>", "fix the build<runtime_info>x</runtime_info>"},
		{"no block", "Reading sample.txt", "Reading sample.txt"},
	}
	for _, c := range cases {
		if got := cutOpenHarnessBlock(c.in); got != c.want {
			t.Errorf("%s: cutOpenHarnessBlock = %q, want %q", c.name, got, c.want)
		}
	}
}

// The per-source extractors share everything but the row shape; one test each pins that
// shape — a noise row must be passed over, not reported, and the fallback keeps the stem.
func TestFirstUserTitlePerSource(t *testing.T) {
	dir := t.TempDir()

	claudePath := filepath.Join(dir, "c.jsonl")
	write(t, claudePath,
		`{"type":"queue-operation","cwd":"/w/a"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<command-name>/init</command-name>"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"把这个项目的测试补上"}]}}`,
	)
	if got := claudeUserTitle(claudePath); got != "把这个项目的测试补上" {
		t.Errorf("claude title = %q", got)
	}

	piPath := filepath.Join(dir, "p.jsonl")
	write(t, piPath,
		`{"type":"session","id":"s1","cwd":"/w/b"}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"为什么这个接口 502"}]}}`,
	)
	if got := piUserTitle(piPath); got != "为什么这个接口 502" {
		t.Errorf("pi title = %q", got)
	}

	codexPath := filepath.Join(dir, "x.jsonl")
	write(t, codexPath,
		`{"type":"session_meta","payload":{"session_id":"sx","cwd":"/w/c"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /w/c"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"任务失败了帮我看下日志"}]}}`,
	)
	if got := codexUserTitle(codexPath); got != "任务失败了帮我看下日志" {
		t.Errorf("codex title = %q", got)
	}

	geminiPath := filepath.Join(dir, "g.jsonl")
	write(t, geminiPath,
		`{"sessionId":"g1","startTime":"2026-09-01T00:00:00Z"}`,
		`{"type":"user","content":[{"text":"这个正则怎么写"}]}`,
	)
	if got := geminiUserTitle(geminiPath); got != "这个正则怎么写" {
		t.Errorf("gemini title = %q", got)
	}

	// Nothing usable anywhere: empty, so the caller falls back to the file stem
	noisePath := filepath.Join(dir, "n.jsonl")
	write(t, noisePath,
		`{"type":"session","id":"s9"}`,
		`{"type":"user","message":{"role":"user","content":"Caveat: fresh session"}}`,
	)
	for name, fn := range map[string]func(string) string{
		"claude": claudeUserTitle, "pi": piUserTitle,
	} {
		if got := fn(noisePath); got != "" {
			t.Errorf("%s: a noise-only file must yield no title, got %q", name, got)
		}
	}
}

// TestEpochToTimeIsExact: an epoch in milliseconds is the instant it names, to the
// microsecond — not 14 nanoseconds before it, as a float division made it, which put a
// message just outside a window anchored at its own time
func TestEpochToTimeIsExact(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{1791312688214, "2026-10-06T18:51:28.214Z"},
		{1789705756.123, "2026-09-18T04:29:16.123Z"},
		{1789705756, "2026-09-18T04:29:16Z"},
	} {
		got, ok := epochToTime(c.in)
		if !ok || got.Format("2006-01-02T15:04:05.999999999Z07:00") != c.want {
			t.Errorf("epochToTime(%v) = %v, want %s", c.in, got, c.want)
		}
	}
}
