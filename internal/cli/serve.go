package cli

import (
	"context"
	"fmt"
	goio "io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/mcp"
	"github.com/itswl/agent-session-query/internal/server"
	"github.com/itswl/agent-session-query/web"
)

// DefaultPort is where `asq serve` listens unless told otherwise.
const DefaultPort = 7717

func cmdServe(ctx context.Context, args []string, io *streams) error {
	c := newCommand("serve", io.err)
	host := c.fs.String("host", envOr("ASQ_HOST", "127.0.0.1"), "the address to listen on (ASQ_HOST); anything but loopback needs --token")
	port := c.fs.Int("port", envInt("ASQ_PORT", DefaultPort), "the port to listen on (ASQ_PORT)")
	token := c.fs.String("token", "", "what every caller must hold (ASQ_TOKEN; prefer the variable: a flag shows in the process list)")
	var origins multi
	c.fs.Var(&origins, "cors-origin", "another origin whose pages may call the API (repeatable; ASQ_CORS_ORIGIN, comma-separated)")
	proxyHeader := c.fs.String("proxy-header", os.Getenv("ASQ_PROXY_HEADER"), "the header in which a proxy in front names the caller, so that wrong tokens are counted per caller (ASQ_PROXY_HEADER): CF-Connecting-IP, X-Real-IP, X-Forwarded-For")
	open := c.fs.Bool("open", false, "open the page in the browser once it is up")
	pageDir := c.fs.String("page-dir", "", "serve the page from this directory, read afresh on every request (for working on it)")
	if _, err := c.parse(args); err != nil {
		return err
	}
	if *token == "" {
		*token = os.Getenv("ASQ_TOKEN")
	}
	for _, o := range strings.Split(os.Getenv("ASQ_CORS_ORIGIN"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	cfg, err := c.config()
	if err != nil {
		return usageError{err}
	}
	a, err := app.Open(cfg)
	if err != nil {
		return err
	}
	defer a.Close()

	opts := server.Options{Host: *host, Port: *port, Token: *token, Origins: origins, Page: web.Files, Log: io.err,
		State: app.StateDir(), ProxyHeader: *proxyHeader}
	if *pageDir != "" {
		opts.Page, opts.Live = os.DirFS(*pageDir), true
	}
	srv, err := server.New(a, opts)
	if err != nil {
		return usageError{err}
	}
	return srv.Run(ctx, func(addr string) {
		url := "http://" + addr
		guard := "this machine only"
		if *token != "" {
			guard = "token required"
			if *proxyHeader != "" {
				guard += ", callers named by " + *proxyHeader
			}
		}
		fmt.Fprintf(io.err, "asq %s · %s · %s\n", app.Version, url, guard)
		fmt.Fprintf(io.err, "  page %s/   api %s/api/   mcp %s/mcp\n", url, url, url)
		if *open {
			openBrowser(url)
		}
	})
}

func cmdMCP(ctx context.Context, args []string, io *streams) error {
	c := newCommand("mcp", io.err)
	if _, err := c.parse(args); err != nil {
		return err
	}
	cfg, err := c.config()
	if err != nil {
		return usageError{err}
	}
	a, err := app.Open(cfg)
	if err != nil {
		return err
	}
	defer a.Close()
	// stdout is the protocol's and nothing else's: everything said goes to stderr. A
	// person who runs it at a terminal sees it wait for input that never makes sense to
	// type, so it says what it is for
	if fromTerminal(io.in) {
		fmt.Fprintln(io.err, "asq mcp speaks JSON-RPC on stdin and stdout, for an MCP client to start: claude mcp add asq -- asq mcp (see 'asq mcp -h'). Ctrl+C or Ctrl+D ends it.")
	}
	err = mcp.New(a, io.err).Serve(ctx, io.in, io.out)
	if ctx.Err() != nil {
		return nil // stopped by a signal: that is how a client ends it
	}
	return err
}

// fromTerminal reports whether r is a terminal a person types into.
func fromTerminal(r goio.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// cmdPing asks a running server whether it is serving: what a container's health check
// runs, in an image that has no shell and no curl to do it with.
func cmdPing(ctx context.Context, args []string, io *streams) error {
	c := newCommand("ping", io.err)
	host := c.fs.String("host", "127.0.0.1", "where the server listens")
	port := c.fs.Int("port", envInt("ASQ_PORT", DefaultPort), "the port it listens on (ASQ_PORT)")
	if _, err := c.parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := "http://" + net.JoinHostPort(*host, strconv.Itoa(*port)) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("no answer from %s: %w", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	fmt.Fprintln(io.out, "ok")
	return nil
}
