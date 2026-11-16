package txt

import (
	"strings"
	"testing"
	"time"
)

func TestStripControls(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                     "plain",
		"\x1b[31mred\x1b[0m":        "red",
		"\x1b[38;5;196mdeep\x1b[m":  "deep",
		"a\x1b]52;c;ZXZpbA==\x07b":  "ab", // OSC 52: it writes the clipboard of whoever prints it
		"a\x1b]0;title\x1b\\b":      "ab", // … ended by ST instead of BEL
		"line\rover":                "lineover",
		"tab\tand\nnewline":         "tab\tand\nnewline",
		"bell\x07 null\x00 del\x7f": "bell null del",
		"\x1b[1;31":                 "", // a sequence cut short takes what is left: its tail is not text
		"\x1b(Bcharset":             "charset",
		"\x1b7saved":                "saved",
		"中文\x1b[0m字":                "中文字",
		"keep é中\U0001F600 intact":  "keep é中\U0001F600 intact",
	} {
		if got := StripControls(in); got != want {
			t.Errorf("StripControls(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedact(t *testing.T) {
	secrets := []string{
		"sk-Zx9Qw7Lm3Pv5Tr8Kd2Nf6Hb4",
		"sk-ant-api03-AbCdEf0123456789AbCdEf0123456789",
		"ghp_0123456789abcdefghijABCDEFGHIJ012345",
		"github_pat_11ABCDEFG0abcdefghijkl_mnopqrstuv",
		"xoxb-123456789012-abcdefghijkl",
		"AIzaSyA0123456789abcdefghijklmnopqrstuv",
		"AKIAIOSFODNN7EXAMPLE",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N",
		"-----BEGIN RSA PRIVATE KEY-----",
	}
	for _, secret := range secrets {
		got := Redact("the value is " + secret + " and then more")
		if strings.Contains(got, secret) || !strings.Contains(got, "[redacted]") || !strings.HasSuffix(got, " and then more") {
			t.Errorf("Redact left %q in %q", secret, got)
		}
	}
	// Words that only begin like a key are words
	for _, plain := range []string{
		"sk-learn-pipeline-utilities",
		"the pk-table-migration-script ran",
		"rk-backup-restore-procedure",
		"task-0123456789abcdef",
		"https://example.com/a/very/long/path/that/is/not/a/key",
		"eyJ is how a JWT starts",
	} {
		if got := Redact(plain); got != plain {
			t.Errorf("Redact(%q) = %q: not a secret", plain, got)
		}
	}
	// A key with a colour code in the middle is one string once the escapes are gone
	if got := Clean("sk-Zx9Qw7Lm3P\x1b[0mv5Tr8Kd2Nf6Hb4"); got != "[redacted]" {
		t.Errorf("Clean = %q", got)
	}
}

func TestCutting(t *testing.T) {
	if got := Truncate("héllo wörld", 5, "…"); got != "héllo…" {
		t.Errorf("Truncate counts characters, not bytes: %q", got)
	}
	if got := Truncate("short", 10, "…"); got != "short" {
		t.Errorf("Truncate = %q", got)
	}
	if got := Cap("the quick brown fox jumps over the lazy dog", 20); got != "the quick brown fox …" {
		t.Errorf("Cap cuts at a space: %q", got)
	}
	if got := Cap("数据库索引已经加上了而且测试也都通过了", 6); got != "数据库索引已 …" {
		t.Errorf("Cap = %q", got)
	}
	if got := OneLine("  one\n\ntwo\tthree  ", 0); got != "one two three" {
		t.Errorf("OneLine = %q", got)
	}
	if got := FirstLine("\n\n  the first  \nthe second"); got != "the first" {
		t.Errorf("FirstLine = %q", got)
	}

	long := strings.Repeat("0123456789", 1000) + "THE END"
	got, cut := Preview(long, 2000)
	if !cut || !strings.HasSuffix(got, "THE END") || !strings.HasPrefix(got, "0123456789") || !strings.Contains(got, "left out") || len(got) > 2100 {
		t.Errorf("Preview keeps the head and the end and says what is missing: %d bytes, cut=%v", len(got), cut)
	}
	if strings.Count(got, "0") <= 150 {
		t.Error("the tail gets the larger share")
	}
	if got, cut := Preview("short", 2000); cut || got != "short" {
		t.Errorf("Preview(short) = %q, %v", got, cut)
	}
	// Never cut inside a character
	wide := strings.Repeat("数", 3000)
	got, _ = Preview(wide, 1000)
	if strings.ContainsRune(got, '�') {
		t.Error("Preview split a character")
	}
}

func TestFigures(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 940: "940", 1000: "1.0k", 18_240: "18.2k", 3_400_000: "3.4M", 1_200_000_000: "1.2B"} {
		if got := Tokens(n); got != want {
			t.Errorf("Tokens(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		850 * time.Millisecond: "850ms", 12 * time.Second: "12s", 4*time.Minute + 7*time.Second: "4m07s",
		2*time.Hour + 13*time.Minute: "2h13m", 76 * time.Hour: "3d4h", -time.Second: "0s",
	} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for ago, want := range map[time.Duration]string{20 * time.Second: "just now", 5 * time.Minute: "5m ago", 3 * time.Hour: "3h ago", 50 * time.Hour: "2d ago"} {
		if got := Ago(now.Add(-ago), now); got != want {
			t.Errorf("Ago(%v) = %q, want %q", ago, got, want)
		}
	}
	if Ago(time.Time{}, now) != "" {
		t.Error("a time that is not known is not 'ago'")
	}
}
