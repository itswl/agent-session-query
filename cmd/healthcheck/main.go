// healthcheck is a tiny liveness probe, used only by the container HEALTHCHECK.
//
// The scratch runtime image has no curl and no shell, so it ships its own static binary:
//
//	HEALTHCHECK CMD ["/healthcheck"]
//
// It deliberately avoids net/http: that package links the whole TLS/HTTP stack in and
// grows the binary from 1.5 MB to 5 MB, when all this needs is "connect, send one GET
// line, check whether the status is 200".
//
// It probes http://127.0.0.1:8080/health by default, overridable by environment:
//
//	HEALTHCHECK_URL   (default http://127.0.0.1:8080/health)
//	HEALTHCHECK_TOKEN (when set, sent as Authorization: Bearer)
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	rawURL := os.Getenv("HEALTHCHECK_URL")
	if rawURL == "" {
		rawURL = "http://127.0.0.1:8080/health"
	}

	addr, path, err := splitURL(rawURL)
	if err != nil {
		return err
	}

	timeout := 5 * time.Second
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	request := "GET " + path + " HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n"
	if token := os.Getenv("HEALTHCHECK_TOKEN"); token != "" {
		request += "Authorization: Bearer " + token + "\r\n"
	}
	request += "\r\n"

	if _, err := conn.Write([]byte(request)); err != nil {
		return err
	}

	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	fields := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(fields) < 2 || fields[1] != "200" {
		return fmt.Errorf("unexpected status line: %q", strings.TrimSpace(statusLine))
	}
	return nil
}

// splitURL accepts only the http://host[:port]/path shape
func splitURL(raw string) (addr string, path string, err error) {
	rest, ok := strings.CutPrefix(raw, "http://")
	if !ok {
		return "", "", fmt.Errorf("only http:// is supported, got %q", raw)
	}
	hostPort, p, found := strings.Cut(rest, "/")
	if !found {
		p = ""
	}
	if hostPort == "" {
		return "", "", fmt.Errorf("missing host in %q", raw)
	}
	if !strings.Contains(hostPort, ":") {
		hostPort += ":80"
	}
	if p == "" {
		p = "/"
	}
	return hostPort, "/" + p, nil
}
