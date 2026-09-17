package app

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 后台运行（-d）。
//
// Go 里不能用 fork：运行时是多线程的，而 fork 只复制调用线程，子进程拿到的是一个
// 半死的运行时。所以走「重新 exec 自己」这条路——把 -d 从参数里摘掉（免得子进程
// 再 daemonize 一次），输出重定向到日志文件，进程脱离当前终端。
const (
	daemonProbeTimeout  = 5 * time.Second
	daemonProbeInterval = 50 * time.Millisecond
	daemonSettleDelay   = 300 * time.Millisecond
	daemonLogTailBytes  = 2048
)

type daemonOptions struct {
	args    []string // 原始命令行参数
	logPath string   // 为空则按端口推导
	host    string
	port    int
}

// startDaemon 把自己拉起到后台，返回父进程的退出码。
func startDaemon(opts daemonOptions) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] 找不到自身可执行文件: %v\n", err)
		return 1
	}

	logPath := opts.logPath
	if logPath == "" {
		logPath = defaultLogPath(opts.port)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] 打不开日志文件 %s: %v\n", logPath, err)
		return 1
	}
	defer logFile.Close()

	cmd := exec.Command(exe, stripDaemonFlag(opts.args)...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] 后台启动失败: %v\n", err)
		return 1
	}
	pid := cmd.Process.Pid

	// Start() 成功只说明进程拉起来了，不代表端口真的监听上了（比如端口被占）。
	// 但「端口通了」也不等于成功——通的可能是先前那个实例，而我们拉起的这个已经
	// 因为 bind 失败死了。所以两件事都要盯：端口起来，且子进程还活着。
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	addr := probeAddr(opts.host, opts.port)
	listening := make(chan error, 1)
	go func() { listening <- waitForListen(addr, daemonProbeTimeout) }()

	select {
	case waitErr := <-exited:
		return daemonFailed(logPath, fmt.Sprintf("后台进程启动后随即退出 (%v)", waitErr))
	case probeErr := <-listening:
		if probeErr != nil {
			return daemonFailed(logPath, fmt.Sprintf("后台进程没能在 %s 上监听: %v", addr, probeErr))
		}
	}

	// 端口通了，静置一下再确认：监听的确实是我们拉起的那个进程
	select {
	case waitErr := <-exited:
		return daemonFailed(logPath,
			fmt.Sprintf("后台进程已退出 (%v)——%s 上监听的是别的进程", waitErr, addr))
	case <-time.After(daemonSettleDelay):
	}

	fmt.Printf("已在后台启动\n")
	fmt.Printf("  PID:   %d\n", pid)
	fmt.Printf("  地址:  http://%s\n", addr)
	fmt.Printf("  日志:  %s\n", logPath)
	fmt.Printf("  停止:  %s\n", stopHint(pid))
	return 0
}

// daemonFailed 把失败原因和日志末尾一起摆到用户眼前——
// 子进程的输出全进了日志文件，不贴出来的话用户在终端上什么都看不到。
func daemonFailed(logPath, reason string) int {
	fmt.Fprintf(os.Stderr, "[FATAL] %s\n", reason)
	if tail := tailFile(logPath, daemonLogTailBytes); tail != "" {
		fmt.Fprintf(os.Stderr, "--- %s 末尾 ---\n%s\n", logPath, tail)
	}
	return 1
}

// defaultLogPath 日志默认落在临时目录，文件名带上端口——
// 同时跑多个实例时日志不会串在一起。
func defaultLogPath(port int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("agent-session-query-%d.log", port))
}

// stripDaemonFlag 把 -d / --daemon（含 -d=true 这种写法）从参数里摘掉。
// 不摘的话子进程会再 daemonize 一次，无限套娃。
func stripDaemonFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			out = append(out, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if name == "d" || name == "daemon" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// probeAddr 探活要连的地址：绑在通配地址上时连回环，别去连 0.0.0.0
func probeAddr(host string, port int) string {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, fmt.Sprint(port))
}

// waitForListen 等到能连上为止，超时就报错
func waitForListen(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, daemonProbeInterval*4)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(daemonProbeInterval)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("等待 %s 超时", timeout)
	}
	return lastErr
}

// tailFile 取文件末尾若干字节，启动失败时把原因摆到用户眼前
func tailFile(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	if size > max {
		if _, err := f.Seek(size-max, 0); err != nil {
			return ""
		}
	}
	buf := make([]byte, max)
	n, _ := f.Read(buf)
	return strings.TrimSpace(string(buf[:n]))
}
