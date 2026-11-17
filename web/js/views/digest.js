// What a window's work added up to, per project.

import { h, mark, skeleton, errorBox, empty, choices, stale } from '../dom.js';
import { api, qs, isAbort, last } from '../api.js';
import { coalesce } from '../live.js';
import * as f from '../format.js';
import { WINDOWS, windowOf as windowIn, within, longer } from '../windows.js';

const KINDS = ['code', 'test', 'docs', 'config'];

const windowOf = (query) => windowIn(query, '7d');

// first is what the view asks for when it opens, so that it can be asked for ahead.
export const first = (query) => '/api/digest' + qs({ since: windowOf(query) });

export function mount(main, route, ctx) {
  document.title = 'Digest · asq';
  const since = windowOf(route.query);
  const path = first(route.query);
  let shown = null;
  const summary = h('p', { class: 'summary' });
  const body = h('div', { class: 'page-body' }, skeleton(5, 'block'));
  const note = stale();
  main.append(h('div', { class: 'page' },
    h('header', { class: 'page-head' }, h('h1', { tabIndex: -1 }, 'Digest'),
      choices('Window', WINDOWS.map(([k, name]) => ['/digest' + qs({ since: k }), name, k === since]))),
    note.el, summary, body));

  function show(r) {
    shown = r;
    const t = r.total;
    summary.replaceChildren(h('span', { class: 'dots' },
      h('span', null, f.plural(r.projects.length, 'project')),
      h('span', null, f.plural(t.sessions, 'session')),
      h('span', null, f.plural(t.turns, 'turn')),
      h('span', null, f.plural(t.tools, 'tool call')),
      h('span', { class: t.failures ? 'bad' : null }, `${f.num(t.failures)} failed`),
      h('span', { title: f.num(f.totalTokens(t.usage)) }, `${f.tokens(f.totalTokens(t.usage))} tokens`),
      h('span', null, `${f.plural(KINDS.reduce((n, k) => n + (t.changed?.[k] || 0), 0), 'file')} changed`)));
    body.replaceChildren(r.projects.length
      ? h('ol', { class: 'digest' }, r.projects.map(project))
      : empty(`Nothing was asked${within(since)}.`, longer(since)));
  }

  async function load() {
    try {
      const r = await api(path, { signal: ctx.signal, keep: true });
      note.say();
      if (r !== shown) show(r);
    } catch (e) {
      if (isAbort(e)) return;
      // What is drawn stays, and says it is from earlier
      if (shown) note.say(e, load);
      else body.replaceChildren(errorBox(e, load));
    }
  }

  // What it showed last time is shown at once; the fresh digest replaces it when it comes
  if (last(path)) show(last(path));
  load();
  return { changes: coalesce(load, 5000) };
}

function project(d) {
  const tok = f.totalTokens(d.usage);
  const changed = KINDS.filter((k) => d.changed?.[k]);
  const models = Object.entries(d.models || {}).sort((a, b) => b[1] - a[1]);
  const stores = Object.entries(d.sources || {}).sort((a, b) => b[1] - a[1]);
  return h('li', { class: 'd-project' },
    h('h2', null, d.cwd ? h('a', { href: '/' + qs({ project: d.cwd }), title: d.cwd }, f.shortPath(d.cwd)) : '(no directory)'),
    d.cwd && h('p', { class: 'path dim' }, d.cwd),
    h('p', { class: 'dots' },
      h('span', null, f.plural(d.sessions, 'session')),
      h('span', null, f.plural(d.turns, 'turn')),
      h('span', null, f.plural(d.tools, 'tool call')),
      d.failures > 0 && h('span', { class: 'bad' }, `${f.num(d.failures)} failed`),
      tok > 0 && h('span', { title: `${f.num(tok)} tokens, ${f.pct(d.cacheHit)} from cache` }, `${f.tokens(tok)} tokens`)),
    h('dl', { class: 'd-facts' },
      changed.length > 0 && h('div', null, h('dt', null, 'Changed'), h('dd', { class: 'dots' }, changed.map((k) => h('span', null, `${k} ${f.num(d.changed[k])}`)))),
      models.length > 0 && h('div', null, h('dt', null, 'Models'), h('dd', { class: 'dots' }, models.map(([m, n]) => h('span', { title: f.plural(n, 'model call') }, m)))),
      stores.length > 0 && h('div', null, h('dt', null, 'Stores'), h('dd', { class: 'dots' }, stores.map(([s, n]) => h('span', { class: 'store', title: f.plural(n, 'session') }, mark(s), s)))),
      h('div', null, h('dt', null, 'Active'), h('dd', null, f.stamp(f.when(d.firstAt)), ' → ', f.stamp(f.when(d.lastAt))))));
}
