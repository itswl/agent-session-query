package skill

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The skill an agent opened in this repository reads is the one the binary prints: an edit
// to one is an edit to both.
func TestTheRepositoryCopyIsTheSame(t *testing.T) {
	repo, err := os.ReadFile(filepath.Join("..", "..", ".agents", "skills", Name, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	unix := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") } // as a checkout on Windows may write either
	if unix(string(repo)) != unix(Text) {
		t.Errorf("internal/skill/SKILL.md and .agents/skills/%s/SKILL.md differ: they are one skill, and both are edited", Name)
	}
}

// Every CLI that reads skills reads the same few lines first: the name, which is the
// directory it is saved in, and a description that says when to use it. What follows names
// every tool, and the command line's way to keep an answer to a budget.
func TestTheSkillSaysWhatItIs(t *testing.T) {
	text := strings.ReplaceAll(Text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\nname: "+Name+"\ndescription: ") || !strings.Contains(text, "\n---\n") {
		t.Errorf("the skill does not open with its name and description:\n%s", text[:min(len(text), 300)])
	}
	// The description is one scalar in single quotes, where an apostrophe of its own ends
	// it: a CLI reading the front matter then reads a skill with no description
	line, _, _ := strings.Cut(strings.TrimPrefix(text, "---\nname: "+Name+"\ndescription: "), "\n")
	if !strings.HasSuffix(line, "'") || strings.Count(strings.ReplaceAll(line, "''", ""), "'") != 2 {
		t.Errorf("the description is not one single-quoted scalar: %s", line)
	}
	for _, word := range []string{"search", "sessions", "read", "brief", "pack", "usage", "failures", "digest", "files", "projects", "keep", "--max-chars", "/asq:handoff"} {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(strings.TrimLeft(word, "-/")) + `\b`).MatchString(text) {
			t.Errorf("the skill does not name %s", word)
		}
	}
}
