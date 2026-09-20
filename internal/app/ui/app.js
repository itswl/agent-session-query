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
// session really changed, and rebuilds preserve expanded blocks and scroll position.
'use strict';

const TOKEN_KEY = 'agent-session-query-token';
// How you like to look at the list, as opposed to what you are looking at (that lives in
// the URL hash). Losing the grouping on every reload was the complaint that added this.
const VIEW_KEY = 'agent-session-query-view';
const MESSAGE_LIMIT = 200;
const REFRESH_MS = 10000;
const SEARCH_DEBOUNCE_MS = 150;
// Content search keeps its own, slower beat: it scans every session file on the server,
// so it waits for a real pause in typing and ignores queries too short to narrow anything.
const CONTENT_DEBOUNCE_MS = 300;
const CONTENT_MIN_CHARS = 2;
// Blocks fold past a threshold so one tool dump does not drown the conversation. The
// threshold depends on what the block is: the messages themselves are what you read
// (a high bar), while tool output and thinking are supporting material you consult
// (a low bar). Both shared 600 before, which left whole screens of shell output open.
const FOLD_AT = { text: 600, thinking: 300, toolResult: 200, toolCall: 400 };
const STICK_TO_BOTTOM_PX = 48;
// How close to an edge the reader has to get before the next page is fetched
const SCROLL_LOAD_PX = 320;

const state = {
  token: '',
  authRequired: true,
  sessions: [],
  byId: new Map(),
  selectedId: '',
  keyword: '',
  source: '',
  role: '',            // '' / 'user' / 'assistant'
  order: 'desc',       // desc = the latest N (the end of a session is the interesting part)
  grouping: 'time',    // time = by update time; project = grouped by project (cwd)
  pane: 'stream',      // phones only: which of the three panes is on screen
  hideList: false,     // wide screens: fold the session list away
  hideSide: false,     // wide screens: fold the details pane away
  content: null,       // content search for the current keyword:
                       //   { query, searching, results, matched, truncated, tookMs }
  contentTimer: null,
  contentAbort: null,  // AbortController for the search still in flight
  detail: null,        // { sessionId, signature, messages, final }
  focusAt: '',         // when set, the stream window is anchored at this time (a search hit)
  exportFormat: 'md',  // md / jsonl / json / html
  loadingMore: false,  // a page is in flight
  noMore: { older: false, newer: false }, // an edge that came back empty
  collapsedGroups: new Set(), // project names folded away in By project grouping
  msgCounts: new Map(),       // sessionId → message count, learned as sessions are opened
  openBlocks: new Set(),
  timer: null,
  searchTimer: null,
  busy: false,
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
  if (found && !found.searching && found.query === state.keyword) {
    return '\u201c' + found.query + '\u201d · ' + found.matched + ' in message bodies · ' +
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

// segmented renders a group of mutually exclusive buttons (which slice / whose messages)
function segmented(options, current, onPick) {
  const wrap = el('div', 'seg');
  for (const [value, label] of options) {
    wrap.appendChild(button('seg-btn' + (value === current ? ' on' : ''), label, () => onPick(value)));
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
const SOURCE_CLASSES = ['claude', 'codex', 'gemini', 'hermes', 'openclaw', 'pi'];
function sourceClass(source) {
  return 'tag' + (SOURCE_CLASSES.includes(source) ? ' ' + source : '');
}
function sourceTag(source) {
  return el('span', sourceClass(source), source);
}
function statusTag(status) {
  const cls = status === 'done' ? ' done' : status === 'running' ? ' running' : '';
  return el('span', 'tag' + cls, status);
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

function showApp() {
  $('gate').classList.add('hidden');
  $('app').classList.remove('hidden');
  // With no token configured there is nothing to change
  $('logout').classList.toggle('hidden', !state.authRequired);
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
  if (!found || found.query !== state.keyword) return [];

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
  row.appendChild(el('span', 'live'));  // live dot (only shown while a session is being written)
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
  live.classList.toggle('on', !!session.isActive);
  live.title = session.isActive ? 'being written' : '';
  // The SQLite sources report a true count; the file sources do not count on list (the
  // list never reads file bodies), so their number is learned when the session is opened
  const n = Number(session.messageCount) || state.msgCounts.get(session.sessionId) || 0;
  setText(msgs, n ? n + (n === 1 ? ' msg' : ' msgs') : '');
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
  // the rest are created
  const existing = new Map();
  for (const node of Array.from(list.children)) {
    if (node.dataset && node.dataset.id) existing.set(node.dataset.id, node);
    else node.remove(); // a leftover empty-state message
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
  state.content = { query: text, searching: true, results: [] };
  renderList();

  try {
    const data = await api('/search?q=' + encodeURIComponent(text) + '&limit=50',
      { signal: controller.signal });
    // A newer keystroke may have landed while this was in flight
    if (controller.signal.aborted || state.keyword !== text) return;
    state.content = Object.assign({ query: text, searching: false }, data);
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
  // matched counts every session with a hit, including the ones already listed above
  setText(note, found.truncated ? '(of ' + found.matched + ', showing the first 50)' : '');
}

function buildHit(rowId, sessionId) {
  const item = el('div', 'item hit');
  item.dataset.id = rowId;
  item.dataset.sid = sessionId;
  item.setAttribute('role', 'option');
  item.tabIndex = -1;
  item.addEventListener('click', () => {
    const first = (item._hit && item._hit.matches && item._hit.matches[0]) || {};
    selectSession(item.dataset.sid, first.timestamp || '');
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
  live.title = hit.isActive ? 'being written' : '';
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
  state.source = select.value;
}

// ---------------------------------------------------------------------------
// Middle pane: session identity + toolbar + message stream
// ---------------------------------------------------------------------------

function renderStreamHead(record) {
  const head = $('stream-head');
  if (!record) {
    head.replaceChildren();
    return;
  }

  const box = el('div');
  const tagrow = el('div', 'tagrow');
  tagrow.appendChild(sourceTag(record.source));
  if (record.status) tagrow.appendChild(statusTag(record.status));
  if (record.updatedAt) {
    const time = el('span', 'time', relTime(record.updatedAt));
    time.title = record.updatedAt;
    tagrow.appendChild(time);
  }
  box.appendChild(tagrow);

  const title = el('h2', '', record.shortKey || record.sessionId);
  title.title = record.key || '';
  box.appendChild(title);
  if (record.cwd) box.appendChild(el('p', 'sub', record.cwd));

  const bar = el('div', 'toolbar');
  bar.appendChild(segmented(
    [['asc', '\u2191 earliest'], ['desc', '\u2193 latest']],
    state.order,
    (value) => {
      if (value === state.order) return;
      state.order = value;
      state.focusAt = ''; // paging to an end is a deliberate move away from the anchor
      saveViewPrefs();
      syncDetail({ force: true });
    },
  ));
  bar.appendChild(segmented(
    [['', 'all'], ['user', 'user'], ['assistant', 'assistant'], ['tools', 'tools']],
    state.role,
    (value) => {
      if (value === state.role) return;
      state.role = value;
      saveViewPrefs();
      renderMessages();
      renderStreamHead(record); // only to move the highlight onto the other button
    },
  ));

  const detail = state.detail;
  if (detail) {
    const shown = (detail.messages.messages || []).length;
    const total = detail.final && typeof detail.final.messageCount === 'number'
      ? detail.final.messageCount : shown;
    // An anchored window is neither the start nor the end of the session, and saying
    // "latest 200" there would be a lie — so it says what it is, with a way out
    let label;
    if (state.loadingMore) {
      label = 'Loading more\u2026';
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
    bar.appendChild(el('span', 'count', label));
  }
  box.appendChild(bar);

  head.replaceChildren(box);
}

// Tool categories, and what each one is for: reading, changing, running, searching,
// reaching the network, delegating. Colouring by category rather than by tool name keeps
// the palette small and meaningful — a transcript is scanned for "where did it run
// something" far more often than for "where did it run grep specifically".
// Order matters: the first match wins.
const TOOL_KINDS = [
  ['exec', /^(bash|shell|terminal|exec|sh$|run|command|process|container|docker)/i],
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

function blockNode(block) {
  switch (block.type || 'unknown') {
    case 'text':
      return el('div', 'block text', block.content);
    case 'thinking':
      return el('div', 'block thinking', block.content);
    case 'toolCall': {
      // Tool name as the heading, arguments indented below, so one glance says what ran
      const kind = toolKind(block.name);
      const wrap = el('div', 'block toolCall tool-' + kind);
      wrap.appendChild(el('div', 'tool-name', '⚙ ' + (block.name || '(unnamed tool)')));
      wrap.appendChild(el('pre', '', JSON.stringify(block.arguments || {}, null, 2)));
      return wrap;
    }
    case 'toolResult': {
      const kind = toolKind(block.toolName);
      const wrap = el('div', 'block toolResult tool-' + kind);
      wrap.appendChild(el('div', 'tool-label', '↳ ' + (block.toolName || 'result')));
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

// roleLabel names the speaker as the reader sees it, not as the file stores it
function roleLabel(message) {
  if (message.role === 'user' && !hasWords(message)) return 'tool';
  return message.role || '?';
}

// isToolMessage: anything that carried a tool call or its result. The tools filter works
// on this rather than on role, because a tool call rides inside an assistant message
// (the call and the prose share one turn) while a result is its own message with role
// tool, or a user-role row for Claude. Role alone would miss half of it.
function isToolMessage(message) {
  return (message.content || []).some(
    (b) => b.type === 'toolCall' || b.type === 'toolResult');
}

// matchesRole is the one place the stream filter is decided, shared by the renderer and
// the table of contents.
//
// The three filters are meant to be distinct categories, so user and assistant mean the
// human's words and the model's words — not "messages whose role field says so". A tool
// result carries role user in Claude's format and a tool call rides inside an assistant
// message, so filtering on role alone put tool traffic under both speakers at once.
// Anything tool-shaped now belongs to tools, and to tools only.
function matchesRole(message) {
  if (!state.role) return true;
  if (state.role === 'tools') return isToolMessage(message);
  // A turn that both speaks and calls a tool is genuinely both: it stays with its
  // speaker (its words are why you are reading it) and also appears under tools
  if (!hasWords(message)) return false;
  return message.role === state.role;
}

// emptyStreamNote names what the current filter left empty
function emptyStreamNote() {
  if (!state.role) return 'This session has no messages';
  if (state.role === 'tools') return 'No tool activity in this window';
  return 'No ' + state.role + ' messages';
}

function messageNode(message, index) {
  // A block with no content renders as an empty box (the dashed thinking box especially
  // stands out), so drop it. These blocks genuinely occur: a Claude thinking block may
  // carry only a signature with an empty body.
  const blocks = (Array.isArray(message.content) ? message.content : [])
    .map((block, position) => ({ block, position, node: blockNode(block) }))
    .filter((item) => (item.node.textContent || '').trim() !== '');

  const node = el('div', 'msg ' + (message.role || '') + (blocks.length === 0 ? ' is-empty' : ''));
  node.dataset.index = index;
  // A user-role row with no words of its own is a tool result (Claude's shape), not a
  // question: label and colour it as tooling so the two speakers stay distinguishable
  if (message.role === 'user' && !hasWords(message)) node.classList.add('is-tool');
  const head = el('div', 'head');
  head.appendChild(el('span', 'role', roleLabel(message)));
  // A message with nothing displayable is worth one line of explanation, not a whole block
  if (blocks.length === 0) head.appendChild(el('span', 'empty-hint', 'nothing to display'));
  if (message.id) head.title = 'id: ' + message.id;
  if (message.timestamp) head.appendChild(el('span', 'time', String(message.timestamp)));
  node.appendChild(head);

  blocks.forEach(({ position, node: rendered, block }) => {
    const plain = rendered.textContent || '';
    const limit = FOLD_AT[block.type] || 600;
    if (plain.length <= limit) {
      node.appendChild(rendered);
      return;
    }
    // The key has to be stable: after a refresh rebuild we still need to recognise
    // which ones the user had expanded
    const key = (message.id || 'i' + index) + ':' + position;
    // A one-line preview rides along with the summary, so a folded block still says
    // what it is instead of only how big it is
    const preview = plain.replace(/\s+/g, ' ').trim().slice(0, 80);
    const details = el('details');
    details.open = state.openBlocks.has(key);
    details.addEventListener('toggle', () => {
      if (details.open) state.openBlocks.add(key);
      else state.openBlocks.delete(key);
    });
    const summary = el('summary');
    summary.appendChild(el('span', 'fold-kind', block.type));
    summary.appendChild(el('span', 'fold-preview', preview));
    summary.appendChild(el('span', 'fold-size', plain.length + ' chars'));
    details.appendChild(summary);
    details.appendChild(rendered);
    node.appendChild(details);
  });
  return node;
}

function renderMessages() {
  const pane = $('messages');
  const detail = state.detail;
  if (!detail) return;

  // The same session showing the same slice counts as the same view, and a rebuild has to
  // land back where it was
  const view = detail.sessionId + '|' + state.order;
  const sameView = pane.dataset.view === view;
  const prevScroll = pane.scrollTop;
  const wasAtBottom = sameView &&
    pane.scrollHeight - pane.scrollTop - pane.clientHeight < STICK_TO_BOTTOM_PX;

  // The index is the position in the full list, not in the filtered one, so the
  // conversation TOC can address a message regardless of the current role filter
  const all = detail.messages.messages || [];
  const shown = [];
  all.forEach((message, index) => {
    if (matchesRole(message)) shown.push({ message, index });
  });

  const box = document.createDocumentFragment();
  if (shown.length === 0) {
    box.appendChild(el('p', 'empty', emptyStreamNote()));
  }
  shown.forEach(({ message, index }) => box.appendChild(messageNode(message, index)));
  pane.replaceChildren(box);
  pane.dataset.view = view;

  if (!sameView) {
    // Just opened: stick to the bottom when viewing the latest, start at the top for the earliest
    scrollTo(pane, state.order === 'desc' ? pane.scrollHeight : 0);
  } else if (wasAtBottom) {
    scrollTo(pane, pane.scrollHeight);
  } else {
    scrollTo(pane, prevScroll);
  }
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
  if (final.text) card.appendChild(el('div', 'text', final.text));
  if (!final.text && !final.error) card.appendChild(el('div', 'text dim', '(no text result)'));

  if (final.thinking) {
    const details = el('details');
    details.appendChild(el('summary', '', 'Thinking'));
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
    ['CLI', record.cliVersion],
    ['Token', record.totalTokens],
    ['Cost $', record.estimatedCostUsd],
    ['File', record.hasFile ? 'present' : 'missing (may exist only in state.db)'],
  ]));
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
  const box = document.createDocumentFragment();
  box.appendChild(finalCard(detail.final || {}));
  const usage = usageCard((detail.final && detail.final.usage) || {});
  if (usage) box.appendChild(usage);
  box.appendChild(sessionCard(record, detail.final));
  const timeline = projectTimeline(record);
  if (timeline) box.appendChild(timeline);
  box.appendChild(tocCard());
  side.replaceChildren(box);
}

// ---------------------------------------------------------------------------
// Selection and detail synchronisation
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Conversation table of contents
// ---------------------------------------------------------------------------

// tocCard lists the user's messages in the loaded window — the questions, which are what
// you navigate a long session by. Clicking one jumps to it in the stream.
function tocCard() {
  const detail = state.detail;
  const card = el('section', 'card toc-card');
  const all = (detail && detail.messages.messages) || [];
  const entries = [];
  all.forEach((message, index) => {
    // Only turns that actually say something. In Claude's format a tool result comes
    // back as a user-role message, and those are not questions to navigate by.
    if (message.role === 'user' && tocPreviewOf(message) !== '') {
      entries.push({ message, index });
    }
  });

  const head = el('h3', '', 'Conversation');
  if (entries.length) head.appendChild(el('span', 'count', entries.length));
  card.appendChild(head);

  // The list covers the loaded window, which is the latest page by default; say so when
  // the session is longer, or the missing early turns look like a bug
  const total = Number(detail && detail.final && detail.final.messageCount) || 0;
  if (total > all.length) {
    card.appendChild(el('p', 'dim toc-note',
      'From the latest ' + all.length + ' of ' + total + ' messages'));
  }

  if (!entries.length) {
    card.appendChild(el('p', 'dim', 'No user messages in this window'));
    return card;
  }

  const list = el('ol', 'toc');
  entries.forEach((entry, at) => {
    const item = el('li');
    const jump = el('button', 'toc-item');
    jump.type = 'button';
    jump.appendChild(el('span', 'toc-no', at + 1));
    jump.appendChild(el('span', 'toc-text', tocPreviewOf(entry.message)));
    jump.title = tocPreviewOf(entry.message, 400);
    jump.addEventListener('click', () => gotoMessage(entry.index));
    item.appendChild(jump);
    list.appendChild(item);
  });
  card.appendChild(list);
  return card;
}

// tocPreviewOf is a message's own words, folded to one line; "" when it has none (a
// tool result, an attachment, a bare signature)
function tocPreviewOf(message, max) {
  const limit = max || 70;
  for (const block of message.content || []) {
    if (block.type === 'text' && block.content) {
      const one = String(block.content).replace(/\s+/g, ' ').trim();
      if (!one) continue;
      return one.length > limit ? one.slice(0, limit) + '\u2026' : one;
    }
  }
  return '';
}

// gotoMessage scrolls a message into view and flashes it. The role filter can hide the
// target, so a miss drops the filter and retries once rather than doing nothing.
function gotoMessage(index) {
  const pane = $('messages');
  const find = () => pane.querySelector('.msg[data-index="' + index + '"]');

  let node = find();
  if (!node && state.role) {
    state.role = '';  // the table of contents lists questions, whatever the filter is
    renderMessages();
    renderStreamHead(state.byId.get(state.selectedId));
    node = find();
  }
  if (!node) return;
  node.scrollIntoView({ block: 'center', behavior: 'smooth' });
  node.classList.add('flash');
  setTimeout(() => node.classList.remove('flash'), 1400);
}

// selectSession opens a session. focusAt, when given, anchors the message window at that
// instant instead of at the end — how a search hit becomes somewhere you can land.
function selectSession(sessionId, focusAt) {
  if (!sessionId) return;
  setChromeHidden(false);
  if (sessionId === state.selectedId && (focusAt || '') === state.focusAt) return;
  state.focusAt = focusAt || '';
  state.selectedId = sessionId;
  // On a phone the list and the conversation are different screens: picking a session
  // there means you want to read it
  if ($('panes') && getComputedStyle($('panes')).display !== 'none') showPane('stream');
  const encoded = encodeURIComponent(sessionId);
  if (location.hash.replace(/^#/, '') !== encoded) location.hash = encoded;
  renderList();
  syncDetail({ force: true });
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
  if (!at) {
    state.noMore[edge] = true; // no anchor, so no way to ask for the neighbouring page
    return;
  }

  state.loadingMore = true;
  const pane = $('messages');
  const beforeHeight = pane.scrollHeight;
  const beforeTop = pane.scrollTop;

  let fresh = null;
  try {
    const id = encodeURIComponent(detail.sessionId);
    const order = edge === 'older' ? 'desc' : 'asc';
    const data = await api('/sessions/' + id + '/messages?limit=' + MESSAGE_LIMIT +
      '&order=' + order + '&at=' + encodeURIComponent(at));
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
  const record = state.byId.get(state.selectedId);
  renderStreamHead(record);
  renderSide(record);
  if (edge === 'older') {
    // Prepending pushes everything down; hold the reader's place on the same message
    scrollTo(pane, beforeTop + (pane.scrollHeight - beforeHeight));
  }
}

async function syncDetail(options) {
  const force = options && options.force;
  const record = state.byId.get(state.selectedId);
  if (!record) {
    clearDetail('\u2190 Pick a session on the left');
    return;
  }

  const switched = !state.detail || state.detail.sessionId !== record.sessionId;

  // Someone who has paged back is reading history. A background refresh refetches the
  // latest page and would replace everything they loaded — on an active session, every
  // ten seconds, which made paging feel like it kept snapping to the newest message.
  // The left pane still updates; only the stream holds still. Refresh (r) still refetches.
  const paged = !switched && (state.detail.messages.messages || []).length > MESSAGE_LIMIT;
  if (!force && paged) return;

  const signature = [record.sessionId, record.updatedAt, record.status, state.order].join('|');
  if (!force && state.detail && state.detail.signature === signature) return;
  if (state.busy) return;

  if (switched) state.noMore = { older: false, newer: false };
  if (switched) {
    // Only clear when switching sessions; a plain refresh of the same session keeps the
    // old content on screen and avoids a flash
    state.openBlocks.clear();
    $('messages').replaceChildren(el('p', 'empty', 'Loading\u2026'));
    $('messages').dataset.view = '';
    $('side').replaceChildren();
  }
  renderStreamHead(record);

  state.busy = true;
  try {
    const id = encodeURIComponent(record.sessionId);
    // The anchor travels with the request, not with the session: it belongs to how this
    // session was opened and goes away the moment you page to an end yourself
    const at = state.focusAt ? '&at=' + encodeURIComponent(state.focusAt) : '';
    const [messages, final] = await Promise.all([
      api('/sessions/' + id + '/messages?limit=' + MESSAGE_LIMIT + '&order=' + state.order + at),
      api('/sessions/' + id + '/final'),
    ]);
    state.detail = { sessionId: record.sessionId, signature, messages, final };
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
  } catch (err) {
    handleError(err);
    clearDetail('Failed to load: ' + err.message);
  } finally {
    state.busy = false;
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

async function refresh() {
  try {
    const data = await api('/sessions');
    state.sessions = data.sessions || [];
    state.byId = new Map(state.sessions.map((s) => [s.sessionId, s]));
    state.loadedAt = new Date().toLocaleTimeString();
    renderSources();
    renderList();
    await syncDetail({});
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
  scrollTo(pane, toBottom ? pane.scrollHeight : 0);
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
    case 'G':
      scrollMessages(true);
      break;
    case 'r':
      refresh();
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
});

// Reading on: fetch the next page when the reader reaches the edge that has one
// Touch devices report the gesture directly, so they do not have to infer it from a
// scroll position that the chrome's own hiding keeps moving.
let touchAnchorY = null;
$('messages').addEventListener('touchstart', (event) => {
  touchAnchorY = event.touches.length ? event.touches[0].clientY : null;
}, { passive: true });
$('messages').addEventListener('touchmove', (event) => {
  if (touchAnchorY === null || !event.touches.length) return;
  const y = event.touches[0].clientY;
  const dy = touchAnchorY - y; // > 0: the finger moved up, content moves down
  if (Math.abs(dy) < CHROME_JITTER_PX) return;
  touchAnchorY = y;
  slideChromeByTouch(dy);
}, { passive: true });
$('messages').addEventListener('touchend', () => { touchAnchorY = null; }, { passive: true });

$('messages').addEventListener('scroll', () => {
  const pane = $('messages');
  slideChrome(pane.scrollTop);
  if (state.loadingMore) return;
  const nearTop = pane.scrollTop < SCROLL_LOAD_PX;
  const nearBottom = pane.scrollHeight - pane.scrollTop - pane.clientHeight < SCROLL_LOAD_PX;
  if (state.order === 'desc') {
    if (nearTop) loadMore('older');
  } else if (nearBottom) {
    loadMore('newer');
  }
}, { passive: true });

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

$('refresh').addEventListener('click', refresh);

if ($('panes')) {
  for (const button of $('panes').children) {
    button.addEventListener('click', () => showPane(button.dataset.pane));
  }
}

$('auto').addEventListener('change', (event) => {
  saveViewPrefs();
  if (event.target.checked) {
    startAutoRefresh();
  } else if (state.timer) {
    clearInterval(state.timer);
    state.timer = null;
  }
});

window.addEventListener('hashchange', () => {
  const id = decodeURIComponent(location.hash.replace(/^#/, ''));
  if (id && id !== state.selectedId && state.byId.has(id)) selectSession(id);
});

// saveViewPrefs records the view controls. Called from each control's own handler, so
// there is no single "settings changed" funnel to forget.
function saveViewPrefs() {
  try {
    localStorage.setItem(VIEW_KEY, JSON.stringify({
      grouping: state.grouping,
      source: state.source,
      order: state.order,
      role: state.role,
      auto: $('auto').checked,
      pane: state.pane,
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
  if (['', 'user', 'assistant', 'tools'].indexOf(prefs.role) >= 0) state.role = prefs.role;
  if (typeof prefs.auto === 'boolean') $('auto').checked = prefs.auto;
  // Names of projects folded away; a name that no longer exists simply never matches
  if (['list', 'stream', 'side'].indexOf(prefs.pane) >= 0) state.pane = prefs.pane;
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
  const list = $('toggle-list'), side = $('toggle-side');
  if (list) {
    list.textContent = state.hideList ? '\u203a' : '\u2039';
    list.title = (state.hideList ? 'Show' : 'Hide') + ' the session list';
  }
  if (side) {
    side.textContent = state.hideSide ? '\u2039' : '\u203a';
    side.title = (state.hideSide ? 'Show' : 'Hide') + ' the details pane';
  }
}

// On a phone the title row and the pane switcher together are a tenth of the screen, and
// while you are reading downwards neither is doing anything. Scrolling down slides them
// away, scrolling up brings them back, and the top of the conversation always shows them
// — the standard phone behaviour, and it costs nothing on a wide screen where the media
// query never matches.
const CHROME_HIDE_AFTER_PX = 40;
const CHROME_JITTER_PX = 8;
let chromeHidden = false;
let chromeLastY = 0;
let scrollQuietUntil = 0;

// The page scrolls itself — opening a session jumps to the newest message, prepending a
// page holds the reader's place, the table of contents scrolls to its target. Those are
// not the reader moving, and treating the jump to the bottom on open as "scrolled down"
// hid the header the moment a session appeared.
function scrollTo(pane, y) {
  scrollQuietUntil = performance.now() + 250;
  pane.scrollTop = y;
}

function slideChrome(y) {
  if (performance.now() < scrollQuietUntil) {
    chromeLastY = y;
    return;
  }
  // No decision is taken at either end of the scroll range. Hiding the chrome changes the
  // container's height and therefore the scroll geometry, so a bounce at an edge produces
  // scroll events that look like the reader moving — which is what made the block flip
  // back and forth a few times when a phone was pulled to the bottom. (An overscroll
  // bounce lives here, which is why it showed up there first.)
  const pane = $('messages');
  if (y <= 0 || y >= pane.scrollHeight - pane.clientHeight - 1) {
    if (y <= 0) {
      chromeLastY = y;
      setChromeHidden(false); // the top of the conversation always shows it
    }
    return;
  }
  // No width check: the stream head slides everywhere, and the header/pane-switcher rules
  // simply do not apply above the phone breakpoint.
  if (y < CHROME_HIDE_AFTER_PX) {
    setChromeHidden(false);
  } else if (y > chromeLastY + CHROME_JITTER_PX) {
    setChromeHidden(true); // moving down the conversation
  } else if (y < chromeLastY - CHROME_JITTER_PX) {
    setChromeHidden(false); // coming back up
  }
  chromeLastY = y;
}

// On a touch screen the finger decides, not the scroll position: a reader dragging
// upwards is moving down the conversation, and that stays true however the layout
// reflows underneath. The scroll path above is what remains for a mouse or a trackpad,
// where the wheel is the only signal there is.
function slideChromeByTouch(dy) {
  const pane = $('messages');
  if (performance.now() < scrollQuietUntil) return;
  if (pane.scrollTop <= 0) {
    setChromeHidden(false);
    return;
  }
  setChromeHidden(dy > 0);
}

function setChromeHidden(hidden) {
  if (hidden === chromeHidden) return;
  chromeHidden = hidden;
  document.body.classList.toggle('chrome-hidden', hidden);
  // The layout changed under the scroll position, so the delta the scroll handler sees
  // next is not the reader's; take the current position as the new baseline. The quiet
  // window covers the reflow that follows.
  chromeLastY = $('messages').scrollTop;
  scrollQuietUntil = performance.now() + 250;
}

// showPane switches the phone layout. On a wide screen the attribute is inert: the CSS
// only consults it below the phone breakpoint.
function showPane(name) {
  state.pane = name;
  document.body.dataset.pane = name;
  setChromeHidden(false); // a pane change is not a scroll: show the chrome again
  const nav = $('panes');
  if (nav) {
    for (const button of nav.children) {
      button.classList.toggle('on', button.dataset.pane === name);
    }
  }
  saveViewPrefs();
}

async function start() {
  applyViewPrefs();
  showPane(state.pane);
  applyPaneFolds();
  syncFoldAllButton();
  const hashId = decodeURIComponent(location.hash.replace(/^#/, ''));
  if (hashId) state.selectedId = hashId;
  await refresh();
  if (!state.selectedId && state.sessions.length > 0) {
    // Select the newest by default, so opening the page shows something
    selectSession(state.sessions[0].sessionId);
  }
  if ($('auto').checked) startAutoRefresh();
}

(async function boot() {
  // With no token configured there is no reason to make anyone invent one; /health says
  // outright whether authentication is required
  try {
    const res = await fetch('/health');
    const health = await res.json();
    state.authRequired = health.authRequired !== false;
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
