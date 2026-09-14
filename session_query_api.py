#!/usr/bin/env python3
"""
本地 Agent 会话查询 HTTP API（OpenClaw / Hermes / Pi / Claude Code / Codex / Gemini CLI）

启动方式:
    python3 session_query_api.py [--port 8080] [--mode auto|all|hermes|openclaw|pi|claude|codex|gemini]

数据源（auto 模式下，存在的数据源都启用；可同时查询多个）:
    - Hermes      ~/.hermes/sessions/sessions.json
    - OpenClaw    ~/.openclaw/agents/default/sessions/sessions.json
    - Pi          ~/.pi/agent/sessions/<项目>/*.jsonl
    - Claude Code ~/.claude/projects/<项目>/*.jsonl
    - Codex       ~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl
    - Gemini CLI  ~/.gemini/tmp/<项目>/chats/session-*.jsonl

API 端点:

GET /sessions
    - 列出所有会话（多数据源合并，按更新时间倒序，每条带 source 字段）

GET /sessions/<pattern>
    - 根据 Session ID、会话 key 或路径片段查询
    - 例如: /sessions/5ab8e024-2740-422c-8503-89c01313f792
    - 例如: /sessions/hook:alert:prometheus:b5123b01-616a-4da0-ac48-d9c81e3be63c

GET /sessions/<pattern>/messages?limit=50
    - 获取会话的消息内容

GET /sessions/<pattern>/final
    - 获取会话的最终结果

GET /health
    - 健康检查（含启用的数据源与连接统计）
"""

import hmac
import json
import os
import re
import sys
import socket
import sqlite3
import threading
import time
import traceback
from pathlib import Path
from http.server import HTTPServer, BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs, unquote
from typing import Optional, Dict, Any, List, Tuple
import argparse
import select

from datetime import datetime, timezone

# 默认模式（自动检测）
MODE = 'auto'

# OpenClaw / Hermes：一个 sessions.json 索引 + 每会话一个 jsonl
_JSON_MAP_DEFS = {
    'hermes': {
        'mode': 'hermes',
        'sessions_json': Path.home() / ".hermes/sessions/sessions.json",
        'sessions_dir': Path.home() / ".hermes/sessions",
        'session_id_field': 'session_id',
        'stop_reason_field': 'finish_reason',
    },
    'openclaw': {
        'mode': 'openclaw',
        'sessions_json': Path.home() / ".openclaw/agents/default/sessions/sessions.json",
        'sessions_dir': Path.home() / ".openclaw/agents/default/sessions",
        'session_id_field': 'sessionId',
        'stop_reason_field': 'stopReason',
    },
}

# 每会话一个文件的几个 CLI：会话目录
PI_SESSIONS_DIR = Path.home() / ".pi/agent/sessions"
CLAUDE_PROJECTS_DIR = Path.home() / ".claude/projects"
CODEX_SESSIONS_DIR = Path.home() / ".codex/sessions"
GEMINI_TMP_DIR = Path.home() / ".gemini/tmp"

# 支持的数据源（--mode 可选值，auto 时按存在与否自动启用）
KNOWN_MODES = ['hermes', 'openclaw', 'pi', 'claude', 'codex', 'gemini']


# ---------------------------------------------------------------------------
# 通用工具
# ---------------------------------------------------------------------------

def _iter_jsonl(path):
    """逐行产出 jsonl 里的对象（生成器，不把整个文件读进内存）"""
    try:
        with open(path, 'r', errors='replace') as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if isinstance(obj, dict):
                    yield obj
    except OSError:
        return


def _read_jsonl(path, limit: Optional[int] = None) -> List[Dict]:
    """逐行读 jsonl，坏行跳过；limit 不为空时最多读这么多条有效记录"""
    out = []
    try:
        with open(path, 'r', errors='replace') as f:
            for line in f:
                if limit is not None and len(out) >= limit:
                    break
                line = line.strip()
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if isinstance(obj, dict):
                    out.append(obj)
    except OSError:
        pass
    return out


def _content_text(value: Any) -> str:
    """把各种形态的 content 收敛成纯文本（字符串 / 数组 / 单个字典）"""
    if isinstance(value, str):
        return value
    if isinstance(value, dict):
        for key in ('text', 'content', 'thinking'):
            inner = value.get(key)
            if isinstance(inner, str):
                return inner
            if isinstance(inner, list):
                return _content_text(inner)
        return ''
    if isinstance(value, list):
        parts = []
        for item in value:
            text = _content_text(item)
            if text:
                parts.append(text)
        return "\n".join(parts)
    return ''


def _iso(ts: Any) -> str:
    """统一成 ISO 字符串，用于排序和展示"""
    if ts is None or ts == '':
        return ''
    if isinstance(ts, (int, float)):
        try:
            seconds = ts / 1000 if ts > 1e11 else ts
            return datetime.fromtimestamp(seconds, tz=timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')
        except (OverflowError, OSError, ValueError):
            return ''
    return str(ts)


def _mtime_iso(path) -> str:
    try:
        return datetime.fromtimestamp(path.stat().st_mtime, tz=timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')
    except OSError:
        return ''


def _match_rank(pattern_lower: str, record: Dict) -> int:
    """pattern 与会话记录的匹配度：0 最精确，-1 表示不匹配"""
    sid = str(record.get('sessionId') or '').lower()
    key = str(record.get('key') or '').lower()
    if sid and pattern_lower == sid:
        return 0
    if key == pattern_lower:
        return 1
    if key.endswith(':' + pattern_lower) or key.endswith('/' + pattern_lower):
        return 2
    if pattern_lower in key:
        return 3
    if sid and pattern_lower in sid:
        return 4
    return -1


def _trim(text: str, limit: int, mark: str = '...[truncated]') -> str:
    if isinstance(text, str) and len(text) > limit:
        return text[:limit] + mark
    return text


class SessionSource:
    """数据源适配器基类：统一 list / find / messages / final 四个动作。

    每条会话记录（record）统一成：
        {'source', 'key', 'sessionId', 'file', 'hasFile', 'status', 'updatedAt', ...}
    """
    mode = 'base'
    location = ''

    def exists(self) -> bool:
        return True

    def list(self) -> List[Dict]:
        return []

    def find(self, pattern: str) -> Optional[Dict]:
        """按「精确 ID → 精确 key → 后缀 → 子串」的优先级返回最佳匹配"""
        pattern_lower = (pattern or '').strip().lower()
        if not pattern_lower:
            return None
        best, best_rank = None, 99
        for record in self.list():
            rank = _match_rank(pattern_lower, record)
            if rank != -1 and rank < best_rank:
                best, best_rank = record, rank
                if rank == 0:
                    break
        return best

    def messages(self, record: Dict, limit: int = 50) -> List[Dict]:
        return []

    def final(self, record: Dict) -> Optional[Dict]:
        return None


# ---------------------------------------------------------------------------
# 适配器 1/2：OpenClaw / Hermes（sessions.json 索引 + jsonl）
# ---------------------------------------------------------------------------

class JsonMapSource(SessionSource):
    def __init__(self, definition: Dict[str, Any]):
        self.mode = definition['mode']
        self.sessions_json = definition['sessions_json']
        self.sessions_dir = definition['sessions_dir']
        self.session_id_field = definition['session_id_field']
        self.stop_reason_field = definition['stop_reason_field']
        self.location = str(self.sessions_json)

    def exists(self) -> bool:
        return self.sessions_json.exists()

    def _load(self) -> Dict[str, Any]:
        if not self.sessions_json.exists():
            return {}
        try:
            with open(self.sessions_json, 'r') as f:
                data = json.load(f)
            return data if isinstance(data, dict) else {}
        except (OSError, json.JSONDecodeError):
            return {}

    def _file_of(self, record: Dict):
        path = record.get('file')
        if path and Path(path).exists():
            return Path(path)
        sid = record.get('sessionId')
        if sid:
            alt = self.sessions_dir / f"{sid}.jsonl"
            if alt.exists():
                return alt
        return None

    def list(self) -> List[Dict]:
        sessions = self._load()
        is_openclaw = self.mode == 'openclaw'
        out = []
        for key, info in sessions.items():
            if not isinstance(info, dict):
                continue
            sid = info.get(self.session_id_field) or info.get('sessionId') or info.get('session_id') or ''
            file = info.get('sessionFile') or ''
            file_exists = Path(file).exists() if file else False
            if not file_exists and sid:
                alt = self.sessions_dir / f"{sid}.jsonl"
                if alt.exists():
                    file, file_exists = str(alt), True

            record = {
                'source': self.mode,
                'key': key,
                'shortKey': key.replace('agent:default:', '') if is_openclaw else key,
                'sessionId': sid,
                'file': file if file_exists else None,
                'hasFile': file_exists,
            }

            if is_openclaw:
                updated = info.get('updatedAt', 0)
                updated_str = ''
                sort_key = ''
                if updated:
                    try:
                        dt = datetime.fromtimestamp(updated / 1000, tz=timezone.utc)
                        updated_str = dt.strftime('%Y-%m-%d %H:%M:%S')
                        sort_key = dt.strftime('%Y-%m-%dT%H:%M:%S')
                    except (OverflowError, OSError, ValueError):
                        updated_str = str(updated)
                record.update({
                    'status': info.get('status', 'unknown'),
                    'updatedAt': updated_str,
                    'model': info.get('model', ''),
                    'runtimeMs': info.get('runtimeMs', 0),
                    'totalTokens': info.get('totalTokens', 0),
                })
            else:  # hermes
                sort_key = str(info.get('updated_at', '') or '')
                record.update({
                    'status': 'done',
                    'updatedAt': info.get('updated_at', ''),
                    'createdAt': info.get('created_at', ''),
                    'displayName': info.get('display_name', ''),
                    'platform': info.get('platform', ''),
                    'totalTokens': info.get('total_tokens', 0),
                    'estimatedCostUsd': info.get('estimated_cost_usd', 0),
                })

            record['_sortKey'] = sort_key
            out.append(record)
        return out

    def messages(self, record: Dict, limit: int = 50) -> List[Dict]:
        """OpenClaw / Hermes 的消息格式由行内容自辨（type=message 或 role=user/assistant）"""
        path = self._file_of(record)
        if path is None:
            return []

        messages = []
        with open(path, 'r', errors='replace') as f:
            for line in f:
                if len(messages) >= limit:
                    break
                try:
                    data = json.loads(line.strip())
                except json.JSONDecodeError:
                    continue
                # OpenClaw 格式: type=message
                if data.get('type') == 'message':
                    messages.append(data)
                # Hermes 格式: role=user/assistant
                elif data.get('role') in ['user', 'assistant']:
                    messages.append(data)

        return [self._format_message(m) for m in messages]

    def _format_message(self, msg: Dict) -> Dict:
        """格式化单条消息（OpenClaw content 数组 / Hermes 字符串）"""
        if msg.get('type') == 'message':
            content = msg.get('message', {}).get('content', [])
            role = msg.get('message', {}).get('role', 'unknown')
        else:
            content = msg.get('content', '')
            role = msg.get('role', 'unknown')

        parts = []

        if isinstance(content, list):
            for item in content:
                if not isinstance(item, dict):
                    continue
                if item.get('type') == 'text':
                    parts.append({'type': 'text', 'content': item.get('text', '')})
                elif item.get('type') == 'thinking':
                    parts.append({'type': 'thinking', 'content': _trim(item.get('thinking', ''), 1000)})
                elif item.get('type') == 'toolCall':
                    parts.append({
                        'type': 'toolCall',
                        'name': item.get('name', ''),
                        'arguments': item.get('arguments', {})
                    })
                elif item.get('type') == 'toolResult':
                    result_text = ''
                    for r in item.get('content', []) or []:
                        if isinstance(r, dict) and r.get('type') == 'text':
                            result_text = _trim(r.get('text', ''), 500)
                    parts.append({
                        'type': 'toolResult',
                        'toolName': item.get('toolName', ''),
                        'content': result_text
                    })
        elif isinstance(content, str):
            if role == 'assistant':
                reasoning = msg.get('reasoning', '')
                if reasoning:
                    parts.append({'type': 'thinking', 'content': _trim(reasoning, 1000)})
            parts.append({'type': 'text', 'content': content})

        return {
            'id': msg.get('id', ''),
            'role': role,
            'timestamp': msg.get('timestamp', ''),
            'content': parts
        }

    def _sqlite_final_message(self, session_id: str, status: str) -> Optional[Dict]:
        """Hermes 的 webhook 会话有时只把最终消息落在 state.db 里"""
        if self.mode != 'hermes' or not session_id:
            return None

        db_path = Path.home() / ".hermes/state.db"
        if not db_path.exists():
            return None

        try:
            conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True, timeout=3)
            conn.row_factory = sqlite3.Row
            try:
                session = conn.execute(
                    """
                    SELECT message_count, input_tokens, output_tokens, cache_read_tokens,
                           cache_write_tokens, reasoning_tokens, estimated_cost_usd
                    FROM sessions
                    WHERE id = ?
                    """,
                    (session_id,),
                ).fetchone()
                row = conn.execute(
                    """
                    SELECT id, content, finish_reason, reasoning, timestamp, token_count
                    FROM messages
                    WHERE session_id = ?
                      AND role = 'assistant'
                      AND COALESCE(active, 1) = 1
                      AND finish_reason = 'stop'
                      AND COALESCE(content, '') <> ''
                    ORDER BY timestamp DESC, id DESC
                    LIMIT 1
                    """,
                    (session_id,),
                ).fetchone()
                if row is None:
                    return None

                message_count = session['message_count'] if session else None
                if not message_count:
                    count_row = conn.execute(
                        "SELECT COUNT(*) AS count FROM messages WHERE session_id = ? AND COALESCE(active, 1) = 1",
                        (session_id,),
                    ).fetchone()
                    message_count = count_row['count'] if count_row else 0

                usage = {}
                if session:
                    usage = {
                        'inputTokens': session['input_tokens'] or 0,
                        'outputTokens': session['output_tokens'] or 0,
                        'cacheReadTokens': session['cache_read_tokens'] or 0,
                        'cacheWriteTokens': session['cache_write_tokens'] or 0,
                        'reasoningTokens': session['reasoning_tokens'] or 0,
                        'estimatedCostUsd': session['estimated_cost_usd'] or 0,
                    }

                return {
                    'status': status,
                    'isFinal': True,
                    'isProcessing': False,
                    'messageCount': int(message_count or 0),
                    'source': self.mode,
                    'id': row['id'],
                    'timestamp': row['timestamp'],
                    'stopReason': row['finish_reason'] or 'stop',
                    'text': row['content'] or '',
                    'thinking': row['reasoning'] or '',
                    'toolCalls': [],
                    'usage': usage,
                }
            finally:
                conn.close()
        except Exception as e:
            print(f"[WARN] 读取 Hermes SQLite 最终消息失败 {session_id}: {e}", file=sys.stderr)
            return None

    def final(self, record: Dict) -> Optional[Dict]:
        stop_reason_field = self.stop_reason_field
        session_id = record.get('sessionId', '')
        status = record.get('status', 'done')
        path = self._file_of(record)

        if path is None:
            sqlite_result = self._sqlite_final_message(session_id, status)
            if sqlite_result is not None:
                return sqlite_result
            return {
                'status': status,
                'isFinal': False,
                'isProcessing': status == 'running',
                'messageCount': 0,
                'source': self.mode,
                'error': 'Session file not available yet (session may be still initializing)'
            }

        # 单次读取：收集 assistant 消息、第一个 "stop" 消息、以及 toolResult 的 parentId
        all_messages = []
        first_stop_message = None
        tool_result_parents = set()

        with open(path, 'r', errors='replace') as f:
            for line in f:
                try:
                    data = json.loads(line.strip())
                except json.JSONDecodeError:
                    continue
                # OpenClaw 格式
                if data.get('type') == 'message':
                    msg = data.get('message', {})
                    role = msg.get('role')
                    if role == 'assistant':
                        msg_stop_reason = msg.get(stop_reason_field) or data.get(stop_reason_field) or ''
                        data['_msg_stopReason'] = msg_stop_reason
                        all_messages.append(data)
                        if first_stop_message is None and msg_stop_reason == 'stop':
                            first_stop_message = data
                    elif role == 'toolResult':
                        parent_id = data.get('parentId')
                        if parent_id:
                            tool_result_parents.add(parent_id)
                # Hermes 格式
                elif data.get('role') == 'assistant':
                    msg_stop_reason = data.get(stop_reason_field, '')
                    data['_msg_stopReason'] = msg_stop_reason
                    all_messages.append(data)
                    if first_stop_message is None and msg_stop_reason == 'stop':
                        first_stop_message = data

        # 第一个 stop 消息之后还有指向它的 toolResult，说明可能仍在处理中
        is_processing = False
        if first_stop_message and status == 'running' and first_stop_message.get('id', '') in tool_result_parents:
            is_processing = True

        if first_stop_message is None:
            sqlite_result = self._sqlite_final_message(session_id, status)
            if sqlite_result is not None:
                return sqlite_result
            return {
                'status': status,
                'isFinal': False,
                'isProcessing': status == 'running',
                'messageCount': len(all_messages),
                'source': self.mode,
                'text': '',
                'thinking': '',
            }

        last = first_stop_message

        if last.get('type') == 'message':
            msg_data = last.get('message', {})
            content = msg_data.get('content', [])
            stop_reason = last.get('_msg_stopReason') or msg_data.get(stop_reason_field) or last.get(stop_reason_field, '')
        else:
            content = last.get('content', '')
            stop_reason = last.get('_msg_stopReason', '')

        is_stopped = stop_reason == 'stop'
        is_done = status == 'done' or is_stopped

        result = {
            'status': status,
            'isFinal': bool(is_done and not is_processing),
            'isProcessing': bool(is_processing or (status == 'running' and not is_stopped)),
            'messageCount': len(all_messages),
            'source': self.mode,
            'id': last.get('id', ''),
            'timestamp': last.get('timestamp', ''),
            'stopReason': stop_reason,
            'text': '',
            'thinking': '',
            'toolCalls': [],
            'usage': last.get('usage', {}),
        }

        if isinstance(content, list):
            for c in content:
                if not isinstance(c, dict):
                    continue
                if c.get('type') == 'text':
                    result['text'] = c.get('text', '')
                elif c.get('type') == 'thinking':
                    result['thinking'] = c.get('thinking', '')
                elif c.get('type') == 'toolCall':
                    result['toolCalls'].append({
                        'name': c.get('name', ''),
                        'arguments': c.get('arguments', {})
                    })
        elif isinstance(content, str):
            result['text'] = content
            result['thinking'] = last.get('reasoning', '')

        return result


# ---------------------------------------------------------------------------
# 适配器 3/6：Pi（~/.pi/agent/sessions/<项目>/<时间>_<uuid>.jsonl）
# ---------------------------------------------------------------------------

class PiSource(SessionSource):
    mode = 'pi'

    def __init__(self, root: Path = PI_SESSIONS_DIR):
        self.root = root
        self.location = str(root)

    def exists(self) -> bool:
        return self.root.exists()

    def _files(self):
        if not self.root.exists():
            return []
        return sorted(self.root.glob('*/*.jsonl'))

    def list(self) -> List[Dict]:
        out = []
        for path in self._files():
            head = _read_jsonl(path, limit=1)
            meta = head[0] if head else {}
            out.append({
                'source': self.mode,
                'key': str(path),
                'shortKey': path.stem,
                'sessionId': str(meta.get('id') or path.stem),
                'file': str(path),
                'hasFile': True,
                'status': 'done',
                'cwd': meta.get('cwd', ''),
                'updatedAt': _mtime_iso(path),
                '_sortKey': _mtime_iso(path),
            })
        return out

    def messages(self, record: Dict, limit: int = 50) -> List[Dict]:
        out = []
        path = record.get('file')
        if not path:
            return out
        for obj in _iter_jsonl(path):
            if len(out) >= limit:
                break
            if obj.get('type') != 'message':
                continue
            msg = obj.get('message') or {}
            parts = []
            for item in msg.get('content') or []:
                if not isinstance(item, dict):
                    continue
                kind = item.get('type')
                if kind == 'text' or 'text' in item:
                    parts.append({'type': 'text', 'content': item.get('text', '')})
                elif kind == 'thinking':
                    parts.append({'type': 'thinking', 'content': _trim(item.get('thinking', ''), 1000)})
                else:
                    parts.append({'type': kind or 'unknown', 'content': _trim(_content_text(item), 500)})
            out.append({
                'id': obj.get('id', ''),
                'role': msg.get('role', 'unknown'),
                'timestamp': msg.get('timestamp', obj.get('timestamp', '')),
                'content': parts,
            })
        return out

    def final(self, record: Dict) -> Optional[Dict]:
        path = record.get('file')
        if not path:
            return None
        last_assistant = None
        count = 0
        for obj in _iter_jsonl(path):
            if obj.get('type') != 'message':
                continue
            count += 1
            if (obj.get('message') or {}).get('role') == 'assistant':
                last_assistant = obj
        if last_assistant is None:
            return {'status': 'done', 'isFinal': False, 'isProcessing': False,
                    'messageCount': count, 'source': self.mode, 'text': '', 'thinking': ''}

        msg = last_assistant.get('message') or {}
        text_parts, think_parts = [], []
        for item in msg.get('content') or []:
            if not isinstance(item, dict):
                continue
            if item.get('type') == 'thinking':
                think_parts.append(item.get('thinking', ''))
            else:
                text_parts.append(_content_text(item))
        stop_reason = msg.get('stopReason', '')
        return {
            'status': 'done',
            'isFinal': stop_reason == 'stop',
            'isProcessing': False,
            'messageCount': count,
            'source': self.mode,
            'id': last_assistant.get('id', ''),
            'timestamp': msg.get('timestamp', last_assistant.get('timestamp', '')),
            'stopReason': stop_reason,
            'model': msg.get('model', ''),
            'text': "\n".join(p for p in text_parts if p),
            'thinking': "\n".join(p for p in think_parts if p),
            'toolCalls': [],
            'usage': msg.get('usage', {}),
        }


# ---------------------------------------------------------------------------
# 适配器 4/6：Claude Code（~/.claude/projects/<项目>/<session-uuid>.jsonl）
# ---------------------------------------------------------------------------

class ClaudeCodeSource(SessionSource):
    mode = 'claude'

    def __init__(self, root: Path = CLAUDE_PROJECTS_DIR):
        self.root = root
        self.location = str(root)

    def exists(self) -> bool:
        return self.root.exists()

    def _files(self):
        if not self.root.exists():
            return []
        return sorted(self.root.glob('*/*.jsonl'))

    def list(self) -> List[Dict]:
        out = []
        for path in self._files():
            head = _read_jsonl(path, limit=5)
            sid, cwd = path.stem, ''
            for obj in head:
                if obj.get('sessionId'):
                    sid = obj['sessionId']
                if obj.get('cwd'):
                    cwd = obj['cwd']
            out.append({
                'source': self.mode,
                'key': str(path),
                'shortKey': path.stem,
                'sessionId': str(sid),
                'file': str(path),
                'hasFile': True,
                'status': 'done',
                'cwd': cwd,
                'updatedAt': _mtime_iso(path),
                '_sortKey': _mtime_iso(path),
            })
        return out

    @staticmethod
    def _parts(content) -> List[Dict]:
        parts = []
        if isinstance(content, str):
            if content:
                parts.append({'type': 'text', 'content': content})
            return parts
        if not isinstance(content, list):
            return parts
        for item in content:
            if not isinstance(item, dict):
                continue
            kind = item.get('type')
            if kind == 'text':
                parts.append({'type': 'text', 'content': item.get('text', '')})
            elif kind == 'thinking':
                parts.append({'type': 'thinking', 'content': _trim(item.get('thinking', ''), 1000)})
            elif kind == 'tool_use':
                parts.append({'type': 'toolCall', 'name': item.get('name', ''),
                              'arguments': item.get('input', {})})
            elif kind == 'tool_result':
                parts.append({'type': 'toolResult', 'toolName': '',
                              'content': _trim(_content_text(item.get('content')), 500)})
        return parts

    def messages(self, record: Dict, limit: int = 50) -> List[Dict]:
        out = []
        path = record.get('file')
        if not path:
            return out
        for obj in _iter_jsonl(path):
            if len(out) >= limit:
                break
            if obj.get('type') not in ('user', 'assistant'):
                continue
            if obj.get('isSidechain'):  # 子代理的消息不计入主线
                continue
            msg = obj.get('message') or {}
            out.append({
                'id': obj.get('uuid', ''),
                'role': msg.get('role', obj.get('type')),
                'timestamp': obj.get('timestamp', ''),
                'content': self._parts(msg.get('content')),
            })
        return out

    def final(self, record: Dict) -> Optional[Dict]:
        path = record.get('file')
        if not path:
            return None
        last_assistant = None
        count = 0
        for obj in _iter_jsonl(path):
            if obj.get('type') not in ('user', 'assistant') or obj.get('isSidechain'):
                continue
            count += 1
            if obj.get('type') == 'assistant':
                last_assistant = obj
        if last_assistant is None:
            return {'status': 'done', 'isFinal': False, 'isProcessing': False,
                    'messageCount': count, 'source': self.mode, 'text': '', 'thinking': ''}

        msg = last_assistant.get('message') or {}
        parts = self._parts(msg.get('content'))
        text = "\n".join(p['content'] for p in parts if p['type'] == 'text' and p.get('content'))
        thinking = "\n".join(p['content'] for p in parts if p['type'] == 'thinking' and p.get('content'))
        tool_calls = [{'name': p.get('name', ''), 'arguments': p.get('arguments', {})}
                      for p in parts if p['type'] == 'toolCall']
        stop_reason = msg.get('stop_reason') or ''
        return {
            'status': 'done',
            'isFinal': stop_reason in ('end_turn', 'stop', 'stop_sequence'),
            'isProcessing': False,
            'messageCount': count,
            'source': self.mode,
            'id': msg.get('id', ''),
            'timestamp': last_assistant.get('timestamp', ''),
            'stopReason': stop_reason,
            'model': msg.get('model', ''),
            'text': text,
            'thinking': thinking,
            'toolCalls': tool_calls,
            'usage': msg.get('usage', {}),
        }


# ---------------------------------------------------------------------------
# 适配器 5/6：Codex（~/.codex/sessions/年/月/日/rollout-*.jsonl）
# ---------------------------------------------------------------------------

class CodexSource(SessionSource):
    mode = 'codex'

    def __init__(self, root: Path = CODEX_SESSIONS_DIR):
        self.root = root
        self.location = str(root)

    def exists(self) -> bool:
        return self.root.exists()

    def _files(self):
        if not self.root.exists():
            return []
        return sorted(self.root.glob('*/*/*/rollout-*.jsonl'))

    def list(self) -> List[Dict]:
        out = []
        for path in self._files():
            head = _read_jsonl(path, limit=1)
            payload = (head[0].get('payload') if head else {}) or {}
            out.append({
                'source': self.mode,
                'key': str(path),
                'shortKey': path.stem,
                'sessionId': str(payload.get('session_id') or payload.get('id') or path.stem),
                'file': str(path),
                'hasFile': True,
                'status': 'done',
                'cwd': payload.get('cwd', ''),
                'cliVersion': payload.get('cli_version', ''),
                'updatedAt': _mtime_iso(path),
                '_sortKey': _mtime_iso(path),
            })
        return out

    def messages(self, record: Dict, limit: int = 50) -> List[Dict]:
        out = []
        path = record.get('file')
        if not path:
            return out
        for obj in _iter_jsonl(path):
            if len(out) >= limit:
                break
            if obj.get('type') != 'response_item':
                continue
            payload = obj.get('payload') or {}
            if payload.get('type') != 'message':
                continue
            role = payload.get('role', 'unknown')
            if role == 'developer':  # 系统拼装的指令，不算对话
                continue
            parts = []
            for item in payload.get('content') or []:
                if isinstance(item, dict) and isinstance(item.get('text'), str):
                    parts.append({'type': 'text', 'content': item['text']})
            out.append({
                'id': payload.get('id', ''),
                'role': role,
                'timestamp': obj.get('timestamp', ''),
                'content': parts,
            })
        return out

    def final(self, record: Dict) -> Optional[Dict]:
        path = record.get('file')
        if not path:
            return None
        last_assistant = None
        count = 0
        usage = {}
        for obj in _iter_jsonl(path):
            if obj.get('type') == 'token_usage_record':
                usage = (obj.get('payload') or {}).get('usage', usage) or usage
                continue
            if obj.get('type') != 'response_item':
                continue
            payload = obj.get('payload') or {}
            if payload.get('type') != 'message' or payload.get('role') == 'developer':
                continue
            count += 1
            if payload.get('role') == 'assistant':
                last_assistant = obj
        if last_assistant is None:
            return {'status': 'done', 'isFinal': False, 'isProcessing': False,
                    'messageCount': count, 'source': self.mode, 'text': '', 'thinking': ''}

        payload = last_assistant.get('payload') or {}
        text = "\n".join(item.get('text', '') for item in payload.get('content') or []
                         if isinstance(item, dict) and isinstance(item.get('text'), str))
        return {
            'status': 'done',
            'isFinal': True,
            'isProcessing': False,
            'messageCount': count,
            'source': self.mode,
            'id': payload.get('id', ''),
            'timestamp': last_assistant.get('timestamp', ''),
            'stopReason': payload.get('stop_reason', ''),
            'text': text,
            'thinking': '',
            'toolCalls': [],
            'usage': usage,
        }


# ---------------------------------------------------------------------------
# 适配器 6/6：Gemini CLI（~/.gemini/tmp/<项目>/chats/session-*.jsonl）
# ---------------------------------------------------------------------------

class GeminiSource(SessionSource):
    mode = 'gemini'

    def __init__(self, root: Path = GEMINI_TMP_DIR):
        self.root = root
        self.location = str(root)

    def exists(self) -> bool:
        return self.root.exists()

    def _files(self):
        if not self.root.exists():
            return []
        return sorted(self.root.glob('*/chats/session-*.jsonl'))

    @staticmethod
    def _meta_of(path):
        """只读首行拿元数据（列表用，不扫全文件）"""
        for obj in _iter_jsonl(path):
            if obj.get('sessionId'):
                return obj
        return {}

    @staticmethod
    def _iter_entries(path):
        """逐行产出消息（生成器）；Gemini 的 jsonl 是「首行元数据 + $set 补丁 + 消息行」的追加日志"""
        for obj in _iter_jsonl(path):
            if '$set' in obj and isinstance(obj['$set'], dict):
                for m in obj['$set'].get('messages') or []:
                    if isinstance(m, dict) and 'type' in m:
                        yield m
                continue
            if 'type' in obj and not obj.get('sessionId'):
                yield obj

    def list(self) -> List[Dict]:
        out = []
        for path in self._files():
            meta = self._meta_of(path)
            # 用元数据里的时间；没有就退回文件修改时间（都不需要扫全文件）
            last_ts = meta.get('lastUpdated') or meta.get('startTime') or _mtime_iso(path)
            out.append({
                'source': self.mode,
                'key': str(path),
                'shortKey': path.stem,
                'sessionId': str(meta.get('sessionId') or path.stem),
                'file': str(path),
                'hasFile': True,
                'status': 'done',
                'project': path.parent.parent.name,
                'updatedAt': last_ts,
                '_sortKey': str(last_ts),
            })
        return out

    def messages(self, record: Dict, limit: int = 50) -> List[Dict]:
        path = record.get('file')
        if not path:
            return []
        out = []
        for m in self._iter_entries(path):
            if len(out) >= limit:
                break
            if m.get('type') not in ('user', 'gemini'):
                continue
            content = m.get('content')
            parts = []
            if isinstance(content, str):
                if content:
                    parts.append({'type': 'text', 'content': content})
            else:
                for item in content or []:
                    if isinstance(item, dict) and isinstance(item.get('text'), str):
                        parts.append({'type': 'text', 'content': item['text']})
            thoughts = m.get('thoughts')
            if isinstance(thoughts, str) and thoughts:
                parts.insert(0, {'type': 'thinking', 'content': _trim(thoughts, 1000)})
            out.append({
                'id': m.get('id', ''),
                'role': 'assistant' if m.get('type') == 'gemini' else 'user',
                'timestamp': m.get('timestamp', ''),
                'content': parts,
            })
        return out

    def final(self, record: Dict) -> Optional[Dict]:
        path = record.get('file')
        if not path:
            return None
        last_gemini, count = None, 0
        for m in self._iter_entries(path):
            if m.get('type') not in ('user', 'gemini'):
                continue
            count += 1
            if m.get('type') == 'gemini':
                last_gemini = m
        if last_gemini is None:
            return {'status': 'done', 'isFinal': False, 'isProcessing': False,
                    'messageCount': count, 'source': self.mode, 'text': '', 'thinking': ''}

        content = last_gemini.get('content')
        text = content if isinstance(content, str) else _content_text(content)
        return {
            'status': 'done',
            'isFinal': True,
            'isProcessing': False,
            'messageCount': count,
            'source': self.mode,
            'id': last_gemini.get('id', ''),
            'timestamp': last_gemini.get('timestamp', ''),
            'stopReason': 'stop',
            'model': last_gemini.get('model', ''),
            'text': text,
            'thinking': _content_text(last_gemini.get('thoughts')) if last_gemini.get('thoughts') else '',
            'toolCalls': [],
            'usage': last_gemini.get('tokens', {}),
        }


# ---------------------------------------------------------------------------
# 数据源装配
# ---------------------------------------------------------------------------

def build_sources(mode: str = 'auto') -> List[SessionSource]:
    """按运行模式装配数据源。

    - 指定单个模式：只启用它
    - all：六个都启用（缺的会警告）
    - auto：存在的数据源都启用（两个 json-map 源看 sessions.json，其它看目录）
    """
    factories = {
        'hermes': lambda: JsonMapSource(dict(_JSON_MAP_DEFS['hermes'])),
        'openclaw': lambda: JsonMapSource(dict(_JSON_MAP_DEFS['openclaw'])),
        'pi': PiSource,
        'claude': ClaudeCodeSource,
        'codex': CodexSource,
        'gemini': GeminiSource,
    }

    if mode in factories and mode != 'auto':
        return [factories[mode]()]

    enabled = []
    for name in KNOWN_MODES:
        source = factories[name]()
        if source.exists():
            enabled.append(source)
        elif mode == 'all':
            print(f"警告: 数据源不存在，已跳过: {name} ({source.location})")

    if enabled:
        return enabled

    print("警告: 未检测到任何数据源，默认使用 OpenClaw")
    return [JsonMapSource(dict(_JSON_MAP_DEFS['openclaw']))]


# 兼容旧名字
source_definitions = build_sources


def init_paths(mode='auto'):
    """兼容旧用法：返回第一个启用的数据源"""
    return build_sources(mode)[0]


# ---------------------------------------------------------------------------
# 统一查询入口
# ---------------------------------------------------------------------------

class SessionQueryAPI:
    """把请求分发到各个数据源适配器。

    会话列表按数据源做短 TTL 缓存（默认 2 秒，`--cache-ttl` 可调，0 = 不缓存）：
    查找会话、列表都用它，所以一次请求不必把所有会话文件重新扫一遍。
    消息与最终结果始终直接读文件，缓存只影响「有哪些会话」这层元数据。
    """

    def __init__(self, cache_ttl: float = 2.0):
        self._cache_ttl = max(0.0, float(cache_ttl))
        self._cache: Dict[str, Tuple[float, List[Dict]]] = {}
        self._cache_lock = threading.Lock()

    def _records_of(self, source: SessionSource) -> List[Dict]:
        """某个数据源的会话列表（带 TTL 缓存）"""
        if self._cache_ttl <= 0:
            return source.list()
        now = time.monotonic()
        with self._cache_lock:
            hit = self._cache.get(source.mode)
            if hit and now - hit[0] < self._cache_ttl:
                return hit[1]
        records = source.list()
        with self._cache_lock:
            self._cache[source.mode] = (now, records)
        return records

    def list_sessions(self) -> List[Dict]:
        out = []
        for source in ADAPTERS:
            try:
                out.extend(self._records_of(source))
            except Exception as e:
                print(f"[WARN] {source.mode} 列出会话失败: {e}", file=sys.stderr)
        out.sort(key=lambda item: item.get('_sortKey', ''), reverse=True)
        # 缓存里带着内部排序键，对外去掉（不改动缓存对象本身）
        return [{k: v for k, v in item.items() if not k.startswith('_')} for item in out]

    def find_session(self, pattern: str) -> Tuple[Optional[SessionSource], Optional[Dict]]:
        """在所有启用的数据源里找最佳匹配（精确优先，跨源不互相遮蔽）"""
        pattern = (pattern or '').strip()
        if pattern.startswith("Run: "):
            pattern = pattern[5:]
        if pattern.startswith("Session: "):
            pattern = pattern[9:]
        if not pattern:
            return None, None

        pattern_lower = pattern.lower()
        best_source, best_record, best_score = None, None, None
        for index, source in enumerate(ADAPTERS):
            try:
                records = self._records_of(source)
            except Exception as e:
                print(f"[WARN] {source.mode} 列出会话失败: {e}", file=sys.stderr)
                continue
            for record in records:
                rank = _match_rank(pattern_lower, record)
                if rank == -1:
                    continue
                score = (rank, index)
                if best_score is None or score < best_score:
                    best_source, best_record, best_score = source, record, score
        return best_source, best_record

    def get_session(self, pattern: str) -> Optional[Dict]:
        source, record = self.find_session(pattern)
        if record is None:
            return None
        # 与 /sessions 列表保持同一套字段（含 file）
        return {k: v for k, v in record.items() if not k.startswith('_')}

    def get_messages(self, pattern: str, limit: int = 50) -> Optional[List[Dict]]:
        source, record = self.find_session(pattern)
        if record is None:
            return None
        return source.messages(record, limit)

    def get_final_message(self, pattern: str) -> Optional[Dict]:
        source, record = self.find_session(pattern)
        if record is None:
            return None
        try:
            result = source.final(record)
        except Exception as e:
            print(f"[WARN] {source.mode} 解析最终结果失败: {e}", file=sys.stderr)
            result = None
        if result is None:
            result = {
                'status': record.get('status', 'unknown'),
                'isFinal': False,
                'isProcessing': record.get('status') == 'running',
                'messageCount': 0,
                'source': record.get('source'),
                'error': 'Session file not available yet (session may be still initializing)',
            }
        return result


SOURCES = build_sources(MODE)
ADAPTERS = SOURCES
PATHS = SOURCES[0]
class RequestHandler(BaseHTTPRequestHandler):
    # HTTP/1.1：客户端可以复用连接（响应都带 Content-Length）
    protocol_version = 'HTTP/1.1'
    api = SessionQueryAPI()

    # 从配置文件读取 token
    _auth_token = None

    @classmethod
    def set_auth_token(cls, token: str):
        cls._auth_token = token

    @classmethod
    def init_semaphore(cls, max_connections: int = 50):
        cls._max_connections = max_connections
        cls._semaphore = threading.BoundedSemaphore(max_connections)

    @classmethod
    def stats(cls) -> dict:
        with cls._stat_lock:
            active = 0
            if cls._semaphore:
                active = cls._max_connections - cls._semaphore._value
            return {
                'max_connections': cls._max_connections,
                'active_connections': active,
                'total_connections': cls._total_connections,
                'bad_requests': cls._bad_requests,
            }

    def setup(self):
        """设置连接级别参数：超时和统计"""
        super().setup()
        try:
            self.connection.settimeout(self._socket_timeout)
        except Exception:
            pass
        with self._stat_lock:
            RequestHandler._total_connections += 1

    # 信号量和统计计数器
    _semaphore = None
    _max_connections = 50
    _socket_timeout = 30
    _total_connections = 0
    _bad_requests = 0
    _stat_lock = threading.Lock()

    def _check_auth(self) -> bool:
        """检查 Authorization header"""
        if not self._auth_token:
            return True  # 未配置 token 时跳过认证
        auth_header = self.headers.get('Authorization', '')
        if auth_header.startswith('Bearer '):
            token = auth_header[7:]
            # 常量时间比较，避免按字符比较带来的时序侧信道
            try:
                return hmac.compare_digest(token, self._auth_token)
            except TypeError:
                return token == self._auth_token
        return False

    def _send_json(self, data: Any, status: int = 200):
        try:
            body = json.dumps(data, ensure_ascii=False).encode('utf-8')
            self.send_response(status)
            self.send_header('Content-Type', 'application/json; charset=utf-8')
            self.send_header('Content-Length', str(len(body)))
            self.send_header('Access-Control-Allow-Origin', '*')
            self.end_headers()
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError, OSError):
            pass  # 客户端已断开，忽略

    def _parse_path(self) -> Tuple[str, str, Dict]:
        """解析路径，返回 (path, query)"""
        parsed = urlparse(self.path)
        query = parse_qs(parsed.query)
        # 处理重复参数
        query = {k: v[0] if len(v) == 1 else v for k, v in query.items()}
        return parsed.path, query

    def do_GET(self):
        try:
            self._do_GET_impl()
        except Exception as e:
            print(f"[ERROR] Unhandled exception in do_GET: {e}", file=sys.stderr)
            traceback.print_exc(file=sys.stderr)
            try:
                self._send_json({'error': 'Internal server error'}, 500)
            except Exception:
                pass

    def _do_GET_impl(self):
        path, query = self._parse_path()

        # 健康检查 - 不要求认证，便于监控探活
        if path == '/health':
            stats = self.stats()
            self._send_json({
                'status': 'ok',
                'mode': MODE,
                'sources': [s.mode for s in SOURCES],
                'stats': stats,
            })
            return

        # 根路径 - 不要求认证
        if path == '/' or path == '':
            self._send_json({
                'name': 'Agent Session API',
                'mode': MODE,
                'sources': [s.mode for s in SOURCES],
                'endpoints': [
                    'GET /sessions - 列出所有 session',
                    'GET /sessions/<pattern> - 查询单个 session',
                    'GET /sessions/<pattern>/messages?limit=50 - 获取消息',
                    'GET /sessions/<pattern>/final - 获取最终结果',
                    'GET /health - 健康检查',
                    'GET /stats - 服务器统计',
                ]
            })
            return

        # 服务器统计 - 不要求认证
        if path == '/stats':
            self._send_json(self.stats())
            return

        # /api 前缀兼容：统一在这里去掉，下面的路由只写一遍
        if path.startswith('/api/'):
            path = path[4:]

        # 检查认证（其余所有端点）
        if not self._check_auth():
            self._send_json({'error': 'Unauthorized'}, 401)
            return

        # 列出所有 session
        if path == '/sessions':
            sessions = self.api.list_sessions()
            self._send_json({'sessions': sessions, 'total': len(sessions)})
            return

        # 获取单个 session 消息 - 需要在 /sessions/<pattern> 之前匹配
        match = re.match(r'^/sessions/([^/]+)/messages$', path)
        if match:
            pattern = unquote(match.group(1))
            limit = 50
            if 'limit' in query:
                try:
                    limit = int(query['limit'])
                except Exception:
                    limit = 50
            messages = self.api.get_messages(pattern, limit)
            if messages is None:
                self._send_json({'error': 'Session not found'}, 404)
                return
            self._send_json({'messages': messages, 'total': len(messages)})
            return

        # 获取单个 session 最终结果
        match = re.match(r'^/sessions/([^/]+)/final$', path)
        if match:
            pattern = unquote(match.group(1))
            result = self.api.get_final_message(pattern)
            if result is None:
                self._send_json({'error': 'Session not found'}, 404)
                return
            self._send_json(result)
            return

        # 获取单个 session 信息
        match = re.match(r'^/sessions/([^/]+)$', path)
        if match:
            pattern = unquote(match.group(1))
            session = self.api.get_session(pattern)
            if session is None:
                self._send_json({'error': 'Session not found'}, 404)
                return
            self._send_json(session)
            return

        self._send_json({'error': 'Not found'}, 404)

    def do_OPTIONS(self):
        self.send_response(200)
        self.send_header('Access-Control-Allow-Origin', '*')
        self.send_header('Access-Control-Allow-Methods', 'GET, OPTIONS')
        self.send_header('Access-Control-Allow-Headers', 'Content-Type')
        self.send_header('Content-Length', '0')
        self.end_headers()

    def log_message(self, format, *args):
        """覆写日志方法，防止 stdout 写入阻塞导致服务卡死"""
        try:
            sys.stderr.write("%s - - [%s] %s\n" %
                             (self.client_address[0],
                              self.log_date_time_string(),
                              format % args))
        except Exception:
            pass  # 忽略日志写入失败

    def handle_one_request(self):
        """覆写 handle_one_request，捕获 HTTP 协议解析层的所有异常。
        这是防御恶意扫描（TLS握手字节、HTTP/2 PRI等）发送到 HTTP 端口的关键层。"""
        try:
            super().handle_one_request()
        except (ConnectionResetError, ConnectionAbortedError, BrokenPipeError):
            self.close_connection = True
        except TimeoutError:
            self.close_connection = True
            with self._stat_lock:
                RequestHandler._bad_requests += 1
        except ValueError:
            # 畸形请求行解析失败（日志中常见的 Bad request version/syntax）
            self.close_connection = True
            with self._stat_lock:
                RequestHandler._bad_requests += 1
        except Exception:
            self.close_connection = True
            with self._stat_lock:
                RequestHandler._bad_requests += 1

    def handle(self):
        """覆写 handle，使用信号量限制并发连接数，捕获连接级异常防止服务崩溃"""
        acquired = False
        if self._semaphore:
            try:
                acquired = self._semaphore.acquire(timeout=10)
            except Exception:
                pass
            if not acquired:
                try:
                    # 过载时返回 503
                    self.send_error(503, "Service temporarily overloaded")
                except Exception:
                    pass
                return

        try:
            super().handle()
        except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError, OSError):
            pass
        except TimeoutError:
            pass
        except Exception as e:
            print(f"[ERROR] Unhandled exception in handle: {e}", file=sys.stderr)
        finally:
            if acquired and self._semaphore:
                try:
                    self._semaphore.release()
                except ValueError:
                    pass


class RobustThreadingHTTPServer(ThreadingHTTPServer):
    """增强版 ThreadingHTTPServer，配置防御性 socket 选项"""

    allow_reuse_address = True
    daemon_threads = True  # 线程随主进程退出，防止残留

    def server_bind(self):
        """绑定地址并设置 socket 选项"""
        super().server_bind()
        try:
            # TCP keepalive: 检测死连接，60秒空闲后开始探测
            self.socket.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
        except Exception:
            pass
        try:
            # TCP_NODELAY: 禁用 Nagle 算法，减少延迟
            self.socket.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        except Exception:
            pass

    def server_activate(self):
        """激活服务器，设置 accept 队列大小"""
        super().server_activate()

    def handle_error(self, request, client_address):
        """处理 accept 级别的错误"""
        print(f"[SERVER_ERROR] 处理来自 {client_address} 的连接时出错",
              file=sys.stderr)
        traceback.print_exc(file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description='OpenClaw/Hermes Session HTTP API (自动检测模式)')
    parser.add_argument('--host', default='0.0.0.0', help='绑定主机 (默认: 0.0.0.0)')
    parser.add_argument('--port', type=int, default=8080, help='端口 (默认: 8080)')
    parser.add_argument('--mode', choices=['auto', 'all'] + KNOWN_MODES, default='auto',
                        help='模式 (默认: auto 自动检测；all 全部启用；也可指定 ' + '/'.join(KNOWN_MODES) + ')')
    parser.add_argument('--hook_token', default=None, help='Bearer hook_token for authentication')
    parser.add_argument('--max-connections', type=int, default=50, help='最大并发连接数 (默认: 50)')
    parser.add_argument('--cache-ttl', type=float, default=2.0,
                        help='会话列表缓存秒数 (默认: 2；0 = 每次重新扫描，完全不做缓存)')
    parser.add_argument('--timeout', type=int, default=30, help='连接超时秒数 (默认: 30)')
    args = parser.parse_args()

    # 设置模式
    global MODE, SOURCES, PATHS
    MODE = args.mode
    SOURCES = source_definitions(MODE)
    PATHS = SOURCES[0]
    print(f"运行模式: {MODE}")
    for source in SOURCES:
        print(f"数据源 [{source.mode}]: {source.location}")

    # 设置认证 hook_token
    if args.hook_token:
        RequestHandler.set_auth_token(args.hook_token)
        print("已启用认证（Bearer hook_token 已设置，不回显）")
    else:
        print("警告: 未设置认证hook_token，API 公开访问")

    # 初始化连接限制与会话列表缓存
    RequestHandler._socket_timeout = args.timeout
    RequestHandler.init_semaphore(args.max_connections)
    RequestHandler.api = SessionQueryAPI(args.cache_ttl)
    print(f"最大并发连接数: {args.max_connections}, 连接超时: {args.timeout}s, 列表缓存: {args.cache_ttl}s")

    server = RobustThreadingHTTPServer((args.host, args.port), RequestHandler)
    print(f"\nSession API 启动成功")
    print(f"监听地址: http://{args.host}:{args.port}")
    print(f"\nAPI 端点:")
    print(f"  GET /sessions                        - 列出所有 session")
    print(f"  GET /sessions/<pattern>              - 查询单个 session")
    print(f"  GET /sessions/<pattern>/messages    - 获取消息")
    print(f"  GET /sessions/<pattern>/final         - 获取最终结果")
    print(f"  GET /health                          - 健康检查 (含服务统计)")
    print(f"\n示例:")
    print(f"  curl -H 'Authorization: Bearer xxx' http://localhost:{args.port}/sessions")
    enabled_modes = {s.mode for s in SOURCES}
    if 'hermes' in enabled_modes:
        print(f"  curl -H 'Authorization: Bearer xxx' http://localhost:{args.port}/sessions/20260419_143935_73e269b4/final")
    if 'openclaw' in enabled_modes:
        print(f"  curl -H 'Authorization: Bearer xxx' http://localhost:{args.port}/sessions/hook:alert:prometheus:b5123b01")
    print(f"\n按 Ctrl+C 停止服务")

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n正在停止服务...")
        server.shutdown()
    except Exception as e:
        print(f"\n[FATAL] 服务异常退出: {e}", file=sys.stderr)
        traceback.print_exc(file=sys.stderr)
        server.shutdown()


if __name__ == '__main__':
    main()
