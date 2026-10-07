package app

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A source that cannot be read must say so (see listErrorReporter): showing it as an
// empty source is the exact failure the mechanism exists to prevent, and for seven of
// the eight sources a moved schema or a corrupt index used to read as "no sessions".

func TestJsonMapListErrorOnCorruptSessionsJSON(t *testing.T) {
	home := t.TempDir()
	def := openclawDef(home)
	if err := os.MkdirAll(filepath.Dir(def.sessionsJSON), 0o755); err != nil {
		t.Fatal(err)
	}
	// Truncated mid-write, as another process's half-finished write leaves it
	if err := os.WriteFile(def.sessionsJSON, []byte(`{"a": {"sessionId": "x"`), 0o644); err != nil {
		t.Fatal(err)
	}
	source := newJsonMapSource(def)
	if recs := source.List(); len(recs) != 0 {
		t.Fatalf("a corrupt index listed %d records", len(recs))
	}
	if err := source.ListError(); err == nil {
		t.Fatal("a corrupt sessions.json must be reported as a read failure, not an empty source")
	}

	// A valid but empty index is an empty source, not a failure
	if err := os.WriteFile(def.sessionsJSON, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	source.List()
	if err := source.ListError(); err != nil {
		t.Fatalf("an empty index is not an error: %v", err)
	}

	// A missing file is not an error either: the source is simply empty
	if err := os.Remove(def.sessionsJSON); err != nil {
		t.Fatal(err)
	}
	source.List()
	if err := source.ListError(); err != nil {
		t.Fatalf("a missing sessions.json is empty, not broken: %v", err)
	}
}

func TestOpenCodeListErrorOnUnreadableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(path, []byte("this is not a SQLite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := newOpenCodeSource(path)
	if recs := source.List(); len(recs) != 0 {
		t.Fatalf("a database that cannot be read listed %d records", len(recs))
	}
	if err := source.ListError(); err == nil {
		t.Fatal("a database that cannot be read must be reported, not shown as empty")
	}
}

func TestOpenClawListErrorOnUnreadableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openclaw-agent.sqlite")
	if err := os.WriteFile(path, []byte("this is not a SQLite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := newOpenClawSource([]string{path})
	if recs := source.List(); len(recs) != 0 {
		t.Fatalf("a database that cannot be read listed %d records", len(recs))
	}
	if err := source.ListError(); err == nil {
		t.Fatal("a database that cannot be read must be reported, not shown as empty")
	}
}

// TestCorruptIndexSurfacesInWarnings: the failure has to reach the surface a caller
// reads — /sessions carries warnings, and without it "no sessions" and "the source
// broke" are the same empty list.
func TestCorruptIndexSurfacesInWarnings(t *testing.T) {
	home := t.TempDir()
	def := openclawDef(home)
	if err := os.MkdirAll(filepath.Dir(def.sessionsJSON), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(def.sessionsJSON, []byte(`{"a":`), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := []SessionSource{newJsonMapSource(def)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)

	_, body := get(t, srv.URL+"/sessions", "")
	warnings, ok := body["warnings"].([]any)
	if !ok || len(warnings) != 1 {
		t.Fatalf("a corrupt index must surface as a warning, got %v", body["warnings"])
	}
	if first := warnings[0].(map[string]any); first["source"] != "openclaw" {
		t.Fatalf("the warning names %v, want openclaw", first["source"])
	}
}

// TestHealthWarningsRequireTokenWhenConfigured: /health answers without a token so a
// probe can read it, but the warnings it reports can name filesystem paths — with a
// token configured they are for authenticated eyes only.
func TestHealthWarningsRequireTokenWhenConfigured(t *testing.T) {
	home := t.TempDir()
	def := openclawDef(home)
	if err := os.MkdirAll(filepath.Dir(def.sessionsJSON), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(def.sessionsJSON, []byte(`{"a":`), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := []SessionSource{newJsonMapSource(def)}

	open := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2), maxConnections: 50,
	}))
	t.Cleanup(open.Close)
	// The warnings report the last scan's failure, so a list has to happen first
	get(t, open.URL+"/sessions", "")
	if _, body := get(t, open.URL+"/health", ""); body["warnings"] == nil {
		t.Fatal("without a token /health must show the warnings: the local operator has no other way to see them")
	}

	guarded := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2),
		token: "secret", maxConnections: 50,
	}))
	t.Cleanup(guarded.Close)
	code, body := get(t, guarded.URL+"/health", "")
	if code != 200 {
		t.Fatalf("the probe must still read /health without a token, got %d", code)
	}
	if _, has := body["warnings"]; has {
		t.Fatal("unauthenticated /health must not carry warnings")
	}
	if _, body := get(t, guarded.URL+"/health", "secret"); body["warnings"] == nil {
		t.Fatal("an authenticated /health keeps the warnings")
	}
}
