package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

// The page keeps a session one by one, and stops: the one change the API makes, in asq's
// own state and folder, and refused like any request from another site.
func TestASessionIsKeptFromThePage(t *testing.T) {
	h := fixture.Write(t)
	cfg := h.Config(t)
	cfg.State = t.TempDir()
	a, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Sync(t.Context(), index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	s, err := New(a, Options{Page: page})
	if err != nil {
		t.Fatal(err)
	}
	alpha := "/api/sessions/claude/" + fixture.Alpha[:8]
	kept := func(c call) (int, index.Session) {
		t.Helper()
		rec := c.do(s)
		var got struct {
			Session index.Session `json:"session"`
		}
		json.Unmarshal(rec.Body.Bytes(), &got)
		return rec.Code, got.Session
	}
	copy := filepath.Join(cfg.State, "kept", "claude", "-work-alpha", fixture.Alpha+".jsonl")

	if code, sess := kept(call{path: alpha}); code != http.StatusOK || !sess.Keepable || sess.Kept != "" {
		t.Errorf("before = %d, keepable %v, kept %q", code, sess.Keepable, sess.Kept)
	}
	if code, sess := kept(call{method: http.MethodPost, path: alpha + "/keep", body: `{"keep":true}`}); code != http.StatusOK || sess.Kept != "one" {
		t.Errorf("keep = %d, kept %q", code, sess.Kept)
	}
	if _, err := os.Stat(copy); err != nil {
		t.Errorf("kept, and no copy: %v", err)
	}
	if code, sess := kept(call{path: alpha}); code != http.StatusOK || sess.Kept != "one" {
		t.Errorf("after = %d, kept %q", code, sess.Kept)
	}
	if code, sess := kept(call{method: http.MethodPost, path: alpha + "/keep", body: `{"keep":false}`}); code != http.StatusOK || sess.Kept != "" {
		t.Errorf("stop keeping = %d, kept %q", code, sess.Kept)
	}
	if _, err := os.Stat(copy); !os.IsNotExist(err) {
		t.Errorf("the copy of a session its CLI holds stays: %v", err)
	}

	for name, c := range map[string]struct {
		call
		code int
	}{
		"a subagent's transcript": {call{method: http.MethodPost, path: "/api/sessions/claude/" + fixture.Agent + "/keep", body: `{"keep":true}`}, http.StatusConflict},
		"no word of what to do":   {call{method: http.MethodPost, path: alpha + "/keep", body: `{}`}, http.StatusBadRequest},
		"another site's script":   {call{method: http.MethodPost, path: alpha + "/keep", body: `{"keep":true}`, header: map[string]string{"Origin": "https://attacker.example"}}, http.StatusForbidden},
		"another site's form":     {call{method: http.MethodPost, path: alpha + "/keep", body: `{"keep":true}`, header: map[string]string{"Sec-Fetch-Site": "cross-site"}}, http.StatusForbidden},
	} {
		if rec := c.do(s); rec.Code != c.code {
			t.Errorf("%s = %d, want %d: %s", name, rec.Code, c.code, rec.Body)
		}
	}
	if _, err := os.Stat(copy); !os.IsNotExist(err) {
		t.Errorf("a refused request kept the session: %v", err)
	}
}
