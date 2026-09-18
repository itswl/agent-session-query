package app

import (
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

func TestUIRoutes(t *testing.T) {
	srv, _ := newTestServer(t, "secret")

	// The page needs no authentication: it holds no data itself
	resp, err := http.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("/ui = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "/ui/app.js") || !strings.Contains(body, "/ui/style.css") {
		t.Fatalf("/ui does not reference its static assets: %s", body[:min(200, len(body))])
	}

	// /ui/ is equivalent
	if resp, err := http.Get(srv.URL + "/ui/"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("/ui/ = %v %v", resp, err)
	} else {
		resp.Body.Close()
	}

	// Static assets and their Content-Type
	for _, asset := range []struct{ path, wantType, wantText string }{
		{"/ui/app.js", "javascript", "textContent"},
		{"/ui/style.css", "text/css", "--bg"},
	} {
		resp, err := http.Get(srv.URL + asset.path)
		if err != nil {
			t.Fatal(err)
		}
		content := readBody(t, resp)
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), asset.wantType) {
			t.Fatalf("%s = %d %s", asset.path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if !strings.Contains(content, asset.wantText) {
			t.Fatalf("%s has the wrong content", asset.path)
		}
	}

	// A missing asset
	if resp, err := http.Get(srv.URL + "/ui/nope.js"); err != nil || resp.StatusCode != 404 {
		t.Fatalf("/ui/nope.js = %v %v", resp, err)
	} else {
		resp.Body.Close()
	}

	// An unauthenticated page does not mean unauthenticated data
	if resp, err := http.Get(srv.URL + "/sessions"); err != nil || resp.StatusCode != 401 {
		t.Fatalf("fetching data without a token should be 401, got %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
}

// TestUIRendersWithoutHTMLInjection: session content is text other programs wrote (tool
// output, web page bodies), so the page must render exclusively through textContent. This
// rule is pinned here so nobody later swaps in string concatenation for convenience.
func TestUIRendersWithoutHTMLInjection(t *testing.T) {
	sub, err := uiSub()
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("}
	for _, name := range []string{"index.html", "app.js", "style.css"} {
		content, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(content), bad) {
				t.Fatalf("%s contains %s; session content must render through textContent", name, bad)
			}
		}
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
		if len(buf) > 1<<20 {
			break
		}
	}
	return string(buf)
}

// TestFaviconRoute: browsers request /favicon.ico from the root on any page, and leaving
// it unhandled puts a 404 in the log on every visit (a 401 once a token is configured).
func TestFaviconRoute(t *testing.T) {
	srv, _ := newTestServer(t, "secret")

	resp, err := http.Get(srv.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Unauthenticated even with a token configured: the icon holds no data
	if resp.StatusCode != 200 {
		t.Fatalf("/favicon.ico = %d; should return 200 without authentication", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/x-icon" {
		t.Fatalf("Content-Type = %q", ct)
	}

	icon, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// ICO header: reserved 0, type 1, image count > 0
	if len(icon) < 6 || icon[0] != 0 || icon[1] != 0 || icon[2] != 1 || icon[3] != 0 {
		t.Fatalf("not a valid ICO, first 6 bytes = %v", icon[:min(6, len(icon))])
	}
	if count := int(icon[4]) | int(icon[5])<<8; count == 0 {
		t.Fatal("the ICO contains no images at all")
	}
}

// TestUIAssetCaching: app.js and style.css are embedded, so they change only when the
// binary does. http.FileServer sent no cache signal for them, and browsers fell back to
// heuristic caching — after an upgrade the page kept loading the previous app.js. The
// build version is the validator now.
func TestUIAssetCaching(t *testing.T) {
	srv, _ := newTestServer(t, "secret")
	for _, asset := range []string{"/ui/app.js", "/ui/style.css"} {
		resp, err := http.Get(srv.URL + asset)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s = %d", asset, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", asset, cc)
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			t.Fatalf("%s has no ETag: a new binary would not invalidate the browser's copy", asset)
		}
		if ct := resp.Header.Get("Content-Type"); ct == "" {
			t.Errorf("%s has no Content-Type", asset)
		}

		// An unchanged asset must answer 304 rather than re-sending the body
		req, _ := http.NewRequest(http.MethodGet, srv.URL+asset, nil)
		req.Header.Set("If-None-Match", etag)
		again, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		again.Body.Close()
		if again.StatusCode != http.StatusNotModified {
			t.Errorf("a matching ETag should be 304, got %d", again.StatusCode)
		}
	}
}

// TestUIAssetUnknownIs404: dropping http.FileServer must not turn a missing asset into an
// empty 200
func TestUIAssetUnknownIs404(t *testing.T) {
	srv, _ := newTestServer(t, "secret")
	resp, err := http.Get(srv.URL + "/ui/nope.js")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing asset = %d, want 404", resp.StatusCode)
	}
}
