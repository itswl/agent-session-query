package web

import (
	"io/fs"
	"maps"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/app"
)

// The page has no build step, so nothing compiles it: these are the checks a bundler
// would have made, and the ones the page's rules need.

func files(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(Files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(Files, name)
		out[name] = string(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var imports = regexp.MustCompile(`(?m)(?:^|[\s;])(?:import|export)\s[^'"\n;]*?from\s*['"]([^'"]+)['"]|(?:^|[\s;])import\s*['"]([^'"]+)['"]|\bimport\(\s*['"]([^'"]+)['"]\s*\)`)

// Every module a module imports is embedded: a missing one is a blank page that no Go
// test and no syntax check would notice.
func TestEveryImportResolves(t *testing.T) {
	all := files(t)
	modules := 0
	for name, body := range all {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		modules++
		for _, m := range imports.FindAllStringSubmatch(body, -1) {
			spec := m[1] + m[2] + m[3]
			if !strings.HasPrefix(spec, ".") && !strings.HasPrefix(spec, "/") {
				t.Errorf("%s imports %q: the page loads nothing that is not its own file", name, spec)
				continue
			}
			target := strings.TrimPrefix(spec, "/")
			if strings.HasPrefix(spec, ".") {
				target = path.Join(path.Dir(name), spec)
			}
			if _, ok := all[target]; !ok {
				t.Errorf("%s imports %q, which is not there (%s)", name, spec, target)
			}
		}
	}
	if modules < 5 {
		t.Fatalf("%d modules embedded: the page is not all here", modules)
	}
}

var references = regexp.MustCompile(`(?:src|href)="([^"#]+)"|url\(["']?([^"')]+)["']?\)|"src":\s*"([^"]+)"`)

// What the page's own files point at — scripts, styles, fonts, icons — is embedded too.
func TestEveryReferenceResolves(t *testing.T) {
	all := files(t)
	for _, name := range []string{"index.html", "app.css", "manifest.webmanifest", "offline.html"} {
		body, ok := all[name]
		if !ok {
			t.Fatalf("%s is not embedded", name)
		}
		for _, m := range references.FindAllStringSubmatch(body, -1) {
			ref := m[1] + m[2] + m[3]
			if strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "/api/") {
				continue // the API is the server's, not a file
			}
			if strings.Contains(ref, "//") {
				t.Errorf("%s points outside the page: %s", name, ref)
				continue
			}
			target := strings.TrimPrefix(ref, "/")
			if !strings.HasPrefix(ref, "/") {
				target = path.Join(path.Dir(name), ref)
			}
			if _, ok := all[target]; !ok {
				t.Errorf("%s points at %s, which is not embedded", name, ref)
			}
		}
	}
}

// Transcript text is other people's text. It reaches the page as text nodes and
// attribute values and no other way: nothing here may turn a string into markup or code.
func TestNothingTurnsTextIntoMarkup(t *testing.T) {
	sinks := regexp.MustCompile(`\.innerHTML\b|\.outerHTML\b|insertAdjacentHTML|document\.write|\beval\(|new Function\(|\.srcdoc\b|setAttribute\(\s*['"](?:style|on\w+)['"]|javascript:`)
	for name, body := range files(t) {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		for i, line := range strings.Split(body, "\n") {
			code, _, _ := strings.Cut(line, "//")
			if sinks.MatchString(code) {
				t.Errorf("%s:%d: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// The page — and the worker's page for when the server cannot be reached — is served
// under a policy that allows no inline script and no inline style; markup that needs
// either would work in no browser.
func TestThePageNeedsNothingInline(t *testing.T) {
	for _, name := range []string{"index.html", "offline.html"} {
		html := files(t)[name]
		for _, bad := range []string{"<style", " style=", " onclick=", " onload=", " onerror="} {
			if strings.Contains(strings.ToLower(html), bad) {
				t.Errorf("%s has %q", name, strings.TrimSpace(bad))
			}
		}
		inline := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
		for _, m := range inline.FindAllStringSubmatch(html, -1) {
			if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
				t.Errorf("%s has an inline script: %.60s", name, m[0])
			}
		}
	}
	if !strings.Contains(files(t)["index.html"], `type="module"`) {
		t.Error("index.html loads no module")
	}
}

// What the page keeps in the browser beyond its own life is the theme and, for the marks
// of what changed since the last look, times: never an answer, a text or an id of the
// record, which would be there for whoever opens the browser next. Each is written in
// one place, and seen.js writes numbers whatever it is given: a write anywhere else is
// a decision to review, not a line to slip in.
func TestTheBrowserKeepsOnlyTheThemeAndTimes(t *testing.T) {
	writes := regexp.MustCompile(`\blocalStorage\s*(?:\.\s*setItem\b|\[|\.\s*\w+\s*=[^=])`)
	want := map[string]int{
		"app.js":     1, // the theme
		"js/seen.js": 1, // when views were last looked at
	}
	got := map[string]int{}
	for name, body := range files(t) {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		for i, line := range strings.Split(body, "\n") {
			code, _, _ := strings.Cut(line, "//")
			if !writes.MatchString(code) {
				continue
			}
			got[name]++
			if want[name] == 0 {
				t.Errorf("%s:%d writes to localStorage: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s writes to localStorage in %d places, want %d", name, got[name], n)
		}
	}
}

// The page asks for a step's pictures only where the server sends them: the same types,
// the same largest size. One the server would refuse is a broken picture on screen, and
// one the page left out a screenshot not shown.
func TestThePageAsksForThePicturesTheServerSends(t *testing.T) {
	body := files(t)["js/views/steps.js"]
	set := regexp.MustCompile(`const SHOWN = new Set\(\[([^\]]*)\]\)`).FindStringSubmatch(body)
	max := regexp.MustCompile(`const MAX_PICTURE = (\d+) << (\d+);`).FindStringSubmatch(body)
	if set == nil || max == nil {
		t.Fatal("steps.js says nowhere which pictures it shows")
	}
	shown := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(set[1], -1) {
		shown[m[1]] = true
	}
	if !maps.Equal(shown, app.Shown) {
		t.Errorf("the page shows %v, the server sends %v", shown, app.Shown)
	}
	n, _ := strconv.Atoi(max[1])
	shift, _ := strconv.Atoi(max[2])
	if n<<shift != app.MaxPicture {
		t.Errorf("the page asks for pictures up to %d bytes, the server sends them up to %d", n<<shift, app.MaxPicture)
	}
	// A picture is asked for once it is near the screen: an image whose address is set
	// before it is told to wait is fetched at once
	img := regexp.MustCompile(`h\('img', \{(.*)\}\)`).FindStringSubmatch(body)
	if img == nil || !strings.Contains(img[1], "loading: 'lazy'") || strings.Index(img[1], "loading:") > strings.Index(img[1], "src:") {
		t.Errorf("a thumbnail is made with %v: loading 'lazy' before its src", img)
	}
}
