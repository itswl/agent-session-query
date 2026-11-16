// What changed, as it changes: the event stream, the index's reading of the stores, and
// the health report that says which stores were found and which could not be read.

import { api } from './api.js';

export const status = { conn: 'connecting', sync: null, health: null };
const changeFns = new Set();
const statusFns = new Set();
let es = null;
let hellos = 0;
let retry = 1000;
let retryTimer = 0;
let healthP = null;

export const onChanges = (fn) => { changeFns.add(fn); return () => changeFns.delete(fn); };
export const onStatus = (fn) => { statusFns.add(fn); fn(status); return () => statusFns.delete(fn); };
const emit = (ev) => changeFns.forEach((fn) => fn(ev));
const notify = () => statusFns.forEach((fn) => fn(status));

export function start() {
  connect();
  refreshHealth();
}

export function stop() {
  clearTimeout(retryTimer);
  es?.close();
  es = null;
}

function connect() {
  es = new EventSource('/api/events');
  es.addEventListener('hello', (e) => {
    retry = 1000;
    status.conn = 'live';
    setSync(JSON.parse(e.data).sync);
    // A stream that came back may have missed changes while it was away
    if (hellos++ > 0) { emit({ all: true }); refreshHealth(); }
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
  try {
    await refreshHealth(true);
    connect();
  } catch (e) {
    if (e.status !== 401) retryTimer = setTimeout(reconnect, retry);
  }
}

function setSync(sync) {
  const finished = status.sync?.running && !sync.running;
  status.sync = sync;
  notify();
  if (finished) refreshHealth();
}

export function health() {
  healthP ||= api('/api/health').then((h) => {
    status.health = h;
    notify();
    return h;
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
