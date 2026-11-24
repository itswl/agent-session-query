// The list of sessions, and the turns a search found. Both share the filters, which live
// in the address: a filtered view can be linked, and Back undoes a filter.

import { h, icon, mark, time, skeleton, errorBox, empty, fill, stale } from '../dom.js';
import { api, qs, sessionHref, isAbort, onForget } from '../api.js';
import { go, savedY } from '../router.js';
import { health, coalesce, onStatus, status } from '../live.js';
import { needles, highlight, formsIn } from '../md.js';
import * as f from '../format.js';
import { WINDOWS } from '../windows.js';
import * as seen from '../seen.js';
import * as projects from '../projects.js';
import { standing, NEEDS_YOU } from '../standing.js';

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
onForget(() => { kept.clear(); });

function apiFilters(q) {
  const since = q.get('since');
  return {
    source: q.get('source'), project: q.get('project'), branch: q.get('branch'),
    since: since === 'today' ? f.localDate() : since,
    archived: q.get('archived') === '1',
  };
}

// listFilters are the filters of the list alone, which a search does not take.
const listFilters = (q) => ({ agents: q.get('agents') === '1', state: q.get('state') });

// hrefWith is the current view's address with some parameters changed.
function hrefWith(route, changes) {
  const q = new URLSearchParams(route.query);
  for (const [k, v] of Object.entries(changes)) { if (v) q.set(k, v); else q.delete(k); }
  const s = q.toString();
  return (route.name === 'search' ? '/search' : '/') + (s ? '?' + s : '');
}

export function mount(main, route, ctx) {
  const v = { route, items: [], total: 0, sessions: 0, partial: null, missing: [], outside: null, end: false, sel: null, busy: null, rows: new Map(), renew: false, failed: false, base: 0 };
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

  // A row is selected by its own link; its branch is a filter, as one at the side is
  list.addEventListener('focusin', (e) => {
    const row = e.target.closest('.row');
    if (row && !e.target.matches('.branch')) select(row, false);
  });
  // A fact raised above a session's row link, so that its title shows on hover, opens the
  // session when clicked, as the rest of the row does
  list.addEventListener('click', (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.target.closest('a, button')) return;
    e.target.closest('.row')?.querySelector('.row-link')?.click();
  });
  const io = new IntersectionObserver((es) => { if (es.some((e) => e.isIntersecting) && !v.busy && !v.end && v.items.length) load(false); }, { rootMargin: '600px' });
  io.observe(more);
  // The filters list the stores health reports; they are drawn again when it changes. So
  // is a list that came back empty, which says when a store could not be read: the list
  // does not wait for the report, and is often answered first.
  // A list that came back empty says too, once nothing is being read, when no store held a
  // session at all: that is drawn again when a read starts or ends.
  let seenHealth = status.health;
  let seenReading = !!status.sync?.running;
  let saidEmpty = false;
  const offStatus = onStatus((st) => {
    const reading = !!st.sync?.running;
    const moved = reading !== seenReading;
    seenReading = reading;
    if (st.health && st.health !== seenHealth) {
      seenHealth = st.health;
      renderFilters();
    } else if (!moved) {
      return;
    }
    if (saidEmpty && !v.busy) renderList();
  });
  // A new visit marks the list against the last look of the one before
  const offVisit = seen.onVisit(() => { if (!v.busy && v.items.length) renderList(); });

  const searching = () => v.route.name === 'search';
  const words = () => needles(v.route.query.get('q') || '');

  function renderHead() {
    const q = v.route.query.get('q') || '';
    title.textContent = searching() ? 'Search' : 'Sessions';
    document.title = (searching() && q ? q + ' · ' : '') + title.textContent + ' · asq';
    count.textContent = !v.items.length && !v.total ? '' : searching()
      ? `${f.plural(v.total, 'turn')} in ${f.plural(v.sessions, 'session')}`
      : f.plural(v.total, 'session');
    const active = ['source', 'project', 'branch', 'since', 'archived', 'agents', 'state'].filter((k) => v.route.query.get(k)).length;
    toggle.lastChild.textContent = active ? `Filters (${active})` : 'Filters';
  }

  async function renderFilters() {
    const q = v.route.query;
    const hl = await health().catch(() => null);
    const dirs = await projects.list();
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
    const branch = q.get('branch');
    let shown = dirs.slice(0, TOP_PROJECTS);
    if (chosen && !shown.some((p) => p.cwd === chosen)) shown = [{ cwd: chosen }, ...shown];
    const rest = dirs.slice(TOP_PROJECTS).filter((p) => p.cwd !== chosen);
    const projectOpt = (p) => option('project', p.cwd, f.shortPath(p.cwd) || '(no directory)', { title: p.cwd, n: p.sessions });
    const restList = h('ul', { class: 'opts', hidden: true }, rest.map(projectOpt));
    const restBtn = rest.length > 0 && h('button', { type: 'button', class: 'link-btn', 'aria-expanded': 'false', onclick() {
      restList.hidden = !restList.hidden;
      this.setAttribute('aria-expanded', String(!restList.hidden));
      this.textContent = restList.hidden ? `${rest.length} more` : 'Fewer';
    } }, `${rest.length} more`);
    fill(aside,
      // Where each agent stands is the list's own: a search finds turns, not where they stand
      !searching() && h('section', null, h('h2', null, 'Agents'), h('ul', { class: 'opts seg' },
        option('state', '', 'All'),
        option('state', 'working', 'Working', { title: 'At work now, as their transcripts say: the model answering, or a result on its way back' }),
        option('state', NEEDS_YOU, 'Needs you', { title: 'Waiting on a call — a command still running, or a prompt asking permission — or done, and waiting for you' }))),
      h('section', null, h('h2', null, 'Store'), h('ul', { class: 'opts' },
        option('source', '', 'All stores'),
        stores.map((s) => option('source', s.source, [mark(s.source), s.title || s.source], { n: s.sessions, title: `${f.plural(s.sessions, 'session')}, ${f.plural(s.transcripts, 'transcript')} with subagents and archived ones · ${s.location}` })))),
      h('section', null, h('h2', null, 'Time'), h('ul', { class: 'opts seg' }, WHEN.map(([k, label]) => option('since', k, label)),
        // a window the address names that is none of these — an older link's "today", a
        // date — is shown as the one in force, until another is chosen
        oddSince && option('since', oddSince, oddSince === 'today' ? 'Today' : oddSince))),
      h('section', null, h('h2', null, 'Include'), h('ul', { class: 'opts' }, flag('archived', 'Archived'), !searching() && flag('agents', 'Subagent transcripts'))),
      dirs.length > 0 && h('section', null, h('h2', null, 'Project'), h('ul', { class: 'opts' }, option('project', '', 'All projects'), shown.map(projectOpt)), restList, restBtn),
      // A branch is chosen on a row; here it is said, and taken off
      branch && h('section', null, h('h2', null, 'Branch'), h('ul', { class: 'opts' }, option('branch', '', 'All branches'), option('branch', branch, h('span', { class: 'branch' }, branch)))),
    );
  }

  function rowOf(item) {
    const key = item.ref;
    const sig = JSON.stringify(item);
    const had = v.rows.get(key);
    if (had?.sig === sig) return had.el;
    const el = searching() ? hitRow(item, words()) : sessionRow(item, v.base, v.route);
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
      list.replaceChildren(v.partial ? partialNote(v) : '', h('ol', { class: 'rows' }, v.items.map(rowOf)));
    } else {
      // What changed since the last look is marked, and a line runs above the first row
      // older than it: at the foot of its day when that row begins the next
      const base = seen.since('sessions');
      if (base !== v.base) { v.base = base; v.rows.clear(); }
      const days = [];
      let lined = !base;
      for (const s of v.items) {
        const at = f.when(s.updatedAt);
        if (!lined && at <= base) {
          lined = true;
          days.at(-1)?.rows.push(seenLine(base, () => { seen.markSeen('sessions'); renderList(); }));
        }
        const label = f.day(at || Date.now());
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
    const filtered = ['source', 'project', 'branch', 'since', 'state'].some((k) => v.route.query.get(k));
    const warned = (status.health?.warnings || []).length;
    const clear = filtered && h('a', { href: hrefWith(v.route, { source: '', project: '', branch: '', since: '', state: '' }) }, 'Clear the filters');
    const none = !filtered && nothingFound();
    if (searching() && !q.trim()) {
      return empty('Search every turn of every session.', 'Type in the search box above. Words match what they begin, and other forms of an English word: "deploy" finds "deployment", "retries" finds "retried".',
        'Use "a phrase" in quotes, -word to exclude, and ask: reply: thinking: tool: output: title: to search one part of a turn.');
    }
    if (none) return none;
    if (searching() && v.outside) {
      // The filters, or archived sessions being left out, are what kept the search from
      // matching: said, and undone in one link
      const o = v.outside;
      const show = o.turns > 0 && h('a', { href: hrefWith(v.route, { source: '', project: '', branch: '', since: '', archived: o.archived ? '1' : v.route.query.get('archived') }) },
        o.filters?.length ? (o.archived ? 'Clear the filters and include archived sessions' : 'Clear the filters') : 'Include archived sessions');
      return empty(`${noTurn(o)} ${v.missing.length ? `holds ${quoted(v.missing)}` : `matches “${q}”`}.`, beyond(o), show || clear);
    }
    if (searching() && v.missing.length) {
      return empty(`No turn holds ${quoted(v.missing)}.`, 'Check the spelling, or try another word for it.', clear);
    }
    if (searching()) {
      return empty(`No turns match “${q}”.`, 'Try fewer words, a shorter word (words match what they begin), or drop a column prefix.', clear);
    }
    const state = v.route.query.get('state');
    if (state) {
      return empty(state === NEEDS_YOU ? 'Nothing is waiting on you.' : 'No agent is at work.',
        'Where an agent stands is said for Claude Code, Codex, Gemini CLI, OpenCode and DeepSeek Harness, by what their transcripts say: at work for ten minutes after they were last written, waiting or done for a day.', clear);
    }
    return empty(filtered ? 'No sessions match these filters.' : 'No sessions yet.',
      warned ? `${f.plural(warned, 'store')} could not be read — see the notice above. This list may be incomplete.` : null,
      filtered ? clear : 'Sessions appear here as soon as an agent CLI on this machine writes one.');
  }

  // nothingFound says where asq looked, when no store it reads held a session and nothing
  // is being read: a service started without the variable that moves a CLI's store, or a
  // container with one mounted in the wrong place, finds none, and "No sessions yet" would
  // be all it said.
  function nothingFound() {
    const stores = f.looked(status.health, status.sync);
    if (!stores) return null;
    return h('div', { class: 'empty' },
      h('p', { class: 'empty-title' }, 'No CLI\'s sessions were found.'),
      h('p', null, 'asq looked in these places, on the machine it runs on:'),
      h('ul', { class: 'looked' }, stores.map((s) => h('li', null, h('span', { class: 'mono' }, s.location), ` — ${s.title || s.source}, ${s.state}`))),
      h('p', null, h('code', null, 'asq doctor'), ' on that machine says more. A store somewhere else is named with ', h('code', null, 'ASQ_PATH'),
        '. A service reads the variables that move a store (', h('code', null, 'CLAUDE_CONFIG_DIR'), ', ', h('code', null, 'CODEX_HOME'), ', …) from its own environment, not from a shell\'s.'));
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
      v.partial = null;
      v.missing = [];
      v.outside = null;
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
        v.partial = r.partial || null;
        v.missing = r.missing || [];
        v.outside = r.outside || null;
        v.end = r.hits.length < HITS;
      } else {
        const r = await api('/api/sessions' + qs({ ...apiFilters(v.route.query), ...listFilters(v.route.query), limit: PAGE, offset: v.items.length }), { signal });
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
      const r = await api('/api/sessions' + qs({ ...apiFilters(v.route.query), ...listFilters(v.route.query), limit: PAGE }), { signal: AbortSignal.any([ctx.signal, c.signal]) });
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
    // A search is not a look at what changed: it is ranked, not dated, and is not marked
    if (searching()) seen.leave('sessions');
    else seen.look('sessions');
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
    kept.set(key, { items: v.items, total: v.total, sessions: v.sessions, partial: v.partial, missing: v.missing, outside: v.outside, end: v.end, sel: v.sel });
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
      // a row links to its branch in the list as it is filtered
      if (moved) v.rows.clear();
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
      offVisit();
      seen.leave('sessions');
      changes.cancel();
      remember();
    },
  };
}

// A session's row opens it from anywhere on the row, by its title's link stretched over
// the whole; its branch is a link of its own above that, to the list on that branch.
function sessionRow(s, base, route) {
  const title = s.title || s.lastAsk || 'Untitled session';
  // A session its CLI did not name is titled by how its first ask begins: when that is its
  // last ask too, the line beneath would say it again, and the answer, after it, would not
  // fit on a phone
  const sub = s.title && !f.repeats(s.title, s.lastAsk) ? [s.lastAsk, s.answer] : [s.answer];
  const st = s.stats || {};
  const onBranch = s.branch && route.query.get('branch') === s.branch;
  return h('li', { class: 'row', dataset: { key: s.ref } },
    h('div', { class: 'row-main' },
      mark(s.source, s.agent),
      h('a', { class: 'row-title row-link', href: sessionHref(s.ref) }, title),
      sub.some(Boolean) && h('span', { class: 'row-sub' }, sub.filter(Boolean).join('  →  ')),
      h('span', { class: 'row-facts dots' },
        standing(s),
        h('span', { class: 'store-name' }, s.source),
        time(s.updatedAt),
        fresh(s, base),
        h('span', null, f.plural(st.turns, 'turn')),
        st.tools > 0 && h('span', null, f.plural(st.tools, 'tool call')),
        st.failures > 0 && h('span', { class: f.open(st) ? 'bad' : null, title: f.passedTitle }, f.failed(st)),
        s.agents > 0 && h('span', null, f.plural(s.agents, 'subagent')),
        s.parent && h('span', null, s.parent.type || 'subagent'),
        s.cwd && h('span', { class: 'path', title: s.cwd }, f.shortPath(s.cwd)),
        s.branch && h('a', { class: 'branch', href: hrefWith(route, { branch: onBranch ? '' : s.branch }), 'aria-current': onBranch ? 'true' : null,
          title: onBranch ? 'All branches' : `Only the sessions on ${s.branch}` }, s.branch),
        s.archived && h('span', null, 'archived'))));
}

// fresh marks a session written to since the last look: new if it began since, updated
// if it began before.
function fresh(s, base) {
  if (!base || !(f.when(s.updatedAt) > base)) return null;
  const began = f.when(s.startedAt) > base;
  return h('span', { class: 'fresh', title: `${began ? 'Begun' : 'Written to'} since you last looked, ${f.stamp(base)}` }, began ? 'new' : 'updated');
}

// The line between what changed since the last visit and the rest, and the way to say it
// has been seen: the marks are against now from then on
const seenLine = (base, markSeen) =>
  h('li', { class: 'seen-line', role: 'separator', 'aria-label': `Changed since your last visit, ${f.stamp(base)}` },
    h('span', { class: 'dots' }, h('span', null, 'Changed since your last visit'), time(base, 'stamp')),
    h('button', { type: 'button', class: 'link-btn', onclick: markSeen }, 'Mark as seen'));

const quoted = (words, or = 'or') => words.map((w) => `“${w}”`).join(words.length === 2 ? ` ${or} ` : ', ');

// partialNote says that no turn holds every word of a search, and what the turns listed
// are instead: the closest, each with the words it lacks.
const partialNote = (v) => h('p', { class: 'notice warn partial-note', role: 'note' },
  `${noTurn(v.outside)} holds all ${v.partial.words} words${v.missing.length ? `; none holds ${quoted(v.missing)}` : ''}. ` +
  (v.outside ? beyond(v.outside) + ' ' : '') +
  `These are partial matches, closest first: the closest hold ${v.partial.held} of the ${v.partial.words}.`);

// noTurn begins what a search found none of: of the turns the filters select, when they
// left out turns that match it, or of those outside archived sessions.
const noTurn = (o) => (!o ? 'No turn' : o.filters?.length ? 'No turn these filters select' : 'No turn outside archived sessions');

// beyond says how many turns would match a search without the filters, or with archived
// sessions included — or, when none would, which of its words turns outside them hold.
const beyond = (o) => (o.turns ? `${f.plural(o.turns, 'turn')} in ${f.plural(o.sessions, o.archived ? 'archived session' : 'session')}${o.filters?.length ? ' outside them' : ''} ${o.turns === 1 ? 'matches' : 'match'}.`
  : o.held?.length ? `None outside them matches either, though some hold ${quoted(o.held, 'and')}.`
    : 'Nor does any outside them.');

function hitRow(hit, query) {
  const s = hit.session;
  const words = formsIn(query, hit);
  const kind = (m) => (m.kind === 'tool' ? [m.tool || 'tool', m.part].filter(Boolean).join(' ') : m.kind);
  // A match in the ask is already marked in the ask shown above it, and one in the title in
  // the title
  const matches = (hit.matches || []).filter((m) => !(hit.ask && m.kind === 'prompt') && m.kind !== 'title');
  return h('li', { class: 'row hit', dataset: { key: hit.ref } },
    h('a', { class: 'row-main', href: sessionHref(s.ref, hit.turn) },
      mark(s.source, s.agent),
      h('span', { class: 'row-title' }, s.title ? highlight(s.title, words) : 'Untitled session', h('span', { class: 'turn-no' }, ' #' + hit.turn)),
      hit.ask && h('span', { class: 'row-sub hit-ask' }, highlight(hit.ask, words)),
      h('span', { class: 'row-facts dots' },
        hit.lacks?.length > 0 && h('span', { class: 'lacks', title: 'A partial match: this turn does not hold these words' }, 'lacks ' + hit.lacks.join(' ')),
        h('span', { class: 'store-name' }, s.source),
        time(hit.at),
        hit.tools > 0 && h('span', null, f.plural(hit.tools, 'tool call')),
        hit.failures > 0 && h('span', { class: f.open(hit) ? 'bad' : null, title: f.passedTitle }, f.failed(hit)),
        hit.interrupted && h('span', { class: 'lacks' }, 'interrupted'),
        s.cwd && h('span', { class: 'path', title: s.cwd }, f.shortPath(s.cwd)))),
    matches.length > 0 && h('ul', { class: 'matches' }, matches.map((m) =>
      h('li', null, h('a', { href: sessionHref(s.ref, `${hit.turn}.${m.seq}`) },
        h('span', { class: 'kind' }, kind(m)), h('span', { class: 'snippet' }, highlight(m.snippet, words)))))));
}
