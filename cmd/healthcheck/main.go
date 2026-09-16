// healthcheck 是一个探活小程序，只给容器 HEALTHCHECK 用。
//
// scratch 运行镜像里没有 curl、没有 shell，所以自带一个静态二进制：
//
//	HEALTHCHECK CMD ["/healthcheck"]
//
// 它刻意不用 net/http：那个包会把整个 TLS/HTTP 栈链进来，二进制从 1.5 MB 涨到 5 MB，
// 而这里只需要「连上、发一行 GET、看状态码是不是 200」。
//
// 默认探 http://127.0.0.1:8080/health，可用环境变量覆盖：
//
//	HEALTHCHECK_URL（默认 http://127.0.0.1:8080/health）
//	HEALTHCHECK_TOKEN（设置了就带 Authorization: Bearer）
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

// splitURL 只认 http://host[:port]/path 这一种形态
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
