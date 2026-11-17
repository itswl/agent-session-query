// The list of sessions, and the turns a search found. Both share the filters, which live
// in the address: a filtered view can be linked, and Back undoes a filter.

import { h, icon, mark, time, skeleton, errorBox, empty, fill, stale } from '../dom.js';
import { api, qs, sessionHref, isAbort, onForget } from '../api.js';
import { go, savedY } from '../router.js';
import { health, coalesce, onStatus, status } from '../live.js';
import { needles, highlight } from '../md.js';
import * as f from '../format.js';
import { WINDOWS } from '../windows.js';

const PAGE = 50;
const HITS = 20;
// The windows every view offers (see windows.js); all time is no filter at all
const WHEN = WINDOWS.map(([k, name]) => [k === 'all' ? '' : k, name]);
const TOP_PROJECTS = 8;

// Each list as it was left, by its address. Coming back to one — by Back from a session,
// by its link in the bar, by taking a filter off again — shows it at once, and then asks
// what has changed since. A search is kept for Back only: see start().
const KEPT = 12;
const kept = new Map();
const keyOf = (route) => (route.name === 'search' ? '/search?' : '/?') + route.query.toString();
let projectsP = null;
onForget(() => { kept.clear(); projectsP = null; });

function apiFilters(q) {
  const since = q.get('since');
  return {
    source: q.get('source'), project: q.get('project'),
    since: since === 'today' ? f.localDate() : since,
    archived: q.get('archived') === '1',
  };
}

// hrefWith is the current view's address with some parameters changed.
function hrefWith(route, changes) {
  const q = new URLSearchParams(route.query);
  for (const [k, v] of Object.entries(changes)) { if (v) q.set(k, v); else q.delete(k); }
  const s = q.toString();
  return (route.name === 'search' ? '/search' : '/') + (s ? '?' + s : '');
}

export function mount(main, route, ctx) {
  const v = { route, items: [], total: 0, sessions: 0, end: false, sel: null, busy: null, rows: new Map(), renew: false, failed: false };
  const aside = h('aside', { class: 'filters', id: 'filters', 'aria-label': 'Filters' });
  const toggle = h('button', { type: 'button', class: 'btn filters-toggle', 'aria-expanded': 'false', 'aria-controls': 'filters' }, icon('filter'), 'Filters');
  toggle.addEventListener('click', () => {
    const open = aside.classList.toggle('open');
    toggle.setAttribute('aria-expanded', String(open));
  });
  const title = h('h1', { tabIndex: -1, id: 'list-title' });
  const count = h('span', { class: 'count' });
  const list = h('div', { class: 'list' });
  const more = h('div', { class: 'more-row' });
  const note = stale();
  main.append(h('div', { class: 'browse' }, h('header', { class: 'results-head' }, title, count, toggle), aside,
    h('section', { class: 'results', 'aria-labelledby': 'list-title' }, note.el, list, more)));

  list.addEventListener('focusin', (e) => {
    const row = e.target.closest('.row');
    if (row) select(row, false);
  });
  const io = new IntersectionObserver((es) => { if (es.some((e) => e.isIntersecting) && !v.busy && !v.end && v.items.length) load(false); }, { rootMargin: '600px' });
  io.observe(more);
  // The filters list the stores health reports; they are drawn again when it changes. So
  // is a list that came back empty, which says when a store could not be read: the list
  // does not wait for the report, and is often answered first.
  let seenHealth = status.health;
  let saidEmpty = false;
  const offStatus = onStatus((st) => {
    if (!st.health || st.health === seenHealth) return;
    seenHealth = st.health;
    renderFilters();
    if (saidEmpty && !v.busy) renderList();
  });

  const searching = () => v.route.name === 'search';
  const words = () => needles(v.route.query.get('q') || '');

  function renderHead() {
    const q = v.route.query.get('q') || '';
    title.textContent = searching() ? 'Search' : 'Sessions';
    document.title = (searching() && q ? q + ' · ' : '') + title.textContent + ' · asq';
    count.textContent = !v.items.length && !v.total ? '' : searching()
      ? `${f.plural(v.total, 'turn')} in ${f.plural(v.sessions, 'session')}`
      : f.plural(v.total, 'session');
    const active = ['source', 'project', 'since', 'archived', 'agents'].filter((k) => v.route.query.get(k)).length;
    toggle.lastChild.textContent = active ? `Filters (${active})` : 'Filters';
  }

  async function renderFilters() {
    const q = v.route.query;
    const hl = await health().catch(() => null);
    projectsP ||= api('/api/projects').then((r) => r.projects).catch(() => { projectsP = null; return []; });
    const projects = await projectsP;
    const option = (key, value, label, extra) => {
      const now = q.get(key) || '';
      const on = (key === 'since' && now === 'all' ? '' : now) === value; // since=all is all time too
      return h('li', null, h('a', { href: hrefWith(v.route, { [key]: on && value ? '' : value }), class: 'opt', 'aria-current': on ? 'true' : null, title: extra?.title }, label, extra?.n != null && h('span', { class: 'n' }, f.num(extra.n))));
    };
    const flag = (key, label) => {
      const on = q.get(key) === '1';
      return h('li', null, h('a', { href: hrefWith(v.route, { [key]: on ? '' : '1' }), class: 'opt flag', 'aria-current': on ? 'true' : null }, h('span', { class: 'box' }, on && icon('check')), label));
    };
    const sinceNow = q.get('since') || '';
    const oddSince = sinceNow && sinceNow !== 'all' && !WHEN.some(([k]) => k === sinceNow) ? sinceNow : '';
    const stores = (hl?.stores || []).filter((s) => s.found);
    const chosen = q.get('project');
    let shown = projects.slice(0, TOP_PROJECTS);
    if (chosen && !shown.some((p) => p.cwd === chosen)) shown = [{ cwd: chosen }, ...shown];
    const rest = projects.slice(TOP_PROJECTS).filter((p) => p.cwd !== chosen);
    const projectOpt = (p) => option('project', p.cwd, f.shortPath(p.cwd) || '(no directory)', { title: p.cwd, n: p.sessions });
    const restList = h('ul', { class: 'opts', hidden: true }, rest.map(projectOpt));
    const restBtn = rest.length > 0 && h('button', { type: 'button', class: 'link-btn', 'aria-expanded': 'false', onclick() {
      restList.hidden = !restList.hidden;
      this.setAttribute('aria-expanded', String(!restList.hidden));
      this.textContent = restList.hidden ? `${rest.length} more` : 'Fewer';
    } }, `${rest.length} more`);
    fill(aside,
      h('section', null, h('h2', null, 'Store'), h('ul', { class: 'opts' },
        option('source', '', 'All stores'),
        stores.map((s) => option('source', s.source, [mark(s.source), s.title || s.source], { n: s.sessions, title: `${f.plural(s.sessions, 'session')}, ${f.plural(s.transcripts, 'transcript')} with subagents and archived ones · ${s.location}` })))),
      h('section', null, h('h2', null, 'Time'), h('ul', { class: 'opts seg' }, WHEN.map(([k, label]) => option('since', k, label)),
        // a window the address names that is none of these — an older link's "today", a
        // date — is shown as the one in force, until another is chosen
        oddSince && option('since', oddSince, oddSince === 'today' ? 'Today' : oddSince))),
      h('section', null, h('h2', null, 'Include'), h('ul', { class: 'opts' }, flag('archived', 'Archived'), !searching() && flag('agents', 'Subagent transcripts'))),
      projects.length > 0 && h('section', null, h('h2', null, 'Project'), h('ul', { class: 'opts' }, option('project', '', 'All projects'), shown.map(projectOpt)), restList, restBtn),
    );
  }

  function rowOf(item) {
    const key = item.ref;
    const sig = JSON.stringify(item);
    const had = v.rows.get(key);
    if (had?.sig === sig) return had.el;
    const el = searching() ? hitRow(item, words()) : sessionRow(item);
    v.rows.set(key, { sig, el });
    return el;
  }

  function renderList() {
    renderHead();
    const focused = list.contains(document.activeElement) ? document.activeElement : null;
    saidEmpty = !v.items.length;
    if (!v.items.length) {
      list.replaceChildren(emptyState());
    } else if (searching()) {
      list.replaceChildren(h('ol', { class: 'rows' }, v.items.map(rowOf)));
    } else {
      const days = [];
      for (const s of v.items) {
        const label = f.day(f.when(s.updatedAt) || Date.now());
        if (days.at(-1)?.label !== label) days.push({ label, rows: [] });
        days.at(-1).rows.push(rowOf(s));
      }
      list.replaceChildren(...days.map((d) => h('section', { class: 'day' }, h('h2', { class: 'day-head' }, d.label), h('ol', { class: 'rows' }, d.rows))));
    }
    for (const row of list.querySelectorAll('.row')) row.classList.toggle('selected', row.dataset.key === v.sel);
    if (focused && list.contains(focused)) focused.focus({ preventScroll: true });
    more.replaceChildren(v.end || !v.items.length ? '' : h('button', { type: 'button', class: 'btn', onclick: () => load(false) }, 'Load more'));
  }

  function emptyState() {
    const q = v.route.query.get('q') || '';
    const filtered = ['source', 'project', 'since'].some((k) => v.route.query.get(k));
    const warned = (status.health?.warnings || []).length;
    const clear = filtered && h('a', { href: hrefWith(v.route, { source: '', project: '', since: '' }) }, 'Clear the filters');
    if (searching() && !q.trim()) {
      return empty('Search every turn of every session.', 'Type in the search box above. Words match what they begin: "deploy" finds "deployment".',
        'Use "a phrase" in quotes, -word to exclude, and ask: reply: thinking: tool: output: to search one part of a turn.');
    }
    if (searching()) {
      return empty(`No turns match “${q}”.`, 'Try fewer words, a shorter word (words match what they begin), or drop a column prefix.', clear);
    }
    return empty(filtered ? 'No sessions match these filters.' : 'No sessions yet.',
      warned ? `${f.plural(warned, 'store')} could not be read — see the notice above. This list may be incomplete.` : null,
      filtered ? clear : 'Sessions appear here as soon as an agent CLI on this machine writes one.');
  }

  async function load(reset) {
    v.busy?.abort();
    v.renew = false; // a list loaded afresh, or read on into, is folded into from now on
    const c = new AbortController();
    v.busy = c;
    const signal = AbortSignal.any([ctx.signal, c.signal]);
    const q = v.route.query.get('q') || '';
    if (reset) {
      v.items = [];
      v.total = 0;
      v.end = false;
      saidEmpty = false;
      note.say();
      list.replaceChildren(skeleton(8, searching() ? 'hit' : 'row'));
      more.replaceChildren();
      renderHead();
    }
    try {
      if (searching() && !q.trim()) {
        v.end = true;
      } else if (searching()) {
        const r = await api('/api/search' + qs({ q, ...apiFilters(v.route.query), limit: HITS, offset: v.items.length }), { signal });
        v.items.push(...r.hits);
        v.total = r.total;
        v.sessions = r.sessions;
        v.end = r.hits.length < HITS;
      } else {
        const r = await api('/api/sessions' + qs({ ...apiFilters(v.route.query), agents: v.route.query.get('agents') === '1', limit: PAGE, offset: v.items.length }), { signal });
        const have = new Set(v.items.map((s) => s.ref));
        v.items.push(...r.sessions.filter((s) => !have.has(s.ref)));
        v.total = r.total;
        v.end = r.sessions.length < PAGE;
      }
      v.failed = false;
      renderList();
    } catch (e) {
      if (isAbort(e)) return;
      if (reset) v.failed = true;
      (reset ? list : more).replaceChildren(errorBox(e, () => load(reset)));
    } finally {
      if (v.busy === c) v.busy = null;
    }
  }

  // A change refetches the first page and folds it into what is shown: rows already
  // on screen are patched where they stand, so neither the scroll nor the selection moves.
  // It is folded only where it joins what is shown — its last row is one shown already,
  // unchanged — so that nothing can lie unseen between the two. Otherwise the first page
  // replaces the list: a list come back to by a link rather than by Back, where what was
  // kept below may no longer match ("today" was yesterday) or be gone; a page that is the
  // whole list; and one of rows all new, past which there may be more. An empty list is
  // asked again like any other, so that a session appears in it as soon as an agent
  // writes one; and one whose first load failed shows the new error, not two.
  async function refresh() {
    if (searching()) return;
    if (v.busy) return changes();
    const c = new AbortController();
    v.busy = c;
    try {
      const r = await api('/api/sessions' + qs({ ...apiFilters(v.route.query), agents: v.route.query.get('agents') === '1', limit: PAGE }), { signal: AbortSignal.any([ctx.signal, c.signal]) });
      const last = r.sessions.at(-1);
      const joins = r.sessions.length >= PAGE && v.items.some((s) => s.ref === last.ref && s.updatedAt === last.updatedAt);
      if (v.renew || !joins) {
        v.items = r.sessions;
        v.end = r.sessions.length < PAGE;
      } else {
        const fresh = new Set(r.sessions.map((s) => s.ref));
        const floor = f.when(last.updatedAt);
        v.items = [...r.sessions, ...v.items.filter((s) => !fresh.has(s.ref) && f.when(s.updatedAt) <= floor)];
      }
      v.renew = v.failed = false;
      v.total = r.total;
      note.say();
      renderList();
    } catch (e) {
      // What is drawn stays, and says it is from earlier; the next change asks again
      if (isAbort(e)) return;
      if (v.failed) {
        list.replaceChildren(errorBox(e, () => load(true)));
        more.replaceChildren();
      } else note.say(e, refresh);
    } finally {
      if (v.busy === c) v.busy = null;
    }
  }
  const changes = coalesce(refresh, 1000);

  function select(row, focus = true) {
    v.sel = row.dataset.key;
    for (const r of list.querySelectorAll('.row.selected')) r.classList.remove('selected');
    row.classList.add('selected');
    if (focus) {
      row.querySelector('a').focus({ preventScroll: true });
      row.scrollIntoView({ block: 'nearest' });
    }
  }

  function start(how) {
    renderFilters();
    note.say();
    const was = kept.get(keyOf(v.route));
    // A search is ranked, not dated, so what it found cannot be brought up to date row by
    // row: it comes back whole on Back, and is asked for afresh any other way
    if (was && (how === 'pop' || !searching())) {
      Object.assign(v, was, { busy: null, rows: new Map(), renew: how !== 'pop', failed: false });
      renderList();
      // Back returns to where the list was read; a link to it starts at its top
      scrollTo(0, how === 'pop' ? savedY() ?? 0 : 0);
      refresh();
    } else {
      scrollTo(0, 0);
      load(true);
    }
  }

  // remember keeps the list under the address it was shown at
  function remember() {
    v.busy?.abort();
    if (!v.items.length) return;
    const key = keyOf(v.route);
    kept.delete(key);
    kept.set(key, { items: v.items, total: v.total, sessions: v.sessions, end: v.end, sel: v.sel });
    if (kept.size > KEPT) kept.delete(kept.keys().next().value);
  }
  start(ctx.how);

  return {
    update(r, how) {
      if (r.name !== 'sessions' && r.name !== 'search') return false;
      const was = v.route;
      const moved = was.query.toString() !== r.query.toString() || was.name !== r.name;
      if (moved) remember();
      v.route = r;
      if (was.name !== r.name) v.rows.clear();
      if (moved) {
        aside.classList.remove('open');
        start(how);
      }
      return true;
    },
    changes,
    key(e) {
      const rows = [...list.querySelectorAll('.row')];
      if (!rows.length) return false;
      let i = rows.findIndex((r) => r.dataset.key === v.sel);
      if (e.key === 'j' || e.key === 'ArrowDown') i = Math.min(rows.length - 1, i + 1);
      else if (e.key === 'k' || e.key === 'ArrowUp') i = Math.max(0, i - 1);
      else if (e.key === 'Enter' && i >= 0 && !e.target.closest('a, button')) { go(rows[i].querySelector('a').href); return true; }
      else return false;
      select(rows[i]);
      if (i >= rows.length - 3 && !v.end && !v.busy) load(false);
      return true;
    },
    unmount() {
      io.disconnect();
      offStatus();
      changes.cancel();
      remember();
    },
  };
}

function sessionRow(s) {
  const title = s.title || s.lastAsk || 'Untitled session';
  const sub = s.title ? [s.lastAsk, s.answer] : [s.answer];
  const st = s.stats || {};
  return h('li', { class: 'row', dataset: { key: s.ref } },
    h('a', { class: 'row-main', href: sessionHref(s.ref) },
      mark(s.source, s.agent),
      h('span', { class: 'row-title' }, title),
      sub.some(Boolean) && h('span', { class: 'row-sub' }, sub.filter(Boolean).join('  →  ')),
      h('span', { class: 'row-facts dots' },
        h('span', { class: 'store-name' }, s.source),
        time(s.updatedAt),
        h('span', null, f.plural(st.turns, 'turn')),
        st.tools > 0 && h('span', null, f.plural(st.tools, 'tool call')),
        st.failures > 0 && h('span', { class: 'bad' }, `${f.num(st.failures)} failed`),
        s.agents > 0 && h('span', null, f.plural(s.agents, 'subagent')),
        s.parent && h('span', null, s.parent.type || 'subagent'),
        s.cwd && h('span', { class: 'path', title: s.cwd }, f.shortPath(s.cwd)),
        s.branch && h('span', { class: 'branch' }, s.branch),
        s.archived && h('span', null, 'archived'),
        s.active && h('span', { class: 'live', title: 'Written to in the last two minutes' }, 'live'))));
}

function hitRow(hit, words) {
  const s = hit.session;
  const kind = (m) => (m.kind === 'tool' ? [m.tool || 'tool', m.part].filter(Boolean).join(' ') : m.kind);
  // A match in the ask is already marked in the ask shown above it
  const matches = (hit.matches || []).filter((m) => !(hit.ask && m.kind === 'prompt'));
  return h('li', { class: 'row hit', dataset: { key: hit.ref } },
    h('a', { class: 'row-main', href: sessionHref(s.ref, hit.turn) },
      mark(s.source, s.agent),
      h('span', { class: 'row-title' }, s.title || 'Untitled session', h('span', { class: 'turn-no' }, ' #' + hit.turn)),
      hit.ask && h('span', { class: 'row-sub hit-ask' }, highlight(hit.ask, words)),
      h('span', { class: 'row-facts dots' },
        h('span', { class: 'store-name' }, s.source),
        time(hit.at),
        hit.tools > 0 && h('span', null, f.plural(hit.tools, 'tool call')),
        hit.failures > 0 && h('span', { class: 'bad' }, `${f.num(hit.failures)} failed`),
        s.cwd && h('span', { class: 'path', title: s.cwd }, f.shortPath(s.cwd)))),
    matches.length > 0 && h('ul', { class: 'matches' }, matches.map((m) =>
      h('li', null, h('a', { href: sessionHref(s.ref, `${hit.turn}.${m.seq}`) },
        h('span', { class: 'kind' }, kind(m)), h('span', { class: 'snippet' }, highlight(m.snippet, words)))))));
}
