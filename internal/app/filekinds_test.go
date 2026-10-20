package app

import "testing"

// TestFileKind: convention, not content — a path says what kind of change it is
func TestFileKind(t *testing.T) {
	cases := map[string]string{
		"internal/app/api.go":                  kindCode,
		"internal/app/api_test.go":             kindTest,
		"src/components/Button.test.tsx":       kindTest,
		"web/__tests__/app.js":                 kindTest,
		"tests/integration.py":                 kindTest,
		"pkg/foo_test.py":                      kindTest,
		"README.md":                            kindDocs,
		"docs/api.md":                          kindDocs,
		"CHANGELOG.md":                         kindDocs,
		"notes/design.mdx":                     kindDocs,
		"go.mod":                               kindConfig,
		"package.json":                         kindConfig,
		"deploy/docker-compose.yml":            kindConfig,
		"config/app.toml":                      kindConfig,
		"yarn.lock":                            kindConfig,
		".golangci.yml":                        kindConfig,
		"cmd/agent-session-query/main.go":      kindCode,
		"internal/source/source_opencode.go":   kindCode,
		"internal/app/ui/app.js":               kindCode,
		"internal/app/ui/style.css":            kindCode,
		"internal/source/hermes_sqlite.go":     kindCode,
		"C:\\work\\repo\\internal\\app\\ui.go": kindCode,
	}
	for path, want := range cases {
		if got := fileKind(path); got != want {
			t.Errorf("fileKind(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestFileKindLine: the brief's one-line split leaves zeros out and counts singulars
func TestFileKindLine(t *testing.T) {
	line := fileKindLine([]string{
		"internal/app/brief.go", "internal/app/api.go", "internal/source/record.go", "cmd/main.go",
		"internal/app/brief_test.go", "internal/source/record_test.go",
		"docs/api.md",
		"go.mod",
	})
	if want := "4 code · 2 tests · 1 doc · 1 config"; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	if got := fileKindLine([]string{"main.go"}); got != "1 code" {
		t.Fatalf("single file = %q", got)
	}
	if got := fileKindLine(nil); got != "" {
		t.Fatalf("no files = %q", got)
	}
}
