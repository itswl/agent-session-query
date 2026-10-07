package app

import (
	"bufio"
	"github.com/itswl/agent-session-query/internal/source"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newGuardServer(t *testing.T, token, corsOrigin string, loopbackOnly bool) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "s.jsonl"),
		`{"type":"session","id":"s1","cwd":"/w/proj"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
	)
	sources := []source.SessionSource{source.NewPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2),
		token: token, corsOrigin: corsOrigin, maxConnections: 50, loopbackOnly: loopbackOnly,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func doWithHost(t *testing.T, srv *httptest.Server, path, host, origin, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestHostGuardDNSRebinding: with no token on a loopback bind, a request whose Host is
// not a loopback name is refused. That is the shape a rebinding page sends — its own
// origin, no Origin header, Host: the attacker's name — which the README's stated
// defences (loopback bind, no CORS headers) do not stop on their own.
func TestHostGuardDNSRebinding(t *testing.T) {
	srv := newGuardServer(t, "", "", true)

	// The names a legitimate local page uses
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "localhost"} {
		if code := doWithHost(t, srv, "/sessions", host, "", ""); code != 200 {
			t.Fatalf("Host %s = %d, want 200", host, code)
		}
	}
	// A forged name is not one of them
	for _, host := range []string{"evil.example", "evil.example:8080", "localhost.evil.example"} {
		if code := doWithHost(t, srv, "/sessions", host, "", ""); code != 403 {
			t.Fatalf("Host %s = %d, want 403", host, code)
		}
	}
	// /health is guarded the same way: it carries source warnings and paths
	if code := doWithHost(t, srv, "/health", "evil.example", "", ""); code != 403 {
		t.Fatalf("forged Host on /health = %d, want 403", code)
	}

	// A configured token turns the Host check off: the attacker gains nothing without
	// the token, and a reverse proxy forwards the public Host
	tokened := newGuardServer(t, "secret", "", true)
	if code := doWithHost(t, tokened, "/sessions", "evil.example", "", "secret"); code != 200 {
		t.Fatalf("with a token the Host check steps aside: got %d", code)
	}
	if code := doWithHost(t, tokened, "/sessions", "evil.example", "", ""); code != 401 {
		t.Fatalf("without the token it is a 401, got %d", code)
	}

	// Bound elsewhere, the operator chose exposure; the check does not second-guess it
	exposed := newGuardServer(t, "", "", false)
	if code := doWithHost(t, exposed, "/sessions", "evil.example", "", ""); code != 200 {
		t.Fatalf("a non-loopback bind is the operator's call: got %d", code)
	}
}

// TestOriginGuardEverywhere: the Origin guard that used to front only /mcp now fronts
// every route, so a cross-origin fetch meets it before anything else.
func TestOriginGuardEverywhere(t *testing.T) {
	srv := newGuardServer(t, "secret", "", false)
	if code := doWithHost(t, srv, "/sessions", "", "https://evil.example", "secret"); code != 403 {
		t.Fatalf("a cross-origin request = %d, want 403", code)
	}
	if code := doWithHost(t, srv, "/sessions", "", "", "secret"); code != 200 {
		t.Fatalf("no Origin = %d, want 200", code)
	}

	allowed := newGuardServer(t, "", "https://ops.example", false)
	if code := doWithHost(t, allowed, "/sessions", "", "https://ops.example", ""); code != 200 {
		t.Fatalf("the allowed origin = %d, want 200", code)
	}
	if code := doWithHost(t, allowed, "/sessions", "", "https://evil.example", ""); code != 403 {
		t.Fatalf("a disallowed origin = %d, want 403", code)
	}
}

func TestHostIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "localhost": true, "LOCALHOST": true, "::1": true,
		"127.0.0.2": true, "0.0.0.0": false, "192.168.1.10": false, "example.com": false,
		// Decorated spellings are names to net.Listen, not the literal address, so they
		// must not pass the bind decision (they refuse without a token instead)
		"127.0.0.1.": false, "localhost.": false, "[::1]": false,
	} {
		if got := hostIsLoopback(host); got != want {
			t.Errorf("hostIsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

// rawRoundTrip sends raw bytes over TCP and returns the status line, so request shapes a
// Go client cannot produce — an empty Host, HTTP/1.0 with none — can be tested.
func rawRoundTrip(t *testing.T, srv *httptest.Server, request string) string {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

// TestHostGuardEmptyHost: the guard's invariant is "the Host is a loopback name"; an
// empty Host (HTTP/1.1 with a bare "Host:", or HTTP/1.0 with none) satisfies nothing and
// is refused rather than waved through as "not a browser".
func TestHostGuardEmptyHost(t *testing.T) {
	srv := newGuardServer(t, "", "", true)

	if got := rawRoundTrip(t, srv, "GET /sessions HTTP/1.1\r\nHost:\r\nConnection: close\r\n\r\n"); !strings.Contains(got, "403") {
		t.Fatalf("an empty Host = %q, want 403", got)
	}
	if got := rawRoundTrip(t, srv, "GET /sessions HTTP/1.0\r\n\r\n"); !strings.Contains(got, "403") {
		t.Fatalf("HTTP/1.0 without Host = %q, want 403", got)
	}
	// A loopback Host over HTTP/1.0 still works
	if got := rawRoundTrip(t, srv, "GET /sessions HTTP/1.0\r\nHost: localhost\r\n\r\n"); !strings.Contains(got, "200") {
		t.Fatalf("HTTP/1.0 with a loopback Host = %q, want 200", got)
	}
}
