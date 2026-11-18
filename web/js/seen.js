// What changed since the person last looked: the marks on the sessions list and on the
// failures, and the line in the list between what is new there and what is not.
//
// The browser keeps times for this and nothing else — no text, no ids: when this visit
// began and when the page was last looked at, and per view, when it was last looked at
// and what this visit's marks are against.
//
// A visit is a stretch of looking at the page. A reload, Back from a session or a few
// minutes in another tab do not end it, so the marks hold while they are worked through;
// the page hidden or closed, or the machine asleep, for longer than AWAY does, and the
// next visit is marked against the last look of this one. The time a view was looked at
// is stored as it is left and as the page is hidden, not as it is opened: a time stored
// on opening is what the next look would be marked against, and would leave it nothing.

const AWAY = 5 * 60 * 1000;
const BEAT = 60 * 1000;
const VISIT = 'asq-visit';
const key = (view) => 'asq-seen-' + view;

// Whatever is passed, numbers are what is written
function read(k) {
  try {
    const v = JSON.parse(localStorage.getItem(k));
    return Array.isArray(v) ? v.map((t) => Number(t) || 0) : [];
  } catch {
    return [];
  }
}

function write(k, times) {
  try { localStorage.setItem(k, JSON.stringify(times.map((t) => Math.round(Number(t)) || 0))); } catch { /* storage refused: the marks last this page */ }
}

// visit is when this visit began; last when the page was last looked at, or hidden.
// A page loaded in the background takes up the visit stored before it until it is shown.
let [visit = 0, last = 0] = read(VISIT);
let shown = false; // whether last is a time the page was being looked at, rather than when it was hidden
const looking = new Set(); // the views on screen
const bases = new Map(); // what each view's marks are against in this visit
const visitFns = new Set();

// since is the time a view's marks are against: its last look before this visit, or 0
// when it has never been looked at, and nothing is marked.
export function since(view) {
  if (!bases.has(view)) {
    const [of = 0, base = 0, seen = 0] = read(key(view));
    const b = of === visit ? base : seen;
    if (of !== visit) write(key(view), [visit, b, seen]);
    bases.set(view, b);
  }
  return bases.get(view);
}

function lookedAt(view, at) {
  write(key(view), [visit, since(view), at]);
}

// look says a view is on screen; leave that it no longer is, which is when the time it
// was looked at is stored. One left while the page is hidden had it stored as it was hidden.
export const look = (view) => { looking.add(view); };
export function leave(view) {
  if (looking.delete(view) && !document.hidden) lookedAt(view, Date.now());
}

// onVisit calls fn when a new visit begins under an open view: its marks are against
// another time from then on.
export const onVisit = (fn) => { visitFns.add(fn); return () => visitFns.delete(fn); };

function hide(at) {
  for (const view of looking) lookedAt(view, at);
  last = at;
  shown = false;
  write(VISIT, [visit, last]);
}

function show() {
  const now = Date.now();
  // A beat that comes this late was held up by the machine sleeping, which no browser
  // need report: the page was hidden from the last beat that came
  if (shown && now - last >= AWAY) hide(last);
  const fresh = !(now - last < AWAY);
  if (fresh) {
    visit = now;
    bases.clear();
  }
  last = now;
  shown = true;
  write(VISIT, [visit, last]);
  if (fresh) visitFns.forEach((fn) => fn());
}

document.addEventListener('visibilitychange', () => (document.hidden ? hide(Date.now()) : show()));
setInterval(() => { if (!document.hidden) show(); }, BEAT);
if (!document.hidden) show();
