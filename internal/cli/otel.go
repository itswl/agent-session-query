package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	goio "io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/otel"
	"github.com/itswl/agent-session-query/internal/server"
)

func cmdOTel(ctx context.Context, args []string, io *streams) error {
	c := newCommand("otel", io.err).selection(0)
	endpoint := c.fs.String("endpoint", "", "the OTLP/HTTP collector (OTEL_EXPORTER_OTLP_ENDPOINT; or LANGFUSE_PUBLIC_KEY + LANGFUSE_SECRET_KEY)")
	var headers multi
	c.fs.Var(&headers, "header", "a header to send, Name=value (repeatable; OTEL_EXPORTER_OTLP_HEADERS)")
	content := c.fs.String("content", "none", "how much of the conversation is sent: none (timings, models, tokens, tool names), asks (and what was asked and answered), all (and every tool's input and output)")
	settle := c.fs.Duration("settle", 2*time.Minute, "leave a turn until it has been quiet this long")
	every := c.fs.Duration("every", 0, "keep running, exporting this often (0: once)")
	resend := c.fs.Bool("resend", false, "send every selected turn again, whatever was sent before")
	dry := c.fs.Bool("dry-run", false, "print the OTLP requests instead of sending them: to a pipe or a file, or with --json; at a terminal, what they hold")
	if _, err := c.parse(args); err != nil {
		return err
	}
	level, err := otel.ParseLevel(*content)
	if err != nil {
		return usageError{err}
	}
	target, head, err := collector(*endpoint, headers)
	if err != nil {
		return usageError{err}
	}
	if target == "" && !*dry {
		return usagef("where to?  --endpoint https://collector:4318, or set OTEL_EXPORTER_OTLP_ENDPOINT, or LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY  (--dry-run prints instead)")
	}
	if u, err := url.Parse(target); err == nil && u.Scheme == "http" && !server.IsLoopback(u.Hostname()) && level > otel.LevelNone && !*dry {
		fmt.Fprintf(io.err, "asq otel: %s is plain HTTP: the conversation travels unencrypted\n", u.Host)
	}

	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	// The sessions are those every flag that selects them picks, a directory with the
	// rest; the window is not one of them, as it picks turns (below), not sessions
	q := c.q
	q.Since, q.Until = "", ""
	f, err := a.Filter(q)
	if err != nil {
		return usageError{err}
	}

	once := func() error {
		// The window is read again on every pass: "7d" means the seven days before now
		w, err := a.Window(c.q, "7d")
		if err != nil {
			return usageError{err}
		}
		opts := otel.Options{Endpoint: target, Headers: head, Level: level, Filter: f, Window: w, Settle: *settle, Resend: *resend}
		// The requests are a line of JSON each, hundreds of kilobytes long: for a pipe, a
		// file, or a terminal that asked for JSON. A terminal that did not is told what
		// they hold, and how to read them
		shown := !toTerminal(io.out) || c.json
		if *dry {
			opts.DryRun = io.out
			if !shown {
				opts.DryRun = goio.Discard
			}
		}
		res, err := otel.Export(ctx, a.Index, opts)
		defer c.lookedIn(io.err, err == nil && res.Turns+res.Current == 0)
		if *dry {
			fmt.Fprintf(io.err, "would send %s of %s as %s in %s\n", plural(res.Turns, "turn"), plural(res.Sessions, "transcript"), plural(res.Spans, "span"), plural(res.Requests, "request"))
			if !shown && res.Requests > 0 {
				fmt.Fprintln(io.err, "the requests are a line of JSON each: pipe them to jq or to a file to read them (asq otel --dry-run … | jq), or add --json to print them here")
			}
			return err
		}
		line := fmt.Sprintf("sent %s of %s as %s", plural(res.Turns, "turn"), plural(res.Sessions, "transcript"), plural(res.Spans, "span"))
		if res.Current > 0 {
			line += fmt.Sprintf(" · %d already there", res.Current)
		}
		if err != nil {
			fmt.Fprintf(io.err, "%s, then stopped: %v\n", line, err)
			return errFailed
		}
		fmt.Fprintln(io.err, line)
		return nil
	}

	if *every <= 0 {
		return once()
	}
	for {
		if err := once(); err != nil && err != errFailed {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*every):
		}
		if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil && ctx.Err() == nil {
			fmt.Fprintf(io.err, "asq otel: the index could not be brought fully up to date: %v\n", err)
		}
		*resend = false // only the first pass sends again what was sent
	}
}

// toTerminal reports whether output goes to a terminal; a variable for the tests.
var toTerminal = isTerminal

// collector works out where spans go and what headers go with them: the flags first,
// then the environment OpenTelemetry's own tools read, then Langfuse's.
func collector(endpoint string, flags []string) (string, map[string]string, error) {
	headers := map[string]string{}
	// OTEL_EXPORTER_OTLP_HEADERS: key=value pairs, comma-separated, values URL-encoded
	for _, pair := range strings.Split(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"), ",") {
		if name, value, ok := strings.Cut(strings.TrimSpace(pair), "="); ok && name != "" {
			if decoded, err := url.QueryUnescape(value); err == nil {
				value = decoded
			}
			headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	for _, h := range flags {
		name, value, ok := strings.Cut(h, "=")
		if i := strings.Index(h, ":"); i > 0 && (!ok || i < len(name)) {
			name, value, ok = h[:i], h[i+1:], true
		}
		if !ok || strings.TrimSpace(name) == "" {
			return "", nil, fmt.Errorf("--header wants Name=value, got %q", h)
		}
		headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	if endpoint == "" {
		endpoint = envOr("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	if endpoint == "" {
		public, secret := os.Getenv("LANGFUSE_PUBLIC_KEY"), os.Getenv("LANGFUSE_SECRET_KEY")
		if public != "" && secret != "" {
			host := envOr("LANGFUSE_HOST", envOr("LANGFUSE_BASE_URL", "https://cloud.langfuse.com"))
			endpoint = strings.TrimRight(host, "/") + "/api/public/otel"
			if _, set := headers["Authorization"]; !set {
				headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(public+":"+secret))
			}
		}
	}
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", nil, fmt.Errorf("the collector is an http(s) address, not %q", endpoint)
		}
	}
	return endpoint, headers, nil
}
