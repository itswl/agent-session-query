#!/usr/bin/env python3
"""
OpenClaw/Hermes Session HTTP API 服务

启动方式:
    python3 openclaw_session_query_api.py [--port 8080]

自动检测模式:
    - 优先检测 Hermes (~/.hermes/sessions/sessions.json)
    - 其次检测 OpenClaw (~/.openclaw/agents/default/sessions/sessions.json)
    - 可通过 --mode 强制指定模式

API 端点:

GET /sessions
    - 列出所有 session

GET /sessions/<pattern>
    - 根据 Run ID 或 Session pattern 查询
    - 例如: /sessions/5ab8e024-2740-422c-8503-89c01313f792
    - 例如: /sessions/hook:alert:prometheus:b5123b01-616a-4da0-ac48-d9c81e3be63c

GET /sessions/<pattern>/messages?limit=50
    - 获取 session 的消息内容

GET /health
    - 健康检查
"""

import hmac
import json
import os
import re
import sys
import socket
import sqlite3
import threading
import traceback
from pathlib import Path
from http.server import HTTPServer, BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs, unquote
from typing import Optional, Dict, Any, List, Tuple
import argparse
import select

# 默认模式（自动检测）
MODE = 'auto'

def init_paths(mode='auto'):
    """根据模式初始化路径"""
    if mode == 'hermes':
        return {
            'mode': 'hermes',
            'sessions_json': Path.home() / ".hermes/sessions/sessions.json",
            'sessions_dir': Path.home() / ".hermes/sessions",
            'session_id_field': 'session_id',
            'stop_reason_field': 'finish_reason',
        }
    elif mode == 'openclaw':
        return {
            'mode': 'openclaw',
            'sessions_json': Path.home() / ".openclaw/agents/default/sessions/sessions.json",
            'sessions_dir': Path.home() / ".openclaw/agents/default/sessions",
            'session_id_field': 'sessionId',
            'stop_reason_field': 'stopReason',
        }
    else:  # auto - 自动检测
        hermes_json = Path.home() / ".hermes/sessions/sessions.json"
        openclaw_json = Path.home() / ".openclaw/agents/default/sessions/sessions.json"
        
        if hermes_json.exists():
            print(f"自动检测到 Hermes 数据源: {hermes_json}")
            return {
                'mode': 'hermes',
                'sessions_json': hermes_json,
                'sessions_dir': Path.home() / ".hermes/sessions",
                'session_id_field': 'session_id',
                'stop_reason_field': 'finish_reason',
            }
        elif openclaw_json.exists():
            print(f"自动检测到 OpenClaw 数据源: {openclaw_json}")
            return {
                'mode': 'openclaw',
                'sessions_json': openclaw_json,
                'sessions_dir': Path.home() / ".openclaw/agents/default/sessions",
                'session_id_field': 'sessionId',
                'stop_reason_field': 'stopReason',
            }
        else:
            # 默认使用 OpenClaw
            print("警告: 未检测到数据源，默认使用 OpenClaw")
            return {
                'mode': 'openclaw',
                'sessions_json': openclaw_json,
                'sessions_dir': Path.home() / ".openclaw/agents/default/sessions",
                'session_id_field': 'sessionId',
                'stop_reason_field': 'stopReason',
            }

PATHS = init_paths(MODE)


class OpenClawAPI:
    def __init__(self):
        pass  # 不缓存任何数据

    def _load_sessions(self) -> Dict[str, Any]:
        if not PATHS['sessions_json'].exists():
            return {}
        with open(PATHS['sessions_json'], 'r') as f:
            return json.load(f)

    def _find_session(self, pattern: str) -> Optional[Tuple[str, Dict]]:
        """根据模式查找 session - 每次都重新加载"""
        sessions = self._load_sessions()

        pattern = pattern.strip()

        # 处理 Run: 或 Session: 前缀
        if pattern.startswith("Run: "):
            pattern = pattern[5:]
        if pattern.startswith("Session: "):
            pattern = pattern[9:]

        session_id_field = PATHS['session_id_field']
        
        # 精确匹配 sessionId/session_id
        for key, info in sessions.items():
            if info.get(session_id_field, '').lower() == pattern.lower():
                return key, info

        # 匹配完整 key 或 key 的后半部分
        pattern_lower = pattern.lower()
        for key, info in sessions.items():
            key_lower = key.lower()
            # 精确匹配完整 key
            if key_lower == pattern_lower:
                return key, info
            # key 以 pattern 结尾（去掉 agent:default: 或 agent:main: 前缀后）
            if key_lower.endswith(':' + pattern_lower):
                return key, info
            # pattern 是 key 的最后一部分
            if pattern_lower in key_lower:
                return key, info

        # 模糊匹配 ID 部分
        for key, info in sessions.items():
            if pattern_lower in info.get(session_id_field, '').lower():
                return key, info

        return None, None

    def _extract_messages(self, session_file: str, limit: int = 50) -> List[Dict]:
        """从 jsonl 文件提取消息"""
        messages = []
        path = Path(session_file)

        if not path.exists():
            return messages

        # 流式读取：取到 limit 条就停，不必把整个 jsonl 读进内存
        with open(path, 'r') as f:
            for line in f:
                if len(messages) >= limit:
                    break
                try:
                    data = json.loads(line.strip())
                    # OpenClaw 格式: type=message
                    if data.get('type') == 'message':
                        messages.append(data)
                    # Hermes 格式: role=user/assistant
                    elif data.get('role') in ['user', 'assistant']:
                        messages.append(data)
                except json.JSONDecodeError:
                    continue

        return messages

    def _format_message(self, msg: Dict) -> Dict:
        """格式化单条消息"""
        # OpenClaw 格式
        if msg.get('type') == 'message':
            content = msg.get('message', {}).get('content', [])
            role = msg.get('message', {}).get('role', 'unknown')
        # Hermes 格式
        else:
            content = msg.get('content', '')
            role = msg.get('role', 'unknown')

        parts = []
        
        # OpenClaw: content 是数组
        if isinstance(content, list):
            for item in content:
                if isinstance(item, dict):
                    if item.get('type') == 'text':
                        parts.append({'type': 'text', 'content': item.get('text', '')})
                    elif item.get('type') == 'thinking':
                        thinking = item.get('thinking', '')
                        if len(thinking) > 1000:
                            thinking = thinking[:1000] + '...[truncated]'
                        parts.append({'type': 'thinking', 'content': thinking})
                    elif item.get('type') == 'toolCall':
                        parts.append({
                            'type': 'toolCall',
                            'name': item.get('name', ''),
                            'arguments': item.get('arguments', {})
                        })
                    elif item.get('type') == 'toolResult':
                        result = item.get('content', [])
                        result_text = ''
                        for r in result:
                            if isinstance(r, dict) and r.get('type') == 'text':
                                result_text = r.get('text', '')
                                if len(result_text) > 500:
                                    result_text = result_text[:500] + '...[truncated]'
                        parts.append({
                            'type': 'toolResult',
                            'toolName': item.get('toolName', ''),
                            'content': result_text
                        })
        # Hermes: content 是字符串
        elif isinstance(content, str):
            if role == 'assistant':
                # 提取 reasoning (thinking)
                reasoning = msg.get('reasoning', '')
                if reasoning:
                    if len(reasoning) > 1000:
                        reasoning = reasoning[:1000] + '...[truncated]'
                    parts.append({'type': 'thinking', 'content': reasoning})
                # 添加文本内容
                parts.append({'type': 'text', 'content': content})
            else:
                parts.append({'type': 'text', 'content': content})

        return {
            'id': msg.get('id', ''),
            'role': role,
            'timestamp': msg.get('timestamp', ''),
            'content': parts
        }

    def _load_sqlite_final_message(self, session_id: str, status: str = 'done') -> Optional[Dict]:
        """Hermes webhook sessions may persist final messages in state.db only."""
        if PATHS.get('mode') != 'hermes' or not session_id:
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
                    'id': row['id'],
                    'timestamp': row['timestamp'],
                    'stopReason': row['finish_reason'] or 'stop',
                    'text': row['content'] or '',
                    'thinking': row['reasoning'] or '',
                    'toolCalls': [],
                    'usage': usage,
                    'source': 'sqlite',
                }
            finally:
                conn.close()
        except Exception as e:
            print(f"[WARN] Failed to read Hermes SQLite final message for {session_id}: {e}", file=sys.stderr)
            return None

    def list_sessions(self) -> List[Dict]:
        """列出所有 session - 每次都重新加载（按当前模式取字段）"""
        sessions = self._load_sessions()
        session_id_field = PATHS['session_id_field']
        is_openclaw = PATHS['mode'] == 'openclaw'

        result = []
        for key, info in sessions.items():
            session_id = info.get(session_id_field, '')
            # 兼容两种命名的兜底，避免数据里字段名不一致时列表为空
            if not session_id:
                session_id = info.get('sessionId', '') or info.get('session_id', '')
            session_file = info.get('sessionFile', '')
            file_exists = Path(session_file).exists() if session_file else False

            item = {
                'key': key,
                'shortKey': key.replace('agent:default:', '') if is_openclaw else key,
                'sessionId': session_id,
                'hasFile': file_exists,
            }

            if is_openclaw:
                updated = info.get('updatedAt', 0)
                updated_str = ''
                if updated:
                    from datetime import datetime
                    dt = datetime.fromtimestamp(updated/1000)
                    updated_str = dt.strftime('%Y-%m-%d %H:%M:%S')
                item.update({
                    'status': info.get('status', 'unknown'),
                    'updatedAt': updated_str,
                    'model': info.get('model', ''),
                    'runtimeMs': info.get('runtimeMs', 0),
                    'totalTokens': info.get('totalTokens', 0),
                })
            else:  # hermes
                item.update({
                    'status': 'done',
                    'updatedAt': info.get('updated_at', ''),
                    'createdAt': info.get('created_at', ''),
                    'displayName': info.get('display_name', ''),
                    'platform': info.get('platform', ''),
                    'totalTokens': info.get('total_tokens', 0),
                    'estimatedCostUsd': info.get('estimated_cost_usd', 0),
                })

            result.append(item)
        return result

    def get_session(self, pattern: str) -> Optional[Dict]:
        """获取单个 session 信息"""
        key, info = self._find_session(pattern)
        if info is None:
            return None

        session_id_field = PATHS['session_id_field']
        session_id = info.get(session_id_field, '')
        session_file = info.get('sessionFile', '')
        file_exists = Path(session_file).exists() if session_file else False

        # 如果 sessionFile 不存在但 sessionId 存在，尝试在 SESSIONS_DIR 中查找
        if not file_exists and session_id:
            alt_path = PATHS['sessions_dir'] / f"{session_id}.jsonl"
            if alt_path.exists():
                session_file = str(alt_path)
                file_exists = True

        result = {
            'key': key,
            'sessionId': session_id,
            'sessionFile': session_file if file_exists else None,
            'hasFile': file_exists,
        }
        
        # 根据模式添加不同字段（用 PATHS['mode']，--mode auto 时它与 MODE 不一定相同）
        if PATHS['mode'] == 'openclaw':
            result.update({
                'status': info.get('status', 'unknown'),
                'updatedAt': info.get('updatedAt', 0),
                'model': info.get('model', ''),
                'runtimeMs': info.get('runtimeMs', 0),
                'inputTokens': info.get('inputTokens', 0),
                'outputTokens': info.get('outputTokens', 0),
                'totalTokens': info.get('totalTokens', 0),
                'estimatedCostUsd': info.get('estimatedCostUsd', 0),
            })
        else:  # hermes
            result.update({
                'status': 'done',  # hermes 可能没有 status 字段
                'createdAt': info.get('created_at', ''),
                'updatedAt': info.get('updated_at', ''),
                'displayName': info.get('display_name', ''),
                'platform': info.get('platform', ''),
                'inputTokens': info.get('input_tokens', 0),
                'outputTokens': info.get('output_tokens', 0),
                'totalTokens': info.get('total_tokens', 0),
                'estimatedCostUsd': info.get('estimated_cost_usd', 0),
            })
        
        return result

    def get_messages(self, pattern: str, limit: int = 50) -> Optional[List[Dict]]:
        """获取 session 的消息"""
        key, info = self._find_session(pattern)
        if info is None:
            return None

        session_id_field = PATHS['session_id_field']
        session_id = info.get(session_id_field, '')
        session_file = info.get('sessionFile', '')

        # 查找实际文件
        path = None
        if session_file and Path(session_file).exists():
            path = Path(session_file)
        elif session_id:
            alt_path = PATHS['sessions_dir'] / f"{session_id}.jsonl"
            if alt_path.exists():
                path = alt_path

        if path is None:
            return []

        messages = self._extract_messages(str(path), limit)
        formatted = [self._format_message(m) for m in messages]
        return formatted

    def get_final_message(self, pattern: str) -> Optional[Dict]:
        """获取 session 的最终结果（第一个 finish_reason/stopReason="stop" 的 assistant 消息）"""
        key, info = self._find_session(pattern)
        if info is None:
            return None

        session_id_field = PATHS['session_id_field']
        stop_reason_field = PATHS['stop_reason_field']
        session_id = info.get(session_id_field, '')
        session_file = info.get('sessionFile', '')
        status = info.get('status', 'done')  # hermes 默认 done

        # 查找实际文件
        path = None
        if session_file and Path(session_file).exists():
            path = Path(session_file)
        elif session_id:
            alt_path = PATHS['sessions_dir'] / f"{session_id}.jsonl"
            if alt_path.exists():
                path = alt_path

        if path is None:
            sqlite_result = self._load_sqlite_final_message(session_id, status)
            if sqlite_result is not None:
                return sqlite_result
            return {
                'status': status,
                'isFinal': False,
                'isProcessing': status == 'running',
                'messageCount': 0,
                'error': 'Session file not available yet (session may be still initializing)'
            }

        # 单次读取：收集 assistant 消息、第一个 "stop" 消息、以及 toolResult 的 parentId
        all_messages = []
        first_stop_message = None
        tool_result_parents = set()

        with open(path, 'r') as f:
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
                        # 保存时包含 msg 里的 finish_reason/stopReason
                        msg_stop_reason = msg.get(stop_reason_field) or data.get(stop_reason_field) or ''
                        data['_msg_stopReason'] = msg_stop_reason
                        all_messages.append(data)
                        # 记录第一个 finish_reason/stopReason="stop" 的消息
                        if first_stop_message is None and msg_stop_reason == 'stop':
                            first_stop_message = data
                    elif role == 'toolResult':
                        parent_id = data.get('parentId')
                        if parent_id:
                            tool_result_parents.add(parent_id)
                # Hermes 格式
                elif data.get('role') == 'assistant':
                    # 保存时包含 finish_reason/stopReason
                    msg_stop_reason = data.get(stop_reason_field, '')
                    data['_msg_stopReason'] = msg_stop_reason
                    all_messages.append(data)
                    # 记录第一个 finish_reason/stopReason="stop" 的消息
                    if first_stop_message is None and msg_stop_reason == 'stop':
                        first_stop_message = data

        # 第一个 stop 消息之后还有指向它的 toolResult，说明可能仍在处理中
        is_processing = False
        if first_stop_message and status == 'running' and first_stop_message.get('id', '') in tool_result_parents:
            is_processing = True

        if first_stop_message is None:
            sqlite_result = self._load_sqlite_final_message(session_id, status)
            if sqlite_result is not None:
                return sqlite_result
            return {
                'status': status,
                'isFinal': False,
                'isProcessing': status == 'running',
                'messageCount': len(all_messages),
                'text': '',
                'thinking': '',
            }

        # 返回第一个 finish_reason/stopReason="stop" 的消息
        last = first_stop_message
        
        # OpenClaw 格式
        if last.get('type') == 'message':
            msg_data = last.get('message', {})
            content = msg_data.get('content', [])
            # 优先从 msg 里的 finish_reason/stopReason 获取，其次从顶层获取
            stop_reason = last.get('_msg_stopReason') or msg_data.get(stop_reason_field) or last.get(stop_reason_field, '')
        # Hermes 格式
        else:
            content = last.get('content', '')
            stop_reason = last.get('_msg_stopReason', '')

        # 如果 stopReason 是 "stop"，认为已结束
        is_stopped = stop_reason == 'stop'
        is_done = status == 'done' or is_stopped

        result = {
            'status': status,
            'isFinal': bool(is_done and not is_processing),
            'isProcessing': bool(is_processing or (status == 'running' and not is_stopped)),
            'messageCount': len(all_messages),
            'id': last.get('id', ''),
            'timestamp': last.get('timestamp', ''),
            'stopReason': stop_reason,
            'text': '',
            'thinking': '',
            'toolCalls': [],
            'usage': last.get('usage', {}),
        }

        # OpenClaw: content 是数组
        if isinstance(content, list):
            for c in content:
                if isinstance(c, dict):
                    if c.get('type') == 'text':
                        result['text'] = c.get('text', '')
                    elif c.get('type') == 'thinking':
                        result['thinking'] = c.get('thinking', '')
                    elif c.get('type') == 'toolCall':
                        result['toolCalls'].append({
                            'name': c.get('name', ''),
                            'arguments': c.get('arguments', {})
                        })
        # Hermes: content 是字符串
        elif isinstance(content, str):
            result['text'] = content
            # 提取 reasoning
            result['thinking'] = last.get('reasoning', '')

        return result


class RequestHandler(BaseHTTPRequestHandler):
    api = OpenClawAPI()

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
                'mode': PATHS['mode'],
                'stats': stats,
            })
            return

        # 根路径 - 不要求认证
        if path == '/' or path == '':
            self._send_json({
                'name': 'OpenClaw Session API',
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
    parser.add_argument('--mode', choices=['auto', 'openclaw', 'hermes'], default='auto', help='模式 (默认: auto 自动检测)')
    parser.add_argument('--hook_token', default=None, help='Bearer hook_token for authentication')
    parser.add_argument('--max-connections', type=int, default=50, help='最大并发连接数 (默认: 50)')
    parser.add_argument('--timeout', type=int, default=30, help='连接超时秒数 (默认: 30)')
    args = parser.parse_args()

    # 设置模式
    global MODE, PATHS
    MODE = args.mode
    PATHS = init_paths(MODE)
    print(f"运行模式: {PATHS['mode']}")
    print(f"Sessions JSON: {PATHS['sessions_json']}")
    print(f"Sessions Dir: {PATHS['sessions_dir']}")

    # 设置认证 hook_token
    if args.hook_token:
        RequestHandler.set_auth_token(args.hook_token)
        print("已启用认证（Bearer hook_token 已设置，不回显）")
    else:
        print("警告: 未设置认证hook_token，API 公开访问")

    # 初始化连接限制
    RequestHandler._socket_timeout = args.timeout
    RequestHandler.init_semaphore(args.max_connections)
    print(f"最大并发连接数: {args.max_connections}, 连接超时: {args.timeout}s")

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
    print(f"  curl http://localhost:{args.port}/sessions")
    if MODE == 'hermes':
        print(f"  curl http://localhost:{args.port}/sessions/1776580775689")
        print(f"  curl http://localhost:{args.port}/sessions/20260419_143935_73e269b4/final")
    else:
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
