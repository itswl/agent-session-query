// Numbers and times as a person reads them. Every humanised figure is shown with its exact
// value in a title, so rounding never hides what the record says.

const nf = new Intl.NumberFormat('en-US');
const dayFmt = new Intl.DateTimeFormat('en-GB', { weekday: 'short', day: 'numeric', month: 'short' });
const dayYearFmt = new Intl.DateTimeFormat('en-GB', { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric' });
const shortFmt = new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short' });
const clockFmt = new Intl.DateTimeFormat('en-GB', { hour: '2-digit', minute: '2-digit' });
const fullFmt = new Intl.DateTimeFormat('en-GB', { dateStyle: 'full', timeStyle: 'medium' });

export const num = (n) => nf.format(n || 0);

export const plural = (n, one, many = one + 's') => `${num(n)} ${n === 1 ? one : many}`;

function scaled(v, units, base) {
  let i = 0;
  while (Math.abs(v) >= base && i < units.length - 1) { v /= base; i++; }
  // What would round up to a whole unit is written in that unit: 1M, not 1000k
  if (Math.round(Math.abs(v)) >= base && i < units.length - 1) { v /= base; i++; }
  return [v >= 100 || i === 0 ? String(Math.round(v)) : v.toFixed(1).replace(/\.0$/, ''), units[i]];
}

export function tokens(n) {
  const [v, unit] = scaled(n || 0, ['', 'k', 'M', 'B', 'T'], 1000);
  return v + unit;
}

export function bytes(n) {
  const [v, unit] = scaled(n || 0, ['B', 'KB', 'MB', 'GB'], 1024);
  return `${v} ${unit}`;
}

export function pct(x) {
  const v = (x || 0) * 100;
  return (v > 0 && v < 10 ? v.toFixed(1) : String(Math.round(v))) + '%';
}

export const cost = (usd) => (!usd ? '' : usd < 0.01 ? '<$0.01' : '$' + usd.toFixed(2));

// failed says how many calls of a turn, a session or a project failed, and how many of
// those the same command passed later: "2 failed, 1 passed later"; with calls, "2 failed
// calls, 1 passed later".
export function failed(x, calls = false) {
  const n = x.failures || 0;
  if (!n) return '';
  return `${calls ? plural(n, 'failed call') : `${num(n)} failed`}${later(n, x.passedLater)}`;
}

// later is how many of n failures the same command passed later, as it follows them:
// ", 1 passed later", ", all passed later"; '' for none.
export const later = (n, p = 0) => (!p ? '' : p < n ? `, ${num(p)} passed later` : n === 1 ? ', passed later' : ', all passed later');

// open is how many of those the same command did not pass later: what is still to see to.
export const open = (x) => Math.max((x.failures || 0) - (x.passedLater || 0), 0);

// passedTitle is what a count of failures says on hover.
export const passedTitle = 'Calls that failed, and how many of them the same command passed later, in that turn or the three after';

// dur is a span of time, cut down as the command line cuts it, never rounded up: 23.6
// seconds are 23s there and here.
export function dur(ms) {
  if (!(ms > 0)) return '';
  if (ms < 1000) return Math.floor(ms) + 'ms';
  const s = ms / 1000;
  if (s < 60) return (s < 10 ? (Math.floor(s * 10) / 10).toFixed(1) : Math.floor(s)) + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${Math.floor(s % 60)}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

// when reads an API timestamp; zero stands for "not recorded".
export const when = (s) => (s ? Date.parse(s) : 0);

const midnight = (t) => new Date(t).setHours(0, 0, 0, 0);
const daysAgo = (t, now = Date.now()) => Math.round((midnight(now) - midnight(t)) / 864e5);
export const sameDay = (a, b) => midnight(a) === midnight(b);

export function day(t) {
  const n = daysAgo(t);
  if (n === 0) return 'Today';
  if (n === 1) return 'Yesterday';
  return (new Date(t).getFullYear() === new Date().getFullYear() ? dayFmt : dayYearFmt).format(t);
}

export const shortDay = (t) => shortFmt.format(t);
export const clock = (t) => clockFmt.format(t);
export const stamp = (t) => `${day(t)} ${clock(t)}`;
export const full = (t) => fullFmt.format(t);

export function rel(t, now = Date.now()) {
  const s = (now - t) / 1000;
  if (s < 45) return 'just now';
  if (s < 3600) return Math.max(1, Math.round(s / 60)) + 'm ago';
  const n = daysAgo(t, now);
  if (n === 0) return Math.round(s / 3600) + 'h ago';
  if (n === 1) return 'yesterday';
  if (n < 7) return n + 'd ago';
  return day(t);
}

export function localDate(d = new Date()) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

// A working directory by the part of it that names the project.
export function shortPath(p) {
  if (!p) return '';
  return p.split(/[\\/]+/).filter(Boolean).slice(-2).join('/') || p;
}

export const promptTokens = (u = {}) => (u.input || 0) + (u.cacheRead || 0) + (u.cacheWrite || 0);
export const totalTokens = (u = {}) => promptTokens(u) + (u.output || 0);
export function cacheHit(u = {}) {
  const p = promptTokens(u);
  return p ? (u.cacheRead || 0) / p : 0;
}

export function firstLine(text, max = 160) {
  const line = (text || '').split('\n').find((l) => l.trim()) || '';
  return line.length > max ? line.slice(0, max) + '…' : line.trim();
}
