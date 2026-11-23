package transcript

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
)

// A transcript that is being written is read again each time it changes, and a long one is
// most of what that costs. For a CLI that only ever adds to the end of its files, a read
// can be taken up where the latest turn began instead, at a place the read before marked:
// the turns before it are in the index already, and are not read again.

// Mark is a place a read of a transcript can be taken up again: where one of its turns
// begins. It holds what the Writer had made of everything before that turn, and what the
// adapter had — its own, given back to it as it was: where the turn's first row is in its
// files, and what it carries from row to row.
type Mark struct {
	Turn   int             `json:"turn"` // the turn that begins there
	Writer begun           `json:"writer"`
	Source json.RawMessage `json:"source"`
}

// Before is what the turns before the mark add up to.
func (m *Mark) Before() model.Stats { return m.Writer.Stats }

// begun is the Writer as a turn began: the turn before it handed to the sink, and nothing
// of the new one made yet.
type begun struct {
	Seq       int           `json:"seq"`
	Turns     int           `json:"turns"`
	FirstAt   time.Time     `json:"firstAt,omitzero"`
	LastAt    time.Time     `json:"lastAt,omitzero"`
	LastModel string        `json:"lastModel,omitempty"`
	FirstAsk  string        `json:"firstAsk,omitempty"`
	AnyPrompt string        `json:"anyPrompt,omitempty"`
	Stats     model.Stats   `json:"stats"`
	Session   model.Session `json:"session"`
}

// ErrNotTakenUp is a read taken up at a mark that found no turn beginning there: the
// transcript does not go on as it did when it was marked. It is to be read whole.
var ErrNotTakenUp = errors.New("the transcript does not go on where it was marked")

// Turns is how many turns have begun: the index of the latest.
func (w *Writer) Turns() int { return w.turns }

// MarkTurn marks the turn the message just added began as a place a later read can take
// the transcript up again (see Mark). place is the adapter's, given back to it as it was:
// where the turn's first row begins, and what it carried from the rows before. An adapter
// calls it only when going on from that row, with place, reads the rest as it was read
// this time. A turn whose first message also answered calls of the turn before is not
// marked: taken up there, the answers would have no call to go to.
func (w *Writer) MarkTurn(place any) {
	if !w.clean || w.cur == nil || w.cur.t.Index != w.turns || w.began.Turns != w.turns-1 {
		return
	}
	raw, err := json.Marshal(place)
	if err != nil {
		return
	}
	b := w.began
	// What the session points to is copied, so that what the adapter sets later does not
	// reach the mark
	s := &b.Session
	if r := s.Recorded; r != nil {
		c := *r
		if r.Usage != nil {
			u := *r.Usage
			c.Usage = &u
		}
		s.Recorded = &c
	}
	if p := s.Parent; p != nil {
		c := *p
		s.Parent = &c
	}
	s.Live = nil
	w.mark = &Mark{Turn: w.turns, Writer: b, Source: raw}
}

// Marked is the latest turn marked; nil when none was.
func (w *Writer) Marked() *Mark { return w.mark }

// Resume takes the transcript up at m: the Writer goes on from what it had made of
// everything before the turn m names, and the adapter adds the messages from that turn's
// first row on. The first must begin the turn; if it does not, or nothing is added, the
// Writer fails with ErrNotTakenUp. What adapters passed over (Unread) is counted only for
// what is read from m on.
func (w *Writer) Resume(m *Mark) {
	b := m.Writer
	w.seq, w.turns = b.Seq, b.Turns
	w.firstAt, w.lastAt = b.FirstAt, b.LastAt
	w.lastModel, w.firstAsk, w.anyPrompt = b.LastModel, b.FirstAsk, b.AnyPrompt
	w.stats, w.S = b.Stats, b.Session
	w.cur, w.live = nil, liveTail{}
	w.resumed, w.takenUp = true, false
}

// Resumed reports whether the read was taken up at a mark (see Resume).
func (w *Writer) Resumed() bool { return w.resumed }

// begins reports whether m begins a turn of its own, with nothing of the turn before in
// it: what the first message of a read taken up at a mark must do.
func begins(m *Message) bool {
	if m.Role != User || m.Injected || m.Queued || m.Inherited || len(m.Blocks) == 0 || m.Blocks[0].Type == Result {
		return false
	}
	text := joinText(m.Blocks, Text)
	if strings.TrimSpace(text) == "" {
		return false
	}
	_, starts := AskOf(text)
	return starts
}
