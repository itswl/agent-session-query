package app

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultLimit / defaultMaxLimit: the default and the hard cap for ?limit=
const (
	defaultLimit    = 50
	defaultMaxLimit = 1000
	mcpPath         = "/mcp"
)

// apiServer: routing, authentication, connection limiting and stats
type apiServer struct {
	mode       string
	sources    []SessionSource
	api        *SessionQueryAPI
	token      string
	corsOrigin string
	maxLimit   int
	mcp        *mcpServer

	maxConnections int

	mu          sync.Mutex
	totalConns  int64
	activeConns int64
	badRequests int64
}

type serverOptions struct {
	mode           string
	sources        []SessionSource
	api            *SessionQueryAPI
	token          string
	corsOrigin     string
	maxConnections int
	maxLimit       int
}

func newAPIServer(opts serverOptions) *apiServer {
	if opts.maxLimit < 1 {
		opts.maxLimit = defaultMaxLimit
	}
	return &apiServer{
		mode:           opts.mode,
		sources:        opts.sources,
		api:            opts.api,
		token:          opts.token,
		corsOrigin:     opts.corsOrigin,
		maxLimit:       opts.maxLimit,
		maxConnections: opts.maxConnections,
		mcp:            &mcpServer{api: opts.api, sources: opts.sources, maxLimit: opts.maxLimit},
	}
}

func (s *apiServer) stats() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"max_connections":    s.maxConnections,
		"active_connections": s.activeConns,
		"total_connections":  s.totalConns,
		"bad_requests":       s.badRequests,
	}
}

func (s *apiServer) countBadRequest() {
	s.mu.Lock()
	s.badRequests++
	s.mu.Unlock()
}

// connState keeps the connection counters (active = currently open)
func (s *apiServer) connState(_ net.Conn, state http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch state {
	case http.StateNew:
		s.totalConns++
		s.activeConns++
	case http.StateClosed, http.StateHijacked:
		s.activeConns--
	}
}

func (s *apiServer) checkAuth(r *http.Request) bool {
	if s.token == "" {
		return true // no token configured, so no authentication
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	// Constant-time comparison, so a character-by-character compare cannot leak the token
	// through timing
	return subtle.ConstantTimeCompare([]byte(header[len("Bearer "):]), []byte(s.token)) == 1
}

// applyCORS only emits CORS headers when --cors-origin was set explicitly.
//
// This used to send Access-Control-Allow-Origin: * unconditionally. Combined with the
// default of "no token means no authentication", that let any web page the user visited
// fetch this machine's /sessions and read the session content — source code, tool output —
// straight out. A plain GET is a simple request, triggers no preflight, and the browser
// hands the response to the calling script the moment it sees *.
// The bundled /ui is same-origin and never needed CORS at all.
func (s *apiServer) applyCORS(w http.ResponseWriter) {
	if s.corsOrigin == "" {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
	if s.corsOrigin != "*" {
		w.Header().Add("Vary", "Origin")
	}
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// writeJSON streams JSON out (no HTML escaping, no escaping of non-ASCII).
//
// It does not buffer the whole response in memory first: a large session's messages can
// run to tens of megabytes, and buffering a copy doubles peak memory — noticeable as soon
// as a few requests overlap.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// The headers are already out, so the status cannot change; all that is left is to
		// record it
		fmt.Fprintf(os.Stderr, "[ERROR] failed to serialise the response: %v\n", err)
	}
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// statusRecorder captures the status code actually written. The embedded page is served
// by http.FileServer, which writes its own 404s; without capturing them the access log
// would report 200 for everything.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func (s *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Unhandled exception: %v\n", rec)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal server error"})
		}
	}()

	s.applyCORS(w)

	switch r.Method {
	case http.MethodOptions:
		if s.corsOrigin != "" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodGet, http.MethodHead:
		// carry on
	case http.MethodPost:
		// MCP's Streamable HTTP uses POST; everything else here is a read-only GET
		if r.URL.EscapedPath() == mcpPath {
			logRequest(r, s.handleMCPPost(w, r))
			return
		}
		writePlain(w, http.StatusNotImplemented, fmt.Sprintf("Unsupported method ('%s')", r.Method))
		return
	default:
		writePlain(w, http.StatusNotImplemented, fmt.Sprintf("Unsupported method ('%s')", r.Method))
		return
	}

	status := s.route(w, r)
	logRequest(r, status)
}

// statusClientClosed mirrors nginx's 499: the client hung up before the response. It is
// never sent on the wire (there is nobody left to send it to) and only reaches the log.
const statusClientClosed = 499

// rootEndpoint is one line of the endpoint list GET / answers with. It is data rather than a
// literal buried in the handler so a test can hold it against the paths this package
// actually routes — see TestRootListsEveryRoute.
type rootEndpoint struct {
	Method string
	Path   string
	Public bool // reachable without a token
	Desc   string
}

// rootEndpoints is what GET / advertises. A caller that has no MCP client — a shell script,
// or an agent that found the port and nothing else — reads this to learn what the service
// can do, and for a long time it did not say: the list named six routes while fourteen were
// served, so /ui, /search, /projects, /export and /mcp were invisible to it.
var rootEndpoints = []rootEndpoint{
	{"GET", "/health", true, "health check"},
	{"GET", "/stats", true, "server stats"},
	{"GET", "/ui", true, "the web page"},
	{"GET", "/sessions", false, "list every session"},
	{"GET", "/sessions/<pattern>", false, "one session"},
	{"GET", "/sessions/<pattern>/messages?limit=50", false, "its messages, newest first by default"},
	{"GET", "/sessions/<pattern>/final", false, "its final result"},
	{"GET", "/sessions/<pattern>/export?format=", false, "one session as a document: md, jsonl, json or html"},
	{"GET", "/sessions/<pattern>/rounds", false, "the session split into rounds — one per real user message"},
	{"GET", "/sessions/<pattern>/brief?round=", false, "a handoff brief of one round (default the latest); ?at=<ts> picks the round a timestamp falls in"},
	{"GET", "/search?q=", false, "full-text search across every source"},
	{"GET", "/projects", false, "session counts grouped by project"},
	{"GET", "/export?project=", false, "a pack: what was asked and concluded across sessions, oldest first"},
	{"GET", "/api/<path>", false, "the same authenticated routes, under an /api prefix"},
	{"POST", "/mcp", false, "MCP over Streamable HTTP"},
}

// rootEndpointLines renders the table for GET /. The public routes are the ones marked: with
// a token configured there are three of them against ten that need one, so marking the
// exceptions is the shorter sentence for whoever reads this in a terminal.
func rootEndpointLines() []string {
	lines := make([]string, 0, len(rootEndpoints))
	for _, e := range rootEndpoints {
		line := e.Method + " " + e.Path + " - " + e.Desc
		if e.Public {
			line += " (no token)"
		}
		lines = append(lines, line)
	}
	return lines
}

func (s *apiServer) route(w http.ResponseWriter, r *http.Request) int {
	// Split on EscapedPath so %2F is not treated as a separator
	path := r.URL.EscapedPath()

	// Health check: unauthenticated, so monitoring can probe it
	if path == "/health" {
		body := map[string]any{
			"status":  "ok",
			"version": buildVersion,
			"mode":    s.mode,
			"sources": sourceModes(s.sources),
			// The page uses this to decide whether to show the token prompt: with no token
			// configured there is no reason to make anyone invent one
			"authRequired": s.token != "",
			"stats":        s.stats(),
		}
		// The health check is unauthenticated, so it cannot scan the sources to find out —
		// it reports the failure the last scan hit (see listWarnings). Without it the one
		// failure worth knowing about looks like perfect health: an empty session list.
		if warnings := s.api.listWarnings(); len(warnings) > 0 {
			body["warnings"] = warnings
		}
		writeJSON(w, http.StatusOK, body)
		return http.StatusOK
	}

	// Root: unauthenticated
	if path == "/" || path == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"name":    "Agent Session API",
			"mode":    s.mode,
			"sources": sourceModes(s.sources),
			// /health carries the same fact as a bare boolean; here it is worth the sentence
			// because it says which of the lines below it applies to
			"auth":      "Authorization: Bearer <token> is required, except where a line says (no token)",
			"endpoints": rootEndpointLines(),
		})
		return http.StatusOK
	}

	// Server stats: unauthenticated
	if path == "/stats" {
		writeJSON(w, http.StatusOK, s.stats())
		return http.StatusOK
	}

	// MCP's Streamable HTTP accepts POST only. The spec requires that a server offering no
	// SSE stream answer GET with 405, rather than holding an empty stream open while the
	// client waits for nothing.
	if path == mcpPath {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "MCP endpoint accepts POST only (no server-initiated stream)",
		})
		return http.StatusMethodNotAllowed
	}

	// Favicon: unauthenticated (browsers request it from the root themselves, see serveFavicon)
	if path == "/favicon.ico" {
		return serveFavicon(w)
	}

	// The embedded read-only page: unauthenticated (it holds no data; the data still needs a
	// token and goes through /sessions)
	if isUIPath(path) {
		rec := &statusRecorder{ResponseWriter: w}
		serveUIAssets(rec, r, path)
		if rec.status == 0 {
			return http.StatusOK
		}
		return rec.status
	}

	// /api prefix compatibility, stripped in one place (drops just the four characters of
	// "/api" and keeps the following "/")
	if strings.HasPrefix(path, "/api/") {
		path = path[len("/api"):]
	}

	// Every remaining endpoint requires authentication
	if !s.checkAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return http.StatusUnauthorized
	}

	if path == "/export" {
		pack, err := s.parsePackQuery(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return http.StatusBadRequest
		}
		entries, matching := s.api.packEntries(pack)
		if len(entries) == 0 {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "no sessions match that selection",
			})
			return http.StatusNotFound
		}
		summary := summarizePack(entries, matching, pack.mode)

		var body, filename, contentType string
		if pack.format == exportFormatJSONL {
			body = renderPackJSONL(entries, summary)
			filename, contentType = "session-pack.jsonl", exportContentType(exportFormatJSONL)
		} else {
			body = renderPackMarkdown(s.api, entries, summary)
			filename, contentType = "session-pack.md", exportContentType(exportFormatMarkdown)
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filename))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
		return http.StatusOK
	}

	if path == "/sessions" {
		sessions, etag := s.api.listSessions()
		// The page polls every 10 seconds and the list has usually not changed; with an
		// ETag those polls end at a 304
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return http.StatusNotModified
		}
		body := map[string]any{"sessions": sessions, "total": len(sessions)}
		// A source that could not be read answers with the same empty list as a source with
		// nothing in it; this is where the two are told apart
		if warnings := s.api.listWarnings(); len(warnings) > 0 {
			body["warnings"] = warnings
		}
		writeJSON(w, http.StatusOK, body)
		return http.StatusOK
	}

	if path == "/projects" {
		projects, ungrouped := s.api.listProjects()
		writeJSON(w, http.StatusOK, map[string]any{
			"projects": projects,
			"total":    len(projects),
			// hermes / openclaw have neither cwd nor project, so they cannot be grouped
			"ungrouped": ungrouped,
		})
		return http.StatusOK
	}

	if path == "/search" {
		query, err := s.parseSearchQuery(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return http.StatusBadRequest
		}
		started := time.Now()
		found := s.api.search(r.Context(), query)
		if found.stopped {
			// The client hung up mid-scan (the page cancels in-flight searches on every
			// keystroke). 499 is nginx's "client closed request" and only reaches the log.
			return statusClientClosed
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"query":     query.needle,
			"results":   found.results,
			"total":     len(found.results),
			"matched":   found.matched, // sessions with a hit, possibly more than total
			"scanned":   found.scanned, // sessions actually scanned
			"truncated": found.matched > len(found.results),
			"tookMs":    time.Since(started).Milliseconds(),
		})
		return http.StatusOK
	}

	if strings.HasPrefix(path, "/sessions/") {
		rest := path[len("/sessions/"):]
		parts := strings.Split(rest, "/")
		switch {
		case len(parts) == 1 && parts[0] != "":
			pattern := unescapePattern(parts[0])
			session, ok := s.api.getSession(pattern, "")
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "Session not found"})
				return http.StatusNotFound
			}
			writeJSON(w, http.StatusOK, session)
			return http.StatusOK

		case len(parts) == 2 && parts[0] != "" && parts[1] == "messages":
			pattern := unescapePattern(parts[0])
			query, err := s.parseMessageQuery(r, s.maxLimit)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return http.StatusBadRequest
			}
			messages, ok := s.api.getMessages(pattern, "", query)
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "Session not found"})
				return http.StatusNotFound
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"messages": messages,
				"total":    len(messages),
				"order":    orderName(query.fromEnd),
			})
			return http.StatusOK

		case len(parts) == 2 && parts[0] != "" && parts[1] == "rounds":
			pattern := unescapePattern(parts[0])
			rounds, err := s.api.sessionRounds(pattern, r.URL.Query().Get("source"))
			if err != nil {
				status, payload := briefHTTPError(err)
				writeJSON(w, status, payload)
				return status
			}
			writeJSON(w, http.StatusOK, rounds)
			return http.StatusOK

		case len(parts) == 2 && parts[0] != "" && parts[1] == "brief":
			pattern := unescapePattern(parts[0])
			roundNo := 0
			if raw := r.URL.Query().Get("round"); raw != "" {
				n, err := strconv.Atoi(raw)
				if err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "round must be a number"})
					return http.StatusBadRequest
				}
				roundNo = n
			}
			brief, err := s.api.sessionBrief(pattern, r.URL.Query().Get("source"), roundNo, r.URL.Query().Get("at"))
			if err != nil {
				status, payload := briefHTTPError(err)
				writeJSON(w, status, payload)
				return status
			}
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Header().Set("Content-Length", strconv.Itoa(len(brief)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(brief))
			return http.StatusOK

		case len(parts) == 2 && parts[0] != "" && parts[1] == "export":
			pattern := unescapePattern(parts[0])
			exportQuery, err := s.parseMessageQuery(r, exportMaxMessages)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return http.StatusBadRequest
			}
			// No ?limit= means the whole session. The page-default of 200 is a page size;
			// an export is a document, and one that silently held a twelfth of the session
			// was taken for the whole thing.
			if _, given := r.URL.Query()["limit"]; !given {
				exportQuery.limit = exportMaxMessages
			}
			format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
			if format == "" {
				format = exportFormatMarkdown
			}
			if !slices.Contains(exportFormats, format) {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "format must be one of: " + strings.Join(exportFormats, ", "),
				})
				return http.StatusBadRequest
			}
			body, filename, contentType, ok := s.api.exportSession(pattern, exportQuery, format)
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "Session not found"})
				return http.StatusNotFound
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filename))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return http.StatusOK

		case len(parts) == 2 && parts[0] != "" && parts[1] == "final":
			pattern := unescapePattern(parts[0])
			result, ok := s.api.getFinalMessage(pattern, "")
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "Session not found"})
				return http.StatusNotFound
			}
			writeJSON(w, http.StatusOK, result)
			return http.StatusOK
		}
	}

	writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
	return http.StatusNotFound
}

// etagMatches compares against If-None-Match, which may be a comma-separated list or *
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}

func unescapePattern(segment string) string {
	if pattern, err := url.PathUnescape(segment); err == nil {
		return pattern
	}
	return segment
}

// parseLimit reads ?limit=, falling back to defaultLimit when absent or invalid, and
// clamps the result into [0, maxLimit].
//
// Without the clamp, ?limit=99999999 spreads a 99 MB session into a 14 MB response in
// memory (measured: RSS 11 MB to 145 MB), and a handful of concurrent requests is enough
// to take the process down.
// exportMaxMessages bounds one export. It sits well above the list's --max-limit on
// purpose: a list is a page, but an export is a document the caller asked for in full, and
// it goes to a file rather than into anything that has to hold it all at once. The bound
// is still there so a pathological session cannot build an unbounded string in memory.
const exportMaxMessages = 20000

// parseLimit reads ?limit= against one ceiling. The ceiling is a parameter because the
// two callers want different things: a list is a page and --max-limit bounds it, while an
// export is a document and the caller asked for the whole of it.
func (s *apiServer) parseLimit(r *http.Request, ceiling int) int {
	limit := defaultLimit
	if values, ok := r.URL.Query()["limit"]; ok && len(values) == 1 {
		if parsed, err := strconv.Atoi(strings.TrimSpace(values[0])); err == nil {
			limit = parsed
		}
	}
	if limit < 0 {
		return 0
	}
	if limit > ceiling {
		return ceiling
	}
	return limit
}

// parseMessageQuery reads ?limit= and ?order= (desc asks for the latest N)
func (s *apiServer) parseMessageQuery(r *http.Request, ceiling int) (messageQuery, error) {
	order := strings.TrimSpace(r.URL.Query().Get("order"))
	q := messageQuery{
		limit:   s.parseLimit(r, ceiling),
		fromEnd: strings.EqualFold(order, "desc"),
	}
	// ?at= positions the window at a point in time rather than at one end, which is how a
	// caller lands on a specific message in a long session (a search hit, say). Absolute
	// forms only: "30d" would be a different question.
	if raw := strings.TrimSpace(r.URL.Query().Get("at")); raw != "" {
		at, err := parseAt(raw)
		if err != nil {
			return messageQuery{}, err
		}
		q.at = at
	}
	return q, nil
}

// parseAt reads an absolute instant: RFC3339, "2006-01-02T15:04:05", "2006-01-02", or a
// bare epoch in seconds or milliseconds.
func parseAt(raw string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		if n > 1e11 { // milliseconds
			return time.UnixMilli(int64(n)).UTC(), nil
		}
		return time.Unix(int64(n), 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("bad at value: %q (want an ISO time or an epoch)", raw)
}

// parseSearchQuery reads the /search parameters: q is required, limit is how many
// sessions come back, per_session is how many hits each session may contribute, and since
// narrows the scan (useful on machines with a lot of history).
func (s *apiServer) parseSearchQuery(r *http.Request) (searchQuery, error) {
	values := r.URL.Query()
	needle := strings.TrimSpace(values.Get("q"))
	if needle == "" {
		return searchQuery{}, errors.New("missing query parameter: q")
	}

	q := searchQuery{
		needle:     needle,
		lowered:    appendLowerASCII(nil, []byte(needle)),
		pattern:    strings.TrimSpace(values.Get("pattern")),
		limit:      defaultSearchLimit,
		perSession: defaultSearchPerSession,
	}
	// limit=0 is legitimate: useful when you only want the hit count, not the bodies
	if n, err := strconv.Atoi(strings.TrimSpace(values.Get("limit"))); err == nil && n >= 0 {
		q.limit = n
	}
	if q.limit > s.maxLimit {
		q.limit = s.maxLimit
	}
	if n, err := strconv.Atoi(strings.TrimSpace(values.Get("per_session"))); err == nil && n > 0 {
		q.perSession = n
	}
	if q.perSession > s.maxLimit {
		q.perSession = s.maxLimit
	}
	if raw := strings.TrimSpace(values.Get("since")); raw != "" {
		since, err := parseSince(raw)
		if err != nil {
			return searchQuery{}, err
		}
		q.since = since
	}
	if raw := strings.TrimSpace(values.Get("until")); raw != "" {
		until, err := parseSince(raw)
		if err != nil {
			return searchQuery{}, err
		}
		q.until = until
	}
	role, err := parseSearchRole(values.Get("role"))
	if err != nil {
		return searchQuery{}, err
	}
	q.role = role
	return q, nil
}

// parseSince reads ?since=, accepting relative forms like 30d / 12h / 90m as well as a
// date such as 2026-09-01.
// parseSearchRole reads ?role= for /search, matching what get_messages accepts
func parseSearchRole(raw string) (string, error) {
	role := strings.TrimSpace(raw)
	if role == "" || role == "user" || role == "assistant" {
		return role, nil
	}
	return "", fmt.Errorf("role must be user or assistant, got %q", role)
}

func parseSince(raw string) (time.Time, error) {
	if len(raw) > 1 {
		if unit := raw[len(raw)-1]; unit == 'd' || unit == 'D' {
			days, err := strconv.Atoi(raw[:len(raw)-1])
			if err == nil && days >= 0 {
				return time.Now().Add(-time.Duration(days) * 24 * time.Hour), nil
			}
		}
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return time.Now().Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("bad since value: %q (want 30d / 12h / 2006-01-02)", raw)
}

func orderName(fromEnd bool) string {
	if fromEnd {
		return "desc"
	}
	return "asc"
}

func logRequest(r *http.Request, status int) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	fmt.Fprintf(os.Stderr, "%s - - [%s] \"%s %s %s\" %d\n",
		host, time.Now().Format("02/Jan/2006:15:04:05 -0700"),
		r.Method, r.URL.RequestURI(), r.Proto, status)
}

// ---------------------------------------------------------------------------
// Connection limiting: anything past the cap gets a 503
// ---------------------------------------------------------------------------

// acceptQueueWait is how long a connection may queue while at capacity before it gets
// a 503
const acceptQueueWait = 10 * time.Second

// The automatic queue depth: autoAcceptQueueFactor times maxConnections, never below
// minAcceptQueue. The queue exists purely to absorb bursts, and making it too small (say
// with --max-connections 2) would reject most of a 20-request burst that the old
// blocking-accept design would have queued up fine.
const (
	minAcceptQueue        = 32
	autoAcceptQueueFactor = 2
)

// acceptQueueDepth resolves the real queue depth; queue <= 0 derives it from
// maxConnections.
func acceptQueueDepth(maxConnections, queue int) int {
	if queue > 0 {
		return queue
	}
	auto := maxConnections * autoAcceptQueueFactor
	if auto < minAcceptQueue {
		return minAcceptQueue
	}
	return auto
}

// limitListener caps how many connections are being handled at once.
//
// The accept loop runs in its own goroutine and hands admitted connections to http.Server
// through ready. It used to wait for a permit inside Accept() itself, which stalled the
// entire accept loop at capacity: connection 51 waited 10 seconds and connection 52 could
// not even start queuing until that finished, so the longer the queue the slower it got.
// Now each connection queues on its own without blocking the others.
//
// waiting caps how many may queue at once: once those slots are gone a connection gets an
// immediate 503, so a flood cannot pile up goroutines and file descriptors. That
// backpressure used to come for free from blocking accept, and now has to be explicit.
type limitListener struct {
	net.Listener
	sem     chan struct{} // permits for concurrent handling
	waiting chan struct{} // queue slots
	ready   chan net.Conn
	failed  chan error
	timeout time.Duration
	server  *apiServer

	once sync.Once
	err  error // only read and written in Accept (http.Server calls it from one goroutine)
}

// newLimitListener: maxConnections caps concurrent handling, queue is the number of queue
// slots (<= 0 derives it from maxConnections, see acceptQueueDepth).
func newLimitListener(inner net.Listener, maxConnections, queue int) *limitListener {
	if maxConnections < 1 {
		maxConnections = 1
	}
	return &limitListener{
		Listener: inner,
		sem:      make(chan struct{}, maxConnections),
		waiting:  make(chan struct{}, acceptQueueDepth(maxConnections, queue)),
		ready:    make(chan net.Conn),
		failed:   make(chan error, 1),
		timeout:  acceptQueueWait,
	}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.once.Do(func() { go l.acceptLoop() })
	if l.err != nil {
		return nil, l.err
	}
	select {
	case conn := <-l.ready:
		return conn, nil
	case err := <-l.failed:
		l.err = err
		return nil, err
	}
}

func (l *limitListener) acceptLoop() {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			l.failed <- err
			return
		}
		select {
		case l.sem <- struct{}{}: // a slot is free, admit it straight away
			l.ready <- l.wrap(conn)
		case l.waiting <- struct{}{}: // at capacity, but there is room to queue
			go func() {
				defer func() { <-l.waiting }()
				l.admit(conn)
			}()
		default: // not even a queue slot left
			l.reject(conn)
		}
	}
}

// admit queues for a permit, hands the connection over once it gets one, and returns a
// 503 on timeout
func (l *limitListener) admit(conn net.Conn) {
	timer := time.NewTimer(l.timeout)
	defer timer.Stop()
	select {
	case l.sem <- struct{}{}:
		l.ready <- l.wrap(conn)
	case <-timer.C:
		l.reject(conn)
	}
}

func (l *limitListener) wrap(conn net.Conn) net.Conn {
	return &releaseConn{Conn: conn, release: func() { <-l.sem }}
}

func (l *limitListener) reject(conn net.Conn) {
	writeOverloaded(conn)
	_ = conn.Close()
	if l.server != nil {
		l.server.countBadRequest()
	}
}

func writeOverloaded(conn net.Conn) {
	body := "Service temporarily overloaded"
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "HTTP/1.1 503 Service temporarily overloaded\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

// releaseConn returns the permit when the connection closes (exactly once)
type releaseConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *releaseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// countingWriter makes http.Server's ErrorLog feed the bad_requests counter too
type countingWriter struct {
	server *apiServer
}

func (w countingWriter) Write(p []byte) (int, error) {
	w.server.countBadRequest()
	return os.Stderr.Write(p)
}
