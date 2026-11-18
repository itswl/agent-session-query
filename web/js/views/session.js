// One session as a timeline of turns. Only the outline is fetched up front — a session can
// be megabytes — and a turn's steps are fetched when it is opened.

import { h, icon, mark, time, copyButton, clamp, skeleton, errorBox, empty, fact, dropdown, fill, reducedMotion } from '../dom.js';
import { api, qs, sessionApi, sessionHref, isAbort } from '../api.js';
import { go, savedY, back } from '../router.js';
import { coalesce } from '../live.js';
import { md, needles, highlight, formsIn } from '../md.js';
import { steps, text } from './steps.js';
import * as f from '../format.js';

const titleOf = (s) => s.title || f.firstLine(s.lastAsk, 120) || 'Untitled session';
const sigOf = (t) => [t.endedAt, t.lastAt, t.steps, t.tools, t.failures, t.interrupted, t.ask?.length, t.answer?.length, f.totalTokens(t.usage)].join('|');
const atBottom = () => innerHeight + scrollY >= document.documentElement.scrollHeight - 120;
const behavior = () => (reducedMotion() ? 'auto' : 'smooth');
// A phone: the bar scrolls away, and the session keeps a row of its own at the top
const narrow = matchMedia('(max-width: 720px)');
const fine = matchMedia('(pointer: fine)');
const FOUND = 100; // the turns a find asks for at a time: the most a search answers
const kindOf = (m) => (m.kind === 'tool' ? [m.tool || 'tool', m.part].filter(Boolean).join(' ') : m.kind);

const MODIFIERS = new Set(['Shift', 'Control', 'Alt', 'Meta', 'CapsLock', 'AltGraph']);

export function mount(main, route, ctx) {
  const ref = route.args.join('/');
  const base = sessionApi(ref);
  const v = { route, sess: null, agents: [], above: [], recs: [], byIndex: new Map(), cur: -1, follow: true, live: false, sheet: null, lastG: 0, count: '', countAt: 0, found: null, dockH: 0 };

  const crumbs = h('nav', { class: 'crumbs', 'aria-label': 'Where this session ran' });
  const h1 = h('h1', { tabIndex: -1 }, ' ');
  // On a phone the head is the title and the first facts, and the rest is behind Details
  const info = h('div', { class: 's-info folded' }, skeleton(1, 'head'));
  // Parts drawn apart, so that a live update redraws the figures without closing an open menu
  const facts = h('div');
  const details = h('div', { class: 's-details', id: 's-details' });
  const actions = h('div', { class: 'actions' });
  const extra = h('div', { class: 's-extra' });
  const notice = h('div');
  const timeline = h('ol', { class: 'timeline', 'aria-label': 'Turns' });
  const pill = h('button', { type: 'button', class: 'follow', hidden: true, 'aria-pressed': 'true' });
  pill.addEventListener('click', () => {
    v.follow = pill.dataset.mode !== 'on';
    if (v.follow) scrollTo({ top: document.documentElement.scrollHeight, behavior: behavior() });
    renderPill();
  });

  // The row that stays at the top on a phone: the way back, the turn in view (a number
  // typed there goes to that turn), the next failed turn, find, the top and the newest
  const where = h('input', { type: 'text', inputMode: 'numeric', enterKeyHint: 'go', autocomplete: 'off', spellcheck: false,
    'aria-label': 'The turn in view; type a number to go to that turn', title: 'The turn in view — type a number to go to that turn' });
  const whereOf = h('span', { class: 'where-of' });
  const failBtn = h('button', { type: 'button', class: 'icon-btn fail-next', hidden: true, 'aria-label': 'The next failed turn', title: 'The next failed turn (x)', onclick: () => toFailed(1) }, icon('alert'));
  const findBtn = h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Find in this session', title: 'Find in this session (f)', 'aria-expanded': 'false', 'aria-controls': 's-find', onclick: () => openFind() }, icon('search'));
  const nav = h('nav', { class: 's-nav', 'aria-label': 'This session' },
    h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Back', title: 'Back to the list or report (Esc)', onclick: back }, icon('back')),
    h('label', { class: 'where' }, '#', where, whereOf),
    h('span', { class: 's-nav-end' }, failBtn, findBtn,
      h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'To the top', title: 'To the top (g g)', onclick: () => toTop() }, icon('up')),
      h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'To the newest turn', title: 'To the newest turn', onclick: () => v.recs.length && jump(v.recs.length - 1) }, icon('down'))));
  where.addEventListener('focus', () => { v.whereWas = where.value; where.select(); });
  where.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); where.blur(); }
    if (e.key === 'Escape') { e.preventDefault(); where.value = v.whereWas; where.blur(); }
  });
  // A phone's number pad may have no key to send it: putting the keyboard away does
  where.addEventListener('change', () => {
    const n = parseInt(where.value, 10);
    if (n >= 0) toTurn(n);
  });
  where.addEventListener('blur', () => renderWhere());

  // Find in this session. The server searches every turn, opened or not — the browser's
  // own find sees only the steps of the turns that are open — and the turns it finds are
  // listed in the order of the session, each opening at the step the words are in.
  const findQ = h('input', { type: 'search', placeholder: 'Find in this session', 'aria-label': 'Find in this session', autocomplete: 'off', spellcheck: false, enterKeyHint: 'search' });
  const findSaid = h('span', { class: 'find-said', role: 'status' });
  const findPrev = h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'The turn found before', title: 'The turn found before (Shift+Enter)', onclick: () => stepFound(-1) }, icon('prev'));
  const findNext = h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'The next turn found', title: 'The next turn found (Enter)', onclick: () => stepFound(1) }, icon('next'));
  const findBar = h('form', { class: 'find-bar', role: 'search', onsubmit(e) { e.preventDefault(); } }, icon('search'), findQ, findSaid, findPrev, findNext,
    h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Close the find', title: 'Close (Esc)', onclick: () => closeFind() }, icon('x')));
  const findList = h('div', { class: 'find-list', hidden: true });
  const finder = h('div', { class: 's-find', id: 's-find', hidden: true }, findBar, findList);
  const dock = h('div', { class: 's-dock' }, nav, finder);
  let typing = 0;
  findQ.addEventListener('input', () => { clearTimeout(typing); typing = setTimeout(runFind, 250); });
  // The list comes back when the box is gone back to, or clicked while it has the focus
  for (const ev of ['focus', 'click']) findQ.addEventListener(ev, () => { if (v.found) showList(true); });
  findQ.addEventListener('keydown', async (e) => {
    if (e.key === 'Escape') { e.preventDefault(); closeFind(); }
    if (e.key !== 'Enter') return;
    e.preventDefault();
    if (v.found?.q !== findQ.value.trim()) await runFind();
    stepFound(e.shiftKey ? -1 : 1, true);
  });
  findList.addEventListener('click', (e) => {
    const li = e.target.closest('a[href]')?.closest('[data-i]');
    if (!li || !v.found) return;
    // The router follows the link; the list folds away so that what it opens is seen
    v.found.at = +li.dataset.i;
    showList(false);
    renderFound();
  });
  const outside = (e) => { if (!finder.contains(e.target)) showList(false); };

  const article = h('article', { class: 'session' }, dock, h('header', { class: 's-head' }, crumbs, h1, info), notice, timeline, pill);
  main.append(article);
  // A turn or a step gone to lands clear of what stays at the top
  const sized = new ResizeObserver(() => { syncDock(); renderWhere(); });
  sized.observe(nav);

  async function load() {
    timeline.replaceChildren(skeleton(4, 'turn'));
    try {
      const d = await api(base, { signal: ctx.signal });
      v.sess = d.session;
      v.agents = d.agents || [];
      v.above = d.above || [];
      renderHead();
      renderCrumbs();
      timeline.replaceChildren();
      for (const t of d.turns) addTurn(t);
      if (!d.turns.length) timeline.append(empty('This session holds no turns yet.'));
      renderPill();
      renderNav();
      await reach(v.route.hash, ctx.how === 'pop' ? savedY() : null);
      renderWhere();
    } catch (e) {
      if (isAbort(e)) return;
      h1.textContent = e.status === 404 ? 'No such session' : 'This session could not be opened';
      info.replaceChildren();
      timeline.replaceChildren(errorBox(e, e.status === 404 ? null : load));
    }
  }

  function renderHead() {
    const s = v.sess;
    const st = s.stats || {};
    const u = st.usage || {};
    document.title = titleOf(s) + ' · asq';
    h1.textContent = titleOf(s);
    if (!actions.childElementCount) {
      info.replaceChildren(facts, details, actions, extra);
      actions.append(
        h('button', { type: 'button', class: 'btn details-btn', 'aria-expanded': 'false', 'aria-controls': details.id, onclick(e) {
          const open = info.classList.toggle('folded') === false;
          e.currentTarget.setAttribute('aria-expanded', String(open));
        } }, icon('chevron', 'chev'), 'Details'),
        h('button', { type: 'button', class: 'btn', onclick: (e) => openBrief(e.currentTarget) }, icon('doc'), 'Brief'),
        dropdown(
          h('button', { type: 'button', class: 'btn', 'aria-haspopup': 'true' }, icon('download'), 'Export'),
          h('div', { class: 'menu' }, [['md', 'Markdown'], ['json', 'JSON'], ['jsonl', 'JSON Lines'], ['html', 'HTML page']].map(([fmt, label]) =>
            h('a', { href: `${base}/export${qs({ format: fmt })}`, download: '' }, label, h('span', { class: 'dim' }, '.' + fmt))))),
        h('button', { type: 'button', class: 'btn find-btn', 'aria-expanded': 'false', 'aria-controls': finder.id, title: 'Find in this session (f)', onclick: () => openFind() }, icon('search'), 'Find'));
    }
    fill(facts,
      h('div', { class: 'meta dots' },
        h('span', { class: 'store' }, mark(s.source, s.agent), s.source),
        h('span', { class: 'ref' }, h('code', null, s.ref), copyButton(s.ref, 'Copy reference')),
        s.cwd && h('a', { class: 'path', href: '/' + qs({ project: s.cwd }), title: s.cwd }, f.shortPath(s.cwd)),
        s.branch && h('span', { class: 'branch' }, s.branch),
        s.model && h('span', null, s.model),
        s.version && h('span', { class: 'dim' }, 'v' + s.version),
        s.archived && h('span', null, 'archived'),
        s.active && h('span', { class: 'live' }, 'live')));
    fill(details,
      h('div', { class: 'meta when' }, time(s.startedAt, 'stamp'), ' → ', time(s.updatedAt, 'stamp'), ' ', h('span', { class: 'dim' }, '(', time(s.updatedAt), ')')),
      h('dl', { class: 'totals' },
        fact('Turns', f.num(st.turns)),
        fact('Tool calls', f.num(st.tools)),
        fact('Failed', f.num(st.failures), { class: st.failures ? 'fact bad' : 'fact' }),
        fact('Tokens in', f.tokens(f.promptTokens(u)), { title: `${f.num(f.promptTokens(u))} prompt tokens: ${f.num(u.input)} new, ${f.num(u.cacheRead)} read from cache, ${f.num(u.cacheWrite)} written to cache` }),
        fact('Tokens out', f.tokens(u.output), { title: `${f.num(u.output)} output tokens${u.reasoning ? `, ${f.num(u.reasoning)} of them reasoning` : ''}` }),
        fact('Cache hit', f.pct(f.cacheHit(u))),
        st.costUsd > 0 && fact('Cost', f.cost(st.costUsd), { title: `$${st.costUsd}` })),
      s.resume && h('div', { class: 'resume' }, h('code', null, s.resume), copyButton(s.resume, 'Copy the resume command')));
    fill(extra,
      s.error && h('p', { class: 'notice warn' }, icon('alert'), h('span', null, 'This transcript could not be read whole: ', s.error)),
      v.agents.length > 0 && agentsList(),
    );
  }

  function agentsList() {
    const rows = v.agents.map((a) => h('li', null, h('a', { href: sessionHref(a.ref) },
      h('span', { class: 'agent-type' }, a.parent?.type || 'subagent'),
      h('span', { class: 'agent-title' }, titleOf(a)),
      h('span', { class: 'dim dots' }, h('span', null, f.plural(a.stats?.turns || 0, 'turn')), h('span', null, f.plural(a.stats?.tools || 0, 'tool call')),
        a.stats?.failures > 0 && h('span', { class: 'bad' }, `${a.stats.failures} failed`), a.agents > 0 && h('span', null, f.plural(a.agents, 'subagent'))))));
    const list = h('ul', { class: 'agents' }, rows);
    const many = rows.length > 6;
    if (many) list.classList.add('folded');
    return h('section', { class: 's-agents' }, h('h2', null, `Subagents (${rows.length})`), list,
      many && h('button', { type: 'button', class: 'link-btn', onclick() { this.textContent = list.classList.toggle('folded') ? `Show all ${rows.length}` : 'Show fewer'; } }, `Show all ${rows.length}`));
  }

  // A subagent's transcript sits beneath the session it ran in — and, for an agent
  // another agent started, beneath the agents between
  function renderCrumbs() {
    if (!v.above.length) return crumbs.replaceChildren();
    const links = v.above.map((p) => h('a', { href: sessionHref(p.ref), title: p.ref }, titleOf(p)));
    fill(crumbs, h('span', { class: 'dim' }, 'Subagent of '), links.flatMap((a, i) => (i ? [' › ', a] : [a])));
  }

  function addTurn(t) {
    const prev = v.recs.at(-1);
    const at = f.when(t.startedAt);
    if (at && (!prev?.at || !f.sameDay(prev.at, at))) timeline.append(h('li', { class: 't-day' }, f.day(at)));
    const rec = { t, sig: sigOf(t), at, open: false, full: null, el: h('li', { class: 'turn', id: 't' + t.index, tabIndex: -1 }) };
    rec.stepsEl = h('div', { class: 'steps', id: 'steps-' + t.index, hidden: true });
    v.recs.push(rec);
    v.byIndex.set(t.index, rec);
    fillTurn(rec);
    timeline.append(rec.el);
  }

  const toggleLabel = (rec) => (rec.open ? 'Hide steps' : rec.t.steps ? f.plural(rec.t.steps, 'step') : rec.t.index === 0 ? 'Show' : 'Steps');

  function fillTurn(rec) {
    const t = rec.t;
    const zero = t.index === 0;
    const [status, said] = t.failures ? ['failed', f.plural(t.failures, 'failed call')] : t.interrupted ? ['interrupted', 'interrupted'] : ['fine', 'fine'];
    const took = f.when(t.endedAt) - f.when(t.startedAt);
    rec.toggle = h('button', { type: 'button', class: 't-toggle', 'aria-expanded': String(rec.open), 'aria-controls': rec.stepsEl.id },
      icon('chevron', 'chev'), h('span', null, toggleLabel(rec)));
    rec.toggle.addEventListener('click', () => setOpen(rec, !rec.open));
    rec.askEl = !zero && t.ask ? h('div', { class: 't-ask' }, clamp(md(t.ask), t.ask, 12)) : null;
    rec.answerEl = t.answer ? h('div', { class: 't-answer', hidden: rec.open }, clamp(md(t.answer), t.answer, 20)) : null;
    const kinds = Object.entries(t.toolKinds || {}).sort((a, b) => b[1] - a[1]);
    const changed = (t.files || []).filter((x) => x.changed);
    const tok = f.totalTokens(t.usage);
    rec.el.className = ['turn', zero && 'zero', rec.open && 'open', v.cur >= 0 && v.recs[v.cur] === rec && 'current'].filter(Boolean).join(' ');
    rec.el.dataset.status = status;
    fill(rec.el,
      h('div', { class: 't-head' },
        h('a', { class: 't-num', href: '#' + t.index, title: 'Link to this turn' }, '#' + t.index),
        zero ? h('span', { class: 't-zero' }, 'Before the first ask') : time(t.startedAt, 'clock'),
        took > 0 && h('span', { class: 't-dur', title: 'From the ask to the last thing the agent did' }, f.dur(took)),
        h('span', { class: 't-status', dataset: { status } }, status === 'fine' ? h('span', { class: 'sr' }, said) : said),
        rec.toggle),
      rec.askEl,
      (kinds.length || changed.length || tok) ? h('div', { class: 't-facts dots' },
        kinds.map(([k, n]) => h('span', { title: f.plural(n, `${k} call`) }, `${k} ${n}`)),
        changed.length > 0 && h('span', { title: changed.map((x) => x.path).join('\n') }, `${f.plural(changed.length, 'file')} changed`),
        tok > 0 && h('span', { title: `${f.num(f.promptTokens(t.usage))} in · ${f.num(t.usage?.output)} out` }, `${f.tokens(tok)} tokens`),
        t.costUsd > 0 && h('span', null, f.cost(t.costUsd))) : null,
      rec.answerEl,
      rec.stepsEl);
  }

  async function setOpen(rec, open) {
    rec.open = open;
    rec.el.classList.toggle('open', open);
    rec.toggle.setAttribute('aria-expanded', String(open));
    rec.toggle.lastChild.textContent = toggleLabel(rec);
    rec.stepsEl.hidden = !open;
    if (rec.answerEl) rec.answerEl.hidden = open;
    if (!open) return;
    if (rec.full) renderSteps(rec);
    else await loadSteps(rec);
  }

  async function loadSteps(rec) {
    rec.ctrl?.abort();
    const c = (rec.ctrl = new AbortController());
    if (!rec.stepsEl.childElementCount) rec.stepsEl.replaceChildren(skeleton(3, 'step'));
    try {
      const d = await api(`${base}/turns${qs({ turns: rec.t.index })}`, { signal: AbortSignal.any([ctx.signal, c.signal]) });
      rec.full = d.turns[0] || { index: rec.t.index, items: [] };
      if (rec.open) renderSteps(rec);
    } catch (e) {
      if (!isAbort(e)) rec.stepsEl.replaceChildren(errorBox(e, () => loadSteps(rec)));
    }
  }

  function renderSteps(rec) {
    // What was open stays open when the steps are drawn again after a change
    const opened = new Set([...rec.stepsEl.querySelectorAll('.step > .disclosure > .disc[aria-expanded=true]')].map((b) => b.closest('.step').dataset.seq));
    const { nodes, ask } = steps(rec.full, { base, turn: rec.t.index, agents: v.agents, signal: ctx.signal });
    rec.stepsEl.replaceChildren(...(nodes.length ? nodes : [h('p', { class: 'dim' }, 'No steps were recorded in this turn.')]));
    for (const seq of opened) rec.stepsEl.querySelector(`#q${seq} > .disclosure > .disc[aria-expanded=false]`)?.click();
    // The outline carries the start of a long ask; the opened turn has all of it
    if (ask && rec.askEl && (ask.bytes || (ask.text || '').length > (rec.t.ask || '').length)) {
      rec.askEl.replaceChildren(clamp(text(ask, { base, signal: ctx.signal }, true), ask.text || '', 12));
    }
  }

  function flash(el) {
    el.classList.remove('flash');
    void el.offsetWidth; // restart the animation
    el.classList.add('flash');
  }

  // reach brings a turn (#4) or a step (#4.31) into view, opening what holds it
  async function reach(hash, y) {
    const hold = v.holdFocus;
    v.holdFocus = false;
    const m = /^(\d+)(?:\.(\d+))?$/.exec(hash || '');
    const rec = m && v.byIndex.get(+m[1]);
    if (!rec) {
      scrollTo(0, y ?? 0);
      return;
    }
    setCur(v.recs.indexOf(rec), false);
    if (!rec.open || !rec.full) await setOpen(rec, true);
    let el = null;
    if (m[2] != null) {
      const seq = +m[2];
      el = rec.stepsEl.querySelector('#q' + seq);
      if (!el) {
        const group = [...rec.stepsEl.querySelectorAll('.copied')].find((g) => +g.dataset.from <= seq && seq <= +g.dataset.to);
        group?.querySelector('.disc[aria-expanded=false]')?.click();
        el = rec.stepsEl.querySelector('#q' + seq);
      }
      el?.querySelector(':scope > .disclosure > .disc[aria-expanded=false]')?.click();
    }
    const target = el || rec.el;
    if (y != null) scrollTo(0, y);
    else target.scrollIntoView({ block: el ? 'center' : 'start' });
    if (!hold) target.focus({ preventScroll: true });
    flash(target);
  }

  function setCur(i, scroll = true, how = behavior()) {
    v.recs[v.cur]?.el.classList.remove('current');
    v.cur = i;
    const el = v.recs[i].el;
    el.classList.add('current');
    if (scroll) {
      el.scrollIntoView({ block: 'start', behavior: how });
      el.focus({ preventScroll: true });
    }
  }

  // The line a turn is in view below: under the bar, and under what stays at the top of
  // the session with it
  const line = () => Math.max(60, dock.getBoundingClientRect().top + v.dockH + 8);

  // The turn a key acts on: the one moved to last, while it is on screen; else the first in
  // view. The turns stand in order down the page, so the first in view is looked for by halves.
  function current() {
    const y = line();
    const r = v.recs[v.cur]?.el.getBoundingClientRect();
    if (r && r.bottom > y && r.top < innerHeight) return v.cur;
    let lo = 0;
    let hi = v.recs.length - 1;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (v.recs[mid].el.getBoundingClientRect().bottom > y) hi = mid;
      else lo = mid + 1;
    }
    return lo;
  }

  function move(d) {
    if (!v.recs.length) return;
    const i = current();
    const onIt = v.cur === i;
    setCur(Math.min(v.recs.length - 1, Math.max(0, onIt ? i + d : i)));
  }

  // jump goes to a turn further off than the next, and marks where it landed. Far off, it
  // goes at once: gliding over a long session takes seconds and shows nothing worth seeing.
  function jump(i) {
    setCur(i, true, Math.abs(v.recs[i].el.getBoundingClientRect().top) > 2 * innerHeight ? 'auto' : behavior());
    flash(v.recs[i].el);
  }

  function toTop() {
    scrollTo({ top: 0, behavior: scrollY > 2 * innerHeight ? 'auto' : behavior() });
  }

  // toTurn goes to turn n, or to the first after it when there is no turn n
  function toTurn(n) {
    if (!v.recs.length) return;
    const i = v.recs.findIndex((r) => r.t.index >= n);
    jump(i < 0 ? v.recs.length - 1 : i);
  }

  // toFailed goes to the next failed turn after the one in view, or before it, round from
  // the end to the start
  function toFailed(d) {
    const n = v.recs.length;
    const from = current();
    for (let k = 1; k <= n; k++) {
      const i = (((from + d * k) % n) + n) % n;
      if (v.recs[i].t.failures > 0) return jump(i);
    }
  }

  function renderNav() {
    whereOf.textContent = v.recs.length ? '/ ' + v.recs.at(-1).t.index : '';
    failBtn.hidden = !v.recs.some((r) => r.t.failures > 0);
    renderWhere();
  }

  // The row's number follows the turn in view, except while one is being typed in it
  function renderWhere() {
    if (!narrow.matches || !v.recs.length || document.activeElement === where) return;
    const n = String(v.recs[current()].t.index);
    if (where.value !== n) where.value = n;
  }

  // syncDock says how much stays at the top, so that a turn or a step gone to lands below it
  function syncDock() {
    const top = dock.getBoundingClientRect().top;
    const bottom = finder.hidden ? dock.getBoundingClientRect().bottom : findBar.getBoundingClientRect().bottom + 8;
    v.dockH = Math.round(bottom - top);
    article.style.setProperty('--dock-h', v.dockH + 'px');
  }

  // A find opened again keeps its words and asks afresh: the session may have grown since
  function openFind() {
    const was = finder.hidden;
    if (was) {
      v.findFrom = document.activeElement;
      finder.hidden = false;
      for (const b of [findBtn, actions.querySelector('.find-btn')]) b?.setAttribute('aria-expanded', 'true');
      syncDock();
    }
    findQ.focus();
    findQ.select();
    if (was && findQ.value.trim()) runFind();
  }

  function closeFind() {
    clearTimeout(typing);
    v.findCtrl?.abort();
    showList(false);
    finder.hidden = true;
    for (const b of [findBtn, actions.querySelector('.find-btn')]) b?.setAttribute('aria-expanded', 'false');
    syncDock();
    if (finder.contains(document.activeElement)) (v.findFrom?.isConnected ? v.findFrom : findBtn).focus({ preventScroll: true });
  }

  function showList(open) {
    const show = open && !finder.hidden && findList.childElementCount > 0;
    if (findList.hidden === !show) return;
    findList.hidden = !show;
    document[show ? 'addEventListener' : 'removeEventListener']('pointerdown', outside);
  }

  async function runFind() {
    clearTimeout(typing);
    typing = 0;
    v.findCtrl?.abort();
    const q = findQ.value.trim();
    if (!q || !v.sess) {
      v.found = null;
      findList.replaceChildren();
      showList(false);
      renderFound();
      return;
    }
    const c = (v.findCtrl = new AbortController());
    findSaid.textContent = '…';
    try {
      const r = await api('/api/search' + qs({ q, session: v.sess.ref, per_session: 0, limit: FOUND }), { signal: AbortSignal.any([ctx.signal, c.signal]) });
      v.found = { q, words: needles(q), hits: r.hits.sort((a, b) => a.turn - b.turn), total: r.total, partial: r.partial, missing: r.missing || [], at: -1 };
      renderFound();
      showList(true);
    } catch (e) {
      if (isAbort(e)) return;
      v.found = null;
      renderFound();
      findList.replaceChildren(errorBox(e, runFind));
      showList(true);
    }
  }

  // moreFound asks for the turns past the first hundred, which are the hundred that match
  // best, and puts them in their places
  async function moreFound() {
    const was = v.found;
    try {
      const r = await api('/api/search' + qs({ q: was.q, session: v.sess.ref, per_session: 0, limit: FOUND, offset: was.hits.length }), { signal: ctx.signal });
      if (v.found !== was) return;
      const have = new Set(was.hits.map((x) => x.turn));
      const at = was.hits[was.at]?.turn;
      was.hits = [...was.hits, ...r.hits.filter((x) => !have.has(x.turn))].sort((a, b) => a.turn - b.turn);
      was.total = r.total;
      was.at = was.hits.findIndex((x) => x.turn === at);
      renderFound();
    } catch (e) {
      if (!isAbort(e)) findList.append(errorBox(e));
    }
  }

  // Where a turn found opens: at the first step the words are in, past the ask, which the
  // outline shows
  // (a match in the session's title is in no step)
  const placeOf = (hit) => {
    const m = (hit.matches || []).find((x) => x.kind !== 'prompt' && x.kind !== 'title');
    return m ? `${hit.turn}.${m.seq}` : String(hit.turn);
  };

  function renderFound() {
    const fd = v.found;
    findPrev.disabled = findNext.disabled = !fd?.hits.length;
    if (!fd) { findSaid.textContent = ''; return; }
    findSaid.textContent = !fd.hits.length ? 'none' : fd.at >= 0 ? `${fd.at + 1} / ${fd.hits.length}` : f.plural(fd.total, 'turn');
    const left = fd.total - fd.hits.length;
    // No turn holding every word, the turns listed are the closest, and say what they lack
    const none = fd.missing.length ? `none holds ${fd.missing.map((w) => `“${w}”`).join(', ')}` : '';
    const partial = fd.partial && `No turn of this session holds all ${fd.partial.words} words${none ? '; ' + none : ''}. The closest hold ${fd.partial.held}: `;
    fill(findList,
      h('p', { class: 'find-note' }, !fd.total ? (none ? `No turn of this session: ${none}.` : 'No turn of this session holds these words.')
        : left > 0 ? [partial || '', `The ${f.num(fd.hits.length)} that match best of ${f.plural(fd.total, 'turn')}, in the order of the session. `,
          h('button', { type: 'button', class: 'link-btn', onclick: moreFound }, `${f.num(Math.min(left, FOUND))} more`)]
          : (partial || '') + `${f.plural(fd.total, 'turn')}, in the order of the session.`),
      fd.hits.length > 0 && h('ol', { class: 'find-hits' }, fd.hits.map((hit, i) => {
        const words = formsIn(fd.words, hit);
        const matches = (hit.matches || []).filter((m) => !(hit.ask && m.kind === 'prompt') && m.kind !== 'title');
        return h('li', { dataset: { i }, 'aria-current': i === fd.at ? 'true' : null },
          h('a', { class: 'find-hit', href: sessionHref(ref, placeOf(hit)) },
            h('span', { class: 't-num' }, '#' + hit.turn),
            h('span', { class: 'find-ask' }, hit.ask ? highlight(hit.ask, words) : h('span', { class: 'dim' }, 'Before the first ask')),
            hit.lacks?.length > 0 && h('span', { class: 'lacks find-failed' }, 'lacks ' + hit.lacks.join(' ')),
            hit.failures > 0 && h('span', { class: 'bad find-failed' }, f.failed(hit))),
          matches.length > 0 && h('ul', { class: 'matches' }, matches.map((m) =>
            h('li', null, h('a', { href: sessionHref(ref, `${hit.turn}.${m.seq}`) },
              h('span', { class: 'kind' }, kindOf(m)), h('span', { class: 'snippet' }, highlight(m.snippet, words)))))));
      })));
  }

  // stepFound goes to the next turn found, or the one before. The first step goes from the
  // turn in view, as a find in a page goes on from where the reader is. Asked from the box
  // at a keyboard, the box keeps the focus for the next Enter; on a touch screen the focus
  // goes to what was found, which puts the screen's keyboard away from in front of it.
  function stepFound(d, fromBox = false) {
    const hits = v.found?.hits;
    if (!hits?.length) return;
    let i = v.found.at;
    if (i < 0) {
      const here = v.recs[current()]?.t.index ?? 0;
      i = d > 0 ? hits.findIndex((x) => x.turn >= here) : hits.findLastIndex((x) => x.turn <= here);
      if (i < 0) i = d > 0 ? 0 : hits.length - 1;
    } else {
      i = (i + d + hits.length) % hits.length;
    }
    v.found.at = i;
    showList(false);
    renderFound();
    v.holdFocus = fromBox && fine.matches;
    go(sessionHref(ref, placeOf(hits[i])), { replace: true });
  }

  function expandAll() {
    const loaded = v.recs.filter((r) => r.full);
    const closed = loaded.some((r) => !r.open || r.stepsEl.querySelector('.disc[aria-expanded=false]'));
    for (const r of loaded) {
      if (closed && !r.open) setOpen(r, true);
      for (const b of r.stepsEl.querySelectorAll(`.disc[aria-expanded=${closed ? 'false' : 'true'}]`)) b.click();
    }
  }

  // Following keeps a reader who is at the bottom there as turns arrive. Scrolled up, the
  // pill offers the way back down; switched off, the way to switch it on.
  function renderPill() {
    const show = Boolean(v.sess?.active || v.live);
    pill.hidden = !show;
    if (!show) return;
    const mode = !v.follow ? 'off' : atBottom() ? 'on' : 'away';
    if (pill.dataset.mode === mode) return;
    pill.dataset.mode = mode;
    pill.setAttribute('aria-pressed', String(mode === 'on'));
    pill.title = { on: 'New turns keep the view at the bottom. Click to stop.', away: 'Back to the newest turn, and keep following.', off: 'Follow new turns as they arrive.' }[mode];
    pill.replaceChildren(mode === 'away' ? icon('down') : h('span', { class: 'dot' }), { on: 'Following', away: 'Latest', off: 'Follow' }[mode]);
  }

  // The brief covers the latest turn, the turn in view and every turn after it, or the
  // whole session: what to hand over is where the reader has got to as often as where the
  // session ended
  async function openBrief(opener) {
    let brief = '';
    let ctrl = null;
    const body = h('div', { class: 'sheet-body' });
    const close = () => {
      ctrl?.abort();
      v.sheet?.remove();
      v.sheet = null;
      for (const el of document.querySelectorAll('[data-inert]')) { el.inert = false; delete el.dataset.inert; }
      opener.focus();
    };
    const latest = v.recs.at(-1)?.t.index ?? 0;
    const here = v.recs.length ? v.recs[current()].t.index : 0;
    const scopes = [
      { key: 'latest', label: 'Latest turn', params: {} },
      here > 1 && here < latest && { key: 'from', label: `From #${here}`, params: { from: here } },
      { key: 'all', label: 'Whole session', params: { since: 'all' } },
    ].filter(Boolean);
    const seg = h('div', { class: 'seg-group', role: 'group', 'aria-label': 'What the brief covers' });
    const load = async (scope) => {
      for (const b of seg.children) b.setAttribute('aria-pressed', String(b.dataset.key === scope.key));
      ctrl?.abort();
      const c = (ctrl = new AbortController());
      brief = '';
      body.replaceChildren(skeleton(6, 'line'));
      try {
        brief = (await api(base + '/brief' + qs(scope.params), { signal: AbortSignal.any([ctx.signal, c.signal]) })).brief || '';
        body.replaceChildren(md(brief));
        body.scrollTop = 0;
      } catch (e) {
        if (!isAbort(e)) body.replaceChildren(errorBox(e, () => load(scope)));
      }
    };
    seg.append(...scopes.map((sc) => h('button', { type: 'button', class: 'seg-btn', dataset: { key: sc.key }, 'aria-pressed': 'false', onclick: () => load(sc) }, sc.label)));
    const closeBtn = h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Close the brief', onclick: close }, icon('x'));
    const sheet = h('div', { class: 'sheet', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': 'brief-title' },
      h('header', { class: 'sheet-head' }, h('h2', { id: 'brief-title' }, 'Brief'), h('span', { class: 'dim' }, 'a handoff in Markdown'),
        copyButton(() => brief, 'Copy the brief'), closeBtn),
      h('div', { class: 'sheet-bar' }, seg),
      body);
    v.sheet = h('div', { class: 'sheet-wrap' }, h('div', { class: 'scrim', onclick: close }), sheet);
    v.sheet.addEventListener('keydown', (e) => { if (e.key === 'Escape') { e.preventDefault(); close(); } });
    for (const el of document.body.children) { if (!el.inert) { el.inert = true; el.dataset.inert = '1'; } }
    document.body.append(v.sheet);
    closeBtn.focus();
    load(scopes[0]);
  }

  // A change to this session: the outline again, each turn compared by what it adds up
  // to, and only the ones that differ drawn again
  const refresh = coalesce(async () => {
    const stay = v.follow && atBottom();
    let d;
    try {
      d = await api(base, { signal: ctx.signal });
    } catch (e) {
      if (e.status === 404) notice.replaceChildren(h('p', { class: 'notice warn' }, icon('alert'), 'This session is no longer in its store.'));
      return;
    }
    v.sess = d.session;
    v.agents = d.agents || [];
    v.above = d.above || [];
    renderHead();
    for (const t of d.turns) {
      const rec = v.byIndex.get(t.index);
      if (!rec) { timeline.querySelector(':scope > .empty')?.remove(); addTurn(t); continue; }
      const sig = sigOf(t);
      if (sig === rec.sig) continue;
      rec.t = t;
      rec.sig = sig;
      rec.full = null;
      fillTurn(rec);
      if (rec.open) loadSteps(rec);
    }
    if (stay) scrollTo({ top: document.documentElement.scrollHeight });
    renderPill();
    renderNav();
  }, 1000);

  let framed = 0;
  const onScroll = () => {
    if (framed || (pill.hidden && !narrow.matches)) return;
    framed = requestAnimationFrame(() => {
      framed = 0;
      if (!pill.hidden) renderPill();
      renderWhere();
    });
  };
  addEventListener('scroll', onScroll, { passive: true });
  load();

  return {
    update(r, how) {
      if (r.name !== 'session' || r.args.join('/') !== ref) return false;
      v.route = r;
      if (v.sess) reach(r.hash, how === 'pop' ? savedY() : null);
      // Back and Forward through the turns found keep the count of where in them the reader is
      if (v.found) {
        const at = v.found.hits.findIndex((x) => x.turn === parseInt(r.hash, 10));
        if (at >= 0 && at !== v.found.at) { v.found.at = at; renderFound(); }
      }
      return true;
    },
    changes(ev) {
      if (!v.sess) return;
      const mine = (c) => (c.source === v.sess.source && c.id === v.sess.id) || v.agents.some((a) => a.source === c.source && a.id === c.id);
      if (!ev.all && !ev.sessions?.some(mine)) return;
      v.live = true;
      refresh();
    },
    key(e) {
      if (v.sheet) return false;
      // A modifier pressed on its own is part of the next key — Shift for G, or for the
      // digits on a keyboard that needs it — and lets go of nothing
      if (MODIFIERS.has(e.key)) return false;
      // A number typed before G is the turn to go to; any other key lets go of it
      const count = Date.now() - v.countAt < 1500 ? v.count : '';
      v.count = '';
      if (/^[0-9]$/.test(e.key)) {
        v.count = count + e.key;
        v.countAt = Date.now();
        return true;
      }
      switch (e.key) {
        case 'j': move(1); return true;
        case 'k': move(-1); return true;
        case 'x': toFailed(1); return true;
        case 'X': toFailed(-1); return true;
        case 'f': openFind(); return true;
        case 'o':
        case 'Enter': {
          if (e.key === 'Enter' && e.target.closest('a, button')) return false;
          const rec = v.recs[current()];
          if (!rec) return false;
          setCur(v.recs.indexOf(rec), false);
          setOpen(rec, !rec.open);
          return true;
        }
        case 'e': expandAll(); return true;
        case 'G':
          if (count) toTurn(+count);
          else scrollTo({ top: document.documentElement.scrollHeight, behavior: behavior() });
          return true;
        case 'g':
          if (Date.now() - v.lastG < 700) { scrollTo({ top: 0, behavior: behavior() }); v.lastG = 0; } else v.lastG = Date.now();
          return true;
        case 'Escape':
          if (!finder.hidden) closeFind();
          else back();
          return true;
        default: return false;
      }
    },
    unmount() {
      refresh.cancel();
      clearTimeout(typing);
      sized.disconnect();
      document.removeEventListener('pointerdown', outside);
      removeEventListener('scroll', onScroll);
      if (v.sheet) {
        v.sheet.remove();
        for (const el of document.querySelectorAll('[data-inert]')) { el.inert = false; delete el.dataset.inert; }
      }
    },
  };
}
