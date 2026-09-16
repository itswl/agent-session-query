package app

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// apiServer：路由 + 认证 + 连接限制 + 统计
type apiServer struct {
	mode    string
	sources []SessionSource
	api     *SessionQueryAPI
	token   string

	maxConnections int

	mu          sync.Mutex
	totalConns  int64
	activeConns int64
	badRequests int64
}

func newAPIServer(mode string, sources []SessionSource, api *SessionQueryAPI, token string, maxConnections int) *apiServer {
	return &apiServer{mode: mode, sources: sources, api: api, token: token, maxConnections: maxConnections}
}

func (s *apiServer) stats() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"max_connections":    s.maxConnections,
		"active_connections": s.activeConns,
		"total_connections":  s.totalConns,
		"bad_requests":       s.badRequests,
	}
}

// connState 用来统计连接数（active = 当前打开的连接）
func (s *apiServer) connState(_ net.Conn, state http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch state {
	case http.StateNew:
		s.totalConns++
		s.activeConns++
	case http.StateClosed, http.StateHijacked:
		s.activeConns--
	}
}

func (s *apiServer) checkAuth(r *http.Request) bool {
	if s.token == "" {
		return true // 未配置 token 时跳过认证
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	// 常量时间比较，避免按字符比较带来的时序侧信道
	return subtle.ConstantTimeCompare([]byte(header[len("Bearer "):]), []byte(s.token)) == 1
}

// ---------------------------------------------------------------------------
// 响应
// ---------------------------------------------------------------------------

// writeJSON 输出 JSON（不转义 HTML，与 Python 的 json.dumps(ensure_ascii=False) 对齐）
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		body := []byte(`{"error":"Internal server error"}`)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(body)
		return
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

func (s *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Unhandled exception: %v\n", rec)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal server error"})
		}
	}()

	w.Header().Set("Access-Control-Allow-Origin", "*")

	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodGet, http.MethodHead:
		// 继续
	default:
		writePlain(w, http.StatusNotImplemented, fmt.Sprintf("Unsupported method ('%s')", r.Method))
		return
	}

	status := s.route(w, r)
	logRequest(r, status)
}

func (s *apiServer) route(w http.ResponseWriter, r *http.Request) int {
	// 用 EscapedPath 分段（%2F 不当作分隔符），与 Python 版先匹配再 unquote 一致
	path := r.URL.EscapedPath()

	// 健康检查 - 不要求认证，便于监控探活
	if path == "/health" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"mode":    s.mode,
			"sources": sourceModes(s.sources),
			"stats":   s.stats(),
		})
		return http.StatusOK
	}

	// 根路径 - 不要求认证
	if path == "/" || path == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"name":    "Agent Session API",
			"mode":    s.mode,
			"sources": sourceModes(s.sources),
			"endpoints": []string{
				"GET /sessions - 列出所有 session",
				"GET /sessions/<pattern> - 查询单个 session",
				"GET /sessions/<pattern>/messages?limit=50 - 获取消息",
				"GET /sessions/<pattern>/final - 获取最终结果",
				"GET /health - 健康检查",
				"GET /stats - 服务器统计",
			},
		})
		return http.StatusOK
	}

	// 服务器统计 - 不要求认证
	if path == "/stats" {
		writeJSON(w, http.StatusOK, s.stats())
		return http.StatusOK
	}

	// 内嵌的只读页面 - 不要求认证（页面里没有数据，数据仍要带 token 走 /sessions）
	if isUIPath(path) {
		serveUIAssets(w, r, path)
		return http.StatusOK
	}

	// /api 前缀兼容：统一在这里去掉（只去 "/api" 四个字符，保留后面的 "/"）
	if strings.HasPrefix(path, "/api/") {
		path = path[len("/api"):]
	}

	// 其余所有端点都要认证
	if !s.checkAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return http.StatusUnauthorized
	}

	if path == "/sessions" {
		sessions := s.api.listSessions()
		writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions, "total": len(sessions)})
		return http.StatusOK
	}

	if strings.HasPrefix(path, "/sessions/") {
		rest := path[len("/sessions/"):]
		parts := strings.Split(rest, "/")
		switch {
		case len(parts) == 1 && parts[0] != "":
			pattern := unescapePattern(parts[0])
			session, ok := s.api.getSession(pattern)
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "Session not found"})
				return http.StatusNotFound
			}
			writeJSON(w, http.StatusOK, session)
			return http.StatusOK

		case len(parts) == 2 && parts[0] != "" && parts[1] == "messages":
			pattern := unescapePattern(parts[0])
			messages, ok := s.api.getMessages(pattern, parseLimit(r))
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "Session not found"})
				return http.StatusNotFound
			}
			writeJSON(w, http.StatusOK, map[string]any{"messages": messages, "total": len(messages)})
			return http.StatusOK

		case len(parts) == 2 && parts[0] != "" && parts[1] == "final":
			pattern := unescapePattern(parts[0])
			result, ok := s.api.getFinalMessage(pattern)
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "Session not found"})
				return http.StatusNotFound
			}
			writeJSON(w, http.StatusOK, result)
			return http.StatusOK
		}
	}

	writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
	return http.StatusNotFound
}

func unescapePattern(segment string) string {
	if pattern, err := url.PathUnescape(segment); err == nil {
		return pattern
	}
	return segment
}

// parseLimit 取 ?limit=，非法或缺省都是 50（与 Python 的 int() 失败回退一致）
func parseLimit(r *http.Request) int {
	values, ok := r.URL.Query()["limit"]
	if !ok || len(values) != 1 {
		return 50
	}
	limit, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil {
		return 50
	}
	return limit
}

func logRequest(r *http.Request, status int) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	fmt.Fprintf(os.Stderr, "%s - - [%s] \"%s %s %s\" %d\n",
		host, time.Now().Format("02/Jan/2006:15:04:05 -0700"),
		r.Method, r.URL.RequestURI(), r.Proto, status)
}

// ---------------------------------------------------------------------------
// 连接限制：超过上限的连接直接返回 503（与 Python 版每连接一个许可一致）
// ---------------------------------------------------------------------------

type limitListener struct {
	net.Listener
	sem     chan struct{}
	timeout time.Duration
	server  *apiServer
}

func newLimitListener(inner net.Listener, maxConnections int) *limitListener {
	if maxConnections < 1 {
		maxConnections = 1
	}
	return &limitListener{Listener: inner, sem: make(chan struct{}, maxConnections), timeout: 10 * time.Second}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		select {
		case l.sem <- struct{}{}:
			return &releaseConn{Conn: conn, release: func() { <-l.sem }}, nil
		default:
		}

		// 已满：最多等 10 秒，等不到就 503
		timer := time.NewTimer(l.timeout)
		select {
		case l.sem <- struct{}{}:
			timer.Stop()
			return &releaseConn{Conn: conn, release: func() { <-l.sem }}, nil
		case <-timer.C:
			writeOverloaded(conn)
			_ = conn.Close()
			if l.server != nil {
				l.server.mu.Lock()
				l.server.badRequests++
				l.server.mu.Unlock()
			}
		}
	}
}

func writeOverloaded(conn net.Conn) {
	body := "Service temporarily overloaded"
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "HTTP/1.1 503 Service temporarily overloaded\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

// releaseConn 在连接关闭时归还许可（只归还一次）
type releaseConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *releaseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// countingWriter 让 http.Server 的 ErrorLog 同时也计入 bad_requests 统计
type countingWriter struct {
	server *apiServer
}

func (w countingWriter) Write(p []byte) (int, error) {
	w.server.mu.Lock()
	w.server.badRequests++
	w.server.mu.Unlock()
	return os.Stderr.Write(p)
}
