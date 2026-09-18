package app

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// The page is baked into the binary with go:embed: no npm, no build step, still a
// single-file distribution.
//
//go:embed ui
var uiAssets embed.FS

// uiSub is the static asset tree with the "ui/" prefix stripped
func uiSub() (fs.FS, error) {
	return fs.Sub(uiAssets, "ui")
}

// uiCSP is the page's content security policy.
//
// The page renders third-party text (tool output, web page bodies), and the cost of XSS
// here is the token in localStorage being read out. Rendering exclusively through
// textContent is the first line of defence (pinned by ui_test.go); the CSP is the second:
// same-origin scripts and styles only, nothing inline, and no framing by anyone else.
// The page has no inline script or style, so adding this required no other change.
const uiCSP = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; " +
	"base-uri 'none'; form-action 'none'"

// serveUIAssets handles /ui and /ui/*.
//
// The page itself carries no data (that needs a token), so like /health it needs no
// authentication. Actual session content still requires an Authorization header.
func serveUIAssets(w http.ResponseWriter, r *http.Request, path string) {
	sub, err := uiSub()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal server error"})
		return
	}

	w.Header().Set("Content-Security-Policy", uiCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")

	if path == "/ui" || path == "/ui/" {
		index, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal server error"})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The page is embedded, so a new binary should mean a new page: do not let the
		// browser hold on to a stale copy
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(versionedAssetRefs(index, sub))
		return
	}

	// Assets are embedded, so they change only when the binary does. http.FileServer
	// sends no cache signal for them (go:embed carries no mtime), which leaves browsers
	// on heuristic caching — after an upgrade the page kept loading the previous app.js.
	// The build version is the natural validator: a new binary invalidates the copy, an
	// unchanged one still answers 304 without re-sending 50 KB.
	name := strings.TrimPrefix(path, "/ui/")
	data, err := fs.ReadFile(sub, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	etag := assetETag(name, data)
	w.Header().Set("Content-Type", assetContentType(name))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(data)
}

// versionedAssetRefs stamps the page's asset URLs with a content hash.
//
// The ETag on an asset is only consulted if the browser asks — and a copy cached before
// there were any cache headers carries no validator, so it is never revalidated and the
// page keeps running old JavaScript through an upgrade. A URL that changes with the
// content cannot be served from a stale cache at all, and the HTML that names it is
// itself no-cache, so the new URL is what the browser sees next.
func versionedAssetRefs(index []byte, fsys fs.FS) []byte {
	page := string(index)
	for _, name := range []string{"app.js", "style.css"} {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		stamp := hex.EncodeToString(sum[:6])
		page = strings.ReplaceAll(page, "/ui/"+name, "/ui/"+name+"?v="+stamp)
	}
	return []byte(page)
}

// assetETag identifies one embedded asset by its content.
//
// Deriving it from the build version alone looked right and was not: a local build has
// the constant version "dev", so rebuilding changed the bytes while the ETag stayed the
// same and the browser kept serving its old copy. Hashing the bytes is correct in both
// cases — a release changes the tag, a working copy changes the content, and neither
// changes without a new ETag.
func assetETag(name string, data []byte) string {
	if cached, ok := assetETags.Load(name); ok {
		return cached.(string)
	}
	sum := sha256.Sum256(data)
	etag := `"` + buildVersion + "-" + hex.EncodeToString(sum[:8]) + `"`
	assetETags.Store(name, etag)
	return etag
}

var assetETags sync.Map // name → etag, filled on first request

// assetContentType names the type explicitly: the set is small and fixed, and
// http.FileServer's extension lookup is no longer in the path.
func assetContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".webmanifest"):
		return "application/manifest+json"
	}
	return "application/octet-stream"
}

func isUIPath(path string) bool {
	return path == "/ui" || strings.HasPrefix(path, "/ui/")
}

// serveFavicon serves /favicon.ico and returns the status code actually written.
//
// Browsers request this from the site root on any page — not just /ui, but the JSON at
// / as well. Left unhandled, every page view leaves a 404 in the access log, or a 401
// once a token is configured, which reads like someone probing the service. The icon
// carries no data, so like /ui it needs no authentication.
func serveFavicon(w http.ResponseWriter) int {
	sub, err := uiSub()
	if err == nil {
		var icon []byte
		if icon, err = fs.ReadFile(sub, "favicon.ico"); err == nil {
			w.Header().Set("Content-Type", "image/x-icon")
			w.Header().Set("Content-Length", strconv.Itoa(len(icon)))
			// The icon is embedded and cannot change within one binary
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(icon)
			return http.StatusOK
		}
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal server error"})
	return http.StatusInternalServerError
}
