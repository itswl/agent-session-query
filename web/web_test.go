package web

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
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
	for _, name := range []string{"index.html", "app.css", "manifest.webmanifest"} {
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

// The page is served under a policy that allows no inline script and no inline style;
// markup that needs either would work in no browser.
func TestThePageNeedsNothingInline(t *testing.T) {
	html := files(t)["index.html"]
	for _, bad := range []string{"<style", " style=", " onclick=", " onload=", " onerror="} {
		if strings.Contains(strings.ToLower(html), bad) {
			t.Errorf("index.html has %q", strings.TrimSpace(bad))
		}
	}
	inline := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	for _, m := range inline.FindAllStringSubmatch(html, -1) {
		if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
			t.Errorf("index.html has an inline script: %.60s", m[0])
		}
	}
	if !strings.Contains(html, `type="module"`) {
		t.Error("index.html loads no module")
	}
}
