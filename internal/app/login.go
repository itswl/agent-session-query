package app

// Login: the one token this service takes, in the form a browser can use safely.
//
// A browser cannot keep a secret: a Bearer token in JavaScript's reach (localStorage) is
// one XSS away from being read out. So with a token configured, POST /login accepts it —
// the one request that ever carries it from a page — and answers with an app_session
// cookie: HttpOnly, so script never reads it back out; SameSite=Lax, so another origin's
// page cannot make the browser spend it; valid for 180 days. checkAuth takes that cookie
// exactly as it takes the Authorization header, and the header form keeps working for
// everything that is not a browser (curl, MCP clients, scripts).
//
// The cookie is stateless — no session store, no user table. Its value is an expiry
// timestamp plus the HMAC-SHA256 of that timestamp under a key derived from the token,
// so a holder cannot extend itself and nobody else can mint one. Rotating the token
// retires every outstanding cookie at once; short of that a cookie lives out its 180
// days.
//
// /login is rate-limited (10 attempts a minute per client IP) and its body capped at
// 4 KiB. That does not make the token unguessable everywhere — the Bearer form is
// checkable on any route at whatever rate the network allows, as it always was — but it
// does keep the login endpoint itself from being hammered, which is where a weak,
// human-chosen token would be tried first.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// loginPath / logoutPath: the cookie endpoints, the only POST routes beside /mcp.
	loginPath  = "/login"
	logoutPath = "/logout"

	// sessionCookieName is the cookie POST /login sets and checkAuth looks for.
	sessionCookieName = "app_session"

	// sessionLifetime is how long a login holds: 180 days from the moment it was issued.
	sessionLifetime = 180 * 24 * time.Hour

	// maxLoginBodyBytes caps the login request body at 4 KiB. The body is one JSON
	// object with a secret in it; anything larger is not a login, and reading on
	// without a cap would commit memory to whatever a stranger shipped.
	maxLoginBodyBytes = 4 << 10

	// loginRateMax / loginRateWindow: at most 10 login attempts per client IP per
	// minute, counting successes and failures alike. The window is fixed, so a burst
	// gets exactly 10 and the 11th waits for the window to roll over.
	loginRateMax    = 10
	loginRateWindow = time.Minute
)

// sessionSignKey derives the cookie-signing key from the token. The cookie never
// carries the token itself, and deriving the key through one more HMAC gives the two
// uses of the token — compared as a Bearer header, signing cookies — separate key
// material, so a signature is not evidence of the token and the token does not sign as
// a cookie key anywhere else it might be used. Changing the token changes the key,
// which retires every outstanding cookie at once.
func sessionSignKey(token string) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("app_session cookie signing v1"))
	return mac.Sum(nil)
}

// signSessionValue builds a cookie value: the expiry as decimal Unix seconds, a dot,
// and the base64url HMAC-SHA256 of that expiry. Nothing is encrypted — an expiry is not
// secret — but the signature means a holder of the cookie cannot push its own expiry
// forward, and nobody without the key can mint a value at all.
func signSessionValue(key []byte, expires time.Time) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(exp))
	return exp + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validSessionValue checks a cookie value against the key that signed it: well-formed,
// signature intact, and not past its expiry.
func validSessionValue(key []byte, value string, now time.Time) bool {
	expStr, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || now.After(time.Unix(expires, 0)) {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(expStr))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	// hmac.Equal is constant-time; a plain == would answer "no" at the first
	// differing byte and leak how many bytes a forgery got right
	return hmac.Equal([]byte(want), []byte(sig))
}

// loginLimiter counts login attempts per client IP in a fixed one-minute window.
//
// It follows the mutex-guarded-counter pattern of the rest of apiServer rather than
// anything fancier: the map holds one entry per IP seen in the last minute, and allow
// itself drops the windows that have ended, so the limiter stays bounded without a
// background goroutine.
type loginLimiter struct {
	mu sync.Mutex
	// hits maps a client IP to its current window; nil until the first attempt
	hits map[string]loginHit
}

// loginHit is one IP's current fixed window
type loginHit struct {
	started time.Time
	count   int
}

// allow records one attempt from ip and reports whether it fits the budget, plus how
// long until the window — and with it the budget — resets, which is the Retry-After a
// refusal should carry. Every attempt counts, successful or not: a minted cookie is
// worth at least as much as a guess.
func (l *loginLimiter) allow(ip string, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = make(map[string]loginHit)
	}
	// Drop the windows that have ended. The map then holds exactly the IPs of the last
	// minute, whatever the caller traffic looks like.
	for key, hit := range l.hits {
		if now.Sub(hit.started) >= loginRateWindow {
			delete(l.hits, key)
		}
	}
	hit := l.hits[ip]
	if now.Sub(hit.started) >= loginRateWindow {
		hit = loginHit{started: now}
	}
	hit.count++
	l.hits[ip] = hit
	if hit.count <= loginRateMax {
		return true, 0
	}
	return false, hit.started.Add(loginRateWindow).Sub(now)
}

// clientIP is the address a login is charged to: the connection's own remote address,
// port stripped.
//
// X-Forwarded-For is deliberately not read — it is client-supplied, and trusting it
// would let one client present a new "IP" per request and walk around the limit
// entirely. The cost is that behind a reverse proxy every caller shares one bucket; for
// a single-secret service that is the conservative failure, and a proxy that should
// rate-limit with real client addresses can do so in front.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleLogin exchanges the token for a session cookie.
//
// The request is JSON — {"token": "the token"} — and the answer is a Set-Cookie for
// app_session plus the moment it expires. Reachable without authentication (it is how
// authentication starts), rate-limited per client IP, and body-capped.
func (s *apiServer) handleLogin(w http.ResponseWriter, r *http.Request) int {
	if s.token == "" {
		// Not 404: the route is advertised, and "not enabled here" is the honest
		// answer to a caller that followed the advertisement.
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "login is not enabled: set --hook_token / HOOK_TOKEN"})
		return http.StatusForbidden
	}

	if ok, retryAfter := s.logins.allow(clientIP(r), time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Round(time.Second).Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": fmt.Sprintf("too many login attempts; try again in %s", retryAfter.Round(time.Second)),
		})
		return http.StatusTooManyRequests
	}

	// The 4 KiB cap answers over-large bodies with 413 rather than reading on
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": fmt.Sprintf("login body is over the %d byte limit", maxLoginBodyBytes),
			})
			return http.StatusRequestEntityTooLarge
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `login body must be JSON: {"token": "..."}`})
		return http.StatusBadRequest
	}

	// Constant-time, as with the Bearer token: a bytewise compare leaks the token
	// through timing. (An absent or empty field simply never equals a set token.)
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.token)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid token"})
		return http.StatusUnauthorized
	}

	expires := time.Now().Add(sessionLifetime)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    signSessionValue(s.sessionKey, expires),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure only when this very request is TLS: the default bind is plain HTTP on
		// loopback, and a Secure cookie the browser refuses to keep would break login
		// there. Behind a TLS-terminating proxy r.TLS is nil — have the proxy add the
		// Secure attribute, or terminate TLS here.
		Secure:  r.TLS != nil,
		Expires: expires.UTC(),
		MaxAge:  int(sessionLifetime / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"cookie":  sessionCookieName,
		"expires": expires.UTC().Format(time.RFC3339),
	})
	return http.StatusOK
}

// handleLogout clears the session cookie. There is no server-side session to end — the
// cookie certifies itself — so logging out is the browser dropping it, and this endpoint
// exists because script cannot clear an HttpOnly cookie on its own. Signing in again
// afterwards is just another /login; the rate limit treats it like any other attempt.
func (s *apiServer) handleLogout(w http.ResponseWriter, r *http.Request) int {
	if s.token == "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "login is not enabled: set --hook_token / HOOK_TOKEN"})
		return http.StatusForbidden
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// The epoch is in the past for every clock; MaxAge < 0 Go renders as
		// Max-Age=0, which every browser reads as "delete it"
		Expires: time.Unix(0, 0).UTC(),
		MaxAge:  -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return http.StatusOK
}
