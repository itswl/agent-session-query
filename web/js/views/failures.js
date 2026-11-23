// The tool calls that failed in a window, newest first, and which tools failed most — or
// grouped by the command that failed, those still open first; every failure, or the ones
// whose command did not pass later.

import { h, time, skeleton, errorBox, empty, choices, fill, stale } from '../dom.js';
import { api, qs, sessionHref, isAbort, last } from '../api.js';
import { coalesce } from '../live.js';
import * as f from '../format.js';
import { WINDOWS, windowOf as windowIn, within, longer } from '../windows.js';
import * as seen from '../seen.js';
import * as projects from '../projects.js';

const PAGE = 50;

const windowOf = (query) => windowIn(query, '7d');
const openOf = (query) => query.get('open') === '1';
const byOf = (query) => (query.get('by') === 'command' ? 'command' : '');
const pageAt = (query, offset) => '/api/failures' + qs({ since: windowOf(query), tool: query.get('tool') || '', open: openOf(query), by: byOf(query), project: query.get('project'), limit: PAGE, offset });

// first is what the view asks for when it opens, so that it can be asked for ahead.
export const first = (query) => pageAt(query, 0);

export function mount(main, route, ctx) {
  document.title = 'Failures · asq';
  const since = windowOf(route.query);
  const tool = route.query.get('tool') || '';
  const open = openOf(route.query);
  const by = byOf(route.query);
  const project = route.query.get('project') || '';
  const href = (changes) => '/failures' + qs({ since, tool, open, by, project, ...changes });
  const v = { items: [], total: 0, end: false, busy: false, top: null };
  const summary = h('p', { class: 'dim summary' });
  const ways = h('nav', { class: 'chips', 'aria-label': 'Which failures, and how' });
  const chips = h('nav', { class: 'chips', 'aria-label': 'Tools' });
  const list = h('ol', { class: 'fails' });
  const more = h('div', { class: 'more-row' });
  const body = h('div', { class: 'page-body' }, skeleton(6, 'row'));
  const note = stale();
  main.append(h('div', { class: 'page' },
    h('header', { class: 'page-head' }, h('h1', { tabIndex: -1 }, 'Failures'),
      choices('Window', WINDOWS.map(([k, name]) => [href({ since: k }), name, k === since])),
      projects.picker(project, (p) => href({ project: p }))),
    note.el, ways, summary, chips, body));
  // A failure since the last look at this view is marked new (see seen.js)
  seen.look('failures');
  const rows = () => { const base = seen.since('failures'); return v.items.map((x) => (by ? group(x, base) : row(x, base))); };
  // A group is new by the latest of its failures
  const atOf = (x) => (by ? x.last.at : x.at);
  // How many are new, and the way to say they have been seen
  const fresh = h('span', { class: 'seen-count' });
  const freshen = () => {
    const base = seen.since('failures');
    const n = base ? v.items.filter((x) => f.when(atOf(x)) > base).length : 0;
    fresh.replaceChildren(...(n ? [` ${f.num(n)} new since your last visit · `, h('button', { type: 'button', class: 'link-btn', onclick() { seen.markSeen('failures'); list.replaceChildren(...rows()); freshen(); } }, 'Mark as seen')] : []));
  };
  const offVisit = seen.onVisit(() => { if (v.items.length) { list.replaceChildren(...rows()); freshen(); } });

  function show(r, reset) {
    if (reset) v.top = r;
    const got = (by ? r.commands : r.failures) || [];
    v.items = reset ? got : v.items.concat(got);
    v.total = r.total;
    v.end = got.length < PAGE;
    // How many failed, and how many of those the same command passed later: the open ones
    // are what is still to see to
    const calls = (r.open || 0) + (r.passedLater || 0);
    const where = `${tool ? ` of ${tool}` : ''}${projects.within(project)}${within(since)}`;
    summary.replaceChildren(
      by ? `${f.plural(r.total, 'command')} · ` : '',
      open ? `${f.num(r.open)} of ${f.plural(calls, 'failed call')}${where} still open.` : `${f.plural(calls, 'failed call')}${where}${f.later(calls, r.passedLater)}.`,
      fresh);
    if (reset) fill(ways,
      h('a', { class: 'chip', href: href({ open: false }), 'aria-current': open ? null : 'true' }, 'Every failure'),
      h('a', { class: 'chip', href: href({ open: true }), 'aria-current': open ? 'true' : null, title: 'The failures whose command did not pass later, in that turn or the three after' },
        'Still open', h('span', { class: 'n' }, f.num(r.open))),
      h('span', { class: 'chip-gap', 'aria-hidden': 'true' }),
      h('a', { class: 'chip', href: href({ by: '' }), 'aria-current': by ? null : 'true' }, 'Newest first'),
      h('a', { class: 'chip', href: href({ by: 'command' }), 'aria-current': by ? 'true' : null, title: 'One row per command that failed, those still open first' }, 'By command'));
    if (reset) fill(chips,
      h('a', { class: 'chip', href: href({ tool: '' }), 'aria-current': tool ? null : 'true' }, 'All tools'),
      r.byTool.map((t) => h('a', { class: 'chip', href: href({ tool: t.tool === tool ? '' : t.tool }), 'aria-current': t.tool === tool ? 'true' : null },
        h('span', { class: 'mono' }, t.tool), h('span', { class: 'n' }, f.num(t.count)))));
    list.replaceChildren(...rows());
    freshen();
    // What the window alone left out is said, with the way to it
    const outside = r.outside > 0 && h('span', null, `${f.plural(r.outside, open ? 'open failure' : 'failed call')} from before then — `, h('a', { href: href({ since: 'all' }) }, 'All time'));
    const none = open && r.passedLater > 0 ? `No failed call${where} is still open: the same command passed later for ${r.passedLater === 1 ? 'it' : r.passedLater === 2 ? 'both' : `all ${f.num(r.passedLater)}`}.` : `No failed tool calls${where}.`;
    if (!v.items.length) list.replaceChildren(h('li', null, empty(none, outside || (open && r.passedLater > 0 ? h('a', { href: href({ open: false }) }, 'Every failure') : tool ? (since === 'all' ? 'Try all tools.' : 'Try all tools, or a longer window.') : longer(since)))));
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
  return h('li', { class: 'fail' }, failure(x, base, 12));
}

// A command's failures: how often it failed, in how many sessions, how many of them are
// still open, and the latest of them.
function group(g, base) {
  const name = g.command || g.tool;
  return h('li', { class: 'fail f-group' },
    h('div', { class: 'f-name dots' },
      h('span', { class: 'mono' }, name),
      !g.command && h('span', { class: 'dim', title: 'Calls of a tool that ran no command whose program is known' }, 'no command'),
      h('span', { class: g.open ? 'bad' : 'dim', title: f.passedTitle }, f.failed({ failures: g.count, passedLater: g.count - g.open })),
      h('span', { class: 'dim' }, f.plural(g.sessions, 'session'))),
    failure(g.last, base, 4, 'Latest'));
}

// failure is one failed call: when, where, how it ended, what it ran, the end of what it
// printed, and where the same command passed later.
function failure(x, base, most, label) {
  const lines = (x.output || '').trimEnd().split('\n');
  const tail = lines.slice(-most).join('\n');
  return [
    h('div', { class: 'f-head dots' },
      label && h('span', null, label),
      time(x.at),
      base > 0 && f.when(x.at) > base && h('span', { class: 'fresh', title: `Failed since you last looked, ${f.stamp(base)}` }, 'new'),
      h('a', { href: sessionHref(x.session, `${x.turn}.${x.seq}`), title: x.cwd || null }, x.title || x.session, h('span', { class: 'turn-no' }, ' #' + x.turn)),
      h('span', { class: 'mono tool-name' }, x.tool),
      h('span', { class: 'bad' }, x.status, x.exit != null && ` · exit ${x.exit}`)),
    x.command && h('pre', { class: 'f-cmd' }, x.command),
    tail && h('pre', { class: 'io out f-out' }, (lines.length > most ? '…\n' : '') + tail),
    x.passedLater && passed(x)];
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
