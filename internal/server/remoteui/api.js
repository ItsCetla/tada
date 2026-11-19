// HTTP and event-stream plumbing for the Ta-Da remote UI. The token only
// ever travels in the Authorization header, never in a URL.

const TOKEN_KEY = 'tada.remote.token';
const CLIENT_KEY = 'tada.remote.client';

export class UnauthorizedError extends Error {
  constructor() {
    super('unauthorized');
    this.name = 'UnauthorizedError';
  }
}

export class HttpError extends Error {
  constructor(status, message) {
    super(message || 'HTTP ' + status);
    this.name = 'HttpError';
    this.status = status;
  }
}

export function uuid() {
  if (window.crypto && typeof crypto.randomUUID === 'function') {
    try {
      return crypto.randomUUID();
    } catch {
      // randomUUID needs a secure context; plain-HTTP LAN pages fall through.
    }
  }
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

// Moves a #token=... fragment into localStorage and scrubs it from the URL.
export function takeTokenFromHash() {
  const hash = window.location.hash.replace(/^#/, '');
  if (!hash) return;
  const params = new URLSearchParams(hash);
  const token = params.get('token');
  if (token) {
    setToken(token);
    params.delete('token');
    const rest = params.toString();
    const url = window.location.pathname + window.location.search + (rest ? '#' + rest : '');
    history.replaceState(null, '', url);
  }
}

export function getToken() {
  try {
    return localStorage.getItem(TOKEN_KEY) || '';
  } catch {
    return '';
  }
}

export function setToken(token) {
  try {
    localStorage.setItem(TOKEN_KEY, token.trim());
  } catch {
    // Storage disabled; the token lives only for this page load.
  }
  memToken = token.trim();
}

let memToken = '';

function token() {
  return getToken() || memToken;
}

export function clientID() {
  let id = '';
  try {
    id = sessionStorage.getItem(CLIENT_KEY) || '';
  } catch {
    // Ignore.
  }
  if (!id) id = resetClientID();
  return id;
}

export function resetClientID() {
  const id = uuid();
  try {
    sessionStorage.setItem(CLIENT_KEY, id);
  } catch {
    // Ignore.
  }
  return id;
}

async function errorFrom(res) {
  let message = '';
  try {
    const text = await res.text();
    try {
      message = JSON.parse(text).message || text;
    } catch {
      message = text;
    }
  } catch {
    // Ignore.
  }
  return new HttpError(res.status, message.trim() || res.statusText);
}

export async function api(method, path, body) {
  const headers = { Authorization: 'Bearer ' + token() };
  const init = { method, headers, cache: 'no-store' };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetch(path, init);
  if (res.status === 401) throw new UnauthorizedError();
  if (!res.ok) throw await errorFrom(res);
  const text = await res.text();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

// Opens the server-sent event stream with fetch so the bearer token can go
// in a header, and parses `data: <json>` frames by hand. Resolves when the
// stream ends; rejects on HTTP errors or network failure.
export async function streamEvents(path, { signal, onOpen, onEvent }) {
  const res = await fetch(path, {
    headers: { Authorization: 'Bearer ' + token(), Accept: 'text/event-stream' },
    cache: 'no-store',
    signal,
  });
  if (res.status === 401) throw new UnauthorizedError();
  if (!res.ok) throw await errorFrom(res);
  if (!res.body) throw new HttpError(0, 'streaming not supported by this browser');
  onOpen();

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true }).replace(/\r\n?/g, '\n');
    let idx;
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const frame = buf.slice(0, idx);
      buf = buf.slice(idx + 2);
      const data = frame
        .split('\n')
        .filter((l) => l.startsWith('data:'))
        .map((l) => l.slice(5).replace(/^ /, ''))
        .join('\n');
      if (!data) continue;
      let ev;
      try {
        ev = JSON.parse(data);
      } catch {
        console.warn('Bad event frame', data.slice(0, 200));
        continue;
      }
      onEvent(ev);
    }
  }
}
