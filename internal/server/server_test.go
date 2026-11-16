package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

const token = "a-token-long-enough-to-be-one"

var page = fstest.MapFS{
	"index.html": {Data: []byte("<!doctype html><title>asq</title>")},
	"app.js":     {Data: []byte("console.log('page')")},
}

func build(t *testing.T, opts Options) (*Server, *app.App, fixture.History) {
	t.Helper()
	a, h := fixture.Open(t)
	if opts.Page == nil {
		opts.Page = page
	}
	s, err := New(a, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, a, h
}

type call struct {
	method, path, body string
	header             map[string]string
}

func (c call) do(s *Server) *httptest.ResponseRecorder {
	method := c.method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if c.body != "" {
		body = strings.NewReader(c.body)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:7717"+c.path, body)
	for k, v := range c.header {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, s *Server, path string, into any) {
	t.Helper()
	rec := call{path: path}.do(s)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
	}
	if into != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("GET %s: %v in %s", path, err, rec.Body)
		}
	}
}

func TestARecordIsNotPublishedByAccident(t *testing.T) {
	a, _ := fixture.Open(t)
	for name, opts := range map[string]Options{
		"every interface, no token":  {Host: "0.0.0.0"},
		"a LAN address, no token":    {Host: "192.168.1.20"},
		"any origin, no token":       {Origins: []string{"*"}},
		"a token someone could type": {Host: "0.0.0.0", Token: "hunter2"},
	} {
		if _, err := New(a, opts); err == nil {
			t.Errorf("%s: the server started", name)
		}
	}
	for name, opts := range map[string]Options{
		"loopback":            {},
		"localhost":           {Host: "localhost"},
		"IPv6 loopback":       {Host: "::1"},
		"everywhere, guarded": {Host: "0.0.0.0", Token: token},
	} {
		if _, err := New(a, opts); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRoutes(t *testing.T) {
	s, _, _ := build(t, Options{})
	alpha := "/api/sessions/claude/" + fixture.Alpha

	var list struct {
		Sessions []index.Session `json:"sessions"`
		Total    int             `json:"total"`
	}
	get(t, s, "/api/sessions", &list)
	if list.Total != 2 || list.Sessions[0].ID != fixture.Alpha || list.Sessions[0].Resume == "" {
		t.Errorf("sessions = %d, first %+v", list.Total, list.Sessions[0].Session)
	}
	get(t, s, "/api/sessions?project=beta&limit=1", &list)
	if list.Total != 1 || list.Sessions[0].ID != fixture.Beta {
		t.Errorf("filtered = %+v", list)
	}

	var one struct {
		Session index.Session    `json:"session"`
		Turns   []map[string]any `json:"turns"`
		Agents  []index.Session  `json:"agents"`
	}
	get(t, s, alpha, &one)
	if one.Session.Title != "Fix flaky test" || len(one.Turns) != 2 || len(one.Agents) != 1 || one.Agents[0].ID != fixture.Agent {
		t.Errorf("session = %+v, %d turns, %d agents", one.Session.Session, len(one.Turns), len(one.Agents))
	}
	if _, has := one.Turns[0]["items"]; has {
		t.Error("the outline carries items: a session is megabytes of them")
	}
	get(t, s, "/api/sessions/claude/aaaa1111", &one) // the start of an id is enough
	if one.Session.ID != fixture.Alpha {
		t.Errorf("by prefix = %q", one.Session.ID)
	}

	var turns struct {
		Turns []struct {
			Index int              `json:"index"`
			Items []map[string]any `json:"items"`
		} `json:"turns"`
	}
	get(t, s, alpha+"/turns?turns=1", &turns)
	if len(turns.Turns) != 1 || turns.Turns[0].Index != 1 || len(turns.Turns[0].Items) != 4 {
		t.Errorf("turns = %+v", turns)
	}
	get(t, s, alpha+"/turns", &turns)
	if len(turns.Turns) != 1 || turns.Turns[0].Index != 2 {
		t.Errorf("with no range asked, the last turn: %+v", turns)
	}

	var item struct {
		Turn int `json:"turn"`
		Item struct {
			Tool struct {
				Output string `json:"output"`
			} `json:"tool"`
		} `json:"item"`
	}
	get(t, s, alpha+"/items/2", &item)
	if item.Turn != 1 || !strings.Contains(item.Item.Tool.Output, "ECONNREFUSED") {
		t.Errorf("item = %+v", item)
	}

	var brief struct {
		Brief string `json:"brief"`
	}
	get(t, s, alpha+"/brief?turn=1", &brief)
	if !strings.Contains(brief.Brief, "Failed: Bash `go test ./...`") {
		t.Errorf("brief = %s", brief.Brief)
	}

	var found index.SearchResult
	get(t, s, "/api/search?q=%E6%95%B0%E6%8D%AE%E5%BA%93", &found)
	if found.Total != 1 || found.Hits[0].Turn != 2 || found.Hits[0].Session.ID != fixture.Alpha {
		t.Errorf("search = %+v", found)
	}

	for _, path := range []string{"/api/usage", "/api/usage?by=day", "/api/failures", "/api/digest", "/api/files?path=schema", "/api/projects", "/api/pack?since=7d", "/api/health"} {
		var any map[string]any
		get(t, s, path, &any)
		if len(any) == 0 {
			t.Errorf("%s answered nothing", path)
		}
	}

	for path, want := range map[string]int{
		"/api/sessions/claude/zzzz":             http.StatusNotFound,
		alpha + "/items/999":                    http.StatusNotFound,
		alpha + "/items/x":                      http.StatusBadRequest,
		alpha + "/turns?turns=seven":            http.StatusBadRequest,
		alpha + "/brief?turn=9":                 http.StatusBadRequest,
		alpha + "/export?format=docx":           http.StatusBadRequest,
		"/api/sessions?limit=many":              http.StatusBadRequest,
		"/api/sessions?since=whenever":          http.StatusBadRequest,
		"/api/sessions?source=emacs":            http.StatusBadRequest,
		"/api/usage?by=colour":                  http.StatusBadRequest,
		"/api/nothing":                          http.StatusNotFound,
		"/healthz":                              http.StatusOK,
		"/api/sessions?source=codex":            http.StatusOK, // a CLI with no store here: an empty list, not an error
		"/api/sessions/claude/" + fixture.Agent: http.StatusOK,
	} {
		if rec := (call{path: path}).do(s); rec.Code != want {
			t.Errorf("GET %s = %d, want %d (%s)", path, rec.Code, want, strings.TrimSpace(rec.Body.String()))
		} else if want >= 400 && !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("GET %s: an error is JSON too: %s", path, rec.Body)
		}
	}
	if rec := (call{method: http.MethodPost, path: "/api/sessions"}).do(s); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("POST /api/sessions = %d: nothing here can be written", rec.Code)
	}
}

func TestExportIsAlwaysADownload(t *testing.T) {
	s, _, _ := build(t, Options{})
	for format, ctype := range map[string]string{"md": "text/markdown", "json": "application/json", "jsonl": "application/x-ndjson", "html": "text/html"} {
		rec := call{path: "/api/sessions/claude/" + fixture.Alpha + "/export?format=" + format}.do(s)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), ctype) {
			t.Errorf("%s = %d %s", format, rec.Code, rec.Header().Get("Content-Type"))
		}
		if d := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(d, "attachment") || !strings.Contains(d, "claude-aaaa1111") || !strings.HasSuffix(d, "."+format+`"`) {
			t.Errorf("%s: Content-Disposition = %q: a transcript laid out as a page must not open as this origin's page", format, d)
		}
		if !strings.Contains(rec.Body.String(), "fix the flaky test") {
			t.Errorf("%s: the export does not hold the session", format)
		}
	}
}

// A reply quotes the caller — the path that matched nothing, the origin that was
// refused — and carries transcripts, which hold whatever a model fetched or a person
// pasted. Whichever it is, the JSON has no <, > or & in it: a reader that took the reply
// for a page would find no tag there.
func TestNoReplyCanBeTakenForAPage(t *testing.T) {
	s, a, h := build(t, Options{})
	const markup = `<img src=x onerror=alert(1)>&<script>alert(2)</script>`
	const gamma = "cccc3333-0000-4000-8000-000000000003"

	var rows []byte
	for _, row := range []map[string]any{
		{"type": "user", "uuid": "g-u1", "timestamp": h.Now.Add(-10 * time.Minute).Format(time.RFC3339), "sessionId": gamma, "cwd": "/work/gamma",
			"message": map[string]any{"role": "user", "content": "what does " + markup + " do"}},
		{"type": "assistant", "uuid": "g-a1", "timestamp": h.Now.Add(-9 * time.Minute).Format(time.RFC3339), "sessionId": gamma,
			"message": map[string]any{"id": "msg_g1", "role": "assistant", "model": "claude-opus-5", "stop_reason": "end_turn",
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}, "content": []map[string]any{{"type": "text", "text": "it is " + markup}}}},
	} {
		line, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(append(rows, line...), '\n')
	}
	path := filepath.Join(h.Dir, "-work-gamma", gamma+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rows, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Sync(t.Context(), index.SyncOptions{Force: true}); err != nil {
		t.Fatal(err)
	}

	session := "/api/sessions/claude/" + gamma
	for name, c := range map[string]struct {
		call
		status int
		whole  bool // the reply holds the text entire, not a cut of it
	}{
		"a path that is no endpoint":    {call{path: "/api/" + url.PathEscape(markup)}, http.StatusNotFound, true},
		"a session that does not exist": {call{path: "/api/sessions/claude/" + url.PathEscape(markup)}, http.StatusNotFound, false},
		"an origin that is refused":     {call{path: "/api/sessions", header: map[string]string{"Origin": "https://attacker.example/" + markup}}, http.StatusForbidden, true},
		"a grouping that is not one":    {call{path: "/api/usage?by=" + url.QueryEscape(markup)}, http.StatusBadRequest, false},
		"a number that is not one":      {call{path: "/api/sessions?limit=" + url.QueryEscape(markup)}, http.StatusBadRequest, false},
		"an MCP method that is not one": {call{method: http.MethodPost, path: "/mcp", body: `{"jsonrpc":"2.0","id":1,"method":"` + markup + `"}`}, http.StatusOK, true},
		"the list":                      {call{path: "/api/sessions?project=gamma"}, http.StatusOK, false},
		"a session's outline":           {call{path: session}, http.StatusOK, true},
		"its turns":                     {call{path: session + "/turns?turns=all"}, http.StatusOK, true},
		"its brief":                     {call{path: session + "/brief"}, http.StatusOK, true},
		"its export as JSON":            {call{path: session + "/export?format=json"}, http.StatusOK, true},
		"a search that finds it":        {call{path: "/api/search?q=onerror"}, http.StatusOK, false},
		"the same over MCP": {call{method: http.MethodPost, path: "/mcp",
			body: `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read","arguments":{"ref":"claude/` + gamma + `","turns":"1"}}}`}, http.StatusOK, true},
	} {
		rec := c.do(s)
		body := rec.Body.String()
		if rec.Code != c.status {
			t.Errorf("%s = %d, want %d: %s", name, rec.Code, c.status, body)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: sent as %q, nosniff %q", name, ct, rec.Header().Get("X-Content-Type-Options"))
		}
		if strings.ContainsAny(body, "<>&") {
			t.Errorf("%s: markup in the reply: %s", name, body)
		}
		// Nothing is lost by it: decoded, the text is what was sent or what the transcript holds
		var v any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Errorf("%s: %v in %s", name, err, body)
			continue
		}
		if c.whole && !holds(v, markup) {
			t.Errorf("%s: the text did not come through whole: %s", name, body)
		}
	}
}

// holds reports whether any string in a decoded JSON value contains s.
func holds(v any, s string) bool {
	switch v := v.(type) {
	case string:
		return strings.Contains(v, s)
	case []any:
		for _, e := range v {
			if holds(e, s) {
				return true
			}
		}
	case map[string]any:
		for _, e := range v {
			if holds(e, s) {
				return true
			}
		}
	}
	return false
}

func TestOnlyThisMachineWithoutAToken(t *testing.T) {
	s, _, _ := build(t, Options{})
	ok := func(c call) bool { return c.do(s).Code == http.StatusOK }
	const api = "/api/sessions"

	if !ok(call{path: api}) || !ok(call{path: api, header: map[string]string{"Host": "localhost:7717"}}) || !ok(call{path: api, header: map[string]string{"Host": "[::1]:7717"}}) {
		t.Error("a loopback name is this machine")
	}
	for name, c := range map[string]call{
		"a name rebound to 127.0.0.1":  {path: api, header: map[string]string{"Host": "attacker.example:7717"}},
		"the page, by a rebound name":  {path: "/", header: map[string]string{"Host": "attacker.example:7717"}},
		"another site's script":        {path: api, header: map[string]string{"Origin": "https://attacker.example"}},
		"another port of this host":    {path: api, header: map[string]string{"Origin": "http://127.0.0.1:9999"}},
		"another site's <img>":         {path: api, header: map[string]string{"Sec-Fetch-Site": "cross-site"}},
		"a sandboxed frame":            {path: api, header: map[string]string{"Origin": "null"}},
		"the MCP endpoint, cross-site": {method: http.MethodPost, path: "/mcp", body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`, header: map[string]string{"Origin": "https://attacker.example"}},
	} {
		if rec := c.do(s); rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", name, rec.Code)
		}
	}
	if !ok(call{path: api, header: map[string]string{"Origin": "http://127.0.0.1:7717", "Sec-Fetch-Site": "same-origin"}}) {
		t.Error("the page's own requests are refused")
	}
	if !ok(call{path: "/", header: map[string]string{"Sec-Fetch-Site": "cross-site"}}) {
		t.Error("a link to the page from elsewhere is refused: the page itself holds nothing")
	}
}

func TestCORSForListedOrigins(t *testing.T) {
	s, _, _ := build(t, Options{Origins: []string{"https://dash.example"}})
	rec := call{path: "/api/sessions", header: map[string]string{"Origin": "https://dash.example", "Sec-Fetch-Site": "cross-site"}}.do(s)
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "https://dash.example" {
		t.Errorf("a listed origin = %d, ACAO %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Error("another origin is given leave to send the cookie")
	}
	pre := call{method: http.MethodOptions, path: "/api/sessions", header: map[string]string{"Origin": "https://dash.example", "Access-Control-Request-Method": "GET"}}.do(s)
	if pre.Code != http.StatusNoContent || !strings.Contains(pre.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("preflight = %d %v", pre.Code, pre.Header())
	}
	if rec := (call{path: "/api/sessions", header: map[string]string{"Origin": "https://other.example"}}).do(s); rec.Code != http.StatusForbidden {
		t.Errorf("an unlisted origin = %d", rec.Code)
	}
}

func TestToken(t *testing.T) {
	s, _, _ := build(t, Options{Host: "0.0.0.0", Token: token})
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

	if rec := (call{path: "/api/sessions"}).do(s); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no token = %d", rec.Code)
	}
	if rec := (call{path: "/api/sessions", header: bearer(token)}).do(s); rec.Code != http.StatusOK {
		t.Errorf("the token = %d", rec.Code)
	}
	if rec := (call{path: "/api/sessions", header: map[string]string{"Host": "sessions.example.com", "Authorization": "bearer " + token}}).do(s); rec.Code != http.StatusOK {
		t.Errorf("with a token the server may be reached by any name = %d", rec.Code)
	}
	for _, open := range []string{"/", "/app.js", "/healthz", "/api/auth"} {
		if rec := (call{path: open}).do(s); rec.Code != http.StatusOK {
			t.Errorf("%s needs no token (it holds nothing) = %d", open, rec.Code)
		}
	}
	for _, shut := range []string{"/api/health", "/api/events", "/api/search?q=x", "/api/sessions/claude/" + fixture.Alpha + "/export"} {
		if rec := (call{path: shut}).do(s); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without the token = %d", shut, rec.Code)
		}
	}
	if rec := (call{method: http.MethodPost, path: "/mcp", body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`}).do(s); rec.Code != http.StatusUnauthorized {
		t.Errorf("/mcp without the token = %d", rec.Code)
	}
	var auth struct{ Required, Authenticated bool }
	json.Unmarshal(call{path: "/api/auth"}.do(s).Body.Bytes(), &auth)
	if !auth.Required || auth.Authenticated {
		t.Errorf("auth = %+v", auth)
	}

	// The page trades the token for a cookie script cannot read
	if rec := (call{method: http.MethodPost, path: "/api/login", body: `{"token":"wrong"}`}).do(s); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Errorf("a wrong token at login = %d", rec.Code)
	}
	rec := call{method: http.MethodPost, path: "/api/login", body: `{"token":"` + token + `"}`}.do(s)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	c := cookies[0]
	if c.Name != cookieName || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge < 179*24*3600 || strings.Contains(c.Value, token) {
		t.Errorf("cookie = %+v", c)
	}
	withCookie := map[string]string{"Cookie": c.Name + "=" + c.Value}
	if rec := (call{path: "/api/sessions", header: withCookie}).do(s); rec.Code != http.StatusOK {
		t.Errorf("the cookie = %d", rec.Code)
	}
	json.Unmarshal(call{path: "/api/auth", header: withCookie}.do(s).Body.Bytes(), &auth)
	if !auth.Authenticated {
		t.Error("the page is not told it is signed in")
	}

	// Nobody can mint or extend one
	exp, _, _ := strings.Cut(c.Value, ".")
	for name, value := range map[string]string{
		"a later expiry, same signature": "99999999999." + c.Value[len(exp)+1:],
		"signed with another token":      sign(signKey("another-token-entirely-0000"), time.Now().Add(time.Hour)),
		"expired":                        sign(s.key, time.Now().Add(-time.Minute)),
		"garbage":                        "garbage",
	} {
		if rec := (call{path: "/api/sessions", header: map[string]string{"Cookie": cookieName + "=" + value}}).do(s); rec.Code != http.StatusUnauthorized {
			t.Errorf("a cookie %s = %d", name, rec.Code)
		}
	}
	out := call{method: http.MethodPost, path: "/api/logout", header: withCookie}.do(s)
	if cs := out.Result().Cookies(); len(cs) != 1 || cs[0].MaxAge >= 0 {
		t.Errorf("logout does not drop the cookie: %+v", cs)
	}
}

func TestGuessingIsThrottled(t *testing.T) {
	s, _, _ := build(t, Options{Host: "0.0.0.0", Token: token})
	guess := func() int {
		return call{path: "/api/sessions", header: map[string]string{"Authorization": "Bearer not-it"}}.do(s).Code
	}
	for i := range attemptsMax {
		if code := guess(); code != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d", i+1, code)
		}
	}
	rec := call{path: "/api/sessions", header: map[string]string{"Authorization": "Bearer not-it"}}.do(s)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("guess %d = %d, want 429 with a Retry-After", attemptsMax+1, rec.Code)
	}
	// Past the limit not even the right token is compared: that is what makes it a limit
	if rec := (call{path: "/api/sessions", header: map[string]string{"Authorization": "Bearer " + token}}).do(s); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the right token while throttled = %d", rec.Code)
	}
	if rec := (call{method: http.MethodPost, path: "/api/login", body: `{"token":"` + token + `"}`}).do(s); rec.Code != http.StatusTooManyRequests {
		t.Errorf("login while throttled = %d", rec.Code)
	}
	// The window rolls over
	var l limiter
	now := time.Now()
	for range attemptsMax {
		l.failed("10.0.0.1", now)
	}
	if blocked, _ := l.blocked("10.0.0.1", now); !blocked {
		t.Error("not blocked at the limit")
	}
	if blocked, _ := l.blocked("10.0.0.2", now); blocked {
		t.Error("one address's guesses are charged to another")
	}
	if blocked, _ := l.blocked("10.0.0.1", now.Add(attemptsWindow)); blocked {
		t.Error("still blocked when the window has passed")
	}
}

func TestPage(t *testing.T) {
	s, _, _ := build(t, Options{})
	rec := call{path: "/"}.do(s)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>asq</title>") {
		t.Fatalf("/ = %d %s", rec.Code, rec.Body)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe") {
		t.Errorf("CSP allows inline code: %s", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers = %v", rec.Header())
	}
	// A path that is not a file is the page: a session's link opens that session
	if deep := (call{path: "/s/claude/" + fixture.Alpha}).do(s); deep.Code != http.StatusOK || deep.Body.String() != rec.Body.String() {
		t.Errorf("a deep link = %d", deep.Code)
	}
	if rec := (call{path: "/missing.js"}).do(s); rec.Code != http.StatusNotFound {
		t.Errorf("a file that is not there = %d", rec.Code)
	}
	js := call{path: "/app.js"}.do(s)
	if !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") || js.Header().Get("ETag") == "" {
		t.Errorf("app.js = %v", js.Header())
	}
	if again := (call{path: "/app.js", header: map[string]string{"If-None-Match": js.Header().Get("ETag")}}).do(s); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Errorf("an unchanged file asked for again = %d", again.Code)
	}
	if rec := (call{method: http.MethodPost, path: "/"}).do(s); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d", rec.Code)
	}
}

func TestLongRepliesAreCompressed(t *testing.T) {
	s, _, _ := build(t, Options{})
	path := "/api/sessions/claude/" + fixture.Beta + "/turns?turns=all&full=1"
	plain := call{path: path}.do(s)
	zipped := call{path: path, header: map[string]string{"Accept-Encoding": "gzip"}}.do(s)
	if zipped.Header().Get("Content-Encoding") != "gzip" || zipped.Body.Len() >= plain.Body.Len()/4 {
		t.Fatalf("gzip: %d bytes → %d (%q)", plain.Body.Len(), zipped.Body.Len(), zipped.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != plain.Body.String() {
		t.Error("the compressed reply is not the reply")
	}
	if small := (call{path: "/api/auth", header: map[string]string{"Accept-Encoding": "gzip"}}).do(s); small.Header().Get("Content-Encoding") != "" {
		t.Error("a reply of a few bytes was compressed")
	}
}

// The stream needs a real connection: it flushes, and it ends when the reader leaves.
func TestEventsSayWhatChanged(t *testing.T) {
	s, a, h := build(t, Options{})
	srv := httptest.NewServer(s)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := bufio.NewScanner(resp.Body)
	next := func() (event, data string) {
		for lines.Scan() {
			line := lines.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = line[6:]
			case line == "" && event != "":
				return event, data
			}
		}
		t.Fatalf("the stream ended: %v", lines.Err())
		return "", ""
	}
	if event, data := next(); event != "hello" || !strings.Contains(data, `"version"`) {
		t.Fatalf("first event = %s %s", event, data)
	}

	// A transcript grows; the next sync says so, and says nothing of what it holds
	path := filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"user","uuid":"b-u9","timestamp":"` + h.Now.Format(time.RFC3339) + `","sessionId":"` + fixture.Beta + `","cwd":"/work/beta","message":{"role":"user","content":"a private follow-up"}}` + "\n")
	f.Close()
	if _, err := a.Sync(ctx, index.SyncOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	for {
		event, data := next()
		if event != "changes" {
			continue
		}
		var got struct {
			Sessions []index.Change `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(data), &got); err != nil || len(got.Sessions) != 1 || got.Sessions[0].ID != fixture.Beta || got.Sessions[0].Source != "claude" {
			t.Fatalf("changes = %s", data)
		}
		if strings.Contains(data, "private") {
			t.Error("the stream carries content")
		}
		break
	}
	if n := s.streams.Load(); n != 1 {
		t.Errorf("open streams = %d", n)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for s.streams.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := s.streams.Load(); n != 0 {
		t.Errorf("a reader that left still holds a stream (%d)", n)
	}
}

func TestRunServesAndStops(t *testing.T) {
	s, _, _ := build(t, Options{Port: 0})
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, func(a string) { addr <- a }) }()
	var base string
	select {
	case a := <-addr:
		base = "http://" + a
	case err := <-done:
		t.Fatalf("Run: %v", err)
	}
	resp, err := http.Get(base + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("served = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
}

// A directory served live is read again for every request, and a browser asks for a
// dozen modules at once: none of them may get another request's half-read set.
func TestLivePageDirUnderConcurrentRequests(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"index.html": "<!doctype html><title>asq</title>", "app.js": "export {}", "js/a.js": "export const a = 1", "js/b.js": "export const b = 2"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, _, _ := build(t, Options{Page: os.DirFS(dir), Live: true})
	var wg sync.WaitGroup
	bad := make(chan string, 400)
	for i := range 400 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := []string{"/app.js", "/js/a.js", "/js/b.js"}[i%3]
			rec := call{path: path}.do(s)
			if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
				bad <- fmt.Sprintf("%s = %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
			}
		}()
	}
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Fatal(msg)
	}
	// An edit shows on the next request, with no restart
	os.WriteFile(filepath.Join(dir, "js/a.js"), []byte("export const a = 3"), 0o644)
	if rec := (call{path: "/js/a.js"}).do(s); !strings.Contains(rec.Body.String(), "a = 3") {
		t.Errorf("an edited file is served stale: %s", rec.Body)
	}
}
