// Read-only page for browsing agent sessions.
//
// Two hard rules:
//   1. Session content is text other programs wrote (tool output, web page bodies, ...).
//      Render it exclusively through textContent; never assign built-up HTML — the token
//      lives in localStorage, so landing an XSS here means leaking it.
//   2. The token stays in this browser's localStorage and travels in the Authorization
//      header, never in a URL.
//
// Rendering strategy: auto-refresh must not rip away what you are reading. So the list is
// patched incrementally by sessionId, the detail pane is only refetched when the selected
// session really changed, and rebuilds preserve expanded blocks, opened rounds and scroll
// position.
'use strict';

const TOKEN_KEY = 'agent-session-query-token';
// How you like to look at the list, as opposed to what you are looking at (that lives in
// the URL hash). Losing the grouping on every reload was the complaint that added this.
const VIEW_KEY = 'agent-session-query-view';
// Light or dark, when you have said so; absent, the page follows the system. theme.js
// reads the same key before the first paint, so keep the two spellings identical.
const THEME_KEY = 'agent-session-query-theme';
const MESSAGE_LIMIT = 200;
const REFRESH_MS = 10000;
const SEARCH_DEBOUNCE_MS = 150;
// Content search keeps its own, slower beat: it reads session files on the server (and
// stops once the page has filled), so it waits for a real pause in typing and ignores
// queries too short to narrow anything.
const CONTENT_DEBOUNCE_MS = 300;
const CONTENT_MIN_CHARS = 2;
// Blocks fold past a threshold so one tool dump does not drown the conversation. The
// threshold depends on what the block is: the messages themselves are what you read
// (a high bar), while tool output and thinking are supporting material you consult
// (a low bar). Both shared 600 before, which left whole screens of shell output open.
const FOLD_AT = { text: 600, thinking: 300, toolResult: 200, toolCall: 400 };
// A round's final reply is the answer; it stays open unless it runs to pages
const FOLD_FINAL_AT = 4000;
const STICK_TO_BOTTOM_PX = 48;
// How close to an edge the reader has to get before the next page is fetched
const SCROLL_LOAD_PX = 320;

const state = {
  token: '',
  authRequired: true,
  serverVersion: '',   // what /health said; a poll answered by another version reloads the page
  insets: null,        // what the system covers of the page, measured or assumed
  sessions: [],
  byId: new Map(),
  selectedId: '',
  keyword: '',
  source: '',
  view: 'all',         // all = rounds with their work / conversation = the words alone / changes = file changes alone
  expandAll: false,    // every round's work open, rather than folded to one line
  roundOverrides: new Map(), // round key → open or folded, chosen by hand over expandAll
  order: 'desc',       // desc = the latest N (the end of a session is the interesting part)
  grouping: 'time',    // time = by update time; project = grouped by project (cwd)
  pane: 'list',        // phones only: which of the three screens is up (list is home)
  hideList: false,     // wide screens: fold the session list away
  hideSide: false,     // wide screens: fold the details pane away
  content: null,       // content search for the current keyword:
                       //   { query, searching, results, matched, scanned, tookMs,
                       //     truncated: { sessions, hits, scan } }
  contentTimer: null,
  contentAbort: null,  // AbortController for the search still in flight
  detail: null,        // { sessionId, signature, messages, final }
  focusAt: '',         // when set, the stream window is anchored at this time (a search hit)
  find: null,          // find in the session: { query, sessionId, span, targets, current,
                       //   lastKey, whole, server: { pending, before, after, inside, more } }
  findTimer: null,
  findAbort: null,     // the session-scoped search find asks the server
  failureAt: '',       // the step key of the failure jumped to last
  reveal: null,        // what to bring into view once the next window paints
  roundsIndex: null,   // the open session's /rounds, read when a failure jump leaves the window
  exportFormat: 'md',  // md / jsonl / json / html
  loadingMore: false,  // a page is in flight
  edgeLock: '',        // after mobile paging, move away from the edge before paging again
  noMore: { older: false, newer: false }, // an edge that came back empty
  collapsedGroups: new Set(), // project names folded away in By project grouping
  msgCounts: new Map(),       // sessionId → message count, learned as sessions are opened
  countWarmTimer: null,       // short polling while background file counts warm
  countWarmAttempts: 0,
  openBlocks: new Set(),
  timer: null,
  searchTimer: null,
  busy: false,
  pendingDetail: false,  // a selection arrived mid-fetch; re-run once the fetch settles
  loadedAt: '',
};

const $ = (id) => document.getElementById(id);

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

async function api(path, options) {
  const headers = {};
  if (state.token) headers.Authorization = 'Bearer ' + state.token;
  const res = await fetch(path, Object.assign({ headers }, options));
  if (res.status === 401) {
    const err = new Error('unauthorized');
    err.status = 401;
    throw err;
  }
  if (!res.ok) {
    let detail = 'HTTP ' + res.status;
    try {
      const body = await res.json();
      if (body && body.error) detail = body.error;
    } catch (e) { /* a non-JSON response leaves just the status code */ }
    const err = new Error(detail);
    err.status = res.status;
    throw err;
  }
  return res.json();
}

// ---------------------------------------------------------------------------
// Rendering helpers (textContent / createElement only)
// ---------------------------------------------------------------------------

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null && text !== '') node.textContent = String(text);
  return node;
}

// setText only touches the DOM when the content actually changed, so a refresh does not
// wipe out whatever the user had selected
function setText(node, text) {
  const value = text === undefined || text === null ? '' : String(text);
  if (node.textContent !== value) node.textContent = value;
}

function kv(pairs) {
  const box = el('div', 'kv');
  for (const [label, value] of pairs) {
    if (value === undefined || value === null || value === '') continue;
    const item = el('span');
    item.appendChild(el('b', '', label));
    item.appendChild(document.createTextNode(' ' + String(value)));
    box.appendChild(item);
  }
  return box;
}

function setStatus(text) {
  setText($('status'), text);
}

// idleStatus is the default line. A content search overwrites it with its own summary,
// and the ten-second refresh must not undo that while the user is still reading it.
function idleStatus() {
  const found = state.content;
  if (found && !found.searching && contentIsCurrent(found)) {
    // A '+' when the scan stopped at the page size: matched is then a lower bound
    const matched = found.truncated && found.truncated.scan ? found.matched + '+' : found.matched;
    return '\u201c' + found.query + '\u201d · ' + matched + ' in message bodies · ' +
      found.scanned + ' scanned · ' + found.tookMs + ' ms';
  }
  return state.sessions.length + ' sessions · updated ' + state.loadedAt;
}

function button(className, label, onClick) {
  const node = el('button', className, label);
  node.type = 'button';
  node.addEventListener('click', onClick);
  return node;
}

// chevron and infoGlyph draw the two glyphs the phone screens navigate by. SVG built from
// nodes, like everything else here: no markup strings.
function svgIcon(viewBox, paths) {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', viewBox);
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  for (const d of paths) {
    const path = document.createElementNS(ns, 'path');
    path.setAttribute('d', d);
    svg.appendChild(path);
  }
  return svg;
}
function chevron(direction) {
  return svgIcon('0 0 16 16', [direction === 'left' ? 'M10 3.5 5.5 8l4.5 4.5' : 'M6 3.5 10.5 8 6 12.5']);
}
// branchGlyph: the git drawing — a line that leaves one and rejoins
function branchGlyph() {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = svgIcon('0 0 16 16', ['M4.5 6v4', 'M4.5 10.5c0-2 7-1 7-3.5']);
  for (const [cx, cy] of [[4.5, 4], [4.5, 12], [11.5, 4.5]]) {
    const dot = document.createElementNS(ns, 'circle');
    dot.setAttribute('cx', String(cx));
    dot.setAttribute('cy', String(cy));
    dot.setAttribute('r', '1.9');
    svg.appendChild(dot);
  }
  return svg;
}

// findGlyph: the magnifier the session search box carries, drawn the same way
function findGlyph() {
  const svg = svgIcon('0 0 16 16', ['M10.6 10.6 14 14']);
  const circle = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
  circle.setAttribute('cx', '7');
  circle.setAttribute('cy', '7');
  circle.setAttribute('r', '4.6');
  svg.insertBefore(circle, svg.firstChild);
  return svg;
}

function infoGlyph() {
  const svg = svgIcon('0 0 16 16', ['M8 7.5v4M8 5v.5']);
  const circle = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
  circle.setAttribute('cx', '8');
  circle.setAttribute('cy', '8');
  circle.setAttribute('r', '6');
  svg.insertBefore(circle, svg.firstChild);
  return svg;
}

// segmented renders a group of mutually exclusive buttons (which slice / which view). An
// option may carry an icon; on a phone the stylesheet keeps the icon and drops the label
// where the row is too narrow for both.
function segmented(options, current, onPick, className) {
  const wrap = el('div', 'seg' + (className ? ' ' + className : ''));
  for (const [value, label, icon] of options) {
    const btn = button('seg-btn' + (value === current ? ' on' : ''), '', () => onPick(value));
    if (icon) btn.appendChild(el('span', 'seg-icon', icon));
    btn.appendChild(el('span', 'seg-label', label));
    btn.title = label;
    wrap.appendChild(btn);
  }
  return wrap;
}

// Relative time. updatedAt is spelled differently by each source (with or without T / Z /
// milliseconds), so normalise to UTC before parsing. Anything uncertain is returned as-is,
// which merely degrades the list to showing the raw string.
function relTime(iso) {
  if (!iso) return '';
  let normalized = String(iso).trim().replace(' ', 'T');
  if (!/[Zz]|[+-]\d{2}:?\d{2}$/.test(normalized)) normalized += 'Z';
  const t = Date.parse(normalized);
  if (Number.isNaN(t)) return String(iso);
  const diff = Date.now() - t;
  if (diff < 0) return String(iso); // clock skew; showing the raw value is the honest answer
  if (diff < 60e3) return 'just now';
  if (diff < 3600e3) return Math.floor(diff / 60e3) + ' min ago';
  if (diff < 86400e3) return Math.floor(diff / 3600e3) + ' h ago';
  if (diff < 7 * 86400e3) return Math.floor(diff / 86400e3) + ' d ago';
  const d = new Date(t);
  const month = String(d.getUTCMonth() + 1).padStart(2, '0');
  const day = String(d.getUTCDate()).padStart(2, '0');
  return (d.getUTCFullYear() !== new Date().getUTCFullYear() ? d.getUTCFullYear() + '-' : '') + month + '-' + day;
}

// Source / status tags. className only ever uses whitelisted values; source is a
// server-side enum but is still never concatenated in directly
const SOURCE_CLASSES = ['claude', 'codex', 'gemini', 'grok', 'hermes', 'openclaw', 'opencode', 'pi'];
function sourceClass(source) {
  if (SOURCE_CLASSES.includes(source)) return 'tag ' + source;
  // A labeled instance (claude:box-2) is not one of the known CLIs; it still gets the
  // neutral dot so a source tag never reads as a status
  return 'tag tagged';
}
function sourceTag(source) {
  return el('span', sourceClass(source), source);
}
function statusTag(status) {
  const cls = status === 'done' ? ' done' : status === 'running' ? ' running' : '';
  return el('span', 'tag' + cls, status);
}

// ---------------------------------------------------------------------------
// Theme. The stylesheet follows the system until <html> carries data-theme; this sets it,
// remembers it, and — the one piece of thought in here — drops it again the moment your
// choice coincides with the system's, so a preference never outlives its reason: switch
// away from what the system shows and the page holds it; switch back and it follows the
// system again, including the next time the system changes.
// ---------------------------------------------------------------------------

const systemLight = matchMedia('(prefers-color-scheme: light)');

function systemTheme() {
  return systemLight.matches ? 'light' : 'dark';
}

// shownTheme is what is on screen: the pinned theme, or the system's
function shownTheme() {
  return document.documentElement.dataset.theme || systemTheme();
}

// applyTheme pins a theme ('light' / 'dark') or, given '', follows the system again. It
// also keeps the button's words and the browser chrome colour in step.
function applyTheme(theme) {
  if (theme) document.documentElement.dataset.theme = theme;
  else delete document.documentElement.dataset.theme;
  const shown = shownTheme();
  const button = $('theme');
  if (button) {
    const next = shown === 'dark' ? 'light' : 'dark';
    button.title = 'Switch to the ' + next + ' theme' + (theme ? '' : ' (following the system)');
    button.setAttribute('aria-label', 'Switch to the ' + next + ' theme');
  }
  // Both theme-color tags say the same thing while a theme is pinned; following the
  // system they are set to the system's colour, which is what their media queries would
  // have picked anyway
  for (const meta of document.querySelectorAll('meta[name="theme-color"]')) {
    meta.content = shown === 'dark' ? '#0a0a0a' : '#fafafa';
  }
}

function toggleTheme() {
  const next = shownTheme() === 'dark' ? 'light' : 'dark';
  const pinned = next === systemTheme() ? '' : next;
  try {
    if (pinned) localStorage.setItem(THEME_KEY, pinned);
    else localStorage.removeItem(THEME_KEY);
  } catch (e) {
    // private mode, or storage full: the theme changes, it just does not stick
  }
  applyTheme(pinned);
}

// ---------------------------------------------------------------------------
// Token gate
// ---------------------------------------------------------------------------

function showGate(message) {
  $('gate').classList.remove('hidden');
  $('app').classList.add('hidden');
  setText($('gate-error'), message || '');
  $('gate-token').focus();
}

// ---------------------------------------------------------------------------
// What the system covers of the page
// ---------------------------------------------------------------------------

// envInsets reads the four safe-area insets the browser gives, through a probe element:
// env() is a CSS value, and a padding is the way to a number in script.
function envInsets() {
  const probe = el('div', 'safe-probe');
  document.body.appendChild(probe);
  const style = getComputedStyle(probe);
  const out = {
    top: Math.round(parseFloat(style.paddingTop) || 0),
    right: Math.round(parseFloat(style.paddingRight) || 0),
    bottom: Math.round(parseFloat(style.paddingBottom) || 0),
    left: Math.round(parseFloat(style.paddingLeft) || 0),
  };
  probe.remove();
  return out;
}

function isInstalled() {
  return matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
}

// applySafeInsets writes the insets the layout keeps clear of. Normally they are the
// browser's own. An installed web app with a translucent status bar is the exception that
// cost several releases: it draws under the status bar and the home indicator and reports
// every inset as zero, so the head came out beneath the status bar with nothing to tap.
// There the page is as tall as the whole screen while something is drawn over it, which is
// what this detects — and the bar's height has to be assumed, since nothing reports it. A
// screen half again taller than it is wide is one of the tall iPhones, whose status bar is
// 44pt and home indicator 34pt; anything else is an older one with a 20pt bar and no
// indicator. Assumed once at a size change, never while scrolling: a static padding cannot
// oscillate with the system the way a correction applied to the scroll position did.
function applySafeInsets() {
  const root = document.documentElement;
  const env = envInsets();
  let { top, bottom } = env;
  const portrait = window.innerHeight >= window.innerWidth;
  const screenHeight = portrait
    ? Math.max(screen.width, screen.height)
    : Math.min(screen.width, screen.height);
  const coversTheScreen = window.innerHeight >= screenHeight - 1;
  if (isInstalled() && coversTheScreen && !top) {
    const tall = Math.max(screen.width, screen.height) / Math.min(screen.width, screen.height) >= 1.9;
    top = tall ? 44 : 20;
    if (!bottom && tall) bottom = 34;
  }
  state.insets = { top, right: env.right, bottom, left: env.left, assumed: top !== env.top };
  root.style.setProperty('--safe-top', top + 'px');
  root.style.setProperty('--safe-bottom', bottom + 'px');
}
window.addEventListener('resize', applySafeInsets);
window.addEventListener('orientationchange', applySafeInsets);

function showApp() {
  $('gate').classList.add('hidden');
  $('app').classList.remove('hidden');
  // With no token configured there is nothing to change
  $('logout').classList.toggle('hidden', !state.authRequired);
  applySafeInsets();
}

// ---------------------------------------------------------------------------
// Left pane: the session list (patched incrementally by sessionId, never rebuilt whole)
// ---------------------------------------------------------------------------

function visibleSessions() {
  const keyword = state.keyword.toLowerCase();
  return state.sessions.filter((s) => {
    if (state.source && s.source !== state.source) return false;
    if (!keyword) return true;
    return [s.sessionId, s.shortKey, s.key, s.cwd]
      .filter(Boolean)
      .some((field) => String(field).toLowerCase().includes(keyword));
  });
}

// listRows lays out the rows to display: by time that is just a run of sessions, by
// project each group gets a header in front of it. Headers carry an id too, so the
// incremental patch below can reuse their nodes exactly like any other row.
function listRows() {
  const sessions = visibleSessions();
  let rows;
  if (state.grouping !== 'project') {
    rows = sessions.map((s) => ({ kind: 'session', id: s.sessionId, session: s }));
  } else {
    // sessions is already newest-first, so Map insertion order puts the most recently
    // touched project first
    const groups = new Map();
    for (const s of sessions) {
      const name = s.project || '(no project)';
      if (!groups.has(name)) groups.set(name, []);
      groups.get(name).push(s);
    }
    rows = [];
    for (const [name, items] of groups) {
      // A collapsed group keeps its header (with the full count, so it stays
      // informative and can be unfolded) and drops its session rows
      const collapsed = state.collapsedGroups.has(name);
      rows.push({
        kind: 'group', id: 'group:' + name, name,
        count: items.length, active: items.some((s) => s.isActive), collapsed,
      });
      if (collapsed) continue;
      for (const s of items) rows.push({ kind: 'session', id: s.sessionId, session: s });
    }
  }
  return rows.concat(contentRows(sessions));
}

// contentRows is the second half of the list: sessions whose message bodies matched but
// whose metadata did not, so they are not already listed above. The metadata half is
// local and instant; this half arrives from the server a moment later.
function contentRows(shown) {
  const found = state.content;
  if (!found || !contentIsCurrent(found)) return [];

  // The server narrowed the scan to the chosen source already; this keeps the exact
  // instance, because a mode asked for there also reaches its labeled instances
  // (claude:box2) while this list shows one at a time
  const listed = new Set(shown.map((s) => s.sessionId));
  const extra = (found.results || []).filter((hit) =>
    !listed.has(hit.sessionId) && (!state.source || hit.source === state.source));

  const rows = [{ kind: 'content-head', id: 'content-head', found, extra: extra.length }];
  for (const hit of extra) rows.push({ kind: 'hit', id: 'hit:' + hit.sessionId, session: hit });
  return rows;
}

function buildGroup(id) {
  const node = el('div', 'group');
  node.dataset.id = id;
  node.tabIndex = 0;
  node.setAttribute('role', 'button');
  node.appendChild(el('span', 'caret'));
  node.appendChild(el('span', 'group-name'));
  node.appendChild(el('span', 'group-count'));
  // The name is the key into collapsedGroups; the row id carries it with a prefix
  node.addEventListener('click', () => toggleGroup(node.dataset.id));
  node.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      toggleGroup(node.dataset.id);
    }
  });
  return node;
}

// visibleProjectNames is the set of group headers currently on screen
function visibleProjectNames() {
  const names = new Set();
  for (const session of visibleSessions()) names.add(session.project || '(no project)');
  return [...names];
}

// foldAllGroups flips every project at once: it collapses when anything is open, and
// opens everything when they all are — which is what the button's label promises
function foldAllGroups() {
  const names = visibleProjectNames();
  if (!names.length) return;
  const collapse = !names.every((name) => state.collapsedGroups.has(name));
  for (const name of names) {
    if (collapse) state.collapsedGroups.add(name);
    else state.collapsedGroups.delete(name);
  }
  saveViewPrefs();
  renderList();
  syncFoldAllButton();
}

// The button only means something while the list is grouped by project, and says which
// way it will go next
function syncFoldAllButton() {
  const button = $('fold-groups');
  if (!button) return;
  button.classList.toggle('hidden', state.grouping !== 'project');
  const names = state.grouping === 'project' ? visibleProjectNames() : [];
  const everyFolded = names.length > 0 && names.every((n) => state.collapsedGroups.has(n));
  button.textContent = everyFolded ? 'Expand all' : 'Collapse all';
  button.title = everyFolded ? 'Expand every project' : 'Collapse every project';
}

function toggleGroup(rowId) {
  const name = rowId.replace(/^group:/, '');
  if (state.collapsedGroups.has(name)) state.collapsedGroups.delete(name);
  else state.collapsedGroups.add(name);
  saveViewPrefs();
  renderList();
  syncFoldAllButton(); // folding one group can flip the button's promise
}

function fillGroup(node, row) {
  const [caret, name, count] = node.children;
  // Show only the last segment of the project path; the full path goes in the title
  const short = String(row.name).split(/[\\/]/).filter(Boolean).pop() || row.name;
  setText(caret, row.collapsed ? '\u25b8' : '\u25be');
  setText(name, short);
  // Say what the number counts. A bare "2" next to a row reading "3726 msgs" reads as a
  // second message count; it is the number of sessions in this project.
  setText(count, row.count + (row.count === 1 ? ' session' : ' sessions'));
  node.title = row.name + ' · ' + row.count + (row.count === 1 ? ' session' : ' sessions') +
    ' — click to ' + (row.collapsed ? 'expand' : 'collapse');
  node.classList.toggle('collapsed', !!row.collapsed);
  node.setAttribute('aria-expanded', row.collapsed ? 'false' : 'true');
  node.classList.toggle('live', !!row.active);
}

function buildRow(row) {
  if (row.kind === 'group') return buildGroup(row.id);
  if (row.kind === 'content-head') return buildContentHead(row.id);
  if (row.kind === 'hit') return buildHit(row.id, row.session.sessionId);
  return buildItem(row.id);
}

function buildItem(sessionId) {
  const item = el('div', 'item');
  item.dataset.id = sessionId;
  item.setAttribute('role', 'option');
  item.tabIndex = -1;
  item.addEventListener('click', () => selectSession(item.dataset.id));

  const row = el('div', 'row1');
  row.appendChild(el('span', 'tag'));   // source
  row.appendChild(el('span', 'live'));  // recent-activity dot (newest message inside activeWindow)
  row.appendChild(el('span', 'msgs'));  // message count (from the source, or learned on open)
  row.appendChild(el('span', 'time'));  // relative time
  item.appendChild(row);
  item.appendChild(el('div', 'key'));
  item.appendChild(el('div', 'meta'));
  return item;
}

function fillItem(item, session) {
  const [tag, live, msgs, time] = item.children[0].children;
  const cls = sourceClass(session.source);
  if (tag.className !== cls) tag.className = cls;
  setText(tag, session.source);
  // File-backed sources always report status done, so "running" can only be inferred
  // from the update time (the server works this out)
  // The dot means the newest message is recent, which is all the server can know — so
  // the tooltip gives that fact rather than asserting the session is being written
  live.classList.toggle('on', !!session.isActive);
  live.title = session.isActive ? 'last message ' + relTime(session.updatedAt) : '';
  // The server counts file-backed sessions in a background worker. Use that count as
  // soon as it arrives; while the first list is warming, say so instead of leaving a
  // misleading blank that only gets filled after opening the session.
  const cached = state.msgCounts.get(session.sessionId);
  const n = Number(session.messageCount) || Number(cached) || 0;
  const countable = ['pi', 'claude', 'codex', 'gemini', 'grok'].includes(session.source);
  setText(msgs, n ? n + (n === 1 ? ' msg' : ' msgs') : countable ? 'counting…' : '');
  setText(time, relTime(session.updatedAt));
  time.title = session.updatedAt || '';

  const key = item.children[1];
  setText(key, session.shortKey || session.sessionId);
  key.title = session.key || '';

  const meta = item.children[2];
  setText(meta, session.cwd || '');
  meta.classList.toggle('hidden', !session.cwd);
}

// The metadata filter came up empty. If something was typed, the user was most likely
// expecting it to search message bodies, so offer that here instead of dead-ending: the
// placeholder does say "press Enter", but it is hidden the moment there is text to read it.
function emptyListState() {
  if (!state.sessions.length) return el('p', 'empty-list', 'No sessions yet');
  const keyword = state.keyword.trim();
  if (!keyword) return el('p', 'empty-list', 'No matching sessions');

  // Only reached for a query too short to have triggered the automatic content search
  const box = el('div', 'empty-list');
  box.appendChild(el('p', '', 'No sessionId, path or cwd matches \u201c' + keyword + '\u201d'));
  box.appendChild(button('ghost', 'Search message bodies anyway', () => runContentSearch(keyword)));
  return box;
}

function renderList() {
  const list = $('list');
  const rows = listRows();

  if (rows.length === 0) {
    list.replaceChildren(emptyListState());
    return;
  }

  // Index the existing nodes by id: reusable ones just get their text updated, and only
  // the rest are created. A repeated id drops the earlier node: the map can hold one
  // per id, so the others would never be reached again — not by this render nor by the
  // stale sweep at its end — and would sit in the list through every filter that should
  // have removed them. (Two rows do share an id when a source reports one sessionId for
  // two sessions; the server no longer does, and this is what kept it visible.)
  const existing = new Map();
  for (const node of Array.from(list.children)) {
    const id = node.dataset && node.dataset.id;
    if (!id || existing.has(id)) node.remove();
    else existing.set(id, node);
  }

  rows.forEach((row, index) => {
    let node = existing.get(row.id);
    if (node) existing.delete(row.id);
    else node = buildRow(row);

    if (row.kind === 'group') {
      fillGroup(node, row);
    } else if (row.kind === 'content-head') {
      fillContentHead(node, row);
    } else {
      if (row.kind === 'hit') fillHit(node, row.session);
      else fillItem(node, row.session);
      // Hit rows carry a prefixed row id, so compare against the session id itself
      const active = row.session.sessionId === state.selectedId;
      node.classList.toggle('active', active);
      node.setAttribute('aria-selected', active ? 'true' : 'false');
    }

    // insertBefore moves an existing node rather than rebuilding it, so reordering
    // loses no state
    if (list.children[index] !== node) list.insertBefore(node, list.children[index] || null);
  });

  for (const stale of existing.values()) stale.remove();
}

// ---------------------------------------------------------------------------
// Content search. Typing filters metadata locally and instantly; the same keystrokes also
// schedule a server-side scan of the message bodies, whose results are appended below the
// metadata matches rather than replacing them. Enter just skips the wait.
// ---------------------------------------------------------------------------

// contentIsCurrent: the body hits answer what the list is showing now — the same keyword,
// searched within the same source
function contentIsCurrent(found) {
  return found.query === state.keyword && (found.source || '') === (state.source || '');
}

function cancelContentSearch() {
  if (state.contentTimer) {
    clearTimeout(state.contentTimer);
    state.contentTimer = null;
  }
  // Aborting matters on both ends: the browser drops a response it would discard anyway,
  // and the server stops a scan that holds every core it can get
  if (state.contentAbort) {
    state.contentAbort.abort();
    state.contentAbort = null;
  }
}

function scheduleContentSearch(keyword) {
  cancelContentSearch();
  if (keyword.length < CONTENT_MIN_CHARS) {
    if (state.content) {
      state.content = null;
      renderList();
    }
    return;
  }
  state.contentTimer = setTimeout(() => runContentSearch(keyword), CONTENT_DEBOUNCE_MS);
}

async function runContentSearch(keyword) {
  const text = String(keyword || '').trim();
  cancelContentSearch();
  // CONTENT_MIN_CHARS throttles the automatic path only: asking for a one-character
  // search outright is allowed
  if (!text) {
    state.content = null;
    renderList();
    return;
  }

  const controller = new AbortController();
  state.contentAbort = controller;
  // The source filter travels with the search. Filtering the page that came back instead
  // could leave nothing for the chosen source: the scan stops once a page of sessions has
  // matched, and newer hits from the other sources can fill it first.
  const scope = state.source;
  state.content = { query: text, source: scope, searching: true, results: [] };
  renderList();

  try {
    const data = await api('/search?q=' + encodeURIComponent(text) + '&limit=50' +
      (scope ? '&source=' + encodeURIComponent(scope) : ''), { signal: controller.signal });
    // A newer keystroke may have landed while this was in flight
    if (controller.signal.aborted || state.keyword !== text) return;
    state.content = Object.assign({ query: text, source: scope, searching: false }, data);
    state.contentAbort = null;
    renderList();
    setStatus(idleStatus());
  } catch (err) {
    if (err.name === 'AbortError') return; // superseded, not a failure
    state.content = null;
    renderList();
    handleError(err);
    setStatus('Search failed: ' + err.message);
  }
}

function buildContentHead(id) {
  const node = el('div', 'content-head');
  node.dataset.id = id;
  node.appendChild(el('span', 'content-head-label'));
  node.appendChild(el('span', 'dim'));
  return node;
}

function fillContentHead(node, row) {
  const [label, note] = node.children;
  const found = row.found;
  if (found.searching) {
    setText(label, 'Searching message bodies\u2026');
    setText(note, '');
    return;
  }
  if (!found.matched) {
    setText(label, 'Nothing in message bodies');
    setText(note, '');
    return;
  }
  setText(label, row.extra
    ? row.extra + ' more in message bodies'
    : 'Also in message bodies, all listed above');
  // matched counts every scanned session with a hit, including the ones already listed
  // above. truncated is an object, so it is always truthy: read the reason this note is
  // about. When the scan itself stopped at the page size, matched is a lower bound, so the
  // note says that rather than printing "(of 50)" for a number that is exactly 50.
  let tail = '';
  if (found.truncated?.sessions) {
    tail = found.truncated.scan
      ? '(showing the first 50 \u2014 the scan stopped there)'
      : '(of ' + found.matched + ', showing the first 50)';
  }
  setText(note, tail);
}

function buildHit(rowId, sessionId) {
  const item = el('div', 'item hit');
  item.dataset.id = rowId;
  item.dataset.sid = sessionId;
  item.setAttribute('role', 'option');
  item.tabIndex = -1;
  item.addEventListener('click', () => {
    const first = (item._hit && item._hit.matches && item._hit.matches[0]) || {};
    // Land on the words, not only near them: the find bar takes the query, and the match
    // the window is anchored at is the one shown
    const keyword = state.keyword.trim();
    if (keyword) {
      $('find').value = keyword;
      $('find-bar').classList.remove('hidden');
      state.reveal = { kind: 'find', sessionId: item.dataset.sid, direction: 1, at: parseTime(first.timestamp) };
    }
    selectSession(item.dataset.sid, first.timestamp || '');
    // Already open at that very match, nothing is fetched and nothing would land it
    if (state.reveal && !state.busy) settleReveal();
  });

  const row = el('div', 'row1');
  row.appendChild(el('span', 'tag'));        // source
  row.appendChild(el('span', 'live'));       // live dot
  row.appendChild(el('span', 'hit-count'));  // how many hits in this session
  row.appendChild(el('span', 'time'));
  item.appendChild(row);
  item.appendChild(el('div', 'key'));
  item.appendChild(el('div', 'snippet'));
  return item;
}

function fillHit(item, hit) {
  item._hit = hit; // the click handler needs the match timestamp to anchor the window
  const [tag, live, count, time] = item.children[0].children;
  const cls = sourceClass(hit.source);
  if (tag.className !== cls) tag.className = cls;
  setText(tag, hit.source);
  live.classList.toggle('on', !!hit.isActive);
  live.title = hit.isActive ? 'last message ' + relTime(hit.updatedAt) : '';
  setText(count, hit.matchCount + (hit.matchCount === 1 ? ' hit' : ' hits'));
  setText(time, relTime(hit.updatedAt));
  time.title = hit.updatedAt || '';

  const key = item.children[1];
  setText(key, hit.shortKey || hit.sessionId);
  key.title = hit.key || '';

  // Only the first hit: the left pane has no room for more, and opening the session shows
  // the rest in context
  const first = (hit.matches || [])[0] || {};
  const snippet = item.children[2];
  setText(snippet, first.snippet || '');
  snippet.title = (first.role || '') + ' · ' + (first.timestamp || '');
}


function renderSources() {
  const select = $('source-filter');
  const counts = new Map();
  for (const s of state.sessions) counts.set(s.source, (counts.get(s.source) || 0) + 1);
  const sources = [...counts.keys()].sort();

  // Do not rebuild when the options have not changed: that would interrupt a dropdown
  // the user is currently operating
  const signature = sources.map((s) => s + ':' + counts.get(s)).join(',');
  if (select.dataset.sig === signature) return;
  select.dataset.sig = signature;

  // state.source is the restored preference on the first render and the select's own
  // value afterwards; without this a restored filter was dropped on the first refresh
  const current = state.source || select.value;
  const all = el('option', '', 'All sources');
  all.value = '';
  const options = [all];
  for (const source of sources) {
    const option = el('option', '', source + ' (' + counts.get(source) + ')');
    option.value = source;
    options.push(option);
  }
  select.replaceChildren(...options);
  select.value = sources.includes(current) ? current : '';
  const before = state.source;
  state.source = select.value;
  // The source a body search ran within has left the list, taking the filter with it
  if (state.source !== before && state.content) runContentSearch(state.keyword);
}

// ---------------------------------------------------------------------------
// Middle pane: session identity + toolbar + the conversation, read by rounds
//
// A session is read the way it happened: you asked, the agent worked, the agent
// answered. The loaded messages are grouped into those rounds — by the rule the server's
// /rounds uses, so the two never disagree about where one ends — and the work in the
// middle folds into one line: how many steps, how many commands, which files changed,
// what failed, how long it took. Failures stay in view when the rest is folded;
// everything else opens on demand. Three views: the whole thing, the conversation
// alone, or only the changes.
// ---------------------------------------------------------------------------

function renderStreamHead(record) {
  const head = $('stream-head');
  const settle = headSettler(head);
  // The head is rebuilt into #stream-main only: the find bar beside it is never rebuilt,
  // or a refresh landing while you type would take the field's focus and text with it
  const main = $('stream-main');
  head.classList.toggle('idle', !record);
  if (!record) {
    main.replaceChildren();
    closeFind();
    settle();
    return;
  }

  const box = el('div');
  // Row one: the title, between the way back to the list and the way to the details. The
  // two buttons exist at every width and the stylesheet shows them on a phone, where the
  // three panes are screens behind one another.
  const nav = el('div', 'head-nav');
  const back = el('button', 'ghost nav-back-btn');
  back.type = 'button';
  back.setAttribute('aria-label', 'Back to the session list');
  back.appendChild(chevron('left'));
  back.appendChild(el('span', '', 'Sessions'));
  back.addEventListener('click', backFromStream);
  nav.appendChild(back);
  const title = el('h2', '', record.shortKey || record.sessionId);
  title.title = record.key || '';
  nav.appendChild(title);
  const details = el('button', 'ghost nav-details-btn');
  details.type = 'button';
  details.setAttribute('aria-label', 'Details of this session');
  details.title = 'Details';
  details.appendChild(infoGlyph());
  details.appendChild(el('span', '', 'Details'));
  details.addEventListener('click', openDetails);
  nav.appendChild(details);
  box.appendChild(nav);

  // Row two: where and when, on one line
  const meta = el('div', 'meta');
  const tagrow = el('div', 'tagrow');
  tagrow.appendChild(sourceTag(record.source));
  if (record.status) tagrow.appendChild(statusTag(record.status));
  // Grok keeps an archived session in a directory of its own; it is still history
  if (record.archived) tagrow.appendChild(el('span', 'tag', 'archived'));
  if (record.updatedAt) {
    const time = el('span', 'time', relTime(record.updatedAt));
    time.title = record.updatedAt;
    tagrow.appendChild(time);
  }
  meta.appendChild(tagrow);
  if (record.cwd) {
    const where = el('span', 'sub', record.cwd);
    where.title = record.cwd;
    meta.appendChild(where);
  }
  if (record.branch) {
    const branch = el('span', 'branch');
    branch.appendChild(branchGlyph());
    branch.appendChild(el('span', 'name', record.branch));
    branch.title = 'the git branch this session opened on';
    meta.appendChild(branch);
  }
  box.appendChild(meta);

  const bar = el('div', 'toolbar');
  bar.appendChild(segmented(
    [['asc', 'earliest', '↑'], ['desc', 'latest', '↓']],
    state.order,
    (value) => {
      if (value === state.order) return;
      state.order = value;
      state.focusAt = ''; // paging to an end is a deliberate move away from the anchor
      saveViewPrefs();
      syncDetail({ force: true });
    },
    'order',
  ));
  // Views of the same rounds: everything, the words alone, or one kind of step alone — the
  // file changes, the commands run, the steps that failed
  bar.appendChild(segmented(VIEWS, state.view, (value) => setView(value), 'view'));
  // The same choice as a select, which is what a phone shows: five buttons do not fit
  // beside the other controls there, and a row of their own is a row less to read in
  const pick = el('select', 'view-select');
  pick.setAttribute('aria-label', 'What to show of each round');
  for (const [value, label] of VIEWS) {
    const option = el('option', '', label);
    option.value = value;
    pick.appendChild(option);
  }
  pick.value = state.view;
  pick.addEventListener('change', () => setView(pick.value));
  bar.appendChild(pick);
  // Every round's work at once, when reading the whole thing. A per-round choice made
  // after this is kept until the switch is thrown again.
  const expand = button('ghost tiny expand-toggle', '', () => {
    state.expandAll = !state.expandAll;
    state.roundOverrides.clear();
    saveViewPrefs();
    renderMessages();
    renderStreamHead(record);
  });
  expand.appendChild(el('span', 'icon', state.expandAll ? '⊟' : '⊞'));
  expand.appendChild(el('span', 'label', state.expandAll ? 'Collapse steps' : 'Expand steps'));
  expand.title = state.expandAll
    ? 'Fold the work of every round back to one line'
    : 'Open the work of every round';
  expand.setAttribute('aria-label', expand.title);
  expand.classList.toggle('hidden', state.view !== 'all');
  bar.appendChild(expand);
  // The magnifier alone: the row is full, and the glyph says what it does
  const find = button('ghost icon-btn find-toggle', '', () => openFind());
  find.appendChild(findGlyph());
  find.title = 'Find in this session (f)';
  find.setAttribute('aria-label', 'Find in this session');
  find.classList.toggle('on', !$('find-bar').classList.contains('hidden'));
  bar.appendChild(find);
  box.appendChild(bar);

  const detail = state.detail;
  if (detail) {
    const shown = (detail.messages.messages || []).length;
    const total = detail.final && typeof detail.final.messageCount === 'number'
      ? detail.final.messageCount : shown;
    // An anchored window is neither the start nor the end of the session, and saying
    // "latest 200" there would be a lie — so it says what it is, with a way out
    let label;
    if (state.loadingMore) {
      label = 'Loading more…';
    } else if (shown > MESSAGE_LIMIT) {
      // The window has grown past one page, so it is no longer "the latest N" — say how
      // far into the session this is instead
      label = shown + ' of ' + total + ' messages';
    } else if (state.focusAt) {
      label = total > shown
        ? total + ' messages · ' + shown + ' from the match'
        : shown + ' messages';
    } else {
      label = total > shown
        ? total + ' messages · showing the ' + (state.order === 'desc' ? 'latest ' : 'earliest ') + shown
        : shown + ' messages';
    }
    // What the window amounts to — rounds, tool calls, failures, how long it ran — and
    // how much of the session it is, on one line
    box.appendChild(windowStats(detail, label));
    const edge = state.order === 'desc' ? 'older' : 'newer';
    if (total > shown && !state.noMore[edge]) {
      const more = button(
        'ghost tiny load-more',
        state.loadingMore
          ? 'Loading messages…'
          : state.order === 'desc' ? 'Load earlier messages' : 'Load newer messages',
        () => loadMore(edge),
      );
      more.disabled = state.loadingMore;
      box.appendChild(more);
    }
  }

  main.replaceChildren(box);
  settle();
}

// headSettler notes a head's height; the function it returns scrolls the page by however
// much the head has grown or shrunk since. On a phone the head is part of the page, and
// when it grows — the line of sums arrives with the final result — everything below it
// would otherwise move under the reader's eyes. The head itself sticks to the top, so it
// stays whole either way.
function headSettler(head) {
  if (!isPhone() || state.pane !== 'stream') return () => {};
  const before = head.getBoundingClientRect().height;
  return () => {
    const delta = head.getBoundingClientRect().height - before;
    if (delta) scrollerOf(head).scrollTop += delta;
  };
}

// windowStats sums the loaded window. The numbers are the window's, and the row says so
// when the session is longer: a total would need the whole file, and the header already
// has the honest count.
function windowStats(detail, countLabel) {
  const all = detail.messages.messages || [];
  const row = el('div', 'stats');
  if (all.length) {
    const { rounds } = roundsOf(all);
    const asked = rounds.filter((r) => r.ask).length;
    const tools = rounds.reduce((n, r) => n + r.summary.steps, 0);
    const failures = rounds.reduce((n, r) => n + r.summary.failures, 0);
    row.appendChild(el('span', '', asked + (asked === 1 ? ' round' : ' rounds')));
    row.appendChild(el('span', '', tools + (tools === 1 ? ' tool call' : ' tool calls')));
    // The count is also the way to the failures: step through them one by one, the
    // folded rounds opening as needed, and on past the window when the session goes on
    if (failures) {
      const nav = el('span', 'fail-nav');
      const prev = button('ghost icon-btn', '', () => gotoFailure(-1));
      prev.appendChild(chevron('left'));
      prev.title = 'Previous failure ([)';
      prev.setAttribute('aria-label', 'Previous failure');
      const count = button('ghost tiny fail', failures + ' failed', () => gotoFailure(1));
      count.title = 'Next failure (])';
      const next = button('ghost icon-btn', '', () => gotoFailure(1));
      next.appendChild(chevron('right'));
      next.title = 'Next failure (])';
      next.setAttribute('aria-label', 'Next failure');
      nav.append(prev, count, next);
      row.appendChild(nav);
    }
    // The work in this window, round by round, rather than first row to last: a session
    // left open over a weekend spans days, and reading that as how long the work took is
    // the same mistake a round made before its end was separated from its last row.
    const worked = rounds.reduce((n, r) => n + (r.summary.durationMs || 0), 0);
    if (worked > 0) row.appendChild(el('span', '', formatDuration(worked)));
  }
  if (countLabel) row.appendChild(el('span', 'dim count', countLabel));
  return row;
}

// parseTime reads a timestamp the way relTime does: each source spells one differently
function parseTime(iso) {
  if (!iso) return NaN;
  // A bare epoch — pi stamps its messages in milliseconds — read by the server's rule: past
  // 1e11 it is milliseconds, below it seconds
  if (typeof iso === 'number' || /^\d{9,}(\.\d+)?$/.test(String(iso).trim())) {
    const n = Number(iso);
    return n > 1e11 ? n : n * 1000;
  }
  let normalized = String(iso).trim().replace(' ', 'T');
  if (!/[Zz]|[+-]\d{2}:?\d{2}$/.test(normalized)) normalized += 'Z';
  return Date.parse(normalized);
}

// timeSpan is the milliseconds between two timestamps, or 0 when either is unreadable or
// they run backwards (clock skew)
function timeSpan(from, to) {
  const start = parseTime(from), end = parseTime(to);
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return 0;
  return end - start;
}

// formatDuration: 300ms / 2.4s / 45s / 2m14s / 1h3m — the same grammar the export uses
function formatDuration(ms) {
  if (ms < 1000) return Math.round(ms) + 'ms';
  if (ms < 10000) return (ms / 1000).toFixed(1).replace(/\.0$/, '') + 's';
  const total = Math.round(ms / 1000);
  if (total < 60) return total + 's';
  if (total < 3600) {
    const m = Math.floor(total / 60), s = total % 60;
    return s ? m + 'm' + s + 's' : m + 'm';
  }
  const h = Math.floor(total / 3600), m = Math.floor((total % 3600) / 60);
  return m ? h + 'h' + m + 'm' : h + 'h';
}

// Tool categories, and what each one is for: reading, changing, running, searching,
// reaching the network, delegating. Colouring by category rather than by tool name keeps
// the palette small and meaningful — a transcript is scanned for "where did it run
// something" far more often than for "where did it run grep specifically".
// Order matters: the first match wins.
const TOOL_KINDS = [
  ['exec', /^(bash|shell|terminal|exec|sh$|run|command|process|container|docker|local_shell)/i],
  ['write', /(write|edit|patch|replace|create|delete|remove|move|rename|apply|notebook_edit)/i],
  ['read', /^(read|view|cat|open|head|tail|notebook_read)/i],
  ['search', /(grep|glob|search|find|list|scan|ls$|^ls)/i],
  ['net', /(fetch|http|curl|web|url|download|browser)/i],
  ['agent', /(task|agent|dispatch|delegate|subagent)/i],
];

function toolKind(name) {
  const n = String(name == null ? '' : name);
  if (!n) return 'other';
  for (const [kind, pattern] of TOOL_KINDS) {
    if (pattern.test(n)) return kind;
  }
  return 'other';
}

// What an event between the turns is called on the page
const EVENT_LABELS = {
  compaction: 'context compacted',
  interrupted: 'interrupted',
  hook_error: 'hook failed',
  model_change: 'model changed',
};

function eventLabel(block) {
  const label = EVENT_LABELS[block.kind] || String(block.kind || 'event');
  const text = String(block.content || '').trim();
  return text && text !== label ? label + ': ' + text : label;
}

// isFailed: a result that reported an error or an interruption. A result without a
// status is one whose writer did not record the outcome, which is not a failure.
function isFailed(result) {
  return !!result && (result.status === 'error' || result.status === 'interrupted');
}

// outcomeText is the suffix a result carries: how it ended, in the words a reader scans
// for — " — failed · exit 1 · 2.3s"
function outcomeText(result) {
  const parts = [];
  if (result.status === 'error') parts.push('failed');
  if (result.status === 'interrupted') parts.push('interrupted');
  if (typeof result.exitCode === 'number' && (result.exitCode !== 0 || parts.length)) parts.push('exit ' + result.exitCode);
  if (Number(result.durationMs) > 0) parts.push(formatDuration(Number(result.durationMs)));
  return parts.length ? ' — ' + parts.join(' · ') : '';
}

// ---------------------------------------------------------------------------
// Markdown. Agents write it, and a reply read as raw marks — ## for a heading, ** around
// every emphasis, a fence around every command — is a reply read through a screen door.
// This renders the subset that shows up in transcripts: headings, lists, fenced code,
// block quotes, tables, emphasis, inline code, links. It builds nodes and sets text
// through textContent only; no HTML is ever assembled from the text, which is what keeps
// the page's one hard rule intact, and a link gets an href only when its scheme is http or
// https. A single newline inside a paragraph is a line break: that is how a chat reads,
// and how the plain rendering behaved before.
// ---------------------------------------------------------------------------

const FENCE_RE = /^\s{0,3}(`{3,}|~{3,})\s*([\w.+#-]*)\s*$/;
const FENCE_CLOSE_RE = /^\s{0,3}(`{3,}|~{3,})\s*$/;
const HEADING_RE = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
const RULE_RE = /^\s{0,3}([-*_])(\s*\1){2,}\s*$/;
const QUOTE_RE = /^\s{0,3}>\s?/;
const ITEM_RE = /^(\s*)([-*+]|\d{1,3}[.)])\s+(.*)$/;
const TABLE_SEP_RE = /^\s{0,3}\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;
const URL_RE = /^https?:\/\/[^\s<>"'`)\]]+/;

// markdownNode renders text into root (a fresh div.md when none is given)
function markdownNode(text, root) {
  const box = root || el('div', 'md');
  const lines = String(text == null ? '' : text).replace(/\r\n?/g, '\n').split('\n');
  let i = 0;
  let para = [];
  const flush = () => {
    if (!para.length) return;
    const p = el('p');
    appendInline(p, para.join('\n'));
    box.appendChild(p);
    para = [];
  };
  while (i < lines.length) {
    const line = lines[i];
    const fence = FENCE_RE.exec(line);
    if (fence) {
      flush();
      const body = [];
      i++;
      while (i < lines.length && !FENCE_CLOSE_RE.test(lines[i])) {
        body.push(lines[i]);
        i++;
      }
      i++; // the closing fence, when there was one
      const pre = el('pre', 'code');
      const code = el('code', '', body.join('\n'));
      if (fence[2]) code.dataset.lang = fence[2];
      pre.appendChild(code);
      box.appendChild(pre);
      continue;
    }
    if (!line.trim()) {
      flush();
      i++;
      continue;
    }
    const heading = HEADING_RE.exec(line);
    if (heading) {
      flush();
      // A message is one voice in a conversation, not a document: its # is an h3
      const h = el('h' + Math.min(6, heading[1].length + 2));
      appendInline(h, heading[2]);
      box.appendChild(h);
      i++;
      continue;
    }
    if (RULE_RE.test(line)) {
      flush();
      box.appendChild(el('hr'));
      i++;
      continue;
    }
    if (QUOTE_RE.test(line)) {
      flush();
      const quoted = [];
      while (i < lines.length && QUOTE_RE.test(lines[i])) {
        quoted.push(lines[i].replace(QUOTE_RE, ''));
        i++;
      }
      box.appendChild(markdownNode(quoted.join('\n'), el('blockquote')));
      continue;
    }
    if (line.trim().startsWith('|') && i + 1 < lines.length && TABLE_SEP_RE.test(lines[i + 1])) {
      flush();
      i = parseTable(lines, i, box);
      continue;
    }
    if (ITEM_RE.test(line)) {
      flush();
      i = parseList(lines, i, box);
      continue;
    }
    para.push(line);
    i++;
  }
  flush();
  return box;
}

function leadingSpaces(line) {
  return line.length - line.replace(/^\s+/, '').length;
}

// parseList reads one list at one indent. A line indented deeper than the item belongs
// to it — a nested list, or a paragraph continuing the item — and is rendered inside it.
function parseList(lines, i, box) {
  const first = ITEM_RE.exec(lines[i]);
  const indent = first[1].length;
  const ordered = /^\d/.test(first[2]);
  const list = el(ordered ? 'ol' : 'ul');
  if (ordered) {
    const start = parseInt(first[2], 10);
    if (start > 1) list.start = start;
  }
  while (i < lines.length) {
    const m = ITEM_RE.exec(lines[i]);
    if (!m || m[1].length !== indent || /^\d/.test(m[2]) !== ordered) break;
    i++;
    const children = [];
    while (i < lines.length) {
      const next = lines[i];
      if (!next.trim()) {
        // A blank line ends the list unless what follows is still inside this item
        const after = lines[i + 1];
        if (after !== undefined && after.trim() && leadingSpaces(after) > indent) {
          children.push('');
          i++;
          continue;
        }
        break;
      }
      if (leadingSpaces(next) > indent) {
        children.push(next.slice(Math.min(leadingSpaces(next), indent + 2)));
        i++;
        continue;
      }
      break;
    }
    const li = el('li');
    appendInline(li, m[3]);
    if (children.length) {
      const sub = markdownNode(children.join('\n'), el('div'));
      while (sub.firstChild) li.appendChild(sub.firstChild);
    }
    list.appendChild(li);
  }
  box.appendChild(list);
  return i;
}

// parseTable reads a pipe table: a header row, the separator, then rows until a line that
// is not one
function parseTable(lines, i, box) {
  const splitRow = (line) => {
    let t = line.trim();
    if (t.startsWith('|')) t = t.slice(1);
    if (t.endsWith('|')) t = t.slice(0, -1);
    return t.split('|').map((cell) => cell.trim());
  };
  const header = splitRow(lines[i]);
  i += 2;
  const table = el('table');
  const thead = el('thead');
  const headRow = el('tr');
  for (const cell of header) {
    const th = el('th');
    appendInline(th, cell);
    headRow.appendChild(th);
  }
  thead.appendChild(headRow);
  table.appendChild(thead);
  const tbody = el('tbody');
  while (i < lines.length && lines[i].trim().startsWith('|')) {
    const cells = splitRow(lines[i]);
    const row = el('tr');
    for (let c = 0; c < header.length; c++) {
      const td = el('td');
      appendInline(td, cells[c] || '');
      row.appendChild(td);
    }
    tbody.appendChild(row);
    i++;
  }
  table.appendChild(tbody);
  box.appendChild(table);
  return i;
}

// appendInline renders a run of text with its inline marks into parent. inLink is set
// while rendering a link's own label, where another link cannot start — and where a
// bare URL used as its own label would otherwise link itself without end.
function appendInline(parent, text, inLink) {
  let i = 0;
  let buf = '';
  const flush = () => {
    if (buf) {
      parent.appendChild(document.createTextNode(buf));
      buf = '';
    }
  };
  while (i < text.length) {
    const c = text[i];
    if (c === '\n') {
      flush();
      parent.appendChild(el('br'));
      i++;
      continue;
    }
    if (c === '\\' && i + 1 < text.length && /[\\`*_{}\[\]()#+\-.!~|>]/.test(text[i + 1])) {
      buf += text[i + 1];
      i += 2;
      continue;
    }
    if (c === '`') {
      const run = /^`+/.exec(text.slice(i))[0];
      const end = text.indexOf(run, i + run.length);
      if (end > 0) {
        flush();
        parent.appendChild(el('code', '', text.slice(i + run.length, end).trim()));
        i = end + run.length;
        continue;
      }
    }
    if (text.startsWith('**', i)) {
      const end = text.indexOf('**', i + 2);
      if (end > i + 2) {
        flush();
        const strong = el('strong');
        appendInline(strong, text.slice(i + 2, end), inLink);
        parent.appendChild(strong);
        i = end + 2;
        continue;
      }
    }
    if (text.startsWith('~~', i)) {
      const end = text.indexOf('~~', i + 2);
      if (end > i + 2) {
        flush();
        const del = el('del');
        appendInline(del, text.slice(i + 2, end), inLink);
        parent.appendChild(del);
        i = end + 2;
        continue;
      }
    }
    // Emphasis with * only: _ is too common inside identifiers to read as a mark
    if (c === '*' && text[i + 1] && text[i + 1] !== ' ' && text[i + 1] !== '*') {
      const end = text.indexOf('*', i + 1);
      if (end > i + 1 && text[end - 1] !== ' ') {
        flush();
        const em = el('em');
        appendInline(em, text.slice(i + 1, end), inLink);
        parent.appendChild(em);
        i = end + 1;
        continue;
      }
    }
    if (c === '!' && text[i + 1] === '[') {
      const m = /^!\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)/.exec(text.slice(i));
      if (m) {
        flush();
        // Never fetched: the page makes no request anywhere else, and an image in a
        // transcript is a file on another machine's disk anyway
        parent.appendChild(el('span', 'md-image', '[image' + (m[1] ? ': ' + m[1] : '') + ']'));
        i += m[0].length;
        continue;
      }
    }
    if (c === '[' && !inLink) {
      const m = /^\[([^\]\n]+)\]\(([^)\s]+)(?:\s+"[^"]*")?\)/.exec(text.slice(i));
      if (m) {
        flush();
        parent.appendChild(linkNode(m[1], m[2]));
        i += m[0].length;
        continue;
      }
    }
    if (c === 'h' && !inLink && (i === 0 || !/[\w/]/.test(text[i - 1]))) {
      const m = URL_RE.exec(text.slice(i));
      if (m) {
        const url = m[0].replace(/[.,;:!?]+$/, '');
        flush();
        parent.appendChild(linkNode(url, url));
        i += url.length;
        continue;
      }
    }
    buf += c;
    i++;
  }
  flush();
}

// linkNode: an href only for http and https. Anything else keeps its label and shows the
// target on hover, which is the honest rendering of a link the page will not follow.
function linkNode(label, href) {
  const a = el('a', 'md-link');
  appendInline(a, label, true);
  if (/^https?:\/\//i.test(href)) {
    a.href = href;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
  }
  a.title = href;
  return a;
}

// plainText is Markdown with its marks removed, for a one-line preview
function plainText(text) {
  return String(text == null ? '' : text)
    .replace(/```[\s\S]*?```/g, ' ')
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/[#>*`~|]+/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
}

// ---- Times ----

function pad2(n) {
  return String(n).padStart(2, '0');
}

// dayKey is the local calendar day of a timestamp, for deciding whether a time needs its
// date in front of it
function dayKey(iso) {
  const t = parseTime(iso);
  if (Number.isNaN(t)) return '';
  const d = new Date(t);
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
}

// shortTime renders a message time the way a reader scans it: the clock alone on the
// session's own day, the date in front on any other day, the year only when it differs.
// The full timestamp stays in the tooltip.
function shortTime(iso, refDay) {
  const t = parseTime(iso);
  if (Number.isNaN(t)) return String(iso || '');
  const d = new Date(t);
  const clock = pad2(d.getHours()) + ':' + pad2(d.getMinutes());
  if (dayKey(iso) === refDay) return clock;
  const year = d.getFullYear() === new Date().getFullYear() ? '' : d.getFullYear() + '-';
  return year + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' + clock;
}

// streamDay is the day the loaded window ends on; times on that day show the clock alone
let streamDay = '';

function blockNode(block) {
  switch (block.type || 'unknown') {
    case 'text':
      return markdownNode(block.content, el('div', 'block text md'));
    case 'thinking':
      return el('div', 'block thinking', block.content);
    case 'event':
      return el('div', 'block event', eventLabel(block));
    case 'toolCall': {
      // Tool name as the heading, arguments indented below, so one glance says what ran
      const kind = toolKind(block.name);
      const wrap = el('div', 'block toolCall tool-' + kind);
      wrap.appendChild(el('div', 'tool-name', block.name || '(unnamed tool)'));
      wrap.appendChild(el('pre', '', JSON.stringify(block.arguments || {}, null, 2)));
      return wrap;
    }
    case 'toolResult': {
      const kind = toolKind(block.toolName);
      const wrap = el('div', 'block toolResult tool-' + kind + (isFailed(block) ? ' failed' : ''));
      wrap.appendChild(el('div', 'tool-label', '↳ ' + (block.toolName || 'result') + outcomeText(block)));
      wrap.appendChild(el('pre', '', block.content));
      return wrap;
    }
    default:
      return el('div', 'block', JSON.stringify(block));
  }
}

// hasWords reports whether a message carries any text of its own
function hasWords(message) {
  return (message.content || []).some(
    (b) => b.type === 'text' && String(b.content || '').trim() !== '');
}

// messageWords is a message's own text, the blocks joined
function messageWords(message) {
  return (message.content || [])
    .filter((b) => b.type === 'text')
    .map((b) => String(b.content || ''))
    .join('\n');
}

// roleLabel names the speaker as the reader sees it, not as the file stores it
function roleLabel(message) {
  if (message.role === 'user' && !hasWords(message)) return 'tool';
  if (message.role === 'toolResult') return 'tool';
  return message.role || '?';
}

// ---- Rounds ----

// The command-plumbing openings a user row can have without being a question; the same
// list the server's isRoundStart keeps
const PLUMBING_PREFIXES = [
  '<command-name>', '<command-message>', '<command-args>', '<command-contents>',
  '<local-command-', '<caveat', 'Caveat:', '<task-notification', '<system-reminder',
];

// The blocks a harness closes a user's words with before a model reads them (T3 Code's,
// on every prompt it gives Grok); the same list the server's StripHarnessBlocks keeps
const HARNESS_BLOCKS = ['runtime_info', 'pull_request_linking'];

// stripHarness drops the harness blocks that close a text, however many. Only those: the
// same tag written mid-question is the user's, and stays.
function stripHarness(text) {
  let t = String(text || '').trimEnd();
  for (;;) {
    let stripped = false;
    for (const tag of HARNESS_BLOCKS) {
      if (!t.endsWith('</' + tag + '>')) continue;
      const open = t.lastIndexOf('<' + tag + '>');
      if (open < 0) continue;
      t = t.slice(0, open).trimEnd();
      stripped = true;
    }
    if (!stripped) return t;
  }
}

// isAsk: a user message carrying words of its own, not command plumbing, not one the CLI
// assembled (those arrive flagged injected), and not one that is wholly an XML-style
// element — a person does not type a question that way
function isAsk(message) {
  if (message.role !== 'user' || message.injected) return false;
  const text = stripHarness(messageWords(message)).trim();
  if (!text) return false;
  if (PLUMBING_PREFIXES.some((prefix) => text.startsWith(prefix))) return false;
  return !wrappedInTag(text);
}

// wrappedInTag: <name …>…</name>, the same name at both ends and nothing outside them
function wrappedInTag(text) {
  if (text.length < 5 || text[0] !== '<' || text[text.length - 1] !== '>') return false;
  const m = /^<([A-Za-z][\w-]*)[\s>]/.exec(text);
  return !!m && text.endsWith('</' + m[1] + '>');
}

// roundsOf groups a window into rounds once per window: the stream, the header's stats
// and the table of contents all read the same grouping
let roundsCache = { source: null, value: null };
function roundsOf(all) {
  if (roundsCache.source !== all) roundsCache = { source: all, value: buildRounds(all) };
  return roundsCache.value;
}

function buildRounds(all) {
  const preface = [];
  const rounds = [];
  let current = null;
  all.forEach((message, index) => {
    if (isAsk(message)) {
      current = { no: rounds.length + 1, key: messageKey(message), ask: { message, index }, items: [], last: false };
      rounds.push(current);
      return;
    }
    if (!current) preface.push({ message, index });
    else current.items.push({ message, index });
  });
  if (rounds.length) rounds[rounds.length - 1].last = true;
  // What comes before the first ask is the tail of a round whose ask lies outside the
  // window (a window opened at the latest page, or at a search hit), or the rows a CLI
  // injected at the start. Either way it is work and words, not a conversation opener,
  // so it is read as a round without an ask: folded like the others, its reply shown.
  let lead = null;
  if (preface.some(({ message }) => message.role !== 'user' || hasWords(message) || (message.content || []).length)) {
    lead = { no: 0, key: 'lead:' + messageKey(preface[0].message), ask: null, items: preface, last: rounds.length === 0 };
  }
  const analysed = lead ? [lead].concat(rounds) : rounds;
  analysed.forEach(analyseRound);
  return { preface: lead ? [] : preface, rounds: analysed };
}

// analyseRound turns the messages after an ask into steps — a tool call paired with its
// result, a thought, an interim note, an event — and picks the final reply: the last
// assistant message in the round that has words. Everything the agent said before that
// is a note along the way.
function analyseRound(round) {
  const steps = [];
  const open = new Map(); // call id → the step waiting for its result
  let finalAt = -1;
  round.items.forEach((item, i) => {
    if (item.message.role === 'assistant' && hasWords(item.message)) finalAt = i;
  });
  round.final = null;
  round.items.forEach((item, i) => {
    const { message, index } = item;
    (message.content || []).forEach((block, position) => {
      const key = messageKey(message) + ':' + position;
      switch (block.type) {
        case 'toolCall': {
          const step = { kind: 'tool', key, call: block, result: null, index, message, position };
          steps.push(step);
          if (block.id) open.set(block.id, step);
          break;
        }
        case 'toolResult': {
          // Paired by id when the source gave one. Without ids the result goes to the
          // oldest call still waiting that it names, or the oldest waiting at all; a
          // result with an id nobody claims is an orphan and stands alone.
          let step = null;
          if (block.callId) {
            step = open.get(block.callId) || null;
          } else {
            step = steps.find((s) => s.kind === 'tool' && s.call && !s.result &&
              (!block.toolName || s.call.name === block.toolName)) ||
              steps.find((s) => s.kind === 'tool' && s.call && !s.result) || null;
          }
          if (step && !step.result) {
            step.result = block;
            step.resultMessage = message;
            step.resultPosition = position;
            if (step.call.id) open.delete(step.call.id);
          } else {
            steps.push({ kind: 'tool', key, call: null, result: block, index, message, position,
              resultMessage: message, resultPosition: position });
          }
          break;
        }
        case 'thinking':
          // A Claude thinking block may carry only a signature and no words: nothing to show
          if (!String(block.content || '').trim()) break;
          steps.push({ kind: 'thinking', key, block, index, message, position });
          break;
        case 'event':
          steps.push({ kind: 'event', key, block, index, position });
          break;
        case 'text':
          if (message.role === 'assistant' && i === finalAt) break; // the reply, rendered on its own
          if (!String(block.content || '').trim()) break;
          if (message.injected) steps.push({ kind: 'injected', key, block, index, position });
          else steps.push({ kind: 'note', key, block, index, position, role: message.role });
          break;
        default:
          break;
      }
    });
    if (i === finalAt) round.final = item;
  });
  round.steps = mergeSteps(steps);
  round.summary = summarizeRound(round);
}

// mergeSteps folds a run of successful reads and searches into one step: "explored —
// read 4 files, searched twice" is what a reader wants to know about that stretch.
// Only runs of two or more, and never a failure: those stay on their own line.
function mergeSteps(steps) {
  const out = [];
  let run = [];
  const flush = () => {
    if (run.length >= 2) out.push({ kind: 'merged', key: run[0].key + '+merged', children: run });
    else out.push(...run);
    run = [];
  };
  for (const step of steps) {
    const kind = step.kind === 'tool' && step.call ? toolKind(step.call.name) : '';
    const mergeable = (kind === 'read' || kind === 'search') && !isFailed(step.result);
    if (mergeable) {
      run.push(step);
      continue;
    }
    flush();
    out.push(step);
  }
  flush();
  return out;
}

// flatToolSteps lists the tool steps of a round, merged ones opened
function flatToolSteps(steps) {
  const out = [];
  for (const step of steps) {
    if (step.kind === 'merged') out.push(...step.children);
    else if (step.kind === 'tool') out.push(step);
  }
  return out;
}

function stepName(step) {
  if (step.call) return step.call.name || '(unnamed tool)';
  return (step.result && step.result.toolName) || 'tool output';
}

function isFailedStep(step) {
  return step.kind === 'tool' && isFailed(step.result);
}

function isChangeStep(step) {
  return step.kind === 'tool' && !!step.call && toolKind(step.call.name) === 'write';
}

function isCommandStep(step) {
  return step.kind === 'tool' && !!step.call && toolKind(step.call.name) === 'exec';
}

// STEP_VIEWS are the views that keep one kind of step, round by round: each round that has
// one shows its ask and those steps, and the rest of the round is left out. open says
// whether the steps come opened — a change is read as its diff and a failure as its output,
// while a command reads on one line, a failed one keeping the start of its output in view.
const STEP_VIEWS = {
  changes: { test: isChangeStep, open: true, empty: 'No file changes in this window' },
  commands: { test: isCommandStep, open: false, empty: 'No commands in this window' },
  failures: { test: isFailedStep, open: true, empty: 'No failed steps in this window' },
};

// VIEWS are the stream's views, in the order the switch offers them
const VIEWS = [['all', 'all'], ['conversation', 'conversation'], ['changes', 'changes'],
  ['commands', 'commands'], ['failures', 'failures']];

// stepView is the step view a name stands for, or null — own keys only, since a view name
// can come back from storage as anything
function stepView(view) {
  return Object.prototype.hasOwnProperty.call(STEP_VIEWS, view) ? STEP_VIEWS[view] : null;
}

// setView switches the stream to another view and redraws what depends on it: the stream,
// and the head, whose buttons say which view is on
function setView(view) {
  if (view === state.view) return;
  state.view = view;
  saveViewPrefs();
  renderMessages();
  renderStreamHead(state.byId.get(state.selectedId));
}

// The argument a tool names a file by, across the CLIs
const FILE_KEYS = ['file_path', 'filePath', 'path', 'notebook_path', 'file', 'target_file'];
function fileOf(args) {
  if (!args || typeof args !== 'object') return '';
  for (const key of FILE_KEYS) {
    if (typeof args[key] === 'string' && args[key]) return args[key];
  }
  return '';
}

// summarizeRound is the folded line: steps, commands, files changed, failures, time
// isWorkRow: the agent's own rows. The ask is one by definition; a row that only carries
// text the CLI put in the user's mouth (a task notification, a reminder, a block of
// context) is not, however late it arrives.
function isWorkRow(message) {
  if (!message) return false;
  if (message.role === 'assistant') return true;
  return (message.content || []).some(
    (b) => b.type === 'toolCall' || b.type === 'toolResult' || b.type === 'event');
}

function summarizeRound(round) {
  const tools = flatToolSteps(round.steps);
  const changed = new Set();
  let commands = 0, failures = 0;
  for (const step of tools) {
    const kind = toolKind(stepName(step));
    if (kind === 'exec') commands++;
    const failed = isFailed(step.result);
    if (failed) failures++;
    // A change is a write that did not fail; a Read of the same file never was one
    if (kind === 'write' && !failed && step.call) {
      const file = fileOf(step.call.arguments);
      if (file) changed.add(file);
    }
  }
  const items = round.items;
  const firstAt = round.ask ? round.ask.message.timestamp : (items.length ? items[0].message.timestamp : '');
  // The end of the work, not the end of the rows: a notification or a block of context
  // the CLI drops in hours later belongs to whichever round was last, and a round of two
  // commands would report ninety-four hours. The server splits the two the same way.
  let lastAt = firstAt;
  for (const item of items) {
    if (isWorkRow(item.message)) lastAt = item.message.timestamp || lastAt;
  }
  let durationMs = timeSpan(firstAt, lastAt);
  if (!(durationMs > 0)) {
    durationMs = tools.reduce((sum, s) => sum + (Number(s.result && s.result.durationMs) || 0), 0);
  }
  const interrupted = round.steps.some((s) => s.kind === 'event' && s.block.kind === 'interrupted') ||
    tools.some((s) => s.result && s.result.status === 'interrupted');
  return {
    steps: tools.length,
    commands,
    filesChanged: changed.size,
    failures,
    durationMs,
    interrupted,
    thinking: round.steps.filter((s) => s.kind === 'thinking').length,
  };
}

function isRoundExpanded(round) {
  if (state.roundOverrides.has(round.key)) return state.roundOverrides.get(round.key);
  return state.expandAll;
}

// emptyStreamNote names what the current view left empty
function emptyStreamNote(count) {
  if (!count) return 'This session has no messages';
  if (stepView(state.view)) return stepView(state.view).empty;
  if (state.view === 'conversation') return 'No conversation in this window';
  return 'Nothing to show in this window';
}

// foldable wraps a long block in a disclosure that still says what it holds. The key has
// to be stable across rebuilds, so an expanded block stays expanded through a refresh.
// summaryNode builds a <summary> whose contents sit in a row of their own. The row is a
// div because WebKit has laid the summary element out as a block whatever display it was
// given: with the flex on the summary itself, the preview — eighty characters that never
// wrap — ran on in one line and pushed the whole conversation sideways on an iPhone.
function summaryNode(children) {
  const summary = el('summary');
  const row = el('div', 'fold-row');
  for (const child of children) row.appendChild(child);
  summary.appendChild(row);
  return summary;
}

function foldable(rendered, key, kind) {
  const plain = rendered.textContent || '';
  const details = el('details');
  details.open = state.openBlocks.has(key);
  details.addEventListener('toggle', () => {
    if (details.open) state.openBlocks.add(key);
    else state.openBlocks.delete(key);
  });
  details.appendChild(summaryNode([
    el('span', 'fold-kind', kind),
    el('span', 'fold-preview', plain.replace(/\s+/g, ' ').trim().slice(0, 80)),
    el('span', 'fold-size', plain.length + ' chars'),
  ]));
  details.appendChild(rendered);
  return details;
}

// messageNode renders one message as a bubble: the asks, the final replies, and whatever
// precedes the first ask. options.only keeps blocks of one type (a reply's words, without
// the calls that rode on the same message), options.label chips a round number onto the
// head, options.extraClass marks the bubble's part in the round.
function messageNode(message, index, options) {
  const opts = options || {};
  const source = Array.isArray(message.content) ? message.content : [];
  // A block with no content renders as an empty box (the dashed thinking box especially
  // stands out), so drop it. These blocks genuinely occur: a Claude thinking block may
  // carry only a signature with an empty body.
  const blocks = source
    .map((block, position) => ({ block, position }))
    .filter(({ block }) => !opts.only || block.type === opts.only)
    .map((item) => Object.assign(item, { node: blockNode(item.block) }))
    .filter((item) => (item.node.textContent || '').trim() !== '');

  const node = el('div', 'msg ' + (message.role || '') + (blocks.length === 0 ? ' is-empty' : '') +
    (opts.extraClass ? ' ' + opts.extraClass : ''));
  node.dataset.index = index;
  // A user-role row with no words of its own is a tool result (Claude's shape), not a
  // question: label and colour it as tooling so the two speakers stay distinguishable
  if (message.role === 'user' && !hasWords(message)) node.classList.add('is-tool');
  if (message.injected) node.classList.add('is-injected');
  const head = el('div', 'head');
  if (opts.label) head.appendChild(el('span', 'round-no', opts.label));
  head.appendChild(el('span', 'role', message.injected ? 'injected context' : roleLabel(message)));
  // A message with nothing displayable is worth one line of explanation, not a whole block
  if (blocks.length === 0) head.appendChild(el('span', 'empty-hint', 'nothing to display'));
  if (message.id) head.title = 'id: ' + message.id;
  if (message.timestamp) {
    const time = el('span', 'time', shortTime(message.timestamp, streamDay));
    time.title = String(message.timestamp);
    head.appendChild(time);
  }
  node.appendChild(head);

  blocks.forEach(({ position, node: rendered, block }) => {
    const plain = rendered.textContent || '';
    // The reply is what you came to read, so it folds only when it is very long; an
    // injected row is never the human speaking and folds whatever its size
    const limit = message.injected ? 0 : opts.extraClass === 'final' ? FOLD_FINAL_AT : (FOLD_AT[block.type] || 600);
    if (plain.length <= limit) {
      node.appendChild(rendered);
      return;
    }
    node.appendChild(foldable(rendered, messageKey(message) + ':' + position, message.injected ? 'injected' : block.type));
  });
  return node;
}

// roundNode lays one round out: the ask, the work (folded to a line), the reply
function roundNode(round) {
  const record = state.byId.get(state.selectedId);
  const only = stepView(state.view);
  if (only && !flatToolSteps(round.steps).some(only.test)) return null;
  const node = el('section', 'round' + (round.ask ? '' : ' lead'));
  node.dataset.round = round.no;
  if (round.ask) {
    node.appendChild(messageNode(round.ask.message, round.ask.index, { label: '#' + round.no }));
  } else {
    // The ask lies before this window (or there is none): say so, where the bubble would be
    const total = Number(state.detail && state.detail.final && state.detail.final.messageCount) || 0;
    const all = (state.detail && state.detail.messages.messages) || [];
    node.appendChild(el('p', 'round-lead', total > all.length
      ? 'earlier in this session — the ask is before the loaded window'
      : 'before the first ask'));
  }

  if (only) {
    const list = el('div', 'steps only ' + state.view);
    for (const step of flatToolSteps(round.steps)) {
      if (!only.test(step)) continue;
      list.appendChild(toolStepNode(step, round, { open: only.open, pinned: !only.open && isFailedStep(step) }));
    }
    node.appendChild(list);
    return node;
  }
  if (state.view === 'all') {
    const timeline = timelineNode(round);
    if (timeline) node.appendChild(timeline);
  } else {
    // The conversation alone: what the agent said along the way, then its reply
    for (const step of round.steps) {
      if (step.kind === 'note' && step.role !== 'user') node.appendChild(markStep(noteNode(step), step));
    }
  }
  if (round.final) {
    node.appendChild(messageNode(round.final.message, round.final.index, { only: 'text', extraClass: 'final' }));
  } else if (round.summary.interrupted) {
    node.appendChild(el('p', 'no-reply', 'interrupted — no reply'));
  } else if (round.last && record && record.isActive) {
    node.appendChild(el('p', 'no-reply', 'no reply yet'));
  } else {
    node.appendChild(el('p', 'no-reply', 'no reply in this round'));
  }
  return node;
}

// timelineNode is the folded line over a round's work, and the work under it when open.
// Folded, the failures stay in view — the first five of them, with the start of their
// output — because they are what a reader scans a session for.
function timelineNode(round) {
  if (!round.steps.length) return null;
  const s = round.summary;
  const expanded = isRoundExpanded(round);
  const wrap = el('div', 'timeline' + (expanded ? ' open' : ''));
  const toggle = el('button', 'timeline-toggle');
  toggle.type = 'button';
  toggle.setAttribute('aria-expanded', expanded ? 'true' : 'false');
  toggle.appendChild(el('span', 'caret'));
  const text = el('span', 'timeline-text');
  const count = s.steps || round.steps.length;
  text.appendChild(el('b', '', count + (count === 1 ? ' step' : ' steps')));
  const parts = [];
  if (s.commands) parts.push([s.commands + (s.commands === 1 ? ' command' : ' commands'), '']);
  if (s.filesChanged) parts.push(['changed ' + s.filesChanged + (s.filesChanged === 1 ? ' file' : ' files'), '']);
  if (s.failures) parts.push([s.failures + ' failed', 'fail']);
  if (s.durationMs > 0) parts.push([formatDuration(s.durationMs), '']);
  for (const [label, cls] of parts) {
    text.appendChild(el('span', 'sep', '·'));
    text.appendChild(el('span', cls, label));
  }
  toggle.appendChild(text);
  toggle.title = expanded ? 'Fold the work back to one line' : 'Open the work of this round';
  toggle.addEventListener('click', () => {
    state.roundOverrides.set(round.key, !expanded);
    renderMessages();
  });
  wrap.appendChild(toggle);

  const list = el('div', 'steps');
  if (expanded) {
    for (const step of round.steps) list.appendChild(stepNode(step, round));
  } else {
    const failed = flatToolSteps(round.steps).filter(isFailedStep);
    for (const step of failed.slice(0, 5)) list.appendChild(toolStepNode(step, round, { pinned: true }));
    if (failed.length > 5) list.appendChild(el('p', 'dim more-failures', '+' + (failed.length - 5) + ' more failed steps'));
  }
  if (list.childNodes.length) wrap.appendChild(list);
  return wrap;
}

function stepNode(step, round) {
  switch (step.kind) {
    case 'tool': return toolStepNode(step, round, {});
    case 'merged': return mergedStepNode(step, round);
    case 'thinking': return markStep(thinkingStepNode(step), step);
    case 'note': return markStep(noteNode(step), step);
    case 'event': return markStep(el('div', 'event', eventLabel(step.block)), step);
    case 'injected': return markStep(injectedNode(step), step);
    default: return el('div', 'event', step.kind);
  }
}

// markStep labels a step's node with the step it draws and the message it came from, which
// is how a failure or a match is found on screen again after the stream is rebuilt
function markStep(node, step) {
  node.dataset.step = step.key;
  const index = step.kind === 'merged' ? step.children[0].index : step.index;
  if (index !== undefined) node.dataset.index = index;
  return node;
}

// stepGlyph says how a step ended at a glance
function stepGlyph(step, round) {
  const result = step.result;
  if (!result) {
    const record = state.byId.get(state.selectedId);
    return round && round.last && record && record.isActive ? '…' : '○';
  }
  if (result.status === 'error') return '✗';
  if (result.status === 'interrupted') return '■';
  return '✓';
}

// stepTitle is the object of a step on one line: the command, the path, the query
function stepTitle(step) {
  const call = step.call;
  const args = call && call.arguments && typeof call.arguments === 'object' ? call.arguments : {};
  const name = stepName(step);
  const kind = toolKind(name);
  let target = '';
  if (kind === 'exec') target = commandOf(args);
  else if (kind === 'read' || kind === 'write' || kind === 'search') {
    target = fileOf(args) || firstString(args, ['pattern', 'query', 'glob', 'path', 'dir', 'directory']);
  } else if (kind === 'net') target = firstString(args, ['url', 'query', 'q']);
  else if (kind === 'agent') target = firstString(args, ['description', 'prompt', 'task']);
  else target = firstString(args, Object.keys(args));
  const record = state.byId.get(state.selectedId);
  return shortenPath(oneLine(target), record && record.cwd);
}

function oneLine(text) {
  return String(text == null ? '' : text).replace(/\s+/g, ' ').trim();
}

// shortenPath shows paths under the project as relative ones
function shortenPath(text, cwd) {
  if (!text || !cwd || !/^(\/|[A-Za-z]:[\\/])/.test(cwd)) return text;
  const root = cwd.replace(/[\\/]+$/, '');
  return text.split(root + '/').join('').split(root + '\\').join('');
}

// commandOf is the command a shell-like call ran, however the CLI spelled the argument
function commandOf(args) {
  for (const key of ['command', 'cmd', 'commands']) {
    const v = args[key];
    if (Array.isArray(v)) return v.map(String).join(' ');
    if (typeof v === 'string' && v) return v;
  }
  if (args.action && typeof args.action === 'object') return commandOf(args.action);
  if (typeof args.input === 'string') return args.input; // Codex exec: a script
  return firstString(args, ['description', 'script']);
}

function firstString(args, keys) {
  for (const key of keys) {
    if (typeof args[key] === 'string' && args[key].trim()) return args[key];
  }
  return '';
}

// stepMeta: exit code when it says something, duration, and the size of the output
function stepMeta(step) {
  const meta = el('span', 'step-meta');
  const result = step.result;
  if (!result) return meta;
  const failed = isFailed(result);
  if (typeof result.exitCode === 'number' && (result.exitCode !== 0 || failed)) {
    meta.appendChild(el('span', 'exit', 'exit ' + result.exitCode));
  }
  if (Number(result.durationMs) > 0) meta.appendChild(el('span', '', formatDuration(Number(result.durationMs))));
  const content = String(result.content || '');
  if (content) {
    const lines = content.split('\n').length;
    meta.appendChild(el('span', '', lines + (lines === 1 ? ' line' : ' lines') + (result.truncated ? '+' : '')));
  }
  return meta;
}

function toolStepNode(step, round, options) {
  const opts = options || {};
  const name = stepName(step);
  const kind = toolKind(name);
  const result = step.result;
  const row = el('div', 'step tool-' + kind +
    (isFailed(result) ? ' failed' : '') +
    (result && result.status === 'interrupted' ? ' interrupted' : '') +
    (opts.pinned ? ' pinned' : ''));
  markStep(row, step);
  const open = opts.open || state.openBlocks.has(step.key);
  const head = el('button', 'step-head');
  head.type = 'button';
  head.setAttribute('aria-expanded', open ? 'true' : 'false');
  head.appendChild(el('span', 'glyph', stepGlyph(step, round)));
  head.appendChild(el('span', 'step-name', name));
  head.appendChild(el('span', 'step-target', stepTitle(step)));
  head.appendChild(stepMeta(step));
  head.title = open ? 'Fold this step' : 'Open this step: arguments and output';
  head.addEventListener('click', () => {
    if (open) state.openBlocks.delete(step.key);
    else state.openBlocks.add(step.key);
    renderMessages();
  });
  row.appendChild(head);
  if (open) {
    row.appendChild(stepDetails(step));
  } else if (opts.pinned && result && result.content) {
    // A failure folded away still shows how it failed: the first lines of its output
    const lines = String(result.content).split('\n');
    const preview = el('pre', 'preview', lines.slice(0, 6).join('\n') + (lines.length > 6 ? '\n…' : ''));
    row.appendChild(preview);
  }
  return row;
}

// stepDetails is what a step opens into: the arguments (a diff for a change, the command
// for a shell, JSON for the rest), then the output with how it ended
function stepDetails(step) {
  const box = el('div', 'step-details');
  const call = step.call;
  if (call) {
    const kind = toolKind(call.name);
    const diff = kind === 'write' ? diffOf(call) : null;
    if (diff && diff.length) {
      box.appendChild(diffNode(diff));
    } else if (kind === 'exec' && commandOf(call.arguments || {})) {
      box.appendChild(el('pre', 'command', commandOf(call.arguments || {})));
    } else {
      box.appendChild(el('pre', 'args', JSON.stringify(call.arguments || {}, null, 2)));
    }
  }
  const result = step.result;
  if (result) {
    box.appendChild(el('div', 'result-label', '↳ output' + outcomeText(result)));
    if (result.content) box.appendChild(el('pre', 'output', result.content));
    else box.appendChild(el('div', 'dim small', '(no output)'));
    if (result.truncated) {
      // The server cut the output to a preview; the rest is one request away
      const more = button('ghost tiny', 'Show full output', (event) => loadFullBlock(step, event.currentTarget));
      box.appendChild(more);
    }
  } else if (call) {
    box.appendChild(el('div', 'dim small', 'no result recorded'));
  }
  return box;
}

// loadFullBlock fetches the one message a cut output belongs to with full=1 and swaps the
// whole block into the loaded window, so a rebuild keeps it. The message is found again by
// its key, which is stable across the two reads.
async function loadFullBlock(step, node) {
  const message = step.resultMessage || step.message;
  const position = step.resultPosition !== undefined ? step.resultPosition : step.position;
  if (!state.detail || !message || position === undefined) return;
  if (!message.timestamp) {
    setText(node, 'not available');
    return;
  }
  setText(node, 'Loading…');
  try {
    if (await fetchWhole(message, position)) renderMessages();
    else setText(node, 'not found');
  } catch (err) {
    handleError(err);
    setText(node, 'failed');
  }
}

// fetchWhole reads one cut block whole and puts it into the window in place of its preview,
// so every rebuild after keeps it. False when the block could not be found again.
async function fetchWhole(message, position) {
  const detail = state.detail;
  if (!detail || !message.timestamp) return false;
  const id = encodeURIComponent(detail.sessionId);
  const data = await api('/sessions/' + id + '/messages?limit=8&order=asc&full=1&at=' +
    encodeURIComponent(message.timestamp));
  const want = messageKey(message);
  const found = (data.messages || []).find((m) => messageKey(m) === want);
  const block = found && Array.isArray(found.content) ? found.content[position] : null;
  if (!block) return false;
  message.content[position] = block;
  roundsCache = { source: null, value: null }; // the steps point at the old block
  return true;
}

// ---- Diffs ----
// A change is read as a diff: what went, what came. The CLIs spell an edit a dozen ways —
// old_string/new_string, oldText/newText, a list of edits, a whole new file, a patch in
// Codex's own format, a unified diff — and all of them come out as the same lines here.

function diffOf(call) {
  const a = call.arguments && typeof call.arguments === 'object' ? call.arguments : {};
  const name = String(call.name || '').toLowerCase();
  const path = fileOf(a);
  const lines = [];
  const pushPair = (oldText, newText) => {
    if (oldText != null && oldText !== '') String(oldText).split('\n').forEach((t) => lines.push({ type: 'del', text: t }));
    if (newText != null && newText !== '') String(newText).split('\n').forEach((t) => lines.push({ type: 'add', text: t }));
  };
  if (typeof a.patch === 'string' && a.patch) return parsePatchText(a.patch);
  if (name === 'apply_patch' && typeof a.input === 'string') return parsePatchText(a.input);
  if (typeof a.input === 'string' && a.input.startsWith('*** Begin Patch')) return parsePatchText(a.input);
  if (Array.isArray(a.edits)) {
    if (path) lines.push({ type: 'file', text: path });
    a.edits.forEach((edit, i) => {
      if (!edit || typeof edit !== 'object') return;
      if (i > 0) lines.push({ type: 'gap', text: '⋮' });
      pushPair(pick(edit, ['old_string', 'oldString', 'oldText', 'old']), pick(edit, ['new_string', 'newString', 'newText', 'new']));
    });
    return lines;
  }
  const oldText = pick(a, ['old_string', 'oldString', 'oldText', 'old_str']);
  const newText = pick(a, ['new_string', 'newString', 'newText', 'new_str']);
  if (oldText !== undefined || newText !== undefined) {
    if (path) lines.push({ type: 'file', text: path });
    pushPair(oldText, newText);
    return lines;
  }
  const content = pick(a, ['content', 'contents', 'text', 'new_source', 'file_text', 'body']);
  if (typeof content === 'string') {
    if (path) lines.push({ type: 'file', text: path });
    content.split('\n').forEach((t) => lines.push({ type: 'add', text: t }));
    return lines;
  }
  if (typeof a.diff === 'string' && a.diff) return parseUnifiedDiff(a.diff);
  return null;
}

function pick(obj, keys) {
  for (const key of keys) {
    if (obj[key] !== undefined && obj[key] !== null) return obj[key];
  }
  return undefined;
}

// parsePatchText reads Codex's apply_patch format: *** Update File: path, @@ hunks, and
// lines signed + / - / space
function parsePatchText(text) {
  const lines = [];
  for (const raw of String(text).replace(/\r\n/g, '\n').split('\n')) {
    if (raw.startsWith('*** Begin Patch') || raw.startsWith('*** End Patch')) continue;
    const file = /^\*\*\* (?:Update|Add|Delete) File: (.+)$/.exec(raw);
    if (file) {
      lines.push({ type: 'file', text: file[1].trim() });
      continue;
    }
    if (raw.startsWith('*** Move to: ')) {
      lines.push({ type: 'file', text: '→ ' + raw.slice('*** Move to: '.length).trim() });
      continue;
    }
    if (raw.startsWith('@@')) {
      lines.push({ type: 'gap', text: raw.length > 2 ? raw : '⋮' });
      continue;
    }
    if (raw.startsWith('+')) lines.push({ type: 'add', text: raw.slice(1) });
    else if (raw.startsWith('-')) lines.push({ type: 'del', text: raw.slice(1) });
    else lines.push({ type: 'ctx', text: raw.startsWith(' ') ? raw.slice(1) : raw });
  }
  return lines;
}

// parseUnifiedDiff reads git-style output: --- / +++ headers, @@ hunks, signed lines
function parseUnifiedDiff(text) {
  const lines = [];
  const rows = String(text).replace(/\r\n/g, '\n').split('\n');
  rows.forEach((raw, i) => {
    if (raw.startsWith('diff --git ') || raw.startsWith('index ') || raw.startsWith('\\ No newline')) return;
    if (raw.startsWith('--- ') && rows[i + 1] && rows[i + 1].startsWith('+++ ')) return;
    if (raw.startsWith('+++ ') && i > 0 && rows[i - 1].startsWith('--- ')) {
      const path = raw.slice(4).replace(/^b\//, '').trim();
      if (path && path !== '/dev/null') lines.push({ type: 'file', text: path });
      return;
    }
    if (raw.startsWith('@@')) {
      lines.push({ type: 'gap', text: raw });
      return;
    }
    if (raw.startsWith('+')) lines.push({ type: 'add', text: raw.slice(1) });
    else if (raw.startsWith('-')) lines.push({ type: 'del', text: raw.slice(1) });
    else lines.push({ type: 'ctx', text: raw.startsWith(' ') ? raw.slice(1) : raw });
  });
  return lines;
}

const DIFF_MAX_LINES = 400;

function diffNode(lines) {
  const box = el('div', 'diff');
  let added = 0, removed = 0;
  for (const line of lines) {
    if (line.type === 'add') added++;
    if (line.type === 'del') removed++;
  }
  const shown = lines.slice(0, DIFF_MAX_LINES);
  for (const line of shown) {
    const row = el('div', 'diff-line ' + line.type);
    if (line.type === 'add' || line.type === 'del' || line.type === 'ctx') {
      row.appendChild(el('span', 'sign', line.type === 'add' ? '+' : line.type === 'del' ? '−' : ' '));
    }
    row.appendChild(el('span', 'code', line.text));
    box.appendChild(row);
  }
  if (lines.length > shown.length) {
    box.appendChild(el('div', 'diff-line gap', '… ' + (lines.length - shown.length) + ' more lines'));
  }
  const stats = el('div', 'diff-stats');
  if (added) stats.appendChild(el('span', 'add', '+' + added));
  if (removed) stats.appendChild(el('span', 'del', '−' + removed));
  if (stats.childNodes.length) box.prepend(stats);
  return box;
}

// mergedStepNode: a run of reads and searches as one line, the children a click away
function mergedStepNode(step, round) {
  const open = state.openBlocks.has(step.key);
  const files = new Set();
  let searches = 0;
  for (const child of step.children) {
    const kind = toolKind(stepName(child));
    if (kind === 'read') {
      const file = fileOf(child.call && child.call.arguments);
      files.add(file || child.key);
    } else {
      searches++;
    }
  }
  const row = markStep(el('div', 'step merged tool-read' + (open ? ' open' : '')), step);
  const head = el('button', 'step-head');
  head.type = 'button';
  head.setAttribute('aria-expanded', open ? 'true' : 'false');
  head.appendChild(el('span', 'glyph', '✓'));
  head.appendChild(el('span', 'step-name', 'explored'));
  const parts = [];
  if (files.size) parts.push('read ' + files.size + (files.size === 1 ? ' file' : ' files'));
  if (searches) parts.push('searched ' + (searches === 1 ? 'once' : searches === 2 ? 'twice' : searches + ' times'));
  head.appendChild(el('span', 'step-target', parts.join(', ')));
  const meta = el('span', 'step-meta');
  const total = step.children.reduce((sum, c) => sum + (Number(c.result && c.result.durationMs) || 0), 0);
  if (total > 0) meta.appendChild(el('span', '', formatDuration(total)));
  meta.appendChild(el('span', '', step.children.length + ' steps'));
  head.appendChild(meta);
  head.title = open ? 'Fold the run' : 'Open the run: every read and search';
  head.addEventListener('click', () => {
    if (open) state.openBlocks.delete(step.key);
    else state.openBlocks.add(step.key);
    renderMessages();
  });
  row.appendChild(head);
  if (open) {
    const list = el('div', 'steps nested');
    for (const child of step.children) list.appendChild(toolStepNode(child, round, {}));
    row.appendChild(list);
  }
  return row;
}

function thinkingStepNode(step) {
  const open = state.openBlocks.has(step.key);
  const text = String(step.block.content || '');
  const row = el('div', 'step thinking' + (open ? ' open' : ''));
  const head = el('button', 'step-head');
  head.type = 'button';
  head.setAttribute('aria-expanded', open ? 'true' : 'false');
  head.appendChild(el('span', 'glyph', '∴'));
  head.appendChild(el('span', 'step-name', 'thinking'));
  head.appendChild(el('span', 'step-target', open ? '' : oneLine(text).slice(0, 120)));
  const meta = el('span', 'step-meta');
  meta.appendChild(el('span', '', text.length + (step.block.truncated ? '+' : '') + ' chars'));
  head.appendChild(meta);
  head.addEventListener('click', () => {
    if (open) state.openBlocks.delete(step.key);
    else state.openBlocks.add(step.key);
    renderMessages();
  });
  row.appendChild(head);
  if (open) {
    const box = el('div', 'step-details');
    box.appendChild(el('div', 'block thinking', text));
    if (step.block.truncated) {
      box.appendChild(button('ghost tiny', 'Show full thinking', (event) => loadFullBlock(step, event.currentTarget)));
    }
    row.appendChild(box);
  }
  return row;
}

// noteNode: what the agent said along the way, before its reply
function noteNode(step) {
  const rendered = markdownNode(step.block.content, el('div', 'note md' + (step.role === 'user' ? ' from-user' : '')));
  const plain = rendered.textContent || '';
  if (plain.length <= FOLD_AT.text) return rendered;
  return foldable(rendered, step.key, 'note');
}

// injectedNode: context the CLI put in the user's mouth, folded by default
function injectedNode(step) {
  return foldable(el('div', 'note injected', step.block.content), step.key, 'injected');
}

function renderMessages() {
  const pane = $('messages');
  const detail = state.detail;
  if (!detail) return;

  // The same session showing the same slice counts as the same view, and a rebuild has to
  // land back where it was
  const view = detail.sessionId + '|' + state.order;
  const sameView = pane.dataset.view === view;
  const metrics = paneMetrics(pane);
  const prevScroll = metrics.top;
  const wasAtBottom = sameView && paneShown(pane) &&
    metrics.height - metrics.top - metrics.visible < STICK_TO_BOTTOM_PX;

  const all = detail.messages.messages || [];
  const { preface, rounds } = roundsOf(all);
  const record = state.byId.get(state.selectedId);
  streamDay = dayKey(all.length ? all[all.length - 1].timestamp : '') || dayKey(record && record.updatedAt);

  const box = document.createDocumentFragment();
  let shown = 0;
  if (state.view !== 'changes') {
    // Whatever came before the first ask: a Codex session opens with the instructions the
    // CLI assembled, a Claude one with command plumbing
    for (const { message, index } of preface) {
      if (state.view === 'conversation' && !hasWords(message)) continue;
      const node = messageNode(message, index);
      node.dataset.anchor = 'm:' + messageKey(message);
      box.appendChild(node);
      shown++;
    }
  }
  for (const round of rounds) {
    const node = roundNode(round);
    if (!node) continue;
    node.dataset.anchor = 'r:' + round.key;
    box.appendChild(node);
    shown++;
  }
  if (!shown) box.appendChild(el('p', 'empty', emptyStreamNote(all.length)));
  // A rebuild of the same view holds the reader's place by what they were looking at, not
  // by a pixel offset: a refresh that adds a step above the viewport, or a round opened
  // above it, would otherwise move the text under their eyes
  const anchor = sameView ? captureAnchor(pane) : null;
  pane.replaceChildren(box);
  pane.dataset.view = view;
  // A session just opened eases its messages in. A refresh of the same view rebuilds
  // in place and must not: the reader is in the middle of it.
  if (!sameView) {
    pane.classList.add('fresh');
    setTimeout(() => pane.classList.remove('fresh'), 300);
  }

  // The match being shown keeps its marks through a rebuild: opening a step, a refresh
  const find = state.find;
  if (find && find.current >= 0 && find.targets[find.current]) {
    const node = pane.querySelector(targetSelector(find.targets[find.current]));
    if (node) markMatches(node, find.query);
  }

  if (!sameView) {
    // Just opened: stick to the bottom when viewing the latest, start at the top for the earliest
    scrollPaneTo(pane, state.order === 'desc' ? SCROLL_END : 0);
  } else if (!paneShown(pane)) {
    // Another screen is up; the place kept for this one stands
  } else if (wasAtBottom) {
    scrollPaneTo(pane, SCROLL_END);
  } else if (!restoreAnchor(pane, anchor)) {
    scrollPaneTo(pane, prevScroll);
  }
}

// captureAnchor notes the first round (or message) still in view and how far below the
// top of the pane it starts; restoreAnchor puts that same element back at that offset
// after a rebuild. Keys are the ones rounds and messages already have, which do not
// change when a page is prepended or a step is opened.
function captureAnchor(pane) {
  if (!paneShown(pane)) return null;
  const paneTop = isPhone() ? 0 : pane.getBoundingClientRect().top;
  for (const node of pane.querySelectorAll('[data-anchor]')) {
    const box = node.getBoundingClientRect();
    if (box.bottom - paneTop > 0) return { key: node.dataset.anchor, offset: box.top - paneTop };
  }
  return null;
}

function restoreAnchor(pane, anchor) {
  if (!anchor || !paneShown(pane)) return false;
  const node = pane.querySelector('[data-anchor="' + CSS.escape(anchor.key) + '"]');
  if (!node) return false;
  const paneTop = isPhone() ? 0 : pane.getBoundingClientRect().top;
  scrollerOf(pane).scrollTop += (node.getBoundingClientRect().top - paneTop) - anchor.offset;
  return true;
}

// ---------------------------------------------------------------------------
// Right pane: the final result stays visible (no scrolling back to the top to find it)
// ---------------------------------------------------------------------------

// Sources name their usage fields differently; fold them into readable labels, with cost
// to four decimal places
const USAGE_LABELS = {
  inputTokens: 'in', outputTokens: 'out',
  input_tokens: 'in', output_tokens: 'out',
  cacheReadTokens: 'cache read', cacheWriteTokens: 'cache write',
  reasoningTokens: 'reasoning', totalTokens: 'total', estimatedCostUsd: 'cost $',
};
const USAGE_DECIMALS = { estimatedCostUsd: 4 };

// Token counts run to nine digits on a long session; unseparated they read as noise
function groupDigits(v) {
  if (typeof v !== 'number' || !isFinite(v)) return String(v);
  return v.toLocaleString('en-US');
}

function finalCard(final) {
  const done = final.isFinal === true;
  const card = el('section', 'card ' + (done ? 'final-done' : 'final-pending'));
  card.appendChild(el('h3', '', 'Final result'));

  const badges = el('div', 'badges');
  badges.appendChild(el('span', 'badge ' + (done ? 'ok' : 'warn'), done ? 'Complete' : 'Incomplete'));
  if (final.isProcessing) badges.appendChild(el('span', 'badge warn', 'Working'));
  if (final.stopReason) badges.appendChild(el('span', 'badge', final.stopReason));
  card.appendChild(badges);

  if (final.error) card.appendChild(el('div', 'text', final.error));
  if (final.text) card.appendChild(markdownNode(final.text, el('div', 'text md')));
  if (!final.text && !final.error) card.appendChild(el('div', 'text dim', '(no text result)'));

  if (final.thinking) {
    const details = el('details');
    details.appendChild(summaryNode([el('span', '', 'Thinking')]));
    details.appendChild(el('div', 'block thinking', final.thinking));
    card.appendChild(details);
  }
  card.appendChild(kv([
    ['Model', final.model],
    ['Time', final.timestamp],
  ]));
  return card;
}

function usageCard(usage) {
  const pairs = Object.entries(usage)
    .filter(([, v]) => v !== null && v !== undefined && v !== '')
    // Usage objects mix in nested objects (cache_creation, output_tokens_details, ...),
    // and String() on those yields a row of [object Object] with no information at all
    .filter(([, v]) => typeof v !== 'object')
    // Recognised fields (in / out / cache / cost) come first, the rest follow as-is
    .sort((a, b) => (USAGE_LABELS[a[0]] ? 0 : 1) - (USAGE_LABELS[b[0]] ? 0 : 1))
    .map(([k, v]) => {
      const digits = USAGE_DECIMALS[k];
      const text = digits !== undefined ? Number(v).toFixed(digits) : groupDigits(v);
      return [USAGE_LABELS[k] || k, text];
    });
  if (pairs.length === 0) return null;
  const card = el('section', 'card');
  // The heading says which question this answers: the totals cover every turn of the
  // session, which is a different number from the one the final message carries
  card.appendChild(el('h3', '', 'Usage \u00b7 whole session'));
  card.appendChild(kv(pairs));
  return card;
}

// The export formats the server offers. Kept in the order the route lists them, with the
// label rather than the bare extension: "jsonl" alone does not say what it is for.
const EXPORT_FORMATS = [
  ['md', 'Markdown'],
  ['jsonl', 'JSONL'],
  ['json', 'JSON'],
  ['html', 'HTML'],
];

// What each format is for — a select cannot show this, and the option labels have to stay
// short enough to read in a narrow side pane. The formats are documented in docs/api.md.
const EXPORT_HINTS = {
  md: 'Markdown — for reading or pasting',
  jsonl: 'JSONL — one JSON object per line, for jq',
  json: 'JSON — the same data in one document',
  html: 'HTML — a standalone page, no scripts',
};

// safeFilename mirrors the server's sanitizeFilename: the characters a filesystem will
// not take become dashes, so the two never disagree about what a download is called.
function safeFilename(name) {
  const cleaned = String(name || '').replace(/[\\/:*?"<>|]/g, '-').replace(/[\x00-\x1f]/g, '-');
  return cleaned.replace(/^[. ]+|[. ]+$/g, '') || 'session';
}

// copyBrief fetches the session's handoff brief and puts it on the clipboard: the brief
// exists to be handed to another agent, so clipboard-first beats a download.
function copyBrief(record) {
  return async (event) => {
    const node = event.currentTarget;
    const original = node.textContent;
    try {
      const headers = state.token ? { Authorization: 'Bearer ' + state.token } : {};
      const res = await fetch('/sessions/' + encodeURIComponent(record.sessionId) + '/brief', { headers });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      await navigator.clipboard.writeText(await res.text());
      setText(node, 'Copied');
    } catch (err) {
      setText(node, 'Failed');
    }
    setTimeout(() => setText(node, original), 1500);
  };
}

// shellArg quotes a path that is not plainly safe to paste into a shell. Almost every
// working directory passes through untouched; the quoting is there so that one containing
// a space or a quote cannot turn the copied line into two commands.
function shellArg(value) {
  return /^[A-Za-z0-9_@%+=:,.\/-]+$/.test(value)
    ? value
    : "'" + String(value).replace(/'/g, "'\\''") + "'";
}

// copyText puts a string already in hand on the clipboard, reporting back on the button
// that asked for it. Same shape as copyBrief, which has to fetch first.
function copyText(value) {
  return async (event) => {
    const node = event.currentTarget;
    const original = node.textContent;
    try {
      await navigator.clipboard.writeText(value);
      setText(node, 'Copied');
    } catch (err) {
      setText(node, 'Failed');
    }
    setTimeout(() => setText(node, original), 1500);
  };
}

// downloadExport exports the session in the chosen format.
// The endpoint requires authentication, so a plain <a href> will not do — the request has
// to carry the Authorization header and the download is built from the response.
async function downloadExport(record, node) {
  const original = node.textContent;
  setText(node, 'Exporting\u2026');
  try {
    // No limit: an export is the whole session. MESSAGE_LIMIT is the message stream's page
    // size and had no business here — it made the button export the latest 200 messages of
    // a session that might have sixteen thousand, with nothing in the file to say so.
    const path = '/sessions/' + encodeURIComponent(record.sessionId) +
      '/export?order=' + state.order + '&format=' + state.exportFormat;
    const headers = {};
    if (state.token) headers.Authorization = 'Bearer ' + state.token;
    const res = await fetch(path, { headers });
    if (!res.ok) throw new Error('HTTP ' + res.status);

    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    // The server sends the right filename in Content-Disposition, but this builds the
    // download from a blob and sets the name itself — which it did with '.md' hardcoded,
    // so every format arrived as Markdown. The extension is the format, and the name is
    // cleaned the way the server cleans its own, so a title containing a slash does not
    // become a path.
    link.download = safeFilename(record.shortKey || record.sessionId) + '.' + state.exportFormat;
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
    setText(node, 'Exported');
  } catch (err) {
    handleError(err);
    setStatus('Export failed: ' + err.message);
    setText(node, 'Export failed');
  }
  setTimeout(() => setText(node, original), 1500);
}

function sessionCard(record, final) {
  const card = el('section', 'card');
  const head = el('div', 'card-head');
  head.appendChild(el('h3', '', 'Session'));
  // Four formats, one control. The format is a preference and is remembered with the
  // others; a per-session choice would be surprising the next time you exported.
  const picker = el('select', 'fmt');
  picker.setAttribute('aria-label', 'Export format');
  for (const [value, label] of EXPORT_FORMATS) {
    const option = el('option', '', label);
    option.value = value;
    picker.appendChild(option);
  }
  picker.value = state.exportFormat;
  picker.addEventListener('change', () => {
    state.exportFormat = picker.value;
    saveViewPrefs();
    renderSide(record); // the button names the format it will produce
  });
  head.appendChild(picker);
  // The picker beside it already names the format; repeating it in the button made the
  // row too wide for the pane and pushed the button past the card's edge
  const briefButton = button('ghost tiny', 'Brief', copyBrief(record));
  briefButton.title = 'A compact handoff brief of the latest round, for handing to another agent';
  head.appendChild(briefButton);
  const exportButton = button('ghost tiny', 'Export',
    (event) => downloadExport(record, event.currentTarget));
  exportButton.title = EXPORT_HINTS[state.exportFormat] || 'Export';
  head.appendChild(exportButton);
  card.appendChild(head);

  // Laid out plainly rather than behind a fold: it is a short table, and the file path
  // under it is what you copy when a session needs looking at
  const body = el('div');
  body.appendChild(kv([
    ['Source', record.source],
    ['Messages', final && final.messageCount],
    ['Updated', record.updatedAt],
    ['Created', record.createdAt],
    ['Model', record.model],
    ['Project', record.project],
    // The branch the session opened on, as the CLI wrote it down at the time. Not what
    // the checkout is on now: the work is months old and the branch may be gone.
    ['Branch', record.branch],
    ['CLI', record.cliVersion],
    ['Token', record.totalTokens],
    ['Cost $', record.estimatedCostUsd],
    ['File', record.hasFile ? 'present' : 'missing (may exist only in state.db)'],
  ]));
  // How to reopen this session in the CLI that wrote it. The server omits the field for
  // the two sources that cannot be resumed by id, so the row is absent rather than empty.
  //
  // The command is shown bare and copied with a cd in front of it. Measured on Claude Code
  // and Grok, both resolve a session id from any working directory, so the cd is not what
  // makes the session findable — it is what makes the resumed agent work in the right
  // place. Without it the conversation continues while its tools point somewhere else,
  // which on a coding session is worse than not resuming at all. The button says what it
  // copies, so the difference between the two is stated rather than hidden.
  if (record.resumeCommand) {
    const resume = el('div', 'resume');
    resume.appendChild(el('code', '', record.resumeCommand));
    // cwd is not always a directory: a labeled --path instance prefixes it with its label
    // (box2:/srv/proj) so the sessions group separately. Prefixing a cd with that produces
    // a command that fails, and since the two are joined by && the resume never runs — a
    // button worse than no button. Only a plainly absolute path earns the cd.
    const dir = /^(\/|[A-Za-z]:[\\/])/.test(record.cwd || '') ? record.cwd : '';
    const full = dir
      ? 'cd ' + shellArg(dir) + ' && ' + record.resumeCommand
      : record.resumeCommand;
    const copy = button('ghost tiny', dir ? 'Copy with cd' : 'Copy', copyText(full));
    copy.title = dir ? 'Copies: ' + full : 'Copies the command';
    resume.appendChild(copy);
    body.appendChild(resume);
  }
  if (record.file) {
    const path = el('p', 'sub', record.file);
    path.title = record.file;
    body.appendChild(path);
  }
  card.appendChild(body);
  return card;
}

function projectTimeline(record) {
  const project = record && (record.project || record.cwd);
  if (!project) return null;
  const items = state.sessions
    .filter((item) => (item.project || item.cwd) === project)
    .slice(0, 12);
  const card = el('section', 'card timeline-card');
  const head = el('div', 'card-head');
  head.appendChild(el('h3', '', 'Project timeline'));
  head.appendChild(el('span', 'count', items.length + (items.length === 12 ? '+' : '')));
  card.appendChild(head);
  card.appendChild(el('p', 'sub', project));
  const list = el('div', 'timeline');
  for (const item of items) {
    const row = el('button', 'timeline-item' + (item.sessionId === state.selectedId ? ' active' : ''));
    row.type = 'button';
    row.title = item.sessionId || '';
    const top = el('span', 'timeline-top');
    top.appendChild(sourceTag(item.source));
    if (item.status) top.appendChild(statusTag(item.status));
    top.appendChild(el('span', 'time', relTime(item.updatedAt)));
    row.appendChild(top);
    row.appendChild(el('span', 'timeline-title', item.shortKey || item.sessionId));
    row.addEventListener('click', () => selectSession(item.sessionId));
    list.appendChild(row);
  }
  card.appendChild(list);
  return card;
}

function renderSide(record) {
  const side = $('side');
  const detail = state.detail;
  if (!record || !detail) {
    side.replaceChildren();
    return;
  }
  // The answer first, then the way around the session, then the facts about it
  const box = document.createDocumentFragment();
  box.appendChild(finalCard(detail.final || {}));
  box.appendChild(tocCard());
  box.appendChild(sessionCard(record, detail.final));
  const usage = usageCard((detail.final && detail.final.usage) || {});
  if (usage) box.appendChild(usage);
  const timeline = projectTimeline(record);
  if (timeline) box.appendChild(timeline);
  box.appendChild(pageCard());
  // A refresh rebuilds this pane; the reader may be halfway down it
  const prevScroll = paneShown(side) ? paneMetrics(side).top : null;
  side.replaceChildren(box);
  if (prevScroll !== null) scrollPaneTo(side, prevScroll);
}

// pageCard says which server the page is talking to, how it was opened and, on a phone,
// how the screen is laid out: the numbers a layout fault on an iPhone comes down to, which
// cannot otherwise be read without a Mac and a cable.
function pageCard() {
  const card = el('section', 'card page-card');
  card.appendChild(el('h3', '', 'This page'));
  const installed = matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
  const pairs = [
    ['Server', state.serverVersion || 'unknown'],
    ['Opened as', installed ? 'an installed app' : 'a browser tab'],
  ];
  if (isPhone()) {
    const visual = window.visualViewport;
    const app = $('app').getBoundingClientRect();
    const env = envInsets();
    const used = state.insets || env;
    const inset = [env.top, env.right, env.bottom, env.left].join(' ')
      + ' · keeping clear of ' + used.top + ' above, ' + used.bottom + ' below'
      + (used.assumed ? ' (assumed: nothing was reported)' : '');
    pairs.push(['Screen', screen.width + '×' + screen.height]);
    pairs.push(['Viewport', innerWidth + '×' + innerHeight
      + (visual ? ', visible ' + Math.round(visual.width) + '×' + Math.round(visual.height) : '')]);
    pairs.push(['Moved', 'scroll ' + Math.round(scrollY)
      + (visual ? ', visual ' + Math.round(visual.offsetTop) + ', scale ' + visual.scale.toFixed(2) : '')]);
    pairs.push(['App', Math.round(app.top) + ' to ' + Math.round(app.bottom)
      + ' of a ' + document.documentElement.scrollHeight + ' document']);
    pairs.push(['Safe area', inset]);
  }
  card.appendChild(kv(pairs));
  return card;
}

// ---------------------------------------------------------------------------
// Selection and detail synchronisation
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Conversation table of contents
// ---------------------------------------------------------------------------

// tocCard lists the rounds in the loaded window, two lines each: what you asked, and
// what the agent concluded, with how many steps it took and a mark when one failed.
// Clicking either line jumps to it in the stream.
function tocCard() {
  const detail = state.detail;
  const card = el('section', 'card toc-card');
  const all = (detail && detail.messages.messages) || [];
  const rounds = roundsOf(all).rounds.filter((r) => r.ask);

  const head = el('h3', '', 'Conversation');
  if (rounds.length) head.appendChild(el('span', 'count', rounds.length));
  card.appendChild(head);

  // The list covers the loaded window, which is the latest page by default; say so when
  // the session is longer, or the missing early turns look like a bug
  const total = Number(detail && detail.final && detail.final.messageCount) || 0;
  if (total > all.length) {
    card.appendChild(el('p', 'dim toc-note',
      'From the latest ' + all.length + ' of ' + total + ' messages'));
  }

  if (!rounds.length) {
    card.appendChild(el('p', 'dim', 'No user messages in this window'));
    return card;
  }

  const list = el('ol', 'toc');
  for (const round of rounds) {
    const item = el('li', 'toc-round');
    const ask = el('button', 'toc-item toc-ask');
    ask.type = 'button';
    ask.appendChild(el('span', 'toc-no', round.no));
    ask.appendChild(el('span', 'toc-text', tocPreviewOf(round.ask.message)));
    ask.title = tocPreviewOf(round.ask.message, 400);
    ask.addEventListener('click', () => gotoRound(round.no, 'ask'));
    item.appendChild(ask);

    const reply = el('button', 'toc-item toc-reply');
    reply.type = 'button';
    const meta = el('span', 'toc-meta');
    const s = round.summary;
    if (s.steps) meta.appendChild(el('span', '', s.steps + (s.steps === 1 ? ' step' : ' steps')));
    if (s.failures || s.interrupted) {
      const dot = el('span', 'toc-dot');
      dot.title = s.failures ? s.failures + ' failed' : 'interrupted';
      meta.appendChild(dot);
    }
    reply.appendChild(meta);
    const text = round.final
      ? tocPreviewOf(round.final.message)
      : (s.interrupted ? 'interrupted' : 'no reply');
    reply.appendChild(el('span', 'toc-text' + (round.final ? '' : ' dim'), text));
    reply.title = round.final ? tocPreviewOf(round.final.message, 400) : text;
    reply.addEventListener('click', () => gotoRound(round.no, 'final'));
    item.appendChild(reply);
    list.appendChild(item);
  }
  card.appendChild(list);
  return card;
}

// tocPreviewOf is a message's own words, folded to one line, without the blocks a harness
// closed them with; "" when it has none (a tool result, an attachment, a bare signature)
function tocPreviewOf(message, max) {
  const limit = max || 70;
  for (const block of message.content || []) {
    if (block.type === 'text' && block.content) {
      const one = plainText(stripHarness(block.content));
      if (!one) continue;
      return one.length > limit ? one.slice(0, limit) + '…' : one;
    }
  }
  return '';
}

// gotoRound scrolls a round's ask or reply into view and flashes it. A step view drops the
// rounds without its kind of step, so a miss there falls back to the whole view first.
function gotoRound(no, part) {
  // On a phone the list of rounds is on the details screen and the rounds themselves on
  // the conversation screen behind it: step back to that first, and jump once it is up
  if (isPhone() && state.pane !== 'stream') {
    window.addEventListener('popstate', () => gotoRound(no, part), { once: true });
    history.back();
    return;
  }
  const pane = $('messages');
  const find = () => pane.querySelector('.round[data-round="' + no + '"]');

  let node = find();
  if (!node && stepView(state.view)) {
    setView('all');
    node = find();
  }
  if (!node) return;
  const target = (part === 'final' && node.querySelector('.msg.final')) || node.querySelector('.msg') || node;
  flashInView(target);
}

// flashInView centres a node in the stream and flashes it, so the eye lands where the jump
// went; focus, when given, is what gets centred instead — a word inside a tall node, which
// may sit in a scrolling output box of its own. Instant on a phone: the page is the scroller
// there, and a glide still under way when the next screen comes up would carry that screen
// off with it.
function flashInView(node, focus) {
  (focus || node).scrollIntoView({ block: 'center', behavior: isPhone() ? 'auto' : 'smooth' });
  node.classList.add('flash');
  setTimeout(() => node.classList.remove('flash'), 1400);
}

// ---------------------------------------------------------------------------
// Finding your way in a long session: the failures one at a time, and words inside it
//
// Both walk targets in the loaded window — the steps that failed, the places whose text
// holds the words — and bring each into view: the view switched to one that draws it, its
// round opened when folding hides it, the step opened, then centred and flashed. A session
// longer than its window carries on past the window's edges, where the server says what is
// there (the rounds that failed, the messages that match) and the window is anchored at it
// the way a search hit from the list anchors it.
// ---------------------------------------------------------------------------

// windowMessages is the loaded window, in order
function windowMessages() {
  return (state.detail && state.detail.messages.messages) || [];
}

// windowIsWhole: the window holds every message of the session, so nothing lies past it
function windowIsWhole() {
  const detail = state.detail;
  if (!detail) return true;
  const total = Number(detail.final && detail.final.messageCount) || 0;
  return total <= windowMessages().length;
}

// windowSpan is the earliest and latest time in the window: what lies outside it in time
// lies outside the window. NaN when the window's rows carry no readable time.
function windowSpan() {
  let from = NaN;
  let to = NaN;
  for (const message of windowMessages()) {
    const t = parseTime(message.timestamp);
    if (isNaN(t)) continue;
    if (isNaN(from) || t < from) from = t;
    if (isNaN(to) || t > to) to = t;
  }
  return { from, to };
}

// targetIndex is the window position a target is drawn at; targetTime the time it happened
function targetIndex(target) {
  return target.kind === 'step' ? target.step.index : target.index;
}
function targetTime(target) {
  if (target.hitAt !== undefined) return target.hitAt;
  if (target.kind === 'step') {
    const message = target.step.resultMessage || target.step.message || windowMessages()[target.step.index];
    return parseTime(message && message.timestamp);
  }
  const message = windowMessages()[target.index];
  return parseTime(message && message.timestamp);
}

// viewTopIndex is the window position of the first message showing at the top of the
// stream — where "the next one" is counted from when there is no last jump to go on from
function viewTopIndex() {
  const pane = $('messages');
  const top = isPhone() ? $('stream-head').getBoundingClientRect().bottom : pane.getBoundingClientRect().top;
  for (const node of pane.querySelectorAll('[data-index]')) {
    if (node.getBoundingClientRect().bottom > top) return Number(node.dataset.index);
  }
  return Infinity;
}

// nextFrom picks where a walk goes: on from the target it showed last while that one is
// still on screen, and from what is on screen otherwise — so scrolling away and asking
// again starts from where you are. -1 or the length when the window runs out that way.
function nextFrom(targets, lastKey, direction) {
  const last = lastKey ? targets.findIndex((t) => t.key === lastKey) : -1;
  if (last >= 0 && onScreen($('messages').querySelector(targetSelector(targets[last])))) {
    return last + direction;
  }
  const top = viewTopIndex();
  if (direction > 0) {
    const i = targets.findIndex((t) => targetIndex(t) >= top);
    return i < 0 ? targets.length : i;
  }
  let i = -1;
  targets.forEach((t, k) => { if (targetIndex(t) < top) i = k; });
  return i;
}

// onScreen: some of the node shows in the stream right now — below the head that sticks
// over the page on a phone, inside the pane on a wide screen
function onScreen(node) {
  if (!node) return false;
  const pane = $('messages');
  const top = isPhone() ? $('stream-head').getBoundingClientRect().bottom : pane.getBoundingClientRect().top;
  const bottom = isPhone() ? document.documentElement.clientHeight : pane.getBoundingClientRect().bottom;
  const box = node.getBoundingClientRect();
  return box.bottom > top && box.top < bottom;
}

// targetSelector finds a target's node in the stream: a step by its key, a bubble by its
// window position
function targetSelector(target) {
  if (target.kind === 'step') return '[data-step="' + CSS.escape(target.step.key) + '"]';
  return '.msg[data-index="' + target.index + '"]';
}

// drawnIn says whether a view draws a target at all, folded or not
function drawnIn(view, target) {
  const only = stepView(view);
  if (target.kind === 'step') {
    if (only) return target.step.kind === 'tool' && only.test(target.step);
    if (view === 'conversation') return target.step.kind === 'note' && target.step.role !== 'user';
    return true;
  }
  if (only) return target.kind === 'ask' && flatToolSteps(target.round.steps).some(only.test);
  if (view === 'conversation' && target.kind === 'preface') return hasWords(windowMessages()[target.index]);
  return true;
}

// revealTarget brings a target into view and flashes it; needle, when given, is marked
// wherever it appears in what was revealed. Opening is part of revealing: a step shows
// what it holds — the output of a failure, the text a match is in — and a round folded
// over the step opens. A failure needs no open round when it is one of the five a folded
// round keeps in view.
function revealTarget(target, needle) {
  if (!drawnIn(state.view, target)) {
    state.view = 'all';
    saveViewPrefs();
    renderStreamHead(state.byId.get(state.selectedId));
  }
  if (target.kind === 'step') {
    const step = target.step;
    state.openBlocks.add(step.key);
    if (target.merged) state.openBlocks.add(target.merged.key);
    if (state.view === 'all') {
      const pinned = isFailedStep(step) && !target.merged &&
        flatToolSteps(target.round.steps).filter(isFailedStep).indexOf(step) < 5;
      if (!pinned || target.merged) state.roundOverrides.set(target.round.key, true);
    }
  }
  renderMessages();
  const node = $('messages').querySelector(targetSelector(target));
  if (!node) return false;
  clearMarks();
  flashInView(node, needle ? markMatches(node, needle) : null);
  return true;
}

// markMatches wraps each case-folded occurrence of the needle under node in a <mark>,
// text node by text node — the page builds nothing from markup, and neither does this. It
// returns the first mark, or null when the words are nowhere in what the node draws.
//
// A step's head and a fold's summary are left alone: each is one clipped line restating
// what the opened step or block shows in full, and centring a word in one scrolled the
// clipped line sideways, leaving a bar of colour where the command was.
function markMatches(node, needle) {
  const lower = needle.toLowerCase();
  // Positions found in the folded text are used on the original, which holds only while
  // folding keeps the length — not so for a few letters (İ folds to two code units)
  if (lower.length !== needle.length) return null;
  const walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT);
  const texts = [];
  while (walker.nextNode()) {
    const value = walker.currentNode.nodeValue;
    const folded = value.toLowerCase();
    if (folded.length !== value.length || !folded.includes(lower)) continue;
    if (walker.currentNode.parentElement.closest('.step-head, summary')) continue;
    texts.push(walker.currentNode);
  }
  let first = null;
  for (let text of texts) {
    let at = text.nodeValue.toLowerCase().indexOf(lower);
    while (at >= 0) {
      const hit = text.splitText(at);
      text = hit.splitText(needle.length);
      const mark = el('mark', 'find-hit');
      hit.parentNode.replaceChild(mark, hit);
      mark.appendChild(hit);
      if (!first) first = mark;
      at = text.nodeValue.toLowerCase().indexOf(lower);
    }
  }
  return first;
}

function clearMarks() {
  for (const mark of $('messages').querySelectorAll('mark.find-hit')) {
    const parent = mark.parentNode;
    parent.replaceChild(document.createTextNode(mark.textContent), mark);
    parent.normalize();
  }
}

// ---- Failures, one at a time ----

// failureTargets lists the window's failed steps in the order they ran
function failureTargets() {
  const out = [];
  for (const round of roundsOf(windowMessages()).rounds) {
    for (const step of flatToolSteps(round.steps)) {
      if (isFailedStep(step)) out.push({ kind: 'step', key: step.key, round, step, merged: null });
    }
  }
  return out;
}

// gotoFailure steps to the next (1) or previous (-1) failed step. Past the window's edge it
// goes to the nearest round beyond it that failed; with nothing further that way it comes
// round to the other end of the window.
async function gotoFailure(direction) {
  if (!state.detail || state.busy) return;
  const targets = failureTargets();
  let next = nextFrom(targets, state.failureAt, direction);
  if (next < 0 || next >= targets.length) {
    if (await failureBeyond(direction)) return;
    if (!targets.length) {
      setStatus('No failed steps in this session');
      return;
    }
    next = direction > 0 ? 0 : targets.length - 1;
  }
  showFailure(targets[next]);
}

function showFailure(target) {
  state.failureAt = target.key;
  revealTarget(target);
}

// failureBeyond anchors the window at the nearest round past its edge that has a failure,
// from the session's rounds as the server counts them. False when there is none that way,
// or when the window cannot be placed in time.
async function failureBeyond(direction) {
  if (windowIsWhole()) return false;
  const span = windowSpan();
  if (isNaN(span.from)) return false;
  let rounds;
  try {
    rounds = await sessionRoundsIndex();
  } catch (err) {
    handleError(err);
    return false;
  }
  const endOf = (r) => r.endAt || r.lastAt || r.startAt;
  let target = null;
  for (const r of rounds) {
    if (!(r.failures > 0)) continue;
    if (direction > 0 && parseTime(r.startAt) > span.to) { target = r; break; }
    if (direction < 0 && parseTime(endOf(r)) < span.from) target = r;
  }
  if (!target) return false;
  state.reveal = {
    kind: 'failure', sessionId: state.detail.sessionId, direction,
    from: parseTime(target.startAt), to: parseTime(endOf(target)),
  };
  // The window takes the anchor the way it reads: ending there for the latest-first order,
  // starting there for the earliest-first one — either way the round is in it
  state.focusAt = state.order === 'desc' ? endOf(target) : target.startAt;
  setStatus('Going to ' + (direction > 0 ? 'a later' : 'an earlier') + ' round that failed\u2026');
  syncDetail({ force: true });
  return true;
}

// sessionRoundsIndex is the open session's rounds from the server, read once per state of
// the session: the whole session's failures, where the window only knows its own
async function sessionRoundsIndex() {
  const detail = state.detail;
  const key = detail.sessionId + '|' + detail.signature;
  if (state.roundsIndex && state.roundsIndex.key === key) return state.roundsIndex.rounds;
  const data = await api('/sessions/' + encodeURIComponent(detail.sessionId) + '/rounds');
  state.roundsIndex = { key, rounds: data.rounds || [] };
  return state.roundsIndex.rounds;
}

// ---- Words in the session ----

// searchableText is what find looks through in a block: the words, the thinking, a call's
// name and the values of its arguments, a result's output, an event's text. Values, not the
// arguments as JSON: a key is not something the page shows, and Bash's own timeout argument
// made every command a match for "timeout".
function searchableText(block) {
  switch (block.type) {
    case 'toolCall': return (block.name || '') + '\n' + argumentValues(block.arguments, 0);
    case 'toolResult': return String(block.content || '');
    case 'event': return eventLabel(block);
    default: return String(block.content == null ? '' : block.content);
  }
}

// argumentValues is the string values of a call's arguments, however deep, one to a line
function argumentValues(value, depth) {
  if (typeof value === 'string') return value;
  if (!value || typeof value !== 'object' || depth > 6) return '';
  return Object.values(value).map((v) => argumentValues(v, depth + 1)).filter(Boolean).join('\n');
}

// blockPlaces maps each block of the window to the place that draws it: a bubble draws all
// of a message ('*'), a reply only its words ('text'), and every other block is a step's
function blockPlaces(all) {
  const { preface, rounds } = roundsOf(all);
  const indexOf = new Map(all.map((message, index) => [message, index]));
  const places = new Map();
  for (const { index } of preface) {
    places.set(index + ':*', { kind: 'preface', key: 'm:' + index, index });
  }
  for (const round of rounds) {
    if (round.ask) places.set(round.ask.index + ':*', { kind: 'ask', key: 'a:' + round.key, round, index: round.ask.index });
    if (round.final) places.set(round.final.index + ':text', { kind: 'final', key: 'f:' + round.key, round, index: round.final.index });
    for (const step of round.steps) {
      for (const child of step.kind === 'merged' ? step.children : [step]) {
        const target = { kind: 'step', key: child.key, round, step: child, merged: step.kind === 'merged' ? step : null };
        if (child.position !== undefined) places.set(child.index + ':' + child.position, target);
        if (child.resultMessage) places.set(indexOf.get(child.resultMessage) + ':' + child.resultPosition, target);
      }
    }
  }
  return places;
}

function placeOf(places, index, block, position) {
  return places.get(index + ':' + position) || places.get(index + ':*') ||
    (block.type === 'text' ? places.get(index + ':text') : null) || null;
}

// ownTargets lists the places whose text in the window holds the needle, in order. One
// place is one target, however many of its blocks match.
function ownTargets(needle, places) {
  const lower = needle.toLowerCase();
  const out = [];
  const seen = new Set();
  windowMessages().forEach((message, index) => {
    (message.content || []).forEach((block, position) => {
      if (!searchableText(block).toLowerCase().includes(lower)) return;
      const target = placeOf(places, index, block, position);
      if (target && !seen.has(target.key)) {
        seen.add(target.key);
        out.push(target);
      }
    });
  });
  return out;
}

// cutTargets are the matches the window holds but cannot see. A server hit inside the window
// at a time where the page found nothing lies past the start of a block the window carries
// only the beginning of — tool output, thinking — so it becomes a target on the message's cut
// block, read whole when the walk gets there. With no cut block at that time the words are
// in something the page does not draw at all: a field a source keeps beside its transcript,
// or a row of its own that is not a message (a prompt Claude Code queued while the agent
// was busy). The message at that time, or the one nearest the row, is still where they
// are, so it is a target too, shown without marks. hitAt keeps the hit's own time for the jumps
// that look for it.
function cutTargets(times, places, own) {
  const all = windowMessages();
  const covered = new Set(own.map(targetTime));
  const keys = new Set(own.map((t) => t.key));
  const out = [];
  const placeIn = (index, accept, extra) => (all[index].content || []).some((block, position) => {
    if (!accept(block)) return false;
    const place = placeOf(places, index, block, position);
    if (!place || keys.has(place.key)) return false;
    keys.add(place.key);
    out.push(Object.assign({}, place, extra(all[index], position)));
    return true;
  });
  for (const time of times) {
    if (covered.has(time)) continue;
    covered.add(time);
    const at = [];
    let nearest = -1;
    let gap = Infinity;
    all.forEach((message, index) => {
      const t = parseTime(message.timestamp);
      if (t === time) at.push(index);
      if (Math.abs(t - time) < gap) {
        nearest = index;
        gap = Math.abs(t - time);
      }
    });
    const cut = (message, position) => ({ cut: { message, position }, hitAt: time });
    const unseen = () => ({ unseen: true, hitAt: time });
    if (at.some((index) => placeIn(index, (block) => block.truncated, cut))) continue;
    if (at.some((index) => placeIn(index, () => true, unseen))) continue;
    if (nearest >= 0) placeIn(nearest, () => true, unseen);
  }
  return out;
}

// listTargets is every match in the window, in the order the stream draws them: the page's
// own, and the server's that the page cannot see. A place already read whole that still
// shows no words is not cut any more, only unseen: the server's hit is there all the same.
function listTargets(find) {
  const places = blockPlaces(windowMessages());
  const own = ownTargets(find.query, places);
  const cut = find.server.inside
    ? cutTargets(find.server.inside, places, own)
      .map((t) => (find.whole.has(t.key) ? Object.assign({}, t, { cut: null, unseen: true }) : t))
    : [];
  return own.concat(cut).sort((a, b) => targetIndex(a) - targetIndex(b));
}

// openFind shows the find bar with the cursor in it. The words already there stay, so f
// and then Enter carries on where the last search left off.
function openFind() {
  if (!state.detail) return;
  const bar = $('find-bar');
  if (bar.classList.contains('hidden')) {
    bar.classList.remove('hidden');
    const toggle = document.querySelector('.find-toggle');
    if (toggle) toggle.classList.add('on');
    if ($('find').value.trim()) refreshFind();
  }
  $('find').focus();
  $('find').select();
}

function closeFind() {
  const bar = $('find-bar');
  if (bar.classList.contains('hidden')) return;
  bar.classList.add('hidden');
  if (state.findTimer) clearTimeout(state.findTimer);
  if (state.findAbort) state.findAbort.abort();
  state.find = null;
  clearMarks();
  setText($('find-count'), '');
  const toggle = document.querySelector('.find-toggle');
  if (toggle) toggle.classList.remove('on');
}

// refreshFind lists the matches for what is in the field against the window as it is now.
// The place shown last stays the current one while the window still holds it. The server is
// asked once per window span — a refresh that changes nothing about where the window lies
// keeps its answer — and its answer is shared by every listing of that span.
function refreshFind() {
  const query = $('find').value.trim();
  const detail = state.detail;
  if (!query || !detail || $('find-bar').classList.contains('hidden')) {
    state.find = null;
    clearMarks();
    setText($('find-count'), '');
    return null;
  }
  const old = state.find;
  const span = windowSpan();
  const same = old && old.query === query && old.sessionId === detail.sessionId;
  const sameSpan = same && old.span.from === span.from && old.span.to === span.to;
  const find = {
    query, sessionId: detail.sessionId, span,
    lastKey: same ? old.lastKey : '',
    whole: same ? old.whole : new Set(), // places read whole already
    server: sameSpan ? old.server : { pending: null, before: null, after: null, inside: null, more: false },
  };
  find.targets = listTargets(find);
  find.current = find.targets.findIndex((t) => t.key === find.lastKey);
  state.find = find;
  if (!find.server.pending) find.server.pending = askServer(find);
  renderFindCount();
  return find;
}

// askServer runs the search scoped to this session's file and places each hit by its time:
// before the window, after it, or inside it. Inside, a hit the page has no match for is in
// text the window holds only the start of.
async function askServer(find) {
  const server = find.server;
  if (state.findAbort) state.findAbort.abort();
  const controller = new AbortController();
  state.findAbort = controller;
  try {
    const record = state.byId.get(find.sessionId);
    const data = await api('/search?q=' + encodeURIComponent(find.query) +
      '&pattern=' + encodeURIComponent(find.sessionId) +
      (record && record.source ? '&source=' + encodeURIComponent(record.source) : '') +
      '&limit=1&per_session=500', { signal: controller.signal });
    const hits = ((data.results || [])[0] || {}).matches || [];
    const whole = windowIsWhole();
    // The window was opened at this instant (a hit from the list, a jump to one): a hit at
    // it is in the window, even when its row is not one the messages are drawn from
    const anchor = parseTime(state.detail && state.detail.anchor);
    server.before = [];
    server.after = [];
    server.inside = [];
    for (const hit of hits) {
      const time = parseTime(hit.timestamp);
      if (isNaN(time)) continue;
      if (time === anchor) server.inside.push(time);
      else if (!whole && time < find.span.from) server.before.push(hit);
      else if (!whole && time > find.span.to) server.after.push(hit);
      else server.inside.push(time);
    }
    // The hits come in file order and stop at the cap, so a cut drops the latest ones
    server.more = !!(data.truncated && data.truncated.hits);
    const current = state.find;
    if (current && current.server === server) {
      current.targets = listTargets(current);
      current.current = current.targets.findIndex((t) => t.key === current.lastKey);
      renderFindCount();
    }
  } catch (err) {
    if (err.name !== 'AbortError') handleError(err);
  }
}

function renderFindCount() {
  const find = state.find;
  if (!find) {
    setText($('find-count'), '');
    return;
  }
  const n = find.targets.length;
  const earlier = find.server.before ? find.server.before.length : 0;
  const later = find.server.after ? find.server.after.length : 0;
  const parts = [];
  if (find.current >= 0) parts.push((find.current + 1) + ' / ' + n);
  else if (n) parts.push(n + (n === 1 ? ' match' : ' matches'));
  else parts.push(earlier || later ? 'none here' : 'No matches');
  if (earlier) parts.push(earlier + ' earlier');
  if (later || find.server.more) parts.push(later + (find.server.more ? '+' : '') + ' later');
  setText($('find-count'), parts.join(' · '));
  $('find-count').classList.toggle('none', !n && !earlier && !later);
}

// findStep moves to the next (1) or previous (-1) match: through the window, then on past
// its edges where the server found more. With nothing further that way it comes round to
// the other end of the session, which lies past the window's other edge when there are
// matches there.
async function findStep(direction) {
  if (!state.detail || state.busy) return;
  let find = state.find;
  if (!find || find.query !== $('find').value.trim() || find.sessionId !== state.detail.sessionId) {
    find = refreshFind();
  }
  if (!find) return;
  let next = nextFrom(find.targets, find.lastKey, direction);
  if (next < 0 || next >= find.targets.length) {
    // Whether there is more past the edge is the server's to say: wait for its answer
    // rather than come round to the other end of a window the session goes on beyond
    await find.server.pending;
    find = state.find;
    if (!find) return;
    next = nextFrom(find.targets, find.lastKey, direction);
  }
  if (next < 0 || next >= find.targets.length) {
    const before = find.server.before || [];
    const after = find.server.after || [];
    const beyond = direction > 0 ? after[0] || before[0] : before[before.length - 1] || after[after.length - 1];
    if (beyond && beyond.timestamp) {
      state.reveal = { kind: 'find', sessionId: state.detail.sessionId, direction, at: parseTime(beyond.timestamp) };
      state.focusAt = beyond.timestamp;
      syncDetail({ force: true });
      return;
    }
    if (!find.targets.length) return;
    next = direction > 0 ? 0 : find.targets.length - 1;
  }
  showMatch(find, next);
}

// showMatch shows one match. A match in cut text reads its block whole first, and is then
// found again among the matches the page can see — the same place, with its words marked.
async function showMatch(find, i) {
  const target = find.targets[i];
  if (target.cut) {
    find.whole.add(target.key);
    try {
      await fetchWhole(target.cut.message, target.cut.position);
    } catch (err) {
      handleError(err);
    }
    // The same place again, read whole now: marked when the words are in what it draws,
    // unmarked when they are not (listTargets keeps it as unseen then). A message can hold
    // more than one cut block; when this one was not it, the server's hit moves on to the
    // next, which is read whole in turn.
    const again = state.find === find ? refreshFind() : null;
    if (!again) return;
    let j = again.targets.findIndex((t) => t.key === target.key);
    if (j < 0) j = again.targets.findIndex((t) => targetTime(t) === targetTime(target));
    if (j >= 0) showMatch(again, j);
    return;
  }
  find.current = i;
  find.lastKey = target.key;
  renderFindCount();
  revealTarget(target, target.unseen ? '' : find.query);
}

// settleReveal finishes a jump that had to load another window first: what it was going
// to is looked for again in the window that came back, by the time it was found at
async function settleReveal() {
  const reveal = state.reveal;
  state.reveal = null;
  if (!reveal || !state.detail || reveal.sessionId !== state.detail.sessionId) return;
  if (reveal.kind === 'failure') {
    const targets = failureTargets();
    const inside = targets.filter((t) => {
      const at = targetTime(t);
      return at >= reveal.from && at <= reveal.to;
    });
    const pool = inside.length ? inside : targets;
    const target = reveal.direction > 0 ? pool[0] : pool[pool.length - 1];
    if (target) showFailure(target);
    setStatus(idleStatus());
    return;
  }
  let find = refreshFind();
  if (!find) return;
  // The match it was going to may be one only the server can see: wait for its answer
  await find.server.pending;
  find = state.find;
  if (!find || !find.targets.length) return;
  let pick = -1;
  if (reveal.direction > 0) {
    pick = find.targets.findIndex((t) => !(targetTime(t) < reveal.at));
  } else {
    find.targets.forEach((t, i) => { if (!(targetTime(t) > reveal.at)) pick = i; });
  }
  if (pick < 0) pick = reveal.direction > 0 ? 0 : find.targets.length - 1;
  showMatch(find, pick);
}

// selectSession opens a session. focusAt, when given, anchors the message window at that
// instant instead of at the end — how a search hit becomes somewhere you can land.
// selectSession opens a session. focusAt, when given, anchors the message window at that
// instant instead of at the end — how a search hit becomes somewhere you can land. quiet
// is the page choosing a session on its own (the newest, on load): the selection is made
// without a history entry and without changing which pane a phone shows.
function selectSession(sessionId, focusAt, quiet) {
  if (!sessionId) return;
  if (sessionId === state.selectedId && (focusAt || '') === state.focusAt) {
    // Already the selected session. On a phone that is still a tap on a row, and a tap
    // on a row means "open it": the page picked this one on load without showing it.
    if (isPhone() && !quiet && state.pane === 'list') {
      history.pushState({ pane: 'stream' }, '', '#' + encodeURIComponent(sessionId));
      showPane('stream');
    }
    return;
  }
  state.focusAt = focusAt || '';
  state.selectedId = sessionId;
  const encoded = encodeURIComponent(sessionId);
  const current = location.hash.replace(/^#/, '');
  if (isPhone()) {
    // On a phone the list and the conversation are different screens: picking a session
    // means you want to read it, and the system's back gesture should bring the list
    // back. The history entry carries the pane so popstate can tell which way it went;
    // the page's own choice replaces the current entry instead, so back still leaves.
    if (quiet) history.replaceState(history.state, '', '#' + encoded);
    else if (current !== encoded) history.pushState({ pane: 'stream' }, '', '#' + encoded);
    else history.replaceState({ pane: 'stream' }, '', '#' + encoded);
    if (!quiet) showPane('stream');
  } else if (current !== encoded) {
    if (quiet) history.replaceState(history.state, '', '#' + encoded);
    else location.hash = encoded;
  }
  renderList();
  syncDetail({ force: true });
}

// isPhone: the one-screen layout, where the three panes are screens behind one another
function isPhone() {
  return matchMedia('(max-width: 860px)').matches;
}

// openDetails slides the details screen in over the conversation, with a history entry
// so the back gesture returns to the conversation
function openDetails() {
  if (!isPhone()) return;
  history.pushState({ pane: 'side' }, '', location.hash || location.pathname);
  showPane('side');
}

// backFromStream and backFromSide are the chevrons on the two inner screens. When the
// screen was reached through a history entry of its own, going back is what the system
// gesture would do; otherwise the screen is shown directly.
function backFromStream() {
  if (history.state && history.state.pane === 'stream') history.back();
  else showPane('list');
}

function backFromSide() {
  if (history.state && history.state.pane === 'side') history.back();
  else showPane('stream');
}

// loadingNodes is the shape of a conversation while one loads: a few blocks the size of
// messages. The stylesheet holds them back for a moment, so the usual fast answer from
// a local server never shows them at all.
function loadingNodes() {
  const box = el('div', 'skeleton');
  box.setAttribute('role', 'status');
  box.setAttribute('aria-label', 'Loading');
  for (let i = 0; i < 4; i++) box.appendChild(el('div', 'sk'));
  return box;
}

function clearDetail(message) {
  state.detail = null;
  renderStreamHead(null);
  $('messages').replaceChildren(el('p', 'empty', message));
  $('messages').dataset.view = '';
  $('side').replaceChildren();
}

// syncDetail only refetches when the selected session has genuinely changed.
// Rebuilding blindly on every refresh would collapse expanded blocks, throw the scroll
// position back to the top, and clear whatever text was selected.
// loadMore extends the window one page at a time.
//
// The window is pinned to one end of the session, and that end has nothing more to give —
// so it grows in the direction the reader is moving away from: with the latest page
// loaded, scrolling up fetches older messages and prepends them; with the earliest page
// loaded, scrolling down appends newer ones. The anchor from the search work is the
// cursor: a page descends to the oldest message on screen, or ascends from the newest.
// messageKey identifies a message across pages. Ids are the reliable case; a session
// whose messages carry none falls back to the fields that make it that message. The
// timestamp alone is not enough — several messages can share one.
function messageKey(message) {
  if (message.id) return 'id:' + message.id;
  const first = (message.content || [])[0] || {};
  return 'k:' + (message.timestamp || '') + '|' + (message.role || '') + '|' +
    String(first.content == null ? '' : first.content).slice(0, 120);
}

async function loadMore(edge) {
  const detail = state.detail;
  if (!detail || state.loadingMore || state.noMore[edge]) return;
  const msgs = (detail.messages && detail.messages.messages) || [];
  if (!msgs.length) return;

  const anchor = edge === 'older' ? msgs[0] : msgs[msgs.length - 1];
  const at = anchor && anchor.timestamp;
  // Normal paging uses an offset and works even for sources whose rows have no
  // timestamp. Only an anchored search window needs the timestamp.
  if (state.focusAt && !at) {
    state.noMore[edge] = true;
    return;
  }

  state.loadingMore = true;
  const pane = $('messages');
  const before = paneMetrics(pane);

  let fresh = null;
  try {
    const id = encodeURIComponent(detail.sessionId);
    const order = edge === 'older' ? 'desc' : 'asc';
    // Timestamp anchors are useful for search hits, but not for paging: many
    // transcripts contain several rows with the same timestamp. Walk by the
    // number of messages already loaded instead, which is stable for every source.
    const cursor = state.focusAt
      ? '&at=' + encodeURIComponent(at)
      : '&offset=' + encodeURIComponent(msgs.length);
    const data = await api('/sessions/' + id + '/messages?limit=' + MESSAGE_LIMIT +
      '&order=' + order + cursor);
    // The session may have changed while this was in flight
    if (state.detail === detail) {
      const got = (data.messages || []);
      // Keep only what is not already on screen. The page overlaps the window by its
      // anchor message, but the overlap is not always exactly one: timestamps repeat
      // within a session (a batch of messages can share one), and the anchored query
      // then returns the same block every time. Slicing one element off assumed an
      // overlap that a repeated timestamp does not give, so the same page was appended
      // over and over. Identity decides it instead, and a page that adds nothing ends
      // the edge.
      const seen = new Set(msgs.map(messageKey));
      fresh = got.filter((m) => !seen.has(messageKey(m)));
    }
  } catch (err) {
    handleError(err);
  } finally {
    // Always, whichever way this left: an early return that skipped this would leave the
    // "Loading more…" label on screen and the guard blocking every later page
    state.loadingMore = false;
  }

  if (!fresh || !fresh.length) {
    if (fresh) state.noMore[edge] = true; // a short page means that edge is the end
    renderStreamHead(state.byId.get(state.selectedId));
    return;
  }

  detail.messages = Object.assign({}, detail.messages, {
    messages: edge === 'older' ? fresh.concat(msgs) : msgs.concat(fresh),
  });
  renderMessages();
  refreshFind();
  const record = state.byId.get(state.selectedId);
  renderStreamHead(record);
  renderSide(record);
  if (edge === 'older') {
    // On a desktop pane, preserve the message that was under the reader's eyes.
    // On a phone the user deliberately pulled to the edge to reveal history: keeping
    // the old pixel position would jump hundreds of messages back down and look like
    // the gesture did nothing. Leave the new history at the top instead.
    if (isPhone()) {
      state.edgeLock = 'older';
      scrollPaneTo(pane, 0);
    } else {
      scrollPaneTo(pane, before.top + (paneMetrics(pane).height - before.height));
    }
  }
}

async function syncDetail(options) {
  const force = options && options.force;
  const record = state.byId.get(state.selectedId);
  if (!record) {
    clearDetail('Pick a session to read it');
    return;
  }

  const switched = !state.detail || state.detail.sessionId !== record.sessionId;

  // Someone who has paged back is reading history. A background refresh refetches the
  // latest page and would replace everything they loaded — on an active session, every
  // ten seconds, which made paging feel like it kept snapping to the newest message.
  // The left pane still updates; only the stream holds still. An explicit refresh
  // (r, the button) passes force and refetches anyway.
  const paged = !switched && (state.detail.messages.messages || []).length > MESSAGE_LIMIT;
  if (!force && paged) return;

  const signature = [record.sessionId, record.updatedAt, record.status, state.order].join('|');
  if (!force && state.detail && state.detail.signature === signature) return;
  if (state.busy) {
    // A fetch is already in flight. Dropping this call used to lose the selection
    // silently: the in-flight response then painted the previous session under the new
    // highlight. Remember it instead, and re-run once the fetch settles.
    state.pendingDetail = true;
    return;
  }

  // A forced refetch rebuilds the window from one page, exactly like a fresh open, so
  // the paging state is reset with it — otherwise a reader who had paged to the end
  // keeps noMore set over a window that no longer holds what they paged through, and
  // the older history is unreachable until they switch sessions
  if (switched || force) {
    state.noMore = { older: false, newer: false };
    state.edgeLock = '';
  }
  if (switched) {
    // Only clear when switching sessions; a plain refresh of the same session keeps the
    // old content on screen and avoids a flash
    state.openBlocks.clear();
    state.roundOverrides.clear();
    state.failureAt = '';
    state.roundsIndex = null;
    $('messages').replaceChildren(loadingNodes());
    $('messages').dataset.view = '';
    $('side').replaceChildren();
  }
  renderStreamHead(record);

  let painted = false;
  state.busy = true;
  try {
    const id = encodeURIComponent(record.sessionId);
    // The anchor travels with the request, not with the session: it belongs to how this
    // session was opened and goes away the moment you page to an end yourself
    const anchor = state.focusAt;
    const at = anchor ? '&at=' + encodeURIComponent(anchor) : '';
    let [messages, final] = await Promise.all([
      api('/sessions/' + id + '/messages?limit=' + MESSAGE_LIMIT + '&order=' + state.order + at),
      api('/sessions/' + id + '/final'),
    ]);
    // An anchored window can come back empty: the instant lies before the session's first
    // message and the window asked for the ones up to it, or after its last and the window
    // asked for the ones from it. A hit in a row that is not a message lands there — Claude
    // Code writes an attachment row just before the prompt it belongs to. The other side of
    // the instant is the window then; an empty stream said the session had no messages.
    if (anchor && !(messages.messages || []).length) {
      const other = state.order === 'desc' ? 'asc' : 'desc';
      messages = await api('/sessions/' + id + '/messages?limit=' + MESSAGE_LIMIT + '&order=' + other + at);
    }
    // Another session may have been picked while this was in flight; its own fetch (or
    // the pending re-run below) owns the stream now, and painting this one would show
    // the wrong session under the right highlight
    if (state.selectedId !== record.sessionId) return;
    state.detail = { sessionId: record.sessionId, signature, messages, final, anchor };
    painted = true;
    // Learn the count for sources that do not report one on list, and refresh the row's
    // badge — the incremental patch only touches what actually changed.
    // It comes from final, not from messages.total: that one is capped at the page size
    // (200), so a 1209-message session would show "200". final.messageCount is the real
    // number — every source already scans for the last message, so it costs nothing.
    const total = Number(final && final.messageCount) || 0;
    if (total && !state.msgCounts.has(record.sessionId)) {
      state.msgCounts.set(record.sessionId, total);
      renderList();
    }
    renderStreamHead(record);
    renderMessages();
    renderSide(record);
    // A jump that needed this window lands now; an open find counts again over it
    if (state.reveal && anchor === state.focusAt) settleReveal();
    else refreshFind();
  } catch (err) {
    handleError(err);
    clearDetail('Failed to load: ' + err.message);
  } finally {
    state.busy = false;
    // A queued call is only worth re-running when this fetch did not already paint the
    // selection it belongs to — otherwise a double r fires the same request pair twice
    // and rebuilds the stream (and any text selection) for no new data
    // A window anchored somewhere else since this fetch left counts as unpainted: a jump
    // to a failure or a match moves the anchor and asks again, and the answer to the
    // earlier question is not the window it wants.
    if (state.pendingDetail) {
      state.pendingDetail = false;
      if (!painted || (state.detail && state.detail.anchor !== state.focusAt)) syncDetail({ force: true });
    }
  }
}

// ---------------------------------------------------------------------------
// Refreshing and errors
// ---------------------------------------------------------------------------

function handleError(err) {
  if (err && err.status === 401) {
    state.authRequired = true;
    showGate('That token was rejected — paste it again');
    if (state.timer) clearInterval(state.timer);
    state.timer = null;
  }
}

// serverChanged reloads the page when the server answering it is no longer the version
// that served it. An installed web app on a phone is never reloaded by hand, so without
// this the previous version's page would run against the new API until the phone
// discarded it — and a layout fix would never arrive.
function serverChanged(version) {
  if (!version) return false;
  if (!state.serverVersion) {
    state.serverVersion = version;
    return false;
  }
  if (version === state.serverVersion) return false;
  location.reload();
  return true;
}

function scheduleCountWarmup() {
  const missing = state.sessions.some((session) =>
    ['pi', 'claude', 'codex', 'gemini', 'grok'].includes(session.source) &&
    !Number(session.messageCount) && !state.msgCounts.has(session.sessionId));
  if (!missing || state.countWarmAttempts >= 12) {
    if (!missing) state.countWarmAttempts = 0;
    return;
  }
  if (state.countWarmTimer) return;
  state.countWarmAttempts++;
  state.countWarmTimer = setTimeout(() => {
    state.countWarmTimer = null;
    refresh();
  }, 700);
}

// refresh refetches the list, then updates the stream. force marks an explicit refresh
// (r, the button): the stream is refetched even when the reader has paged back — a
// background refresh must not replace a paged window under them, but a pressed key is a
// deliberate "update this now".
async function refresh(options) {
  const force = !!(options && options.force);
  try {
    const data = await api('/sessions');
    if (serverChanged(data.version)) return;
    state.sessions = data.sessions || [];
    state.byId = new Map(state.sessions.map((s) => [s.sessionId, s]));
    state.loadedAt = new Date().toLocaleTimeString();
    renderSources();
    renderList();
    scheduleCountWarmup();
    await syncDetail(force ? { force: true } : {});
    setStatus(idleStatus());
  } catch (err) {
    handleError(err);
    setStatus('Refresh failed: ' + err.message);
  }
}

function startAutoRefresh() {
  if (state.timer) clearInterval(state.timer);
  state.timer = setInterval(() => {
    if (document.hidden) return; // do not call the API while the page is in the background
    refresh();
  }, REFRESH_MS);
}

// ---------------------------------------------------------------------------
// Keyboard
// ---------------------------------------------------------------------------

function isTyping(node) {
  if (!node) return false;
  const tag = node.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || node.isContentEditable;
}

function moveSelection(delta) {
  // Both halves are navigable: metadata matches first, then the content hits
  const sessions = listRows()
    .filter((r) => r.kind === 'session' || r.kind === 'hit')
    .map((r) => r.session);
  if (sessions.length === 0) return;
  const index = sessions.findIndex((s) => s.sessionId === state.selectedId);
  const next = index < 0
    ? (delta > 0 ? 0 : sessions.length - 1)
    : Math.min(sessions.length - 1, Math.max(0, index + delta));
  selectSession(sessions[next].sessionId);
  const active = $('list').querySelector('.item.active');
  if (active) active.scrollIntoView({ block: 'nearest' });
}

function scrollMessages(toBottom) {
  const pane = $('messages');
  scrollPaneTo(pane, toBottom ? SCROLL_END : 0);
}

function clearSearch() {
  if (!state.keyword && !state.content) return;
  state.keyword = '';
  $('search').value = '';
  cancelContentSearch();
  state.content = null;
  renderList();
  setStatus(idleStatus());
}

document.addEventListener('keydown', (event) => {
  if (event.metaKey || event.ctrlKey || event.altKey) return;
  if (isTyping(event.target)) {
    if (event.key === 'Escape') event.target.blur();
    return;
  }
  switch (event.key) {
    case '/':
      event.preventDefault();
      $('search').focus();
      break;
    case 'j': case 'ArrowDown':
      event.preventDefault();
      moveSelection(1);
      break;
    case 'k': case 'ArrowUp':
      event.preventDefault();
      moveSelection(-1);
      break;
    case 'g':
      scrollMessages(false);
      break;
    case 'f':
      if (state.detail) {
        event.preventDefault(); // or the f lands in the field it just focused
        openFind();
      }
      break;
    case ']':
      gotoFailure(1);
      break;
    case '[':
      gotoFailure(-1);
      break;
    case 'G':
      scrollMessages(true);
      break;
    case 'r':
      refresh({ force: true });
      break;
    case 'Escape':
      clearSearch();
      break;
    default:
      break;
  }
});

// ---------------------------------------------------------------------------
// Events and startup
// ---------------------------------------------------------------------------

$('gate-form').addEventListener('submit', (event) => {
  event.preventDefault();
  const token = $('gate-token').value.trim();
  if (!token) return;
  state.token = token;
  localStorage.setItem(TOKEN_KEY, token);
  showApp();
  start();
});

$('logout').addEventListener('click', () => {
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(VIEW_KEY);
  state.token = '';
  state.sessions = [];
  state.byId = new Map();
  state.selectedId = '';
  state.detail = null;
  if (state.timer) clearInterval(state.timer);
  state.timer = null;
  showGate('The stored token has been cleared');
});

// Debounce the search: re-laying out the whole list on every keystroke is unnecessary
$('search').addEventListener('input', (event) => {
  const value = event.target.value.trim();
  if (state.searchTimer) clearTimeout(state.searchTimer);
  state.searchTimer = setTimeout(() => {
    state.keyword = value;
    renderList();                 // metadata: local, immediate
    scheduleContentSearch(value); // message bodies: server-side, a moment later
  }, SEARCH_DEBOUNCE_MS);
});

// The content search runs on its own; Enter only skips the wait for it
$('search').addEventListener('keydown', (event) => {
  if (event.key === 'Enter') {
    event.preventDefault();
    const value = event.target.value.trim();
    if (state.searchTimer) clearTimeout(state.searchTimer);
    state.keyword = value;
    renderList();
    runContentSearch(value);
  } else if (event.key === 'Escape') {
    event.preventDefault();
    clearSearch();
  }
});

// Find in the session: typing counts the matches, Enter walks them, Shift+Enter walks back
$('find').addEventListener('input', () => {
  if (state.findTimer) clearTimeout(state.findTimer);
  state.findTimer = setTimeout(() => {
    state.findTimer = null;
    refreshFind();
  }, SEARCH_DEBOUNCE_MS);
});
$('find').addEventListener('keydown', (event) => {
  if (event.key === 'Enter') {
    event.preventDefault();
    if (state.findTimer) {
      clearTimeout(state.findTimer);
      state.findTimer = null;
    }
    findStep(event.shiftKey ? -1 : 1);
  } else if (event.key === 'Escape') {
    event.preventDefault();
    event.stopPropagation(); // Esc here closes the find bar, not the list's search
    closeFind();
    $('messages').focus({ preventScroll: true });
  }
});
$('find-prev').addEventListener('click', () => findStep(-1));
$('find-next').addEventListener('click', () => findStep(1));
$('find-close').addEventListener('click', closeFind);

// List grouping: by time / by project
$('grouping').addEventListener('change', (event) => {
  state.grouping = event.target.value;
  saveViewPrefs();
  renderList();
  syncFoldAllButton(); // the fold-all button only means something grouped by project
});

$('source-filter').addEventListener('change', (event) => {
  state.source = event.target.value;
  saveViewPrefs();
  renderList();
  // Body hits were searched for within one source, so another source is another search
  if (state.content) runContentSearch(state.keyword);
});

// Reading on: fetch the next page when the reader reaches the edge that has one. On a
// phone the page itself is the scroller, so the window's scroll says the same thing.
function onMessagesScroll() {
  const pane = $('messages');
  if (state.loadingMore || !paneShown(pane)) return;
  const m = paneMetrics(pane);
  const nearTop = m.top < SCROLL_LOAD_PX;
  const nearBottom = m.height - m.top - m.visible < SCROLL_LOAD_PX;
  const edge = state.order === 'desc' ? 'older' : 'newer';
  const nearEdge = edge === 'older' ? nearTop : nearBottom;
  // A mobile prepend used to restore the old pixel position, which made an upward
  // swipe appear to bounce back. The new page is left at the edge instead; do not
  // immediately fetch every older page in a loop while the edge is still there.
  if (state.edgeLock === edge) {
    const movedAway = edge === 'older'
      ? m.top > SCROLL_LOAD_PX + 80
      : m.height - m.top - m.visible > SCROLL_LOAD_PX + 80;
    if (movedAway) state.edgeLock = '';
    else return;
  }
  if (nearEdge) loadMore(edge);
}
$('messages').addEventListener('scroll', onMessagesScroll, { passive: true });
window.addEventListener('scroll', () => { if (isPhone()) onMessagesScroll(); }, { passive: true });

// The page keeps each screen's place itself (showPane); the browser restoring a remembered
// position of its own on the back gesture would fight that
if ('scrollRestoration' in history) history.scrollRestoration = 'manual';

$('fold-groups').addEventListener('click', foldAllGroups);

$('toggle-list').addEventListener('click', () => {
  state.hideList = !state.hideList;
  applyPaneFolds();
  saveViewPrefs();
});
$('toggle-side').addEventListener('click', () => {
  state.hideSide = !state.hideSide;
  applyPaneFolds();
  saveViewPrefs();
});

$('refresh').addEventListener('click', () => refresh({ force: true }));
$('theme').addEventListener('click', toggleTheme);
// Following the system, a change of system theme changes the page; only the button's
// words and the chrome colour need telling
systemLight.addEventListener('change', () => {
  if (!document.documentElement.dataset.theme) applyTheme('');
});

$('auto').addEventListener('change', (event) => {
  saveViewPrefs();
  if (event.target.checked) {
    startAutoRefresh();
  } else if (state.timer) {
    clearInterval(state.timer);
    state.timer = null;
  }
});

// Back and forward. On a phone every screen but the list was reached through a history
// entry naming it, so the entry landed on says which screen to show; landing on one with
// no pane is the list.
window.addEventListener('popstate', (event) => {
  if (!isPhone()) return;
  const pane = event.state && event.state.pane;
  showPane(pane === 'side' || pane === 'stream' ? pane : 'list');
});

// A hash names a session. On a phone with the list up — the reader just stepped back
// out — the selection follows it quietly, so the row stays highlighted and the
// conversation behind it matches the address without the screen changing.
window.addEventListener('hashchange', () => {
  const id = decodeURIComponent(location.hash.replace(/^#/, ''));
  if (!id || id === state.selectedId || !state.byId.has(id)) return;
  selectSession(id, '', isPhone() && state.pane !== 'stream');
});

$('side-back').addEventListener('click', backFromSide);

// saveViewPrefs records the view controls. Called from each control's own handler, so
// there is no single "settings changed" funnel to forget.
function saveViewPrefs() {
  try {
    localStorage.setItem(VIEW_KEY, JSON.stringify({
      grouping: state.grouping,
      source: state.source,
      order: state.order,
      view: state.view,
      expandAll: state.expandAll,
      auto: $('auto').checked,
      exportFormat: state.exportFormat,
      hideList: state.hideList,
      hideSide: state.hideSide,
      collapsed: [...state.collapsedGroups],
    }));
  } catch (e) {
    // private mode, or storage full: the page works, the preference just does not stick
  }
}

function applyViewPrefs() {
  let prefs;
  try {
    prefs = JSON.parse(localStorage.getItem(VIEW_KEY) || '{}');
  } catch (e) {
    return; // a corrupt entry is not worth failing the page over
  }
  if (prefs.grouping === 'project' || prefs.grouping === 'time') {
    state.grouping = prefs.grouping;
    $('grouping').value = prefs.grouping;
  }
  if (typeof prefs.source === 'string') state.source = prefs.source;
  if (prefs.order === 'asc' || prefs.order === 'desc') state.order = prefs.order;
  if (prefs.view === 'all' || prefs.view === 'conversation' || stepView(prefs.view)) state.view = prefs.view;
  // The earlier filters were by speaker; a saved one maps onto the nearest view
  else if (prefs.role === 'user' || prefs.role === 'assistant') state.view = 'conversation';
  if (typeof prefs.expandAll === 'boolean') state.expandAll = prefs.expandAll;
  if (typeof prefs.auto === 'boolean') $('auto').checked = prefs.auto;
  if (EXPORT_FORMATS.some(([v]) => v === prefs.exportFormat)) state.exportFormat = prefs.exportFormat;
  if (typeof prefs.hideList === 'boolean') state.hideList = prefs.hideList;
  if (typeof prefs.hideSide === 'boolean') state.hideSide = prefs.hideSide;
  if (Array.isArray(prefs.collapsed)) state.collapsedGroups = new Set(prefs.collapsed);
}

// applyPaneFolds folds the two side panes away on a wide screen, leaving the conversation
// the full width. The toggles say which way they will go next.
function applyPaneFolds() {
  document.body.classList.toggle('hide-list', state.hideList);
  document.body.classList.toggle('hide-side', state.hideSide);
  // The chevron is markup, turned by the stylesheet on the body class; here only the
  // words change
  const list = $('toggle-list'), side = $('toggle-side');
  if (list) {
    list.title = (state.hideList ? 'Show' : 'Hide') + ' the session list';
    list.setAttribute('aria-expanded', state.hideList ? 'false' : 'true');
  }
  if (side) {
    side.title = (state.hideSide ? 'Show' : 'Hide') + ' the details pane';
    side.setAttribute('aria-expanded', state.hideSide ? 'false' : 'true');
  }
}

// scrollTo sets a pane's scroll position. Named so the places the page moves the reader
// on purpose — opening at an end, holding their place through a prepend, a jump from the
// table of contents — read as such.
// ---------------------------------------------------------------------------
// Scrolling. On a wide screen each pane scrolls on its own. On a phone the page is one
// document that scrolls as a whole, the way every page on a phone does, and each screen's
// head sticks to the top of it: nothing is sized to the viewport and nothing holds the
// document still. (An app exactly one viewport tall, with the document held still beneath
// it, was three releases of fighting iOS over where the document rests — an installed web
// app with an opaque status bar keeps the bar's height as an inset above the page, a
// finger could drag the page up into it, and code that put the page back oscillated with
// the system. A page that is meant to scroll has nothing to fight about.) Everything that
// reads or sets a pane's scroll position goes through these.
// ---------------------------------------------------------------------------

const PANE_SCREEN = { list: 'list', messages: 'stream', side: 'side' };
// phones: each screen's place in the page, kept while another screen is up
const paneScroll = { list: 0, stream: 0, side: 0 };
// a y past any height: the bottom, whatever the height is by the time the screen is shown
const SCROLL_END = 1e9;

// scrollerOf: the element whose scrollTop moves the pane's content
function scrollerOf(pane) {
  return isPhone() ? (document.scrollingElement || document.documentElement) : pane;
}

// paneShown: on a phone, whether the pane's screen is the one up. A screen that is not up
// has no layout, so its position is kept in paneScroll until it is.
function paneShown(pane) {
  return !isPhone() || state.pane === PANE_SCREEN[pane.id];
}

// paneMetrics: where the pane's content stands — the scroll position, the full height and
// the visible height — in whichever scroller moves it
function paneMetrics(pane) {
  if (!isPhone()) return { top: pane.scrollTop, height: pane.scrollHeight, visible: pane.clientHeight };
  const scroller = scrollerOf(pane);
  return { top: scroller.scrollTop, height: scroller.scrollHeight, visible: scroller.clientHeight };
}

function scrollPaneTo(pane, y) {
  if (!isPhone()) {
    pane.scrollTop = y;
    return;
  }
  const screen = PANE_SCREEN[pane.id];
  if (state.pane !== screen) {
    paneScroll[screen] = y;
    return;
  }
  scrollerOf(pane).scrollTop = y;
}

// showPane switches the phone screen. On a wide screen the attribute is inert: the CSS
// only consults it below the phone breakpoint. The screens have an order — list,
// conversation, details — and the direction of the change picks which way the incoming
// screen slides.
const PANE_ORDER = ['list', 'stream', 'side'];
function showPane(name) {
  const from = PANE_ORDER.indexOf(state.pane);
  const to = PANE_ORDER.indexOf(name);
  const phone = isPhone();
  // Each screen keeps its place in the page while another is up
  if (phone && state.pane !== name) paneScroll[state.pane] = window.scrollY;
  document.body.classList.toggle('nav-back', to < from);
  state.pane = name;
  document.body.dataset.pane = name;
  if (phone) scrollerOf($('messages')).scrollTop = paneScroll[name] || 0;
}

async function start() {
  applyViewPrefs();
  showPane(state.pane);
  applyPaneFolds();
  syncFoldAllButton();
  const hashId = decodeURIComponent(location.hash.replace(/^#/, ''));
  if (hashId) {
    state.selectedId = hashId;
    // A deep link opens on the conversation it names, on a phone too — with the list
    // behind it, so the back gesture has somewhere to go
    if (isPhone()) {
      history.replaceState({ pane: 'list' }, '', location.pathname);
      history.pushState({ pane: 'stream' }, '', '#' + encodeURIComponent(hashId));
      showPane('stream');
    }
  }
  await refresh();
  if (!state.selectedId && state.sessions.length > 0) {
    // Select the newest by default, so opening the page shows something — quietly: no
    // history entry, and a phone stays on the pane it was on
    selectSession(state.sessions[0].sessionId, '', true);
  }
  if ($('auto').checked) startAutoRefresh();
}

(async function boot() {
  // theme.js has already pinned a stored theme; this fills in the button's words
  applyTheme(document.documentElement.dataset.theme || '');
  // With no token configured there is no reason to make anyone invent one; /health says
  // outright whether authentication is required
  try {
    const res = await fetch('/health');
    const health = await res.json();
    state.authRequired = health.authRequired !== false;
    state.serverVersion = health.version || '';
  } catch (e) {
    state.authRequired = true; // if we cannot ask, assume authentication is required
  }

  const saved = localStorage.getItem(TOKEN_KEY) || '';
  if (state.authRequired && !saved) {
    showGate('');
    return;
  }
  state.token = saved;
  showApp();
  await start();
})();
