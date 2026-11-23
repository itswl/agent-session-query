package web

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The worker is run in node against a cache and a network of its own: what it keeps and
// what it answers are matters of the requests it is handed, which no syntax check sees.
const workerRun = `
const base = 'https://asq.test';
const stores = new Map([['asq-old', new Map([['/offline.html', 'an older page']])]]);
const handlers = {};
let network = null;
globalThis.self = globalThis;
self.location = new URL(base + '/sw.js?v=abc');
self.addEventListener = (type, fn) => { handlers[type] = fn; };
self.skipWaiting = async () => {};
self.clients = { claim: async () => {} };
const Native = Request;
globalThis.Request = class extends Native { constructor(u, o) { super(typeof u === 'string' ? new URL(u, base) : u, o); } };
globalThis.fetch = async (req) => network(new URL(req.url).pathname);
globalThis.caches = {
  async open(name) {
    if (!stores.has(name)) stores.set(name, new Map());
    const kept = stores.get(name);
    return { async addAll(reqs) { for (const r of reqs) { const p = new URL(r.url).pathname; kept.set(p, await (await fetch(r)).text()); } } };
  },
  async keys() { return [...stores.keys()]; },
  async delete(name) { return stores.delete(name); },
  async match(path, { cacheName } = {}) {
    const text = stores.get(cacheName)?.get(path);
    return text == null ? undefined : new Response(text, { status: 200 });
  },
};
await import('./sw.mjs');
const settle = async (type) => { const waits = []; handlers[type]({ waitUntil: (p) => waits.push(p) }); await Promise.all(waits); };
network = async (path) => new Response('kept ' + path);
await settle('install');
await settle('activate');
const out = { caches: [...stores.keys()], kept: [...stores.get('asq-abc').keys()], visits: [] };
const nets = {
  up: async (path) => new Response('the server: ' + path, { status: 200 }),
  down: async () => { throw new TypeError('Failed to fetch'); },
  tunnel: async () => new Response('<h1>Error 1033</h1>', { status: 530, headers: { 'Content-Type': 'text/html' } }),
};
for (const [method, mode, path, net] of JSON.parse(process.argv[2])) {
  network = nets[net];
  let answer = null;
  handlers.fetch({ request: { url: base + path, method, mode }, respondWith: (p) => { answer = p; } });
  if (!answer) { out.visits.push('not answered'); continue; }
  const res = await answer;
  out.visits.push(res.type === 'error' ? 'error' : res.status + ' ' + await res.text());
}
out.after = [...stores.get('asq-abc').keys()];
console.log(JSON.stringify(out));
`

// The worker keeps a page of its own and what it is drawn with, and nothing else: no
// answer of the API, nothing of the record, whatever passes through. It shows that page
// when the page is asked for and the server cannot be reached — no network, or a tunnel
// answering for a server that is not there — and leaves the API to answer for itself.
func TestTheWorkerKeepsOnlyAPageOfItsOwn(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here to run the worker in")
	}
	cases := []struct {
		name                    string
		method, mode, path, net string
		want                    string
	}{
		{"the page, reached", "GET", "navigate", "/s/claude/a1", "up", "200 the server: /s/claude/a1"},
		{"the page, no network", "GET", "navigate", "/s/claude/a1", "down", "200 kept /offline.html"},
		{"the page, the tunnel's error", "GET", "navigate", "/usage", "tunnel", "200 kept /offline.html"},
		{"an export opened", "GET", "navigate", "/api/sessions/claude/a1/export", "down", "not answered"},
		{"an answer of the API", "GET", "cors", "/api/sessions", "up", "not answered"},
		{"a picture", "GET", "no-cors", "/api/sessions/claude/a1/items/2/attachments/0", "down", "not answered"},
		{"the stream", "GET", "cors", "/api/events", "down", "not answered"},
		{"a sign-in", "POST", "cors", "/api/login", "up", "not answered"},
		{"the page's style, by its tag", "GET", "no-cors", "/a/abc/app.css", "up", "not answered"},
		{"what its own page is drawn with", "GET", "no-cors", "/app.css", "down", "200 kept /app.css"},
		// put after the page's origin, it reads as asq.test@other.test: another host
		{"another origin", "GET", "navigate", "@other.test/", "down", "not answered"},
	}
	var arg [][]string
	for _, c := range cases {
		arg = append(arg, []string{c.method, c.mode, c.path, c.net})
	}
	dir := t.TempDir()
	src, err := fs.ReadFile(Files, "sw.js")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"sw.mjs": string(src), "run.mjs": workerRun} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(arg)
	cmd := exec.Command(node, filepath.Join(dir, "run.mjs"), string(raw))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Caches, Kept, Visits, After []string
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil || len(got.Visits) != len(cases) {
		t.Fatalf("node said %q (%v)", out, err)
	}
	shell := []string{"/offline.html", "/app.css", "/js/theme.js", "/icons/icon-192.png"}
	if !slices.Equal(got.Caches, []string{"asq-abc"}) || !slices.Equal(got.Kept, shell) || !slices.Equal(got.After, shell) {
		t.Errorf("caches %v holding %v, then %v: one, of this set of files, holding its own page and what draws it", got.Caches, got.Kept, got.After)
	}
	for i, c := range cases {
		if got.Visits[i] != c.want {
			t.Errorf("%s: %s, want %s", c.name, got.Visits[i], c.want)
		}
	}
	// What it keeps is the page's own files, there to be kept
	all := files(t)
	for _, path := range shell {
		if _, ok := all[strings.TrimPrefix(path, "/")]; !ok {
			t.Errorf("the worker keeps %s, which is not embedded", path)
		}
	}
	if m := regexp.MustCompile(`const SHELL = \[([^\]]*)\]`).FindStringSubmatch(string(src)); m == nil || strings.Count(m[1], "'")/2 != len(shell) {
		t.Errorf("sw.js keeps other files than the test knows of: %v", m)
	}
}
