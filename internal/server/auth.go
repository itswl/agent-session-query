package server

// Who may ask.
//
// The record this server reads out is private: every prompt a person typed, every file
// an agent printed. There are two ways to run it and each has its own guard.
//
// Without a token it listens on loopback only — binding anywhere else is refused at
// start — and what protects it is that only this machine can connect. There are two
// holes in that. One is the browser: any page the person visits can make their browser
// connect to localhost. So a request must name a loopback Host (a DNS name rebound to
// 127.0.0.1 does not), and must not come from another site (Origin, Sec-Fetch-Site). The
// other is a proxy or a tunnel on this machine: it connects from loopback, writes
// whatever Host it is told to, and passes on what the whole network sends. So a request
// that bears a proxy's mark is refused (see relayed), and a server told of a proxy in
// front (Options.ProxyHeader) does not start.
//
// With a token, every request to the API proves it holds the token: an Authorization
// header for programs, a cookie for the page. A browser cannot keep a secret that script
// can read, so the page trades the token once (POST /api/login) for a cookie script
// never sees: HttpOnly, SameSite=Strict, good for 180 days. The cookie is an expiry and
// its HMAC, and nothing about it is kept on this side, so nobody can mint or extend one.
// The key it is signed with is made from the token and from a secret only this server
// holds: changing the token retires every cookie, and a forged cookie is refused
// whatever token its forger had in mind (see signKey).
//
// A wrong token is counted against whoever offered it, and ten in a minute shut that
// caller out for the rest of the minute. Who "that caller" is takes more saying than it
// seems to: see caller. What the guards turn away, and who signs in, is written to the
// log and counted (see door).

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cookieName     = "asq_session"
	cookieLifetime = 180 * 24 * time.Hour
	maxLoginBody   = 4 << 10

	// attemptsMax wrong tokens per caller per attemptsWindow, by either route a token is
	// offered on: the login form or an Authorization header. Past it the answer is 429
	// until the window rolls over, which turns guessing a human-chosen token from a
	// matter of minutes into one of geological time.
	attemptsMax    = 10
	attemptsWindow = time.Minute
	// proxyMax wrong tokens per attemptsWindow, together, from every caller a proxy names:
	// ten callers' worth.
	proxyMax = 10 * attemptsMax
	// attemptsKept callers are remembered at once; past it the rest share an allowance of
	// their kind. A flood of addresses costs the server a table of this size and no more.
	attemptsKept = 4096
	// knownFor is how long a caller that has shown the right token is known: neither a
	// spent proxy allowance nor a full table holds it back. knownKept of them at once.
	knownFor  = 24 * time.Hour
	knownKept = 256
	// knownNoted is how stale a known caller's time may grow before showing the token
	// again is written down (see keepKnown): a caller asking all day is kept at most hourly.
	knownNoted = time.Hour
	knownFile  = "known.json"
	// knownRetry is how long after a failed write of the callers known the next is tried.
	knownRetry = time.Minute

	secretFile = "session.key"
	secretSize = 32
)

// signKey makes the key cookies are signed with, from the token and a secret only this
// server holds.
//
// The secret is what keeps a cookie from being a question about the token. Were the key
// made from the token alone, a cookie signed under a guess would be accepted exactly when
// the guess is right: an Authorization header by another name, and one that no allowance
// counts. With the secret in the key, a forged cookie is refused whatever was guessed.
// The token is in it too, so that changing the token still retires every cookie.
func signKey(secret []byte, token string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("asq session cookie v2\x00"))
	mac.Write([]byte(token))
	return mac.Sum(nil)
}

// loadSecret reads the secret kept in dir, making it the first time: 32 random bytes in
// a file only its owner reads. Servers that start together end up holding the same one,
// whichever was written first.
//
// The file must be this user's alone, and the directory this user's. A secret another
// user could read, or a directory anyone could write a secret of their choosing into — a
// state directory that fell back to a shared temporary one — is no secret: the file is
// replaced, and the directory refused. A directory its group may write is not refused:
// that is what `mkdir` makes under the umask of a system that gives each user a group
// of their own.
func loadSecret(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(dir); err != nil {
		return nil, err
	} else if owned, known := ownedHere(info); known && (!owned || info.Mode().Perm()&0o002 != 0) {
		return nil, fmt.Errorf("%s is not this user's to keep a secret in (mode %v)", dir, info.Mode().Perm())
	}
	path := filepath.Join(dir, secretFile)
	for try := 0; try < 40; try++ {
		have, err := os.ReadFile(path)
		if err == nil && len(have) == secretSize {
			if info, serr := os.Stat(path); serr == nil {
				if owned, known := ownedHere(info); known && (!owned || info.Mode().Perm()&0o077 != 0) {
					// Readable by others, or someone else's: it may be known. A new one
					// signs everyone out, which is the point
					if err := os.Remove(path); err != nil {
						return nil, err
					}
					continue
				}
			}
		}
		switch {
		case err == nil && len(have) == secretSize:
			return have, nil
		case err == nil && try < 10:
			// Not whole: another server is writing it this instant
			time.Sleep(20 * time.Millisecond)
			continue
		case err == nil:
			// Not whole and nobody is finishing it: it is no secret, and is replaced
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
		secret := make([]byte, secretSize)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue // another server made it first: theirs is the one
		}
		if err != nil {
			return nil, err
		}
		_, err = f.Write(secret)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
			return nil, err
		}
		return secret, nil
	}
	return nil, fmt.Errorf("%s is neither whole nor left alone", path)
}

// cookieSecret is the secret this server signs with: the one kept in its state
// directory, or — with no directory, or one that cannot be used — one made for this run.
// cookieSecret is the secret cookies are signed with, and whether the state directory
// keeps it — and so may keep the callers known as well (see keepKnown).
func (s *Server) cookieSecret() ([]byte, bool) {
	if dir := s.opts.State; dir != "" {
		secret, err := loadSecret(dir)
		if err == nil {
			return secret, true
		}
		s.logf("sign-ins will not outlive this run: the secret that signs them could not be kept in %s: %v", dir, err)
	}
	secret := make([]byte, secretSize)
	if _, err := rand.Read(secret); err != nil {
		panic(err) // no randomness, no server
	}
	return secret, false
}

// The callers known — those that showed the right token in the last knownFor — outlive a
// restart: kept in the state directory beside the secret, under the same rules, and read
// back when the server starts. Without them a restart made every caller a newcomer, and
// while someone kept a proxy's allowance spent, a remote agent that had asked a minute
// before was shut out until they stopped.
//
// They are known by a token, and are not by the next: the file says which (For), and a
// server with another token starts with nobody known — as a sign-in, a caller that held
// the old token is a stranger once it changes.
type keptKnown struct {
	For   string               `json:"for"`
	Known map[string]time.Time `json:"known"`
}

// knownMark names the token the callers are known by, in two bytes of what the key cookies
// are signed with makes of it. Two are enough that a new token finds the callers of the
// last strangers, but for one change in 65536 — and too few for the file and the secret
// beside it to confirm a guess at the token: one guess in 65536 matches, and a match is
// still only a guess to try against the server, at the pace it allows.
func (s *Server) knownMark() string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("asq callers known v1"))
	return hex.EncodeToString(mac.Sum(nil)[:2])
}

// loadKnown reads the callers known when the server last kept them: those still within
// knownFor, from a file this user owns and nobody else can read or write.
func (s *Server) loadKnown(now time.Time) {
	path := filepath.Join(s.opts.State, knownFile)
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if owned, known := ownedHere(info); known && (!owned || info.Mode().Perm()&0o077 != 0) {
		s.logf("the callers known were not read: %s is not this user's alone (mode %v)", path, info.Mode().Perm())
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var kept keptKnown
	if json.Unmarshal(raw, &kept) != nil {
		return
	}
	if !hmac.Equal([]byte(kept.For), []byte(s.knownMark())) {
		return // known by another token, or another secret: nobody is, until they show this one
	}
	s.attempts.mu.Lock()
	defer s.attempts.mu.Unlock()
	if s.attempts.hits == nil {
		s.attempts.hits, s.attempts.known = map[string]hit{}, map[string]time.Time{}
	}
	for key, at := range kept.Known {
		if now.Sub(at) < knownFor && len(s.attempts.known) < knownKept {
			s.attempts.known[key] = at
		}
	}
}

// keepKnown writes the callers known down, when that moved: a new one, or one whose time
// grew more than knownNoted stale. Written whole to a file of the owner's alone, and put
// in place in one step, so that a server starting meanwhile reads the old set or the new.
func (s *Server) keepKnown() {
	if !s.keepsKnown {
		return
	}
	// One writer at a time, from taking the set to putting it in place: two that took it
	// in one order and wrote it in the other would leave the older set in the file
	s.keepMu.Lock()
	defer s.keepMu.Unlock()
	// A write that failed is tried again, but not on every request while the directory
	// stays as it is
	if time.Since(s.keepFailed) < knownRetry {
		return
	}
	s.attempts.mu.Lock()
	if !s.attempts.noted {
		s.attempts.mu.Unlock()
		return
	}
	s.attempts.noted = false
	kept := keptKnown{For: s.knownMark(), Known: maps.Clone(s.attempts.known)}
	s.attempts.mu.Unlock()
	raw, err := json.Marshal(kept)
	if err != nil {
		return
	}
	path := filepath.Join(s.opts.State, knownFile)
	failed := func(why string) {
		s.attempts.mu.Lock()
		s.attempts.noted = true // still to be written
		s.attempts.mu.Unlock()
		s.keepFailed = time.Now()
		s.logf("the callers known could not be kept in %s: %s", path, why)
	}
	tmp, err := os.CreateTemp(s.opts.State, knownFile+".*")
	if err != nil {
		failed(err.Error())
		return
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Chmod(tmp.Name(), 0o600) != nil || os.Rename(tmp.Name(), path) != nil {
		os.Remove(tmp.Name())
		failed("the file could not be written and put in place")
		return
	}
	s.keepFailed = time.Time{}
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

// An allowance is what a caller may get wrong in a window. Who "a caller" is decides
// whether the limit does its work, and whom it hurts when it does:
//
//   - The programs on this machine — the agents asking over HTTP, a script with curl —
//     are one caller, "local": a loopback connection with no forwarding header and no
//     mark of a browser. Nothing that arrives through a proxy and nothing a web page
//     does is charged to it, so neither can shut the agents out.
//   - A page in a browser on this machine is another, "local browser": a loopback
//     connection whose request carries what a browser adds and script cannot remove
//     (Sec-Fetch-Site, Origin). A page that rebinds a name of its own to 127.0.0.1 is
//     such a page. Node's fetch, which MCP clients use, sends neither.
//   - What a proxy relays is charged apart from what reached the port directly. When
//     the proxy says who is calling (Options.ProxyHeader), and is one to believe, each
//     caller behind it has an allowance of its own — kept by the address the header
//     names and the proxy that wrote it, so that naming an address through one proxy
//     spends nothing of what it has through another, or directly — and all of them
//     together also draw on one larger allowance per proxy. A caller who can choose what the header says — a page that reaches the
//     proxy's port, a device on the network — would otherwise take a fresh allowance
//     with every guess. A proxy on this machine is one proxy whichever loopback address
//     it connects from: on Linux a process may connect from any of 127.0.0.0/8.
//   - An IPv6 caller holds a /64, and is charged as one.
//   - A caller that has shown the right token is known for a while (knownFor), and what
//     others spend does not hold it back: neither its proxy's allowance nor a full
//     table, only its own allowance. Callers guessing through a proxy can keep newcomers
//     out for as long as they keep at it, and nobody who is already in — short of
//     writing the header through the same proxy, naming them, and spending their own.
//     A known caller is its address through that proxy: a device that can write the
//     header and connects some other way is a proxy of its own, capped like any other.
//     Who is known is kept in the state directory, and outlives a restart (see
//     keepKnown).
//   - The table of callers is bounded. Past attemptsKept, the callers it does not know
//     share one allowance of their kind. The callers on this machine are always known by
//     name, and never share it.
type allowance struct {
	key   string // whose it is
	kind  string // the shared allowance it falls back on when the table is full; "" for none
	proxy string // the allowance it also draws on, with every caller behind the same proxy
}

// limiter counts wrong tokens per allowance in a fixed window, and remembers the callers
// that showed the right one. It needs no goroutine of its own: an entry that has
// outlived its window counts for nothing wherever it is read, and the table is swept as
// attempts come in — once a window, or once a second while it is full.
type limiter struct {
	mu    sync.Mutex
	hits  map[string]hit
	swept time.Time
	known map[string]time.Time // when each caller known last showed the right token
	noted bool                 // known has moved since it was last kept
}

type hit struct {
	started time.Time
	count   int
}

// The reasons a caller is refused, for the answer to say.
const (
	byCaller = "caller"
	byKind   = "kind"
	byProxy  = "proxy"
)

// attempt settles one token a caller offers, in one step: refused, whatever the token,
// when the caller's allowance or its proxy's is used up; otherwise counted against both
// when it is wrong, and its caller known when it is right.
//
// One step, so that tokens offered at once are counted at once. Were the allowance
// checked before a login's body is read and charged after, a caller could hold a hundred
// logins open past the check, and spend its proxy's allowance alone.
func (l *limiter) attempt(a allowance, right bool, now time.Time) (bool, time.Duration, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits, l.known = map[string]hit{}, map[string]time.Time{}
	}
	if now.Sub(l.swept) >= attemptsWindow {
		l.sweep(now)
	}
	key, whose := l.slot(a, now)
	if b, wait := l.over(key, attemptsMax, now); b {
		return true, wait, whose
	}
	proxy := a.proxy
	if l.knows(a.key, now) {
		proxy = ""
	}
	if proxy != "" {
		if b, wait := l.over(proxy, proxyMax, now); b {
			return true, wait, byProxy
		}
	}
	switch {
	case right && a.kind != "":
		l.know(a.key, now)
	case !right:
		l.add(key, now)
		if proxy != "" {
			l.add(proxy, now)
		}
	}
	return false, 0, ""
}

// slot is the entry an allowance's attempts are kept under: its own while there is
// room, or when it is one of this machine's, or a caller known.
func (l *limiter) slot(a allowance, now time.Time) (string, string) {
	if a.kind == "" {
		return a.key, byCaller
	}
	if _, ok := l.hits[a.key]; ok || l.knows(a.key, now) {
		return a.key, byCaller
	}
	if len(l.hits) >= attemptsKept && now.Sub(l.swept) >= time.Second {
		l.sweep(now) // full of callers whose minute may be over
	}
	if len(l.hits) < attemptsKept {
		return a.key, byCaller
	}
	return "everyone else " + a.kind, byKind
}

func (l *limiter) sweep(now time.Time) {
	for key, h := range l.hits {
		if now.Sub(h.started) >= attemptsWindow {
			delete(l.hits, key)
		}
	}
	l.swept = now
}

func (l *limiter) over(key string, most int, now time.Time) (bool, time.Duration) {
	h, ok := l.hits[key]
	if !ok || now.Sub(h.started) >= attemptsWindow || h.count < most {
		return false, 0
	}
	return true, h.started.Add(attemptsWindow).Sub(now)
}

// add counts one attempt; an entry past its window starts over where it lies.
func (l *limiter) add(key string, now time.Time) {
	h := l.hits[key]
	if h.count == 0 || now.Sub(h.started) >= attemptsWindow {
		h = hit{started: now}
	}
	h.count++
	l.hits[key] = h
}

func (l *limiter) knows(key string, now time.Time) bool {
	at, ok := l.known[key]
	return ok && now.Sub(at) < knownFor
}

// know remembers a caller that showed the right token. With knownKept remembered, the
// one that showed it longest ago is forgotten.
func (l *limiter) know(key string, now time.Time) {
	if at, ok := l.known[key]; !ok || now.Sub(at) >= knownNoted {
		l.noted = true
	}
	if _, ok := l.known[key]; !ok && len(l.known) >= knownKept {
		oldest := ""
		for k, at := range l.known {
			if oldest == "" || at.Before(l.known[oldest]) {
				oldest = k
			}
		}
		delete(l.known, oldest)
	}
	l.known[key] = now
}

// relayHeaders are what a proxy adds to a request it passes on, beside the headers that
// begin with relayPrefix.
var relayHeaders = []string{"Forwarded", "X-Real-Ip", "Via", "Cf-Connecting-Ip"}

// relayPrefix begins a family of forwarding headers: -For, -Host and -Proto, and -Port,
// -Server, -Prefix, -Ssl and the rest, any of which a proxy may add without the others.
const relayPrefix = "X-Forwarded-"

// relayed reports whether a request bears a proxy's mark: a forwarding header, or the
// header Options.ProxyHeader names. The mark is taken by its presence alone: a client can
// write anything into one, but cannot take away the one its proxy adds. A proxy that adds
// none, and a tunnel that passes bytes through, leave no mark.
func (s *Server) relayed(r *http.Request) bool {
	if header := s.opts.ProxyHeader; header != "" && len(r.Header.Values(header)) > 0 {
		return true
	}
	for _, name := range relayHeaders {
		if len(r.Header.Values(name)) > 0 {
			return true
		}
	}
	for name := range r.Header {
		if len(name) > len(relayPrefix) && strings.EqualFold(name[:len(relayPrefix)], relayPrefix) {
			return true
		}
	}
	return false
}

// caller is the allowance a request's wrong token is charged to (see allowance), and its
// key is the name the log gives the caller (see door).
//
// What a forwarding header says is believed only when it is the header
// Options.ProxyHeader names, on a connection from this machine or a private network —
// where a proxy of the owner's connects from — and only the last address in it, the one
// the nearest proxy wrote.
func (s *Server) caller(r *http.Request) allowance {
	peer, ok := addrOf(r.RemoteAddr)
	if !ok {
		return allowance{key: "unknown " + r.RemoteAddr, kind: "unknown"}
	}
	header := s.opts.ProxyHeader
	relayed := s.relayed(r)
	local := peer.IsLoopback()
	if !relayed {
		switch {
		case local && fromBrowser(r):
			return allowance{key: "local browser"}
		case local:
			return allowance{key: "local"}
		}
		return allowance{key: network(peer), kind: "direct"}
	}
	if header != "" && (local || peer.IsPrivate() || peer.IsLinkLocalUnicast()) {
		if said, ok := lastAddr(r.Header.Values(header)); ok {
			via := network(peer)
			if local {
				via = "local"
			}
			return allowance{key: "named " + network(said) + " via " + via, kind: "named", proxy: "proxy " + via}
		}
	}
	if local {
		return allowance{key: "local relayed"}
	}
	return allowance{key: network(peer) + " relayed", kind: "relayed"}
}

// fromBrowser reports whether a request carries what a browser adds to every request it
// makes and a page's script cannot remove.
func fromBrowser(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Origin") != ""
}

// addrOf reads an address that may carry a port: a connection's, or an entry of a
// forwarding header.
func addrOf(s string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"))
	return a.Unmap().WithZone(""), err == nil
}

// lastAddr is the last address of a header that lists them, as X-Forwarded-For does; a
// header that holds one address holds its last.
func lastAddr(values []string) (netip.Addr, bool) {
	if len(values) == 0 {
		return netip.Addr{}, false
	}
	list := strings.Split(values[len(values)-1], ",")
	return addrOf(strings.TrimSpace(list[len(list)-1]))
}

// network is what an address is counted as: itself, or for IPv6 the /64 it is in.
func network(a netip.Addr) string {
	if a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64)
	return p.String()
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

// authorize decides whether a request may read the record; when it is refused for
// guessing, for how long and whose allowance ran out.
func (s *Server) authorize(r *http.Request) (verdict, time.Duration, string) {
	if s.opts.Token == "" {
		return allowed, 0, "" // loopback only; the guards in front already ran
	}
	now := time.Now()
	if c, err := r.Cookie(cookieName); err == nil && signed(s.key, c.Value, now) {
		return allowed, 0, ""
	}
	given, ok := bearer(r)
	if !ok {
		return unauthorized, 0, ""
	}
	right := subtle.ConstantTimeCompare([]byte(given), []byte(s.opts.Token)) == 1
	if blocked, wait, whose := s.offer(s.caller(r), right, "in an Authorization header", now); blocked {
		return throttled, wait, whose // right or not: past the limit the answer does not say
	}
	if right {
		s.keepKnown()
		return allowed, 0, ""
	}
	return unauthorized, 0, ""
}

// offer settles a token a caller offers (see limiter.attempt), and writes down what came
// of it that the owner should hear of: a wrong token, or a caller refused whatever it
// brought. on says where the token was offered.
func (s *Server) offer(a allowance, right bool, on string, now time.Time) (bool, time.Duration, string) {
	blocked, wait, whose := s.attempts.attempt(a, right, now)
	switch {
	case blocked:
		s.door.note(now, refused, a.key, "too many wrong tokens "+whom(whose))
	case !right:
		s.door.note(now, wrongToken, a.key, on)
	}
	return blocked, wait, whose
}

// noToken is the answer to a request that came through a proxy to a server without a
// token.
const noToken = "this server has no token, and this request came to it through a proxy or a tunnel: " +
	"without a token it answers this machine only. To reach it through one, give it a token " +
	"(ASQ_TOKEN, at least 16 characters; openssl rand -hex 24 makes a good one) and start it again"

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
	if s.opts.Token == "" && s.relayed(r) {
		// Only this machine can connect, and this is something on it passing on what
		// others sent: a tunnel that writes Host: localhost (cloudflared's httpHostHeader,
		// ngrok's host-header rewrite) would otherwise put the record on the network
		s.door.note(time.Now(), refused, s.caller(r).key, "this server has no token, and the request came through a proxy")
		return noToken
	}
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
	// An MCP request of the 2026-07-28 revision says its method and tool in headers too, and
	// is turned down without them
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version, Mcp-Method, Mcp-Name, Last-Event-ID")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Max-Age", "600")
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	v, _, _ := s.authorize(r)
	writeJSON(w, r, http.StatusOK, map[string]any{"required": s.opts.Token != "", "authenticated": v == allowed})
}

// handleLogin trades the token for the cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.opts.Token == "" {
		writeError(w, r, http.StatusConflict, "this server has no token: there is nothing to sign in to")
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
	// Counted once the body is in, with the token it brings: see limiter.attempt
	now := time.Now()
	right := subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.opts.Token)) == 1
	caller := s.caller(r)
	if blocked, wait, whose := s.offer(caller, right, "at sign-in", now); blocked {
		tooMany(w, r, wait, whose)
		return
	}
	if !right {
		writeError(w, r, http.StatusUnauthorized, "that is not the token")
		return
	}
	s.keepKnown()
	s.door.note(now, signedIn, caller.key, "")
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

func tooMany(w http.ResponseWriter, r *http.Request, wait time.Duration, whose string) {
	secs := max(1, int(wait.Round(time.Second).Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, r, http.StatusTooManyRequests, fmt.Sprintf("too many wrong tokens %s: try again in %ds", whom(whose), secs))
}

// whom says whose allowance ran out, as the answer and the log put it.
func whom(whose string) string {
	switch whose {
	case byProxy:
		return "through this proxy"
	case byKind:
		return "from callers this server cannot tell apart, of whom there are too many"
	}
	return "from this caller"
}
