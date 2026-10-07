package app

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/itswl/agent-session-query/internal/source"
)

// A source that cannot be read has to reach the surface a caller reads: /sessions carries
// warnings, /health repeats the last scan's verdict without scanning for itself, and
// without either, "no sessions" and "the source broke" are the same empty list.
//
// The failing source is an opencode database that is not one — the cheapest way to make a
// real source fail through its exported constructor. The source-side bookkeeping
// (listErrorReporter) is tested with the source package's own fixtures.
func newFailingSource(t *testing.T) (source.SessionSource, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(path, []byte("this is not a SQLite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	return source.NewOpenCodeSource(path), path
}

func TestListFailureSurfacesInWarnings(t *testing.T) {
	src, _ := newFailingSource(t)
	sources := []source.SessionSource{src}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "opencode", sources: sources, api: newSessionQueryAPI(sources, 0), maxConnections: 10,
	}))
	t.Cleanup(srv.Close)

	// /health is unauthenticated, so it reports what the last scan hit rather than scanning
	// for itself: with nothing scanned yet it has nothing to report
	if _, body := get(t, srv.URL+"/health", ""); body["warnings"] != nil {
		t.Fatalf("/health must not scan the sources to answer: %v", body["warnings"])
	}

	code, body := get(t, srv.URL+"/sessions", "")
	if code != 200 {
		t.Fatalf("/sessions = %d %v", code, body)
	}
	warnings, ok := body["warnings"].([]any)
	if !ok || len(warnings) != 1 {
		t.Fatalf("/sessions must carry the source failure: %v", body)
	}
	if first := warnings[0].(map[string]any); first["source"] != "opencode" {
		t.Fatalf("warning = %v", first)
	}

}

// TestHealthWarningsRequireTokenWhenConfigured: /health answers without a token so a probe
// can read it, but the warnings it reports can name filesystem paths — with a token
// configured they are for authenticated eyes only.
func TestHealthWarningsRequireTokenWhenConfigured(t *testing.T) {
	src, _ := newFailingSource(t)
	sources := []source.SessionSource{src}

	open := httptest.NewServer(newAPIServer(serverOptions{
		mode: "opencode", sources: sources, api: newSessionQueryAPI(sources, 0), maxConnections: 10,
	}))
	t.Cleanup(open.Close)
	// The warnings report the last scan's failure, so a list has to happen first
	get(t, open.URL+"/sessions", "")
	if _, body := get(t, open.URL+"/health", ""); body["warnings"] == nil {
		t.Fatal("without a token /health must show the warnings: the local operator has no other way to see them")
	}

	guarded := httptest.NewServer(newAPIServer(serverOptions{
		mode: "opencode", sources: sources, api: newSessionQueryAPI(sources, 0),
		token: "secret", maxConnections: 10,
	}))
	t.Cleanup(guarded.Close)
	get(t, guarded.URL+"/sessions", "secret")
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
