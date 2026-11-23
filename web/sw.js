// The service worker. It keeps one thing: a page of its own for when the server cannot be
// reached — the machine asleep, a tunnel down — so that the installed app says so and
// offers to try again, where it would show the browser's error or the tunnel's. It keeps
// nothing else: no answer of the API, nothing of the record. Everything the page shows is
// asked of the server, as it was before there was a worker.
//
// It is named for the set of the page's files it came with (?v=, see app.js): a new set is
// a new worker, which keeps its own copy of that page and lets the old one go.

const version = new URL(self.location.href).searchParams.get('v') || 'dev';
const CACHE = 'asq-' + version;
// The page shown when the server cannot be reached, and what it is drawn with: the font
// too, which app.css asks for and would otherwise fail to come, an error in the console
const SHELL = ['/offline.html', '/app.css', '/js/theme.js', '/js/offline.js', '/icons/icon-192.png', '/fonts/Geist-Variable.woff2'];
const OFFLINE = '/offline.html';

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE)
    .then((c) => c.addAll(SHELL.map((path) => new Request(path, { cache: 'reload' }))))
    .then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys()
    .then((keys) => Promise.all(keys.filter((k) => k.startsWith('asq-') && k !== CACHE).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin || req.method !== 'GET') return;
  if (req.mode === 'navigate') {
    // The page, at any of its addresses. What the API sends — an export, a picture opened
    // in a tab — is the API's to answer, reached or not
    if (url.pathname.startsWith('/api/') || url.pathname === '/mcp' || url.pathname === '/healthz') return;
    e.respondWith(fetch(req).then(
      // asq never answers its page with a 5xx: one is a tunnel or a proxy saying the server
      // is not there
      (res) => (res.status >= 500 ? offline(res) : res),
      () => offline(Response.error())));
    return;
  }
  // What the page of its own is drawn with, which the app itself asks for by other paths
  if (SHELL.includes(url.pathname) && url.pathname !== OFFLINE) {
    e.respondWith(caches.match(url.pathname, { cacheName: CACHE }).then((kept) => kept || fetch(req)));
  }
});

function offline(otherwise) {
  return caches.match(OFFLINE, { cacheName: CACHE }).then((kept) => kept || otherwise);
}
