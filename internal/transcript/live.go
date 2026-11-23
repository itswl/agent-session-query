package transcript

import (
	"slices"
	"strconv"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
)

// Tells is how far a CLI's transcript records where its latest turn stands.
type Tells int

const (
	// TellsNothing: it is not said. The default, for the CLIs whose rows do not show it
	// with any confidence.
	TellsNothing Tells = iota
	// TellsByRows: the last rows say it. The agent's words with nothing after them, and
	// nothing said to follow, end its turn, as does a mark the CLI writes (Message.Ends)
	// or the person stopping it; a call with no result is waited on; anything else is work.
	TellsByRows
	// TellsByMarks: the CLI marks the end of every turn (Message.Ends), and a turn without
	// the mark is in flight, whatever its last words were.
	TellsByMarks
)

// liveTail is where the latest turn stands, as the messages read so far say.
type liveTail struct {
	pend     []int     // the turn's calls with no result yet, of its latest response: indexes into its items
	answered bool      // the agent's words, with nothing said to follow, are the last thing
	ended    bool      // the turn is over: marked so, or stopped
	at       time.Time // when it was answered or ended
	since    time.Time // when the agent's present stretch of work began

	// Which response the latest assistant message is of, and which one answered: a call
	// in the response that answered is more of the same work, and one after it is work
	// taken up again
	messages   int
	resp       string
	answeredIn string
}

// response notes an assistant message, and names the response it is of. Calls of an
// earlier response still waiting were given up on: the model went on without them.
func (l *liveTail) response(id string) string {
	l.messages++
	key := "message " + strconv.Itoa(l.messages) // a message of its own, as CLIs that name no response write them
	if id != "" {
		key = "response " + id
	}
	if key != l.resp {
		l.pend = l.pend[:0]
	}
	l.resp = key
	return key
}

// work notes something the agent did, in the response named (or "" for none): a turn
// that was over is in flight again from then.
func (l *liveTail) work(at time.Time, in string) {
	if l.ended || l.since.IsZero() || l.answered && (in == "" || in != l.answeredIn) {
		l.since = at
	}
	l.answered, l.ended = false, false
}

func (l *liveTail) call(idx int, at time.Time, in string) {
	l.work(at, in)
	l.pend = append(l.pend, idx)
}

// result notes a call answered. One the person stopped or declined, with nothing else
// waiting, ends the turn: the agent is not asked to go on.
func (l *liveTail) result(idx int, status string, at time.Time, in string) {
	l.pend = slices.DeleteFunc(l.pend, func(i int) bool { return i == idx })
	l.work(at, in)
	if status == model.StatusInterrupted && len(l.pend) == 0 {
		l.end(at)
	}
}

// said notes the agent's words. Calls it made before them and has no result for were
// given up on: it went on without them.
func (l *liveTail) said(at time.Time, in string) {
	l.work(at, in)
	l.pend = l.pend[:0]
	l.answered, l.at, l.answeredIn = true, at, in
}

func (l *liveTail) end(at time.Time) {
	l.pend = l.pend[:0]
	l.answered, l.ended, l.at = false, true, at
}

// state is where the turn stands, by what the CLI records.
func (l *liveTail) state(t *model.Turn, tells Tells) *model.Live {
	switch {
	case l.ended:
		return &model.Live{State: model.LiveIdle, Since: l.at}
	case len(l.pend) > 0:
		it := &t.Items[l.pend[len(l.pend)-1]]
		live := &model.Live{State: model.LiveWaiting, Since: it.At, Seq: it.Seq}
		if tool := it.Tool; tool != nil {
			live.Tool, live.Command = tool.Name, tool.Command
			if live.Command == "" && len(tool.Files) > 0 {
				live.File = tool.Files[0]
			}
		}
		return live
	case l.answered && tells == TellsByRows:
		return &model.Live{State: model.LiveIdle, Since: l.at}
	}
	return &model.Live{State: model.LiveWorking, Since: l.since}
}

// callsFollow reports whether a response's stop reason, as a CLI records it, says the
// model stopped to have tools called: what it said before them is not its answer.
func callsFollow(stop string) bool {
	switch stop {
	case "tool_use", "tool_calls", "toolUse", "tool-calls", "function_call":
		return true
	}
	return false
}
