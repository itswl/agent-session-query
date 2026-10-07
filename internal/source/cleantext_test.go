package source

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestIndexFold(t *testing.T) {
	cases := []struct {
		text, needle string
		want         int
	}{
		{"Hello World", "world", 6},
		{"HELLO", "hello", 0},
		{"nginx.conf", "NGINX", -1}, // the needle must already be lowercase; that is the caller's job
		{"配置 Nginx 反代", "nginx", 7}, // deliberately non-ASCII: "配置 " is 7 bytes, so this
		//                                pins the byte offset against multibyte text
		{"abc", "", 0},
		{"abc", "abcd", -1},
	}
	for _, c := range cases {
		if got := IndexFold(c.text, c.needle); got != c.want {
			t.Errorf("IndexFold(%q, %q) = %d, want %d", c.text, c.needle, got, c.want)
		}
	}
}

func TestSnippetAround(t *testing.T) {
	// The hit sits in the middle, so both ends need an ellipsis — and the snippet must not
	// slice a UTF-8 sequence in half. These fixtures are deliberately non-ASCII: that is
	// precisely what they test.
	long := strings.Repeat("一二三四五", 60) + "关键词" + strings.Repeat("六七八九十", 60)
	got := SnippetAround(long, "关键词", 10)
	if !strings.Contains(got, "关键词") {
		t.Fatalf("the snippet does not contain the Needle: %q", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("both ends were trimmed but carry no ellipsis: %q", got)
	}
	if !utf8Valid(got) {
		t.Fatalf("the snippet broke a UTF-8 sequence: %q", got)
	}
	if n := len([]rune(got)); n > 40 {
		t.Fatalf("the snippet is too long: %d characters", n)
	}

	// Short text is returned as-is, with no ellipsis
	if got := SnippetAround("就这么短", "这么", 20); got != "就这么短" {
		t.Fatalf("short text = %q", got)
	}
	// The hit is at the start, so only the tail should carry an ellipsis
	head := SnippetAround("开头命中"+strings.Repeat("填充", 100), "开头", 5)
	if strings.HasPrefix(head, "…") || !strings.HasSuffix(head, "…") {
		t.Fatalf("hit at the start = %q", head)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestFindMatchingTextPrefersBody(t *testing.T) {
	// When the needle appears in both a field name and the body, return the body
	obj := map[string]any{
		"snippetish": "unrelated",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "changed the nginx timeout"}},
		},
	}
	got, ok := findMatchingText(obj, "nginx", 0)
	if !ok || got != "changed the nginx timeout" {
		t.Fatalf("findMatchingText = %q %v", got, ok)
	}
	// Appearing only in a key name is not a match
	if _, ok := findMatchingText(map[string]any{"nginx": 1}, "nginx", 0); ok {
		t.Fatal("a key name must not count as a match")
	}
}

func TestEscapeLike(t *testing.T) {
	// Searching for "100%" must not turn into matching anything
	if got := EscapeLike("100%"); got != `100\%` {
		t.Fatalf("escapeLike = %q", got)
	}
	if got := EscapeLike("a_b"); got != `a\_b` {
		t.Fatalf("escapeLike = %q", got)
	}
}

func TestSearchFileStopsMidFile(t *testing.T) {
	// Every line matches, so an uncancelled scan necessarily runs to the end. Cancellation
	// is sampled every CancelCheckLines rather than tested per line, so the scan stops
	// within one sample window instead of at an exact line.
	path := filepath.Join(t.TempDir(), "big.jsonl")
	total := 4 * CancelCheckLines
	lines := make([]string, 0, total)
	for i := 0; i < total; i++ {
		lines = append(lines, `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"nginx"}]}}`)
	}
	write(t, path, lines...)

	q := SearchQuery{Needle: "nginx", Lowered: []byte("nginx"), PerSession: total + 1}
	if full := SearchFile(context.Background(), path, q); len(full) != total {
		t.Fatalf("an uncancelled scan should read the whole file: %d hits, want %d", len(full), total)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if stopped := SearchFile(ctx, path, q); len(stopped) > CancelCheckLines {
		t.Fatalf("a cancelled scan should stop within one sample window, got %d hits", len(stopped))
	}
}

// A snippet is cut for display, so it must not carry instructions to whatever prints it.
// Measured on a real corpus: 24 of 157 Claude sessions hold escape sequences, all on the
// rows that carry tool results.
func TestStripTerminalControls(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"colour codes", "\x1b[1;32mpassed\x1b[0m", "passed"},
		{"24-bit colour", "\x1b[38;2;153;153;153mdim\x1b[39m", "dim"},
		{"osc 52 clipboard, BEL terminated", "a\x1b]52;c;cGF5bG9hZA==\x07b", "ab"},
		{"osc terminated by ST", "a\x1b]0;title\x1b\\b", "ab"},
		{"cursor move", "a\x1b[2Jb", "ab"},
		{"bare two-byte escape", "a\x1bMb", "ab"},
		{"carriage return rewrites the line", "done\rFAKE", "doneFAKE"},
		{"tab and newline are layout, and stay", "a\tb\nc", "a\tb\nc"},
		{"unterminated CSI takes the rest", "keep\x1b[38;2;1", "keep"},
		// The nF class: ESC, one or more intermediates in 0x20..0x2f, then a final byte.
		// ESC ( B is what tput sgr0 writes, so it rides along in anything ncurses, less,
		// vim or git coloured. Read as a two-byte escape it leaves a stray "B" behind.
		{"nF escape, the tput sgr0 reset", "test \x1b[32mok\x1b(B\x1b[m done", "test ok done"},
		{"nF escape, select UTF-8", "prefix \x1b%G tail", "prefix  tail"},
		// A second ESC opens a new sequence rather than closing this one
		{"doubled ESC does not hide the second", "\x1b\x1b[0mvisible", "visible"},
		// ESC before a multibyte rune must take only itself, or the rune's continuation
		// bytes are left behind as invalid UTF-8
		{"ESC before a rune keeps the rune whole", "a\x1b中b", "a中b"},
		{"no controls is returned unchanged", "plain 文本 text", "plain 文本 text"},
		{"multibyte survives", "\x1b[31m北京\x1b[0m", "北京"},
	}
	for _, c := range cases {
		got := StripTerminalControls(c.in)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		// Whatever it removes, what it returns must still be text
		if utf8.ValidString(c.in) && !utf8.ValidString(got) {
			t.Errorf("%s: valid input produced invalid UTF-8: %q", c.name, []byte(got))
		}
	}
}

// The cleaning happens inside snippetAround, which is the one place all three snippet
// callers go through, so a match sitting next to colour codes comes back readable.
func TestSnippetAroundStripsControls(t *testing.T) {
	text := "build \x1b[1;32mpassed\x1b[0m every check"
	got := SnippetAround(text, "passed", 70)
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("snippet still carries an escape: %q", got)
	}
	if got != "build passed every check" {
		t.Errorf("snippet = %q", got)
	}
	// Stripping first also repairs a needle that colour codes had split in the stored text
	split := "pas\x1b[0msed the check"
	if s := SnippetAround(split, "passed", 70); s != "passed the check" {
		t.Errorf("split needle snippet = %q", s)
	}
}

// A brief is copied and handed to another agent, and it quotes the prompt, so a key pasted
// into a prompt rides along. Measured on a real brief here before this existed: one live
// 51-character key, in full. Every value below is invented.
func TestRedactSecrets(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"provider key", "use sk-Ab3xQ9zK7mN2pR5tV8wY1cE4gH6jL0sD as the token", "use [redacted] as the token"},
		{"github token", "export GH=ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8", "export GH=[redacted]"},
		{"aws key id", "AKIAJ7PQ3MZX2WVTKL4A is the id", "[redacted] is the id"},
		{"jwt", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.dBjftJeZ4CVPmB92K27u", "Bearer [redacted]"},
		{"pem header", "-----BEGIN RSA PRIVATE KEY-----\nMIIE", "[redacted]\nMIIE"},
		// A name that merely starts with a key prefix is words joined by dashes: no digits,
		// no mixed case, and redacting it would mangle ordinary text
		{"a kebab name is not a key", "see sk-migration-runner-config for that", "see sk-migration-runner-config for that"},
		{"ordinary text is untouched", "把数据库迁移脚本跑一遍 then commit a1b2c3d", "把数据库迁移脚本跑一遍 then commit a1b2c3d"},
		{"a git sha is not a key", "fixed in 9f8e7d6c5b4a3210fedcba9876543210abcdef12", "fixed in 9f8e7d6c5b4a3210fedcba9876543210abcdef12"},
	} {
		if got := RedactSecrets(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// Colour codes inside a key would hide it from a pattern, so the escapes come off first
func TestRedactAfterStripping(t *testing.T) {
	split := "token \x1b[32msk-Ab3xQ9zK7mN2pR5tV8wY1cE4gH6jL0sD\x1b[0m ok"
	if got := RedactSecrets(StripTerminalControls(split)); got != "token [redacted] ok" {
		t.Errorf("got %q", got)
	}
}
