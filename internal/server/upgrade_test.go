package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// A page left open is told when the program was replaced, and asks for the health report
// again, whose notice says to restart: it does not wait for a reload to show it.
func TestAnOpenPageIsToldTheProgramWasReplaced(t *testing.T) {
	s, _, _ := build(t, Options{})
	srv := httptest.NewServer(s)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	next := func() (event, data string) {
		for lines.Scan() {
			switch line := lines.Text(); {
			case strings.HasPrefix(line, "event: "):
				event = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = line[6:]
			case line == "" && event != "":
				return event, data
			}
		}
		t.Fatalf("the stream ended before it said the program was replaced: %v", lines.Err())
		return "", ""
	}
	if event, _ := next(); event != "hello" {
		t.Fatalf("first event = %s", event)
	}

	s.program = func() (string, bool) { return "/opt/asq/bin/asq", true }
	done := make(chan struct{})
	go func() { s.watch(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for {
		if event, data := next(); event == "health" {
			if data != `{"replaced":true}` {
				t.Errorf("health = %s", data)
			}
			break
		}
	}
}
