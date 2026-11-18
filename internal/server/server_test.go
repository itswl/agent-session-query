package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/web"
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
	remote             string // the address the connection comes from; httptest's own when empty
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
	if c.remote != "" {
		req.RemoteAddr = c.remote
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
		"a proxy in front, no token": {ProxyHeader: "X-Real-IP"},
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
	get(t, s, alpha+"/brief?since=all", &brief)
	if !strings.Contains(brief.Brief, "## The whole session") || !strings.Contains(brief.Brief, "## Turn 1\n") {
		t.Errorf("brief of the whole session = %s", brief.Brief)
	}
	get(t, s, alpha+"/brief?from=2", &brief)
	if !strings.Contains(brief.Brief, "## From turn 2") || strings.Contains(brief.Brief, "## Turn 1\n") {
		t.Errorf("brief from turn 2 = %s", brief.Brief)
	}

	var found index.SearchResult
	get(t, s, "/api/search?q=%E6%95%B0%E6%8D%AE%E5%BA%93", &found)
	if found.Total != 1 || found.Hits[0].Turn != 2 || found.Hits[0].Session.ID != fixture.Alpha {
		t.Errorf("search = %+v", found)
	}
	// One session's own turns, named by its ref as the page names it: the words are in
	// alpha's turn 2 and in the subagent that turn started, which is not alpha
	get(t, s, "/api/search?q=missing+indexes", &found)
	if found.Total != 2 {
		t.Errorf("everywhere = %+v", found)
	}
	get(t, s, "/api/search?q=missing+indexes&per_session=0&session=claude/aaaa1111", &found)
	if found.Total != 1 || found.Sessions != 1 || found.Hits[0].Turn != 2 || found.Hits[0].Session.ID != fixture.Alpha {
		t.Errorf("in one session = %+v", found)
	}
	// No turn holds every word: the closest, marked, each with what it lacks, and the word
	// no turn holds at all
	found = index.SearchResult{}
	get(t, s, "/api/search?q=ECONNREFUSED+flaky+postgres", &found)
	if p := found.Partial; p == nil || p.Words != 3 || p.Held != 2 || found.Total != 1 || !reflect.DeepEqual(found.Missing, []string{"postgres"}) ||
		!reflect.DeepEqual(found.Hits[0].Lacks, []string{"postgres"}) || found.Hits[0].Failed != 1 {
		t.Errorf("a partial match = %+v", found)
	}
	var fails index.FailureReport
	get(t, s, "/api/failures?q=econnrefused", &fails)
	if fails.Total != 1 {
		t.Errorf("failures holding a word = %+v", fails)
	}
	get(t, s, "/api/failures?q=postgres", &fails)
	if fails.Total != 0 || len(fails.ByTool) != 1 {
		t.Errorf("failures holding a word none holds = %+v", fails)
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
		"/api/search?q=x&session=claude/zzzz":   http.StatusNotFound,
		alpha + "/items/999":                    http.StatusNotFound,
		alpha + "/items/x":                      http.StatusBadRequest,
		alpha + "/turns?turns=seven":            http.StatusBadRequest,
		alpha + "/brief?turn=9":                 http.StatusBadRequest,
		alpha + "/brief?from=9":                 http.StatusBadRequest,
		alpha + "/brief?from=x":                 http.StatusBadRequest,
		alpha + "/brief?since=soon":             http.StatusBadRequest,
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

// forwardingHeaders are the marks a proxy leaves, each of which alone says a request was
// passed on: X-Forwarded- headers beyond the three everyone knows included, since a proxy
// may add one of those and none of the others.
var forwardingHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
	"X-Forwarded-Port", "X-Forwarded-Server", "X-Forwarded-Prefix", "X-Forwarded-Ssl", "X-Real-IP", "Via", "CF-Connecting-IP"}

// A proxy or a tunnel on this machine connects from loopback and may write Host:
// localhost — cloudflared's httpHostHeader, ngrok's host-header rewrite, Caddy's
// header_up Host — and pass on what the whole network sends. Without a token, whatever
// bears a proxy's mark is refused, and written down; what reached the port directly is
// served as it was.
func TestWithoutATokenNothingComesThroughAProxy(t *testing.T) {
	var said bytes.Buffer
	s, _, _ := build(t, Options{Log: &said})
	const local = "127.0.0.1:50000"
	const ping = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	tunnel := map[string]string{"Host": "localhost", "X-Forwarded-For": "203.0.113.9", "Cf-Connecting-Ip": "203.0.113.9"}
	refusedAs := func(name string, c call) {
		t.Helper()
		rec := c.do(s)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", name, rec.Code)
			return
		}
		var body struct{ Error string }
		json.Unmarshal(rec.Body.Bytes(), &body)
		if !strings.Contains(body.Error, "has no token") || !strings.Contains(body.Error, "through a proxy") || !strings.Contains(body.Error, "ASQ_TOKEN") {
			t.Errorf("%s: the refusal does not say why, or what to do: %q", name, body.Error)
		}
	}
	refusals := 0
	for name, c := range map[string]call{
		"the API, through a tunnel that writes Host: localhost": {path: "/api/sessions", header: tunnel, remote: local},
		"the page, through it":                {path: "/", header: tunnel, remote: local},
		"the MCP endpoint, through it":        {method: http.MethodPost, path: "/mcp", body: ping, header: tunnel, remote: local},
		"the health check, through it":        {path: "/healthz", header: tunnel, remote: local},
		"through a proxy that keeps the Host": {path: "/api/sessions", header: map[string]string{"Host": "sessions.example.com", "X-Forwarded-Proto": "https"}, remote: local},
	} {
		refusedAs(name, c)
		refusals++
	}
	for _, name := range forwardingHeaders {
		refusedAs(name+" alone", call{path: "/api/sessions", header: map[string]string{"Host": "127.0.0.1:7717", name: "x"}, remote: local})
		refusals++
	}

	// What reached the port directly is served: the page and its requests, an agent's MCP
	// client, a script
	for name, c := range map[string]call{
		"a program":                           {path: "/api/sessions", remote: local},
		"the page":                            {path: "/", remote: local},
		"the page's own request":              {path: "/api/sessions", header: map[string]string{"Origin": "http://127.0.0.1:7717", "Sec-Fetch-Site": "same-origin"}, remote: local},
		"an MCP client in Node":               {method: http.MethodPost, path: "/mcp", body: ping, header: map[string]string{"Sec-Fetch-Mode": "cors", "User-Agent": "node"}, remote: local},
		"a header no proxy means anything by": {path: "/api/sessions", header: map[string]string{"X-Request-Id": "x"}, remote: local},
	} {
		if rec := c.do(s); rec.Code != http.StatusOK {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}

	// Written down once for the minute, under the name the limiter gives the caller, and
	// counted
	lines := strings.Split(strings.TrimSpace(said.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `refused "local relayed": this server has no token`) {
		t.Errorf("what was said of %d refusals: %q", refusals, said.String())
	}
	var health struct{ Guard guardCounts }
	get(t, s, "/api/health", &health)
	if g := health.Guard; g.Refused != refusals || g.WrongTokens != 0 || g.SignIns != 0 {
		t.Errorf("counted %+v, after %d refusals", g, refusals)
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

	// Nobody can mint or extend one. Least of all from the token: a cookie signed under
	// a key made of the token alone would be a way to ask whether a guess is right, and
	// one that nothing counts
	exp, _, _ := strings.Cut(c.Value, ".")
	fromTokenAlone := hmac.New(sha256.New, []byte(token))
	fromTokenAlone.Write([]byte("asq session cookie v1"))
	for name, value := range map[string]string{
		"a later expiry, same signature":        "99999999999." + c.Value[len(exp)+1:],
		"signed with the token and no secret":   sign(fromTokenAlone.Sum(nil), time.Now().Add(time.Hour)),
		"signed by a server with other secrets": sign(signKey(make([]byte, secretSize), token), time.Now().Add(time.Hour)),
		"expired":                               sign(s.key, time.Now().Add(-time.Minute)),
		"garbage":                               "garbage",
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
	// The login form is the same door: ten wrong tokens there are the ten
	s, _, _ = build(t, Options{Host: "0.0.0.0", Token: token})
	for i := range attemptsMax {
		if rec := (call{method: http.MethodPost, path: "/api/login", body: `{"token":"not-it"}`}).do(s); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong login %d = %d", i+1, rec.Code)
		}
	}
	if rec := (call{method: http.MethodPost, path: "/api/login", body: `{"token":"` + token + `"}`}).do(s); rec.Code != http.StatusTooManyRequests || len(rec.Result().Cookies()) != 0 {
		t.Errorf("the right token at login, after ten wrong ones = %d", rec.Code)
	}
	if rec := (call{path: "/api/sessions", header: map[string]string{"Authorization": "Bearer " + token}}).do(s); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the header, after ten wrong logins = %d", rec.Code)
	}

	// The window rolls over, with or without a sweep having come by
	var l limiter
	now := time.Now()
	one, other := allowance{key: "10.0.0.1", kind: "direct"}, allowance{key: "10.0.0.2", kind: "direct"}
	blocked := func(a allowance, at time.Time) (bool, time.Duration) {
		b, wait, _ := l.attempt(allowance{key: a.key}, true, at) // the right token, from nobody to remember
		return b, wait
	}
	for range attemptsMax {
		l.attempt(one, false, now)
	}
	if b, _ := blocked(one, now); !b {
		t.Error("not blocked at the limit")
	}
	if b, _ := blocked(other, now); b {
		t.Error("one address's guesses are charged to another")
	}
	if b, _ := blocked(one, now.Add(attemptsWindow)); b {
		t.Error("still blocked when the window has passed")
	}
	// An entry that outlived its window starts over where it lies: the next minute's
	// guesses are the next minute's, counted from one, sweep or no sweep
	later := now.Add(attemptsWindow)
	l.swept = later // no sweep is due: the old entry is still in the table
	l.attempt(one, false, later)
	if b, _ := blocked(one, later); b {
		t.Error("a guess in a new window was added to the old window's ten")
	}
	for range attemptsMax - 1 {
		l.attempt(one, false, later)
	}
	if b, wait := blocked(one, later.Add(time.Second)); !b || wait != attemptsWindow-time.Second {
		t.Errorf("ten guesses in the new window: blocked %v, for %v", b, wait)
	}

	// Callers known are kept to a number, and a day: the one known longest ago goes first
	l = limiter{}
	for i := range knownKept + 1 {
		l.attempt(allowance{key: fmt.Sprint(i), kind: "named"}, true, now.Add(time.Duration(i)*time.Second))
	}
	if len(l.known) != knownKept || l.knows("0", now.Add(time.Hour)) || !l.knows("1", now.Add(time.Hour)) {
		t.Errorf("%d known; the first %v, the second %v", len(l.known), l.knows("0", now.Add(time.Hour)), l.knows("1", now.Add(time.Hour)))
	}
	if l.knows("1", now.Add(knownFor+time.Second)) {
		t.Error("a caller is known past knownFor")
	}
	l.attempt(allowance{key: "local"}, true, now)
	if _, ok := l.known["local"]; ok {
		t.Error("this machine's programs were remembered: they need not be")
	}
}

// Who a wrong token is charged to. Each rule is here because without it somebody could
// shut out a caller they are not, or take a fresh allowance with every guess.
func TestGuessesAreChargedToWhoMadeThem(t *testing.T) {
	const local = "127.0.0.1:50000" // where a proxy on this machine, and this machine's own programs, connect from
	wrong, right := "Bearer not-it", "Bearer "+token
	ask := func(s *Server, remote, auth string, header map[string]string) *httptest.ResponseRecorder {
		h := map[string]string{"Authorization": auth}
		for k, v := range header {
			h[k] = v
		}
		return call{path: "/api/sessions", header: h, remote: remote}.do(s)
	}
	code := func(s *Server, remote, auth string, header map[string]string) int {
		return ask(s, remote, auth, header).Code
	}
	spend := func(t *testing.T, s *Server, remote string, header func(i int) map[string]string) {
		t.Helper()
		for i := range attemptsMax {
			if c := code(s, remote, wrong, header(i)); c != http.StatusUnauthorized {
				t.Fatalf("guess %d = %d", i+1, c)
			}
		}
	}
	same := func(h map[string]string) func(int) map[string]string {
		return func(int) map[string]string { return h }
	}
	browser := map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:7717"}
	node := map[string]string{"Sec-Fetch-Mode": "cors", "User-Agent": "node"} // what Node's fetch sends: an MCP client's

	t.Run("this machine's programs are shut out by nothing but themselves", func(t *testing.T) {
		s, _, _ := build(t, Options{Token: token})
		relayed := map[string]string{"Host": "sessions.example.com", "X-Forwarded-For": "203.0.113.9", "X-Forwarded-Proto": "https"}
		spend(t, s, local, same(relayed))
		if c := code(s, local, right, relayed); c != http.StatusTooManyRequests {
			t.Errorf("through the proxy, after its ten = %d", c)
		}
		spend(t, s, local, same(browser)) // a page in a browser here: one that rebinds its name to 127.0.0.1
		if c := code(s, local, right, browser); c != http.StatusTooManyRequests {
			t.Errorf("a page here, after its ten = %d", c)
		}
		for name, h := range map[string]map[string]string{"a program": nil, "an MCP client in Node": node} {
			if c := code(s, local, right, h); c != http.StatusOK {
				t.Errorf("%s on this machine, with the token = %d: somebody else spent its guesses", name, c)
			}
		}
		if rec := (call{method: http.MethodPost, path: "/api/login", body: `{"token":"` + token + `"}`, remote: local}).do(s); rec.Code != http.StatusOK {
			t.Errorf("signing in from a program here = %d", rec.Code)
		}
		// Each mark a proxy leaves is one, and a header no proxy means anything by is not
		for _, name := range forwardingHeaders {
			if c := code(s, local, right, map[string]string{name: "x"}); c != http.StatusTooManyRequests {
				t.Errorf("%s does not mark a request as relayed (%d)", name, c)
			}
		}
		if c := code(s, local, right, map[string]string{"X-Request-Id": "x"}); c != http.StatusOK {
			t.Errorf("a header no proxy means anything by = %d", c)
		}
		// The other way round: a program here with the token wrong is shut out by its own
		// ten, and a page in a browser is not, nor what comes through a proxy
		s, _, _ = build(t, Options{Token: token})
		spend(t, s, local, same(nil))
		if c := code(s, local, right, nil); c != http.StatusTooManyRequests {
			t.Errorf("this machine's programs, after their own ten = %d", c)
		}
		for name, h := range map[string]map[string]string{"a page in a browser": browser, "a proxy": relayed} {
			if c := code(s, local, right, h); c != http.StatusOK {
				t.Errorf("%s here, after the programs' ten = %d", name, c)
			}
		}
	})

	t.Run("a proxy that says who is calling", func(t *testing.T) {
		s, _, _ := build(t, Options{Token: token, ProxyHeader: "cf-connecting-ip"})
		from := func(addr string) map[string]string {
			return map[string]string{"Host": "sessions.example.com", "CF-Connecting-IP": addr, "X-Forwarded-For": addr}
		}
		spend(t, s, local, same(from("203.0.113.9")))
		if c := code(s, local, right, from("203.0.113.9")); c != http.StatusTooManyRequests {
			t.Errorf("the caller that guessed = %d", c)
		}
		if c := code(s, local, right, from("203.0.113.10")); c != http.StatusOK {
			t.Errorf("another caller behind the same proxy = %d: it was charged for the first one's guesses", c)
		}
		if c := code(s, local, right, nil); c != http.StatusOK {
			t.Errorf("this machine = %d", c)
		}
		// A value that is no address names nobody: the request is one of the relayed
		spend(t, s, local, same(from("unknown")))
		if c := code(s, local, right, from("_hidden")); c != http.StatusTooManyRequests {
			t.Errorf("callers the proxy did not name share an allowance; got %d", c)
		}
		// A proxy chain that names its own hop (nginx behind cloudflared: $remote_addr is
		// 127.0.0.1) puts every caller in one allowance — not this machine's programs'
		s, _, _ = build(t, Options{Token: token, ProxyHeader: "X-Real-IP"})
		spend(t, s, local, same(map[string]string{"X-Real-IP": "127.0.0.1"}))
		if c := code(s, local, right, map[string]string{"X-Real-IP": "127.0.0.1"}); c != http.StatusTooManyRequests {
			t.Errorf("the chain's own hop, after its ten = %d", c)
		}
		if c := code(s, local, right, nil); c != http.StatusOK {
			t.Errorf("this machine's programs = %d: a proxy that named 127.0.0.1 spent their guesses", c)
		}
	})

	t.Run("callers who choose what the header says share their proxy's allowance", func(t *testing.T) {
		s, _, _ := build(t, Options{Token: token, ProxyHeader: "CF-Connecting-IP"})
		in := map[string]string{"CF-Connecting-IP": "203.0.113.100"}
		if c := code(s, local, right, in); c != http.StatusOK {
			t.Fatalf("a caller with the token, before anyone guessed = %d", c)
		}
		// Every guess names a new caller: a page that reaches the proxy's port, a device
		// on the network. Ten callers' worth, and that proxy is shut to newcomers
		for i := range proxyMax {
			if c := code(s, local, wrong, map[string]string{"CF-Connecting-IP": fmt.Sprintf("198.51.%d.%d", i/250, i%250+1)}); c != http.StatusUnauthorized {
				t.Fatalf("guess %d through the proxy = %d", i+1, c)
			}
		}
		rec := ask(s, local, right, map[string]string{"CF-Connecting-IP": "203.0.113.200"})
		if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "through this proxy") {
			t.Errorf("a new caller through a proxy that has been guessed through = %d %s", rec.Code, rec.Body)
		}
		login := func(h map[string]string) int {
			return call{method: http.MethodPost, path: "/api/login", body: `{"token":"` + token + `"}`, header: h, remote: local}.do(s).Code
		}
		if c := login(map[string]string{"CF-Connecting-IP": "203.0.113.201"}); c != http.StatusTooManyRequests {
			t.Errorf("a new sign-in through it = %d", c)
		}
		// Nobody already in is shut out: a caller that has shown the token gets through,
		// asking or signing in, and its wrong tokens are its own
		if c := code(s, local, right, in); c != http.StatusOK {
			t.Errorf("a caller that had shown the token, after others spent the proxy's allowance = %d", c)
		}
		if c := login(in); c != http.StatusOK {
			t.Errorf("its sign-in = %d", c)
		}
		spend(t, s, local, same(in))
		if c := code(s, local, right, in); c != http.StatusTooManyRequests {
			t.Errorf("a caller known, after ten wrong tokens of its own = %d", c)
		}
		if c := code(s, local, right, nil); c != http.StatusOK {
			t.Errorf("this machine's programs = %d", c)
		}
		// Another proxy, on the private network, has its own
		if c := code(s, "10.1.2.3:40000", right, map[string]string{"CF-Connecting-IP": "203.0.113.202"}); c != http.StatusOK {
			t.Errorf("through another proxy = %d", c)
		}
	})

	t.Run("a proxy on this machine is one, whichever loopback address it comes from", func(t *testing.T) {
		s, _, _ := build(t, Options{Token: token, ProxyHeader: "CF-Connecting-IP"})
		// On Linux a process may connect from any of 127.0.0.0/8
		for i := range proxyMax {
			remote := fmt.Sprintf("127.%d.%d.%d:50000", 1+i/250, i%250, 1+i%7)
			if c := code(s, remote, wrong, map[string]string{"CF-Connecting-IP": fmt.Sprintf("198.51.%d.%d", i/250, i%250+1)}); c != http.StatusUnauthorized {
				t.Fatalf("guess %d = %d", i+1, c)
			}
		}
		if c := code(s, "127.9.9.9:50000", wrong, map[string]string{"CF-Connecting-IP": "198.51.9.9"}); c != http.StatusTooManyRequests {
			t.Errorf("a guess from yet another loopback address, naming yet another caller = %d", c)
		}
		if c := code(s, local, right, map[string]string{"CF-Connecting-IP": "203.0.113.5"}); c != http.StatusTooManyRequests {
			t.Errorf("a newcomer through the proxy at 127.0.0.1 = %d: the guesses were not its proxy's", c)
		}
	})

	t.Run("naming a caller does not spend what it has when it calls directly", func(t *testing.T) {
		s, _, _ := build(t, Options{Host: "0.0.0.0", Token: token, ProxyHeader: "X-Real-IP"})
		spend(t, s, "192.168.1.30:40000", same(map[string]string{"X-Real-IP": "192.168.1.20"}))
		if c := code(s, "192.168.1.20:40000", right, nil); c != http.StatusOK {
			t.Errorf("the caller named, calling directly = %d", c)
		}
		if c := code(s, "192.168.1.30:40000", right, map[string]string{"X-Real-IP": "192.168.1.20"}); c != http.StatusTooManyRequests {
			t.Errorf("the caller named, named again through the same proxy = %d", c)
		}
		if c := code(s, "192.168.1.31:40000", right, map[string]string{"X-Real-IP": "192.168.1.20"}); c != http.StatusOK {
			t.Errorf("the caller named, named through another proxy = %d", c)
		}
	})

	t.Run("a device that writes the header is a proxy of its own, and cannot reach the callers known", func(t *testing.T) {
		s, _, _ := build(t, Options{Host: "0.0.0.0", Token: token, ProxyHeader: "X-Real-IP"})
		// The owner's devices have used the token through the proxy on this machine
		for i := 101; i <= 120; i++ {
			if c := code(s, local, right, map[string]string{"X-Real-IP": fmt.Sprintf("192.168.1.%d", i)}); c != http.StatusOK {
				t.Fatalf("device %d = %d", i, c)
			}
		}
		// A device on the network, straight at the port, names every address in turn
		compared := 0
		for i := 1; i <= 254; i++ {
			for range attemptsMax {
				switch c := code(s, "192.168.1.66:40000", wrong, map[string]string{"X-Real-IP": fmt.Sprintf("192.168.1.%d", i)}); c {
				case http.StatusUnauthorized:
					compared++
				case http.StatusTooManyRequests:
				default:
					t.Fatalf("guess naming %d = %d", i, c)
				}
			}
		}
		if compared != proxyMax {
			t.Errorf("%d of its guesses were compared; its proxy's allowance is %d", compared, proxyMax)
		}
		for i := 101; i <= 120; i++ {
			if c := code(s, local, right, map[string]string{"X-Real-IP": fmt.Sprintf("192.168.1.%d", i)}); c != http.StatusOK {
				t.Errorf("device %d, after the guessing = %d", i, c)
			}
		}
	})

	t.Run("logins held open are counted as they are settled", func(t *testing.T) {
		s, _, _ := build(t, Options{Token: token})
		const held = 3 * attemptsMax
		var reading, done sync.WaitGroup
		release := make(chan struct{})
		codes := make(chan int, held)
		for range held {
			reading.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				body := &heldBody{rest: strings.NewReader(`{"token":"not-it"}`), first: reading.Done, release: release}
				req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7717/api/login", body)
				req.RemoteAddr = "198.51.100.7:40000"
				rec := httptest.NewRecorder()
				s.ServeHTTP(rec, req)
				codes <- rec.Code
			}()
		}
		reading.Wait() // every one of them is past whatever comes before its body
		close(release)
		done.Wait()
		close(codes)
		tried := 0
		for c := range codes {
			if c == http.StatusUnauthorized {
				tried++
			}
		}
		if tried != attemptsMax {
			t.Errorf("%d of %d logins held open had their token compared; the allowance is %d", tried, held, attemptsMax)
		}
	})

	t.Run("the last address of a list is the proxy's own word", func(t *testing.T) {
		s, _, _ := build(t, Options{Token: token, ProxyHeader: "X-Forwarded-For"})
		spend(t, s, local, func(i int) map[string]string {
			return map[string]string{"X-Forwarded-For": fmt.Sprintf("198.18.0.%d, 203.0.113.9", i)}
		})
		if c := code(s, local, wrong, map[string]string{"X-Forwarded-For": "198.18.0.99, 203.0.113.9"}); c != http.StatusTooManyRequests {
			t.Errorf("a caller that writes itself a new address each time = %d", c)
		}
		if c := code(s, local, right, map[string]string{"X-Forwarded-For": "203.0.113.9, 203.0.113.10:4711"}); c != http.StatusOK {
			t.Errorf("another caller, port and all = %d", c)
		}
	})

	t.Run("from the network at large the header is a caller describing itself", func(t *testing.T) {
		s, _, _ := build(t, Options{Host: "0.0.0.0", Token: token, ProxyHeader: "X-Real-IP"})
		const outside = "198.51.100.7:40000"
		spend(t, s, outside, func(i int) map[string]string { return map[string]string{"X-Real-IP": fmt.Sprintf("203.0.113.%d", i)} })
		if c := code(s, outside, wrong, map[string]string{"X-Real-IP": "203.0.113.200"}); c != http.StatusTooManyRequests {
			t.Errorf("a new claimed address bought a new allowance (%d)", c)
		}
		if c := code(s, "10.1.2.3:40000", right, map[string]string{"X-Real-IP": "203.0.113.77"}); c != http.StatusOK {
			t.Errorf("through a proxy on the private network = %d", c)
		}
	})

	t.Run("an IPv6 caller is its /64", func(t *testing.T) {
		s, _, _ := build(t, Options{Host: "::", Token: token})
		for i := range attemptsMax {
			if c := code(s, fmt.Sprintf("[2001:db8:1:2::%x]:40000", i+1), wrong, nil); c != http.StatusUnauthorized {
				t.Fatalf("guess %d = %d", i+1, c)
			}
		}
		if c := code(s, "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:40000", right, nil); c != http.StatusTooManyRequests {
			t.Errorf("a new address in the same /64 = %d", c)
		}
		if c := code(s, "[2001:db8:1:3::1]:40000", right, nil); c != http.StatusOK {
			t.Errorf("the /64 next door = %d", c)
		}
		if c := code(s, "[::ffff:192.0.2.9]:40000", right, nil); c != http.StatusOK {
			t.Errorf("an IPv4 caller on an IPv6 socket = %d", c)
		}
	})

	t.Run("a full table shuts out strangers, never this machine", func(t *testing.T) {
		s, _, _ := build(t, Options{Host: "::", Token: token, ProxyHeader: "CF-Connecting-IP"})
		const regular = "[2001:db8:ffff::1]:40000" // a caller from elsewhere that has shown the token before
		if c := code(s, regular, right, nil); c != http.StatusOK {
			t.Fatalf("a caller with the token, before the flood = %d", c)
		}
		// A flood of addresses straight at the port fills the table, and ten more fill
		// the strangers' allowance
		for i := range attemptsKept + attemptsMax {
			if c := code(s, fmt.Sprintf("[2001:db8:%x::1]:40000", i), wrong, nil); c != http.StatusUnauthorized {
				t.Fatalf("guess %d = %d", i+1, c)
			}
		}
		rec := ask(s, "[2001:db9::1]:40000", right, nil)
		if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "cannot tell apart") {
			t.Errorf("a stranger once the table is full = %d %s", rec.Code, rec.Body)
		}
		for name, h := range map[string]map[string]string{"its programs": nil, "a page in its browser": browser} {
			if c := code(s, local, right, h); c != http.StatusOK {
				t.Errorf("this machine, %s, never seen before = %d: shut out by strangers", name, c)
			}
		}
		if c := code(s, local, right, map[string]string{"CF-Connecting-IP": "203.0.113.5"}); c != http.StatusOK {
			t.Errorf("a caller through the proxy, whose strangers are counted apart = %d", c)
		}
		if c := code(s, regular, right, nil); c != http.StatusOK {
			t.Errorf("a caller that had shown the token, once the table is full = %d", c)
		}
		if n := len(s.attempts.hits); n > attemptsKept+8 {
			t.Errorf("the table holds %d entries", n)
		}
		// A full table of callers whose minute is over makes room: a stranger's wrong
		// tokens are its own again, and do not shut out the next stranger
		s.attempts.mu.Lock()
		for k, h := range s.attempts.hits {
			h.started = h.started.Add(-2 * attemptsWindow)
			s.attempts.hits[k] = h
		}
		s.attempts.swept = time.Now().Add(-time.Minute / 2) // the last sweep was not long ago
		s.attempts.mu.Unlock()
		spend(t, s, "[2001:dba::1]:40000", same(nil))
		if c := code(s, "[2001:dbb::1]:40000", right, nil); c != http.StatusOK {
			t.Errorf("a stranger, after another's ten once the flood's minute is over = %d", c)
		}
	})

	if _, err := New(nil, Options{Token: token, ProxyHeader: "X-Real-IP: 1.2.3.4"}); err == nil {
		t.Error("a proxy header that is no header name was accepted")
	}
}

// A wrong token, a caller refused for too many, a sign-in: each is a line in the log,
// naming the caller as the limiter does and never what it brought, and a count in
// /api/health for those the guards let in.
func TestWhatTheGuardsDidIsWrittenDown(t *testing.T) {
	var said bytes.Buffer
	s, _, _ := build(t, Options{Host: "0.0.0.0", Token: token, Log: &said})
	const guesser, local = "198.51.100.7:40000", "127.0.0.1:50000"
	const guess = "a-guess-at-the-token-0000"
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

	// A caller guesses past its allowance: its first wrong token and its first refusal are
	// written, the rest of the minute counted
	for range attemptsMax + 5 {
		call{path: "/api/sessions", header: bearer(guess), remote: guesser}.do(s)
	}
	// Another tries the form once; the page here signs in; an agent asks with the token,
	// and the page with its cookie, which are not news
	if rec := (call{method: http.MethodPost, path: "/api/login", body: `{"token":"` + guess + `"}`, remote: "192.0.2.50:40000"}).do(s); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong token at sign-in = %d", rec.Code)
	}
	browser := map[string]string{"Origin": "http://127.0.0.1:7717", "Sec-Fetch-Site": "same-origin"}
	rec := call{method: http.MethodPost, path: "/api/login", body: `{"token":"` + token + `"}`, header: browser, remote: local}.do(s)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("sign-in = %d", rec.Code)
	}
	cookie := rec.Result().Cookies()[0]
	for name, h := range map[string]map[string]string{"an agent": bearer(token), "the page": {"Cookie": cookie.Name + "=" + cookie.Value}} {
		if c := (call{path: "/api/sessions", header: h, remote: local}).do(s).Code; c != http.StatusOK {
			t.Fatalf("%s = %d", name, c)
		}
	}

	log := said.String()
	want := []string{
		`a wrong token from "198.51.100.7", in an Authorization header`,
		`refused "198.51.100.7": too many wrong tokens from this caller`,
		`a wrong token from "192.0.2.50", at sign-in`,
		`signed in: "local browser"`,
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != len(want) {
		t.Errorf("%d lines, want %d: %s", len(lines), len(want), log)
	}
	for i, line := range lines {
		if i < len(want) && !strings.HasSuffix(line, " "+want[i]) {
			t.Errorf("line %d = %q, want it to end %q", i+1, line, want[i])
		}
		at, _, _ := strings.Cut(line, " ")
		if _, err := time.Parse(time.RFC3339, at); err != nil {
			t.Errorf("line %d does not start with when: %q", i+1, line)
		}
	}
	for what, secret := range map[string]string{"the token": token, "a wrong token": guess, "the cookie": cookie.Value} {
		if strings.Contains(log, secret) {
			t.Errorf("the log holds %s: %s", what, log)
		}
	}

	// Counted, for those let in and nobody else
	var health struct{ Guard guardCounts }
	if err := json.Unmarshal(call{path: "/api/health", header: bearer(token), remote: local}.do(s).Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if g := health.Guard; g.WrongTokens != attemptsMax+1 || g.Refused != 5 || g.SignIns != 1 || time.Since(g.Since) > time.Minute {
		t.Errorf("counted %+v", g)
	}
	for _, open := range []string{"/api/auth", "/healthz"} {
		if body := (call{path: open, remote: guesser}).do(s).Body.String(); strings.Contains(body, "wrongTokens") || strings.Contains(body, "guard") {
			t.Errorf("%s tells anyone what the guards did: %s", open, body)
		}
	}
	if c := (call{path: "/api/health", remote: guesser}).do(s).Code; c != http.StatusUnauthorized {
		t.Errorf("/api/health without the token = %d", c)
	}
}

// A flood is the log's to sum up, not to hold: a caller's first line of each kind in a
// minute, linesMax lines in all, and once the minute is over a line that says what was
// not written. The counts go back a day, by the hour.
func TestTheLogIsNotFloodedByWhatItReportsOn(t *testing.T) {
	var out bytes.Buffer
	now := time.Date(2026, 11, 18, 15, 4, 5, 0, time.UTC)
	d := door{out: &out, began: now.Add(-48 * time.Hour)}
	written := func() int { return strings.Count(out.String(), "\n") }

	for i := range 100 { // one caller, all minute long
		d.note(now.Add(time.Duration(i)*500*time.Millisecond), wrongToken, "198.51.100.7", "in an Authorization header")
	}
	if n := written(); n != 1 {
		t.Errorf("one caller, a hundred wrong tokens in a minute: %d lines", n)
	}
	for i := range 1000 { // a thousand callers, once each
		d.note(now.Add(30*time.Second), refused, fmt.Sprintf("2001:db8:%x::/64", i), "too many wrong tokens from this caller")
	}
	if n := written(); n != linesMax {
		t.Errorf("a thousand callers in a minute: %d lines, want %d", n, linesMax)
	}
	d.flush(now.Add(59 * time.Second))
	if n := written(); n != linesMax {
		t.Errorf("the minute was summed up before it was over: %s", &out)
	}
	d.flush(now.Add(time.Minute))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if want := "the minute from 15:04:05 held more than was written: 99 wrong tokens, 991 refusals"; len(lines) != linesMax+1 || !strings.HasSuffix(lines[linesMax], want) {
		t.Errorf("the summing up = %q, want %q", lines[len(lines)-1], want)
	}
	d.flush(now.Add(2 * time.Minute))
	if n := written(); n != linesMax+1 {
		t.Errorf("a minute summed up twice: %s", &out)
	}
	// The next minute starts over
	d.note(now.Add(2*time.Minute), wrongToken, "198.51.100.7", "in an Authorization header")
	if n := written(); n != linesMax+2 {
		t.Errorf("the caller's next minute was not written: %s", &out)
	}

	for name, tc := range map[string]struct {
		at              time.Time
		wrong, refusals int
	}{
		"a moment later":            {now.Add(3 * time.Minute), 101, 1000},
		"in the 23rd hour after":    {now.Add(23 * time.Hour), 101, 1000},
		"the same hour a day after": {now.Add(24 * time.Hour), 0, 0},
	} {
		c := d.counts(tc.at)
		if c.WrongTokens != tc.wrong || c.Refused != tc.refusals || c.SignIns != 0 {
			t.Errorf("%s: counted %+v, want %d wrong, %d refused", name, c, tc.wrong, tc.refusals)
		}
		if want := tc.at.Truncate(time.Hour).Add(-23 * time.Hour); !c.Since.Equal(want) {
			t.Errorf("%s: counted since %v, want %v", name, c.Since, want)
		}
	}
	// No further back than the server's start
	fresh := door{began: now}
	if c := fresh.counts(now.Add(time.Hour)); !c.Since.Equal(now) {
		t.Errorf("a server started an hour ago counts since %v", c.Since)
	}
}

// The callers known — that showed the right token in the last day — outlive a restart, so
// that someone keeping a proxy's allowance spent cannot shut out, by waiting for an
// upgrade, a remote agent that was asking a minute before it.
func TestTheCallersKnownOutliveARestart(t *testing.T) {
	const local = "127.0.0.1:50000"
	dir := filepath.Join(t.TempDir(), "state")
	// A secret of the test's own: the mark is two bytes of what it and the token make, and
	// with a secret drawn at random two tokens would share one, now and then
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, secretFile), bytes.Repeat([]byte{7}, secretSize), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := Options{Token: token, ProxyHeader: "CF-Connecting-IP", State: dir}
	ask := func(s *Server, auth, ip string) int {
		return call{path: "/api/sessions", remote: local, header: map[string]string{"Authorization": auth, "CF-Connecting-IP": ip}}.do(s).Code
	}
	flood := func(s *Server) {
		t.Helper()
		for i := range proxyMax {
			if c := ask(s, "Bearer not-it", fmt.Sprintf("198.51.%d.%d", i/250, i%250+1)); c != http.StatusUnauthorized {
				t.Fatalf("guess %d = %d", i+1, c)
			}
		}
	}
	s, _, _ := build(t, opts)
	if c := ask(s, "Bearer "+token, "203.0.113.100"); c != http.StatusOK {
		t.Fatalf("a caller with the token = %d", c)
	}
	path := filepath.Join(dir, knownFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the callers known were not kept: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the callers known are readable by others: %v", info.Mode().Perm())
	}

	// A restart, and the proxy's allowance spent by others: the caller known still gets
	// through, a newcomer does not
	s, _, _ = build(t, opts)
	flood(s)
	if c := ask(s, "Bearer "+token, "203.0.113.100"); c != http.StatusOK {
		t.Errorf("a caller known before the restart = %d", c)
	}
	if c := ask(s, "Bearer "+token, "203.0.113.101"); c != http.StatusTooManyRequests {
		t.Errorf("a newcomer = %d", c)
	}
	mark := s.knownMark()
	if len(mark) != 4 {
		t.Errorf("the mark %q is more than two bytes: beside the secret, it would confirm a guess at the token", mark)
	}

	// The token changed: whoever knew the old one is a stranger to the new
	other := opts
	other.Token = token + "-the-next-one"
	s, _, _ = build(t, other)
	flood(s)
	if c := ask(s, "Bearer "+other.Token, "203.0.113.100"); c != http.StatusTooManyRequests {
		t.Errorf("a caller known by the token before = %d", c)
	}
	if s.knownMark() == mark {
		t.Error("two tokens are known by one mark")
	}

	// A file others could read or write is not believed
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		s, _, _ = build(t, opts)
		flood(s)
		if c := ask(s, "Bearer "+token, "203.0.113.100"); c != http.StatusTooManyRequests {
			t.Errorf("a caller known from a file others can read = %d", c)
		}
	}

	// Nor is a caller known more than a day ago
	old := `{"for":"` + mark + `","known":{"named 203.0.113.100 via local":"` + time.Now().Add(-knownFor-time.Minute).UTC().Format(time.RFC3339) + `"}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, _ = build(t, opts)
	flood(s)
	if c := ask(s, "Bearer "+token, "203.0.113.100"); c != http.StatusTooManyRequests {
		t.Errorf("a caller known more than a day ago = %d", c)
	}

	// A directory that would not take the file: the caller is written down once it does,
	// on a later request — not on every one meanwhile. Root writes into any directory, and
	// Windows does not say no by mode
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		s, _, _ = build(t, opts)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		if c := ask(s, "Bearer "+token, "203.0.113.100"); c != http.StatusOK {
			t.Fatalf("a caller with the token, the directory read-only = %d", c)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		ask(s, "Bearer "+token, "203.0.113.100")
		if _, err := os.Stat(path); err == nil {
			t.Error("a failed write was tried again at once")
		}
		s.keepMu.Lock()
		s.keepFailed = s.keepFailed.Add(-knownRetry)
		s.keepMu.Unlock()
		ask(s, "Bearer "+token, "203.0.113.100")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the callers known were not kept once the directory took them: %v", err)
		}
	}

	// A server with nowhere to keep its secret keeps no callers either
	nowhere := Options{Token: token, ProxyHeader: "CF-Connecting-IP"}
	s, _, _ = build(t, nowhere)
	if c := ask(s, "Bearer "+token, "203.0.113.100"); c != http.StatusOK || s.keepsKnown {
		t.Errorf("without a state directory: %d, keeps %v", c, s.keepsKnown)
	}
}

// A sign-in is good for 180 days, and a restart is not the end of it: the secret the
// cookies are signed with is kept. What retires them is changing the token, or removing
// the secret.
func TestSignInsOutliveARestart(t *testing.T) {
	a, _ := fixture.Open(t)
	dir := filepath.Join(t.TempDir(), "state", "asq")
	var said bytes.Buffer
	start := func(opts Options) *Server {
		t.Helper()
		opts.Page, opts.Log = page, &said
		if opts.Token != "" {
			opts.Host = "0.0.0.0"
		}
		s, err := New(a, opts)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	signIn := func(s *Server, tok string) string {
		t.Helper()
		rec := call{method: http.MethodPost, path: "/api/login", body: `{"token":"` + tok + `"}`}.do(s)
		if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
			t.Fatalf("login = %d %s", rec.Code, rec.Body)
		}
		c := rec.Result().Cookies()[0]
		return c.Name + "=" + c.Value
	}
	takes := func(s *Server, cookie string) bool {
		return call{path: "/api/sessions", header: map[string]string{"Cookie": cookie}}.do(s).Code == http.StatusOK
	}
	// What the server said of its secret: every sign-in is written down as well
	complaints := func() string {
		var kept []string
		for _, line := range strings.Split(strings.TrimSpace(said.String()), "\n") {
			if line != "" && !strings.Contains(line, " signed in: ") {
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n")
	}

	cookie := signIn(start(Options{Token: token, State: dir}), token)
	if !takes(start(Options{Token: token, State: dir}), cookie) {
		t.Error("the server was restarted and the sign-in is gone")
	}
	path := filepath.Join(dir, secretFile)
	info, err := os.Stat(path)
	if err != nil || info.Size() != secretSize {
		t.Fatalf("the secret: %v, %v", info, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the secret is readable by more than its owner: %v", info.Mode().Perm())
	}
	const other = "another-token-entirely-0000"
	if takes(start(Options{Token: other, State: dir}), cookie) {
		t.Error("the token was changed and the old cookie still works")
	}
	if takes(start(Options{Token: token}), cookie) || takes(start(Options{Token: token, State: t.TempDir()}), cookie) {
		t.Error("a server with another secret took the cookie")
	}

	// A secret that is not whole is no secret: it is replaced, and the server says nothing
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := start(Options{Token: token, State: dir})
	if takes(s, cookie) {
		t.Error("the secret was lost and the old cookie still works")
	}
	if info, err := os.Stat(path); err != nil || info.Size() != secretSize {
		t.Errorf("the damaged secret was not replaced: %v, %v", info, err)
	}
	if !takes(start(Options{Token: token, State: dir}), signIn(s, token)) {
		t.Error("the replaced secret was not kept")
	}

	// Nowhere to keep it: the server runs, sign-ins last as long as it does, and it says so
	if said := complaints(); said != "" {
		t.Errorf("said, with a directory that works: %s", said)
	}
	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s = start(Options{Token: token, State: filepath.Join(blocker, "asq")})
	if !takes(s, signIn(s, token)) {
		t.Error("without a place for the secret nobody can sign in")
	}
	if !strings.Contains(said.String(), "will not outlive this run") {
		t.Errorf("a secret that could not be kept was not mentioned: %q", &said)
	}
	// A secret others can read may be known: it is replaced, and the sign-ins it made end
	if runtime.GOOS != "windows" {
		cookie := signIn(start(Options{Token: token, State: dir}), token)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if takes(start(Options{Token: token, State: dir}), cookie) {
			t.Error("a secret readable by others was kept")
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the secret that replaced it: %v, %v", info, err)
		}
		// A directory others can write is no place for one: the server keeps none
		open := filepath.Join(t.TempDir(), "shared")
		if err := os.Mkdir(open, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(open, 0o777); err != nil { // past the umask
			t.Fatal(err)
		}
		said.Reset()
		s := start(Options{Token: token, State: open})
		if _, err := os.Stat(filepath.Join(open, secretFile)); err == nil {
			t.Error("a secret was kept in a directory anyone can write")
		}
		if !strings.Contains(said.String(), "not this user's to keep a secret in") {
			t.Errorf("the refusal was not said: %q", &said)
		}
		if !takes(s, signIn(s, token)) {
			t.Error("without a place for the secret nobody can sign in")
		}
		said.Reset()
		// One its group may write is what mkdir makes where each user has a group of their
		// own, under a umask of 002: the secret is kept there
		mine := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(mine, 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(mine, 0o775); err != nil {
			t.Fatal(err)
		}
		cookie = signIn(start(Options{Token: token, State: mine}), token)
		took := takes(start(Options{Token: token, State: mine}), cookie)
		if said := complaints(); !took || said != "" {
			t.Errorf("a directory its group may write was refused: %q", said)
		}
	}

	// Servers that start in the same instant hold one secret, not one each
	together := filepath.Join(t.TempDir(), "asq")
	secrets := make([][]byte, 16)
	var wg sync.WaitGroup
	for i := range secrets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			secret, err := loadSecret(together)
			if err != nil {
				t.Error(err)
			}
			secrets[i] = secret
		}()
	}
	wg.Wait()
	for _, secret := range secrets {
		if len(secret) != secretSize || !bytes.Equal(secret, secrets[0]) {
			t.Fatal("servers that started together hold different secrets")
		}
	}

	// Without a token there is nothing to sign, and nothing is written
	none := filepath.Join(t.TempDir(), "unused")
	start(Options{State: none})
	if _, err := os.Stat(none); err == nil {
		t.Error("a server without a token made itself a secret")
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

// The page names its script, style and fonts under a tag of this set of its files, which
// may be kept for good: a cache in between that keeps scripts whatever the server says
// (Cloudflare does, for four hours) cannot then run a page the server is no longer.
func TestThePagesFilesAreNamedForWhatTheyAre(t *testing.T) {
	s, _, _ := build(t, Options{Page: web.Files}) // the page itself: its script, its modules, its fonts
	page := call{path: "/"}.do(s).Body.String()
	tag := s.pageTag
	if len(tag) != 12 {
		t.Fatalf("tag %q", tag)
	}
	for _, ref := range []string{"/app.js", "/app.css", "/js/theme.js", "/fonts/Geist-Variable.woff2"} {
		if !strings.Contains(page, `"/a/`+tag+ref+`"`) || strings.Contains(page, `"`+ref+`"`) {
			t.Errorf("the page does not name %s under its tag", ref)
		}
	}
	for _, ref := range []string{"/manifest.webmanifest", "/favicon.ico"} {
		if !strings.Contains(page, `"`+ref+`"`) {
			t.Errorf("%s moved: only what the page runs and draws with is tagged", ref)
		}
	}
	plain := call{path: "/app.js"}.do(s)
	under := call{path: "/a/" + tag + "/app.js"}.do(s)
	if under.Code != http.StatusOK || under.Body.String() != plain.Body.String() || !strings.Contains(under.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("app.js under its tag = %d, %q", under.Code, under.Header().Get("Cache-Control"))
	}
	if plain.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("app.js by its own path = %q", plain.Header().Get("Cache-Control"))
	}
	// What the script imports, and the style's fonts, follow it under the tag
	for _, ref := range []string{"/js/api.js", "/js/views/sessions.js", "/fonts/GeistMono-Variable.woff2"} {
		if rec := (call{path: "/a/" + tag + ref}).do(s); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("%s under the tag = %d", ref, rec.Code)
		}
	}
	// A page loaded before an upgrade asks under the old tag: it is given the file as it
	// is now, not to be kept
	if old := (call{path: "/a/0123456789ab/app.js"}).do(s); old.Code != http.StatusOK || old.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("under another tag = %d, %q", old.Code, old.Header().Get("Cache-Control"))
	}
	for _, p := range []string{"/a/" + tag + "/missing.js", "/a/" + tag + "/", "/a/" + tag + "/s/claude/x"} {
		if rec := (call{path: p}).do(s); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d: under a tag only the files are", p, rec.Code)
		}
	}

	// Any file that changes is a new set, under a new tag
	files := fstest.MapFS{
		"index.html": {Data: []byte(`<link rel="stylesheet" href="/app.css"><script type="module" src="/app.js"></script>`)},
		"app.js":     {Data: []byte("1")},
		"app.css":    {Data: []byte("a{}")},
	}
	_, first, err := readPage(files)
	if err != nil {
		t.Fatal(err)
	}
	files["app.css"] = &fstest.MapFile{Data: []byte("b{}")}
	assets, second, _ := readPage(files)
	if first == second {
		t.Error("the style changed, and the tag did not")
	}
	if want := `<link rel="stylesheet" href="/a/` + second + `/app.css"><script type="module" src="/a/` + second + `/app.js"></script>`; string(assets["/index.html"].body) != want {
		t.Errorf("index = %s", assets["/index.html"].body)
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
		if event == "sync" && strings.Contains(data, `"running":true`) {
			t.Errorf("one transcript read again, and the stream announces a sync: %s", data)
		}
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

// The stores are read closely while someone is looking and loosely otherwise — and the
// watcher finds out which every couple of seconds, so that the first question after a
// quiet spell is followed at once.
func TestTheWatchersPace(t *testing.T) {
	s, _, _ := build(t, Options{})
	justNow, longAgo := time.Now(), time.Now().Add(-2*watchIdle)
	if !s.due(time.Time{}) {
		t.Error("a server that has never read its stores is not about to")
	}
	if s.due(justNow) {
		t.Error("nobody is looking, the stores were read a moment ago, and they are read again")
	}
	if !s.due(longAgo) {
		t.Error("nobody is looking, and the loose pace has stopped altogether")
	}
	s.asked.Store(time.Now().UnixNano())
	if !s.due(justNow) {
		t.Error("someone just asked, and the stores are not followed closely")
	}
	s.asked.Store(time.Now().Add(-2 * lookedFor).UnixNano())
	if s.due(justNow) {
		t.Error("the last question was long ago, and the stores are still followed closely")
	}
	s.streams.Add(1)
	if !s.due(justNow) {
		t.Error("a stream is open, and the stores are not followed closely")
	}
	s.streams.Add(-1)
	// While the watcher keeps the close pace, a question finds the index followed: the
	// answer may be older than the pause between two reads, and is not waited for
	if maxAge <= watchBusy {
		t.Errorf("an answer may be %v old and the stores are read every %v: every question would wait", maxAge, watchBusy)
	}
	if s.mcp.MaxAge != maxAge {
		t.Errorf("the MCP endpoint's answers may be %v old, the API's %v: one of them waits for what the other does not", s.mcp.MaxAge, maxAge)
	}
}

// The stream speaks of a sync when there is one to wait for. A transcript that grows is
// read again every few seconds on a machine where an agent is at work, and a status that
// said so each time would never hold still.
func TestTheStreamSpeaksOfCatchUps(t *testing.T) {
	now := time.Now()
	before := now.Add(-time.Minute) // when the stores were last read
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for name, tc := range map[string]struct {
		st     index.SyncState
		saying bool
		want   index.SyncState
	}{
		"nothing under way": {
			st:   index.SyncState{At: before, Total: 3, Done: 3},
			want: index.SyncState{At: before}},
		"one transcript read again as it grows, however long it takes": {
			st:   index.SyncState{Running: true, Total: 1, At: before, Began: ago(30 * time.Second)},
			want: index.SyncState{At: before, Began: ago(30 * time.Second)}},
		"a handful of sessions being written at once": {
			st:   index.SyncState{Running: true, Total: catchUp - 1, At: before, Began: ago(10 * time.Second)},
			want: index.SyncState{At: before, Began: ago(10 * time.Second)}},
		"many, and over before anyone could wait for them": {
			st:   index.SyncState{Running: true, Total: 300, Done: 20, At: before, Began: ago(feltAfter / 2)},
			want: index.SyncState{At: before, Began: ago(feltAfter / 2)}},
		"many, and going on": {
			st:   index.SyncState{Running: true, Total: 300, Done: 20, At: before, Began: ago(feltAfter)},
			want: index.SyncState{Running: true, Total: 300, Done: 20, At: before, Began: ago(feltAfter)}},
		"the first read, from its first moment and whatever its size": {
			st:   index.SyncState{Running: true, Total: 2, Began: now},
			want: index.SyncState{Running: true, Total: 2, Began: now}},
		"a catch-up that was announced, down to its last transcript": {
			st:     index.SyncState{Running: true, Total: 300, Done: 299, At: before, Began: ago(time.Minute)},
			saying: true,
			want:   index.SyncState{Running: true, Total: 300, Done: 299, At: before, Began: ago(time.Minute)}},
		"announced, and over": {
			st:     index.SyncState{Total: 300, Done: 300, At: now},
			saying: true,
			want:   index.SyncState{At: now}},
	} {
		if got := tell(tc.st, tc.saying, now); got != tc.want {
			t.Errorf("%s: told %+v, want %+v", name, got, tc.want)
		}
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

// heldBody is a request body whose sender is slow: it says when it is first read from,
// and gives nothing until released.
type heldBody struct {
	rest    io.Reader
	first   func()
	once    sync.Once
	release chan struct{}
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.once.Do(b.first)
	<-b.release
	return b.rest.Read(p)
}
