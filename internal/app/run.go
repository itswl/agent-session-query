// agent-session-query: an HTTP API for querying local agent sessions.
//
// It exposes the session records written by the agent CLIs on this machine over HTTP,
// read-only: listing, a single session, its messages, its final result. Sources (whichever
// exist are queried, and they can be merged in one query):
//
//	Hermes      ~/.hermes/sessions/sessions.json
//	OpenClaw    ~/.openclaw/agents/default/sessions/sessions.json
//	Pi          ~/.pi/agent/sessions/<project>/*.jsonl
//	Claude Code ~/.claude/projects/<project>/*.jsonl
//	Codex       ~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl
//	Gemini CLI  ~/.gemini/tmp/<project>/chats/session-*.jsonl
//	Grok CLI    ~/.grok/sessions/<url-encoded-cwd>/<session-id>/updates.jsonl
//
// The only external dependency is a pure-Go SQLite driver (for reading Hermes's
// state.db). To start it:
//
//	agent-session-query [--port 8080] [--mode auto|all|hermes|openclaw|pi|claude|codex|gemini|opencode|grok]
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

// buildVersion is injected by release.yml with -ldflags -X at release time; a local
// build simply reads dev.
var buildVersion = "dev"

// Run parses the flags, starts the server, and returns the process exit code (the entry
// point lives in cmd/agent-session-query).
func Run(args []string) int {
	enableUTF8Console() // the legacy Windows console is not UTF-8 by default

	fs := flag.NewFlagSet("agent-session-query", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	// Loopback only by default. This service can read complete session content (tool output
	// included) and is unauthenticated unless a token is set, so binding 0.0.0.0 hands every
	// agent record on the machine to the whole local network. To share it, pass
	// --host 0.0.0.0 explicitly and put authentication in front of it (which is exactly what
	// the Dockerfile CMD does).
	host := fs.String("host", "127.0.0.1", "bind address (default: 127.0.0.1, local only; use 0.0.0.0 to expose it)")
	port := fs.Int("port", 8080, "listen port (default: 8080)")
	mode := fs.String("mode", "auto", "mode: auto to detect, all to enable everything, or one of "+joinModes())
	var paths pathFlag
	fs.Var(&paths, "path", "relocate or duplicate a file-backed source, mode[:label]=dir; repeatable, e.g. --path claude:box2=/mnt/box2/.claude/projects (supports pi, claude, codex, gemini, grok)")
	hookToken := fs.String("hook_token", "", "Bearer token; once set, every /sessions endpoint requires it")
	maxConnections := fs.Int("max-connections", 50, "maximum concurrent connections (default: 50)")
	cacheTTL := fs.Float64("cache-ttl", 2.0, "seconds to cache the session list (default: 2; 0 rescans every time)")
	timeout := fs.Int("timeout", 30, "connection timeout in seconds (default: 30)")
	maxLimit := fs.Int("max-limit", defaultMaxLimit, "upper bound for ?limit= (default: 1000)")
	acceptQueue := fs.Int("accept-queue", 0, "queue slots when at capacity (default: 0, derived from max-connections)")
	corsOrigin := fs.String("cors-origin", "", "allowed CORS origin (off by default; * or a specific origin)")
	mcp := fs.Bool("mcp", false, "run as an MCP server on stdio for agents to call; does not listen on a port")
	daemon := fs.Bool("d", false, "run in the background, detached from the terminal, logging to a file")
	logPath := fs.String("log-file", "", "log path used with -d (default <tmp>/agent-session-query-<port>.log)")
	showVersion := fs.Bool("version", false, "print the version and exit")

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
		fmt.Fprintf(os.Stderr, "invalid --mode: %q (choose from: auto, all, %s)\n", *mode, joinModes())
		return 2
	}
	if *daemon && *mcp {
		fmt.Fprintln(os.Stderr, "[FATAL] --mcp speaks a protocol over stdio; detaching from the terminal makes no sense, so it cannot be combined with -d")
		return 2
	}
	// Re-exec ourselves in the background; the parent prints the PID and exits once the
	// probe succeeds
	if *daemon {
		return startDaemon(daemonOptions{args: args, logPath: *logPath, host: *host, port: *port})
	}

	// Fall back to the environment when --hook_token is absent: command-line arguments show
	// up in ps, environment variables do not
	if *hookToken == "" {
		*hookToken = os.Getenv("HOOK_TOKEN")
	}

	sources, err := buildSources(*mode, paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] %v\n", err)
		return 1
	}

	api := newSessionQueryAPI(sources, *cacheTTL)

	// MCP mode: stdout belongs to JSON-RPC alone, so startup output must go to stderr
	if *mcp {
		mcpStartupBanner(*mode, sources)
		return runMCP(&mcpServer{api: api, sources: sources, maxLimit: *maxLimit}, os.Stdin, os.Stdout)
	}

	fmt.Printf("Mode: %s\n", *mode)
	for _, source := range sources {
		fmt.Printf("Source [%s]: %s\n", source.Mode(), source.Location())
	}
	if *hookToken != "" {
		fmt.Println("Authentication enabled (Bearer hook_token is set; not echoed)")
	} else {
		fmt.Println("[WARN] no hook_token set; the API is open to anyone who can reach it")
	}
	fmt.Printf("Max connections: %d (queue %d, waiting up to %s), timeout: %ds, list cache: %gs, limit cap: %d\n",
		*maxConnections, acceptQueueDepth(*maxConnections, *acceptQueue), acceptQueueWait,
		*timeout, *cacheTTL, *maxLimit)
	if *corsOrigin != "" {
		fmt.Printf("CORS enabled: Access-Control-Allow-Origin: %s\n", *corsOrigin)
		if *corsOrigin == "*" && *hookToken == "" {
			fmt.Println("[WARN] --cors-origin * with no token: any web page can read this machine's sessions")
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
		fmt.Fprintf(os.Stderr, "[FATAL] failed to listen: %v\n", err)
		return 1
	}
	listener := newLimitListener(inner, *maxConnections, *acceptQueue)
	listener.server = server

	fmt.Println("\nSession API started")
	fmt.Printf("Listening on: http://%s:%d\n", *host, *port)
	fmt.Println("\nEndpoints:")
	fmt.Println("  GET /sessions                        - list every session")
	fmt.Println("  GET /sessions/<pattern>              - one session")
	fmt.Println("  GET /sessions/<pattern>/messages     - its messages")
	fmt.Println("  GET /sessions/<pattern>/final        - its final result")
	fmt.Println("  GET /health                          - health check (with stats)")
	fmt.Println("\nExample:")
	fmt.Printf("  curl -H 'Authorization: Bearer xxx' http://localhost:%d/sessions\n", *port)
	fmt.Println("\nPress Ctrl+C to stop")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "[FATAL] server exited unexpectedly: %v\n", err)
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
