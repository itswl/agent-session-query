// The API, and the addresses of things in it and on the page.

let lost = () => {};
export const onUnauthorized = (fn) => { lost = fn; };

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

export const isAbort = (e) => e?.name === 'AbortError';

export async function api(path, { signal, method = 'GET', body } = {}) {
  let res;
  try {
    res = await fetch(path, {
      method, signal, credentials: 'same-origin',
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch (e) {
    if (isAbort(e)) throw e;
    throw new ApiError(0, 'The server could not be reached. Is asq still running?');
  }
  const data = await res.json().catch(() => null);
  if (res.status === 401 && path !== '/api/login') lost();
  if (!res.ok) throw new ApiError(res.status, data?.error || `${res.status} ${res.statusText}`);
  return data;
}

// qs renders parameters, leaving out the empty ones; true is "1".
export function qs(params) {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v == null || v === '' || v === false) continue;
    u.set(k, v === true ? '1' : String(v));
  }
  const s = u.toString();
  return s ? '?' + s : '';
}

// A ref is "source/id"; the id is everything after the first slash.
export function splitRef(ref) {
  const i = ref.indexOf('/');
  return [ref.slice(0, i), ref.slice(i + 1)];
}

const path = (ref) => splitRef(ref).map(encodeURIComponent).join('/');
export const sessionApi = (ref) => '/api/sessions/' + path(ref);
export const sessionHref = (ref, at) => '/s/' + path(ref) + (at != null ? '#' + at : '');
