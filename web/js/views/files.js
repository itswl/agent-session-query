// The turns that changed a file, newest first, each with how the file stands now against
// what that turn left: whether a recollection of it still holds.

import { h, icon, time, skeleton, errorBox, empty, choices, fill, stale } from '../dom.js';
import { api, qs, sessionHref, isAbort, last } from '../api.js';
import { coalesce } from '../live.js';
import { go } from '../router.js';
import * as f from '../format.js';
import { WINDOWS, windowOf as windowIn, within, longer } from '../windows.js';

const PAGE = 50;

const windowOf = (query) => windowIn(query, '7d');
const readToo = (query) => query.get('read') === '1';
const pageAt = (query, offset) =>
  '/api/files' + qs({ since: windowOf(query), path: query.get('path') || '', changed: !readToo(query), limit: PAGE, offset });

// first is what the view asks for when it opens, so that it can be asked for ahead.
export const first = (query) => pageAt(query, 0);

// How a file stands now, as the command line says it (render.FileNowLine), and how much
// to make of it: as the turn left it, moved on since, or gone.
const NOW = {
  'same/content': ['as this turn left it', 'ok'],
  'same/time': ['not written since', 'ok'],
  'written/content': ['written since — what this turn wrote is still in it', 'warn'],
  'written/time': ['written since', 'warn'],
  'changed/content': ['changed since — what this turn wrote is no longer all there', 'bad'],
  'deleted/': ['deleted', 'bad'],
};
const NOW_TITLE = {
  content: 'Judged by content: the text this turn last wrote to the file, against the file.',
  time: 'Judged by time: the file’s modification time, against the end of the turn.',
};

export function mount(main, route, ctx) {
  document.title = 'Files · asq';
  const since = windowOf(route.query);
  const path = route.query.get('path') || '';
  const read = readToo(route.query);
  const href = (changes) => '/files' + qs({ since, path, read, ...changes });
  const v = { items: [], total: 0, end: false, busy: false, top: null };
  const summary = h('p', { class: 'dim summary' });
  const waiting = h('div', { class: 'waiting' });
  const list = h('ol', { class: 'fails file-list' });
  const more = h('div', { class: 'more-row' });
  const body = h('div', { class: 'page-body' }, skeleton(6, 'row'));
  const note = stale();
  const input = h('input', { type: 'search', name: 'path', value: path, placeholder: 'part of a path: auth.go, src/api/', 'aria-label': 'Files whose path contains', autocomplete: 'off', spellcheck: false });
  const form = h('form', { class: 'file-filter', role: 'search' }, input);
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    go(href({ path: input.value.trim() }));
  });
  main.append(h('div', { class: 'page' },
    h('header', { class: 'page-head' }, h('h1', { tabIndex: -1 }, 'Files'),
      choices('Window', WINDOWS.map(([k, name]) => [href({ since: k }), name, k === since]))),
    form,
    h('nav', { class: 'chips', 'aria-label': 'Which turns' },
      h('a', { class: 'chip', href: href({ read: false }), 'aria-current': read ? null : 'true' }, 'Changed'),
      h('a', { class: 'chip', href: href({ read: true }), 'aria-current': read ? 'true' : null }, 'Read or changed')),
    note.el, waiting, summary, body));

  function show(r, reset) {
    if (reset) v.top = r;
    v.items = reset ? r.files : v.items.concat(r.files);
    v.total = r.total;
    v.end = r.files.length < PAGE;
    // A row is a file and a turn that touched it, so a file two turns changed is counted
    // twice, as the digest counts files changed
    // Folders a look is waiting in: on a Mac, macOS asking on its screen whether asq may
    // read them. Their files are listed, and not judged, until it is answered
    const folders = [...new Set(v.items.filter((x) => x.waiting && !x.now).map((x) => x.waiting))];
    waiting.replaceChildren(...folders.map((dir) => h('div', { class: 'notice warn', role: 'status' }, icon('alert'),
      h('p', null, 'How files in ', h('code', null, dir), ' stand now is not shown: asq is waiting to read that folder. On a Mac, macOS is asking on the Mac’s screen whether asq may — answer it there, or allow asq in System Settings › Privacy & Security › Files and Folders.'))));
    summary.textContent = `${f.plural(r.total, 'file')}${path ? ` whose path contains “${path}”` : ''} ${read ? 'read or changed' : 'changed'}${within(since)}.`;
    list.replaceChildren(...v.items.map(row));
    if (!v.items.length) list.replaceChildren(h('li', null, empty(path ? `No file whose path contains “${path}”${within(since)}.` : `No files ${read ? 'read or changed' : 'changed'}${within(since)}.`, longer(since))));
    if (!body.contains(list)) body.replaceChildren(list, more);
    more.replaceChildren(v.end ? '' : h('button', { type: 'button', class: 'btn', onclick: () => load(false) }, 'Load more'));
  }

  async function load(reset) {
    if (v.busy) return;
    v.busy = true;
    try {
      const r = await api(pageAt(route.query, reset ? 0 : v.items.length), { signal: ctx.signal, keep: reset });
      if (reset) note.say();
      if (reset && r === v.top) return askAgain(); // the first page is what is on screen already
      show(r, reset);
      if (reset) askAgain();
    } catch (e) {
      if (isAbort(e)) return;
      // What was shown before stays, says it is from earlier, and the next change asks again
      if (reset && v.top) return note.say(e, () => load(true));
      (body.contains(list) ? more : body).replaceChildren(errorBox(e, () => { v.busy = false; load(reset); }));
    } finally {
      v.busy = false;
    }
  }

  // A folder a look is waiting in is asked about again every few seconds while the page is
  // looked at: its answer comes from the Mac, and no change to the stores says it came
  let again = 0;
  ctx.signal.addEventListener('abort', () => clearTimeout(again));
  function askAgain() {
    clearTimeout(again);
    if (ctx.signal.aborted || v.items.length > PAGE || !v.items.some((x) => x.waiting && !x.now)) return;
    again = setTimeout(() => (document.hidden ? askAgain() : load(true)), 5000);
  }

  // What it showed last time is shown at once; the fresh list replaces it when it comes
  if (last(first(route.query))) show(last(first(route.query)), true);
  load(true);
  return { changes: coalesce(() => { if (v.items.length <= PAGE) load(true); }, 5000) };
}

function row(x) {
  const [said, tone] = NOW[x.now ? `${x.now.state}/${x.now.by || ''}` : ''] || [];
  return h('li', { class: 'fail file' },
    h('div', { class: 'f-head dots' },
      time(x.at),
      h('a', { href: sessionHref(x.session, String(x.turn)), title: x.cwd || null }, x.title || x.session, h('span', { class: 'turn-no' }, ' #' + x.turn)),
      !x.changed && h('span', { class: 'dim' }, 'read')),
    h('p', { class: 'mono file-path' }, x.path),
    said && h('p', { class: `file-now ${tone}`, title: NOW_TITLE[x.now.by] || null }, 'now: ', said),
    !said && x.waiting && h('p', { class: 'file-now dim', title: `asq is waiting to read ${x.waiting}` }, 'now: not looked at yet'),
    x.ask && h('p', { class: 'dim file-ask' }, f.firstLine(x.ask, 200)));
}
