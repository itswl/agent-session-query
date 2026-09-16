// Agent 会话查询的只读页面。
//
// 两条铁律：
//   1. 会话内容是别人写的文本（工具输出、网页正文……），一律用 textContent 渲染，
//      禁止任何 HTML 拼接赋值 —— 页面的 token 存在 localStorage，被打中等于泄露。
//   2. 令牌只存在本机 localStorage，请求一律带 Authorization 头，不放进 URL。
'use strict';

const TOKEN_KEY = 'agent-session-query-token';
const MESSAGE_LIMIT = 200;
const REFRESH_MS = 10000;

const state = {
  token: '',
  sessions: [],
  selectedId: '',
  keyword: '',
  source: '',
  timer: null,
  loadedAt: '',
};

const $ = (id) => document.getElementById(id);

// ---------------------------------------------------------------------------
// 接口
// ---------------------------------------------------------------------------

async function api(path) {
  const res = await fetch(path, { headers: { Authorization: 'Bearer ' + state.token } });
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

function kv(pairs) {
  const box = el('div', 'kv');
  for (const [label, value] of pairs) {
    if (value === undefined || value === null || value === '') continue;
    const item = el('span');
    item.appendChild(el('b', '', label + ' '));
    item.appendChild(document.createTextNode(String(value)));
    box.appendChild(item);
  }
  return box;
}

function status(text) {
  $('status').textContent = text;
}

// 相对时间： updatedAt 各源格式不一（有无 T / Z / 毫秒），统一先按 UTC 补齐再解析。
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
function sourceTag(source) {
  const cls = SOURCE_CLASSES.includes(source) ? ' ' + source : '';
  return el('span', 'tag' + cls, source);
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
  $('gate-error').textContent = message || '';
  $('gate-token').focus();
}

function showApp() {
  $('gate').classList.add('hidden');
  $('app').classList.remove('hidden');
}

$('gate-form').addEventListener('submit', (event) => {
  event.preventDefault();
  const token = $('gate-token').value.trim();
  if (!token) return;
  state.token = token;
  localStorage.setItem(TOKEN_KEY, token);
  showApp();
  refresh();
});

$('logout').addEventListener('click', () => {
  localStorage.removeItem(TOKEN_KEY);
  state.token = '';
  state.sessions = [];
  state.selectedId = '';
  showGate('已清除本机保存的 token');
});

// ---------------------------------------------------------------------------
// 列表
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

function renderSources() {
  const select = $('source-filter');
  const current = select.value;
  const sources = [...new Set(state.sessions.map((s) => s.source))].sort();
  select.replaceChildren(el('option', '', '全部数据源'));
  select.firstChild.value = '';
  for (const source of sources) {
    const option = el('option', '', source + '（' + state.sessions.filter((s) => s.source === source).length + '）');
    option.value = source;
    select.appendChild(option);
  }
  select.value = sources.includes(current) ? current : '';
  state.source = select.value;
}

function renderList() {
  const list = $('list');
  const sessions = visibleSessions();
  list.replaceChildren();

  if (sessions.length === 0) {
    list.appendChild(el('p', 'empty-list', '没有匹配的会话'));
    return;
  }
  for (const session of sessions) {
    const item = el('div', 'item' + (session.sessionId === state.selectedId ? ' active' : ''));
    item.addEventListener('click', () => selectSession(session.sessionId));

    const row1 = el('div', 'row1');
    row1.appendChild(sourceTag(session.source));
    if (session.updatedAt) {
      const time = el('span', 'time', relTime(session.updatedAt));
      time.title = session.updatedAt; // 悬浮看完整时间
      row1.appendChild(time);
    }
    item.appendChild(row1);

    const key = el('div', 'key', session.shortKey || session.sessionId);
    if (session.cwd) key.title = session.cwd;
    item.appendChild(key);

    if (session.cwd) item.appendChild(el('div', 'meta', session.cwd));
    list.appendChild(item);
  }
}

// ---------------------------------------------------------------------------
// 详情
// ---------------------------------------------------------------------------

function renderDetailHeader(record) {
  const box = el('div');

  const tagrow = el('div', 'tagrow');
  tagrow.appendChild(sourceTag(record.source));
  if (record.status) tagrow.appendChild(statusTag(record.status));
  box.appendChild(tagrow);

  box.appendChild(el('h2', '', record.shortKey || record.sessionId));
  box.appendChild(el('p', 'sub', record.key || ''));

  box.appendChild(kv([
    ['sessionId', record.sessionId],
    ['更新时间', record.updatedAt],
    ['cwd', record.cwd],
    ['文件', record.hasFile ? '在' : '缺失（可能只在 state.db 里）'],
    ['模型', record.model],
  ]));
  return box;
}

function blockNode(block) {
  const type = block.type || 'unknown';
  switch (type) {
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

function messageNode(message) {
  const node = el('div', 'msg ' + (message.role || ''));
  const head = el('div', 'head');
  head.appendChild(el('span', 'role', message.role || '?'));
  if (message.id) head.title = 'id: ' + message.id;
  if (message.timestamp) {
    const time = el('span', 'time', String(message.timestamp));
    head.appendChild(time);
  }
  node.appendChild(head);

  const blocks = Array.isArray(message.content) ? message.content : [];
  if (blocks.length === 0) {
    node.appendChild(el('div', 'block empty-hint', '（空消息）'));
  }
  for (const block of blocks) {
    // 超长的块折起来，避免一个工具输出淹没整页
    const rendered = blockNode(block);
    const plain = rendered.textContent || '';
    if (plain.length > 600) {
      const details = el('details');
      details.appendChild(el('summary', '', '展开 ' + plain.length + ' 字符'));
      details.appendChild(rendered);
      node.appendChild(details);
    } else {
      node.appendChild(rendered);
    }
  }
  return node;
}

// usage 各源字段名不同，收敛成可读的键名；费用保留 4 位
const USAGE_LABELS = {
  inputTokens: '输入', outputTokens: '输出',
  input_tokens: '输入', output_tokens: '输出',
  cacheReadTokens: '缓存读', cacheWriteTokens: '缓存写',
  reasoningTokens: '推理', estimatedCostUsd: '费用$',
};
const USAGE_DECIMALS = { estimatedCostUsd: 4 };

function finalCard(final) {
  const done = final.isFinal === true;
  const card = el('div', 'card ' + (done ? 'final-done' : 'final-pending'));
  card.appendChild(el('h3', '', '最终结果'));

  const badges = el('div', 'badges');
  badges.appendChild(el('span', 'badge ' + (done ? 'ok' : 'warn'), done ? '已完成' : '未完成'));
  if (final.isProcessing) badges.appendChild(el('span', 'badge warn', '处理中'));
  card.appendChild(badges);

  if (final.error) {
    card.appendChild(el('div', 'text', final.error));
  }
  if (final.text) card.appendChild(el('div', 'text', final.text));
  if (!final.text && !final.error) card.appendChild(el('div', 'text', '（没有文本结果）'));
  if (final.thinking) {
    const details = el('details');
    details.appendChild(el('summary', '', '思考过程'));
    details.appendChild(el('div', 'block thinking', final.thinking));
    card.appendChild(details);
  }
  card.appendChild(kv([
    ['stopReason', final.stopReason],
    ['messageCount', final.messageCount],
    ['模型', final.model],
    ['时间', final.timestamp],
  ]));
  if (final.usage && Object.keys(final.usage).length > 0) {
    const pairs = Object.entries(final.usage)
      .filter(([, v]) => v !== null && v !== undefined && v !== '')
      .map(([k, v]) => {
        const digits = USAGE_DECIMALS[k];
        const text = digits !== undefined ? Number(v).toFixed(digits) : String(v);
        return [USAGE_LABELS[k] || k, text];
      });
    card.appendChild(kv(pairs));
  }
  return card;
}

async function selectSession(sessionId) {
  state.selectedId = sessionId;
  location.hash = encodeURIComponent(sessionId);
  renderList();
  await renderDetail();
}

async function renderDetail() {
  const detail = $('detail');
  const record = state.sessions.find((s) => s.sessionId === state.selectedId);
  if (!record) {
    detail.replaceChildren(el('p', 'empty', '← 从左边选一个会话'));
    return;
  }

  const sessionId = encodeURIComponent(record.sessionId);
  detail.replaceChildren(el('p', 'empty', '加载中…'));

  try {
    const [messages, final] = await Promise.all([
      api('/sessions/' + sessionId + '/messages?limit=' + MESSAGE_LIMIT),
      api('/sessions/' + sessionId + '/final'),
    ]);

    const box = el('div');
    box.appendChild(renderDetailHeader(record));
    box.appendChild(finalCard(final));

    const count = messages.total || 0;
    const head = el('div', 'card');
    const title = el('h3', '', '消息 ' + count + ' 条' + (count > MESSAGE_LIMIT ? '（只取最早 ' + MESSAGE_LIMIT + ' 条）' : ''));
    head.appendChild(title);
    box.appendChild(head);

    for (const message of messages.messages || []) {
      box.appendChild(messageNode(message));
    }
    detail.replaceChildren(box);
  } catch (err) {
    handleError(err);
    detail.replaceChildren(el('p', 'empty', '加载失败：' + err.message));
  }
}

// ---------------------------------------------------------------------------
// 刷新与错误
// ---------------------------------------------------------------------------

function handleError(err) {
  if (err && err.status === 401) {
    showGate('token 无效或已失效，请重新粘贴');
    if (state.timer) clearInterval(state.timer);
    state.timer = null;
  }
}

async function refresh() {
  try {
    const data = await api('/sessions');
    state.sessions = data.sessions || [];
    state.loadedAt = new Date().toLocaleTimeString();
    renderSources();
    renderList();
    if (state.selectedId) await renderDetail();
    status('共 ' + state.sessions.length + ' 个会话 · 更新于 ' + state.loadedAt);
  } catch (err) {
    handleError(err);
    status('刷新失败：' + err.message);
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
// 启动
// ---------------------------------------------------------------------------

$('search').addEventListener('input', (event) => {
  state.keyword = event.target.value.trim();
  renderList();
});

$('source-filter').addEventListener('change', (event) => {
  state.source = event.target.value;
  renderList();
});

$('refresh').addEventListener('click', refresh);

$('auto').addEventListener('change', (event) => {
  if (event.target.checked) startAutoRefresh();
  else if (state.timer) clearInterval(state.timer), (state.timer = null);
});

window.addEventListener('hashchange', () => {
  const id = decodeURIComponent(location.hash.replace(/^#/, ''));
  if (id && id !== state.selectedId && state.sessions.some((s) => s.sessionId === id)) {
    state.selectedId = id;
    renderList();
    renderDetail();
  }
});

(async function boot() {
  const saved = localStorage.getItem(TOKEN_KEY);
  if (!saved) {
    showGate('');
    return;
  }
  state.token = saved;
  showApp();
  const id = decodeURIComponent(location.hash.replace(/^#/, ''));
  if (id) state.selectedId = id;
  await refresh();
  if (!state.selectedId && state.sessions.length > 0) {
    // 默认选中最新的一个，打开就有东西看
    await selectSession(state.sessions[0].sessionId);
  } else if (state.selectedId) {
    await renderDetail();
  }
  startAutoRefresh();
})();
