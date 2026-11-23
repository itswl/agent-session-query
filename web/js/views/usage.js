// What the model calls consumed: rolling windows, the last thirty days, and where it went
// in the window chosen.

import { h, mark, skeleton, errorBox, stale, choices } from '../dom.js';
import { api, qs, last, sessionHref } from '../api.js';
import { coalesce } from '../live.js';
import * as f from '../format.js';
import { WINDOWS, windowOf, label, span } from '../windows.js';
import * as projects from '../projects.js';

// Stacked bottom to top in this order; the colours are the chart palette's first four. The
// record's input is the part of the prompt neither read from cache nor written to it — new
// input: "in", on the command line and a session's page, is the whole prompt
const PARTS = [['cacheRead', 'Cache read'], ['cacheWrite', 'Cache write'], ['input', 'New input'], ['output', 'Output']];
// The window the breakdowns cover: all of it unless one is chosen
const windowIn = (query) => windowOf(query, 'all');

// first is what the view asks for when it opens, so that it can be asked for ahead.
export const first = (query) => '/api/usage' + qs({ since: windowIn(query), project: query.get('project') });

export function mount(main, route, ctx) {
  document.title = 'Usage · asq';
  const since = windowIn(route.query);
  const project = route.query.get('project') || '';
  // All time, the view's own window, is no since in its address
  const href = (changes) => '/usage' + qs({ since: since === 'all' ? '' : since, project, ...changes });
  // What it showed last time is shown at once; the fresh report replaces it when it comes
  const state = { withCache: true, table: false, report: last(first(route.query)) };
  const body = h('div', { class: 'page-body' }, state.report ? null : skeleton(6, 'line'));
  const note = stale();
  main.append(h('div', { class: 'page' },
    h('header', { class: 'page-head' }, h('h1', { tabIndex: -1 }, 'Usage'),
      choices('Window', WINDOWS.map(([k, name]) => [href({ since: k === 'all' ? '' : k }), name, k === since])),
      projects.picker(project, (p) => href({ project: p }))),
    note.el, body));

  async function load() {
    try {
      const report = await api(first(route.query), { signal: ctx.signal, keep: true });
      note.say();
      if (report === state.report) return; // nothing changed: what is drawn stays as it is, under the pointer
      state.report = report;
      render();
    } catch (e) {
      if (e.name === 'AbortError') return;
      // What is drawn stays, and says it is from earlier
      if (state.report) note.say(e, load);
      else body.replaceChildren(errorBox(e, load));
    }
  }

  function render() {
    const r = state.report;
    body.replaceChildren(
      // Every window at once, whichever is chosen: what each consumed, the chosen one marked
      // as the one the tables below add up to
      h('dl', { class: 'windows' }, r.windows.map((w) => h('div', { class: w.key === since ? 'window on' : 'window', 'aria-current': w.key === since ? 'true' : null },
        h('dt', null, label(w.key)),
        h('dd', { class: 'big', title: f.num(w.total) + ' tokens' }, f.tokens(w.total)),
        h('dd', { class: 'dots dim' }, h('span', { title: f.num(w.calls) + ' model calls' }, f.plural(w.calls, 'call')),
          w.total > 0 && h('span', null, f.pct(w.cacheHit) + ' cached'))))),
      chart(r.byDay),
      section('By model', span(since), table(r.byModel, r.total, 'Model', (k) => k || '(not recorded)')),
      section('By store', span(since), table(r.bySource, r.total, 'Store', (k) => h('span', { class: 'store' }, mark(k), k))),
      // a project's sessions, in the window its row is of
      section('By project', span(since), table(r.byProject, r.total, 'Project', (k) => (k ? h('a', { href: '/' + qs({ project: k, since: since === 'all' ? '' : since }), title: k }, f.shortPath(k)) : '(no directory)'))),
      // a subagent's tokens are the session's it ran in
      r.bySession && section('By session', span(since), table(r.bySession, r.total, 'Session', (k, row) => [
        h('a', { href: sessionHref(k), title: k }, row.title || k),
        row.cwd && h('span', { class: 'dim', title: row.cwd }, ' · ' + f.shortPath(row.cwd))])),
    );
  }

  function chart(byDay) {
    const parts = state.withCache ? PARTS : PARTS.slice(1);
    const days = lastDays(byDay, 30);
    const sum = (d) => parts.reduce((n, [k]) => n + (d.usage[k] || 0), 0);
    const top = niceMax(Math.max(1, ...days.map(sum)));
    const describe = (d) => `${f.day(d.at)}: ${f.num(sum(d))} tokens — ` + parts.map(([k, label]) => `${label.toLowerCase()} ${f.num(d.usage[k] || 0)}`).join(', ');
    const readout = h('p', { class: 'readout', 'aria-live': 'polite' }, 'Point at a day for its figures.');
    const cols = days.map((d) => {
      const col = h('div', { class: 'col', tabIndex: 0, title: describe(d), 'aria-label': describe(d) },
        parts.map(([k]) => {
          if (!d.usage[k]) return null;
          const seg = h('span', { class: 'seg', dataset: { part: k } });
          seg.style.height = (d.usage[k] / top) * 100 + '%';
          return seg;
        }));
      const show = () => { readout.textContent = describe(d); };
      col.addEventListener('pointerenter', show);
      col.addEventListener('focus', show);
      return col;
    });
    const mode = (withCache, label) => h('button', { type: 'button', class: 'seg-btn', 'aria-pressed': String(state.withCache === withCache), onclick() { state.withCache = withCache; render(); } }, label);
    const tableBtn = h('button', { type: 'button', class: 'btn', 'aria-pressed': String(state.table), onclick() { state.table = !state.table; render(); } }, state.table ? 'Hide table' : 'Show as table');
    return h('section', { class: 'chart' },
      h('header', { class: 'section-head' }, h('h2', null, 'Tokens by day'), h('span', { class: 'dim' }, span('30d')),
        h('div', { class: 'seg-group', role: 'group', 'aria-label': 'What the bars count' }, mode(true, 'All tokens'), mode(false, 'Without cache reads')), tableBtn),
      h('ul', { class: 'legend' }, parts.map(([k, label]) => h('li', null, h('span', { class: 'swatch', dataset: { part: k } }), label))),
      readout,
      h('div', { class: 'plot', role: 'group', 'aria-label': 'Tokens by day, one bar per day' },
        h('div', { class: 'grid' }, h('span', null, f.tokens(top)), h('span', null, f.tokens(top / 2)), h('span', null, '0')),
        h('div', { class: 'cols' }, cols)),
      h('div', { class: 'x-axis', 'aria-hidden': 'true' }, days.map((d, i) => h('span', null, (days.length - 1 - i) % 7 === 0 ? f.shortDay(d.at) : ''))),
      state.table && h('div', { class: 'table-wrap' }, h('table', null,
        h('thead', null, h('tr', null, h('th', { scope: 'col' }, 'Day'), parts.map(([, label]) => h('th', { scope: 'col', class: 'n' }, label)), h('th', { scope: 'col', class: 'n' }, 'Total'))),
        h('tbody', null, days.map((d) => h('tr', null, h('th', { scope: 'row' }, f.day(d.at)),
          parts.map(([k]) => h('td', { class: 'n' }, f.num(d.usage[k] || 0))), h('td', { class: 'n' }, f.num(sum(d)))))))));
  }

  if (state.report) render();
  load();
  return { changes: coalesce(load, 5000) };
}

// lastDays lays the report's days on a calendar: a day nothing was spent on is a gap
// that has to show, not a day that is skipped.
function lastDays(rows, n) {
  const by = new Map(rows.map((r) => [r.key, r]));
  const out = [];
  for (let i = n - 1; i >= 0; i--) {
    const d = new Date();
    d.setHours(12, 0, 0, 0);
    d.setDate(d.getDate() - i);
    out.push({ at: d.getTime(), usage: by.get(f.localDate(d))?.usage || {} });
  }
  return out;
}

function niceMax(v) {
  const p = 10 ** Math.floor(Math.log10(v));
  return [1, 2, 2.5, 5, 10].map((m) => m * p).find((x) => x >= v);
}

const section = (title, when, content) => h('section', null, h('header', { class: 'section-head' }, h('h2', null, title), h('span', { class: 'dim' }, when)), content);

const HEAD_TITLES = {
  'New input': 'The prompt, less what was read from cache or written to it. With those two, it is the prompt: what the command line calls in',
  Reasoning: 'The part of the output that was reasoning',
};

// A table of sessions has no column of how many sessions each row is: one
function table(rows, total, keyLabel, keyCell) {
  const counted = keyLabel !== 'Session';
  const n = (v) => h('td', { class: 'n', title: f.num(v) }, f.tokens(v));
  // Reasoning is the part of the output that was thinking, where the provider said so:
  // nothing recorded is a dash, not a zero
  const reasoning = (v) => (v > 0 ? n(v) : h('td', { class: 'n dim', title: 'Not recorded' }, '—'));
  const row = (r, head) => h('tr', null, head,
    counted && h('td', { class: 'n' }, f.num(r.sessions)), h('td', { class: 'n' }, f.num(r.calls)),
    n(r.usage?.input), n(r.usage?.output), reasoning(r.usage?.reasoning), n(r.usage?.cacheRead), n(r.usage?.cacheWrite), n(r.total),
    h('td', { class: 'n' }, f.pct(r.cacheHit)));
  return h('div', { class: 'table-wrap' }, h('table', null,
    h('thead', null, h('tr', null, [keyLabel, counted && 'Sessions', 'Calls', 'New input', 'Output', 'Reasoning', 'Cache read', 'Cache write', 'Total', 'Cache hit']
      .filter(Boolean).map((c, i) => h('th', { scope: 'col', class: i ? 'n' : null, title: HEAD_TITLES[c] }, c)))),
    h('tbody', null, rows.map((r) => row(r, h('th', { scope: 'row' }, keyCell(r.key, r))))),
    h('tfoot', null, row(total, h('th', { scope: 'row' }, 'Total', rows.length >= 12 && h('span', { class: 'dim' }, ' · with those not listed'))))));
}
