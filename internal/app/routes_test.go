package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestRootListsEveryRoute keeps the endpoint list at GET / honest. That list is what a
// caller reads to find out what this service can do — a shell script, or an agent that found
// the port and nothing else — and being hand-written it drifted: it named six routes while
// thirteen were served, so /ui, /search, /projects, /export and /mcp were undiscoverable
// from it.
//
// Both directions, because either one alone rots:
//
//   - every path literal in this package's routing files must be advertised, or named in
//     unlistedRoutes with the reason it is not. The literals are read out of the source, so
//     a route added without a line in the list fails here instead of going unnoticed.
//   - every advertised line must reach a route that answers, and must be gated the way its
//     "(no token)" marker says. That direction stops the list from advertising something
//     renamed or removed.
func TestRootListsEveryRoute(t *testing.T) {
	srv, _ := newTestServer(t, "secret")

	_, body := get(t, srv.URL+"/", "")
	raw, ok := body["endpoints"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("GET / advertises no endpoints: %v", body)
	}
	lines := make([]string, 0, len(raw))
	for _, entry := range raw {
		lines = append(lines, entry.(string))
	}
	advertised := strings.Join(lines, "\n")

	// Served, deliberately not advertised: the list describes the API, it is not an
	// inventory of every path the mux happens to answer.
	unlistedRoutes := map[string]string{
		"/favicon.ico": "browsers ask for it on their own",
	}

	for _, literal := range pathLiterals(t, "http.go", "ui.go") {
		if literal == "/" {
			continue // the list itself
		}
		base := strings.TrimSuffix(literal, "/") // "/ui/" is the same route as "/ui"
		if _, ok := unlistedRoutes[base]; ok {
			continue
		}
		if !strings.Contains(advertised, base) {
			t.Errorf("this package routes %q but GET / does not list it: a caller reading the list cannot find it", literal)
		}
	}

	// Every advertised line, checked against the route it names. Both requests are needed:
	// an advertised route that does not exist answers 401 to an anonymous request, exactly
	// like one that does — the auth gate runs before the 404 — so only the token-carrying
	// request can tell them apart.
	for _, line := range lines {
		path := requestPath(t, line)
		anon := probe(t, srv.URL, line, path, "")
		authed := probe(t, srv.URL, line, path, "secret")

		switch authed {
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
			t.Errorf("GET / advertises %q but a request for %s answers %d: there is no such route", line, path, authed)
		case http.StatusUnauthorized:
			t.Errorf("GET / advertises %q but it rejects the server's own token", line)
		}

		// "(no token)" is a promise, and so is its absence
		if strings.Contains(line, "(no token)") {
			if anon == http.StatusUnauthorized {
				t.Errorf("%q is marked (no token) but answers 401", line)
			}
			continue
		}
		if anon != http.StatusUnauthorized {
			t.Errorf("%q does not say (no token), so it should need one, but it answers %d", line, anon)
		}
	}
}

// pathLiterals returns every string literal that starts with "/" in the named files. Routing
// here is a chain of path comparisons rather than a table, so those literals are the only
// list of routes that exists.
func pathLiterals(t *testing.T, files ...string) []string {
	t.Helper()
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err == nil && strings.HasPrefix(value, "/") {
				seen[value] = true
			}
			return true
		})
	}
	out := make([]string, 0, len(seen))
	for literal := range seen {
		out = append(out, literal)
	}
	slices.Sort(out)
	return out
}

// requestPath turns one advertised line into something requestable:
// "GET /sessions/<pattern>/messages?limit=50 - its messages" becomes /sessions/sess-1/messages.
// The query is dropped — the point here is that the route exists and is gated as claimed,
// not what it answers to a well-formed query, which its own tests cover.
func requestPath(t *testing.T, line string) string {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("malformed endpoint line: %q", line)
	}
	path := fields[1]
	path = strings.ReplaceAll(path, "<pattern>", "sess-1") // the id newTestServer writes
	path = strings.ReplaceAll(path, "<path>", "sessions")
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	return path
}

// probe sends the request an advertised line describes, with a token only when given one.
func probe(t *testing.T, base, line, path, token string) int {
	t.Helper()
	method := strings.Fields(line)[0]
	req, err := http.NewRequest(method, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
