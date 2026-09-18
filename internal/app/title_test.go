package app

import (
	"path/filepath"
	"testing"
)

func TestTitleFromUserText(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"a plain question", "帮我连一下 VPN\n然后看看路由表", "帮我连一下 VPN"},
		{"truncated at 80 runes", string(make([]rune, 120)), truncate(string(make([]rune, 120)), titleLength, "…")},
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
	}
	for _, c := range cases {
		if got := titleFromUserText(c.in); got != c.want {
			t.Errorf("%s: titleFromUserText = %q, want %q", c.name, got, c.want)
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
