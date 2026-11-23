package mcp

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

// An agent whose question meets a damaged index is told so, and its next question is
// answered from an index set aside and made anew in between.
func TestADamagedIndexIsMadeAnewBeforeTheNextQuestion(t *testing.T) {
	h := fixture.Write(t)
	cfg := h.Config(t)
	a, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Sync(t.Context(), index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	fixture.Damage(t, cfg.IndexPath, "words_data")

	a, err = app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s := New(a, io.Discard)
	s.MaxAge = time.Nanosecond // every question looks at the stores first, as one a while after the last does

	text, failed := call(t, s, "search", map[string]any{"query": "flaky"})
	if !failed || !strings.Contains(text, "the index is damaged") {
		t.Fatalf("a search of a damaged index (error %v): %s", failed, text)
	}
	text, failed = call(t, s, "search", map[string]any{"query": "flaky"})
	if failed || !strings.Contains(text, "aaaa1111") {
		t.Errorf("the next search (error %v): %s", failed, text)
	}
}
