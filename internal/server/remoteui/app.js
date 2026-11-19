// Ta-Da remote control web app. All model, tool and user text is inserted
// with textContent or the escaping Markdown renderer; never as HTML.

import {
  api,
  clientID,
  getToken,
  HttpError,
  resetClientID,
  setToken,
  streamEvents,
  takeTokenFromHash,
  UnauthorizedError,
  uuid,
} from './api.js';
import { renderMarkdown } from './markdown.js';

const $ = (id) => document.getElementById(id);

function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

const AVATAR_COLORS = ['#8a5a3b', '#e5484d', '#f0762a', '#f2a03d', '#46b26b', '#3fae9c', '#3b82f6', '#8b5cf6', '#e5469a', '#71717a'];

// avatar draws a coloured circle with the label's first letter; the colour
// is picked from the seed so a session keeps its colour.
function avatar(seed, label, cls) {
  let h = 0;
  for (const ch of String(seed)) h = (h * 31 + ch.codePointAt(0)) >>> 0;
  const a = el('span', 'avatar' + (cls ? ' ' + cls : ''), (String(label).trim()[0] || '·').toUpperCase());
  a.style.background = AVATAR_COLORS[h % AVATAR_COLORS.length];
  a.setAttribute('aria-hidden', 'true');
  return a;
}

function lsGet(key) {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function lsSet(key, value) {
  try {
    if (value === null || value === undefined || value === '') localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    // Storage disabled.
  }
}

const DRAFT_KEY = 'tada.remote.draft';
const PATH_KEY = 'tada.remote.path';
const sessionKey = () => 'tada.remote.session:' + state.path;
const enc = encodeURIComponent;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const state = {
  info: null,
  auto: false,
  path: '',
  ws: null,
  sessions: new Map(),
  sessionId: null,
  messages: new Map(),
  order: [],
  results: new Map(),
  callOwner: new Map(),
  nodes: new Map(),
  busy: new Set(),
  queued: 0,
  perms: [],
  question: null,
  sheet: null,
  openDetails: new Set(),
  openFull: new Set(),
  stick: true,
  lastSend: 0,
};

const ui = {};

// ---------------------------------------------------------------------------
// Boot and auth.

function boot() {
  for (const id of [
    'sidebar', 'scrim', 'project-btn', 'project-name', 'sidebar-close', 'new-session',
    'session-list', 'host-line', 'menu-btn', 'session-avatar', 'session-title', 'session-sub', 'auto-btn', 'conn',
    'conn-text', 'banner', 'scroller', 'empty', 'messages', 'jump', 'composer-status',
    'busy-text', 'queued', 'composer-form', 'prompt', 'stop', 'send', 'token-screen',
    'token-form', 'token-input', 'token-error', 'sheet-root', 'sheet', 'toast',
  ]) {
    ui[id.replace(/-(\w)/g, (_, c) => c.toUpperCase())] = $(id);
  }
  takeTokenFromHash();
  bindUI();
  ui.prompt.value = lsGet(DRAFT_KEY) || '';
  autosize();
  if (!getToken()) {
    showTokenScreen(false);
    return;
  }
  start();
}

let startTimer = 0;

async function start() {
  clearTimeout(startTimer);
  hideTokenScreen();
  setConn('connecting');
  try {
    state.info = await api('GET', '/remote/api/info');
  } catch (e) {
    if (e instanceof UnauthorizedError) return showTokenScreen(true);
    setConn('reconnecting');
    startTimer = setTimeout(start, 3000);
    return;
  }
  const host = state.info.hostname || 'This computer';
  const who = el('span', 'host-text');
  who.append(el('span', 'host-name', host));
  if (state.info.version) who.append(el('span', 'host-ver', 'Ta-Da ' + state.info.version));
  ui.hostLine.replaceChildren(avatar(host, host), who);
  const paths = projectPaths();
  const saved = lsGet(PATH_KEY);
  const path = saved && paths.includes(saved) ? saved : state.info.default_path || paths[0] || '';
  await joinProject(path);
}

function showTokenScreen(rejected) {
  stopStream();
  setConn('unauthorized');
  ui.tokenError.hidden = !rejected;
  ui.tokenError.textContent = 'That token was not accepted. Check it and try again.';
  ui.tokenScreen.hidden = false;
  ui.tokenInput.value = '';
  setTimeout(() => ui.tokenInput.focus(), 50);
}

function hideTokenScreen() {
  ui.tokenScreen.hidden = true;
}

// Handles an error from a user action. Returns true when it was an auth
// failure (the token screen is now up).
function fail(e, what) {
  if (e instanceof UnauthorizedError) {
    showTokenScreen(true);
    return true;
  }
  console.warn(what, e);
  toast(what + ': ' + (e && e.message ? e.message : String(e)), true);
  return false;
}

// ---------------------------------------------------------------------------
// Projects and workspaces.

function projectPaths() {
  const info = state.info || {};
  const list = (info.projects || []).map((p) => p.path);
  if (info.default_path && !list.includes(info.default_path)) list.unshift(info.default_path);
  return list;
}

function projectName(path) {
  const p = ((state.info && state.info.projects) || []).find((x) => x.path === path);
  if (p && p.name) return p.name;
  return path.split(/[\\/]/).filter(Boolean).pop() || path;
}

async function ensureWorkspace() {
  const ws = await api('POST', '/v1/workspaces', {
    path: state.path,
    client_id: clientID(),
    version: (state.info && state.info.version) || '',
    yolo: !!(state.info && state.info.start_auto),
  });
  state.ws = { id: ws.id, path: ws.path || state.path };
  syncAuto(ws.id);
  return state.ws;
}

// Auto mode: permission prompts off for the workspace. A host started
// with --auto turns it on for every workspace the app joins; the switch
// shows only when the host allows it.
async function syncAuto(wsID) {
  const info = state.info || {};
  if (!info.allow_skip_permissions) return renderAuto();
  try {
    const url = `/v1/workspaces/${enc(wsID)}/permissions/skip`;
    if (info.start_auto) await api('POST', url, { skip: true });
    const r = await api('GET', url);
    if (state.ws && state.ws.id === wsID) state.auto = !!(r && r.skip);
  } catch (e) {
    console.warn('Auto mode', e);
  }
  renderAuto();
}

async function toggleAuto() {
  const next = !state.auto;
  ui.autoBtn.disabled = true;
  try {
    await wsApi('POST', '/permissions/skip', { skip: next });
    state.auto = next;
    toast(next ? 'Auto mode on: the agent runs without asking' : 'Auto mode off: the agent asks before acting');
  } catch (e) {
    fail(e, 'Could not change Auto mode');
  } finally {
    ui.autoBtn.disabled = false;
    renderAuto();
  }
}

function renderAuto() {
  const allowed = !!(state.info && state.info.allow_skip_permissions);
  ui.autoBtn.hidden = !allowed;
  ui.autoBtn.setAttribute('aria-pressed', state.auto ? 'true' : 'false');
  ui.autoBtn.title = state.auto ? 'Auto mode is on. Tap to require permission again.' : 'Turn on Auto mode: no permission prompts';
}

function isWorkspaceGone(e) {
  return e instanceof HttpError && e.status === 404 && /workspace not found/i.test(e.message);
}

// Calls a workspace-scoped endpoint, re-creating the workspace once if the
// server reports it gone.
async function wsApi(method, sub, body) {
  if (!state.ws) await ensureWorkspace();
  try {
    return await api(method, `/v1/workspaces/${enc(state.ws.id)}${sub}`, body);
  } catch (e) {
    if (!isWorkspaceGone(e)) throw e;
    await ensureWorkspace();
    kickStream();
    return api(method, `/v1/workspaces/${enc(state.ws.id)}${sub}`, body);
  }
}

async function joinProject(path) {
  stopStream();
  state.path = path;
  lsSet(PATH_KEY, path);
  state.ws = null;
  state.sessions.clear();
  state.busy.clear();
  state.perms = [];
  state.question = null;
  if (state.sheet === 'permission' || state.sheet === 'question') closeSheet();
  state.sessionId = null;
  resetConversation();
  ui.projectName.textContent = projectName(path);
  renderSessions();
  renderHeader();
  try {
    await ensureWorkspace();
  } catch (e) {
    if (e instanceof UnauthorizedError) return showTokenScreen(true);
    toast('Could not open ' + projectName(path) + ': ' + e.message, true);
  }
  startStream();
  await loadSessions();
  const saved = lsGet(sessionKey());
  if (saved && state.sessions.has(saved)) await openSession(saved);
}

// ---------------------------------------------------------------------------
// Event stream.

let streamGen = 0;
let streamAbort = null;
let wake = null;
let connected = false;

function startStream() {
  stopStream();
  runStream(++streamGen);
}

function stopStream() {
  streamGen++;
  connected = false;
  if (streamAbort) streamAbort.abort();
  streamAbort = null;
  if (wake) wake();
}

// Drops the current connection (or skips the backoff wait) so the loop
// reconnects right away.
function kickStream() {
  if (streamAbort) streamAbort.abort();
  if (wake) wake();
}

function sleepOrWake(ms) {
  return new Promise((resolve) => {
    const done = () => {
      clearTimeout(t);
      wake = null;
      resolve();
    };
    const t = setTimeout(done, ms);
    wake = done;
  });
}

async function runStream(gen) {
  let attempt = 0;
  while (gen === streamGen) {
    const ctrl = new AbortController();
    streamAbort = ctrl;
    try {
      if (!state.ws) await ensureWorkspace();
      const path = `/v1/workspaces/${enc(state.ws.id)}/events?client_id=${enc(clientID())}`;
      await streamEvents(path, {
        signal: ctrl.signal,
        onOpen: () => {
          if (gen !== streamGen) return;
          attempt = 0;
          connected = true;
          setConn('connected');
          afterConnect();
        },
        onEvent: (ev) => {
          if (gen === streamGen) handleEvent(ev);
        },
      });
    } catch (e) {
      if (gen !== streamGen) return;
      if (e instanceof UnauthorizedError) {
        showTokenScreen(true);
        return;
      }
      if (e instanceof HttpError) {
        if (e.status === 404) state.ws = null;
        else if (e.status === 409 && /retired/i.test(e.message)) {
          resetClientID();
          state.ws = null;
        }
      }
      if (e.name !== 'AbortError') console.warn('Event stream ended', e);
    }
    if (gen !== streamGen) return;
    connected = false;
    setConn('reconnecting');
    const delay = Math.min(15000, 500 * 2 ** attempt) * (0.75 + Math.random() * 0.5);
    attempt++;
    await sleepOrWake(delay);
  }
}

// Runs after every successful (re)connect: re-attach the current session so
// the server replays its pending prompts, then re-fetch state.
async function afterConnect() {
  if (state.sessionId) reportSession(state.sessionId);
  await loadSessions();
  if (state.sessionId) {
    await loadMessages(state.sessionId);
    refreshBusy();
  }
  checkAgent();
}

async function reportSession(sid) {
  if (!connected) return; // afterConnect reports it once the stream is up.
  try {
    await wsApi('POST', `/current-session?client_id=${enc(clientID())}`, { session_id: sid });
  } catch (e) {
    if (e instanceof UnauthorizedError) return showTokenScreen(true);
    // A 409 means the stream is not attached yet; afterConnect retries.
    if (!(e instanceof HttpError && e.status === 409)) console.warn('current-session', e);
  }
}

function setConn(s) {
  ui.conn.dataset.state = s;
  ui.connText.textContent = {
    connecting: 'Connecting',
    connected: 'Connected',
    reconnecting: 'Reconnecting',
    unauthorized: 'Signed out',
  }[s];
}

function handleEvent(ev) {
  if (!ev || typeof ev !== 'object') return;
  const inner = ev.payload || {};
  const wrapped = inner && typeof inner === 'object' && 'payload' in inner &&
    ['created', 'updated', 'deleted'].includes(inner.type);
  const kind = wrapped ? inner.type : 'updated';
  const data = wrapped ? inner.payload : inner;
  if (!data) return;
  switch (ev.type) {
    case 'message':
      onMessage(kind, data);
      break;
    case 'session':
      onSession(kind, data);
      break;
    case 'permission_request':
      onPermission(data);
      break;
    case 'permission_notification':
      onPermissionResolved(data);
      break;
    case 'question_batch_request':
      onQuestion(data);
      break;
    case 'question_batch_notification':
      onQuestionResolved(data);
      break;
    case 'run_complete':
      onRunComplete(data);
      break;
    case 'agent_event':
      onAgentEvent(data);
      break;
    default:
      break;
  }
}

// ---------------------------------------------------------------------------
// Sessions.

async function loadSessions() {
  if (!state.ws && !state.path) return;
  let list;
  try {
    list = await wsApi('GET', '/sessions');
  } catch (e) {
    if (e instanceof UnauthorizedError) showTokenScreen(true);
    return;
  }
  state.sessions.clear();
  for (const s of list || []) {
    if (s.parent_session_id) continue;
    state.sessions.set(s.id, s);
    if (s.is_busy) state.busy.add(s.id);
    else if (Date.now() - state.lastSend > 4000 || s.id !== state.sessionId) state.busy.delete(s.id);
  }
  renderSessions();
  renderHeader();
  renderStatus();
}

function onSession(kind, s) {
  if (s.parent_session_id) return;
  if (kind === 'deleted') {
    state.sessions.delete(s.id);
    state.busy.delete(s.id);
    if (s.id === state.sessionId) newSession();
  } else {
    state.sessions.set(s.id, s);
  }
  renderSessions();
  if (s.id === state.sessionId) renderHeader();
}

function relTime(t) {
  if (!t) return '';
  const ms = t > 1e12 ? t : t * 1000;
  const d = (Date.now() - ms) / 1000;
  if (d < 60) return 'just now';
  if (d < 3600) return Math.floor(d / 60) + 'm ago';
  if (d < 86400) return Math.floor(d / 3600) + 'h ago';
  if (d < 7 * 86400) return Math.floor(d / 86400) + 'd ago';
  return new Date(ms).toLocaleDateString();
}

function renderSessions() {
  const list = [...state.sessions.values()].sort((a, b) => (b.updated_at || 0) - (a.updated_at || 0));
  const items = list.map((s) => {
    const li = el('li');
    const btn = el('button', 'session-item');
    btn.type = 'button';
    btn.setAttribute('aria-current', s.id === state.sessionId ? 'true' : 'false');
    const title = s.title || 'Untitled session';
    const text = el('span', 's-text');
    text.append(el('span', 's-title', title));
    const meta = el('span', 's-meta');
    meta.append(el('span', '', relTime(s.updated_at)));
    if (state.busy.has(s.id)) meta.append(el('span', 's-busy', 'working'));
    text.append(meta);
    btn.append(avatar(s.id, title), text);
    btn.addEventListener('click', () => {
      closeDrawer();
      if (s.id !== state.sessionId) openSession(s.id);
    });
    li.append(btn);
    return li;
  });
  if (!items.length) {
    const li = el('li', 'list-empty', 'No sessions yet');
    items.push(li);
  }
  ui.sessionList.replaceChildren(...items);
}

function renderHeader() {
  const s = state.sessionId && state.sessions.get(state.sessionId);
  ui.sessionTitle.textContent = s ? s.title || 'Untitled session' : 'New session';
  ui.sessionAvatar.replaceChildren(s ? avatar(s.id, s.title || 'U', 'avatar-sm') : el('span', 'avatar avatar-sm avatar-brand', '✦'));
  ui.sessionSub.textContent = state.path ? projectName(state.path) : '';
  document.title = (s && s.title ? s.title + ' - ' : '') + 'Ta-Da Remote';
}

let loadGen = 0;

async function openSession(id) {
  state.sessionId = id;
  lsSet(sessionKey(), id);
  resetConversation();
  state.stick = true;
  state.queued = 0;
  renderSessions();
  renderHeader();
  renderStatus();
  reportSession(id);
  await loadMessages(id);
  refreshBusy();
}

function newSession() {
  state.sessionId = null;
  lsSet(sessionKey(), null);
  resetConversation();
  state.queued = 0;
  renderSessions();
  renderHeader();
  renderStatus();
  closeDrawer();
  ui.prompt.focus();
}

async function loadMessages(id) {
  const gen = ++loadGen;
  let msgs;
  try {
    msgs = await wsApi('GET', `/sessions/${enc(id)}/messages`);
  } catch (e) {
    if (gen === loadGen) fail(e, 'Could not load messages');
    return;
  }
  if (gen !== loadGen || id !== state.sessionId) return;
  const next = new Map();
  for (const m of msgs || []) {
    const cur = state.messages.get(m.id);
    next.set(m.id, cur && (cur.updated_at || 0) > (m.updated_at || 0) ? cur : m);
  }
  // Keep live messages that arrived after the fetch started.
  for (const [mid, m] of state.messages) if (!next.has(mid)) next.set(mid, m);
  state.messages = next;
  state.order = [...next.keys()];
  renderAll();
}

async function refreshBusy() {
  const sid = state.sessionId;
  if (!sid || !state.ws) return;
  try {
    const [info, queued] = await Promise.all([
      wsApi('GET', `/agent/sessions/${enc(sid)}`),
      wsApi('GET', `/agent/sessions/${enc(sid)}/prompts/queued`),
    ]);
    if (sid !== state.sessionId) return;
    if (info && info.is_busy) state.busy.add(sid);
    else if (Date.now() - state.lastSend > 4000) state.busy.delete(sid);
    state.queued = typeof queued === 'number' ? queued : 0;
    renderStatus();
    renderSessions();
  } catch (e) {
    if (e instanceof UnauthorizedError) showTokenScreen(true);
  }
}

async function checkAgent() {
  try {
    const a = await wsApi('GET', '/agent');
    if (a && a.is_ready === false) {
      ui.banner.textContent = 'No model is ready on the host yet. Set one up in Ta-Da on your computer.';
      ui.banner.hidden = false;
    } else {
      ui.banner.hidden = true;
    }
  } catch (e) {
    if (e instanceof UnauthorizedError) showTokenScreen(true);
  }
}

function onRunComplete(rc) {
  state.busy.delete(rc.session_id);
  renderSessions();
  if (rc.session_id === state.sessionId) {
    renderStatus();
    if (rc.error && !rc.cancelled) toast(rc.error, true);
    rerenderOpenCalls();
    // A queued prompt may start right away; ask the server.
    setTimeout(refreshBusy, 300);
  }
  loadSessions();
}

function onAgentEvent(ev) {
  if (ev.session_id && ev.session_id !== state.sessionId) return;
  if (ev.type === 'error') toast(ev.error || ev.message || 'Agent error', true);
}

// ---------------------------------------------------------------------------
// Messages.

function resetConversation() {
  loadGen++;
  state.messages = new Map();
  state.order = [];
  state.results = new Map();
  state.callOwner = new Map();
  state.nodes = new Map();
  ui.messages.replaceChildren();
  ui.empty.hidden = !!state.sessionId;
  ui.jump.hidden = true;
}

function indexMessage(m) {
  for (const p of m.parts || []) {
    const d = p.data || {};
    if (p.type === 'tool_call' && d.id) state.callOwner.set(d.id, m.id);
    if (p.type === 'tool_result' && d.tool_call_id) state.results.set(d.tool_call_id, d);
  }
}

function hasFinish(m) {
  return (m.parts || []).some((p) => p.type === 'finish');
}

function finishReason(m) {
  const p = (m.parts || []).find((x) => x.type === 'finish');
  return p && p.data ? p.data.reason : '';
}

function onMessage(kind, m) {
  if (m.session_id !== state.sessionId) {
    if (m.role === 'assistant' && kind !== 'deleted' && !hasFinish(m) && state.sessions.has(m.session_id)) {
      state.busy.add(m.session_id);
      renderSessions();
    }
    return;
  }
  if (kind === 'deleted') {
    state.messages.delete(m.id);
    state.order = state.order.filter((x) => x !== m.id);
    const node = state.nodes.get(m.id);
    if (node) node.remove();
    state.nodes.delete(m.id);
    return;
  }
  if (!state.messages.has(m.id)) state.order.push(m.id);
  state.messages.set(m.id, m);
  indexMessage(m);
  if (m.role === 'assistant' && !hasFinish(m) && !state.busy.has(m.session_id)) {
    state.busy.add(m.session_id);
    renderStatus();
    renderSessions();
  }
  ui.empty.hidden = true;
  renderMessage(m);
  if (m.role === 'tool') {
    const owners = new Set();
    for (const p of m.parts || []) {
      const owner = p.type === 'tool_result' && state.callOwner.get(p.data && p.data.tool_call_id);
      if (owner) owners.add(owner);
    }
    for (const id of owners) renderMessage(state.messages.get(id));
  }
  afterRender();
}

function renderAll() {
  state.results = new Map();
  state.callOwner = new Map();
  for (const id of state.order) indexMessage(state.messages.get(id));
  state.nodes = new Map();
  ui.messages.replaceChildren();
  for (const id of state.order) renderMessage(state.messages.get(id));
  ui.empty.hidden = !!state.sessionId || state.order.length > 0;
  afterRender(true);
}

// Re-renders assistant messages so tool cards drop the "running" label
// once the run is over.
function rerenderOpenCalls() {
  for (const id of state.order) {
    const m = state.messages.get(id);
    if (m && m.role === 'assistant' && (m.parts || []).some((p) => p.type === 'tool_call')) renderMessage(m);
  }
}

function renderMessage(m) {
  if (!m) return;
  const fresh = buildMessage(m);
  const old = state.nodes.get(m.id);
  if (!fresh) {
    if (old) old.remove();
    state.nodes.delete(m.id);
    return;
  }
  if (old) {
    old.replaceWith(fresh);
  } else {
    const idx = state.order.indexOf(m.id);
    let before = null;
    for (let j = idx + 1; j < state.order.length && !before; j++) before = state.nodes.get(state.order[j]) || null;
    ui.messages.insertBefore(fresh, before);
  }
  state.nodes.set(m.id, fresh);
}

function attachmentChip(label) {
  return el('span', 'attach', label);
}

function buildMessage(m) {
  const parts = m.parts || [];
  if (m.role === 'user') {
    const wrap = el('div', 'msg msg-user');
    const text = parts
      .filter((p) => p.type === 'text' && p.data && !p.data.hidden)
      .map((p) => p.data.text)
      .join('\n');
    if (text) wrap.append(text);
    for (const p of parts) {
      if (p.type === 'binary') {
        const d = p.data || {};
        const name = (d.Path || d.path || '').split(/[\\/]/).pop() || d.MIMEType || 'attachment';
        wrap.append(el('br'), attachmentChip(name));
      } else if (p.type === 'image_url') {
        wrap.append(el('br'), attachmentChip('image'));
      } else if (p.type === 'shell_command') {
        wrap.append(shellCard(p.data || {}, m));
      }
    }
    return wrap.childNodes.length ? wrap : null;
  }

  if (m.role === 'tool') {
    // Results render inside their call's card; only orphans show here.
    const orphans = parts.filter(
      (p) => p.type === 'tool_result' && p.data && !state.callOwner.has(p.data.tool_call_id),
    );
    if (!orphans.length) return null;
    const wrap = el('div', 'msg msg-assistant');
    for (const p of orphans) {
      wrap.append(toolCard({ id: p.data.tool_call_id, name: p.data.name, input: '' }, p.data, m));
    }
    return wrap;
  }

  if (m.role === 'system') {
    const text = parts.filter((p) => p.type === 'text' && p.data && !p.data.hidden).map((p) => p.data.text).join('\n');
    return text ? el('div', 'msg msg-system', text) : null;
  }

  const wrap = el('div', 'msg msg-assistant');
  if (m.is_summary_message) wrap.append(el('div', 'msg-label', 'Conversation summary'));
  const finished = hasFinish(m);
  for (const p of parts) {
    const d = p.data || {};
    switch (p.type) {
      case 'reasoning':
        if (d.thinking && d.thinking.trim()) wrap.append(reasoningNode(m.id, d, finished));
        break;
      case 'text':
        if (!d.hidden && d.text) {
          const md = el('div', 'md');
          renderMarkdown(d.text, md);
          wrap.append(md);
        }
        break;
      case 'tool_call':
        wrap.append(toolCard(d, state.results.get(d.id), m));
        break;
      case 'shell_command':
        wrap.append(shellCard(d, m));
        break;
      case 'image_url':
        wrap.append(attachmentChip('image'));
        break;
      case 'binary':
        wrap.append(attachmentChip(d.MIMEType || 'file'));
        break;
      case 'finish': {
        const note = finishNote(d);
        if (note) wrap.append(note);
        break;
      }
      default:
        break;
    }
  }
  return wrap.childNodes.length ? wrap : null;
}

function finishNote(d) {
  switch (d.reason) {
    case 'canceled':
      return el('div', 'finish-note', 'Canceled');
    case 'error': {
      const text = [d.message || 'The request failed.', d.details].filter(Boolean).join('\n');
      return el('div', 'finish-note error', text);
    }
    case 'max_tokens':
      return el('div', 'finish-note', 'Stopped: output limit reached');
    case 'content_filter':
      return el('div', 'finish-note', 'Stopped by the content filter');
    default:
      return null;
  }
}

function keepOpen(details, key) {
  details.open = state.openDetails.has(key);
  details.addEventListener('toggle', () => {
    if (details.open) state.openDetails.add(key);
    else state.openDetails.delete(key);
  });
}

function reasoningNode(msgId, d, finished) {
  const det = el('details', 'reasoning');
  keepOpen(det, 'reason:' + msgId);
  det.append(el('summary', '', d.finished_at || finished ? 'Thought' : 'Thinking'));
  det.append(el('div', 'reasoning-body', d.thinking));
  return det;
}

function parseInput(raw) {
  if (raw && typeof raw === 'object') return raw;
  if (typeof raw !== 'string' || !raw.trim()) return {};
  try {
    const v = JSON.parse(raw);
    return v && typeof v === 'object' ? v : {};
  } catch {
    return null;
  }
}

function oneLine(s, max) {
  const t = String(s).replace(/\s+/g, ' ').trim();
  return t.length > max ? t.slice(0, max - 1) + '…' : t;
}

function shortPath(p) {
  if (typeof p !== 'string') return p;
  if (state.path && p.startsWith(state.path + '/')) return p.slice(state.path.length + 1);
  return p;
}

function inputSummary(name, inp, raw) {
  const n = (name || '').toLowerCase();
  if (!inp) return typeof raw === 'string' ? oneLine(raw, 140) : '';
  const str = (k) => (typeof inp[k] === 'string' && inp[k] ? inp[k] : '');
  let s = '';
  if (n === 'bash') s = str('command');
  else if (n === 'grep' || n === 'glob') s = str('pattern') + (str('path') ? '  in ' + shortPath(str('path')) : '');
  if (!s) {
    for (const k of ['file_path', 'path', 'url', 'pattern', 'query', 'prompt', 'command', 'description', 'name']) {
      if (str(k)) {
        s = /path$/.test(k) ? shortPath(str(k)) : str(k);
        break;
      }
    }
  }
  if (!s) {
    const v = Object.values(inp).find((x) => typeof x === 'string' && x);
    if (v) s = v;
  }
  return oneLine(s, 140);
}

const LIMIT_LINES = 24;
const LIMIT_CHARS = 2400;

// Renders text in a <pre>, truncated with an expand control.
function truncBlock(text, key, cls) {
  const box = el('div');
  const pre = el('pre', cls || '');
  const lines = text.split('\n');
  const long = lines.length > LIMIT_LINES || text.length > LIMIT_CHARS;
  if (long && !state.openFull.has(key)) {
    pre.textContent = lines.slice(0, LIMIT_LINES).join('\n').slice(0, LIMIT_CHARS) + '\n…';
    const more = el('button', 'more-btn', `Show all (${lines.length} lines)`);
    more.type = 'button';
    more.addEventListener('click', () => {
      state.openFull.add(key);
      pre.textContent = text;
      more.remove();
    });
    box.append(pre, more);
  } else {
    pre.textContent = text;
    box.append(pre);
  }
  return box;
}

function labeled(label, node) {
  const box = el('div');
  box.append(el('div', 'label', label), node);
  return box;
}

function toolCard(call, result, m) {
  const key = 'tool:' + call.id;
  const det = el('details', 'tool');
  keepOpen(det, key);
  const inp = parseInput(call.input);
  const sum = el('summary');
  sum.append(el('span', 'tool-name', call.name || 'tool'));
  sum.append(el('span', 'tool-sum', inputSummary(call.name, inp, call.input)));
  let label = '';
  let cls = '';
  if (result) {
    if (result.is_error) {
      label = 'Error';
      cls = 'error';
      det.classList.add('is-error');
    } else {
      label = 'Done';
    }
  } else if (finishReason(m) === 'canceled') {
    label = 'Canceled';
  } else if (state.busy.has(m.session_id)) {
    label = 'Running';
    cls = 'running';
  } else {
    label = 'No result';
  }
  sum.append(el('span', 'tool-state ' + cls, label));
  det.append(sum);

  const body = el('div', 'tool-body');
  const n = (call.name || '').toLowerCase();
  if (inp === null) {
    if (call.input) body.append(labeled('Input', truncBlock(String(call.input), key + ':in')));
  } else if (n === 'bash' && inp.command) {
    body.append(labeled('Command', truncBlock('$ ' + inp.command, key + ':in')));
  } else if ((n === 'edit' || n === 'multiedit') && (inp.old_string !== undefined || inp.edits)) {
    const edits = n === 'multiedit' && Array.isArray(inp.edits) ? inp.edits : [inp];
    const file = shortPath(inp.file_path || '');
    for (const e of edits) body.append(labeled(file || 'Edit', diffView(e.old_string || '', e.new_string || '')));
  } else if (n === 'write' && typeof inp.content === 'string') {
    body.append(labeled(shortPath(inp.file_path || 'Content'), truncBlock(inp.content, key + ':in')));
  } else if (Object.keys(inp).length) {
    body.append(labeled('Input', truncBlock(JSON.stringify(inp, null, 2), key + ':in')));
  }
  if (result) {
    let content = result.content || '';
    if (!content && result.mime_type) content = '[' + result.mime_type + ' output]';
    if (content) body.append(labeled(result.is_error ? 'Error' : 'Result', truncBlock(content, key + ':out', result.is_error ? 'err' : '')));
  }
  if (body.childNodes.length) det.append(body);
  return det;
}

function shellCard(d, m) {
  const call = { id: 'shell:' + m.id + ':' + (d.command || ''), name: 'shell', input: '' };
  const det = el('details', 'tool');
  keepOpen(det, 'tool:' + call.id);
  const sum = el('summary');
  sum.append(el('span', 'tool-name', '!'), el('span', 'tool-sum', oneLine(d.command || '', 140)));
  const failed = d.exit_code && d.exit_code !== 0;
  if (failed) det.classList.add('is-error');
  sum.append(el('span', 'tool-state ' + (failed ? 'error' : ''), failed ? 'Exit ' + d.exit_code : 'Done'));
  det.append(sum);
  const body = el('div', 'tool-body');
  body.append(labeled('Command', truncBlock('$ ' + (d.command || ''), 'tool:' + call.id + ':in')));
  if (d.output) body.append(labeled('Output', truncBlock(d.output, 'tool:' + call.id + ':out', failed ? 'err' : '')));
  det.append(body);
  return det;
}

// Line diff using an LCS table on the changed middle section.
function lineDiff(a, b) {
  const A = a.split('\n');
  const B = b.split('\n');
  let pre = 0;
  while (pre < A.length && pre < B.length && A[pre] === B[pre]) pre++;
  let suf = 0;
  while (suf < A.length - pre && suf < B.length - pre && A[A.length - 1 - suf] === B[B.length - 1 - suf]) suf++;
  const a2 = A.slice(pre, A.length - suf);
  const b2 = B.slice(pre, B.length - suf);
  const out = A.slice(0, pre).map((s) => [' ', s]);
  const n = a2.length;
  const mm = b2.length;
  if (n * mm > 250000) {
    for (const s of a2) out.push(['-', s]);
    for (const s of b2) out.push(['+', s]);
  } else {
    const w = mm + 1;
    const t = new Uint32Array((n + 1) * w);
    for (let i = n - 1; i >= 0; i--) {
      for (let j = mm - 1; j >= 0; j--) {
        t[i * w + j] = a2[i] === b2[j] ? t[(i + 1) * w + j + 1] + 1 : Math.max(t[(i + 1) * w + j], t[i * w + j + 1]);
      }
    }
    let i = 0;
    let j = 0;
    while (i < n || j < mm) {
      if (i < n && j < mm && a2[i] === b2[j]) {
        out.push([' ', a2[i]]);
        i++;
        j++;
      } else if (j < mm && (i >= n || t[i * w + j + 1] >= t[(i + 1) * w + j])) {
        out.push(['+', b2[j]]);
        j++;
      } else {
        out.push(['-', a2[i]]);
        i++;
      }
    }
  }
  for (const s of A.slice(A.length - suf)) out.push([' ', s]);
  return out;
}

function diffView(a, b) {
  const rows = lineDiff(a, b);
  const CONTEXT = 3;
  const keep = new Array(rows.length).fill(false);
  rows.forEach((r, i) => {
    if (r[0] !== ' ') for (let k = Math.max(0, i - CONTEXT); k <= Math.min(rows.length - 1, i + CONTEXT); k++) keep[k] = true;
  });
  const box = el('div', 'diff');
  let skipped = false;
  let shown = 0;
  for (let i = 0; i < rows.length; i++) {
    if (!keep[i]) {
      if (!skipped) box.append(el('div', 'gap', '…'));
      skipped = true;
      continue;
    }
    skipped = false;
    if (++shown > 400) {
      box.append(el('div', 'gap', `… ${rows.length - i} more lines`));
      break;
    }
    const [op, s] = rows[i];
    box.append(el('div', op === '+' ? 'add' : op === '-' ? 'del' : '', op + ' ' + s));
  }
  if (!box.childNodes.length) box.append(el('div', 'gap', 'No changes'));
  return box;
}

// ---------------------------------------------------------------------------
// Scrolling.

function nearBottom() {
  const s = ui.scroller;
  return s.scrollHeight - s.scrollTop - s.clientHeight < 60;
}

let scrollQueued = false;

function afterRender(force) {
  if (force) state.stick = true;
  if (scrollQueued) return;
  scrollQueued = true;
  requestAnimationFrame(() => {
    scrollQueued = false;
    if (state.stick) {
      ui.scroller.scrollTop = ui.scroller.scrollHeight;
      ui.jump.hidden = true;
    } else {
      ui.jump.hidden = false;
    }
  });
}

// ---------------------------------------------------------------------------
// Composer.

function renderStatus() {
  const busy = !!state.sessionId && state.busy.has(state.sessionId);
  ui.stop.hidden = !busy;
  ui.composerStatus.hidden = !busy && state.queued <= 0;
  ui.busyText.textContent = busy ? 'Working' : 'Waiting';
  ui.queued.hidden = state.queued <= 0;
  ui.queued.textContent = state.queued > 0 ? `· ${state.queued} queued` : '';
  updateComposerHeight();
}

function autosize() {
  ui.prompt.style.height = 'auto';
  ui.prompt.style.height = Math.min(ui.prompt.scrollHeight + 2, window.innerHeight * 0.4) + 'px';
  updateComposerHeight();
}

function updateComposerHeight() {
  const h = document.querySelector('.composer').offsetHeight;
  document.documentElement.style.setProperty('--composer-h', h + 'px');
}

let sending = false;

async function send() {
  const text = ui.prompt.value.trim();
  if (!text || sending) return;
  sending = true;
  ui.send.disabled = true;
  try {
    let sid = state.sessionId;
    if (!sid) {
      const s = await wsApi('POST', '/sessions', { title: 'New Session' });
      state.sessions.set(s.id, s);
      await openSession(s.id);
      sid = s.id;
    }
    await wsApi('POST', '/agent', { session_id: sid, prompt: text, run_id: uuid() });
    if (ui.prompt.value.trim() === text) ui.prompt.value = '';
    lsSet(DRAFT_KEY, ui.prompt.value);
    autosize();
    state.lastSend = Date.now();
    state.busy.add(sid);
    state.stick = true;
    renderStatus();
    renderSessions();
    afterRender();
    setTimeout(refreshBusy, 600);
  } catch (e) {
    if (e instanceof HttpError && e.status === 409) toast('Ta-Da could not take the prompt: ' + e.message, true);
    else fail(e, 'Send failed');
  } finally {
    sending = false;
    ui.send.disabled = false;
  }
}

async function stop() {
  const sid = state.sessionId;
  if (!sid) return;
  ui.stop.disabled = true;
  try {
    await wsApi('POST', `/agent/sessions/${enc(sid)}/cancel`);
    setTimeout(refreshBusy, 500);
  } catch (e) {
    fail(e, 'Stop failed');
  } finally {
    ui.stop.disabled = false;
  }
}

// ---------------------------------------------------------------------------
// Sheets: permission, question, project.

function openSheet(kind, build) {
  state.sheet = kind;
  ui.sheet.replaceChildren();
  build(ui.sheet);
  ui.sheetRoot.hidden = false;
  ui.sheet.scrollTop = 0;
}

function closeSheet() {
  state.sheet = null;
  ui.sheetRoot.hidden = true;
  ui.sheet.replaceChildren();
}

// Shows the most urgent pending prompt, if no other prompt is up.
function showNextSheet() {
  if (state.sheet === 'permission' || state.sheet === 'question') return;
  if (state.perms.length) openSheet('permission', (root) => buildPermission(root, state.perms[0]));
  else if (state.question) openSheet('question', (root) => buildQuestion(root, state.question));
}

function onPermission(req) {
  if (!req || state.perms.some((p) => p.id === req.id)) return;
  state.perms.push(req);
  if (state.sheet === 'project') closeSheet();
  if (state.sheet === 'permission') {
    // Refresh the "1 of N" count.
    const cur = state.perms[0];
    openSheet('permission', (root) => buildPermission(root, cur));
  } else {
    showNextSheet();
  }
}

function onPermissionResolved(n) {
  // The backend also sends a "requested" notification (neither flag set)
  // just before each request; only a decision dismisses the sheet.
  if (!n.granted && !n.denied) return;
  const before = state.perms[0];
  state.perms = state.perms.filter((p) => p.tool_call_id !== n.tool_call_id);
  if (state.sheet === 'permission' && before && before.tool_call_id === n.tool_call_id) {
    closeSheet();
    showNextSheet();
  }
}

function onQuestion(q) {
  if (!q) return;
  state.question = q;
  if (state.sheet === 'project') closeSheet();
  showNextSheet();
}

function onQuestionResolved(n) {
  if (state.question && state.question.id === n.batch_id) {
    state.question = null;
    if (state.sheet === 'question') {
      closeSheet();
      showNextSheet();
    }
  }
}

function kv(pairs) {
  const dl = el('dl', 'kv');
  for (const [k, v] of pairs) {
    if (v === undefined || v === null || v === '') continue;
    dl.append(el('dt', '', k), el('dd', '', v));
  }
  return dl.childNodes.length ? dl : null;
}

function buildPermission(root, req) {
  const count = state.perms.length > 1 ? ` (1 of ${state.perms.length})` : '';
  root.append(el('div', 'sheet-kicker', 'Permission requested' + count));
  root.append(el('h2', '', req.tool_name || 'Tool'));
  if (req.description) root.append(el('p', 'sheet-desc', req.description));

  const params = req.params && typeof req.params === 'object' ? { ...req.params } : {};
  const other = req.session_id && req.session_id !== state.sessionId && state.sessions.get(req.session_id);
  const facts = kv([
    ['Action', req.action],
    ['Path', req.path],
    ['File', params.file_path ? shortPath(params.file_path) : ''],
    ['URL', typeof params.url === 'string' ? params.url : ''],
    ['Dir', typeof params.working_dir === 'string' ? params.working_dir : ''],
    ['Session', other ? other.title || 'another session' : ''],
    ['Reason', req.reason],
  ]);
  if (facts) root.append(facts);
  delete params.file_path;
  delete params.url;
  delete params.working_dir;

  if (typeof params.command === 'string') {
    root.append(el('div', 'field-label', 'Command'));
    root.append(el('pre', 'block', '$ ' + params.command));
    delete params.command;
    delete params.description;
  }
  if ('old_content' in params || 'new_content' in params) {
    root.append(el('div', 'field-label', 'Changes'));
    root.append(diffView(params.old_content || '', params.new_content || ''));
    delete params.old_content;
    delete params.new_content;
  }
  const rest = Object.entries(params).filter(([, v]) => v !== '' && v !== null && v !== undefined && v !== false && v !== 0);
  if (rest.length) {
    root.append(el('div', 'field-label', 'Parameters'));
    const text = JSON.stringify(Object.fromEntries(rest), null, 2);
    root.append(el('pre', 'block', text.length > 6000 ? text.slice(0, 6000) + '\n…' : text));
  }

  const actions = el('div', 'sheet-actions');
  const mk = (label, action, cls) => {
    const b = el('button', 'btn ' + cls, label);
    b.type = 'button';
    b.addEventListener('click', async () => {
      for (const x of actions.querySelectorAll('button')) x.disabled = true;
      try {
        await wsApi('POST', '/permissions/grant', { permission: req, action });
        state.perms = state.perms.filter((p) => p.id !== req.id);
        closeSheet();
        showNextSheet();
      } catch (e) {
        for (const x of actions.querySelectorAll('button')) x.disabled = false;
        fail(e, 'Could not send the decision');
      }
    });
    return b;
  };
  actions.append(
    mk('Allow once', 'allow', 'btn-primary'),
    mk('Allow for session', 'allow_session', ''),
    mk('Deny', 'deny', 'btn-danger'),
  );
  root.append(actions);
}

function choiceRow(type, name, value, label, desc) {
  const row = el('label', 'choice');
  const input = el('input');
  input.type = type;
  input.name = name;
  input.value = value;
  const text = el('span');
  text.append(el('span', '', label));
  if (desc) text.append(el('span', 'c-desc', desc));
  row.append(input, text);
  return row;
}

function buildQuestion(root, q) {
  const qs = q.questions || [];
  root.append(el('div', 'sheet-kicker', 'Question'));
  root.append(el('h2', '', q.confirm_title || (qs.length > 1 ? 'Ta-Da has some questions' : 'Ta-Da has a question')));
  if (q.confirm_description) root.append(el('p', 'sheet-desc', q.confirm_description));

  const form = el('form');
  qs.forEach((item, idx) => {
    const box = el('div', 'question');
    if (item.label) box.append(el('div', 'q-label', item.label));
    box.append(el('div', 'q-text', item.question || ''));
    if (item.description) box.append(el('p', 'q-desc', item.description));
    const name = 'q' + idx;
    const choices = item.choices || [];
    if (item.type === 'yes_no') {
      box.append(choiceRow('radio', name, 'yes', 'Yes'), choiceRow('radio', name, 'no', 'No'));
    } else if (item.type === 'multi_choice') {
      for (const c of choices) box.append(choiceRow('checkbox', name, c.id, c.label || c.id, c.description));
    } else if (item.type === 'single_choice' || choices.length) {
      for (const c of choices) box.append(choiceRow('radio', name, c.id, c.label || c.id, c.description));
    } else {
      const ta = el('textarea');
      ta.name = name;
      ta.rows = 3;
      box.append(ta);
    }
    form.append(box);
  });

  const actions = el('div', 'sheet-actions');
  const submit = el('button', 'btn btn-primary', 'Submit');
  submit.type = 'submit';
  const cancel = el('button', 'btn', 'Cancel');
  cancel.type = 'button';
  actions.append(submit, cancel);
  form.append(actions);

  const lock = (on) => {
    submit.disabled = on;
    cancel.disabled = on;
  };

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const responses = [];
    for (let idx = 0; idx < qs.length; idx++) {
      const item = qs[idx];
      const name = 'q' + idx;
      const checked = [...form.querySelectorAll(`input[name="${name}"]:checked`)].map((i) => i.value);
      const r = { request_id: item.id };
      if (item.type === 'yes_no') {
        if (!checked.length) return toast('Answer every question first.', true);
        r.yes = checked[0] === 'yes';
      } else if (item.type === 'multi_choice') {
        r.selected_ids = checked;
      } else if (item.type === 'single_choice' || (item.choices || []).length) {
        if (!checked.length) return toast('Answer every question first.', true);
        r.selected_ids = checked;
      } else {
        const ta = form.querySelector(`textarea[name="${name}"]`);
        r.fill_in_text = ta ? ta.value : '';
      }
      responses.push(r);
    }
    lock(true);
    try {
      await wsApi('POST', '/questions/answer', { batch_request_id: q.id, responses });
      if (state.question && state.question.id === q.id) state.question = null;
      closeSheet();
      showNextSheet();
    } catch (e) {
      lock(false);
      fail(e, 'Could not send the answer');
    }
  });

  cancel.addEventListener('click', async () => {
    lock(true);
    try {
      await wsApi('POST', '/questions/cancel');
      if (state.question && state.question.id === q.id) state.question = null;
      closeSheet();
      showNextSheet();
    } catch (e) {
      lock(false);
      fail(e, 'Could not cancel');
    }
  });

  root.append(form);
}

function openProjects() {
  if (!state.info) return;
  openSheet('project', (root) => {
    root.append(el('h2', '', 'Projects'));
    const list = el('ul', 'project-list');
    for (const path of projectPaths()) {
      const li = el('li');
      const b = el('button');
      b.type = 'button';
      b.setAttribute('aria-current', path === state.path ? 'true' : 'false');
      b.append(el('span', '', projectName(path)), el('span', 'p-path', path));
      b.addEventListener('click', () => {
        closeSheet();
        closeDrawer();
        if (path !== state.path) joinProject(path);
      });
      li.append(b);
      list.append(li);
    }
    root.append(list);
    const actions = el('div', 'sheet-actions');
    const close = el('button', 'btn', 'Close');
    close.type = 'button';
    close.addEventListener('click', closeSheet);
    actions.append(close);
    root.append(actions);
  });
}

// ---------------------------------------------------------------------------
// Misc UI.

let toastTimer = 0;

function toast(text, isError) {
  ui.toast.textContent = text;
  ui.toast.classList.toggle('error', !!isError);
  ui.toast.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    ui.toast.hidden = true;
  }, 5000);
}

function openDrawer() {
  ui.sidebar.classList.add('open');
  ui.scrim.hidden = false;
}

function closeDrawer() {
  ui.sidebar.classList.remove('open');
  ui.scrim.hidden = true;
}

function bindUI() {
  ui.menuBtn.addEventListener('click', openDrawer);
  ui.sidebarClose.addEventListener('click', closeDrawer);
  ui.scrim.addEventListener('click', closeDrawer);
  ui.projectBtn.addEventListener('click', openProjects);
  ui.newSession.addEventListener('click', newSession);
  ui.stop.addEventListener('click', stop);
  ui.autoBtn.addEventListener('click', toggleAuto);
  ui.toast.addEventListener('click', () => {
    ui.toast.hidden = true;
  });
  ui.jump.addEventListener('click', () => {
    state.stick = true;
    afterRender();
  });
  ui.scroller.addEventListener('scroll', () => {
    state.stick = nearBottom();
    if (state.stick) ui.jump.hidden = true;
  }, { passive: true });
  ui.sheetRoot.querySelector('.sheet-scrim').addEventListener('click', () => {
    if (state.sheet === 'project') closeSheet();
  });

  for (const b of document.querySelectorAll('.suggestion')) {
    b.addEventListener('click', () => {
      ui.prompt.value = b.dataset.prompt;
      autosize();
      ui.prompt.focus();
    });
  }
  ui.prompt.addEventListener('input', () => {
    lsSet(DRAFT_KEY, ui.prompt.value);
    autosize();
  });
  const finePointer = window.matchMedia('(pointer: fine)');
  ui.prompt.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Enter' || ev.isComposing) return;
    if (ev.metaKey || ev.ctrlKey || (finePointer.matches && !ev.shiftKey)) {
      ev.preventDefault();
      send();
    }
  });
  ui.composerForm.addEventListener('submit', (ev) => {
    ev.preventDefault();
    send();
  });

  ui.tokenForm.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const t = ui.tokenInput.value.trim();
    if (!t) return;
    setToken(t);
    start();
  });

  let hiddenAt = 0;
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
      hiddenAt = Date.now();
    } else if (getToken() && ui.tokenScreen.hidden) {
      // Phones often kill idle sockets silently while asleep.
      if (Date.now() - hiddenAt > 15000) kickStream();
      else if (wake) wake();
    }
  });
  window.addEventListener('online', kickStream);
  window.addEventListener('resize', updateComposerHeight);

  // Poll busy state and queued count while the open session is working,
  // and keep relative times fresh.
  setInterval(() => {
    if (connected && state.sessionId && (state.busy.has(state.sessionId) || state.queued > 0)) refreshBusy();
  }, 4000);
  setInterval(renderSessions, 60000);
}

boot();
