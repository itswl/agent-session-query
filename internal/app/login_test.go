package app

// Tests for the login flow: the one token this service takes, exchanged at POST /login
// for an HttpOnly app_session cookie that then carries every authenticated route
// alongside the Bearer header form.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/source"
)

// newLoginServer builds a server around one fixture session. The apiServer itself comes
// back too, for tests that need to mint a cookie with it.
func newLoginServer(t *testing.T, token string) (*httptest.Server, *apiServer) {
	t.Helper()
	root := t.TempDir()
	sessionPath := filepath.Join(root, "p", "2026-01-01T00-00-00_abc.jsonl")
	write(t, sessionPath,
		`{"type":"session","id":"sess-1","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"question"}]}}`,
	)
	sources := []source.SessionSource{source.NewPiSource(root)}
	api := newSessionQueryAPI(sources, 2)
	server := newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: api, token: token, maxConnections: 50,
	})
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	return srv, server
}

// postLogin posts a JSON body and leaves the response unread beyond the headers
func postLogin(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// sessionCookieFrom picks the app_session cookie out of a response
func sessionCookieFrom(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in the response: %v", sessionCookieName, resp.Header.Values("Set-Cookie"))
	return nil
}

// getWithCookie requests a URL carrying a cookie instead of a Bearer token
func getWithCookie(t *testing.T, url string, cookie *http.Cookie) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestLoginIssuesSessionCookie(t *testing.T) {
	srv, _ := newLoginServer(t, "s3cret")

	resp := postLogin(t, srv.URL+loginPath, `{"token":"s3cret"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /login = %d, want 200", resp.StatusCode)
	}
	cookie := sessionCookieFrom(t, resp)
	if !cookie.HttpOnly {
		t.Error("the session cookie is not HttpOnly: script could read it back out")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("cookie Path = %q, want \"/\" (every authenticated route takes it)", cookie.Path)
	}
	if want := int(sessionLifetime / time.Second); cookie.MaxAge != want {
		t.Errorf("cookie Max-Age = %d, want %d (180 days)", cookie.MaxAge, want)
	}
	// The cookie carries no trace of the token it was issued for
	if strings.Contains(cookie.Value, "s3cret") {
		t.Fatalf("the cookie value carries the token itself: %q", cookie.Value)
	}

	// The cookie, not the token, carries the authenticated routes afterwards
	if code := getWithCookie(t, srv.URL+"/sessions", cookie); code != http.StatusOK {
		t.Fatalf("GET /sessions with the session cookie = %d, want 200", code)
	}
	// And the /api prefix form too — one gate, checkAuth, serves both
	if code := getWithCookie(t, srv.URL+"/api/sessions", cookie); code != http.StatusOK {
		t.Fatalf("GET /api/sessions with the session cookie = %d, want 200", code)
	}
	// The same token keeps its header form: one token, two channels
	if code, _ := get(t, srv.URL+"/sessions", "s3cret"); code != http.StatusOK {
		t.Fatalf("GET /sessions with Bearer token = %d, want 200", code)
	}
}

func TestLoginRejectsWrongToken(t *testing.T) {
	srv, _ := newLoginServer(t, "s3cret")

	for _, body := range []string{
		`{"token":"wrong"}`,
		`{"token":""}`,
		`{}`,
	} {
		resp := postLogin(t, srv.URL+loginPath, body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST /login with %s = %d, want 401", body, resp.StatusCode)
		}
		if len(resp.Header.Values("Set-Cookie")) > 0 {
			t.Errorf("a refused login set a cookie: %v", resp.Header.Values("Set-Cookie"))
		}
	}
}

func TestLoginNotEnabledWithoutToken(t *testing.T) {
	// The advertised route answers honestly when the operator set no token
	srv, _ := newLoginServer(t, "")
	for _, path := range []string{loginPath, logoutPath} {
		resp := postLogin(t, srv.URL+path, `{"token":"anything"}`)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s with no token configured = %d, want 403", path, resp.StatusCode)
		}
	}
}

func TestLoginRejectsBadBodies(t *testing.T) {
	srv, _ := newLoginServer(t, "s3cret")

	// Over the 4 KiB cap: refused as 413, not read to the end
	big := `{"token":"` + strings.Repeat("a", maxLoginBodyBytes) + `"}`
	if resp := postLogin(t, srv.URL+loginPath, big); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a %d byte login body answers %d, want 413", len(big), resp.StatusCode)
	}

	// Not JSON, or not the JSON shape the endpoint takes
	for _, body := range []string{
		``,
		`not json`,
		`{"token":`,
		`[1, 2, 3]`,
		`{"token": 123}`,
	} {
		if resp := postLogin(t, srv.URL+loginPath, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /login with body %q = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	srv, _ := newLoginServer(t, "s3cret")

	// Ten attempts fit the budget — wrong token, so they also prove failures count
	for i := 0; i < loginRateMax; i++ {
		if resp := postLogin(t, srv.URL+loginPath, `{"token":"wrong"}`); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401 (the first ten fit the budget)", i+1, resp.StatusCode)
		}
	}
	// The eleventh, even with the right token, waits for the window to roll over
	resp := postLogin(t, srv.URL+loginPath, `{"token":"s3cret"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attempt %d = %d, want 429", loginRateMax+1, resp.StatusCode)
	}
	if retry := resp.Header.Get("Retry-After"); retry == "" {
		t.Error("the 429 carries no Retry-After")
	} else if seconds, err := strconv.Atoi(retry); err != nil || seconds < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", retry)
	}
}

func TestLoginRateLimitPerIP(t *testing.T) {
	// The budget is per client IP and the window rolls over: one loud stranger does not
	// silence the house, and a minute later even the stranger is heard again
	var limiter loginLimiter
	now := time.Now()
	for i := 0; i < loginRateMax; i++ {
		if ok, _ := limiter.allow("10.0.0.1", now); !ok {
			t.Fatalf("attempt %d from 10.0.0.1 refused inside the budget", i+1)
		}
	}
	if ok, _ := limiter.allow("10.0.0.1", now.Add(time.Second)); ok {
		t.Error("the 11th attempt from the same IP inside the window was allowed")
	}
	if ok, _ := limiter.allow("10.0.0.2", now.Add(time.Second)); !ok {
		t.Error("a different IP was refused for 10.0.0.1's noise")
	}
	if ok, _ := limiter.allow("10.0.0.1", now.Add(loginRateWindow+time.Second)); !ok {
		t.Error("the window never rolled over: a minute later the IP is still muted")
	}
}

func TestSessionValueSigning(t *testing.T) {
	key := sessionSignKey("s3cret")
	otherKey := sessionSignKey("rotated")
	now := time.Now()
	valid := signSessionValue(key, now.Add(sessionLifetime))

	if !validSessionValue(key, valid, now) {
		t.Error("a freshly signed value was rejected")
	}
	if validSessionValue(otherKey, valid, now) {
		t.Error("a value signed by the old token still validates after rotation")
	}
	if validSessionValue(key, signSessionValue(key, now.Add(-time.Second)), now) {
		t.Error("an expired value was accepted")
	}

	// Tampering: a moved expiry without a new signature, a dented signature, and junk.
	// The dent swaps the FIRST character of the signature on purpose: the last character
	// of an unpadded base64 string carries only two significant bits, so swapping it can
	// decode back to the identical signature — a "tampered" value that is genuinely valid,
	// which the Windows runner accepted one run in eight before this comment existed.
	exp, sig, _ := strings.Cut(valid, ".")
	dented := "A" + sig[1:]
	if sig[0] == 'A' {
		dented = "B" + sig[1:]
	}
	for _, tampered := range []string{
		exp + "0." + sig,    // expiry pushed forward, signature untouched
		exp + "." + dented,  // a character of the signature swapped (one whose bits count)
		"",                  // empty
		"noseparator",       // not exp.sig at all
		"abc.def",           // not a number for an expiry
		"9999999999." + sig, // a far future expiry with someone's signature
	} {
		if validSessionValue(key, tampered, now) {
			t.Errorf("tampered cookie value %q was accepted", tampered)
		}
	}
}

func TestExpiredSessionCookieRejected(t *testing.T) {
	srv, server := newLoginServer(t, "s3cret")

	// Minted with the server's own key but already past its moment
	expired := &http.Cookie{
		Name:  sessionCookieName,
		Value: signSessionValue(server.sessionKey, time.Now().Add(-time.Minute)),
	}
	if code := getWithCookie(t, srv.URL+"/sessions", expired); code != http.StatusUnauthorized {
		t.Fatalf("GET /sessions with an expired session cookie = %d, want 401", code)
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	srv, _ := newLoginServer(t, "s3cret")

	resp := postLogin(t, srv.URL+logoutPath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /logout = %d, want 200", resp.StatusCode)
	}
	cookie := sessionCookieFrom(t, resp)
	if cookie.MaxAge >= 0 {
		t.Errorf("logout cookie MaxAge = %d, want negative (delete it)", cookie.MaxAge)
	}
	if !cookie.Expires.Before(time.Now().Add(-time.Hour)) {
		t.Errorf("logout cookie Expires = %v, want long past (every clock agrees it is dead)", cookie.Expires)
	}
}

func TestLoginOriginGuards(t *testing.T) {
	srv, _ := newLoginServer(t, "s3cret")

	// The bundled /ui posts /login same-origin; a browser puts Origin on every POST, so
	// that case must pass. Another origin's page is still refused.
	req, err := http.NewRequest(http.MethodPost, srv.URL+loginPath, strings.NewReader(`{"token":"s3cret"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", srv.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Errorf("same-origin POST /login = %d, want 200", resp.StatusCode)
	}

	// A second server: this one's attempt budget is untouched by the cross-origin probe
	other, _ := newLoginServer(t, "s3cret")
	req, err = http.NewRequest(http.MethodPost, other.URL+loginPath, strings.NewReader(`{"token":"s3cret"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST /login = %d, want 403", resp.StatusCode)
	}
}

func TestLoginHealthAndCapabilities(t *testing.T) {
	// /health is how the page learns which gate to show: with a token set,
	// authentication is required and the login flow is advertised
	srv, _ := newLoginServer(t, "s3cret")
	code, body := get(t, srv.URL+"/health", "")
	if code != http.StatusOK || body["authRequired"] != true {
		t.Fatalf("/health with a token set = %d %v, want authRequired=true", code, body["authRequired"])
	}
	caps, ok := body["capabilities"].([]any)
	if !ok {
		t.Fatalf("/health capabilities = %v", body["capabilities"])
	}
	found := false
	for _, c := range caps {
		if c == "auth.login" {
			found = true
		}
	}
	if !found {
		t.Errorf("/health capabilities lack auth.login: %v", caps)
	}

	// Without a token the login flow is not advertised
	plain, _ := newLoginServer(t, "")
	_, body = get(t, plain.URL+"/health", "")
	for _, c := range body["capabilities"].([]any) {
		if c == "auth.login" {
			t.Errorf("/health advertises auth.login with no token set: %v", body["capabilities"])
		}
	}
}
