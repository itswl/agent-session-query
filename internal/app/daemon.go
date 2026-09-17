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

// Background mode (-d).
//
// fork is not an option in Go: the runtime is multi-threaded and fork only clones the
// calling thread, leaving the child with a half-dead runtime. So the route is to re-exec
// ourselves — strip -d from the arguments (or the child would daemonise again), redirect
// output to a log file, and detach the process from the current terminal.
const (
	daemonProbeTimeout  = 5 * time.Second
	daemonProbeInterval = 50 * time.Millisecond
	daemonSettleDelay   = 300 * time.Millisecond
	daemonLogTailBytes  = 2048
)

type daemonOptions struct {
	args    []string // the original command line
	logPath string   // empty means derive it from the port
	host    string
	port    int
}

// startDaemon relaunches this program in the background and returns the parent's exit
// code.
func startDaemon(opts daemonOptions) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] cannot locate own executable: %v\n", err)
		return 1
	}

	logPath := opts.logPath
	if logPath == "" {
		logPath = defaultLogPath(opts.port)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] cannot open log file %s: %v\n", logPath, err)
		return 1
	}
	defer logFile.Close()

	cmd := exec.Command(exe, stripDaemonFlag(opts.args)...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] failed to start in the background: %v\n", err)
		return 1
	}
	pid := cmd.Process.Pid

	// Start() succeeding only means the process launched, not that the port is actually
	// listening (it may already be taken). But "the port answers" does not mean success
	// either — what answers could be an earlier instance while the child we just launched
	// has already died on a failed bind. So watch both: the port comes up, and the child
	// is still alive.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	addr := probeAddr(opts.host, opts.port)
	listening := make(chan error, 1)
	go func() { listening <- waitForListen(addr, daemonProbeTimeout) }()

	select {
	case waitErr := <-exited:
		return daemonFailed(logPath, fmt.Sprintf("the background process exited immediately after starting (%v)", waitErr))
	case probeErr := <-listening:
		if probeErr != nil {
			return daemonFailed(logPath, fmt.Sprintf("the background process never listened on %s: %v", addr, probeErr))
		}
	}

	// The port answers; settle briefly and confirm that what is listening really is the
	// process we launched
	select {
	case waitErr := <-exited:
		return daemonFailed(logPath,
			fmt.Sprintf("the background process exited (%v); something else is listening on %s",
				waitErr, addr))
	case <-time.After(daemonSettleDelay):
	}

	fmt.Printf("Started in the background\n")
	fmt.Printf("  PID:   %d\n", pid)
	fmt.Printf("  Address: http://%s\n", addr)
	fmt.Printf("  Log:     %s\n", logPath)
	fmt.Printf("  Stop:    %s\n", stopHint(pid))
	return 0
}

// daemonFailed puts the reason and the tail of the log in front of the user. All of the
// child's output went to the log file, so without printing it there is nothing to see in
// the terminal at all.
func daemonFailed(logPath, reason string) int {
	fmt.Fprintf(os.Stderr, "[FATAL] %s\n", reason)
	if tail := tailFile(logPath, daemonLogTailBytes); tail != "" {
		fmt.Fprintf(os.Stderr, "--- tail of %s ---\n%s\n", logPath, tail)
	}
	return 1
}

// defaultLogPath puts the log in the temp directory with the port in its name, so
// several instances running at once do not interleave their logs.
func defaultLogPath(port int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("agent-session-query-%d.log", port))
}

// stripDaemonFlag removes -d / --daemon (including the -d=true spelling) from the
// arguments. Leave it in and the child daemonises again, forever.
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

// probeAddr is the address the probe dials: when bound to a wildcard address, dial
// loopback rather than 0.0.0.0
func probeAddr(host string, port int) string {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, fmt.Sprint(port))
}

// waitForListen waits until the address accepts a connection, erroring on timeout
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
		lastErr = fmt.Errorf("timed out after %s", timeout)
	}
	return lastErr
}

// tailFile reads the last few bytes of a file, so a failed start can show why
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
