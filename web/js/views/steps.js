// The steps of one turn, in the order they happened: words, reasoning, tool calls with
// their results, and the events between them. A step whose text was cut to a preview
// can be read whole on request.

import { h, icon, disclosure, copyButton, time } from '../dom.js';
import { api, sessionHref, isAbort } from '../api.js';
import { md } from '../md.js';
import * as f from '../format.js';

// ctx: { base (the session's API path), turn, agents (its direct subagents), signal }
export function steps(turn, ctx) {
  const items = turn.items || [];
  // The prompt that opens a turn is its ask, which the turn already shows
  const ask = turn.index > 0 ? items.find((it) => it.kind === 'prompt' && !it.queued && !it.inherited) : null;
  const runs = [];
  for (const it of items) {
    if (it === ask) continue;
    const last = runs.at(-1);
    if (it.inherited && last?.inherited) last.items.push(it);
    else runs.push({ inherited: Boolean(it.inherited), items: [it] });
  }
  const nodes = runs.flatMap((run) => (run.inherited ? [copied(run.items, ctx)] : run.items.map((it) => step(it, ctx))));
  return { nodes, ask };
}

function copied(items, ctx) {
  const el = disclosure({
    cls: 'copied',
    label: [h('span', { class: 'tag' }, 'copied from the parent session'), h('span', { class: 'gist' }, f.plural(items.length, 'step'))],
    body: () => items.map((it) => step(it, ctx)),
  });
  el.dataset.from = items[0].seq;
  el.dataset.to = items.at(-1).seq;
  return el;
}

function step(it, ctx) {
  const props = { class: `step ${it.kind}`, id: 'q' + it.seq, tabIndex: -1, dataset: { seq: it.seq } };
  const seq = h('a', { class: 'seq', href: `#${ctx.turn}.${it.seq}`, title: 'Link to this step' }, `[${it.seq}]`);
  switch (it.kind) {
    case 'reply':
      return h('div', props, text(it, ctx, true), seq);
    case 'prompt':
      return h('div', props, h('div', { class: 'tag' }, it.queued ? 'queued while the agent worked' : 'prompt'), text(it, ctx, true), seq);
    case 'tool':
      return tool(it, ctx, props, seq);
    case 'event': {
      const body = (it.text || '').trim();
      const label = [h('span', { class: 'tag' }, (it.event || 'event').replace(/_/g, ' ')), body && h('span', { class: 'gist' }, f.firstLine(body, 120))];
      const long = body.length > 120 || body.includes('\n');
      return h('div', props, long ? disclosure({ label, body: () => text(it, ctx, false) }) : h('div', { class: 'event-line' }, label), seq);
    }
    default: // thinking, context, and whatever a CLI adds: folded until asked for
      return h('div', props, disclosure({
        label: [h('span', { class: 'tag' }, it.kind), h('span', { class: 'gist' }, f.firstLine(it.text))],
        body: () => text(it, ctx, it.kind === 'thinking'),
      }), seq);
  }
}

// text is a step's words; markdown for what a model or a person wrote, plain otherwise.
export function text(it, ctx, markdown) {
  const render = (s) => (markdown ? md(s || '') : h('div', { class: 'plain' }, s || ''));
  const box = h('div', { class: 'text' }, render(it.text));
  if (it.bytes) box.append(showAll(it.bytes, ctx, it.seq, (whole) => box.replaceChildren(render(whole.text))));
  return box;
}

function showAll(size, ctx, seq, apply) {
  const b = h('button', { type: 'button', class: 'btn show-all' }, icon('down'), `Show all (${f.bytes(size)})`);
  b.addEventListener('click', async () => {
    b.disabled = true;
    b.lastChild.textContent = 'Loading…';
    try {
      const d = await api(`${ctx.base}/items/${seq}`, { signal: ctx.signal });
      apply(d.item);
      b.remove();
    } catch (e) {
      if (isAbort(e)) return;
      b.disabled = false;
      b.lastChild.textContent = `Could not load (${e.message}) — try again`;
    }
  });
  return b;
}

// An empty status is a result whose CLI did not record how the call ended: unknown, not ok
const STATUS = { ok: 'ok', error: 'error', interrupted: 'interrupted', pending: 'no result yet', unknown: 'outcome not recorded' };

function tool(it, ctx, props, seq) {
  const t = it.tool || {};
  const status = !t.done ? 'pending' : t.status || 'unknown';
  const took = t.durMs || (t.endAt && it.at ? f.when(t.endAt) - f.when(it.at) : 0);
  const summary = t.command || t.files?.[0] || compact(t.input);
  const sub = t.kind === 'agent' && t.callId ? ctx.agents.find((a) => a.parent?.call === t.callId) : null;
  props.dataset.status = status;
  return h('div', props,
    disclosure({
      cls: 'tool-disc',
      label: [
        h('span', { class: 'tool-status', dataset: { status }, title: STATUS[status] || status }, h('span', { class: 'sr' }, STATUS[status] || status)),
        h('span', { class: 'tool-name' }, t.name || 'tool'),
        h('span', { class: 'tool-sum', title: summary.length > 80 ? summary.slice(0, 600) : null }, summary.replace(/\s+/g, ' ')),
        h('span', { class: 'tool-meta dots' },
          status !== 'ok' && h('span', { class: 'st' }, STATUS[status] || status),
          t.exit != null && h('span', { class: t.exit ? 'bad' : null }, `exit ${t.exit}`),
          took > 0 && h('span', null, f.dur(took))),
      ],
      body: () => toolBody(it, ctx),
    }),
    seq,
    sub && h('a', { class: 'sub-link', href: sessionHref(sub.ref) }, icon('arrow'),
      h('span', null, `${sub.parent?.type || 'subagent'}: `), sub.title || sub.lastAsk || sub.id,
      h('span', { class: 'dim' }, ` · ${f.plural(sub.stats?.turns || 0, 'turn')}`)));
}

function toolBody(it, ctx) {
  const t = it.tool || {};
  const input = h('pre', { class: 'io' }, pretty(t.input));
  const output = h('pre', { class: 'io out' }, t.output || '');
  const cut = Math.max(t.inBytes || 0, t.outBytes || 0);
  return h('div', { class: 'tool-body' },
    t.files?.length > 1 && h('ul', { class: 'files' }, t.files.map((p) => h('li', null, p))),
    h('div', { class: 'io-head' }, h('span', null, 'Input'), it.at && time(it.at, 'stamp'), copyButton(() => input.textContent, 'Copy input')),
    input,
    h('div', { class: 'io-head' }, h('span', null, 'Output'), t.endAt && time(t.endAt, 'stamp'), t.done && copyButton(() => output.textContent, 'Copy output')),
    t.done ? (t.output ? output : h('p', { class: 'dim' }, 'The call returned nothing.')) : h('p', { class: 'dim' }, 'No result was recorded for this call.'),
    cut > 0 && showAll(cut, ctx, it.seq, (whole) => {
      input.textContent = pretty(whole.tool?.input);
      output.textContent = whole.tool?.output || '';
    }));
}

function pretty(input) {
  if (input == null) return '';
  return typeof input === 'string' ? input : JSON.stringify(input, null, 2);
}

// compact is a call's arguments in one line, led by the one that says the most.
function compact(input) {
  if (input == null) return '';
  if (typeof input !== 'object') return String(input);
  for (const k of ['description', 'query', 'pattern', 'url', 'prompt', 'path', 'file_path', 'name']) {
    if (typeof input[k] === 'string' && input[k]) return input[k];
  }
  const s = JSON.stringify(input);
  return s === '{}' ? '' : s.slice(0, 300);
}
