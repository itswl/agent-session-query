package app

import (
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

// defaultLimit / defaultMaxLimit：?limit= 的缺省值与硬上限
const (
	defaultLimit    = 50
	defaultMaxLimit = 1000
)

// apiServer：路由 + 认证 + 连接限制 + 统计
type apiServer struct {
	mode       string
	sources    []SessionSource
	api        *SessionQueryAPI
	token      string
	corsOrigin string
	maxLimit   int

	maxConnections int

	mu          sync.Mutex
	totalConns  int64
	activeConns int64
	badRequests int64
}

type serverOptions struct {
	mode           string
	sources        []SessionSource
	api            *SessionQueryAPI
	token          string
	corsOrigin     string
	maxConnections int
	maxLimit       int
}

func newAPIServer(opts serverOptions) *apiServer {
	if opts.maxLimit < 1 {
		opts.maxLimit = defaultMaxLimit
	}
	return &apiServer{
		mode:           opts.mode,
		sources:        opts.sources,
		api:            opts.api,
		token:          opts.token,
		corsOrigin:     opts.corsOrigin,
		maxLimit:       opts.maxLimit,
		maxConnections: opts.maxConnections,
	}
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

func (s *apiServer) countBadRequest() {
	s.mu.Lock()
	s.badRequests++
	s.mu.Unlock()
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

// applyCORS 只有显式配了 --cors-origin 才放 CORS 头。
//
// 原先是无条件 Access-Control-Allow-Origin: *。配上「不设 token 就免认证」这个默认值，
// 等于用户访问的任何网页都能 fetch 本机的 /sessions 把会话内容（源码、工具输出）读走——
// 裸 GET 是 simple request，不触发预检，浏览器看见 * 就直接把响应交给对方脚本。
// 自带的 /ui 是同源的，本来就不需要 CORS。
func (s *apiServer) applyCORS(w http.ResponseWriter) {
	if s.corsOrigin == "" {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
	if s.corsOrigin != "*" {
		w.Header().Add("Vary", "Origin")
	}
}

// ---------------------------------------------------------------------------
// 响应
// ---------------------------------------------------------------------------

// writeJSON 流式输出 JSON（不转义 HTML、不转义非 ASCII）。
//
// 不在内存里先缓冲整个响应：一个大会话的 messages 能有十几 MB，
// 缓冲一份等于把峰值内存翻倍，并发几个就很可观。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// 响应头已经发出去了，改不了状态码，只能记一笔
		fmt.Fprintf(os.Stderr, "[ERROR] 序列化响应失败: %v\n", err)
	}
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// statusRecorder 记下实际写出的状态码：内嵌页面走 http.FileServer，
// 404 是它自己写的，不记的话访问日志里全是 200。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
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

	s.applyCORS(w)

	switch r.Method {
	case http.MethodOptions:
		if s.corsOrigin != "" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
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
	// 用 EscapedPath 分段（%2F 不当作分隔符）
	path := r.URL.EscapedPath()

	// 健康检查 - 不要求认证，便于监控探活
	if path == "/health" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"mode":    s.mode,
			"sources": sourceModes(s.sources),
			// 页面据此决定要不要弹令牌框：服务端没设 token 时不该还逼人随便填一个
			"authRequired": s.token != "",
			"stats":        s.stats(),
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
		rec := &statusRecorder{ResponseWriter: w}
		serveUIAssets(rec, r, path)
		if rec.status == 0 {
			return http.StatusOK
		}
		return rec.status
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
		sessions, etag := s.api.listSessions()
		// 页面每 10 秒轮询一次，列表多半没变：带上 ETag 就能在 304 结束
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return http.StatusNotModified
		}
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
			query := s.parseMessageQuery(r)
			messages, ok := s.api.getMessages(pattern, query)
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "Session not found"})
				return http.StatusNotFound
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"messages": messages,
				"total":    len(messages),
				"order":    orderName(query.fromEnd),
			})
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

// etagMatches 比对 If-None-Match（可能是逗号分隔的一串，也可能是 *）
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}

func unescapePattern(segment string) string {
	if pattern, err := url.PathUnescape(segment); err == nil {
		return pattern
	}
	return segment
}

// parseLimit 取 ?limit=，非法或缺省都是 defaultLimit，并夹在 [0, maxLimit] 内。
//
// 不夹上限的话，?limit=99999999 会把一个 99 MB 的会话在内存里摊成 14 MB 的响应
// （实测 RSS 11 MB → 145 MB），几个并发请求就能把进程打爆。
func (s *apiServer) parseLimit(r *http.Request) int {
	limit := defaultLimit
	if values, ok := r.URL.Query()["limit"]; ok && len(values) == 1 {
		if parsed, err := strconv.Atoi(strings.TrimSpace(values[0])); err == nil {
			limit = parsed
		}
	}
	if limit < 0 {
		return 0
	}
	if limit > s.maxLimit {
		return s.maxLimit
	}
	return limit
}

// parseMessageQuery 取 ?limit= 与 ?order=（desc 表示要最新的 N 条）
func (s *apiServer) parseMessageQuery(r *http.Request) messageQuery {
	order := strings.TrimSpace(r.URL.Query().Get("order"))
	return messageQuery{
		limit:   s.parseLimit(r),
		fromEnd: strings.EqualFold(order, "desc"),
	}
}

func orderName(fromEnd bool) string {
	if fromEnd {
		return "desc"
	}
	return "asc"
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
// 连接限制：超过上限的连接返回 503
// ---------------------------------------------------------------------------

// acceptQueueWait 满载时一个连接最多排队多久，等不到就 503
const acceptQueueWait = 10 * time.Second

// 排队位的自动取值：maxConnections 的 autoAcceptQueueFactor 倍，不低于 minAcceptQueue。
// 队列只是给突发流量一点缓冲，取太小的话（比如 --max-connections 2）一个 20 并发的
// 小突发就会被打掉大半，而原先靠阻塞 accept 时这些请求是能排上队的。
const (
	minAcceptQueue        = 32
	autoAcceptQueueFactor = 2
)

// acceptQueueDepth 算实际的排队位数：queue <= 0 表示按 maxConnections 自动取。
func acceptQueueDepth(maxConnections, queue int) int {
	if queue > 0 {
		return queue
	}
	auto := maxConnections * autoAcceptQueueFactor
	if auto < minAcceptQueue {
		return minAcceptQueue
	}
	return auto
}

// limitListener 控制同时在处理的连接数。
//
// accept 循环单独跑一个 goroutine，拿到许可的连接经 ready 交给 http.Server：
// 原先是在 Accept() 里原地等许可，满载时整个 accept 循环停摆——第 51 个连接等 10 秒，
// 第 52 个得等它走完才开始排，队伍越长越慢。现在每个连接各排各的，互不挡道。
//
// waiting 限制同时排队的连接数：排队的位置也满了就立刻 503，免得大量连接把 goroutine
// 和文件描述符堆起来——这层背压原先是靠阻塞 accept 实现的，现在要显式写出来。
type limitListener struct {
	net.Listener
	sem     chan struct{} // 并发处理许可
	waiting chan struct{} // 排队位
	ready   chan net.Conn
	failed  chan error
	timeout time.Duration
	server  *apiServer

	once sync.Once
	err  error // 只在 Accept 里读写（http.Server 单 goroutine 调用）
}

// newLimitListener：maxConnections 是同时处理的上限，queue 是排队位数
// （<= 0 按 maxConnections 自动取，见 acceptQueueDepth）。
func newLimitListener(inner net.Listener, maxConnections, queue int) *limitListener {
	if maxConnections < 1 {
		maxConnections = 1
	}
	return &limitListener{
		Listener: inner,
		sem:      make(chan struct{}, maxConnections),
		waiting:  make(chan struct{}, acceptQueueDepth(maxConnections, queue)),
		ready:    make(chan net.Conn),
		failed:   make(chan error, 1),
		timeout:  acceptQueueWait,
	}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.once.Do(func() { go l.acceptLoop() })
	if l.err != nil {
		return nil, l.err
	}
	select {
	case conn := <-l.ready:
		return conn, nil
	case err := <-l.failed:
		l.err = err
		return nil, err
	}
}

func (l *limitListener) acceptLoop() {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			l.failed <- err
			return
		}
		select {
		case l.sem <- struct{}{}: // 有空位，直接放行
			l.ready <- l.wrap(conn)
		case l.waiting <- struct{}{}: // 满了但还能排队
			go func() {
				defer func() { <-l.waiting }()
				l.admit(conn)
			}()
		default: // 连排队的位置都没了
			l.reject(conn)
		}
	}
}

// admit 排队等一个许可，等到了就把连接交出去，超时就 503
func (l *limitListener) admit(conn net.Conn) {
	timer := time.NewTimer(l.timeout)
	defer timer.Stop()
	select {
	case l.sem <- struct{}{}:
		l.ready <- l.wrap(conn)
	case <-timer.C:
		l.reject(conn)
	}
}

func (l *limitListener) wrap(conn net.Conn) net.Conn {
	return &releaseConn{Conn: conn, release: func() { <-l.sem }}
}

func (l *limitListener) reject(conn net.Conn) {
	writeOverloaded(conn)
	_ = conn.Close()
	if l.server != nil {
		l.server.countBadRequest()
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
	w.server.countBadRequest()
	return os.Stderr.Write(p)
}
