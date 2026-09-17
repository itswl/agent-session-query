package app

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestStripDaemonFlag: leave -d in and the child daemonises again, forever
func TestStripDaemonFlag(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{[]string{"-d", "--port", "8080"}, []string{"--port", "8080"}},
		{[]string{"--daemon", "--mode", "claude"}, []string{"--mode", "claude"}},
		{[]string{"-d=true", "--port", "1"}, []string{"--port", "1"}},
		{[]string{"--daemon=false"}, []string{}},
		{[]string{"--port", "8080"}, []string{"--port", "8080"}},
		// A value that happens to be d or daemon must not be caught
		{[]string{"--mode", "d"}, []string{"--mode", "d"}},
		{[]string{"--hook_token=-d"}, []string{"--hook_token=-d"}},
		{[]string{"--log-file", "/tmp/daemon"}, []string{"--log-file", "/tmp/daemon"}},
	}
	for _, c := range cases {
		if got := stripDaemonFlag(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("stripDaemonFlag(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestProbeAddr: bound to a wildcard address, the probe must dial loopback, not 0.0.0.0
func TestProbeAddr(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0":   "127.0.0.1:8080",
		"::":        "127.0.0.1:8080",
		"":          "127.0.0.1:8080",
		"127.0.0.1": "127.0.0.1:8080",
		"localhost": "localhost:8080",
	}
	for host, want := range cases {
		if got := probeAddr(host, 8080); got != want {
			t.Errorf("probeAddr(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestWaitForListen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if err := waitForListen(ln.Addr().String(), time.Second); err != nil {
		t.Fatalf("already listening but the probe missed it: %v", err)
	}

	// Once closed the probe must fail, and must return within the timeout
	addr := ln.Addr().String()
	ln.Close()
	started := time.Now()
	if err := waitForListen(addr, 300*time.Millisecond); err == nil {
		t.Fatal("the port is closed but the probe found it")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("took far too long to return after the timeout: %v", elapsed)
	}
}

func TestDefaultLogPath(t *testing.T) {
	// The port is in the filename so concurrent instances do not interleave their logs
	a, b := defaultLogPath(8080), defaultLogPath(9090)
	if a == b {
		t.Fatalf("different ports should mean different log files: %s", a)
	}
	if !strings.Contains(filepath.Base(a), "8080") {
		t.Fatalf("the log filename should carry the port: %s", a)
	}
}

func TestTailFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)+"\nlast line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tailFile(path, 40); !strings.Contains(got, "last line") {
		t.Fatalf("should have read the tail: %q", got)
	}
	if got := tailFile(filepath.Join(t.TempDir(), "nope"), 40); got != "" {
		t.Fatalf("a missing file should yield the empty string: %q", got)
	}
}
