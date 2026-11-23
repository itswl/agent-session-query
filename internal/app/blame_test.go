package app

import (
	"strings"
	"testing"
	"time"
)

// What git commit prints for the commit it made names it, however the line begins; a
// line of git log, or a commit of another id, does not.
func TestMade(t *testing.T) {
	const id = "3f9c2ab4e5f60718293a4b5c6d7e8f9012345678"
	for out, want := range map[string]bool{
		"[main 3f9c2ab] Trim titles\n 1 file changed":                   true,
		"[main (root-commit) 3f9c2ab4e5f6] First":                       true,
		"[detached HEAD 3f9c2ab] Try it":                                true,
		"pre-commit hook passed\n[feature/x 3f9c2ab] Trim titles":       true,
		"[main 3f9c2ac] Another commit":                                 false,
		"commit 3f9c2ab4e5f60718293a4b5c6d7e8f9012345678\nAuthor: test": false,
		"3f9c2ab Trim titles":                                           false,
		"see [main 3f9c2ab] in the log":                                 false,
		"[main 3f9c2a] Six characters is no id git prints for a commit": false,
	} {
		if got := made(out, id); got != want {
			t.Errorf("made(%q) = %v, want %v", out, got, want)
		}
	}
	// A repository that names its objects by SHA-256 prints them as long as it must
	sha256 := strings.Repeat("9b74c98f", 8)
	if !made("[main "+sha256[:41]+"] Long enough", sha256) {
		t.Errorf("a SHA-256 commit printed at 41 characters is not taken for made")
	}
}

// git blame --porcelain says of one line its commit and lines about it, then the line
// after a tab; a line not committed yet is the commit of zeros, which is none.
func TestParseBlame(t *testing.T) {
	b, err := parseBlame("3f9c2ab4e5f60718293a4b5c6d7e8f9012345678 6 6 1\nauthor test\nauthor-time 1763800000\n" +
		"committer-time 1763800060\nsummary Trim the title\nprevious 1111111111111111111111111111111111111111 notes.go\nfilename notes.go\n\treturn strings.TrimSpace(s)\n")
	if err != nil || b.Commit != "3f9c2ab4e5f60718293a4b5c6d7e8f9012345678" || b.Subject != "Trim the title" ||
		!b.At.Equal(time.Unix(1763800060, 0)) || b.Text != "return strings.TrimSpace(s)" {
		t.Errorf("blame = %+v, %v", b, err)
	}
	b, err = parseBlame("0000000000000000000000000000000000000000 1 1 1\nauthor Not Committed Yet\nsummary Version of notes.go from notes.go\nfilename notes.go\n\tpackage notes\n")
	if err != nil || b.Commit != "" || b.Subject != "" || !b.At.IsZero() || b.Text != "package notes" {
		t.Errorf("a line not committed = %+v, %v", b, err)
	}
	sha256 := strings.Repeat("ab12", 16)
	if b, err = parseBlame(sha256 + " 1 1 1\nsummary x\n\tline\n"); err != nil || b.Commit != sha256 {
		t.Errorf("a SHA-256 repository = %+v, %v", b, err)
	}
	for _, out := range []string{"", "usage: git blame\n"} {
		if _, err := parseBlame(out); err == nil {
			t.Errorf("parseBlame(%q) took it for a blame", out)
		}
	}
}
