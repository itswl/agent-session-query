// The tool calls that failed in a window, newest first, and which tools failed most.

import { h, time, skeleton, errorBox, empty, choices, fill, stale } from '../dom.js';
import { api, qs, sessionHref, isAbort, last } from '../api.js';
import { coalesce } from '../live.js';
import * as f from '../format.js';
import { WINDOWS, windowOf as windowIn, within, longer } from '../windows.js';
import * as seen from '../seen.js';
import * as projects from '../projects.js';

const PAGE = 50;

const windowOf = (query) => windowIn(query, '7d');
const pageAt = (query, offset) => '/api/failures' + qs({ since: windowOf(query), tool: query.get('tool') || '', project: query.get('project'), limit: PAGE, offset });

// first is what the view asks for when it opens, so that it can be asked for ahead.
export const first = (query) => pageAt(query, 0);

export function mount(main, route, ctx) {
  document.title = 'Failures · asq';
  const since = windowOf(route.query);
  const tool = route.query.get('tool') || '';
  const project = route.query.get('project') || '';
  const href = (changes) => '/failures' + qs({ since, tool, project, ...changes });
  const v = { items: [], total: 0, end: false, busy: false, top: null };
  const summary = h('p', { class: 'dim summary' });
  const chips = h('nav', { class: 'chips', 'aria-label': 'Tools' });
  const list = h('ol', { class: 'fails' });
  const more = h('div', { class: 'more-row' });
  const body = h('div', { class: 'page-body' }, skeleton(6, 'row'));
  const note = stale();
  main.append(h('div', { class: 'page' },
    h('header', { class: 'page-head' }, h('h1', { tabIndex: -1 }, 'Failures'),
      choices('Window', WINDOWS.map(([k, name]) => [href({ since: k }), name, k === since])),
      projects.picker(project, (p) => href({ project: p }))),
    note.el, summary, chips, body));
  // A failure since the last look at this view is marked new (see seen.js)
  seen.look('failures');
  const rows = () => { const base = seen.since('failures'); return v.items.map((x) => row(x, base)); };
  const offVisit = seen.onVisit(() => { if (v.items.length) list.replaceChildren(...rows()); });

  function show(r, reset) {
    if (reset) v.top = r;
    v.items = reset ? r.failures : v.items.concat(r.failures);
    v.total = r.total;
    v.end = r.failures.length < PAGE;
    summary.textContent = `${f.plural(r.total, 'failed call')}${tool ? ` of ${tool}` : ''}${projects.within(project)}${within(since)}.`;
    if (reset) fill(chips,
      h('a', { class: 'chip', href: href({ tool: '' }), 'aria-current': tool ? null : 'true' }, 'All tools'),
      r.byTool.map((t) => h('a', { class: 'chip', href: href({ tool: t.tool === tool ? '' : t.tool }), 'aria-current': t.tool === tool ? 'true' : null },
        h('span', { class: 'mono' }, t.tool), h('span', { class: 'n' }, f.num(t.count)))));
    list.replaceChildren(...rows());
    if (!v.items.length) list.replaceChildren(h('li', null, empty(`No failed tool calls${tool ? ` of ${tool}` : ''}${projects.within(project)}${within(since)}.`, tool ? (since === 'all' ? 'Try all tools.' : 'Try all tools, or a longer window.') : longer(since))));
    if (!body.contains(list)) body.replaceChildren(list, more);
    more.replaceChildren(v.end ? '' : h('button', { type: 'button', class: 'btn', onclick: () => load(false) }, 'Load more'));
  }

  async function load(reset) {
    if (v.busy) return;
    v.busy = true;
    try {
      const r = await api(pageAt(route.query, reset ? 0 : v.items.length), { signal: ctx.signal, keep: reset });
      if (reset) note.say();
      if (reset && r === v.top) return; // the first page is what is on screen already
      show(r, reset);
    } catch (e) {
      if (isAbort(e)) return;
      // What was shown before stays, says it is from earlier, and the next change asks again
      if (reset && v.top) return note.say(e, () => load(true));
      (body.contains(list) ? more : body).replaceChildren(errorBox(e, () => { v.busy = false; load(reset); }));
    } finally {
      v.busy = false;
    }
  }

  // What it showed last time is shown at once; the fresh list replaces it when it comes
  if (last(first(route.query))) show(last(first(route.query)), true);
  load(true);
  return {
    changes: coalesce(() => { if (v.items.length <= PAGE) load(true); }, 5000),
    unmount() {
      offVisit();
      seen.leave('failures');
    },
  };
}

function row(x, base) {
  const lines = (x.output || '').trimEnd().split('\n');
  const tail = lines.slice(-12).join('\n');
  return h('li', { class: 'fail' },
    h('div', { class: 'f-head dots' },
      time(x.at),
      base > 0 && f.when(x.at) > base && h('span', { class: 'fresh', title: `Failed since you last looked, ${f.stamp(base)}` }, 'new'),
      h('a', { href: sessionHref(x.session, `${x.turn}.${x.seq}`), title: x.cwd || null }, x.title || x.session, h('span', { class: 'turn-no' }, ' #' + x.turn)),
      h('span', { class: 'mono tool-name' }, x.tool),
      h('span', { class: 'bad' }, x.status, x.exit != null && ` · exit ${x.exit}`)),
    x.command && h('pre', { class: 'f-cmd' }, x.command),
    tail && h('pre', { class: 'io out f-out' }, (lines.length > 12 ? '…\n' : '') + tail),
    x.passedLater && passed(x));
}

// Where the same command next succeeded in the session: what changed in between is how
// it was got past.
function passed(x) {
  const l = x.passedLater;
  const after = f.when(l.at) - f.when(x.at);
  const same = l.command === (x.command || '').trim().split(/\s+/).join(' ');
  return h('div', { class: 'f-later' },
    h('p', null, 'Passed ', after > 0 ? f.dur(after) + ' later' : 'later', ', at ',
      h('a', { href: sessionHref(x.session, `${l.turn}.${l.seq}`) }, `#${l.turn} step ${l.seq}`),
      h('span', { class: 'dim' }, ' — the turns between are how it was got past')),
    // Run again in another form: shown, so that whether it is the same thing passing is
    // the reader's to judge
    !same && h('pre', { class: 'f-cmd' }, l.command));
}
