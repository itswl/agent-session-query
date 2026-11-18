package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
	"github.com/itswl/agent-session-query/internal/txt"
)

// What a turn keeps in line, and what goes to the bodies table instead. A turn is read
// far more often than any one of its outputs: the page draws a step as a line and opens
// it on request, a brief quotes a command and not its log. So the turn holds a preview
// of whatever is long — the head, and more of the tail, where a command says how it
// failed — and the whole text is one lookup away.
const (
	outputInline = 2 << 10  // a tool's output
	inputInline  = 4 << 10  // a tool's arguments, as JSON
	inputString  = 512      // one string inside them
	textInline   = 32 << 10 // words and reasoning: what a turn is read for
	askInHead    = 2000     // characters of the ask and the answer a turn's head carries
	answerInHead = 4000
	// What the words table reads of one tool call. Past this an output is a log, and its
	// head and tail are where the searchable part is.
	outputWords = 32 << 10
	inputWords  = 8 << 10
	failureOut  = 600 // characters of a failed call's output the failures table keeps
)

// reading is one transcript as it was read, ready to be written.
type reading struct {
	source string
	agent  string
	reads  int  // the adapter's reading (sources.Spec.Reads)
	again  bool // read only because that reading is newer than the index's
	ref    sources.Ref
	sess   model.Session
	stats  model.Stats
	turns  []turnRow
	err    string
	readMs int64

	firstID, lastID string
	lastAsk, answer string
}

type turnRow struct {
	idx                        int
	startedAt, endedAt, lastAt int64
	firstSeq, items            int
	tools, failures            int
	interrupted                bool
	hash                       []byte
	head                       string
	body                       []byte
	bodies                     []bodyRow
	usage                      []usageRow
	files                      []model.File
	failed                     []failureRow
	words                      [5]string // ask, reply, thinking, tool, output
}

type bodyRow struct {
	seq  int
	part string
	data []byte
}

type usageRow struct {
	model string
	at    int64
	calls int
	u     model.Usage
}

type failureRow struct {
	seq     int
	at      int64
	tool    string
	kind    string
	status  string
	exit    *int
	command string
	output  string
}

// turnBody is what a turn's body column holds.
type turnBody struct {
	Items []model.Item      `json:"items"`
	Calls []model.ModelCall `json:"calls,omitempty"`
}

// read parses one transcript into a reading. It does everything that needs no database —
// the parse, the previews, the compression — so that many can run at once and the one
// writer only has rows to insert.
func (ix *Index) read(ctx context.Context, st Store, ref sources.Ref) *reading {
	start := time.Now()
	rd := &reading{source: st.Name, agent: st.Spec.Name, reads: st.Spec.Reads, ref: ref}
	w := transcript.New(transcript.SinkFunc(func(t *model.Turn) error {
		if t.Index > maxTurnIndex {
			return nil
		}
		rd.turns = append(rd.turns, ix.turnRow(rd, t))
		return ctx.Err()
	}))
	err := st.Source.Read(ctx, ref, w)
	stats, werr := w.Finish()
	if err == nil {
		err = werr
	}
	if err != nil {
		rd.err = err.Error()
	}
	rd.sess, rd.stats = w.S, stats
	rd.sess.Source, rd.sess.Agent = st.Name, st.Spec.Name
	if rd.sess.ID == "" {
		rd.sess.ID = ref.Key
	}
	if rd.sess.UpdatedAt.IsZero() {
		rd.sess.UpdatedAt = ref.ModTime.UTC()
	}
	if rd.sess.StartedAt.IsZero() {
		rd.sess.StartedAt = rd.sess.UpdatedAt
	}
	// What the CLI recorded for the session beyond what its responses add up to — all of
	// it, for a CLI that keeps no counters per response — has no response to hang on. It
	// is entered against the session's last turn, under the session's model, so every
	// figure summed from the usage table includes it.
	if r := rd.sess.Recorded; r != nil && r.Usage != nil {
		if rest := r.Usage.Minus(sumUsage(rd.turns)); r.Usage.Covers(sumUsage(rd.turns)) && !rest.IsZero() {
			if len(rd.turns) == 0 {
				rd.turns = append(rd.turns, ix.turnRow(rd, &model.Turn{Index: 0}))
			}
			last := &rd.turns[len(rd.turns)-1]
			merged := false
			for i := range last.usage {
				if last.usage[i].model == rd.sess.Model {
					last.usage[i].u.Add(rest)
					merged = true
					break
				}
			}
			if !merged {
				last.usage = append(last.usage, usageRow{model: rd.sess.Model, at: ms(rd.sess.UpdatedAt), u: rest})
			}
			// The turn's rows changed, though its content did not: the totals are part
			// of what names it
			last.hash = append(last.hash, []byte(strconv.FormatInt(r.Usage.Total(), 36))...)
		}
	}
	rd.readMs = time.Since(start).Milliseconds()
	return rd
}

func sumUsage(rows []turnRow) model.Usage {
	var u model.Usage
	for i := range rows {
		for _, r := range rows[i].usage {
			u.Add(r.u)
		}
	}
	return u
}

// turnRow renders a turn as the rows it is stored in.
func (ix *Index) turnRow(rd *reading, t *model.Turn) turnRow {
	row := turnRow{
		idx: t.Index, startedAt: ms(t.StartedAt), endedAt: ms(t.EndedAt), lastAt: ms(t.LastAt),
		items: len(t.Items), tools: t.Tools, failures: t.Failures, interrupted: t.Interrupted,
		files: t.Files,
	}
	if len(t.Items) > 0 {
		row.firstSeq = t.Items[0].Seq
	}
	row.words = turnWords(t.Items)
	h := sha256.New()

	for i := range t.Items {
		it := &t.Items[i]
		if it.ID != "" && !it.Inherited && (it.Kind == model.Prompt || it.Kind == model.Reply || it.Kind == model.ToolUse) {
			if rd.firstID == "" {
				rd.firstID = it.ID
			}
			rd.lastID = it.ID
		}
		if tool := it.Tool; !it.Inherited && it.Kind == model.ToolUse && tool.Failed() {
			row.failed = append(row.failed, failureRow{
				seq: it.Seq, at: ms(firstTime(tool.EndAt, it.At)), tool: tool.Name, kind: tool.Kind,
				status: tool.Status, exit: tool.Exit, command: txt.Truncate(tool.Command, 2000, "…"),
				output: tailPreview(tool.Output, failureOut),
			})
		}

		// What is too long to keep in line goes to the bodies table whole
		if len(it.Text) > textInline {
			row.bodies = append(row.bodies, bodyRow{it.Seq, "text", ix.enc.EncodeAll([]byte(it.Text), nil)})
			it.Bytes = len(it.Text)
			it.Text, _ = txt.Preview(it.Text, textInline)
		}
		if tool := it.Tool; tool != nil {
			if len(tool.Output) > outputInline {
				row.bodies = append(row.bodies, bodyRow{it.Seq, "output", ix.enc.EncodeAll([]byte(tool.Output), nil)})
				tool.OutBytes = len(tool.Output)
				tool.Output, _ = txt.Preview(tool.Output, outputInline)
			}
			if len(tool.Input) > inputInline {
				row.bodies = append(row.bodies, bodyRow{it.Seq, "input", ix.enc.EncodeAll(tool.Input, nil)})
				tool.InBytes = len(tool.Input)
				tool.Input = clipJSON(tool.Input, inputString)
			}
		}
	}
	for i := range row.bodies {
		h.Write(row.bodies[i].data)
	}

	// What the turn's calls consumed, by model
	byModel := map[string]int{}
	for i := range t.Calls {
		c := &t.Calls[i]
		j, ok := byModel[c.Model]
		if !ok {
			j = len(row.usage)
			byModel[c.Model] = j
			row.usage = append(row.usage, usageRow{model: c.Model, at: ms(firstTime(c.At, t.StartedAt))})
		}
		row.usage[j].calls++
		if c.Usage != nil {
			row.usage[j].u.Add(*c.Usage)
		}
	}

	body, _ := json.Marshal(turnBody{Items: t.Items, Calls: t.Calls})
	h.Write(body)
	row.body = ix.enc.EncodeAll(body, nil)

	head := *t
	head.Items, head.Calls = nil, nil
	head.Ask = txt.Truncate(head.Ask, askInHead, "…")
	head.Answer = txt.Truncate(head.Answer, answerInHead, "…")
	headJSON, _ := json.Marshal(head)
	h.Write(headJSON)
	row.head = string(headJSON)
	row.hash = h.Sum(nil)

	if t.Index > 0 {
		rd.lastAsk = head.Ask
	}
	if head.Answer != "" {
		rd.answer = head.Answer
	}
	return row
}

func firstTime(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}

// turnWords is what a turn gives the words table, by column — ask, reply, thinking, tool,
// output: what each side said, what was run and what came back, each column's words
// followed by their stems. A fork's copy of its parent's rows is the parent's to be found
// in, and injected context is the same text in every session. The items are whole: a turn
// written again from the index (see Index.refill) has what the index keeps as a preview
// read whole first, so that its words come out as they did.
func turnWords(items []model.Item) [5]string {
	var words [5]strings.Builder
	for i := range items {
		it := &items[i]
		if it.Inherited {
			continue
		}
		switch it.Kind {
		case model.Prompt:
			addWords(&words[0], it.Text, textInline)
		case model.Reply:
			addWords(&words[1], it.Text, textInline)
		case model.Thinking:
			addWords(&words[2], it.Text, textInline)
		case model.Event:
			addWords(&words[2], it.Text, inputWords)
		case model.ToolUse:
			tool := it.Tool
			addWords(&words[3], tool.Name, 0)
			if tool.Command != "" {
				addWords(&words[3], tool.Command, inputWords)
			} else {
				addWords(&words[3], inputText(tool.Input), inputWords)
			}
			addWords(&words[4], tool.Output, outputWords)
		}
	}
	var out [5]string
	for i := range words {
		out[i] = withStems(words[i].String())
	}
	return out
}

// titleWords is a session's title as the words table holds it, in the row of the turn that
// holds it (see titleTurn).
func titleWords(title string) string {
	return withStems(segment(title))
}

// titleTurn is the turn whose row in the words table holds its session's title: the first
// ask, as turns are numbered. A word of the title finds the session once, there, and not
// in each of its turns — which would have every turn match, be counted and be listed by
// Find for a word only the title says. It is held whether the session's CLI named it or it
// is titled by how its first ask begins: title: finds it either way.
const titleTurn = 1

// addWords appends text for the words table, keeping the head and the tail of anything
// longer than limit.
func addWords(b *strings.Builder, s string, limit int) {
	if s == "" {
		return
	}
	if limit > 0 && len(s) > limit {
		s, _ = txt.Preview(s, limit)
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString(segment(s))
}

// inputText is the string values of a call's arguments, which is the part of them a
// person would search for: a path, a pattern, a query — not the keys.
func inputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t)
		case []any:
			for _, item := range t {
				walk(item)
			}
		case map[string]any:
			// In the order of the keys, not of a map's whim: the same call reads the
			// same every time it is indexed and every time a match in it is shown
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		}
	}
	walk(v)
	return b.String()
}

// tailPreview is the end of an output, cleaned: where a failed command says why.
func tailPreview(s string, n int) string {
	s = strings.TrimSpace(txt.Clean(s))
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return "…" + string(runes[len(runes)-n:])
}

// clipJSON shortens the long strings inside a JSON value, leaving its shape — and so
// its validity — as it was: a reader can still see which file an edit named and how its
// text begins and ends.
func clipJSON(raw json.RawMessage, limit int) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return raw
	}
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			if len(t) > limit {
				s, _ := txt.Preview(t, limit)
				return s
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k])
			}
		}
		return v
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		return raw
	}
	return out
}

// write stores a reading: the session's row, and whichever of its turns changed since
// it was last read. It reports whether anything was written.
func (ix *Index) write(ctx context.Context, rd *reading) (sid int64, changed bool, err error) {
	err = ix.tx(ctx, func(tx *sql.Tx) error {
		s, st := &rd.sess, &rd.stats
		p := model.Parent{}
		if s.Parent != nil {
			p = *s.Parent
		}
		err := tx.QueryRowContext(ctx, `
INSERT INTO sessions (source, agent, key, stamp, reads, id, path, title, cwd, branch, model, version, platform,
  started_at, updated_at, archived, running,
  parent_session, parent_agent, parent_call, parent_run, agent_type, fork,
  turns, items, tools, failures, calls, tok_in, tok_out, tok_cr, tok_cw, tok_think, cost,
  last_ask, answer, first_id, last_id, error, read_ms, read_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?, ?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?,?)
ON CONFLICT (source, key) DO UPDATE SET
  agent=excluded.agent, stamp=excluded.stamp, reads=excluded.reads, reads_seq=sessions.reads_seq+1, id=excluded.id, path=excluded.path, title=excluded.title,
  cwd=excluded.cwd, branch=excluded.branch, model=excluded.model, version=excluded.version,
  platform=excluded.platform, started_at=excluded.started_at, updated_at=excluded.updated_at,
  archived=excluded.archived, running=excluded.running,
  parent_session=excluded.parent_session, parent_agent=excluded.parent_agent,
  parent_call=excluded.parent_call, parent_run=excluded.parent_run, agent_type=excluded.agent_type,
  fork=excluded.fork,
  turns=excluded.turns, items=excluded.items, tools=excluded.tools, failures=excluded.failures,
  calls=excluded.calls, tok_in=excluded.tok_in, tok_out=excluded.tok_out, tok_cr=excluded.tok_cr,
  tok_cw=excluded.tok_cw, tok_think=excluded.tok_think, cost=excluded.cost,
  last_ask=excluded.last_ask, answer=excluded.answer, first_id=excluded.first_id,
  last_id=excluded.last_id, error=excluded.error, read_ms=excluded.read_ms, read_at=excluded.read_at
RETURNING sid`,
			rd.source, rd.agent, rd.ref.Key, rd.ref.Stamp, rd.reads, s.ID, s.Path, s.Title, s.Cwd, s.Branch, s.Model, s.Version, s.Platform,
			ms(s.StartedAt), ms(s.UpdatedAt), b2i(s.Archived), b2i(s.Running),
			p.Session, p.Agent, p.Call, p.Run, p.Type, b2i(p.Fork),
			st.Turns, st.Items, st.Tools, st.Failures, st.Calls,
			st.Usage.Input, st.Usage.Output, st.Usage.CacheRead, st.Usage.CacheWrite, st.Usage.Reasoning, st.CostUSD,
			rd.lastAsk, rd.answer, rd.firstID, rd.lastID, rd.err, rd.readMs, time.Now().UnixMilli(),
		).Scan(&sid)
		if err != nil {
			return err
		}

		have := map[int][]byte{}
		rows, err := tx.QueryContext(ctx, `SELECT idx, hash FROM turns WHERE sid = ?`, sid)
		if err != nil {
			return err
		}
		for rows.Next() {
			var idx int
			var hash []byte
			if err := rows.Scan(&idx, &hash); err != nil {
				rows.Close()
				return err
			}
			have[idx] = hash
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// The turns whose words are to be written again though their rows are not: those
		// another asq wrote, and the one that holds the title when it changed — which the
		// upsert above has just named, if it did (see wordsStale)
		stale, err := staleTurns(ctx, tx, sid)
		if err != nil {
			return err
		}
		words := func(t *turnRow, again bool) error {
			title := ""
			if t.idx == titleTurn {
				title = titleWords(s.Title)
			}
			return putWords(ctx, tx, sid, t, title, again)
		}

		for i := range rd.turns {
			t := &rd.turns[i]
			old, known := have[t.idx]
			delete(have, t.idx)
			if known && bytes.Equal(old, t.hash) {
				if stale[t.idx] {
					if err := words(t, true); err != nil {
						return err
					}
				}
				continue
			}
			changed = true
			if known {
				if err := deleteTurn(ctx, tx, sid, t.idx); err != nil {
					return err
				}
			}
			if err := insertTurn(ctx, tx, sid, t); err != nil {
				return err
			}
			if err := words(t, false); err != nil {
				return err
			}
		}
		for idx := range have { // turns the transcript no longer has
			changed = true
			if err := deleteTurn(ctx, tx, sid, idx); err != nil {
				return err
			}
		}
		// Every turn the session has now holds the words this asq writes
		_, err = tx.ExecContext(ctx, `DELETE FROM words_stale WHERE sid = ?`, sid)
		return err
	})
	return sid, changed, err
}

// staleTurns are the turns of a session words_stale names.
func staleTurns(ctx context.Context, tx *sql.Tx, sid int64) (map[int]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT idx FROM words_stale WHERE sid = ?`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, err
		}
		out[idx] = true
	}
	return out, rows.Err()
}

// putWords writes a turn's row of the words table, with its session's title when it is
// the turn that holds it (see titleTurn): a turn with no words of its own has none, and is
// not found by the title alone. again says the turn may have a row already, which goes.
func putWords(ctx context.Context, tx *sql.Tx, sid int64, t *turnRow, title string, again bool) error {
	id := turnRowID(sid, t.idx)
	if again {
		if _, err := tx.ExecContext(ctx, `DELETE FROM words WHERE rowid = ?`, id); err != nil {
			return err
		}
	}
	if t.words == [5]string{} {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO words (rowid, ask, reply, thinking, tool, output, title) VALUES (?,?,?,?,?,?,?)`,
		id, t.words[0], t.words[1], t.words[2], t.words[3], t.words[4], title)
	return err
}

func insertTurn(ctx context.Context, tx *sql.Tx, sid int64, t *turnRow) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO turns (sid, idx, started_at, ended_at, last_at, first_seq, items, tools, failures, interrupted, hash, head, body)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sid, t.idx, t.startedAt, t.endedAt, t.lastAt, t.firstSeq, t.items, t.tools, t.failures, b2i(t.interrupted),
		t.hash, t.head, t.body); err != nil {
		return err
	}
	for _, b := range t.bodies {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO bodies (sid, seq, part, idx, data) VALUES (?,?,?,?,?)`,
			sid, b.seq, b.part, t.idx, b.data); err != nil {
			return err
		}
	}
	for _, u := range t.usage {
		if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO usage (sid, idx, model, at, calls, tok_in, tok_out, tok_cr, tok_cw, tok_think)
VALUES (?,?,?,?,?,?,?,?,?,?)`,
			sid, t.idx, u.model, u.at, u.calls, u.u.Input, u.u.Output, u.u.CacheRead, u.u.CacheWrite, u.u.Reasoning); err != nil {
			return err
		}
	}
	at := t.startedAt
	if at == 0 {
		at = t.lastAt
	}
	for _, f := range t.files {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO files (sid, idx, path, changed, at) VALUES (?,?,?,?,?)`,
			sid, t.idx, f.Path, b2i(f.Changed), at); err != nil {
			return err
		}
	}
	for _, f := range t.failed {
		var exit any
		if f.exit != nil {
			exit = *f.exit
		}
		if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO failures (sid, seq, idx, at, tool, kind, status, exit_code, command, output) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			sid, f.seq, t.idx, f.at, f.tool, f.kind, f.status, exit, f.command, f.output); err != nil {
			return err
		}
	}
	return nil
}

func deleteTurn(ctx context.Context, tx *sql.Tx, sid int64, idx int) error {
	for _, q := range []string{
		`DELETE FROM turns WHERE sid = ? AND idx = ?`,
		`DELETE FROM bodies WHERE sid = ? AND idx = ?`,
		`DELETE FROM usage WHERE sid = ? AND idx = ?`,
		`DELETE FROM files WHERE sid = ? AND idx = ?`,
		`DELETE FROM failures WHERE sid = ? AND idx = ?`,
		`DELETE FROM words_stale WHERE sid = ? AND idx = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, sid, idx); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM words WHERE rowid = ?`, turnRowID(sid, idx))
	return err
}

// deleteSession removes a transcript the store no longer holds.
func deleteSession(ctx context.Context, tx *sql.Tx, sid int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM words WHERE rowid >= ? AND rowid <= ?`, turnRowID(sid, 0), turnRowID(sid, maxTurnIndex)); err != nil {
		return err
	}
	for _, table := range []string{"turns", "bodies", "usage", "files", "failures", "words_stale", "sessions"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE sid = ?`, sid); err != nil {
			return err
		}
	}
	return nil
}
