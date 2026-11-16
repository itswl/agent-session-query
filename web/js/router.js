// Real addresses through the History API: a view is a URL that can be linked, reloaded
// and gone back to. In a session's address the fragment names a turn (#4) or a step (#4.31).

const ROUTES = [
  ['sessions', /^\/$/],
  ['search', /^\/search\/?$/],
  ['session', /^\/s\/([^/]+)\/(.+)$/],
  ['usage', /^\/usage\/?$/],
  ['failures', /^\/failures\/?$/],
  ['digest', /^\/digest\/?$/],
];

const LISTS = new Set(['sessions', 'search']);
let render = () => {};

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

export function start(fn) {
  render = fn;
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
    history.pushState({ fromList: LISTS.has(from.name) }, '', to.pathname + to.search + to.hash);
  }
  render(parse(), replace ? 'replace' : 'push');
}

// savedY is where the current entry was scrolled to when it was left, if it was.
export const savedY = () => history.state?.y;
export const cameFromList = () => Boolean(history.state?.fromList);

function follow(e) {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const a = e.target.closest('a[href]');
  if (!a || a.target || a.hasAttribute('download') || a.origin !== location.origin || a.pathname.startsWith('/api/')) return;
  e.preventDefault();
  go(a.href);
}
