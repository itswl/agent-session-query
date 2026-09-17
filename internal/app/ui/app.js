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
const MESSAGE_LIMIT = 200;
const REFRESH_MS = 10000;
const SEARCH_DEBOUNCE_MS = 150;
// Content search keeps its own, slower beat: it scans every session file on the server,
// so it waits for a real pause in typing and ignores queries too short to narrow anything.
const CONTENT_DEBOUNCE_MS = 300;
const CONTENT_MIN_CHARS = 2;
const BIG_BLOCK_CHARS = 600;
const STICK_TO_BOTTOM_PX = 48;

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
  content: null,       // content search for the current keyword:
                       //   { query, searching, results, matched, truncated, tookMs }
  contentTimer: null,
  contentAbort: null,  // AbortController for the search still in flight
  detail: null,        // { sessionId, signature, messages, final }
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

function copyButton(text) {
  return button('ghost tiny', 'Copy', async (event) => {
    const node = event.currentTarget;
    try {
      await navigator.clipboard.writeText(text);
      setText(node, 'Copied');
      setTimeout(() => setText(node, 'Copy'), 1200);
    } catch (e) {
      setStatus('Copy failed — select the text manually');
    }
  });
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
      rows.push({
        kind: 'group', id: 'group:' + name, name,
        count: items.length, active: items.some((s) => s.isActive),
      });
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
  node.appendChild(el('span', 'group-name'));
  node.appendChild(el('span', 'group-count'));
  return node;
}

function fillGroup(node, row) {
  const [name, count] = node.children;
  // Show only the last segment of the project path; the full path goes in the title
  const short = String(row.name).split(/[\\/]/).filter(Boolean).pop() || row.name;
  setText(name, short);
  node.title = row.name;
  setText(count, row.count);
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
  row.appendChild(el('span', 'time'));  // relative time
  item.appendChild(row);
  item.appendChild(el('div', 'key'));
  item.appendChild(el('div', 'meta'));
  return item;
}

function fillItem(item, session) {
  const [tag, live, time] = item.children[0].children;
  const cls = sourceClass(session.source);
  if (tag.className !== cls) tag.className = cls;
  setText(tag, session.source);
  // File-backed sources always report status done, so "running" can only be inferred
  // from the update time (the server works this out)
  live.classList.toggle('on', !!session.isActive);
  live.title = session.isActive ? 'being written' : '';
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
  item.addEventListener('click', () => selectSession(item.dataset.sid));

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

  const current = select.value;
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
      syncDetail({ force: true });
    },
  ));
  bar.appendChild(segmented(
    [['', 'all'], ['user', 'user'], ['assistant', 'assistant']],
    state.role,
    (value) => {
      if (value === state.role) return;
      state.role = value;
      renderMessages();
      renderStreamHead(record); // only to move the highlight onto the other button
    },
  ));

  const detail = state.detail;
  if (detail) {
    const shown = (detail.messages.messages || []).length;
    const total = detail.final && typeof detail.final.messageCount === 'number'
      ? detail.final.messageCount : shown;
    const label = total > shown
      ? total + ' messages · showing the ' + (state.order === 'desc' ? 'latest ' : 'earliest ') + shown
      : shown + ' messages';
    bar.appendChild(el('span', 'count', label));
  }
  box.appendChild(bar);

  head.replaceChildren(box);
}

function blockNode(block) {
  switch (block.type || 'unknown') {
    case 'text':
      return el('div', 'block text', block.content);
    case 'thinking':
      return el('div', 'block thinking', block.content);
    case 'toolCall': {
      // Tool name as the heading, arguments indented below, so one glance says what ran
      const wrap = el('div', 'block toolCall');
      wrap.appendChild(el('div', 'tool-name', '⚙ ' + (block.name || '(unnamed tool)')));
      wrap.appendChild(el('pre', '', JSON.stringify(block.arguments || {}, null, 2)));
      return wrap;
    }
    case 'toolResult': {
      const wrap = el('div', 'block toolResult');
      wrap.appendChild(el('div', 'tool-label', '↳ ' + (block.toolName || 'result')));
      wrap.appendChild(el('pre', '', block.content));
      return wrap;
    }
    default:
      return el('div', 'block', JSON.stringify(block));
  }
}

function messageNode(message, index) {
  // A block with no content renders as an empty box (the dashed thinking box especially
  // stands out), so drop it. These blocks genuinely occur: a Claude thinking block may
  // carry only a signature with an empty body.
  const blocks = (Array.isArray(message.content) ? message.content : [])
    .map((block, position) => ({ block, position, node: blockNode(block) }))
    .filter((item) => (item.node.textContent || '').trim() !== '');

  const node = el('div', 'msg ' + (message.role || '') + (blocks.length === 0 ? ' is-empty' : ''));
  const head = el('div', 'head');
  head.appendChild(el('span', 'role', message.role || '?'));
  // A message with nothing displayable is worth one line of explanation, not a whole block
  if (blocks.length === 0) head.appendChild(el('span', 'empty-hint', 'nothing to display'));
  if (message.id) head.title = 'id: ' + message.id;
  if (message.timestamp) head.appendChild(el('span', 'time', String(message.timestamp)));
  node.appendChild(head);

  blocks.forEach(({ position, node: rendered }) => {
    const plain = rendered.textContent || '';
    if (plain.length <= BIG_BLOCK_CHARS) {
      node.appendChild(rendered);
      return;
    }
    // Fold very long blocks away so one tool output does not drown the page.
    // The key has to be stable: after a refresh rebuild we still need to recognise which
    // ones the user had expanded
    const key = (message.id || 'i' + index) + ':' + position;
    const details = el('details');
    details.open = state.openBlocks.has(key);
    details.addEventListener('toggle', () => {
      if (details.open) state.openBlocks.add(key);
      else state.openBlocks.delete(key);
    });
    details.appendChild(el('summary', '', 'Expand ' + plain.length + ' characters'));
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

  const messages = (detail.messages.messages || [])
    .filter((m) => !state.role || m.role === state.role);

  const box = document.createDocumentFragment();
  if (messages.length === 0) {
    box.appendChild(el('p', 'empty', state.role ? 'No ' + state.role + ' messages' : 'This session has no messages'));
  }
  messages.forEach((message, index) => box.appendChild(messageNode(message, index)));
  pane.replaceChildren(box);
  pane.dataset.view = view;

  if (!sameView) {
    // Just opened: stick to the bottom when viewing the latest, start at the top for the earliest
    pane.scrollTop = state.order === 'desc' ? pane.scrollHeight : 0;
  } else if (wasAtBottom) {
    pane.scrollTop = pane.scrollHeight;
  } else {
    pane.scrollTop = prevScroll;
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
  reasoningTokens: 'reasoning', estimatedCostUsd: 'cost $',
};
const USAGE_DECIMALS = { estimatedCostUsd: 4 };

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
      const text = digits !== undefined ? Number(v).toFixed(digits) : String(v);
      return [USAGE_LABELS[k] || k, text];
    });
  if (pairs.length === 0) return null;
  const card = el('section', 'card');
  card.appendChild(el('h3', '', 'Usage'));
  card.appendChild(kv(pairs));
  return card;
}

// downloadExport exports the session as Markdown.
// The endpoint requires authentication, so a plain <a href> will not do — the request has
// to carry the Authorization header and the download is built from the response.
async function downloadExport(record, node) {
  const original = node.textContent;
  setText(node, 'Exporting\u2026');
  try {
    const path = '/sessions/' + encodeURIComponent(record.sessionId) +
      '/export?limit=' + MESSAGE_LIMIT + '&order=' + state.order;
    const headers = {};
    if (state.token) headers.Authorization = 'Bearer ' + state.token;
    const res = await fetch(path, { headers });
    if (!res.ok) throw new Error('HTTP ' + res.status);

    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = (record.shortKey || record.sessionId) + '.md';
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
  card.appendChild(el('h3', '', 'Session'));

  const idRow = el('div', 'idrow');
  idRow.appendChild(el('code', '', record.sessionId));
  idRow.appendChild(copyButton(record.sessionId));
  idRow.appendChild(button('ghost tiny', 'Export md',
    (event) => downloadExport(record, event.currentTarget)));
  card.appendChild(idRow);

  card.appendChild(kv([
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
    card.appendChild(path);
  }
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
  side.replaceChildren(box);
}

// ---------------------------------------------------------------------------
// Selection and detail synchronisation
// ---------------------------------------------------------------------------

function selectSession(sessionId) {
  if (!sessionId || sessionId === state.selectedId) return;
  state.selectedId = sessionId;
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
async function syncDetail(options) {
  const force = options && options.force;
  const record = state.byId.get(state.selectedId);
  if (!record) {
    clearDetail('\u2190 Pick a session on the left');
    return;
  }

  const signature = [record.sessionId, record.updatedAt, record.status, state.order].join('|');
  if (!force && state.detail && state.detail.signature === signature) return;
  if (state.busy) return;

  const switched = !state.detail || state.detail.sessionId !== record.sessionId;
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
    const [messages, final] = await Promise.all([
      api('/sessions/' + id + '/messages?limit=' + MESSAGE_LIMIT + '&order=' + state.order),
      api('/sessions/' + id + '/final'),
    ]);
    state.detail = { sessionId: record.sessionId, signature, messages, final };
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
  pane.scrollTop = toBottom ? pane.scrollHeight : 0;
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
  renderList();
});

$('source-filter').addEventListener('change', (event) => {
  state.source = event.target.value;
  renderList();
});

$('refresh').addEventListener('click', refresh);

$('auto').addEventListener('change', (event) => {
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

async function start() {
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
