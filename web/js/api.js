// The API, and the addresses of things in it and on the page.

let lost = () => {};
export const onUnauthorized = (fn) => { lost = fn; };

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

export const isAbort = (e) => e?.name === 'AbortError';

// What a view was last answered. A view that is opened again shows this at once and asks
// for the fresh answer behind it, so that going from one view to another is not a wait
// for the network. It lives as long as the page does, holds the last few answers, and is
// emptied when the sign-in is lost.
const KEPT = 24;
const answers = new Map();

const forgetFns = new Set();
// era counts the sign-ins lost. An answer asked for before one is not kept after it: a
// request already on its way, such as one asked for ahead, can land later.
let era = 0;

export const last = (path) => answers.get(path)?.data;
// onForget registers what else must be let go of when the sign-in is lost
export const onForget = (fn) => { forgetFns.add(fn); };
export function forget() {
  era++;
  answers.clear();
  forgetFns.forEach((fn) => fn());
}

function keepAnswer(path, text, data) {
  const had = answers.get(path);
  answers.delete(path);
  // The same text is the same answer: handing back the object already held lets a view
  // see by identity that there is nothing to draw again
  const entry = had?.text === text ? had : { text, data };
  answers.set(path, entry);
  if (answers.size > KEPT) answers.delete(answers.keys().next().value);
  return entry.data;
}

// api asks the server. With keep, the answer is remembered for last(), and an answer that
// has not changed is the very object returned before.
export async function api(path, { signal, method = 'GET', body, keep = false } = {}) {
  const askedIn = era;
  let res;
  try {
    res = await fetch(path, {
      method, signal, credentials: 'same-origin',
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch (e) {
    if (isAbort(e)) throw e;
    throw new ApiError(0, 'The server could not be reached. Is asq still running?');
  }
  // A request given up while its body was on the way is given up, not an error: the
  // view that gave it up has moved on, and must not be told it failed
  const text = await res.text().catch((e) => { if (isAbort(e)) throw e; return ''; });
  let data = null;
  try { data = JSON.parse(text); } catch { /* not JSON: an error page from something in between */ }
  if (res.status === 401 && path !== '/api/login') {
    // The page is left first, and what it was showing let go of after: leaving puts the
    // list on screen away to come back to, and that too must go
    lost();
    forget();
  }
  if (!res.ok) throw new ApiError(res.status, data?.error || `${res.status} ${res.statusText}`);
  return keep && data != null && askedIn === era ? keepAnswer(path, text, data) : data;
}

// ahead asks for what a view will want before it is opened — when a link to it is
// pointed at — so that the answer is there when it is. Nothing is asked twice in a row,
// and nothing is said if it fails: the view will ask again and say so itself.
const asked = new Map();
export function ahead(path) {
  if (Date.now() - (asked.get(path) || 0) < 5000) return;
  asked.set(path, Date.now());
  api(path, { keep: true }).catch(() => {});
}

// qs renders parameters, leaving out the empty ones; true is "1".
export function qs(params) {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v == null || v === '' || v === false) continue;
    u.set(k, v === true ? '1' : String(v));
  }
  const s = u.toString();
  return s ? '?' + s : '';
}

// A ref is "source/id"; the id is everything after the first slash.
export function splitRef(ref) {
  const i = ref.indexOf('/');
  return [ref.slice(0, i), ref.slice(i + 1)];
}

const path = (ref) => splitRef(ref).map(encodeURIComponent).join('/');
export const sessionApi = (ref) => '/api/sessions/' + path(ref);
export const sessionHref = (ref, at) => '/s/' + path(ref) + (at != null ? '#' + at : '');
