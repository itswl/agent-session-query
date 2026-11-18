// A deliberately small Markdown: fences, inline code, bold and italic, headings as bold
// lines, lists, and http(s) links — built as nodes, never as markup. Anything else stays
// the plain text it was, line breaks and all.

import { h, copyButton } from './dom.js';

const INLINE = new RegExp([
  /(`+)([^`\n][^\n]*?)\1(?!`)/.source, // 1, 2: code
  /\*\*(?=\S)([^\n]+?)\*\*|__(?=\S)([^\n]+?)__/.source, // 3, 4: bold
  /(?<![\p{L}\p{N}*\\])\*(?=[^\s*])([^*\n]+?)\*(?![\p{L}\p{N}*])/.source, // 5: italic
  /(?<![\p{L}\p{N}_\\])_(?=[^\s_])([^_\n]+?)_(?![\p{L}\p{N}_])/.source, // 6: italic
  /\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/.source, // 7, 8: link
  /(https?:\/\/[^\s<>"'`]+)/.source, // 9: a bare address
].join('|'), 'gu');

export function link(href, text) {
  let u;
  try { u = new URL(href); } catch { return text; }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return text;
  return h('a', { href: u.href, rel: 'noopener noreferrer', target: '_blank' }, text);
}

export function inline(text) {
  const out = [];
  let last = 0;
  for (const m of text.matchAll(INLINE)) {
    if (m.index > last) out.push(text.slice(last, m.index));
    last = m.index + m[0].length;
    if (m[2] != null) out.push(h('code', null, m[2]));
    else if (m[3] != null || m[4] != null) out.push(h('strong', null, inline(m[3] ?? m[4])));
    else if (m[5] != null || m[6] != null) out.push(h('em', null, inline(m[5] ?? m[6])));
    else if (m[7] != null) out.push(link(m[8], m[7]));
    else {
      // Sentence punctuation after an address is the sentence's, not the address's
      const trail = m[9].match(/[.,;:!?)\]}'"。，；：！？）]+$/u)?.[0] || '';
      out.push(link(m[9].slice(0, m[9].length - trail.length), m[9].slice(0, m[9].length - trail.length)));
      if (trail) out.push(trail);
    }
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

export function code(text, lang = '') {
  return h('div', { class: 'code' },
    h('div', { class: 'code-bar' }, h('span', null, lang), copyButton(text, 'Copy code')),
    h('pre', null, h('code', null, text)));
}

export function md(text) {
  const out = [];
  const lines = String(text).replace(/\r\n?/g, '\n').split('\n');
  let para = [];
  let list = null;
  const flush = () => {
    if (para.length) out.push(h('p', null, inline(para.join('\n'))));
    para = [];
  };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const fence = line.match(/^( {0,3})(`{3,}|~{3,})\s*([^\s`]*)/);
    if (fence) {
      flush();
      // The fence's own indent is not the code's: a block beneath a list item is indented
      // to sit in it, and is part of it
      const pad = new RegExp(`^ {0,${fence[1].length}}`);
      const body = [];
      for (i++; i < lines.length && !lines[i].trimStart().startsWith(fence[2]); i++) body.push(lines[i].replace(pad, ''));
      const block = code(body.join('\n'), fence[3]);
      if (list && fence[1].length >= 2) list.el.lastChild.append(block);
      else {
        list = null;
        out.push(block);
      }
      continue;
    }
    const head = line.match(/^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$/);
    if (head) {
      flush();
      list = null;
      out.push(h('p', { class: 'md-h' }, inline(head[1])));
      continue;
    }
    const item = line.match(/^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/);
    if (item) {
      flush();
      const ordered = /\d/.test(item[2]);
      if (!list || list.ordered !== ordered) {
        list = { ordered, el: h(ordered ? 'ol' : 'ul', ordered ? { start: parseInt(item[2], 10) } : null) };
        out.push(list.el);
      }
      list.el.append(h('li', { class: item[1].length >= 2 ? 'nested' : null }, inline(item[3])));
      continue;
    }
    if (list && /^\s{2,}\S/.test(line)) {
      list.el.lastChild.append('\n', ...inline(line.trim()));
      continue;
    }
    list = null;
    if (!line.trim()) flush();
    else para.push(line);
  }
  flush();
  return h('div', { class: 'md' }, out);
}

// The words of a search query as they should be marked in what it found: phrases whole,
// bare words as their letters, without the excluded terms and the column prefixes.
export function needles(query) {
  const out = [];
  for (const m of String(query).matchAll(/(-?)(?:(?:ask|reply|thinking|tool|output):)?(?:"([^"]*)"?|(\S+))/giu)) {
    if (m[1]) continue;
    if (m[2] != null) { if (m[2].trim()) out.push(m[2].trim().replace(/\s+/g, ' ')); }
    else out.push(...(m[3].match(/[\p{L}\p{N}]+/gu) || []));
  }
  return [...new Set(out.map((s) => s.toLowerCase()))].sort((a, b) => b.length - a.length);
}

const escape = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/ /g, '\\s+');

export function highlight(text, words) {
  if (!words.length || !text) return [text];
  const out = [];
  let last = 0;
  for (const m of text.matchAll(new RegExp(words.map(escape).join('|'), 'giu'))) {
    if (!m[0]) continue;
    if (m.index > last) out.push(text.slice(last, m.index));
    out.push(h('mark', null, m[0]));
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}
