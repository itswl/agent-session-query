package transcript

import (
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
)

// SyntheticModel is the model name Claude Code writes on a row it made itself — a resumed
// session's "No response requested.", an error it reports in the assistant's place. No
// model answered it: it is no model call, and it must not stretch a turn to the day the
// session was resumed.
const SyntheticModel = "<synthetic>"

// Sink receives a session's turns as they are completed, in order.
type Sink interface {
	Turn(*model.Turn) error
}

// SinkFunc adapts a function to a Sink.
type SinkFunc func(*model.Turn) error

func (f SinkFunc) Turn(t *model.Turn) error { return f(t) }

// Writer builds a session's canonical record from the messages an adapter hands it.
//
// An adapter fills S with what its CLI recorded about the session (id, cwd, model, a
// title of the CLI's own …), calls Add for every message in the order they were written,
// and leaves the rest to Finish: a turn is handed to the sink as soon as the next begins,
// so a transcript of any length is read in the memory of its largest turn.
type Writer struct {
	S model.Session

	sink  Sink
	stats model.Stats
	err   error

	seq       int
	turns     int // real turns begun (the next index is turns+1)
	cur       *turn
	firstAt   time.Time
	lastAt    time.Time
	lastModel string
	firstAsk  string
	anyPrompt string // the first user text of any kind, the title of last resort
}

// turn is a Turn while it is being built.
type turn struct {
	t          model.Turn
	open       map[string]int // call id → index into t.Items, for the result that answers it
	responses  map[string]int // response id → index into t.Calls
	pending    map[int]bool   // items whose write waits for its result before it counts
	files      map[string]int // path → index into t.Files
	lastWasAsk bool
}

// New returns a Writer that hands completed turns to sink.
func New(sink Sink) *Writer { return &Writer{sink: sink} }

// Add takes the next message of the transcript.
func (w *Writer) Add(m Message) {
	if w.err != nil {
		return
	}
	if !m.At.IsZero() {
		if w.firstAt.IsZero() {
			w.firstAt = m.At
		}
		w.lastAt = m.At
	}
	switch m.Role {
	case User:
		w.addUser(m)
	case Assistant:
		w.addAssistant(m)
	default:
		w.addOther(m)
	}
}

// addUser decides what a user message is — a person's ask, words typed mid-turn, or text
// the CLI put in the user's seat — and files its blocks.
func (w *Writer) addUser(m Message) {
	text := joinText(m.Blocks, Text)
	ask, starts := "", false
	if strings.TrimSpace(text) != "" && !m.Injected && !m.Queued && !m.Inherited {
		ask, starts = AskOf(text)
	}
	if strings.TrimSpace(text) != "" && w.anyPrompt == "" && !m.Inherited {
		w.anyPrompt = text
	}

	if starts {
		// Results that precede the ask in its own row answer calls of the turn that is
		// ending — a fork's first row carries the result of the call that started it —
		// and are settled there before the new turn begins
		if w.cur != nil {
			lead := 0
			for lead < len(m.Blocks) && m.Blocks[lead].Type == Result {
				w.settle(w.cur, m, m.Blocks[lead])
				lead++
			}
			m.Blocks = m.Blocks[lead:]
		}
		w.flush()
		w.turns++
		w.cur = newTurn(w.turns)
		w.cur.t.StartedAt = m.At
		w.cur.t.Ask = ask
		if w.firstAsk == "" {
			w.firstAsk = ask
		}
	}
	cur := w.current()
	cur.lastWasAsk = starts
	if !m.At.IsZero() {
		cur.t.LastAt = m.At
	}

	work := starts
	wroteText := false
	for _, b := range m.Blocks {
		switch b.Type {
		case Text, Thinking:
			if wroteText || strings.TrimSpace(text) == "" {
				continue // a message's text blocks are one item, where the first stood
			}
			wroteText = true
			item := model.Item{Kind: model.Context, At: m.At, ID: m.ID, Text: text, Inherited: m.Inherited}
			if starts || m.Queued || (m.Inherited && !m.Injected && isAsk(text)) {
				item.Kind = model.Prompt
				item.Queued = m.Queued
			}
			w.push(cur, item)
		case Call:
			work = true
			w.pushCall(cur, m, b, 0)
		case Result:
			work = true
			w.settle(cur, m, b)
		case EventBlock:
			work = true
			w.pushEvent(cur, m, b)
		}
	}
	if work && !m.At.IsZero() && !m.Inherited {
		cur.t.EndedAt = m.At
	}
}

func isAsk(text string) bool {
	_, ok := AskOf(text)
	return ok
}

// addAssistant files an assistant message's blocks under the model response it is part of.
func (w *Writer) addAssistant(m Message) {
	cur := w.current()
	cur.lastWasAsk = false
	if !m.At.IsZero() {
		cur.t.LastAt = m.At
	}
	synthetic := m.Model == SyntheticModel
	call := 0
	if !synthetic && !m.Inherited {
		call = w.callOf(cur, m)
	}
	for _, b := range m.Blocks {
		switch b.Type {
		case Text:
			if b.Text == "" {
				continue
			}
			w.push(cur, model.Item{Kind: model.Reply, At: m.At, ID: m.ID, Text: b.Text, Call: call, Inherited: m.Inherited})
		case Thinking:
			if b.Text == "" {
				continue
			}
			w.push(cur, model.Item{Kind: model.Thinking, At: m.At, ID: m.ID, Text: b.Text, Call: call, Inherited: m.Inherited})
		case Call:
			w.pushCall(cur, m, b, call)
		case Result:
			w.settle(cur, m, b)
		case EventBlock:
			w.pushEvent(cur, m, b)
		}
	}
	if m.Inherited {
		return
	}
	if text := strings.TrimSpace(joinText(m.Blocks, Text)); text != "" && !synthetic {
		cur.t.Answer = text
	}
	if !synthetic && !m.At.IsZero() {
		cur.t.EndedAt = m.At
		if m.DurMs > 0 {
			if end := m.At.Add(time.Duration(m.DurMs) * time.Millisecond); end.After(cur.t.EndedAt) {
				cur.t.EndedAt = end // a measured call ends after the row's own time
			}
		}
	}
}

// addOther files what the CLI itself or a tool wrote: results, events, and text that is
// neither side's words.
func (w *Writer) addOther(m Message) {
	cur := w.current()
	cur.lastWasAsk = false
	if !m.At.IsZero() {
		cur.t.LastAt = m.At
	}
	work := false
	for _, b := range m.Blocks {
		switch b.Type {
		case Text, Thinking:
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			if m.Role == ToolRole {
				// a tool row whose adapter gave it plain text: it is still a result
				work = true
				w.settle(cur, m, Block{Type: Result, Text: b.Text})
				continue
			}
			w.push(cur, model.Item{Kind: model.Context, At: m.At, ID: m.ID, Text: b.Text, Inherited: m.Inherited})
		case Call:
			work = true
			w.pushCall(cur, m, b, 0)
		case Result:
			work = true
			w.settle(cur, m, b)
		case EventBlock:
			work = true
			w.pushEvent(cur, m, b)
		}
	}
	if work && !m.At.IsZero() && !m.Inherited {
		cur.t.EndedAt = m.At
	}
}

// callOf finds or opens the model call an assistant message belongs to.
func (w *Writer) callOf(cur *turn, m Message) int {
	if m.Model != "" {
		w.lastModel = m.Model
	}
	if m.Response != "" {
		if i, ok := cur.responses[m.Response]; ok {
			c := &cur.t.Calls[i]
			// Later rows of one response: its counters are the largest any row reports
			// (some CLI versions write a running count), and it ends at its last row
			if m.Usage != nil {
				if c.Usage == nil {
					u := *m.Usage
					c.Usage = &u
				} else {
					c.Usage.Raise(*m.Usage)
				}
			}
			c.CostUSD = max(c.CostUSD, m.CostUSD)
			if m.Stop != "" {
				c.Stop = m.Stop
			}
			if m.At.After(c.EndAt) {
				c.EndAt = m.At
			}
			return i + 1
		}
	}
	c := model.ModelCall{
		ID: m.Response, Model: m.Model, At: m.At, EndAt: m.At,
		CostUSD: m.CostUSD, DurMs: m.DurMs, TTFTMs: m.TTFTMs, Stop: m.Stop,
	}
	if c.Model == "" {
		c.Model = w.lastModel
	}
	if m.Usage != nil && !m.Usage.IsZero() {
		u := *m.Usage
		c.Usage = &u
	}
	if m.DurMs > 0 && !m.At.IsZero() {
		c.EndAt = m.At.Add(time.Duration(m.DurMs) * time.Millisecond)
	}
	cur.t.Calls = append(cur.t.Calls, c)
	if m.Response != "" {
		cur.responses[m.Response] = len(cur.t.Calls) - 1
	}
	return len(cur.t.Calls)
}

func (w *Writer) pushCall(cur *turn, m Message, b Block, call int) {
	name := b.Name
	if name == "" {
		name = "tool"
	}
	tool := &model.Tool{CallID: b.CallID, Name: name, Kind: ToolKind(name), Input: inputJSON(b.Input)}
	tool.Files, tool.Command = callFacts(tool.Kind, tool.Input)
	idx := w.push(cur, model.Item{Kind: model.ToolUse, At: m.At, ID: m.ID, Call: call, Tool: tool, Inherited: m.Inherited})
	if b.CallID != "" {
		cur.open[b.CallID] = idx
	}
	if m.Inherited {
		return
	}
	cur.t.Tools++
	cur.t.ToolKinds[tool.Kind]++
	for _, f := range tool.Files {
		cur.touch(f, false)
	}
	if tool.Kind == model.ToolWrite && len(tool.Files) > 0 {
		if b.CallID == "" {
			// It cannot be paired with a result, so it counts as a change on the spot
			for _, f := range tool.Files {
				cur.touch(f, true)
			}
		} else {
			cur.pending[idx] = true
		}
	}
}

// settle pairs a result with its call. A result names its call by id; one that does not,
// or whose call is not in this turn, goes to the latest call still waiting — the same
// tool's first — and failing that stands on its own, so no output is ever dropped.
func (w *Writer) settle(cur *turn, m Message, b Block) {
	idx, ok := -1, false
	if b.CallID != "" {
		if idx, ok = cur.open[b.CallID]; ok {
			delete(cur.open, b.CallID)
		}
	}
	if !ok && b.CallID == "" {
		idx, ok = cur.waiting(b.Name)
	}
	if !ok {
		name := b.Name
		if name == "" {
			name = "tool"
		}
		tool := &model.Tool{CallID: b.CallID, Name: name, Kind: ToolKind(name)}
		idx = w.push(cur, model.Item{Kind: model.ToolUse, At: m.At, ID: m.ID, Tool: tool, Inherited: m.Inherited})
		if !m.Inherited {
			cur.t.Tools++
			cur.t.ToolKinds[tool.Kind]++
		}
	}
	tool := cur.t.Items[idx].Tool
	if cur.t.Items[idx].Inherited {
		m.Inherited = true // the call was the parent's: so is what its result adds up to
	}
	if tool.Done {
		// A second result for one call (a progress update that became the answer): the
		// later one is how it ended
		if tool.Failed() && !m.Inherited {
			cur.t.Failures--
		}
	}
	tool.Done = true
	tool.Output = b.Text
	tool.Status = b.Status
	tool.Exit = b.Exit
	tool.DurMs = b.DurMs
	tool.EndAt = m.At
	if b.Run != "" {
		tool.Run = b.Run
	}
	// A non-zero exit code with no explicit status is an error: the tool said so in the
	// only way a command can
	if tool.Status == "" && tool.Exit != nil && *tool.Exit != 0 {
		tool.Status = model.StatusError
	}
	if m.Inherited {
		return
	}
	if tool.Failed() {
		cur.t.Failures++
	}
	if cur.pending[idx] {
		delete(cur.pending, idx)
		if !tool.Failed() { // a failed edit is not a change
			for _, f := range tool.Files {
				cur.touch(f, true)
			}
		}
	}
}

func (w *Writer) pushEvent(cur *turn, m Message, b Block) {
	kind := b.Name
	if kind == "" {
		kind = "event"
	}
	w.push(cur, model.Item{Kind: model.Event, At: m.At, ID: m.ID, Event: kind, Text: b.Text, Inherited: m.Inherited})
	if kind == model.EventInterrupted && !m.Inherited {
		cur.t.Interrupted = true
	}
}

func (w *Writer) push(cur *turn, item model.Item) int {
	item.Seq = w.seq
	w.seq++
	cur.t.Items = append(cur.t.Items, item)
	return len(cur.t.Items) - 1
}

// current is the turn being built; before the first ask that is turn 0.
func (w *Writer) current() *turn {
	if w.cur == nil {
		w.cur = newTurn(0)
	}
	return w.cur
}

func newTurn(index int) *turn {
	return &turn{
		t:         model.Turn{Index: index, ToolKinds: map[string]int{}},
		open:      map[string]int{},
		responses: map[string]int{},
		pending:   map[int]bool{},
		files:     map[string]int{},
	}
}

func (t *turn) touch(path string, changed bool) {
	if i, ok := t.files[path]; ok {
		t.t.Files[i].Changed = t.t.Files[i].Changed || changed
		return
	}
	t.files[path] = len(t.t.Files)
	t.t.Files = append(t.t.Files, model.File{Path: path, Changed: changed})
}

// waiting is the latest call with no result yet: one of the named tool when there is one.
func (t *turn) waiting(name string) (int, bool) {
	fallback := -1
	for i := len(t.t.Items) - 1; i >= 0; i-- {
		tool := t.t.Items[i].Tool
		if tool == nil || tool.Done {
			continue
		}
		if name == "" || tool.Name == name {
			delete(t.open, tool.CallID)
			return i, true
		}
		if fallback < 0 {
			fallback = i
		}
	}
	if fallback >= 0 {
		delete(t.open, t.t.Items[fallback].Tool.CallID)
		return fallback, true
	}
	return -1, false
}

// flush completes the turn being built and hands it to the sink.
func (w *Writer) flush() {
	cur := w.cur
	w.cur = nil
	if cur == nil || w.err != nil {
		return
	}
	t := &cur.t
	if len(t.Items) == 0 && t.Index == 0 {
		return
	}
	// A turn whose last row is its own ask was never answered: the person asked again, or
	// stopped it, before the agent wrote anything
	if t.Index > 0 && cur.lastWasAsk {
		t.Interrupted = true
	}
	if t.Index > 0 && t.EndedAt.IsZero() {
		t.EndedAt = t.StartedAt
	}
	if t.LastAt.IsZero() {
		t.LastAt = t.EndedAt
	}
	for i := range t.Calls {
		if u := t.Calls[i].Usage; u != nil {
			t.Usage.Add(*u)
		}
		t.CostUSD += t.Calls[i].CostUSD
	}
	if len(t.ToolKinds) == 0 {
		t.ToolKinds = nil
	}
	if t.Index > 0 {
		w.stats.Turns++
	}
	w.stats.Items += len(t.Items)
	w.stats.Tools += t.Tools
	w.stats.Failures += t.Failures
	w.stats.Calls += len(t.Calls)
	w.stats.Usage.Add(t.Usage)
	w.stats.CostUSD += t.CostUSD
	if w.sink != nil {
		w.err = w.sink.Turn(t)
	}
}

// Finish completes the last turn and fills in what the session's own messages say about
// it where the adapter said nothing: when it started and was last written, the model that
// answered last, and — for the CLIs that give a session no title — the first thing asked.
func (w *Writer) Finish() (model.Stats, error) {
	w.flush()
	s := &w.S
	if s.StartedAt.IsZero() {
		s.StartedAt = w.firstAt
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = w.lastAt
	}
	if s.Model == "" {
		s.Model = w.lastModel
	}
	if s.Title != "" {
		s.Title = CleanTitle(s.Title)
	}
	if s.Title == "" {
		s.Title = TitleOf(w.firstAsk)
	}
	if s.Title == "" {
		s.Title = TitleOf(w.anyPrompt)
	}
	// What the CLI kept at session level is the session's total when it holds at least
	// what the responses add up to: all of it where no response carried counters, and
	// the calls that left no response where they did. Totals that hold less describe
	// something else — a fork's own spending, beside copies read as its own because
	// their originals are gone; a counter that was reset — and the responses stand,
	// their cost with them.
	if r := s.Recorded; r != nil && (r.Usage == nil || r.Usage.Covers(w.stats.Usage)) {
		if r.Usage != nil {
			w.stats.Usage = *r.Usage
		}
		// The CLI's own figure is the authoritative one: the responses only know what
		// their own rows cost
		if r.CostUSD > 0 {
			w.stats.CostUSD = r.CostUSD
		}
	}
	return w.stats, w.err
}

// Err is the first error the sink returned; once it is set, Add does nothing.
func (w *Writer) Err() error { return w.err }

func joinText(blocks []Block, kind BlockType) string {
	n := 0
	var only string
	for _, b := range blocks {
		if b.Type == kind && b.Text != "" {
			n++
			only = b.Text
		}
	}
	if n <= 1 {
		return only
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == kind && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// Collected is a whole session in memory: what Collect returns.
type Collected struct {
	Session model.Session
	Stats   model.Stats
	Turns   []model.Turn
}

// Collector is a Sink that keeps every turn. It is for the callers that want a session
// whole — tests, an export — rather than turn by turn.
type Collector struct{ Turns []model.Turn }

func (c *Collector) Turn(t *model.Turn) error {
	c.Turns = append(c.Turns, *t)
	return nil
}

// Items is every item of every turn, in order.
func (c *Collected) Items() []model.Item {
	var out []model.Item
	for i := range c.Turns {
		out = append(out, c.Turns[i].Items...)
	}
	return out
}
