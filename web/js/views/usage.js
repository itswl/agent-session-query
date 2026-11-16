// What the model calls consumed: rolling windows, the last thirty days, and where it went.

import { h, mark, skeleton, errorBox } from '../dom.js';
import { api, qs } from '../api.js';
import { coalesce } from '../live.js';
import * as f from '../format.js';

// Stacked bottom to top in this order; the colours are the chart palette's first four
const PARTS = [['cacheRead', 'Cache read'], ['cacheWrite', 'Cache write'], ['input', 'Input'], ['output', 'Output']];
const WINDOWS = { '24h': 'Last 24 hours', '3d': 'Last 3 days', '7d': 'Last 7 days', '30d': 'Last 30 days', all: 'All time' };

export function mount(main, route, ctx) {
  document.title = 'Usage · asq';
  const state = { withCache: true, table: false, report: null };
  const body = h('div', { class: 'page-body' }, skeleton(6, 'line'));
  main.append(h('div', { class: 'page' }, h('header', { class: 'page-head' }, h('h1', { tabIndex: -1 }, 'Usage')), body));

  async function load() {
    try {
      state.report = await api('/api/usage', { signal: ctx.signal });
      render();
    } catch (e) {
      if (e.name !== 'AbortError') body.replaceChildren(errorBox(e, load));
    }
  }

  function render() {
    const r = state.report;
    body.replaceChildren(
      h('dl', { class: 'windows' }, r.windows.map((w) => h('div', { class: 'window' },
        h('dt', null, w.key, h('span', { class: 'sr' }, ' — ' + WINDOWS[w.key])),
        h('dd', { class: 'big', title: f.num(w.total) + ' tokens' }, f.tokens(w.total)),
        h('dd', { class: 'dots dim' }, h('span', { title: f.num(w.calls) + ' model calls' }, f.plural(w.calls, 'call')),
          w.total > 0 && h('span', null, f.pct(w.cacheHit) + ' cached'))))),
      chart(r.byDay),
      section('By model', table(r.byModel, r.total, 'Model', (k) => k || '(not recorded)')),
      section('By store', table(r.bySource, r.total, 'Store', (k) => h('span', { class: 'store' }, mark(k), k))),
      section('By project', table(r.byProject, r.total, 'Project', (k) => (k ? h('a', { href: '/' + qs({ project: k }), title: k }, f.shortPath(k)) : '(no directory)'))),
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
      h('header', { class: 'section-head' }, h('h2', null, 'Tokens by day'), h('span', { class: 'dim' }, 'last 30 days'),
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

const section = (title, content) => h('section', null, h('header', { class: 'section-head' }, h('h2', null, title)), content);

function table(rows, total, keyLabel, keyCell) {
  const n = (v) => h('td', { class: 'n', title: f.num(v) }, f.tokens(v));
  // Reasoning is the part of the output that was thinking, where the provider said so:
  // nothing recorded is a dash, not a zero
  const reasoning = (v) => (v > 0 ? n(v) : h('td', { class: 'n dim', title: 'Not recorded' }, '—'));
  const row = (r, head) => h('tr', null, head,
    h('td', { class: 'n' }, f.num(r.sessions)), h('td', { class: 'n' }, f.num(r.calls)),
    n(r.usage?.input), n(r.usage?.output), reasoning(r.usage?.reasoning), n(r.usage?.cacheRead), n(r.usage?.cacheWrite), n(r.total),
    h('td', { class: 'n' }, f.pct(r.cacheHit)));
  return h('div', { class: 'table-wrap' }, h('table', null,
    h('thead', null, h('tr', null, [keyLabel, 'Sessions', 'Calls', 'Input', 'Output', 'Reasoning', 'Cache read', 'Cache write', 'Total', 'Cache hit']
      .map((c, i) => h('th', { scope: 'col', class: i ? 'n' : null,
        title: c === 'Reasoning' ? 'The part of the output that was reasoning' : null }, c)))),
    h('tbody', null, rows.map((r) => row(r, h('th', { scope: 'row' }, keyCell(r.key))))),
    h('tfoot', null, row(total, h('th', { scope: 'row' }, 'Total', rows.length >= 12 && h('span', { class: 'dim' }, ' · with those not listed'))))));
}
