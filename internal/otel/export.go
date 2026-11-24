package otel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
)

// Options say what to send where.
type Options struct {
	// Endpoint is an OTLP/HTTP collector: its base address, or its /v1/traces path.
	Endpoint string
	Headers  map[string]string
	// Level is how much of the conversation is sent. The zero value sends none of it.
	Level Level

	// Filter and Window select the turns: which sessions, asked when.
	Filter index.Filter
	Window index.Window
	// Settle holds a turn back until it has been quiet this long, so that one being
	// written is sent once, whole, instead of again with every row.
	Settle time.Duration

	// Resend sends every selected turn, whatever the ledger says was sent before.
	Resend bool
	// DryRun, when set, is where the requests are written instead of being sent.
	DryRun io.Writer
	// Ledger is the ledger's file; empty means LedgerPath().
	Ledger string

	Client *http.Client
	// Progress is told how far the export is, turn by turn.
	Progress func(done, total int)
}

// Result is what one export did.
type Result struct {
	Turns    int // turns sent
	Spans    int // as this many spans
	Sessions int // of this many transcripts: sessions, and the subagents that ran in them
	Current  int // turns left alone: the collector already has them as they are
	Requests int
}

// A request is closed at about this size or this many spans, whichever comes first:
// collectors cap what one POST may carry, and a request that fails loses less.
const (
	maxRequestBytes = 3 << 20
	maxRequestSpans = 500
)

// Export sends the selected turns that the collector does not already have.
func Export(ctx context.Context, ix *index.Index, o Options) (Result, error) {
	var res Result
	if o.Endpoint == "" && o.DryRun == nil {
		return res, errors.New("no collector to send to")
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 60 * time.Second}
	}
	var quiet time.Time
	if o.Settle > 0 {
		quiet = time.Now().Add(-o.Settle)
	}
	f := o.Filter
	f.Agents, f.Archived = true, true // a subagent's turns are part of the record
	stamps, err := ix.TurnStamps(ctx, f, o.Window, quiet)
	if err != nil {
		return res, err
	}

	var led *ledger
	sent := map[turnKey]string{}
	if o.Endpoint != "" {
		path := o.Ledger
		if path == "" {
			path = LedgerPath()
		}
		if led, err = openLedger(path, o.Endpoint); err != nil {
			return res, err
		}
		defer led.close()
		if !o.Resend {
			if sent, err = led.sent(ctx); err != nil {
				return res, err
			}
		}
	}

	e := &exporter{ix: ix, places: map[int64]place{}, calls: map[int64]map[string]callAt{}}
	suffix := ":" + strconv.Itoa(Format) + ":" + o.Level.String()

	var (
		pending      []Span
		pendingBytes int
		complete     = map[turnKey]string{}
		sessions     = map[int64]bool{}
	)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if o.DryRun != nil {
			body, err := encode(pending)
			if err != nil {
				return err
			}
			if _, err := o.DryRun.Write(append(body, '\n')); err != nil {
				return err
			}
		} else {
			if err := send(ctx, o.Client, o.Endpoint, o.Headers, pending); err != nil {
				return err
			}
			if err := led.record(ctx, complete); err != nil {
				return fmt.Errorf("sent, but the ledger could not be written (these turns will be sent again): %w", err)
			}
		}
		res.Requests++
		res.Spans += len(pending)
		res.Turns += len(complete)
		pending, pendingBytes = pending[:0], 0
		clear(complete)
		return nil
	}

	var sess index.Session
	var have int64 = -1
	for i, st := range stamps {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if o.Progress != nil {
			o.Progress(i, len(stamps))
		}
		if st.SID != have {
			if sess, err = ix.BySID(ctx, st.SID); err != nil {
				continue // gone since the list was read
			}
			have = st.SID
		}
		key, hash := turnKey{sess.Source, sess.ID, st.Index}, st.Hash+suffix
		if sent[key] == hash {
			res.Current++
			continue
		}
		// The turn the agent is at has not ended: how it ends — on an error, stopped — is
		// not said of it yet, and a mark that it is over moves no hash, so what was sent
		// now would stand. It is left for a later pass, as a turn still being written is
		if sess.UnderWay(st.Index) {
			continue
		}
		turns, err := ix.Turns(ctx, &sess, index.TurnRange{First: st.Index, Last: st.Index, Full: o.Level >= LevelAll})
		if err != nil {
			return res, err
		}
		if len(turns) == 0 {
			continue
		}
		spans := spansOf(&sess, &turns[0], e.placeOf(ctx, &sess, 0), o.Level)
		if len(spans) == 0 {
			continue
		}
		for _, sp := range spans {
			size := sizeOf(sp)
			if len(pending) > 0 && (pendingBytes+size > maxRequestBytes || len(pending) >= maxRequestSpans) {
				if err := flush(); err != nil {
					return res, err
				}
			}
			pending = append(pending, sp)
			pendingBytes += size
		}
		// Written down only once the request holding its last span has been taken
		complete[key] = hash
		sessions[st.SID] = true
	}
	if err := flush(); err != nil {
		return res, err
	}
	if o.Progress != nil {
		o.Progress(len(stamps), len(stamps))
	}
	res.Sessions = len(sessions)
	return res, nil
}

// sizeOf estimates what a span adds to a request.
func sizeOf(s Span) int {
	n := 300 + len(s.Name)
	for _, a := range s.Attrs {
		n += len(a.Key) + 40
		switch v := a.Value.(type) {
		case string:
			n += len(v) + len(v)/8 // JSON escaping
		case []string:
			for _, item := range v {
				n += len(item) + 24
			}
		}
	}
	return n
}

type exporter struct {
	ix     *index.Index
	places map[int64]place
	calls  map[int64]map[string]callAt // per parent transcript: where each of its calls is
}

type callAt struct{ turn, seq int }

// placeOf works out where a session's turns go. A session's are each a trace of their
// own. A subagent's all join the trace of the turn that started it, beneath the very
// call that did — found in its parent's transcript by the call's id, or by the workflow
// run the call began. One whose call cannot be found keeps traces of its own, filed
// under the session it ran in.
func (e *exporter) placeOf(ctx context.Context, s *index.Session, depth int) place {
	if p, ok := e.places[s.SID()]; ok {
		return p
	}
	p := place{group: s.ID}
	if s.Parent != nil && depth < 8 {
		if s.Parent.Session != "" {
			p.group = s.Parent.Session
		}
		if parent, ok, _ := e.ix.Parent(ctx, s); ok {
			above := e.placeOf(ctx, &parent, depth+1)
			p.group = above.group
			if at, ok := e.callIn(ctx, &parent, s.Parent); ok {
				p.trace = above.trace
				if p.trace == "" {
					p.trace = turnTrace(&parent, at.turn)
				}
				p.parent = itemSpan(&parent, at.seq)
			}
		}
	}
	e.places[s.SID()] = p
	return p
}

// callIn finds, in a parent's transcript, the call that started a subagent.
func (e *exporter) callIn(ctx context.Context, parent *index.Session, link *model.Parent) (callAt, bool) {
	calls, ok := e.calls[parent.SID()]
	if !ok {
		calls = map[string]callAt{}
		turns, err := e.ix.Turns(ctx, parent, index.TurnRange{First: 1, Last: -1})
		if err == nil {
			for _, t := range turns {
				for _, it := range t.Items {
					if it.Kind != model.ToolUse || it.Tool == nil || it.Inherited {
						continue
					}
					at := callAt{t.Index, it.Seq}
					if it.Tool.CallID != "" {
						calls["call:"+it.Tool.CallID] = at
					}
					if it.Tool.Run != "" {
						calls["run:"+it.Tool.Run] = at
					}
				}
			}
		}
		e.calls[parent.SID()] = calls
	}
	if link.Call != "" {
		if at, ok := calls["call:"+link.Call]; ok {
			return at, true
		}
	}
	if link.Run != "" {
		if at, ok := calls["run:"+link.Run]; ok {
			return at, true
		}
	}
	return callAt{}, false
}
