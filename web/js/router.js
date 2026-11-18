// Real addresses through the History API: a view is a URL that can be linked, reloaded
// and gone back to. In a session's address the fragment names a turn (#4) or a step (#4.31).

const ROUTES = [
  ['sessions', /^\/$/],
  ['search', /^\/search\/?$/],
  ['session', /^\/s\/([^/]+)\/(.+)$/],
  ['usage', /^\/usage\/?$/],
  ['failures', /^\/failures\/?$/],
  ['files', /^\/files\/?$/],
  ['digest', /^\/digest\/?$/],
];

// The views a session opened from is gone back to: the lists, and the reports, which
// open a session from what they found
const OPENERS = new Set(['sessions', 'search', 'usage', 'failures', 'files', 'digest']);
let render = () => {};
let list = () => '/';

function decode(s) {
  try { return decodeURIComponent(s); } catch { return s; }
}

export function parse(loc = location) {
  for (const [name, re] of ROUTES) {
    const m = loc.pathname.match(re);
    if (m) return { name, args: m.slice(1).map(decode), query: new URLSearchParams(loc.search), hash: decode(loc.hash.slice(1)) };
  }
  return { name: 'notfound', args: [], query: new URLSearchParams(loc.search), hash: '' };
}

// start draws the view the address names, and every one after it. listOf is the sessions
// list a page opened from nowhere goes back to.
export function start(fn, listOf = list) {
  render = fn;
  list = listOf;
  history.scrollRestoration = 'manual';
  addEventListener('popstate', () => render(parse(), 'pop'));
  document.addEventListener('click', follow);
  render(parse(), 'load');
}

export function go(url, { replace = false } = {}) {
  const from = parse();
  const to = new URL(url, location.href);
  if (replace) {
    history.replaceState(history.state, '', to.pathname + to.search + to.hash);
  } else {
    // The entry being left keeps where it was scrolled to, for Back
    history.replaceState({ ...history.state, y: scrollY }, '');
    // A link within the page on screen — a turn of the session — is a step taken inside
    // it: the view the page was opened from is still there, that many entries back
    const within = decode(to.pathname) === decode(location.pathname) && to.search === location.search;
    const state = within ? { opened: cameFromView(), within: (history.state?.within || 0) + 1 } : { opened: OPENERS.has(from.name) };
    history.pushState(state, '', to.pathname + to.search + to.hash);
  }
  render(parse(), replace ? 'replace' : 'push');
}

// savedY is where the current entry was scrolled to when it was left, if it was.
export const savedY = () => history.state?.y;
export const cameFromView = () => Boolean(history.state?.opened);

// back leaves a page for the list or the report it was opened from, as it was left — its
// project, its window — past the steps taken within the page; a page opened from anywhere
// else leaves for the sessions list start was given.
export function back() {
  if (cameFromView()) history.go(-1 - (history.state?.within || 0));
  else go(list());
}

function follow(e) {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const a = e.target.closest('a[href]');
  if (!a || a.target || a.hasAttribute('download') || a.origin !== location.origin || a.pathname.startsWith('/api/')) return;
  e.preventDefault();
  go(a.href);
}
