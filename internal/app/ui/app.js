// Agent 会话查询的只读页面。
//
// 两条铁律：
//   1. 会话内容是别人写的文本（工具输出、网页正文……），一律用 textContent 渲染，
//      禁止任何 HTML 拼接赋值 —— 页面的 token 存在 localStorage，被打中等于泄露。
//   2. 令牌只存在本机 localStorage，请求一律带 Authorization 头，不放进 URL。
//
// 渲染策略：自动刷新不能把正在读的东西掀掉。所以列表按 sessionId 做增量 patch，
// 详情只在「选中会话真的变了」时才重拉，重建时保住展开的折叠块与滚动位置。
'use strict';

const TOKEN_KEY = 'agent-session-query-token';
const MESSAGE_LIMIT = 200;
const REFRESH_MS = 10000;
const SEARCH_DEBOUNCE_MS = 150;
const BIG_BLOCK_CHARS = 600;
const STICK_TO_BOTTOM_PX = 48;

const state = {
  token: '',
  authRequired: true,
  sessions: [],
  byId: new Map(),
  selectedId: '',
  keyword: '',
  source: '',
  role: '',            // '' / 'user' / 'assistant'
  order: 'desc',       // desc = 最新 N 条（会话最有价值的是结尾）
  detail: null,        // { sessionId, signature, messages, final }
  openBlocks: new Set(),
  timer: null,
  searchTimer: null,
  busy: false,
  loadedAt: '',
};

const $ = (id) => document.getElementById(id);

// ---------------------------------------------------------------------------
// 接口
// ---------------------------------------------------------------------------

async function api(path) {
  const headers = {};
  if (state.token) headers.Authorization = 'Bearer ' + state.token;
  const res = await fetch(path, { headers });
  if (res.status === 401) {
    const err = new Error('unauthorized');
    err.status = 401;
    throw err;
  }
  if (!res.ok) {
    let detail = 'HTTP ' + res.status;
    try {
      const body = await res.json();
      if (body && body.error) detail = body.error;
    } catch (e) { /* 非 JSON 响应就用状态码 */ }
    const err = new Error(detail);
    err.status = res.status;
    throw err;
  }
  return res.json();
}

// ---------------------------------------------------------------------------
// 渲染工具（只用 textContent / createElement）
// ---------------------------------------------------------------------------

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null && text !== '') node.textContent = String(text);
  return node;
}

// setText 只在内容真的变了时才写 DOM：避免每次刷新都把用户选中的文本清掉
function setText(node, text) {
  const value = text === undefined || text === null ? '' : String(text);
  if (node.textContent !== value) node.textContent = value;
}

function kv(pairs) {
  const box = el('div', 'kv');
  for (const [label, value] of pairs) {
    if (value === undefined || value === null || value === '') continue;
    const item = el('span');
    item.appendChild(el('b', '', label));
    item.appendChild(document.createTextNode(' ' + String(value)));
    box.appendChild(item);
  }
  return box;
}

function setStatus(text) {
  setText($('status'), text);
}

function button(className, label, onClick) {
  const node = el('button', className, label);
  node.type = 'button';
  node.addEventListener('click', onClick);
  return node;
}

// segmented 一组互斥的小按钮（取哪一段 / 只看谁）
function segmented(options, current, onPick) {
  const wrap = el('div', 'seg');
  for (const [value, label] of options) {
    wrap.appendChild(button('seg-btn' + (value === current ? ' on' : ''), label, () => onPick(value)));
  }
  return wrap;
}

function copyButton(text) {
  return button('ghost tiny', '复制', async (event) => {
    const node = event.currentTarget;
    try {
      await navigator.clipboard.writeText(text);
      setText(node, '已复制');
      setTimeout(() => setText(node, '复制'), 1200);
    } catch (e) {
      setStatus('复制失败，请手动选中');
    }
  });
}

// 相对时间：updatedAt 各源格式不一（有无 T / Z / 毫秒），统一先按 UTC 补齐再解析。
// 拿不准就原样返回，列表显示退化成原始字符串而已。
function relTime(iso) {
  if (!iso) return '';
  let normalized = String(iso).trim().replace(' ', 'T');
  if (!/[Zz]|[+-]\d{2}:?\d{2}$/.test(normalized)) normalized += 'Z';
  const t = Date.parse(normalized);
  if (Number.isNaN(t)) return String(iso);
  const diff = Date.now() - t;
  if (diff < 0) return String(iso); // 时钟偏差，显示原文最诚实
  if (diff < 60e3) return '刚刚';
  if (diff < 3600e3) return Math.floor(diff / 60e3) + ' 分钟前';
  if (diff < 86400e3) return Math.floor(diff / 3600e3) + ' 小时前';
  if (diff < 7 * 86400e3) return Math.floor(diff / 86400e3) + ' 天前';
  const d = new Date(t);
  const month = String(d.getUTCMonth() + 1).padStart(2, '0');
  const day = String(d.getUTCDate()).padStart(2, '0');
  return (d.getUTCFullYear() !== new Date().getUTCFullYear() ? d.getUTCFullYear() + '-' : '') + month + '-' + day;
}

// 数据源 / 状态标签：className 只用白名单里的值，source 是服务端枚举也不直接拼
const SOURCE_CLASSES = ['claude', 'codex', 'gemini', 'hermes', 'openclaw', 'pi'];
function sourceClass(source) {
  return 'tag' + (SOURCE_CLASSES.includes(source) ? ' ' + source : '');
}
function sourceTag(source) {
  return el('span', sourceClass(source), source);
}
function statusTag(status) {
  const cls = status === 'done' ? ' done' : status === 'running' ? ' running' : '';
  return el('span', 'tag' + cls, status);
}

// ---------------------------------------------------------------------------
// 令牌闸门
// ---------------------------------------------------------------------------

function showGate(message) {
  $('gate').classList.remove('hidden');
  $('app').classList.add('hidden');
  setText($('gate-error'), message || '');
  $('gate-token').focus();
}

function showApp() {
  $('gate').classList.add('hidden');
  $('app').classList.remove('hidden');
  // 服务端没设 token 时没有「换 token」这回事
  $('logout').classList.toggle('hidden', !state.authRequired);
}

// ---------------------------------------------------------------------------
// 左栏：会话列表（按 sessionId 增量 patch，不整栏重建）
// ---------------------------------------------------------------------------

function visibleSessions() {
  const keyword = state.keyword.toLowerCase();
  return state.sessions.filter((s) => {
    if (state.source && s.source !== state.source) return false;
    if (!keyword) return true;
    return [s.sessionId, s.shortKey, s.key, s.cwd]
      .filter(Boolean)
      .some((field) => String(field).toLowerCase().includes(keyword));
  });
}

function buildItem(sessionId) {
  const item = el('div', 'item');
  item.dataset.id = sessionId;
  item.setAttribute('role', 'option');
  item.tabIndex = -1;
  item.addEventListener('click', () => selectSession(item.dataset.id));

  const row = el('div', 'row1');
  row.appendChild(el('span', 'tag'));   // 数据源
  row.appendChild(el('span', 'time'));  // 相对时间
  item.appendChild(row);
  item.appendChild(el('div', 'key'));
  item.appendChild(el('div', 'meta'));
  return item;
}

function fillItem(item, session) {
  const [tag, time] = item.children[0].children;
  const cls = sourceClass(session.source);
  if (tag.className !== cls) tag.className = cls;
  setText(tag, session.source);
  setText(time, relTime(session.updatedAt));
  time.title = session.updatedAt || '';

  const key = item.children[1];
  setText(key, session.shortKey || session.sessionId);
  key.title = session.key || '';

  const meta = item.children[2];
  setText(meta, session.cwd || '');
  meta.classList.toggle('hidden', !session.cwd);
}

function renderList() {
  const list = $('list');
  const sessions = visibleSessions();

  if (sessions.length === 0) {
    list.replaceChildren(el('p', 'empty-list', state.sessions.length ? '没有匹配的会话' : '还没有会话'));
    return;
  }

  // 已有节点按 id 收好，复用得上的就地改文本，改不上的才新建
  const existing = new Map();
  for (const node of Array.from(list.children)) {
    if (node.dataset && node.dataset.id) existing.set(node.dataset.id, node);
    else node.remove(); // 之前的空状态提示
  }

  sessions.forEach((session, index) => {
    let node = existing.get(session.sessionId);
    if (node) existing.delete(session.sessionId);
    else node = buildItem(session.sessionId);

    fillItem(node, session);
    const active = session.sessionId === state.selectedId;
    node.classList.toggle('active', active);
    node.setAttribute('aria-selected', active ? 'true' : 'false');

    // insertBefore 会移动已有节点，不重建 —— 顺序变了也不丢状态
    if (list.children[index] !== node) list.insertBefore(node, list.children[index] || null);
  });

  for (const stale of existing.values()) stale.remove();
}

function renderSources() {
  const select = $('source-filter');
  const counts = new Map();
  for (const s of state.sessions) counts.set(s.source, (counts.get(s.source) || 0) + 1);
  const sources = [...counts.keys()].sort();

  // 选项没变就别重建：重建会把用户正在操作的下拉框打断
  const signature = sources.map((s) => s + ':' + counts.get(s)).join(',');
  if (select.dataset.sig === signature) return;
  select.dataset.sig = signature;

  const current = select.value;
  const all = el('option', '', '全部数据源');
  all.value = '';
  const options = [all];
  for (const source of sources) {
    const option = el('option', '', source + '（' + counts.get(source) + '）');
    option.value = source;
    options.push(option);
  }
  select.replaceChildren(...options);
  select.value = sources.includes(current) ? current : '';
  state.source = select.value;
}

// ---------------------------------------------------------------------------
// 中栏：会话标识 + 工具条 + 消息流
// ---------------------------------------------------------------------------

function renderStreamHead(record) {
  const head = $('stream-head');
  if (!record) {
    head.replaceChildren();
    return;
  }

  const box = el('div');
  const tagrow = el('div', 'tagrow');
  tagrow.appendChild(sourceTag(record.source));
  if (record.status) tagrow.appendChild(statusTag(record.status));
  if (record.updatedAt) {
    const time = el('span', 'time', relTime(record.updatedAt));
    time.title = record.updatedAt;
    tagrow.appendChild(time);
  }
  box.appendChild(tagrow);

  const title = el('h2', '', record.shortKey || record.sessionId);
  title.title = record.key || '';
  box.appendChild(title);
  if (record.cwd) box.appendChild(el('p', 'sub', record.cwd));

  const bar = el('div', 'toolbar');
  bar.appendChild(segmented(
    [['asc', '↑ 最早'], ['desc', '↓ 最新']],
    state.order,
    (value) => {
      if (value === state.order) return;
      state.order = value;
      syncDetail({ force: true });
    },
  ));
  bar.appendChild(segmented(
    [['', '全部'], ['user', 'user'], ['assistant', 'assistant']],
    state.role,
    (value) => {
      if (value === state.role) return;
      state.role = value;
      renderMessages();
      renderStreamHead(record); // 只为把按钮的高亮换过去
    },
  ));

  const detail = state.detail;
  if (detail) {
    const shown = (detail.messages.messages || []).length;
    const total = detail.final && typeof detail.final.messageCount === 'number'
      ? detail.final.messageCount : shown;
    const label = total > shown
      ? '共 ' + total + ' 条 · 这里是' + (state.order === 'desc' ? '最新' : '最早') + ' ' + shown + ' 条'
      : '共 ' + shown + ' 条';
    bar.appendChild(el('span', 'count', label));
  }
  box.appendChild(bar);

  head.replaceChildren(box);
}

function blockNode(block) {
  switch (block.type || 'unknown') {
    case 'text':
      return el('div', 'block text', block.content);
    case 'thinking':
      return el('div', 'block thinking', block.content);
    case 'toolCall': {
      // 工具名做头、参数缩进展开，扫一眼就知道调了什么
      const wrap = el('div', 'block toolCall');
      wrap.appendChild(el('div', 'tool-name', '⚙ ' + (block.name || '(未命名工具)')));
      wrap.appendChild(el('pre', '', JSON.stringify(block.arguments || {}, null, 2)));
      return wrap;
    }
    case 'toolResult': {
      const wrap = el('div', 'block toolResult');
      wrap.appendChild(el('div', 'tool-label', '↳ ' + (block.toolName || '结果')));
      wrap.appendChild(el('pre', '', block.content));
      return wrap;
    }
    default:
      return el('div', 'block', JSON.stringify(block));
  }
}

function messageNode(message, index) {
  // 内容为空的块会渲染成一个空框（虚线的思考框尤其显眼），直接不要。
  // 这种块是真实存在的：Claude 的 thinking 块可能只带 signature、正文是空串。
  const blocks = (Array.isArray(message.content) ? message.content : [])
    .map((block, position) => ({ block, position, node: blockNode(block) }))
    .filter((item) => (item.node.textContent || '').trim() !== '');

  const node = el('div', 'msg ' + (message.role || '') + (blocks.length === 0 ? ' is-empty' : ''));
  const head = el('div', 'head');
  head.appendChild(el('span', 'role', message.role || '?'));
  // 一条可显示内容都没有的消息，压成一行提示就够了，不值得占一整块
  if (blocks.length === 0) head.appendChild(el('span', 'empty-hint', '无可显示内容'));
  if (message.id) head.title = 'id: ' + message.id;
  if (message.timestamp) head.appendChild(el('span', 'time', String(message.timestamp)));
  node.appendChild(head);

  blocks.forEach(({ position, node: rendered }) => {
    const plain = rendered.textContent || '';
    if (plain.length <= BIG_BLOCK_CHARS) {
      node.appendChild(rendered);
      return;
    }
    // 超长的块折起来，避免一个工具输出淹没整页。
    // key 要稳定：刷新重建之后还得认得出哪些是用户展开过的
    const key = (message.id || 'i' + index) + ':' + position;
    const details = el('details');
    details.open = state.openBlocks.has(key);
    details.addEventListener('toggle', () => {
      if (details.open) state.openBlocks.add(key);
      else state.openBlocks.delete(key);
    });
    details.appendChild(el('summary', '', '展开 ' + plain.length + ' 字符'));
    details.appendChild(rendered);
    node.appendChild(details);
  });
  return node;
}

function renderMessages() {
  const pane = $('messages');
  const detail = state.detail;
  if (!detail) return;

  // 同一个会话、同一段消息算「同一个视图」：重建后要回到原来的位置
  const view = detail.sessionId + '|' + state.order;
  const sameView = pane.dataset.view === view;
  const prevScroll = pane.scrollTop;
  const wasAtBottom = sameView &&
    pane.scrollHeight - pane.scrollTop - pane.clientHeight < STICK_TO_BOTTOM_PX;

  const messages = (detail.messages.messages || [])
    .filter((m) => !state.role || m.role === state.role);

  const box = document.createDocumentFragment();
  if (messages.length === 0) {
    box.appendChild(el('p', 'empty', state.role ? '没有 ' + state.role + ' 的消息' : '这个会话没有消息'));
  }
  messages.forEach((message, index) => box.appendChild(messageNode(message, index)));
  pane.replaceChildren(box);
  pane.dataset.view = view;

  if (!sameView) {
    // 刚打开：看最新时贴着底部，看最早时从头开始
    pane.scrollTop = state.order === 'desc' ? pane.scrollHeight : 0;
  } else if (wasAtBottom) {
    pane.scrollTop = pane.scrollHeight;
  } else {
    pane.scrollTop = prevScroll;
  }
}

// ---------------------------------------------------------------------------
// 右栏：最终结果常驻（不必滚到顶部去找）
// ---------------------------------------------------------------------------

// usage 各源字段名不同，收敛成可读的键名；费用保留 4 位
const USAGE_LABELS = {
  inputTokens: '输入', outputTokens: '输出',
  input_tokens: '输入', output_tokens: '输出',
  cacheReadTokens: '缓存读', cacheWriteTokens: '缓存写',
  reasoningTokens: '推理', estimatedCostUsd: '费用 $',
};
const USAGE_DECIMALS = { estimatedCostUsd: 4 };

function finalCard(final) {
  const done = final.isFinal === true;
  const card = el('section', 'card ' + (done ? 'final-done' : 'final-pending'));
  card.appendChild(el('h3', '', '最终结果'));

  const badges = el('div', 'badges');
  badges.appendChild(el('span', 'badge ' + (done ? 'ok' : 'warn'), done ? '已完成' : '未完成'));
  if (final.isProcessing) badges.appendChild(el('span', 'badge warn', '处理中'));
  if (final.stopReason) badges.appendChild(el('span', 'badge', final.stopReason));
  card.appendChild(badges);

  if (final.error) card.appendChild(el('div', 'text', final.error));
  if (final.text) card.appendChild(el('div', 'text', final.text));
  if (!final.text && !final.error) card.appendChild(el('div', 'text dim', '（没有文本结果）'));

  if (final.thinking) {
    const details = el('details');
    details.appendChild(el('summary', '', '思考过程'));
    details.appendChild(el('div', 'block thinking', final.thinking));
    card.appendChild(details);
  }
  card.appendChild(kv([
    ['模型', final.model],
    ['时间', final.timestamp],
  ]));
  return card;
}

function usageCard(usage) {
  const pairs = Object.entries(usage)
    .filter(([, v]) => v !== null && v !== undefined && v !== '')
    // 各源的 usage 里混着嵌套对象（cache_creation、output_tokens_details……），
    // 直接 String() 出来是一串 [object Object]，没有任何信息量，不如不显示
    .filter(([, v]) => typeof v !== 'object')
    // 认识的字段（输入/输出/缓存/费用）排前面，其余按原样跟在后面
    .sort((a, b) => (USAGE_LABELS[a[0]] ? 0 : 1) - (USAGE_LABELS[b[0]] ? 0 : 1))
    .map(([k, v]) => {
      const digits = USAGE_DECIMALS[k];
      const text = digits !== undefined ? Number(v).toFixed(digits) : String(v);
      return [USAGE_LABELS[k] || k, text];
    });
  if (pairs.length === 0) return null;
  const card = el('section', 'card');
  card.appendChild(el('h3', '', '用量'));
  card.appendChild(kv(pairs));
  return card;
}

function sessionCard(record, final) {
  const card = el('section', 'card');
  card.appendChild(el('h3', '', '会话'));

  const idRow = el('div', 'idrow');
  idRow.appendChild(el('code', '', record.sessionId));
  idRow.appendChild(copyButton(record.sessionId));
  card.appendChild(idRow);

  card.appendChild(kv([
    ['数据源', record.source],
    ['消息数', final && final.messageCount],
    ['更新', record.updatedAt],
    ['创建', record.createdAt],
    ['模型', record.model],
    ['cwd', record.cwd],
    ['项目', record.project],
    ['CLI', record.cliVersion],
    ['Token', record.totalTokens],
    ['费用 $', record.estimatedCostUsd],
    ['文件', record.hasFile ? '在' : '缺失（可能只在 state.db 里）'],
  ]));
  if (record.file) {
    const path = el('p', 'sub', record.file);
    path.title = record.file;
    card.appendChild(path);
  }
  return card;
}

function renderSide(record) {
  const side = $('side');
  const detail = state.detail;
  if (!record || !detail) {
    side.replaceChildren();
    return;
  }
  const box = document.createDocumentFragment();
  box.appendChild(finalCard(detail.final || {}));
  const usage = usageCard((detail.final && detail.final.usage) || {});
  if (usage) box.appendChild(usage);
  box.appendChild(sessionCard(record, detail.final));
  side.replaceChildren(box);
}

// ---------------------------------------------------------------------------
// 选中与详情同步
// ---------------------------------------------------------------------------

function selectSession(sessionId) {
  if (!sessionId || sessionId === state.selectedId) return;
  state.selectedId = sessionId;
  const encoded = encodeURIComponent(sessionId);
  if (location.hash.replace(/^#/, '') !== encoded) location.hash = encoded;
  renderList();
  syncDetail({ force: true });
}

function clearDetail(message) {
  state.detail = null;
  renderStreamHead(null);
  $('messages').replaceChildren(el('p', 'empty', message));
  $('messages').dataset.view = '';
  $('side').replaceChildren();
}

// syncDetail 只在「选中会话真的变了」时才重拉。
// 每次刷新都无脑重建的话，展开的折叠块会被收起、滚动位置会跳回顶部、选中的文本会没。
async function syncDetail(options) {
  const force = options && options.force;
  const record = state.byId.get(state.selectedId);
  if (!record) {
    clearDetail('← 从左边选一个会话');
    return;
  }

  const signature = [record.sessionId, record.updatedAt, record.status, state.order].join('|');
  if (!force && state.detail && state.detail.signature === signature) return;
  if (state.busy) return;

  const switched = !state.detail || state.detail.sessionId !== record.sessionId;
  if (switched) {
    // 换了会话才清空；同一个会话只是刷新的话留着旧内容，不闪
    state.openBlocks.clear();
    $('messages').replaceChildren(el('p', 'empty', '加载中…'));
    $('messages').dataset.view = '';
    $('side').replaceChildren();
  }
  renderStreamHead(record);

  state.busy = true;
  try {
    const id = encodeURIComponent(record.sessionId);
    const [messages, final] = await Promise.all([
      api('/sessions/' + id + '/messages?limit=' + MESSAGE_LIMIT + '&order=' + state.order),
      api('/sessions/' + id + '/final'),
    ]);
    state.detail = { sessionId: record.sessionId, signature, messages, final };
    renderStreamHead(record);
    renderMessages();
    renderSide(record);
  } catch (err) {
    handleError(err);
    clearDetail('加载失败：' + err.message);
  } finally {
    state.busy = false;
  }
}

// ---------------------------------------------------------------------------
// 刷新与错误
// ---------------------------------------------------------------------------

function handleError(err) {
  if (err && err.status === 401) {
    state.authRequired = true;
    showGate('token 无效或已失效，请重新粘贴');
    if (state.timer) clearInterval(state.timer);
    state.timer = null;
  }
}

async function refresh() {
  try {
    const data = await api('/sessions');
    state.sessions = data.sessions || [];
    state.byId = new Map(state.sessions.map((s) => [s.sessionId, s]));
    state.loadedAt = new Date().toLocaleTimeString();
    renderSources();
    renderList();
    await syncDetail({});
    setStatus('共 ' + state.sessions.length + ' 个会话 · 更新于 ' + state.loadedAt);
  } catch (err) {
    handleError(err);
    setStatus('刷新失败：' + err.message);
  }
}

function startAutoRefresh() {
  if (state.timer) clearInterval(state.timer);
  state.timer = setInterval(() => {
    if (document.hidden) return; // 页面不在前台就不打接口
    refresh();
  }, REFRESH_MS);
}

// ---------------------------------------------------------------------------
// 键盘
// ---------------------------------------------------------------------------

function isTyping(node) {
  if (!node) return false;
  const tag = node.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || node.isContentEditable;
}

function moveSelection(delta) {
  const sessions = visibleSessions();
  if (sessions.length === 0) return;
  const index = sessions.findIndex((s) => s.sessionId === state.selectedId);
  const next = index < 0
    ? (delta > 0 ? 0 : sessions.length - 1)
    : Math.min(sessions.length - 1, Math.max(0, index + delta));
  selectSession(sessions[next].sessionId);
  const active = $('list').querySelector('.item.active');
  if (active) active.scrollIntoView({ block: 'nearest' });
}

function scrollMessages(toBottom) {
  const pane = $('messages');
  pane.scrollTop = toBottom ? pane.scrollHeight : 0;
}

function clearSearch() {
  if (!state.keyword) return;
  state.keyword = '';
  $('search').value = '';
  renderList();
}

document.addEventListener('keydown', (event) => {
  if (event.metaKey || event.ctrlKey || event.altKey) return;
  if (isTyping(event.target)) {
    if (event.key === 'Escape') event.target.blur();
    return;
  }
  switch (event.key) {
    case '/':
      event.preventDefault();
      $('search').focus();
      break;
    case 'j': case 'ArrowDown':
      event.preventDefault();
      moveSelection(1);
      break;
    case 'k': case 'ArrowUp':
      event.preventDefault();
      moveSelection(-1);
      break;
    case 'g':
      scrollMessages(false);
      break;
    case 'G':
      scrollMessages(true);
      break;
    case 'r':
      refresh();
      break;
    case 'Escape':
      clearSearch();
      break;
    default:
      break;
  }
});

// ---------------------------------------------------------------------------
// 事件与启动
// ---------------------------------------------------------------------------

$('gate-form').addEventListener('submit', (event) => {
  event.preventDefault();
  const token = $('gate-token').value.trim();
  if (!token) return;
  state.token = token;
  localStorage.setItem(TOKEN_KEY, token);
  showApp();
  start();
});

$('logout').addEventListener('click', () => {
  localStorage.removeItem(TOKEN_KEY);
  state.token = '';
  state.sessions = [];
  state.byId = new Map();
  state.selectedId = '';
  state.detail = null;
  if (state.timer) clearInterval(state.timer);
  state.timer = null;
  showGate('已清除本机保存的 token');
});

// 搜索防抖：每敲一个字就整栏重排是没必要的
$('search').addEventListener('input', (event) => {
  const value = event.target.value.trim();
  if (state.searchTimer) clearTimeout(state.searchTimer);
  state.searchTimer = setTimeout(() => {
    state.keyword = value;
    renderList();
  }, SEARCH_DEBOUNCE_MS);
});

$('source-filter').addEventListener('change', (event) => {
  state.source = event.target.value;
  renderList();
});

$('refresh').addEventListener('click', refresh);

$('auto').addEventListener('change', (event) => {
  if (event.target.checked) {
    startAutoRefresh();
  } else if (state.timer) {
    clearInterval(state.timer);
    state.timer = null;
  }
});

window.addEventListener('hashchange', () => {
  const id = decodeURIComponent(location.hash.replace(/^#/, ''));
  if (id && id !== state.selectedId && state.byId.has(id)) selectSession(id);
});

async function start() {
  const hashId = decodeURIComponent(location.hash.replace(/^#/, ''));
  if (hashId) state.selectedId = hashId;
  await refresh();
  if (!state.selectedId && state.sessions.length > 0) {
    // 默认选中最新的一个，打开就有东西看
    selectSession(state.sessions[0].sessionId);
  }
  if ($('auto').checked) startAutoRefresh();
}

(async function boot() {
  // 服务端没设 token 时不该还逼人随便填一个字符串，/health 会直说要不要认证
  try {
    const res = await fetch('/health');
    const health = await res.json();
    state.authRequired = health.authRequired !== false;
  } catch (e) {
    state.authRequired = true; // 问不到就按需要认证处理
  }

  const saved = localStorage.getItem(TOKEN_KEY) || '';
  if (state.authRequired && !saved) {
    showGate('');
    return;
  }
  state.token = saved;
  showApp();
  await start();
})();
