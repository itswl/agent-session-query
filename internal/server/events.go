package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/index"
)

// The stream of what changed, as Server-Sent Events.
//
//	event: hello     {"version": "…", "sync": {…}}      once, on connecting
//	event: changes   {"sessions": [{"source","id","gone"}…]}   the sessions a sync wrote
//	event: changes   {"all": true}                       more changed than is worth naming
//	event: sync      {"running", "total", "done", …}     a long read of the stores, as it goes
//
// It carries no content: a client that hears a session changed asks for what it wants of
// it. That keeps the stream cheap enough to leave open on every tab, and means a missed
// event costs a stale row, never a wrong one.
const (
	gather     = 250 * time.Millisecond // changes this close together travel as one event
	heartbeat  = 20 * time.Second       // a comment line, so proxies do not close a quiet stream
	writeGrace = 15 * time.Second       // a reader that takes longer than this to accept a line is gone
	nameUpTo   = 100                    // past this many sessions the event just says "all"
)

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.streams.Add(1) > maxStream {
		s.streams.Add(-1)
		w.Header().Set("Retry-After", "30")
		writeError(w, r, http.StatusServiceUnavailable, "too many open streams")
		return
	}
	defer s.streams.Add(-1)

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: pass each event on as it comes
	w.WriteHeader(http.StatusOK)

	send := func(event string, data any) bool {
		body, err := json.Marshal(data)
		if err != nil {
			return true
		}
		_ = rc.SetWriteDeadline(time.Now().Add(writeGrace))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	changes, stop := s.app.Index.Subscribe()
	defer stop()

	state := s.app.Index.State()
	if !send("hello", map[string]any{"version": app.Version, "sync": state}) {
		return
	}

	progress := time.NewTicker(time.Second)
	defer progress.Stop()
	beat := time.NewTicker(heartbeat)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case c := <-changes:
			batch := []index.Change{c}
			seen := map[index.Change]bool{c: true}
			timer := time.NewTimer(gather)
		collect:
			for {
				select {
				case c := <-changes:
					if !seen[c] {
						seen[c] = true
						batch = append(batch, c)
					}
				case <-timer.C:
					break collect
				case <-r.Context().Done():
					timer.Stop()
					return
				}
			}
			var payload any = map[string]any{"sessions": batch}
			if len(batch) > nameUpTo {
				payload = map[string]any{"all": true}
			}
			if !send("changes", payload) {
				return
			}
		case <-progress.C:
			now := s.app.Index.State()
			if now.Running != state.Running || now.Done != state.Done || now.Total != state.Total {
				state = now
				if !send("sync", state) {
					return
				}
			}
		case <-beat.C:
			_ = rc.SetWriteDeadline(time.Now().Add(writeGrace))
			if _, err := fmt.Fprint(w, ": still here\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
