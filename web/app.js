// The page: the bar, the view the address names, the keys, and the line to the server.

import { h, icon, tick, errorBox, dropdown, fill } from './js/dom.js';
import { api, qs, onUnauthorized, ahead } from './js/api.js';
import * as router from './js/router.js';
import * as live from './js/live.js';
import * as f from './js/format.js';
import * as sessions from './js/views/sessions.js';
import * as session from './js/views/session.js';
import * as usage from './js/views/usage.js';
import * as failures from './js/views/failures.js';
import * as files from './js/views/files.js';
import * as digest from './js/views/digest.js';
import { login } from './js/views/login.js';

const VIEWS = { sessions, search: sessions, session, usage, failures, files, digest };
const RELEASES = 'https://github.com/itswl/agent-session-query/releases/tag/';
const NAV = [['/', 'Sessions', 'sessions search session'], ['/usage', 'Usage', 'usage'], ['/failures', 'Failures', 'failures'], ['/files', 'Files', 'files'], ['/digest', 'Digest', 'digest']];
const LIST_FILTERS = ['source', 'project', 'since', 'archived'];
const HELP = [
  ['Search', [['deploy', 'words match what they begin: deployment, deployed'], ['"exit status 1"', 'a phrase, word for word'], ['-flaky', 'leave out turns that hold this word'],
    ['ask: reply: thinking:', 'look in one part of a turn'], ['tool: output:', '…or in what tools were given and gave back']]],
  ['Keys', [['/', 'search'], ['j  k', 'next, previous'], ['Enter', 'open'], ['o', "show or hide a turn's steps"], ['e', 'expand every step of the opened turns'],
    ['g g   G', 'top, bottom'], ['12 G', 'turn 12 of a session'], ['x  X', 'next, previous failed turn'], ['f', 'find in this session'],
    ['Esc', 'back to the list'], ['?', 'this help']]],
];

const main = document.getElementById('main');
const bar = document.getElementById('bar');
const notices = document.getElementById('notices');
const search = h('input', { type: 'search', id: 'q', placeholder: 'Search every turn', 'aria-label': 'Search every turn', autocomplete: 'off', spellcheck: false, enterKeyHint: 'search' });
const helpBtn = h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Search syntax and keys', title: 'Search syntax and keys (?)' }, icon('help'));
const themeBtn = h('button', { type: 'button', class: 'icon-btn' });
const syncEl = h('span', { class: 'sync', role: 'status' });
const syncDot = h('span', { class: 'dot', 'aria-hidden': 'true' });
const syncText = h('span', { class: 'sync-text' });
const brand = h('a', { class: 'brand', href: '/', 'aria-label': 'asq — sessions' }, h('img', { src: '/icons/icon-192.png', alt: '', width: 20, height: 20 }), h('span', null, 'asq'));
const versionEl = h('p', { class: 'help-foot' });
let loadedVersion = ''; // the server's version when this page was loaded: what its script is
const navLinks = NAV.map(([href, label, names]) => h('a', { href, dataset: { names } }, label));
// Pointing at a view asks for what it will show, so that it is there when the click lands
for (const a of navLinks) {
  const first = VIEWS[a.dataset.names.split(' ').pop()]?.first;
  if (!first) continue;
  const ask = () => { if (!locked && a.getAttribute('aria-current') !== 'page') ahead(first(new URLSearchParams())); };
  a.addEventListener('pointerenter', ask);
  a.addEventListener('focus', ask);
}
let view = null;
let ctrl = null;
let locked = false;
let seenHealth = null;
let typing = 0;
const dismissed = new Set(JSON.parse(sessionStorage.getItem('asq-dismissed') || '[]'));
// A dismissed notice is kept for the tab's life as a hash of what it said, not the words: a
// store's error names paths, and nothing of what the server says is kept in the browser
const hashed = (s) => {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return (h >>> 0).toString(36);
};

boot();

async function boot() {
  onUnauthorized(showLogin);
  let auth;
  try {
    auth = await api('/api/auth');
  } catch (e) {
    main.replaceChildren(h('div', { class: 'page' }, errorBox(e, () => location.reload())));
    return;
  }
  if (auth.required && !auth.authenticated) return showLogin();
  buildBar(auth.required);
  live.onStatus(renderStatus);
  live.onChanges((ev) => view?.changes?.(ev));
  live.start();
  document.addEventListener('keydown', onKey);
  setInterval(tick, 30000);
  router.start(render);
}

function showLogin() {
  if (locked) return;
  locked = true;
  live.stop();
  view?.unmount?.();
  ctrl?.abort();
  view = null;
  bar.hidden = true;
  notices.replaceChildren();
  document.removeEventListener('keydown', onKey);
  login(main);
}

function buildBar(signedIn) {
  const form = h('form', { class: 'search', role: 'search' }, icon('search'), search, h('kbd', { 'aria-hidden': 'true' }, '/'));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    clearTimeout(typing);
    runSearch();
  });
  search.addEventListener('input', () => {
    clearTimeout(typing);
    typing = setTimeout(runSearch, 200);
  });
  search.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { e.preventDefault(); search.blur(); }
  });
  const help = h('div', { class: 'help', role: 'dialog', 'aria-label': 'Search syntax and keys' },
    HELP.map(([title, rows]) => h('section', null, h('h2', null, title), h('dl', null, rows.map(([k, d]) => h('div', null, h('dt', null, h('kbd', null, k)), h('dd', null, d)))))),
    versionEl);
  themeBtn.addEventListener('click', () => {
    const next = theme() === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem('asq-theme', next); } catch { /* private mode: the choice lasts this page */ }
    renderTheme();
  });
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', renderTheme);
  fill(bar,
    brand,
    h('nav', { class: 'nav', 'aria-label': 'Views' }, navLinks),
    form,
    dropdown(helpBtn, help, 'help-drop'),
    syncEl,
    themeBtn,
    signedIn && h('button', { type: 'button', class: 'icon-btn signout', 'aria-label': 'Sign out', title: 'Sign out', onclick: signOut }, icon('logout')),
  );
  bar.hidden = false;
  renderTheme();
}

const theme = () => document.documentElement.dataset.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');

function renderTheme() {
  const dark = theme() === 'dark';
  themeBtn.replaceChildren(icon(dark ? 'sun' : 'moon'));
  themeBtn.setAttribute('aria-label', dark ? 'Use the light theme' : 'Use the dark theme');
  themeBtn.title = themeBtn.getAttribute('aria-label');
}

async function signOut() {
  await api('/api/logout', { method: 'POST' }).catch(() => null);
  location.assign('/');
}

// The search keeps the list's filters: a search is a narrower look at the same sessions
function runSearch() {
  const q = search.value.trim();
  const cur = router.parse();
  const keep = {};
  if (cur.name === 'sessions' || cur.name === 'search') for (const k of LIST_FILTERS) keep[k] = cur.query.get(k);
  if (!q) {
    if (cur.name === 'search') router.go('/' + qs(keep), { replace: true });
    return;
  }
  if (cur.name === 'search' && cur.query.get('q') === q) return;
  router.go('/search' + qs({ q, ...keep }), { replace: cur.name === 'search' });
}

function render(r, how) {
  if (locked) return;
  for (const a of navLinks) {
    if (a.dataset.names.split(' ').includes(r.name)) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  if (document.activeElement !== search) search.value = r.name === 'search' ? r.query.get('q') || '' : '';
  if (view?.update?.(r, how)) return;
  view?.unmount?.();
  ctrl?.abort();
  ctrl = new AbortController();
  main.replaceChildren();
  const mod = VIEWS[r.name];
  view = mod ? mod.mount(main, r, { signal: ctrl.signal, how }) || {} : notFound();
  if (r.name !== 'sessions' && r.name !== 'search' && r.name !== 'session') scrollTo(0, 0);
  if (how !== 'load' && document.activeElement !== search) main.querySelector('h1')?.focus({ preventScroll: true });
}

function notFound() {
  document.title = 'Not found · asq';
  main.append(h('div', { class: 'page' }, h('h1', { tabIndex: -1 }, 'Nothing here'),
    h('p', null, 'This address names no view. ', h('a', { href: '/' }, 'See the sessions'), '.')));
  return {};
}

// The status says one of three things, and says it once. It is drawn again only when it
// would read differently: a region that announces itself must not be rewritten for
// nothing, and a dot that pulses must not start over.
let shownStatus = '';
let offTimer = 0;

function renderStatus(st) {
  const s = st.sync;
  let text = 'up to date';
  let cls = 'ok';
  let title = 'The index follows the stores as they are written.' + (s?.at ? ` Last read ${f.stamp(f.when(s.at))}${s.tookMs ? `, in ${f.dur(s.tookMs)}` : ''}.` : '');
  if (s?.running) {
    cls = 'busy';
    text = s.total ? `reading ${f.num(s.done || 0)} of ${f.num(s.total)}…` : 'reading…';
    title = 'Catching up: reading transcripts into the index. What is shown so far is what has been read.';
  } else if (st.conn === 'reconnecting') {
    cls = 'off';
    text = 'reconnecting…';
    title = 'The live connection dropped. Changes will show once it is back.';
  } else if (st.conn === 'connecting') {
    cls = 'off';
    text = 'connecting…';
    title = '';
  }
  if (st.health !== seenHealth) {
    seenHealth = st.health;
    renderVersion(seenHealth?.version);
    renderWarnings();
  }
  // A connection that is a moment in coming is not news: it is said if it lasts
  clearTimeout(offTimer);
  if (cls === 'off' && !shownStatus.startsWith('off')) {
    offTimer = setTimeout(() => drawStatus(cls, text, title), 1500);
    return;
  }
  drawStatus(cls, text, title);
}

// The dot and the words are made once and changed in place: a dot made anew starts its
// pulse over, and a catch-up changes its figure every second.
function drawStatus(cls, text, title) {
  syncEl.title = title;
  if (shownStatus === cls + '\n' + text) return;
  shownStatus = cls + '\n' + text;
  if (syncEl.className !== 'sync ' + cls) syncEl.className = 'sync ' + cls;
  syncText.textContent = text;
  if (!syncEl.contains(syncDot)) syncEl.append(syncDot, syncText);
}

// A store that could not be read says so, so that its silence is not read as "no sessions"
// The version the server is, at the foot of the help and on the name in the bar. A
// release links to what it changed. A page left open while the server was upgraded runs
// the script it was loaded with, and says so (see renderWarnings).
function renderVersion(v) {
  if (!v) return;
  loadedVersion ||= v;
  brand.title = `asq ${v}`;
  const released = /^v\d+\.\d+\.\d+$/.test(v);
  fill(versionEl, 'asq ', h('span', { class: 'mono' }, v),
    released && [' · ', h('a', { href: RELEASES + v, target: '_blank', rel: 'noopener' }, 'what changed')]);
}

function renderWarnings() {
  const key = (w) => hashed(w.source + '\n' + w.error);
  const now = seenHealth?.version;
  const updated = loadedVersion && now && now !== loadedVersion && !dismissed.has(hashed('version ' + now)) &&
    h('div', { class: 'notice warn', role: 'status' }, icon('alert'),
      h('p', null, `asq is now ${now}; this page was loaded from ${loadedVersion}. Reload to use the new one.`),
      h('button', { type: 'button', class: 'btn', onclick: () => location.reload() }, 'Reload'),
      h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Dismiss the notice about the update', onclick() {
        dismissed.add(hashed('version ' + now));
        sessionStorage.setItem('asq-dismissed', JSON.stringify([...dismissed]));
        renderWarnings();
      } }, icon('x')));
  notices.replaceChildren(...[updated].filter(Boolean), ...(seenHealth?.warnings || []).filter((w) => !dismissed.has(key(w))).map((w) =>
    h('div', { class: 'notice warn', role: 'alert' }, icon('alert'),
      h('p', null, h('strong', null, w.source), ' could not be read: ', w.error, w.location && h('span', { class: 'dim' }, ` (${w.location})`)),
      h('button', { type: 'button', class: 'icon-btn', 'aria-label': `Dismiss the notice about ${w.source}`, onclick() {
        dismissed.add(key(w));
        sessionStorage.setItem('asq-dismissed', JSON.stringify([...dismissed]));
        renderWarnings();
      } }, icon('x')))));
}

function onKey(e) {
  if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
  if (e.target === search) {
    if (e.key === 'ArrowDown' && view?.key) {
      e.preventDefault();
      search.blur();
      view.key({ key: 'j', target: document.body });
    }
    return;
  }
  if (e.target.closest?.('input, textarea, select, [contenteditable="true"]')) return;
  if (e.key === '/') {
    e.preventDefault();
    search.focus();
    search.select();
  } else if (e.key === '?') {
    e.preventDefault();
    helpBtn.click();
  } else if (view?.key?.(e)) {
    e.preventDefault();
  }
}
