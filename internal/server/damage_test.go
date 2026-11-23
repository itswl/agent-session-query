package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

// A server whose index is damaged where only a question reads — the words a search looks
// in — answers that question with what went wrong, and has the index set aside and made
// anew by the watcher's next look at the stores.
func TestADamagedIndexMetByAQuestionIsMadeAnew(t *testing.T) {
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
	s, err := New(a, Options{Page: page})
	if err != nil {
		t.Fatal(err)
	}
	rec := call{path: "/api/search?q=flaky"}.do(s)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "the index is damaged") {
		t.Fatalf("a search of a damaged index: %d %s", rec.Code, rec.Body)
	}
	res, err := a.Sync(t.Context(), index.SyncOptions{})
	if err != nil || !strings.Contains(res.Reopened, "set aside") {
		t.Fatalf("the watcher's next sync: %+v, %v", res, err)
	}
	rec = call{path: "/api/search?q=flaky"}.do(s)
	var found struct{ Total int }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &found) != nil || found.Total != 1 {
		t.Errorf("a search once the index is made anew: %d %s", rec.Code, rec.Body)
	}
}
