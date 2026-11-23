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

	told := tell(s.app.Index.State(), false, time.Now())
	if !send("hello", map[string]any{"version": app.Version, "sync": told}) {
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
			now := tell(s.app.Index.State(), told.Running, time.Now())
			if now.Running != told.Running || now.Done != told.Done || now.Total != told.Total || now.Stores != told.Stores {
				told = now
				if !send("sync", told) {
					return
				}
			}
		case <-beat.C:
			// When the stores were last read is worth knowing and not worth an event of
			// its own: it rides on the line that keeps the stream open
			if now := tell(s.app.Index.State(), told.Running, time.Now()); !now.Running && !told.Running && !now.At.Equal(told.At) {
				told = now
				if !send("sync", told) {
					return
				}
				continue
			}
			_ = rc.SetWriteDeadline(time.Now().Add(writeGrace))
			if _, err := fmt.Fprint(w, ": still here\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// A sync is said aloud when it is a catch-up: this many transcripts still to read, and
// going on for this long.
const (
	catchUp   = 8
	feltAfter = 2 * time.Second
)

// tell is what the stream says of where a sync stands.
//
// Not all of it. On a machine with an agent at work a transcript grows every few
// seconds and is read again, and that read is the ordinary state of things: what it
// found arrives as "changes", and a status that announced each one would blink for as
// long as anything ran. What is said is a read somebody may be waiting on — the first
// one, or a catch-up of many transcripts that has gone on long enough to be felt. Once
// said, it is followed to its end, however few are left.
func tell(st index.SyncState, saying bool, now time.Time) index.SyncState {
	if st.Running && !saying {
		first := st.At.IsZero()
		long := st.Total-st.Done >= catchUp && now.Sub(st.Began) >= feltAfter
		st.Running = first || long
	}
	if !st.Running {
		st.Total, st.Done, st.Again = 0, 0, 0
	}
	return st
}
