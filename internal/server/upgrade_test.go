package server

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a log the watcher writes while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The health report names the file the server was started from and whether an upgrade has
// replaced it; the log says so once it has, so that whoever reads it knows to restart.
func TestAServerSaysWhenItsProgramWasReplaced(t *testing.T) {
	var log lockedBuffer
	s, _, _ := build(t, Options{Log: &log})
	rec := call{path: "/api/health"}.do(s)
	var h struct {
		Executable *string `json:"executable"`
		Replaced   *bool   `json:"replaced"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil || h.Executable == nil || *h.Executable == "" || h.Replaced == nil || *h.Replaced {
		t.Errorf("health of a server whose program nobody touched: %s", rec.Body)
	}

	s.program = func() (string, bool) { return "/opt/asq/bin/asq", true }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.watch(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), "/opt/asq/bin/asq was replaced since this server started") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if said := log.String(); strings.Count(said, "was replaced") != 1 || !strings.Contains(said, "until it is restarted") {
		t.Errorf("the log of a server whose program was replaced:\n%s", said)
	}
}
