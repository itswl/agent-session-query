// The one way anything reaches the page. Transcript text is other people's text: it
// becomes text nodes and attribute values here, never markup, so nothing it contains
// can turn into an element, a handler or a style.

import * as f from './format.js';

const SVG = 'http://www.w3.org/2000/svg';
const FORBIDDEN = new Set(['style', 'innerHTML', 'outerHTML', 'srcdoc']);

export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v == null || v === false) continue;
      if (FORBIDDEN.has(k)) throw new Error(`h: ${k} is not set this way`);
      if (k === 'class') el.className = Array.isArray(v) ? v.filter(Boolean).join(' ') : v;
      else if (k === 'dataset') for (const [dk, dv] of Object.entries(v)) { if (dv != null) el.dataset[dk] = dv; }
      else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else if (k.includes('-') || !(k in el)) el.setAttribute(k, v === true ? '' : String(v));
      else el[k] = v;
    }
  }
  return add(el, children);
}

// fill replaces an element's children by the same rules as h(): false, null and '' are skipped.
export const fill = (el, ...children) => { el.replaceChildren(); return add(el, children); };

export function add(el, children) {
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false || c === '') continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

const ICONS = {
  search: 'M17 11a6 6 0 1 1-12 0 6 6 0 0 1 12 0zM15.5 15.5 20 20',
  sun: 'M12 8.5a3.5 3.5 0 1 0 0 7 3.5 3.5 0 0 0 0-7zM12 3v2M12 19v2M5.6 5.6 7 7M17 17l1.4 1.4M3 12h2M19 12h2M5.6 18.4 7 17M17 7l1.4-1.4',
  moon: 'M19.5 14.5A7.5 7.5 0 0 1 9.5 4.5a7.5 7.5 0 1 0 10 10z',
  copy: 'M9 9h10v11H9zM15 9V5H5v11h4',
  check: 'm5 12.5 4.5 4.5L19 7.5',
  x: 'M6 6l12 12M18 6 6 18',
  chevron: 'm9.5 6 6 6-6 6',
  down: 'M12 5v14M6 13l6 6 6-6',
  download: 'M12 4v11M7.5 10.5 12 15l4.5-4.5M5 20h14',
  logout: 'M10 4H5v16h5M15 8l4 4-4 4M19 12H9',
  alert: 'M12 4 21 20H3zM12 10v4.5M12 17.5v.01',
  help: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM9.6 9.4a2.5 2.5 0 1 1 3.4 2.4c-.6.3-1 .8-1 1.5v.5M12 16.8v.01',
  filter: 'M4 6h16M7 12h10M10 18h4',
  arrow: 'M5 12h14M13 6l6 6-6 6',
  doc: 'M7 3h7l5 5v13H7zM14 3v5h5M10 13h6M10 17h6',
  up: 'M12 19V5M6 11l6-6 6 6',
};

export function icon(name, cls = '') {
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('class', 'icon ' + cls);
  const p = document.createElementNS(SVG, 'path');
  p.setAttribute('d', ICONS[name]);
  svg.append(p);
  return svg;
}

// A labelled store: instances such as "claude@box2" take the colour of their CLI.
export const agentOf = (source, agent) => agent || String(source).split('@')[0];
export const mark = (source, agent) => h('span', { class: 'mark', dataset: { store: agentOf(source, agent) }, 'aria-hidden': 'true' });
export const store = (source, agent) => h('span', { class: 'store' }, mark(source, agent), source);

// A time that says how long ago, kept current by tick(); the exact moment is its title.
export function time(at, as = 'rel') {
  const t = typeof at === 'number' ? at : f.when(at);
  if (!t) return null;
  const text = as === 'clock' ? f.clock(t) : as === 'stamp' ? f.stamp(t) : f.rel(t);
  return h('time', { dateTime: new Date(t).toISOString(), title: f.full(t), dataset: { rel: as === 'rel' ? t : null } }, text);
}

export function tick() {
  const now = Date.now();
  for (const el of document.querySelectorAll('time[data-rel]')) el.textContent = f.rel(+el.dataset.rel, now);
}

async function writeClipboard(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Outside a secure context (a token-protected server reached over plain http) the
    // clipboard API is absent; the old selection route still works there
    const ta = h('textarea', { value: text, readOnly: true, class: 'offscreen', 'aria-hidden': 'true' });
    document.body.append(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch { ok = false; }
    ta.remove();
    return ok;
  }
}

export function copyButton(text, label = 'Copy') {
  const b = h('button', { type: 'button', class: 'copy', title: label, 'aria-label': label }, icon('copy'));
  let timer = 0;
  b.addEventListener('click', async () => {
    const ok = await writeClipboard(typeof text === 'function' ? text() : text);
    clearTimeout(timer);
    b.replaceChildren(icon(ok ? 'check' : 'x'), h('span', { class: 'copy-said', role: 'status' }, ok ? 'Copied' : 'Copy failed'));
    b.classList.toggle('failed', !ok);
    timer = setTimeout(() => { b.replaceChildren(icon('copy')); b.classList.remove('failed'); }, 1500);
  });
  return b;
}

// A button that shows and hides what it labels. The body is built on first opening: a
// turn holds hundreds of steps, and most are never opened.
export function disclosure({ cls, label, body, open = false }) {
  const panel = h('div', { class: 'disc-body', hidden: true });
  const btn = h('button', { type: 'button', class: 'disc', 'aria-expanded': 'false' }, icon('chevron', 'chev'), label);
  let built = false;
  btn.addEventListener('click', () => {
    const now = btn.getAttribute('aria-expanded') !== 'true';
    if (now && !built) { add(panel, [body()]); built = true; }
    btn.setAttribute('aria-expanded', String(now));
    panel.hidden = !now;
  });
  if (open) btn.click();
  return h('div', { class: ['disclosure', cls] }, btn, panel);
}

// Long text is cut to a number of lines with a way to read the rest. The estimate is
// from the text, not from layout, so nothing is measured.
export function clamp(content, text, lines) {
  let n = 0;
  for (const line of String(text).split('\n')) n += Math.max(1, Math.ceil(line.length / 90));
  if (n <= lines + 3) return content;
  content.classList.add('clamped');
  content.style.setProperty('--lines', lines);
  const btn = h('button', { type: 'button', class: 'more', 'aria-expanded': 'false' }, 'Show more');
  btn.addEventListener('click', () => {
    const open = content.classList.toggle('clamped') === false;
    btn.setAttribute('aria-expanded', String(open));
    btn.textContent = open ? 'Show less' : 'Show more';
  });
  return h('div', { class: 'clamp' }, content, btn);
}

// A button and a panel that drops from it; a click elsewhere or Escape closes it.
export function dropdown(button, panel, cls) {
  const wrap = h('div', { class: ['dropdown', cls] }, button, panel);
  panel.hidden = true;
  button.setAttribute('aria-expanded', 'false');
  const outside = (e) => { if (!wrap.contains(e.target)) set(false); };
  // Escape closes it wherever the focus is, before any view hears the key
  const escape = (e) => { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); set(false); button.focus(); } };
  function set(open) {
    panel.hidden = !open;
    button.setAttribute('aria-expanded', String(open));
    const how = open ? 'addEventListener' : 'removeEventListener';
    document[how]('pointerdown', outside);
    document[how]('keydown', escape, true);
  }
  button.addEventListener('click', () => set(panel.hidden));
  panel.addEventListener('click', (e) => { if (e.target.closest('a')) set(false); });
  return wrap;
}

export const skeleton = (n, cls = 'row') =>
  h('div', { class: 'skeleton', 'aria-busy': 'true', 'aria-label': 'Loading' },
    Array.from({ length: n }, () => h('div', { class: 'skel skel-' + cls }, h('i'), h('i'), h('i'))));

export const errorBox = (err, retry) =>
  h('div', { class: 'error-box', role: 'alert' }, icon('alert'),
    h('p', null, err?.message || String(err)),
    retry && h('button', { type: 'button', class: 'btn', onclick: retry }, 'Retry'));

export const empty = (title, ...hints) =>
  h('div', { class: 'empty' }, h('p', { class: 'empty-title' }, title), hints.map((x) => h('p', null, x)));

export const fact = (label, value, props) => h('div', { class: 'fact', ...props }, h('dt', null, label), h('dd', null, value));

export const reducedMotion = () => matchMedia('(prefers-reduced-motion: reduce)').matches;

// choices is a row of links of which one is the current: a window, a filter.
export const choices = (label, opts) =>
  h('nav', { class: 'seg-group', 'aria-label': label },
    opts.map(([href, text, on]) => h('a', { class: 'seg-btn', href, 'aria-current': on ? 'true' : null }, text)));
