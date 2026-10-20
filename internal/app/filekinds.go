package app

import (
	"path"
	"strconv"
	"strings"
)

// File kinds: what a round's changes were made of, by path alone — a code-heavy change and
// one that is mostly tests and docs read differently in a handoff. Convention, not
// content: the rules are the mainstream layouts and a miss shows a wrong word, nothing
// more. No configuration, no AI.
const (
	kindCode   = "code"
	kindTest   = "test"
	kindDocs   = "docs"
	kindConfig = "config"
)

// fileKind classifies one path: tests first (a *_test.go is a test, not code), then docs,
// then configuration, and everything else is code.
func fileKind(p string) string {
	p = strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
	base := path.Base(p)
	segments := strings.Split(p, "/")

	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasSuffix(base, "_test.py"),
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.Contains(base, ".test."),
		strings.Contains(base, ".spec."),
		hasSegment(segments, "test", "tests", "__tests__", "spec", "specs"):
		return kindTest
	case strings.HasSuffix(base, ".md") || strings.HasSuffix(base, ".mdx") || strings.HasSuffix(base, ".rst"),
		hasSegment(segments, "docs", "doc", "documentation"),
		strings.HasPrefix(base, "readme"), strings.HasPrefix(base, "changelog"),
		strings.HasPrefix(base, "license"), strings.HasPrefix(base, "contributing"):
		return kindDocs
	case strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml") ||
		strings.HasSuffix(base, ".toml") || strings.HasSuffix(base, ".ini") || strings.HasSuffix(base, ".cfg") ||
		strings.HasSuffix(base, ".conf") || strings.HasSuffix(base, ".lock") ||
		base == "go.mod" || base == "go.sum" || base == "go.work" || base == "dockerfile" ||
		base == ".gitignore" || base == ".dockerignore" ||
		strings.HasPrefix(base, ".") && strings.HasSuffix(base, "rc"):
		return kindConfig
	}
	return kindCode
}

// hasSegment reports whether any path segment is one of names (a directory, not a file)
func hasSegment(segments []string, names ...string) bool {
	for _, segment := range segments[:len(segments)-1] { // the base is the file
		for _, name := range names {
			if segment == name {
				return true
			}
		}
	}
	return false
}

// fileKindCounts counts a round's changed files by kind. Every kind is present, zero
// included, so a client reads the same keys on every round.
func fileKindCounts(files []string) map[string]int {
	counts := map[string]int{kindCode: 0, kindTest: 0, kindDocs: 0, kindConfig: 0}
	for _, f := range files {
		counts[fileKind(f)]++
	}
	return counts
}

// fileKindLine renders counts as "4 code · 2 tests · 1 doc" — zero kinds left out, code
// first, then tests, docs and config. Empty when nothing was changed.
func fileKindLine(files []string) string {
	if len(files) == 0 {
		return ""
	}
	counts := fileKindCounts(files)
	english := map[string][2]string{
		kindCode:   {"code", "code"},
		kindTest:   {"test", "tests"},
		kindDocs:   {"doc", "docs"},
		kindConfig: {"config", "config"},
	}
	order := []string{kindCode, kindTest, kindDocs, kindConfig}
	parts := []string{}
	for _, kind := range order {
		n := counts[kind]
		if n == 0 {
			continue
		}
		word := english[kind][1]
		if n == 1 {
			word = english[kind][0]
		}
		parts = append(parts, strconv.Itoa(n)+" "+word)
	}
	return strings.Join(parts, " · ")
}
