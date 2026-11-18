// Package server is the program over HTTP: the JSON API, the page that is a client of
// it, the live stream of what changed, and the MCP endpoint.
//
//	GET  /api/sessions                          the list             ?text ?source ?project ?branch ?since ?until ?archived ?agents ?limit ?offset
//	GET  /api/sessions/{source}/{id}            one session: its outline and its subagents
//	GET  /api/sessions/{source}/{id}/turns      turns step by step   ?turns=3 | 3-5 | last | all  ?full
//	GET  /api/sessions/{source}/{id}/items/{seq}  one step whole
//	GET  /api/sessions/{source}/{id}/brief      a handoff brief      ?turn ?since
//	GET  /api/sessions/{source}/{id}/export     the session whole    ?format=md | json | jsonl | html
//	GET  /api/search                            the turns that match ?q …and the list's filters ?recent ?per_session
//	GET  /api/usage  /api/failures  /api/digest  /api/files  /api/projects  /api/pack
//	GET  /api/events                            a stream of what changed (Server-Sent Events)
//	GET  /api/health                            the stores, the index, and what could not be read
//	GET  /api/auth   POST /api/login  /api/logout
//	POST /mcp                                   the MCP server
//	GET  /healthz                               200 while the process serves; says nothing else
//
// Everything is read-only. Nothing here can change a session.
package server

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/mcp"
)

// Options configure a server.
type Options struct {
	Host string
	Port int
	// Token, when set, is what every caller of the API must hold. Without one the server
	// listens on loopback only.
	Token string
	// Origins are the other origins whose pages may call the API ("*" for any). The
	// page this server serves needs no entry.
	Origins []string
	// Page is the web page's files, index.html at the root. Nil serves the API alone.
	Page fs.FS
	// Live reads the page's files again on every request instead of once: for working on
	// the page from a directory, without restarting.
	Live bool
	// State is the directory the server keeps its cookie secret in, so that a sign-in
	// outlives a restart. Empty keeps nothing: each start makes a new secret, and the
	// cookies of the run before stop working.
	State string
	// ProxyHeader names the header in which a proxy in front states who is calling —
	// CF-Connecting-IP, X-Real-IP, an X-Forwarded-For the proxy appends to. Wrong tokens
	// are then counted per caller rather than per proxy (see allowance).
	ProxyHeader string
	// ProxyHeaderSetting is how ProxyHeader was given — "--proxy-header", or
	// "ASQ_PROXY_HEADER" — so that a refusal of it names what to change. Empty is the flag.
	ProxyHeaderSetting string
	// Log receives one line per request that failed, what the server has to say, and
	// what its guards did (see door).
	Log io.Writer
}

// Server is one listening program.
type Server struct {
	app  *app.App
	opts Options
	mcp  *mcp.Server

	key      []byte
	attempts limiter
	// keepsKnown says the state directory keeps the callers known (see keepKnown)
	keepsKnown bool
	keepMu     sync.Mutex
	keepFailed time.Time // when keeping the callers known last failed; under keepMu
	door       door      // what the guards did, written down and counted

	mux     *http.ServeMux
	assets  map[string]asset
	pageTag string // names this set of the page's files (see versioned)

	// For pacing the watch: how many streams are open, and when anyone last asked
	streams atomic.Int64
	asked   atomic.Int64
}

type asset struct {
	body  []byte
	ctype string
	etag  string
}

// How closely the index follows the stores: while someone is looking, a transcript that
// grows shows within watchBusy; an idle server looks only every watchIdle, and catches
// up at once when asked (see fresh).
const (
	watchBusy = 2 * time.Second
	watchIdle = 30 * time.Second
	lookedFor = 90 * time.Second // how long after the last request someone still counts as looking
	// An answer may be this far behind the stores. It is a little more than the close
	// pace, so that while the watcher keeps that pace no question waits for it: see
	// App.Fresh.
	maxAge    = watchBusy + time.Second
	waitFresh = 4 * time.Second
	maxStream = 64
)

// New builds a server, refusing a configuration that would publish the record.
func New(a *app.App, opts Options) (*Server, error) {
	if opts.Host == "" {
		opts.Host = "127.0.0.1"
	}
	if opts.Token == "" {
		if !IsLoopback(opts.Host) {
			return nil, fmt.Errorf("listening on %s would let anyone who can reach this machine read every session: set a token (--token, or ASQ_TOKEN), or listen on 127.0.0.1", opts.Host)
		}
		for _, o := range opts.Origins {
			if o == "*" {
				return nil, errors.New("--cors-origin * lets any page in the browser read every session: it needs a token (--token, or ASQ_TOKEN)")
			}
		}
	} else if len(opts.Token) < 16 {
		return nil, errors.New("the token is the only thing between the network and every session: make it at least 16 characters (openssl rand -hex 24)")
	}
	setting := cmp.Or(opts.ProxyHeaderSetting, "--proxy-header")
	if !headerName(opts.ProxyHeader) {
		return nil, fmt.Errorf("%s takes the name of a header (CF-Connecting-IP, X-Real-IP, X-Forwarded-For), not %q", setting, opts.ProxyHeader)
	}
	if opts.Token == "" && opts.ProxyHeader != "" {
		return nil, fmt.Errorf("%s is for a server behind a proxy, and a server without a token is not to be put behind one: set a token (--token, or ASQ_TOKEN), or remove %s", setting, setting)
	}
	s := &Server{app: a, opts: opts, mcp: mcp.New(a, opts.Log), assets: map[string]asset{}}
	s.door.out, s.door.began = opts.Log, time.Now()
	// The MCP endpoint answers as the rest of the API does: from the index the watcher
	// keeps, without waiting for a read that is only following the stores
	s.mcp.MaxAge = maxAge
	if opts.Token != "" {
		secret, kept := s.cookieSecret()
		s.key = signKey(secret, opts.Token)
		if s.keepsKnown = kept; kept {
			s.loadKnown(time.Now())
		}
	}
	if opts.Page != nil {
		var err error
		if s.assets, s.pageTag, err = readPage(opts.Page); err != nil {
			return nil, err
		}
	}
	s.routes()
	return s, nil
}

// headerName reports whether s could name a header, or names none.
func headerName(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, format+"\n", args...)
	}
}

// readPage reads the page's files: a few hundred kilobytes that never change while the
// process lives, each with the ETag its bytes decide; and the tag that names this set of
// them (see versioned).
func readPage(page fs.FS) (map[string]asset, string, error) {
	assets := map[string]asset{}
	err := fs.WalkDir(page, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(name, ".go") {
			return nil
		}
		body, err := fs.ReadFile(page, name)
		if err != nil {
			return err
		}
		ctype := mime.TypeByExtension(path.Ext(name))
		switch path.Ext(name) {
		case ".js", ".mjs":
			ctype = "text/javascript; charset=utf-8"
		case ".css":
			ctype = "text/css; charset=utf-8"
		case ".html":
			ctype = "text/html; charset=utf-8"
		case ".svg":
			ctype = "image/svg+xml"
		case ".webmanifest":
			ctype = "application/manifest+json"
		case ".woff2":
			ctype = "font/woff2"
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		sum := sha256.Sum256(body)
		assets["/"+name] = asset{body: body, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	tag := pageTag(assets)
	if idx, ok := assets["/index.html"]; ok {
		idx.body = versioned(idx.body, tag, assets)
		sum := sha256.Sum256(idx.body)
		idx.etag = `"` + hex.EncodeToString(sum[:8]) + `"`
		assets["/index.html"] = idx
	}
	return assets, tag, nil
}

// pageTag names a set of the page's files: it changes when any of them does.
func pageTag(assets map[string]asset) string {
	names := make([]string, 0, len(assets))
	for name := range assets {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		io.WriteString(h, name+"\x00"+assets[name].etag+"\x00")
	}
	return hex.EncodeToString(h.Sum(nil)[:6])
}

// versioned points the page at its script, its style and its fonts under /a/<tag>/: a
// path that names this set of the files, and so may be kept for good. What the script
// imports, and the style's fonts, are named relative to it and follow.
//
// Asked for again by its own path, a file is asked whether it changed. A cache in
// between need not ask: Cloudflare, by default, keeps a script for four hours whatever
// the server says, and a browser behind it would run a page the server is no longer.
// Under its tag, a file that changes is a file at another path.
func versioned(index []byte, tag string, assets map[string]asset) []byte {
	return pageRef.ReplaceAllFunc(index, func(m []byte) []byte {
		sub := pageRef.FindSubmatch(m)
		ref := string(sub[2])
		switch path.Ext(ref) {
		case ".js", ".css", ".woff2":
			if _, ok := assets[ref]; ok {
				return []byte(string(sub[1]) + `="/a/` + tag + ref + `"`)
			}
		}
		return m
	})
}

var pageRef = regexp.MustCompile(`\b(href|src)="(/[^"]+)"`)

func (s *Server) routes() {
	mux := http.NewServeMux()
	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.private(h)) }

	api("GET /api/health", s.handleHealth)
	api("GET /api/sessions", s.handleSessions)
	api("GET /api/sessions/{source}/{id}", s.handleSession)
	api("GET /api/sessions/{source}/{id}/turns", s.handleTurns)
	api("GET /api/sessions/{source}/{id}/items/{seq}", s.handleItem)
	api("GET /api/sessions/{source}/{id}/brief", s.handleBrief)
	api("GET /api/sessions/{source}/{id}/export", s.handleExport)
	api("GET /api/search", s.handleSearch)
	api("GET /api/projects", s.handleProjects)
	api("GET /api/usage", s.handleUsage)
	api("GET /api/failures", s.handleFailures)
	api("GET /api/digest", s.handleDigest)
	api("GET /api/files", s.handleFiles)
	api("GET /api/pack", s.handlePack)
	api("GET /api/events", s.handleEvents)
	mux.Handle("/mcp", s.private(s.mcp.ServeHTTP))

	mux.HandleFunc("GET /api/auth", s.handleAuth)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	mux.HandleFunc("/", s.handlePage)
	s.mux = mux
}

// private wraps a route that reads the record: the caller proves who it is first.
func (s *Server) private(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch v, wait, whose := s.authorize(r); v {
		case throttled:
			tooMany(w, r, wait, whose)
			return
		case unauthorized:
			w.Header().Set("WWW-Authenticate", `Bearer realm="asq"`)
			writeError(w, r, http.StatusUnauthorized, "this server needs its token: send it as 'Authorization: Bearer <token>', or sign in on the page")
			return
		}
		s.asked.Store(time.Now().UnixNano())
		h(w, r)
	})
}

// ServeHTTP is the front door: the guards, the headers every reply carries, then the
// route.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	if why := s.guard(r); why != "" {
		writeError(w, r, http.StatusForbidden, why)
		return
	}
	s.cors(w, r)
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rec := &recorder{ResponseWriter: w}
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				panic(p)
			}
			s.logf("%s %s: panic: %v", r.Method, r.URL.Path, p)
			if !rec.wrote {
				writeError(rec, r, http.StatusInternalServerError, "something broke on this side")
			}
		}
	}()
	s.mux.ServeHTTP(rec, r)
	if rec.status >= 500 {
		s.logf("%s %s → %d", r.Method, r.URL.Path, rec.status)
	}
}

type recorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the connection: the event stream flushes
// and lifts its write deadline through it.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// pageCSP confines the page to its own files: no inline script or style, nothing loaded
// from elsewhere, nothing framing it. A transcript is full of other people's text, and
// the page puts it on screen as text only; this is the second lock on that door.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, r, http.StatusMethodNotAllowed, "the page is only read")
		return
	}
	assets, tag := s.assets, s.pageTag
	if s.opts.Live {
		// Each request reads the directory for itself: a browser asks for a dozen modules
		// at once, and none of them may see another's half-read set
		var err error
		if assets, tag, err = readPage(s.opts.Page); err != nil {
			writeError(w, r, http.StatusInternalServerError, "the page could not be read: "+err.Error())
			return
		}
	}
	if len(assets) == 0 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "asq "+app.Version+" — the API is under /api (see /api/health); this build carries no page.\n")
		return
	}
	name := path.Clean(r.URL.Path)
	// Always asked for again, and answered 304 while unchanged — unless it is asked for
	// under the tag of this set of the files, which may be kept for good (see versioned).
	// A file under another tag — a page loaded before an upgrade — is the file as it is
	// now, and not to be kept
	cache := "no-cache"
	tagged := false
	if rest, ok := strings.CutPrefix(name, "/a/"); ok {
		t, file, _ := strings.Cut(rest, "/")
		name, tagged = "/"+file, true
		if t == tag && !s.opts.Live {
			cache = "public, max-age=31536000, immutable"
		}
	}
	a, ok := assets[name]
	if !ok {
		// Any path that is not a file is the page, which reads the address itself: a
		// session's link opens that session
		if tagged || path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		a = assets["/index.html"]
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("ETag", a.etag)
	h.Set("Cache-Control", cache)
	h.Set("Content-Security-Policy", pageCSP)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeBody(w, r, http.StatusOK, a.body)
}

// Addr is the address the server listens on.
func (s *Server) Addr() string { return net.JoinHostPort(s.opts.Host, strconv.Itoa(s.opts.Port)) }

// Run listens and serves until ctx ends, keeping the index in step with the stores
// meanwhile. ready, when not nil, is told the address once the listener is up.
func (s *Server) Run(ctx context.Context, ready func(addr string)) error {
	ln, err := net.Listen("tcp", s.Addr())
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute, // the event stream lifts its own
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	if ready != nil {
		// The host as configured, the port as bound: ":0" asks the system for one, and
		// "0.0.0.0" reads better than the "[::]" a listener calls it
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		ready(net.JoinHostPort(s.opts.Host, port))
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.watch(ctx)
	}()

	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	select {
	case err := <-failed:
		wg.Wait()
		return err
	case <-ctx.Done():
	}
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(stop)
	wg.Wait()
	return nil
}

// watch keeps the index in step: closely while someone is looking, loosely otherwise. It
// wakes at the close pace either way, so that when somebody asks after a quiet spell the
// stores are followed from then on, and not from half a minute later.
func (s *Server) watch(ctx context.Context) {
	var looked time.Time
	for {
		if s.due(looked) {
			if _, err := s.app.Sync(ctx, index.SyncOptions{}); err != nil && ctx.Err() == nil {
				s.logf("keeping the index in step: %v", err)
			}
			looked = time.Now()
		}
		// A minute of the guards' lines that is over is summed up now, and not only when
		// the next caller comes along
		s.door.flush(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(watchBusy):
		}
	}
}

// due reports whether the stores are to be read now, having last been read at looked:
// every time while someone is looking, and otherwise when the loose pace says so.
func (s *Server) due(looked time.Time) bool {
	return s.looking() || time.Since(looked) >= watchIdle
}

// looking reports whether anyone is: an open stream, or a question not long ago.
func (s *Server) looking() bool {
	return s.streams.Load() > 0 || time.Since(time.Unix(0, s.asked.Load())) < lookedFor
}

// fresh brings the index up to date before a question is answered, when nobody has for
// a while, and waits a bounded time for that: past it the answer comes from what has
// been read so far, and the stream says how far that is. While the watcher is keeping
// pace the question is answered at once (see App.Fresh).
func (s *Server) fresh(r *http.Request) {
	s.app.Fresh(r.Context(), maxAge, waitFresh)
}
