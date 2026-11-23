// What changed, as it changes: the event stream, the index's reading of the stores, and
// the health report that says which stores were found and which could not be read.
//
// The stream is listened to while the page is looked at. A tab left in the background
// hears nothing and asks for nothing — the server follows the stores closely for as long
// as a stream is open, and nobody is reading a hidden tab — and catches up in one go
// when it is looked at again.

import { api } from './api.js';

export const status = { conn: 'connecting', sync: null, health: null };
const changeFns = new Set();
const statusFns = new Set();
const AWAY = 60000; // a tab hidden this long lets go of the stream
let es = null;
let hellos = 0;
let retry = 1000;
let retryTimer = 0;
let awayTimer = 0;
let away = false;
let line = 0; // counts the times the stream was let go of, so that a reconnect under way can tell
let missed = false;
let healthP = null;
let healthKey = '';

export const onChanges = (fn) => { changeFns.add(fn); return () => changeFns.delete(fn); };
export const onStatus = (fn) => { statusFns.add(fn); fn(status); return () => statusFns.delete(fn); };
const notify = () => statusFns.forEach((fn) => fn(status));

// A change is passed on while the page is looked at; while it is not, it is remembered
// as one thing: something changed.
function emit(ev, askHealth = staleHealth) {
  if (document.hidden) { missed = true; return; }
  changeFns.forEach((fn) => fn(ev));
  askHealth();
}

export function start() {
  connect();
  refreshHealth();
  document.addEventListener('visibilitychange', seen);
  // A tab opened in the background is told nothing until it is first shown: it is hidden
  // from the start, and lets go of the stream a minute after it as well
  if (document.hidden) seen();
}

export function stop() {
  document.removeEventListener('visibilitychange', seen);
  clearTimeout(awayTimer);
  hangUp();
}

function hangUp() {
  line++;
  clearTimeout(retryTimer);
  es?.close();
  es = null;
}

function seen() {
  clearTimeout(awayTimer);
  if (document.hidden) {
    awayTimer = setTimeout(() => { away = true; hangUp(); }, AWAY);
    return;
  }
  if (away) {
    // The stream's first word, on its return, says everything may have changed
    away = false;
    missed = false;
    connect();
  } else if (missed) {
    missed = false;
    emit({ all: true });
  }
}

function connect() {
  // One stream at a time: one opened before and not closed would be heard twice over, and
  // would hold the server to the stores for as long as the page is open
  es?.close();
  es = new EventSource('/api/events');
  es.addEventListener('hello', (e) => {
    retry = 1000;
    status.conn = 'live';
    setSync(JSON.parse(e.data).sync);
    // A stream that came back may have missed changes while it was away
    if (hellos++ > 0) emit({ all: true }, refreshHealth);
  });
  es.addEventListener('changes', (e) => emit(JSON.parse(e.data)));
  es.addEventListener('sync', (e) => setSync(JSON.parse(e.data)));
  es.addEventListener('error', () => {
    status.conn = 'reconnecting';
    notify();
    if (es.readyState !== EventSource.CLOSED) return; // the browser retries by itself
    es.close();
    retryTimer = setTimeout(reconnect, retry);
    retry = Math.min(retry * 2, 30000);
  });
}

// A stream the server refused stays closed; ask why before opening another, so that a
// lost sign-in shows the login rather than a retry loop.
async function reconnect() {
  const was = line;
  try {
    await refreshHealth(true);
    // Let go of while the answer was on its way — the tab went away, or the page stopped:
    // the stream is opened again by whatever brings the page back, not from here
    if (line === was) connect();
  } catch (e) {
    if (e.status !== 401 && line === was) retryTimer = setTimeout(reconnect, retry);
  }
}

// The stream speaks of a sync when it is a catch-up, and of the stores when their
// condition moved. Either is a reason to ask how things stand; the reading that follows
// a session as it is written is neither, and is not heard of here at all.
function setSync(sync) {
  const was = status.sync;
  status.sync = sync;
  notify();
  if ((was?.running && !sync.running) || (was && (was.stores || '') !== (sync.stores || ''))) refreshHealth();
}

// health is the report as it was last asked for. status.health changes — and those who
// watch it are told — only when what the page shows of it does: the server's version,
// which stores there are, how many sessions each holds, which could not be read, whether the
// server's own program file was replaced by an upgrade. The rest of the report moves with
// every line an agent writes.
export function health() {
  healthP ||= api('/api/health').then((h) => {
    const key = JSON.stringify([h.version, h.replaced, h.stores, h.warnings]);
    if (key !== healthKey || !status.health) {
      healthKey = key;
      status.health = h;
      notify();
    }
    return status.health;
  });
  healthP.catch(() => { healthP = null; });
  return healthP;
}

export function refreshHealth(strict = false) {
  healthP = null;
  const p = health();
  return strict ? p : p.catch(() => null);
}

// coalesce runs fn at most once per interval, however often it is asked: a burst of
// change events costs one request, made after the burst.
export function coalesce(fn, ms = 1000) {
  let last = 0;
  let timer = 0;
  const call = () => {
    if (timer) return;
    timer = setTimeout(() => { timer = 0; last = Date.now(); fn(); }, Math.max(0, last + ms - Date.now()));
  };
  call.cancel = () => { clearTimeout(timer); timer = 0; };
  return call;
}

// The counts beside the stores go out of date as sessions are added; half a minute
// behind is near enough for a number in a filter.
const staleHealth = coalesce(refreshHealth, 30000);
