package mcp

import (
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// health says in its own body what the notes at the end of an answer say — where the index
// stands, and each store with what it holds — and what only asq index said before, the
// version. It takes the budget and nothing else, and is not followed by those notes, which
// would say it again.
func TestHealthSaysWhereAsqStands(t *testing.T) {
	s := server(t)
	text, isErr := call(t, s, "health", map[string]any{})
	if isErr {
		t.Fatal(text)
	}
	for _, want := range []string{
		"asq " + app.Version + "\n",
		"\nIndex ",
		" · 2 sessions · 3 turns · 1 subagent transcript\n",
		"\nLast brought up to date ", ", in ",
		"\nStores:\n  claude       2 sessions · 3 transcripts   ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("health does not say %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"\n[", "asq doctor there", "No CLI's sessions", "upgrade", "archived"} {
		if strings.Contains(text, not) {
			t.Errorf("health says %q:\n%s", not, text)
		}
	}
	if health, ok := find("health"); !ok || len(health.props) != 1 || health.props[0].name != "max_chars" || !health.bare {
		t.Errorf("health takes the budget and nothing else, and says its notes itself: %+v", health)
	}
}

// A store that is there and could not be read is a line of health, with why — and is not
// said again beneath the answer, as it is beneath every other answer.
func TestHealthSaysAStoreCouldNotBeRead(t *testing.T) {
	s := withCodex(t)
	var codex string
	for _, st := range s.App.Set.All() {
		if st.Name == "codex" {
			codex = st.Source.Location()
		}
	}
	sourcetest.Unreadable(t, codex)
	if _, err := s.App.Sync(t.Context(), index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, s, "health", map[string]any{})
	if isErr || !strings.Contains(text, "\n  codex ") || !strings.Contains(text, "there, and could not be read (") ||
		!strings.Contains(text, "; the index holds 1 transcript of it") || strings.Contains(text, "\n[") {
		t.Errorf("health, with a store that cannot be read:\n%s", text)
	}
}
