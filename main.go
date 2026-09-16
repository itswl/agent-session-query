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
// 只用标准库，无外部依赖；启动方式：
//
//	agent-session-query [--port 8080] [--mode auto|all|hermes|openclaw|pi|claude|codex|gemini]
package main

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

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("agent-session-query", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	host := fs.String("host", "0.0.0.0", "绑定主机 (默认: 0.0.0.0)")
	port := fs.Int("port", 8080, "端口 (默认: 8080)")
	mode := fs.String("mode", "auto", "模式: auto 自动检测 / all 全部启用 / "+joinModes())
	hookToken := fs.String("hook_token", "", "Bearer token（设置后所有 /sessions 端点都要带）")
	maxConnections := fs.Int("max-connections", 50, "最大并发连接数 (默认: 50)")
	cacheTTL := fs.Float64("cache-ttl", 2.0, "会话列表缓存秒数 (默认: 2；0 = 每次重新扫描)")
	timeout := fs.Int("timeout", 30, "连接超时秒数 (默认: 30)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if !validMode(*mode) {
		fmt.Fprintf(os.Stderr, "无效的 --mode: %q（可选: auto, all, %s）\n", *mode, joinModes())
		return 2
	}

	sources, err := buildSources(*mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] %v\n", err)
		return 1
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
	fmt.Printf("最大并发连接数: %d, 连接超时: %ds, 列表缓存: %gs\n", *maxConnections, *timeout, *cacheTTL)

	api := newSessionQueryAPI(sources, *cacheTTL)
	server := newAPIServer(*mode, sources, api, *hookToken, *maxConnections)

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
	listener := newLimitListener(inner, *maxConnections)
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
