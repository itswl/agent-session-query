// One session as a timeline of turns. Only the outline is fetched up front — a session can
// be megabytes — and a turn's steps are fetched when it is opened.

import { h, icon, mark, time, copyButton, clamp, skeleton, errorBox, empty, fact, dropdown, fill, reducedMotion } from '../dom.js';
import { api, qs, sessionApi, sessionHref, isAbort } from '../api.js';
import { go, savedY, cameFromList } from '../router.js';
import { coalesce } from '../live.js';
import { md } from '../md.js';
import { steps, text } from './steps.js';
import * as f from '../format.js';

const titleOf = (s) => s.title || f.firstLine(s.lastAsk, 120) || 'Untitled session';
const sigOf = (t) => [t.endedAt, t.lastAt, t.steps, t.tools, t.failures, t.interrupted, t.ask?.length, t.answer?.length, f.totalTokens(t.usage)].join('|');
const atBottom = () => innerHeight + scrollY >= document.documentElement.scrollHeight - 120;
const behavior = () => (reducedMotion() ? 'auto' : 'smooth');

export function mount(main, route, ctx) {
  const ref = route.args.join('/');
  const base = sessionApi(ref);
  const v = { route, sess: null, agents: [], above: [], recs: [], byIndex: new Map(), cur: -1, follow: true, live: false, sheet: null, lastG: 0 };

  const crumbs = h('nav', { class: 'crumbs', 'aria-label': 'Where this session ran' });
  const h1 = h('h1', { tabIndex: -1 }, ' ');
  const info = h('div', { class: 's-info' }, skeleton(1, 'head'));
  // Three parts, so that a live update redraws the figures without closing an open menu
  const facts = h('div');
  const actions = h('div', { class: 'actions' });
  const extra = h('div');
  const notice = h('div');
  const timeline = h('ol', { class: 'timeline', 'aria-label': 'Turns' });
  const pill = h('button', { type: 'button', class: 'follow', hidden: true, 'aria-pressed': 'true' });
  pill.addEventListener('click', () => {
    v.follow = pill.dataset.mode !== 'on';
    if (v.follow) scrollTo({ top: document.documentElement.scrollHeight, behavior: behavior() });
    renderPill();
  });
  main.append(h('article', { class: 'session' }, h('header', { class: 's-head' }, crumbs, h1, info), notice, timeline, pill));

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
      await reach(v.route.hash, ctx.how === 'pop' ? savedY() : null);
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
      info.replaceChildren(facts, actions, extra);
      actions.append(
        h('button', { type: 'button', class: 'btn', onclick: (e) => openBrief(e.currentTarget) }, icon('doc'), 'Brief'),
        dropdown(
          h('button', { type: 'button', class: 'btn', 'aria-haspopup': 'true' }, icon('download'), 'Export'),
          h('div', { class: 'menu' }, [['md', 'Markdown'], ['json', 'JSON'], ['jsonl', 'JSON Lines'], ['html', 'HTML page']].map(([fmt, label]) =>
            h('a', { href: `${base}/export${qs({ format: fmt })}`, download: '' }, label, h('span', { class: 'dim' }, '.' + fmt))))));
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
        s.active && h('span', { class: 'live' }, 'live')),
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
    target.focus({ preventScroll: true });
    flash(target);
  }

  function setCur(i, scroll = true) {
    v.recs[v.cur]?.el.classList.remove('current');
    v.cur = i;
    const el = v.recs[i].el;
    el.classList.add('current');
    if (scroll) {
      el.scrollIntoView({ block: 'start', behavior: behavior() });
      el.focus({ preventScroll: true });
    }
  }

  // The turn a key acts on: the one moved to last, while it is on screen; else the first in view
  function current() {
    const r = v.recs[v.cur]?.el.getBoundingClientRect();
    if (r && r.bottom > 60 && r.top < innerHeight) return v.cur;
    const i = v.recs.findIndex((x) => x.el.getBoundingClientRect().bottom > 60);
    return i < 0 ? v.recs.length - 1 : i;
  }

  function move(d) {
    if (!v.recs.length) return;
    const i = current();
    const onIt = v.cur === i;
    setCur(Math.min(v.recs.length - 1, Math.max(0, onIt ? i + d : i)));
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

  async function openBrief(opener) {
    let brief = '';
    const body = h('div', { class: 'sheet-body' }, skeleton(6, 'line'));
    const close = () => {
      v.sheet?.remove();
      v.sheet = null;
      for (const el of document.querySelectorAll('[data-inert]')) { el.inert = false; delete el.dataset.inert; }
      opener.focus();
    };
    const closeBtn = h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Close the brief', onclick: close }, icon('x'));
    const sheet = h('div', { class: 'sheet', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': 'brief-title' },
      h('header', { class: 'sheet-head' }, h('h2', { id: 'brief-title' }, 'Brief'), h('span', { class: 'dim' }, 'a handoff in Markdown'),
        copyButton(() => brief, 'Copy the brief'), closeBtn),
      body);
    v.sheet = h('div', { class: 'sheet-wrap' }, h('div', { class: 'scrim', onclick: close }), sheet);
    v.sheet.addEventListener('keydown', (e) => { if (e.key === 'Escape') { e.preventDefault(); close(); } });
    for (const el of document.body.children) { if (!el.inert) { el.inert = true; el.dataset.inert = '1'; } }
    document.body.append(v.sheet);
    closeBtn.focus();
    try {
      brief = (await api(base + '/brief', { signal: ctx.signal })).brief || '';
      body.replaceChildren(md(brief));
    } catch (e) {
      if (!isAbort(e)) body.replaceChildren(errorBox(e));
    }
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
  }, 1000);

  let framed = 0;
  const onScroll = () => {
    if (pill.hidden || framed) return;
    framed = requestAnimationFrame(() => { framed = 0; renderPill(); });
  };
  addEventListener('scroll', onScroll, { passive: true });
  load();

  return {
    update(r, how) {
      if (r.name !== 'session' || r.args.join('/') !== ref) return false;
      v.route = r;
      if (v.sess) reach(r.hash, how === 'pop' ? savedY() : null);
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
      switch (e.key) {
        case 'j': move(1); return true;
        case 'k': move(-1); return true;
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
        case 'G': scrollTo({ top: document.documentElement.scrollHeight, behavior: behavior() }); return true;
        case 'g':
          if (Date.now() - v.lastG < 700) { scrollTo({ top: 0, behavior: behavior() }); v.lastG = 0; } else v.lastG = Date.now();
          return true;
        case 'Escape':
          if (cameFromList()) history.back();
          else go('/');
          return true;
        default: return false;
      }
    },
    unmount() {
      refresh.cancel();
      removeEventListener('scroll', onScroll);
      if (v.sheet) {
        v.sheet.remove();
        for (const el of document.querySelectorAll('[data-inert]')) { el.inert = false; delete el.dataset.inert; }
      }
    },
  };
}
