package server

// Who may ask.
//
// The record this server reads out is private: every prompt a person typed, every file
// an agent printed. There are two ways to run it and each has its own guard.
//
// Without a token it listens on loopback only — binding anywhere else is refused at
// start — and what protects it is that only this machine can connect. The hole in that
// is the browser: any page the person visits can make their browser connect to
// localhost. So a request must name a loopback Host (a DNS name rebound to 127.0.0.1
// does not), and must not come from another site (Origin, Sec-Fetch-Site).
//
// With a token, every request to the API proves it holds the token: an Authorization
// header for programs, a cookie for the page. A browser cannot keep a secret that script
// can read, so the page trades the token once (POST /api/login) for a cookie script
// never sees: HttpOnly, SameSite=Strict, good for 180 days. The cookie is stateless —
// an expiry and its HMAC under a key derived from the token — so nobody can mint or
// extend one, and changing the token retires them all.

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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cookieName     = "asq_session"
	cookieLifetime = 180 * 24 * time.Hour
	maxLoginBody   = 4 << 10

	// attemptsMax wrong tokens per client address per attemptsWindow, by any route: the
	// login form or an Authorization header. Past it the answer is 429 until the window
	// rolls over, which turns guessing a human-chosen token from a matter of minutes
	// into one of geological time.
	attemptsMax    = 10
	attemptsWindow = time.Minute
)

// signKey derives the cookie-signing key from the token, so that the two uses of the
// token — compared as a header, signing cookies — never share key material.
func signKey(token string) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("asq session cookie v1"))
	return mac.Sum(nil)
}

// sign builds a cookie value: the expiry in Unix seconds, a dot, and its HMAC.
func sign(key []byte, expires time.Time) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(exp))
	return exp + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// signed reports whether a cookie value is one sign made and has not expired.
func signed(key []byte, value string, now time.Time) bool {
	exp, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || now.After(time.Unix(expires, 0)) {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(exp))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

// limiter counts wrong tokens per client address in a fixed window. It holds one entry
// per address seen in the last window and drops the rest as it goes, so it stays bounded
// without a goroutine of its own.
type limiter struct {
	mu   sync.Mutex
	hits map[string]hit
}

type hit struct {
	started time.Time
	count   int
}

// blocked reports whether addr has used up its attempts, and for how long.
func (l *limiter) blocked(addr string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.hits[addr]
	if !ok || now.Sub(h.started) >= attemptsWindow || h.count < attemptsMax {
		return false, 0
	}
	return true, h.started.Add(attemptsWindow).Sub(now)
}

// failed records one wrong token from addr.
func (l *limiter) failed(addr string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string]hit{}
	}
	for key, h := range l.hits {
		if now.Sub(h.started) >= attemptsWindow {
			delete(l.hits, key)
		}
	}
	h := l.hits[addr]
	if h.count == 0 {
		h.started = now
	}
	h.count++
	l.hits[addr] = h
}

// clientAddr is the address an attempt is charged to: the connection's own, port
// stripped. X-Forwarded-For is not read — it is whatever the client says it is, and
// trusting it would hand every guess a fresh address. Behind a proxy all callers then
// share one allowance, which is the conservative way for that to fail.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// bearer is the token a request presents in its Authorization header.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:]), true
	}
	return "", false
}

type verdict int

const (
	allowed verdict = iota
	unauthorized
	throttled
)

// authorize decides whether a request may read the record.
func (s *Server) authorize(r *http.Request) (verdict, time.Duration) {
	if s.opts.Token == "" {
		return allowed, 0 // loopback only; the guards in front already ran
	}
	now := time.Now()
	if c, err := r.Cookie(cookieName); err == nil && signed(s.key, c.Value, now) {
		return allowed, 0
	}
	given, ok := bearer(r)
	if !ok {
		return unauthorized, 0
	}
	addr := clientAddr(r)
	if blocked, wait := s.attempts.blocked(addr, now); blocked {
		return throttled, wait
	}
	if subtle.ConstantTimeCompare([]byte(given), []byte(s.opts.Token)) == 1 {
		return allowed, 0
	}
	s.attempts.failed(addr, now)
	return unauthorized, 0
}

// isLoopbackHost reports whether a Host header names this machine by a name that cannot
// be made to mean it from outside.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsLoopback reports whether a listen address accepts connections from this machine only.
func IsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	return ip != nil && ip.IsLoopback()
}

// sameOrigin reports whether an Origin header names the address this request came to.
func sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *Server) originListed(origin string) bool {
	for _, o := range s.opts.Origins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// guard is what stands in front of every route: it reports why a request is refused, or
// "" when it may go on. It runs before authentication and does not depend on it.
func (s *Server) guard(r *http.Request) string {
	if s.opts.Token == "" && !isLoopbackHost(r.Host) {
		// Only this machine can connect, so a Host that is not a loopback name is a name
		// someone pointed at 127.0.0.1 to borrow this machine's browser
		return "this server answers on its loopback address only: open it as http://localhost or http://127.0.0.1"
	}
	origin := r.Header.Get("Origin")
	if origin != "" && origin != "null" && !sameOrigin(r, origin) && !s.originListed(origin) {
		return "requests from " + origin + " are not accepted (--cors-origin lists the origins that are)"
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/mcp" {
		return "" // the page itself may be linked to from anywhere: it holds nothing
	}
	// A browser says when a request was started by another site, even one that carries
	// no Origin — an <img>, a <script>. Nothing here is theirs to fetch.
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" && !(origin != "" && s.originListed(origin)) {
		return "cross-site requests are not accepted"
	}
	if origin == "null" && !s.originListed("null") {
		return "requests from an opaque origin are not accepted"
	}
	return ""
}

// cors answers a listed origin with leave to read the reply. Cookies are never part of
// it: a page on another origin authenticates with the header, like any other program.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || sameOrigin(r, origin) || !s.originListed(origin) {
		return
	}
	h := w.Header()
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version, Last-Event-ID")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Max-Age", "600")
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	v, _ := s.authorize(r)
	writeJSON(w, r, http.StatusOK, map[string]any{"required": s.opts.Token != "", "authenticated": v == allowed})
}

// handleLogin trades the token for the cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.opts.Token == "" {
		writeError(w, r, http.StatusConflict, "this server has no token: there is nothing to sign in to")
		return
	}
	addr, now := clientAddr(r), time.Now()
	if blocked, wait := s.attempts.blocked(addr, now); blocked {
		tooMany(w, r, wait)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBody)).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "that is not a login")
			return
		}
		writeError(w, r, http.StatusBadRequest, `send the token as JSON: {"token": "…"}`)
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.opts.Token)) != 1 {
		s.attempts.failed(addr, now)
		writeError(w, r, http.StatusUnauthorized, "that is not the token")
		return
	}
	expires := now.Add(cookieLifetime)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: sign(s.key, expires), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r),
		Expires: expires.UTC(), MaxAge: int(cookieLifetime / time.Second),
	})
	writeJSON(w, r, http.StatusOK, map[string]any{"ok": true, "expires": expires.UTC()})
}

// handleLogout drops the cookie. There is no session on this side to end — the cookie
// certifies itself — and script cannot clear an HttpOnly cookie on its own.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r),
		Expires: time.Unix(0, 0).UTC(), MaxAge: -1,
	})
	writeJSON(w, r, http.StatusOK, map[string]any{"ok": true})
}

// isHTTPS reports whether the browser reached this server over TLS, here or at a proxy
// in front. Only the cookie's Secure attribute hangs on the answer: a proxy that lies
// about it can make a browser refuse the cookie, and nothing more.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func tooMany(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	secs := max(1, int(wait.Round(time.Second).Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, r, http.StatusTooManyRequests, fmt.Sprintf("too many wrong tokens from this address: try again in %ds", secs))
}
