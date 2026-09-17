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

// TestStripDaemonFlag：不摘掉 -d 的话，子进程会再 daemonize 一次，无限套娃
func TestStripDaemonFlag(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{[]string{"-d", "--port", "8080"}, []string{"--port", "8080"}},
		{[]string{"--daemon", "--mode", "claude"}, []string{"--mode", "claude"}},
		{[]string{"-d=true", "--port", "1"}, []string{"--port", "1"}},
		{[]string{"--daemon=false"}, []string{}},
		{[]string{"--port", "8080"}, []string{"--port", "8080"}},
		// 值恰好叫 d 或 daemon 的不能误伤
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

// TestProbeAddr：绑在通配地址上时要连回环，不能去连 0.0.0.0
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
		t.Fatalf("已经在监听却没探到: %v", err)
	}

	// 关掉之后应当探不到，且要在超时内返回
	addr := ln.Addr().String()
	ln.Close()
	started := time.Now()
	if err := waitForListen(addr, 300*time.Millisecond); err == nil {
		t.Fatal("端口已关却探到了")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("超时后拖了太久才返回: %v", elapsed)
	}
}

func TestDefaultLogPath(t *testing.T) {
	// 文件名带端口：同时跑多个实例时日志不会串在一起
	a, b := defaultLogPath(8080), defaultLogPath(9090)
	if a == b {
		t.Fatalf("不同端口应当是不同的日志文件: %s", a)
	}
	if !strings.Contains(filepath.Base(a), "8080") {
		t.Fatalf("日志文件名里应当带端口: %s", a)
	}
}

func TestTailFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)+"\n最后一行\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tailFile(path, 40); !strings.Contains(got, "最后一行") {
		t.Fatalf("应当取到末尾: %q", got)
	}
	if got := tailFile(filepath.Join(t.TempDir(), "nope"), 40); got != "" {
		t.Fatalf("文件不存在应当返回空: %q", got)
	}
}
