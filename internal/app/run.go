// agent-session-query：本地 Agent 会话查询 HTTP API
//
// 只读地把本机上各种 Agent / CLI 的会话记录暴露成 HTTP：列表、单个会话、
// 消息、最终结果。数据源（存在哪几种就查哪几种，可同时合并查询）：
//
//	Hermes      ~/.hermes/sessions/sessions.json
//	OpenClaw    ~/.openclaw/agents/default/sessions/sessions.json
//	Pi          ~/.pi/agent/sessions/<项目>/*.jsonl
//	Claude Code ~/.claude/projects/<项目>/*.jsonl
//	Codex       ~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl
//	Gemini CLI  ~/.gemini/tmp/<项目>/chats/session-*.jsonl
//
// 唯一的外部依赖是纯 Go 的 SQLite 驱动（读 Hermes 的 state.db）；启动方式：
//
//	agent-session-query [--port 8080] [--mode auto|all|hermes|openclaw|pi|claude|codex|gemini]
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// buildVersion 发版时由 release.yml 用 -ldflags -X 注入；本地构建就是 dev。
var buildVersion = "dev"

// Run 解析参数并启动服务，返回进程退出码（入口在 cmd/agent-session-query）。
func Run(args []string) int {
	enableUTF8Console() // Windows 的传统控制台默认不是 UTF-8，中文会乱码

	fs := flag.NewFlagSet("agent-session-query", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	// 默认只监听回环：这个服务能读到完整会话内容（含工具输出），
	// 默认又是免认证的，绑 0.0.0.0 等于把本机所有 Agent 记录摊给整个局域网。
	// 要给别人用就显式 --host 0.0.0.0，并在反向代理上加认证（Dockerfile 的 CMD 就是这么传的）。
	host := fs.String("host", "127.0.0.1", "绑定主机 (默认: 127.0.0.1，仅本机；对外暴露用 0.0.0.0)")
	port := fs.Int("port", 8080, "端口 (默认: 8080)")
	mode := fs.String("mode", "auto", "模式: auto 自动检测 / all 全部启用 / "+joinModes())
	hookToken := fs.String("hook_token", "", "Bearer token（设置后所有 /sessions 端点都要带）")
	maxConnections := fs.Int("max-connections", 50, "最大并发连接数 (默认: 50)")
	cacheTTL := fs.Float64("cache-ttl", 2.0, "会话列表缓存秒数 (默认: 2；0 = 每次重新扫描)")
	timeout := fs.Int("timeout", 30, "连接超时秒数 (默认: 30)")
	maxLimit := fs.Int("max-limit", defaultMaxLimit, "?limit= 的上限 (默认: 1000)")
	acceptQueue := fs.Int("accept-queue", 0, "满载时的排队位数 (默认: 0 = 按 max-connections 自动取)")
	corsOrigin := fs.String("cors-origin", "", "允许的跨域来源（默认关闭；填 * 或具体 origin）")
	mcp := fs.Bool("mcp", false, "以 MCP server 跑在 stdio 上（供 Agent 调用），不监听端口")
	daemon := fs.Bool("d", false, "后台运行：脱离终端，输出写到日志文件")
	logPath := fs.String("log-file", "", "-d 时的日志路径（默认 <临时目录>/agent-session-query-<端口>.log）")
	showVersion := fs.Bool("version", false, "打印版本后退出")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Println(buildVersion)
		return 0
	}
	if !validMode(*mode) {
		fmt.Fprintf(os.Stderr, "无效的 --mode: %q（可选: auto, all, %s）\n", *mode, joinModes())
		return 2
	}
	if *daemon && *mcp {
		fmt.Fprintln(os.Stderr, "[FATAL] --mcp 是 stdio 上的协议，脱离终端就没意义了，不能和 -d 一起用")
		return 2
	}
	// 重新 exec 自己跑到后台；父进程探活成功后打印 PID 退出
	if *daemon {
		return startDaemon(daemonOptions{args: args, logPath: *logPath, host: *host, port: *port})
	}

	// 没给 --hook_token 就看环境变量：命令行参数会出现在 ps 里，环境变量不会
	if *hookToken == "" {
		*hookToken = os.Getenv("HOOK_TOKEN")
	}

	sources, err := buildSources(*mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] %v\n", err)
		return 1
	}

	api := newSessionQueryAPI(sources, *cacheTTL)

	// MCP 模式：stdout 归 JSON-RPC 独占，启动信息只能写 stderr
	if *mcp {
		mcpStartupBanner(*mode, sources)
		return runMCP(&mcpServer{api: api, sources: sources, maxLimit: *maxLimit}, os.Stdin, os.Stdout)
	}

	fmt.Printf("运行模式: %s\n", *mode)
	for _, source := range sources {
		fmt.Printf("数据源 [%s]: %s\n", source.Mode(), source.Location())
	}
	if *hookToken != "" {
		fmt.Println("已启用认证（Bearer hook_token 已设置，不回显）")
	} else {
		fmt.Println("警告: 未设置认证hook_token，API 公开访问")
	}
	fmt.Printf("最大并发连接数: %d (排队位 %d，最多等 %s), 连接超时: %ds, 列表缓存: %gs, limit 上限: %d\n",
		*maxConnections, acceptQueueDepth(*maxConnections, *acceptQueue), acceptQueueWait,
		*timeout, *cacheTTL, *maxLimit)
	if *corsOrigin != "" {
		fmt.Printf("已开启跨域: Access-Control-Allow-Origin: %s\n", *corsOrigin)
		if *corsOrigin == "*" && *hookToken == "" {
			fmt.Println("警告: --cors-origin * 且未设 token —— 任何网页都能读走本机会话内容")
		}
	}

	server := newAPIServer(serverOptions{
		mode:           *mode,
		sources:        sources,
		api:            api,
		token:          *hookToken,
		corsOrigin:     *corsOrigin,
		maxConnections: *maxConnections,
		maxLimit:       *maxLimit,
	})

	httpServer := &http.Server{
		Handler:           server,
		ReadTimeout:       time.Duration(*timeout) * time.Second,
		ReadHeaderTimeout: time.Duration(*timeout) * time.Second,
		WriteTimeout:      time.Duration(*timeout) * time.Second,
		IdleTimeout:       time.Duration(*timeout) * time.Second,
		ErrorLog:          log.New(countingWriter{server: server}, "", 0),
		ConnState:         server.connState,
	}

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	inner, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] 监听失败: %v\n", err)
		return 1
	}
	listener := newLimitListener(inner, *maxConnections, *acceptQueue)
	listener.server = server

	fmt.Println("\nSession API 启动成功")
	fmt.Printf("监听地址: http://%s:%d\n", *host, *port)
	fmt.Println("\nAPI 端点:")
	fmt.Println("  GET /sessions                        - 列出所有 session")
	fmt.Println("  GET /sessions/<pattern>              - 查询单个 session")
	fmt.Println("  GET /sessions/<pattern>/messages     - 获取消息")
	fmt.Println("  GET /sessions/<pattern>/final        - 获取最终结果")
	fmt.Println("  GET /health                          - 健康检查 (含服务统计)")
	fmt.Println("\n示例:")
	fmt.Printf("  curl -H 'Authorization: Bearer xxx' http://localhost:%d/sessions\n", *port)
	fmt.Println("\n按 Ctrl+C 停止服务")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n正在停止服务...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "[FATAL] 服务异常退出: %v\n", err)
		return 1
	}
	return 0
}

func validMode(mode string) bool {
	if mode == "auto" || mode == "all" {
		return true
	}
	for _, m := range knownModes {
		if mode == m {
			return true
		}
	}
	return false
}
